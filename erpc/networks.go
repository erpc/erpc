package erpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/data"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/rs/zerolog"
)

type Network struct {
	Config *common.NetworkConfig

	NetworkId string
	ProjectId string
	Logger    *zerolog.Logger
	// Upstreams map[string][]*upstream.Upstream

	// upstreamsMutex   *sync.RWMutex
	inFlightMutex    *sync.Mutex
	inFlightRequests map[string]*multiplexedInFlightRequest

	failsafePolicies     []failsafe.Policy[common.NormalizedResponse]
	failsafeExecutor     failsafe.Executor[common.NormalizedResponse]
	rateLimitersRegistry *upstream.RateLimitersRegistry
	// rateLimiterDal       data.RateLimitersDAL
	cacheDal          data.CacheDAL
	evmBlockTracker   *EvmBlockTracker
	metricsTracker    *health.Tracker
	upstreamsRegistry *upstream.UpstreamsRegistry
}

type multiplexedInFlightRequest struct {
	resp common.NormalizedResponse
	err  error
	done chan struct{}
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

	// if len(n.Upstreams) > 0 {
	// 	go func(pn *Network) {
	// 		ticker := time.NewTicker(1 * time.Second)
	// 		defer ticker.Stop()
	// 		for {
	// 			select {
	// 			case <-ctx.Done():
	// 				pn.Logger.Debug().Msg("shutting down upstream reordering timer due to context cancellation")
	// 				return
	// 			case <-ticker.C:
	// 				upsList := n.reorderUpstreams(pn.Upstreams)
	// 				pn.upstreamsMutex.Lock()
	// 				pn.Upstreams = upsList
	// 				pn.upstreamsMutex.Unlock()
	// 			}
	// 		}
	// 	}(n)
	// }

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
	n.Logger.Debug().Object("req", req).Msgf("forwarding request")
	req.WithNetwork(n)

	method, _ := req.Method()
	lg := n.Logger.With().Str("method", method).Logger()

	// 1) In-flight multiplexing
	mlxHash, _ := req.CacheHash()
	n.inFlightMutex.Lock()
	if inf, exists := n.inFlightRequests[mlxHash]; exists {
		n.inFlightMutex.Unlock()
		lg.Debug().Object("req", req).Msgf("found similar in-flight request, waiting for result")
		health.MetricNetworkMultiplexedRequests.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()
		if inf.resp != nil || inf.err != nil {
			return inf.resp, inf.err
		}
		select {
		case <-inf.done:
			return inf.resp, inf.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	inf := &multiplexedInFlightRequest{
		done: make(chan struct{}),
	}
	n.inFlightRequests[mlxHash] = inf
	n.inFlightMutex.Unlock()
	defer func() {
		<-inf.done
		n.inFlightMutex.Lock()
		delete(n.inFlightRequests, mlxHash)
		n.inFlightMutex.Unlock()
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
			inf.resp = resp
			close(inf.done) // Ensure done is closed
			return resp, err
		}
	}

	// 3) Apply rate limits
	if err := n.acquireRateLimitPermit(req); err != nil {
		inf.err = err
		close(inf.done) // Ensure done is closed
		return nil, err
	}

	// 4) Iterate over upstreams and forward the request until success or fatal failure
	var errorsByUpstream = []error{}
	tryForward := func(
		u *upstream.Upstream,
		ctx context.Context,
	) (resp common.NormalizedResponse, skipped bool, err error) {
		lg := u.Logger.With().Str("upstream", u.Config().Id).Logger()

		// TODO skip upstream if method not supported / score is negative / too much block lag and request is for recent data
		resp, err = n.forwardToUpstream(u, ctx, req)
		if !common.IsNull(err) {
			return nil, false, err
		}

		lg.Info().Msgf("successfully forwarded request to upstream")
		return resp, false, nil
	}

	mtx := sync.Mutex{}
	i := 0
	resp, execErr := n.failsafeExecutor.
		WithContext(ctx).
		GetWithExecution(func(exec failsafe.Execution[common.NormalizedResponse]) (common.NormalizedResponse, error) {
			upsList, err := n.upstreamsRegistry.GetSortedUpstreams(n.NetworkId, method)
			if err != nil {
				return nil, err
			}

			isHedged := exec.Hedges() > 0

			// We should try all upstreams at least once, but using "i" we make sure
			// across different executions of the failsafe we pick up next upstream vs retrying the same upstream.
			// This mimicks a round-robin behavior, for example when doing hedge or retries.
			// Upstream-level retry is handled by the upstream itself (and its own failsafe policies).
			ln := len(upsList)
			for count := 0; count < ln; count++ {
				mtx.Lock()
				u := upsList[i]
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
				mtx.Unlock()

				resp, skipped, err := n.processResponse(
					tryForward(u, exec.Context()),
				)

				if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) && exec.Hedges() > 0 {
					lg.Debug().Err(err).Msgf("discarding hedged request to upstream %s: %v", u.Config().Id, skipped)
					return nil, err
				}

				if isHedged {
					lg.Debug().Err(err).Msgf("forwarded hedged request to upstream %s skipped: %v err: %v", u.Config().Id, skipped, err)
				} else {
					lg.Debug().Err(err).Msgf("forwarded request to upstream %s skipped: %v err: %v", u.Config().Id, skipped, err)
				}
				if !skipped {
					return resp, err
				} else if err != nil {
					errorsByUpstream = append(errorsByUpstream, err)
					continue
				}
			}

			return nil, common.NewErrUpstreamsExhausted(errorsByUpstream)
		})

	if execErr != nil {
		err := upstream.TranslateFailsafeError(execErr)
		// if error is due to empty response be generous and accept it
		if common.HasCode(err, common.ErrCodeFailsafeRetryExceeded) {
			lr := tryExtractLastResult(err)
			if lr != nil && lr.IsResultEmptyish() {
				resp = lr
			} else {
				inf.err = err
				close(inf.done)
				return nil, err
			}
		} else {
			inf.err = err
			close(inf.done)
			return nil, err
		}
	}

	if n.cacheDal != nil && resp != nil {
		go (func(resp common.NormalizedResponse) {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := n.cacheDal.Set(c, req, resp)
			if err != nil {
				lg.Warn().Err(err).Msgf("could not store response in cache")
			}
		})(resp)
	}

	inf.resp = resp
	close(inf.done)
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
		if common.HasCode(err, common.ErrCodeJsonRpcException) {
			return resp, skipped, err
		}
		if common.HasCode(err, common.ErrCodeJsonRpcRequestUnmarshal) {
			return resp, skipped, common.NewErrJsonRpcException(
				0,
				common.JsonRpcErrorParseException,
				"failed to parse json-rpc request",
				err,
			)
		}
		return resp, skipped, common.NewErrJsonRpcException(
			0,
			common.JsonRpcErrorServerSideException,
			fmt.Sprintf("failed request on evm network %s", n.NetworkId),
			err,
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
			permit := (*rule.Limiter).TryAcquirePermit()
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

func (n *Network) forwardToUpstream(
	ups *upstream.Upstream,
	ctx context.Context,
	req *upstream.NormalizedRequest,
) (common.NormalizedResponse, error) {
	var category string = ""
	if m, _ := req.Method(); m != "" {
		category = m
	}

	n.metricsTracker.RecordUpstreamRequest(n.NetworkId, ups.Config().Id, category)
	timer := n.metricsTracker.RecordUpstreamDurationStart(n.NetworkId, ups.Config().Id, category)
	defer timer.ObserveDuration()

	return ups.Forward(ctx, req)
}

func tryExtractLastResult(err interface{}) common.NormalizedResponse {
	re, ok := err.(*common.ErrFailsafeRetryExceeded)
	var lr interface{}
	if ok {
		lr = re.LastResult()
	}

	if lr == nil {
		if be, ok := err.(interface {
			GetCause() error
		}); ok {
			c := be.GetCause()
			if c != nil && common.HasCode(c, common.ErrCodeFailsafeRetryExceeded) {
				return tryExtractLastResult(c)
			}
		}
		return nil
	}

	return lr.(common.NormalizedResponse)
}
