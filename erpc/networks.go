package erpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/upstream"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
)

type Network struct {
	Config *common.NetworkConfig

	NetworkId string
	ProjectId string
	Logger    *zerolog.Logger

	inFlightMutex    *sync.Mutex
	inFlightRequests map[string]*Multiplexer

	failsafePolicies     []failsafe.Policy[*common.NormalizedResponse]
	failsafeExecutor     failsafe.Executor[*common.NormalizedResponse]
	rateLimitersRegistry *upstream.RateLimitersRegistry
	// rateLimiterDal       data.RateLimitersDAL
	cacheDal          data.CacheDAL
	evmBlockTracker   *EvmBlockTracker
	metricsTracker    *health.Tracker
	upstreamsRegistry *upstream.UpstreamsRegistry
}

func (n *Network) Bootstrap(ctx context.Context) error {
	if n.Architecture() == common.ArchitectureEvm {
		n.evmBlockTracker = NewEvmBlockTracker(n)
		if err := n.evmBlockTracker.Bootstrap(ctx); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("network architecture not supported: %s", n.Architecture())
	}

	return nil
}

func (n *Network) Id() string {
	return n.NetworkId
}

func (n *Network) Architecture() common.NetworkArchitecture {
	if n.Config.Architecture == "" {
		if n.Config.Evm != nil {
			n.Config.Architecture = common.ArchitectureEvm
		}
	}

	return n.Config.Architecture
}

