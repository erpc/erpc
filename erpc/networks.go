package erpc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
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
	appCtx context.Context
	cfg    *common.NetworkConfig

	NetworkId string
	ProjectId string
	Logger    *zerolog.Logger

	inFlightRequests *sync.Map
	evmStatePollers  map[string]*upstream.EvmStatePoller

	failsafePolicies     []failsafe.Policy[*common.NormalizedResponse]
	failsafeExecutor     failsafe.Executor[*common.NormalizedResponse]
	rateLimitersRegistry *upstream.RateLimitersRegistry
	cacheDal             data.CacheDAL
	metricsTracker       *health.Tracker
	upstreamsRegistry    *upstream.UpstreamsRegistry
}

func (n *Network) Bootstrap(ctx context.Context) error {
	n.appCtx = ctx
	if n.Architecture() == common.ArchitectureEvm {
		upsList, err := n.upstreamsRegistry.GetSortedUpstreams(n.NetworkId, "*")
		if err != nil {
			return err
		}
		n.evmStatePollers = make(map[string]*upstream.EvmStatePoller, len(upsList))
		for _, u := range upsList {
			poller, err := upstream.NewEvmStatePoller(ctx, n.Logger, n, u, n.metricsTracker)
			if err != nil {
				return err
			}
			n.evmStatePollers[u.Config().Id] = poller
			n.Logger.Info().Str("upstreamId", u.Config().Id).Msgf("bootstraped evm state poller to track upstream latest, finalized blocks and syncing states")
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
	if n.cfg.Architecture == "" {
		if n.cfg.Evm != nil {
			n.cfg.Architecture = common.ArchitectureEvm
		}
	}

	return n.cfg.Architecture
}

func (n *Network) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	startTime := time.Now()
	req.SetNetwork(n)

	method, _ := req.Method()
	lg := n.Logger.With().Str("method", method).Interface("id", req.ID()).Str("ptr", fmt.Sprintf("%p", req)).Logger()

	mlx, resp, err := n.handleMultiplexing(ctx, req, startTime)
	if err != nil || resp != nil {
		// When the original request is already fulfilled by multiplexer
		return resp, err
	}
	if mlx != nil {
		// If we decided to multiplex, we need to make sure to clean up the multiplexer
		defer func() {
			if r := recover(); r != nil {
				lg.Error().Msgf("panic in multiplexer cleanup: %v stack: %s", r, string(debug.Stack()))
			}
			n.cleanupMultiplexer(mlx)
		}()
	}

	// 2) Get from cache if exists
	if n.cacheDal != nil && !req.SkipCacheRead() {
		lg.Debug().Msgf("checking cache for request")
		cctx, cancel := context.WithTimeoutCause(ctx, 2*time.Second, errors.New("cache driver timeout during get"))
		defer cancel()
		resp, err := n.cacheDal.Get(cctx, req)
		if err != nil {
			lg.Debug().Err(err).Msgf("could not find response in cache")
			health.MetricNetworkCacheMisses.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()
		} else if resp != nil && !resp.IsObjectNull() && !resp.IsResultEmptyish() {
			lg.Info().Msgf("response served from cache")
			health.MetricNetworkCacheHits.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()
			if mlx != nil {
				mlx.Close(resp, err)
			}
			return resp, err
		}
	}

	upsList, err := n.upstreamsRegistry.GetSortedUpstreams(n.NetworkId, method)
	if err != nil {
		if mlx != nil {
			mlx.Close(nil, err)
		}
		return nil, err
	}

	// 3) Check if we should handle this method on this network
	if err := n.shouldHandleMethod(method, upsList); err != nil {
		if mlx != nil {
			mlx.Close(nil, err)
		}
		return nil, err
	}

	// 3) Apply rate limits
	if err := n.acquireRateLimitPermit(req); err != nil {
		if mlx != nil {
			mlx.Close(nil, err)
		}
		return nil, err
	}

	// 4) Iterate over upstreams and forward the request until success or fatal failure
	tryForward := func(
		u *upstream.Upstream,
		ctx context.Context,
		lg *zerolog.Logger,
	) (resp *common.NormalizedResponse, err error) {
		lg.Debug().Msgf("trying to forward request to upstream")

		resp, err = u.Forward(ctx, req)

		if !common.IsNull(err) {
			// If upstream complains that the method is not supported let's dynamically add it ignoreMethods config
			if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
				lg.Warn().Err(err).Msgf("upstream does not support method, dynamically adding to ignoreMethods")
				u.IgnoreMethod(method)
			}

			return nil, err
		}

		if err != nil {
			lg.Debug().Err(err).Msgf("finished forwarding request to upstream with error")
		} else {
			lg.Info().Msgf("finished forwarding request to upstream with success")
		}

		return resp, err
	}

	// 5) Actual forwarding logic
	var execution failsafe.Execution[*common.NormalizedResponse]
	var errorsByUpstream = map[string]error{}

	ectx := context.WithValue(ctx, common.RequestContextKey, req)

	i := 0
	resp, execErr := n.failsafeExecutor.
		WithContext(ectx).
		GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			req.Lock()
			execution = exec
			req.Unlock()

			// We should try all upstreams at least once, but using "i" we make sure
			// across different executions of the failsafe we pick up next upstream vs retrying the same upstream.
			// This mimicks a round-robin behavior, for example when doing hedge or retries.
			// Upstream-level retry is handled by the upstream itself (and its own failsafe policies).
			ln := len(upsList)
			for count := 0; count < ln; count++ {
				if err := exec.Context().Err(); err != nil {
					return nil, err
				}
				// We need to use write-lock here because "i" is being updated.
				req.Lock()
				u := upsList[i]
				i++
				if i >= ln {
					i = 0
				}
				upsId := u.Config().Id
				if prevErr, exists := errorsByUpstream[upsId]; exists {
					if !common.IsRetryableTowardsUpstream(prevErr) || common.IsCapacityIssue(prevErr) {
						// Do not even try this upstream if we already know
						// the previous error was not retryable. e.g. Billing issues
						// Or there was a rate-limit error.
						req.Unlock()
						continue
					}
				}
				req.Unlock()

				ulg := lg.With().Str("upstreamId", u.Config().Id).Logger()

				resp, err := tryForward(u, exec.Context(), &ulg)
				if e := n.normalizeResponse(req, resp); e != nil {
					ulg.Error().Err(e).Msgf("failed to normalize response")
					err = e
				}

				isClientErr := common.IsClientError(err)
				isHedged := exec.Hedges() > 0

				if isHedged && (err != nil && errors.Is(err, context.Canceled)) {
					ulg.Debug().Err(err).Msgf("discarding hedged request to upstream")
					return nil, common.NewErrUpstreamHedgeCancelled(u.Config().Id, context.Cause(exec.Context()))
				}

				if err != nil {
					req.Lock()
					if ser, ok := err.(common.StandardError); ok {
						ber := ser.Base()
						if ber.Details == nil {
							ber.Details = map[string]interface{}{}
						}
						ber.Details["timestampMs"] = time.Now().UnixMilli()
					}
					errorsByUpstream[upsId] = err
					req.Unlock()
				}

				if err == nil || isClientErr {
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
		err := upstream.TranslateFailsafeError(common.ScopeNetwork, "", method, execErr, &startTime)
		// If error is due to empty response be generous and accept it,
		// because this means after many retries still no data is available.
		if common.HasErrorCode(err, common.ErrCodeFailsafeRetryExceeded) {
			// TODO is there a cleaner way to use last result when retry is exhausted?
			lvr := req.LastValidResponse()
			if !lvr.IsObjectNull() {
				if lvr.IsResultEmptyish() {
					// We don't need to worry about replying wrongly empty responses for unfinalized data
					// because cache layer already is not caching unfinalized data.
					resp = lvr
				} else if n.Architecture() == common.ArchitectureEvm {
					evmBlkNum, err := lvr.EvmBlockNumber()
					if err == nil && evmBlkNum == 0 {
						// For pending txs we can accept the response, if after retries it is still pending.
						// This avoids failing with "retry" error, when we actually do have a response but blockNumber is null since tx is pending.
						resp = lvr
					}
				}
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
				if mlx != nil {
					mlx.Close(nil, err)
				}
				return nil, err
			}
		} else {
			if mlx != nil {
				mlx.Close(nil, err)
			}
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
			resp.RLock()
			go (func(resp *common.NormalizedResponse) {
				defer resp.RUnlock()
				c, cancel := context.WithTimeoutCause(n.appCtx, 10*time.Second, errors.New("cache driver timeout during set"))
				defer cancel()
				err := n.cacheDal.Set(c, req, resp)
				if err != nil {
					lg.Warn().Err(err).Msgf("could not store response in cache")
				}
			})(resp)
		}
	}

	if execErr == nil && resp != nil && !resp.IsObjectNull() {
		n.enrichStatePoller(method, req, resp)
	}
	if mlx != nil {
		mlx.Close(resp, nil)
	}
	return resp, nil
}

func (n *Network) EvmIsBlockFinalized(blockNumber int64) (bool, error) {
	if n == nil || n.evmStatePollers == nil || len(n.evmStatePollers) == 0 {
		return false, nil
	}

	for _, poller := range n.evmStatePollers {
		if fin, err := poller.IsBlockFinalized(blockNumber); err != nil {
			if common.HasErrorCode(err, common.ErrCodeFinalizedBlockUnavailable) {
				continue
			}
			return false, err
		} else if fin {
			return true, nil
		}
	}
	return false, nil
}

func (n *Network) Config() *common.NetworkConfig {
	return n.cfg
}

func (n *Network) EvmChainId() (int64, error) {
	if n.cfg == nil || n.cfg.Evm == nil {
		return 0, common.NewErrUnknownNetworkID(n.Architecture())
	}
	return n.cfg.Evm.ChainId, nil
}

func (n *Network) handleMultiplexing(ctx context.Context, req *common.NormalizedRequest, startTime time.Time) (*Multiplexer, *common.NormalizedResponse, error) {
	mlxHash, err := req.CacheHash()
	if err != nil || mlxHash == "" {
		n.Logger.Debug().Str("hash", mlxHash).Err(err).Object("request", req).Msgf("could not get multiplexing hash for request")
		return nil, nil, nil
	}

	// Check for existing multiplexer
	if vinf, exists := n.inFlightRequests.Load(mlxHash); exists {
		inf := vinf.(*Multiplexer)
		method, _ := req.Method()
		health.MetricNetworkMultiplexedRequests.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()

		resp, err := n.waitForMultiplexResult(ctx, inf, req, startTime)
		if err != nil {
			return nil, nil, err
		}
		if resp != nil {
			return nil, resp, nil
		}
	}

	mlx := NewMultiplexer(mlxHash)
	n.inFlightRequests.Store(mlxHash, mlx)

	return mlx, nil, nil
}

func (n *Network) waitForMultiplexResult(ctx context.Context, mlx *Multiplexer, req *common.NormalizedRequest, startTime time.Time) (*common.NormalizedResponse, error) {
	// Check if result is already available
	mlx.mu.RLock()
	if mlx.resp != nil || mlx.err != nil {
		mlx.mu.RUnlock()
		resp, err := common.CopyResponseForRequest(mlx.resp, req)
		if err != nil {
			return nil, err
		}
		return resp, mlx.err
	}
	mlx.mu.RUnlock()

	// Wait for result
	select {
	case <-mlx.done:
		resp, err := common.CopyResponseForRequest(mlx.resp, req)
		if err != nil {
			return nil, err
		}
		return resp, mlx.err
	case <-ctx.Done():
		n.cleanupMultiplexer(mlx)
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, common.NewErrNetworkRequestTimeout(time.Since(startTime))
		}
		return nil, err
	}
}

