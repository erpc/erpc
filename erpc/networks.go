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

	failsafePolicies     []failsafe.Policy[common.NormalizedResponse]
	failsafeExecutor     failsafe.Executor[common.NormalizedResponse]
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

func (n *Network) Forward(ctx context.Context, req *upstream.NormalizedRequest) (common.NormalizedResponse, error) {
	startTime := time.Now()

	n.Logger.Debug().Object("req", req).Msgf("forwarding request")
	req.SetNetwork(n)

	method, _ := req.Method()
	lg := n.Logger.With().Str("method", method).Logger()

	// 1) In-flight multiplexing
	mlxHash, _ := req.CacheHash()
	n.inFlightMutex.Lock()
	if inf, exists := n.inFlightRequests[mlxHash]; exists {
		n.inFlightMutex.Unlock()
		lg.Debug().Object("req", req).Msgf("found similar in-flight request, waiting for result")
		health.MetricNetworkMultiplexedRequests.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()

		inf.mu.RLock()
		if inf.resp != nil || inf.err != nil {
			inf.mu.RUnlock()
			return inf.resp, inf.err
		}
		inf.mu.RUnlock()

		select {
		case <-inf.done:
			// inf.mu.RLock()
			// defer inf.mu.RUnlock()
			return inf.resp, inf.err
		case <-ctx.Done():
			return nil, ctx.Err()
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
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		resp, err := n.cacheDal.Get(cctx, req)
		lg.Debug().Err(err).Msgf("cache response: %v", resp)
		if err != nil {
			lg.Debug().Err(err).Msgf("could not find response in cache")
			health.MetricNetworkCacheMisses.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()
		} else if resp != nil {
			lg.Info().Object("req", req).Err(err).Msgf("response served from cache")
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
	) (resp common.NormalizedResponse, skipped bool, err error) {
		lg := u.Logger.With().Str("upstream", u.Config().Id).Logger()

		lg.Debug().Str("method", method).Str("rid", fmt.Sprintf("%p", req)).Msgf("trying to forward request to upstream")

		resp, skipped, err = u.Forward(ctx, req)
		if !common.IsNull(err) {
			// If upstream complains that the method is not supported let's dynamically add it ignoreMethods config
			if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
				lg.Warn().Err(err).Str("method", method).Msgf("upstream does not support method, dynamically adding to ignoreMethods")
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

	imtx := sync.Mutex{}
	i := 0
	upsList, err := n.upstreamsRegistry.GetSortedUpstreams(n.NetworkId, method)
	if err != nil {
		inf.Close(nil, err)
		return nil, err
	}
	var execution failsafe.Execution[common.NormalizedResponse]
	var errorsByUpstream = []error{}
	var errorsMutex sync.Mutex
	resp, execErr := n.failsafeExecutor.
		WithContext(ctx).
		GetWithExecution(func(exec failsafe.Execution[common.NormalizedResponse]) (common.NormalizedResponse, error) {
			execution = exec
			isHedged := exec.Hedges() > 0

			// We should try all upstreams at least once, but using "i" we make sure
			// across different executions of the failsafe we pick up next upstream vs retrying the same upstream.
			// This mimicks a round-robin behavior, for example when doing hedge or retries.
			// Upstream-level retry is handled by the upstream itself (and its own failsafe policies).
			ln := len(upsList)
			for count := 0; count < ln; count++ {
				imtx.Lock()
				n.upstreamsRegistry.RLockUpstreams()
				u := upsList[i]
				n.upstreamsRegistry.RUnlockUpstreams()
				i++
				if i >= ln {
					i = 0
				}
				if isHedged {
					lg.Debug().
						Str("upstream", u.Config().Id).
						Int("index", i).
						Msgf("executing hedged forward to upstream")
				} else {
					lg.Debug().
						Str("upstream", u.Config().Id).
						Int("index", i).
						Msgf("executing forward to upstream")
				}
				imtx.Unlock()

				resp, skipped, err := n.processResponse(
					tryForward(u, exec.Context()),
				)

				if isHedged && err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
					lg.Debug().Err(err).Msgf("discarding hedged request to upstream %s: %v", u.Config().Id, skipped)
					return nil, err
				}

				if isHedged {
					lg.Debug().Err(err).Msgf("forwarded hedged request to upstream %s skipped: %v err: %v", u.Config().Id, skipped, err)
				} else {
					lg.Debug().Err(err).Msgf("forwarded request to upstream %s skipped: %v err: %v", u.Config().Id, skipped, err)
				}
				if err != nil {
					errorsMutex.Lock()
					errorsByUpstream = append(errorsByUpstream, err)
					errorsMutex.Unlock()
				}
				if !skipped {
					return resp, err
				} else if err != nil {
					continue
				}
			}

			return nil, common.NewErrUpstreamsExhausted(req, errorsByUpstream, time.Since(startTime))
		})

	if execErr != nil {
		err := upstream.TranslateFailsafeError(execution, execErr, map[string]interface{}{
			"projectId": n.ProjectId,
			"networkId": n.NetworkId,
		})
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
					err = common.NewErrUpstreamsExhausted(req, errorsByUpstream, time.Since(startTime))
				}
				inf.Close(nil, err)
				return nil, err
			}
		} else {
			inf.Close(nil, err)
			return nil, err
		}
	}

	if n.cacheDal != nil && resp != nil {
		go (func(resp common.NormalizedResponse) {
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := n.cacheDal.Set(c, req, resp)
			if err != nil {
				lg.Warn().Err(err).Msgf("could not store response in cache")
			}
		})(resp)
	}

	inf.Close(resp, nil)
	return resp, nil
}