func (n *Network) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	startTime := time.Now()

	n.Logger.Trace().Object("req", req).Msgf("forwarding request")
	req.SetNetwork(n)

	method, _ := req.Method()
	lg := n.Logger.With().Str("method", method).Str("id", req.Id()).Str("ptr", fmt.Sprintf("%p", req)).Logger()

	// 1) In-flight multiplexing
	mlxHash, _ := req.CacheHash()
	n.inFlightMutex.Lock()
	if inf, exists := n.inFlightRequests[mlxHash]; exists {
		n.inFlightMutex.Unlock()
		lg.Debug().Msgf("found similar in-flight request, waiting for result")
		health.MetricNetworkMultiplexedRequests.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()

		inf.mu.RLock()
		if inf.resp != nil || inf.err != nil {
			inf.mu.RUnlock()
			return inf.resp, inf.err
		}
		inf.mu.RUnlock()

		select {
		case <-inf.done:
			return inf.resp, inf.err
		case <-ctx.Done():
			err := ctx.Err()
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, common.NewErrNetworkRequestTimeout(time.Since(startTime))
			}

			return nil, err
		}
	}
	inf := NewMultiplexer()
	n.inFlightRequests[mlxHash] = inf
	n.inFlightMutex.Unlock()
	defer func() {
		n.inFlightMutex.Lock()
		defer n.inFlightMutex.Unlock()
		delete(n.inFlightRequests, mlxHash)
	}()

	// 2) Get from cache if exists
	if n.cacheDal != nil {
		lg.Debug().Msgf("checking cache for request")
		cctx, cancel := context.WithTimeoutCause(ctx, 2*time.Second, errors.New("cache driver timeout during get"))
		defer cancel()
		resp, err := n.cacheDal.Get(cctx, req)
		if err != nil {
			lg.Debug().Err(err).Msgf("could not find response in cache")
			health.MetricNetworkCacheMisses.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()
		} else if resp != nil && !resp.IsObjectNull() && !resp.IsResultEmptyish() {
			resp.SetFromCache(true)
			lg.Info().Msgf("response served from cache")
			health.MetricNetworkCacheHits.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()
			inf.Close(resp, err)
			return resp, err
		}
	}

	// 3) Apply rate limits
	if err := n.acquireRateLimitPermit(req); err != nil {
		inf.Close(nil, err)
		return nil, err
	}

	// 4) Iterate over upstreams and forward the request until success or fatal failure
	tryForward := func(
		u *upstream.Upstream,
		ctx context.Context,
	) (resp *common.NormalizedResponse, skipped bool, err error) {
		lg := u.Logger.With().Str("upstreamId", u.Config().Id).Str("method", method).Str("id", req.Id()).Str("ptr", fmt.Sprintf("%p", req)).Logger()

		lg.Debug().Msgf("trying to forward request to upstream")

		resp, skipped, err = u.Forward(ctx, req)
		if !common.IsNull(err) {
			// If upstream complains that the method is not supported let's dynamically add it ignoreMethods config
			if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
				lg.Warn().Err(err).Msgf("upstream does not support method, dynamically adding to ignoreMethods")
				u.IgnoreMethod(method)
			}

			return nil, skipped, err
		}

		if skipped {
			lg.Debug().Err(err).Msgf("skipped forwarding request to upstream")
		} else {
			lg.Info().Msgf("finished forwarding request to upstream")
		}

		return resp, skipped, err
	}

	upsList, err := n.upstreamsRegistry.GetSortedUpstreams(n.NetworkId, method)
	if err != nil {
		inf.Close(nil, err)
		return nil, err
	}

	var execution failsafe.Execution[*common.NormalizedResponse]
	var errorsByUpstream = []error{}

	coordMu := sync.Mutex{}
	i := 0
	resp, execErr := n.failsafeExecutor.
		WithContext(ctx).
		GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			coordMu.Lock()
			execution = exec
			coordMu.Unlock()
			isHedged := exec.Hedges() > 0

			// We should try all upstreams at least once, but using "i" we make sure
			// across different executions of the failsafe we pick up next upstream vs retrying the same upstream.
			// This mimicks a round-robin behavior, for example when doing hedge or retries.
			// Upstream-level retry is handled by the upstream itself (and its own failsafe policies).
			ln := len(upsList)
			for count := 0; count < ln; count++ {
				coordMu.Lock()
				n.upstreamsRegistry.RLockUpstreams()
				u := upsList[i]
				n.upstreamsRegistry.RUnlockUpstreams()
				i++
				if i >= ln {
					i = 0
				}
				if isHedged {
					lg.Debug().
						Str("upstreamId", u.Config().Id).
						Int("index", i).
						Msgf("executing hedged forward to upstream")
				} else {
					lg.Debug().
						Str("upstreamId", u.Config().Id).
						Int("index", i).
						Msgf("executing forward to upstream")
				}
				coordMu.Unlock()

				resp, skipped, err := n.processResponse(
					tryForward(u, exec.Context()),
				)

				if isHedged && err != nil && errors.Is(err, context.Canceled) {
					lg.Debug().Err(err).Msgf("discarding hedged request to upstream %s: %v", u.Config().Id, skipped)
					return nil, err
				}

				if isHedged {
					lg.Debug().Msgf("forwarded hedged request to upstream %s skipped: %v", u.Config().Id, skipped)
				} else {
					lg.Debug().Msgf("forwarded request to upstream %s skipped: %v", u.Config().Id, skipped)
				}
				if err != nil {
					coordMu.Lock()
					errorsByUpstream = append(errorsByUpstream, err)
					coordMu.Unlock()
				}
				if !skipped {
					if resp != nil {
						resp.SetUpstream(u)
					}
					return resp, err
				} else if err != nil {
					continue
				}
			}

			return nil, common.NewErrUpstreamsExhausted(
				req,
				errorsByUpstream,
				n.ProjectId,
				n.NetworkId,
				time.Since(startTime),
				exec.Attempts(),
				exec.Retries(),
				exec.Hedges(),
			)
		})

	if execErr != nil {
		err := upstream.TranslateFailsafeError(execErr)
		// If error is due to empty response be generous and accept it,
		// because this means after many retries still no data is available.
		if common.HasErrorCode(err, common.ErrCodeFailsafeRetryExceeded) {
			lvr := req.LastValidResponse()
			if !lvr.IsObjectNull() && lvr.IsResultEmptyish() {
				// We don't need to worry about replying wrongly empty responses for unfinalized data
				// because cache layer already is not caching unfinalized data.
				resp = lvr
			} else {
				if len(errorsByUpstream) > 0 {
					err = common.NewErrUpstreamsExhausted(
						req,
						errorsByUpstream,
						n.ProjectId,
						n.NetworkId,
						time.Since(startTime),
						execution.Attempts(),
						execution.Retries(),
						execution.Hedges(),
					)
				}
				inf.Close(nil, err)
				return nil, err
			}
		} else {
			inf.Close(nil, err)
			return nil, err
		}
	}

	if resp != nil {
		if execution != nil {
			resp.SetAttempts(execution.Attempts())
			resp.SetRetries(execution.Retries())
			resp.SetHedges(execution.Hedges())
		}

		if n.cacheDal != nil {
			go (func(resp *common.NormalizedResponse) {
				c, cancel := context.WithTimeoutCause(context.Background(), 10*time.Second, errors.New("cache driver timeout during set"))
				defer cancel()
				err := n.cacheDal.Set(c, req, resp)
				if err != nil {
					lg.Warn().Err(err).Msgf("could not store response in cache")
				}
			})(resp)
		}
	}

	inf.Close(resp, nil)
	return resp, nil
}