func (n *Network) cleanupMultiplexer(mlx *Multiplexer) {
	n.inFlightRequests.Delete(mlx.hash)
}

func (n *Network) shouldHandleMethod(method string, upsList []*upstream.Upstream) error {
	if method == "eth_newFilter" ||
		method == "eth_newBlockFilter" ||
		method == "eth_newPendingTransactionFilter" {
		if len(upsList) > 1 {
			return common.NewErrNotImplemented("eth_newFilter, eth_newBlockFilter and eth_newPendingTransactionFilter are not supported yet when there are more than 1 upstream defined")
		}
	}
	if method == "eth_accounts" || method == "eth_sign" {
		return common.NewErrNotImplemented("eth_accounts and eth_sign are not supported")
	}

	return nil
}

func (n *Network) enrichStatePoller(method string, req *common.NormalizedRequest, resp *common.NormalizedResponse) {
	switch n.Architecture() {
	case common.ArchitectureEvm:
		if method == "eth_getBlockByNumber" {
			jrq, _ := req.JsonRpcRequest()
			if blkTag, ok := jrq.Params[0].(string); ok {
				if blkTag == "finalized" || blkTag == "latest" {
					jrs, _ := resp.JsonRpcResponse()
					bnh, err := jrs.PeekStringByPath("number")
					if err == nil {
						blockNumber, err := common.HexToInt64(bnh)
						if err == nil {
							poller, ok := n.evmStatePollers[resp.Upstream().Config().Id]
							if ok {
								if blkTag == "finalized" {
									poller.SuggestFinalizedBlock(blockNumber)
								} else if blkTag == "latest" {
									poller.SuggestLatestBlock(blockNumber)
								}
							}
						}
					}
				}
			}
		}
	}
}

func (n *Network) normalizeResponse(req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	switch n.Architecture() {
	case common.ArchitectureEvm:
		if resp != nil {
			// This ensures that even if upstream gives us wrong/missing ID we'll
			// use correct one from original incoming request.
			if jrr, err := resp.JsonRpcResponse(); err == nil {
				jrq, err := req.JsonRpcRequest()
				if err != nil {
					return err
				}
				err = jrr.SetID(jrq.ID)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (n *Network) acquireRateLimitPermit(req *common.NormalizedRequest) error {
	if n.cfg.RateLimitBudget == "" {
		return nil
	}

	rlb, errNetLimit := n.rateLimitersRegistry.GetBudget(n.cfg.RateLimitBudget)
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
					n.cfg.RateLimitBudget,
					fmt.Sprintf("%+v", rule.Config),
				)
			} else {
				lg.Debug().Object("rateLimitRule", rule.Config).Msgf("network-level rate limit passed")
			}
		}
	}

	return nil
}