func (n *Network) EvmIsBlockFinalized(blockNumber uint64) (bool, error) {
	if n.evmBlockTracker == nil {
		return false, nil
	}

	finalizedBlock := n.evmBlockTracker.FinalizedBlock()
	latestBlock := n.evmBlockTracker.LatestBlock()
	if latestBlock == 0 && finalizedBlock == 0 {
		n.Logger.Debug().
			Uint64("finalizedBlock", finalizedBlock).
			Uint64("latestBlock", latestBlock).
			Uint64("blockNumber", blockNumber).
			Msgf("finalized/latest blocks are not available yet when checking block finality")
		return false, nil
	}

	n.Logger.Debug().
		Uint64("finalizedBlock", finalizedBlock).
		Uint64("latestBlock", latestBlock).
		Uint64("blockNumber", blockNumber).
		Msgf("calculating block finality")

	if finalizedBlock > 0 {
		return blockNumber <= finalizedBlock, nil
	}

	if latestBlock == 0 {
		return false, nil
	}

	var fb uint64

	if n.Config.Evm != nil {
		fb = latestBlock - n.Config.Evm.FinalityDepth
	} else {
		fb = latestBlock - 128
	}

	n.Logger.Debug().
		Uint64("inferredFinalizedBlock", fb).
		Uint64("latestBlock", latestBlock).
		Uint64("blockNumber", blockNumber).
		Msgf("calculating block finality using inferred finalized block")

	return blockNumber <= fb, nil
}

func (n *Network) EvmBlockTracker() common.EvmBlockTracker {
	return n.evmBlockTracker
}

func (n *Network) EvmChainId() (uint64, error) {
	if n.Config == nil || n.Config.Evm == nil {
		return 0, common.NewErrUnknownNetworkID(n.Architecture())
	}
	return uint64(n.Config.Evm.ChainId), nil
}

func (n *Network) processResponse(resp common.NormalizedResponse, skipped bool, err error) (common.NormalizedResponse, bool, error) {
	if err == nil {
		return resp, skipped, nil
	}

	switch n.Architecture() {
	case common.ArchitectureEvm:
		if common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal) {
			return resp, skipped, err
		} else if common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal) {
			return resp, skipped, common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorParseException,
				"failed to parse json-rpc request",
				err,
				nil,
			)
		} else if common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen) {
			// Explicitly skip when CB is open to not count the failed request towards network "retries"
			return resp, true, err
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

func (n *Network) acquireRateLimitPermit(req *upstream.NormalizedRequest) error {
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