func (n *Network) EvmIsBlockFinalized(blockNumber int64) (bool, error) {
	if n.evmBlockTracker == nil {
		return false, nil
	}

	finalizedBlock := n.evmBlockTracker.FinalizedBlock()
	latestBlock := n.evmBlockTracker.LatestBlock()
	if latestBlock == 0 && finalizedBlock == 0 {
		n.Logger.Debug().
			Int64("finalizedBlock", finalizedBlock).
			Int64("latestBlock", latestBlock).
			Int64("blockNumber", blockNumber).
			Msgf("finalized/latest blocks are not available yet when checking block finality")
		return false, common.NewErrFinalizedBlockUnavailable(blockNumber)
	}

	n.Logger.Debug().
		Int64("finalizedBlock", finalizedBlock).
		Int64("latestBlock", latestBlock).
		Int64("blockNumber", blockNumber).
		Msgf("calculating block finality")

	if finalizedBlock > 0 {
		return blockNumber <= finalizedBlock, nil
	}

	if latestBlock == 0 {
		return false, nil
	}

	var fb int64

	if n.Config.Evm != nil {
		if latestBlock > n.Config.Evm.FinalityDepth {
			fb = latestBlock - n.Config.Evm.FinalityDepth
		} else {
			fb = 0
		}
	} else {
		if latestBlock > 1024 {
			fb = latestBlock - 1024
		} else {
			fb = 0
		}
	}

	n.Logger.Debug().
		Int64("inferredFinalizedBlock", fb).
		Int64("latestBlock", latestBlock).
		Int64("blockNumber", blockNumber).
		Msgf("calculating block finality using inferred finalized block")

	return blockNumber <= fb, nil
}

func (n *Network) EvmBlockTracker() common.EvmBlockTracker {
	return n.evmBlockTracker
}

func (n *Network) EvmChainId() (int64, error) {
	if n.Config == nil || n.Config.Evm == nil {
		return 0, common.NewErrUnknownNetworkID(n.Architecture())
	}
	return n.Config.Evm.ChainId, nil
}

func (n *Network) processResponse(resp *common.NormalizedResponse, skipped bool, err error) (*common.NormalizedResponse, bool, error) {
	if err == nil {
		return resp, skipped, nil
	}

	switch n.Architecture() {
	case common.ArchitectureEvm:
		if common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen) {
			// Explicitly skip when CB is open to not count the failed request towards network "retries"
			return resp, true, err
		} else if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) || common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped) {
			// Explicitly skip when method is not supported so it is not counted towards retries
			return resp, true, err
		} else if common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal) {
			return resp, skipped, err
		} else if common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal) {
			return resp, skipped, common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorParseException,
				"failed to parse json-rpc request",
				err,
				nil,
			)
		}

		return resp, skipped, common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorServerSideException,
			fmt.Sprintf("failed request on evm network %s", n.NetworkId),
			err,
			nil,
		)
	default:
		return resp, skipped, err
	}
}

func (n *Network) acquireRateLimitPermit(req *common.NormalizedRequest) error {
	if n.Config.RateLimitBudget == "" {
		return nil
	}

	rlb, errNetLimit := n.rateLimitersRegistry.GetBudget(n.Config.RateLimitBudget)
	if errNetLimit != nil {
		return errNetLimit
	}
	if rlb == nil {
		return nil
	}

	method, errMethod := req.Method()
	if errMethod != nil {
		return errMethod
	}
	lg := n.Logger.With().Str("method", method).Logger()

	rules := rlb.GetRulesByMethod(method)
	lg.Debug().Msgf("found %d network-level rate limiters", len(rules))

	if len(rules) > 0 {
		for _, rule := range rules {
			permit := rule.Limiter.TryAcquirePermit()
			if !permit {
				health.MetricNetworkRequestSelfRateLimited.WithLabelValues(
					n.ProjectId,
					n.NetworkId,
					method,
				).Inc()
				return common.NewErrNetworkRateLimitRuleExceeded(
					n.ProjectId,
					n.NetworkId,
					n.Config.RateLimitBudget,
					fmt.Sprintf("%+v", rule.Config),
				)
			} else {
				lg.Debug().Object("rateLimitRule", rule.Config).Msgf("network-level rate limit passed")
			}
		}
	}

	return nil
}
