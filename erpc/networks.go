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
	cfg *common.NetworkConfig

	NetworkId string
	ProjectId string
	Logger    *zerolog.Logger

	inFlightMutex    *sync.Mutex
	inFlightRequests map[string]*Multiplexer

	failsafePolicies     []failsafe.Policy[*common.NormalizedResponse]
	failsafeExecutor     failsafe.Executor[*common.NormalizedResponse]
	rateLimitersRegistry *upstream.RateLimitersRegistry
	cacheDal             data.CacheDAL
	metricsTracker       *health.Tracker
	upstreamsRegistry    *upstream.UpstreamsRegistry

	evmStatePollers map[string]*upstream.EvmStatePoller
}

func (n *Network) Bootstrap(ctx context.Context) error {
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

	n.Logger.Trace().Object("req", req).Msgf("forwarding request for network")
	req.SetNetwork(n)

	method, _ := req.Method()
	lg := n.Logger.With().Str("method", method).Str("id", req.Id()).Str("ptr", fmt.Sprintf("%p", req)).Logger()

	// 1) In-flight multiplexing
	var inf *Multiplexer
	mlxHash, err := req.CacheHash()
	if err == nil && mlxHash != "" {
		n.inFlightMutex.Lock()
		var exists bool
		if inf, exists = n.inFlightRequests[mlxHash]; exists {
			n.inFlightMutex.Unlock()
			lg.Debug().Msgf("found similar in-flight request, waiting for result")
			health.MetricNetworkMultiplexedRequests.WithLabelValues(n.ProjectId, n.NetworkId, method).Inc()

			inf.mu.RLock()
			if inf.resp != nil || inf.err != nil {
				inf.mu.RUnlock()
				resp, err := common.CopyResponseForRequest(inf.resp, req)
				if err != nil {
					return nil, err
				}
				return resp, inf.err
			}
			inf.mu.RUnlock()

			select {
			case <-inf.done:
				resp, err := common.CopyResponseForRequest(inf.resp, req)
				if err != nil {
					return nil, err
				}
				return resp, inf.err
			case <-ctx.Done():
				err := ctx.Err()
				if errors.Is(err, context.DeadlineExceeded) {
					return nil, common.NewErrNetworkRequestTimeout(time.Since(startTime))
				}

				return nil, err
			}
		}
		inf = NewMultiplexer()
		n.inFlightRequests[mlxHash] = inf
		n.inFlightMutex.Unlock()
		defer func() {
			n.inFlightMutex.Lock()
			defer n.inFlightMutex.Unlock()
			delete(n.inFlightRequests, mlxHash)
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
			if inf != nil {
				inf.Close(resp, err)
			}
			return resp, err
		}
	}

	upsList, err := n.upstreamsRegistry.GetSortedUpstreams(n.NetworkId, method)
	if err != nil {
		if inf != nil {
			inf.Close(nil, err)
		}
		return nil, err
	}

	// 3) Check if we should handle this method on this network
	if err := n.shouldHandleMethod(method, upsList); err != nil {
		if inf != nil {
			inf.Close(nil, err)
		}
		return nil, err
	}

	// 3) Apply rate limits
	if err := n.acquireRateLimitPermit(req); err != nil {
		if inf != nil {
			inf.Close(nil, err)
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

	i := 0
	resp, execErr := n.failsafeExecutor.
		WithContext(ctx).
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

				rp, er := tryForward(u, exec.Context(), &ulg)
				resp, err := n.normalizeResponse(req, rp, er)

				isClientErr := err != nil && common.HasErrorCode(err, common.ErrCodeEndpointClientSideException)
				isHedged := exec.Hedges() > 0

				if isHedged && err != nil && errors.Is(err, context.Canceled) {
					ulg.Debug().Err(err).Msgf("discarding hedged request to upstream")
					return nil, common.NewErrUpstreamHedgeCancelled(u.Config().Id)
				}
				if isHedged {
					ulg.Debug().Msgf("forwarded hedged request to upstream")
				} else {
					ulg.Debug().Msgf("forwarded request to upstream")
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
		err := upstream.TranslateFailsafeError("", method, execErr)
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
				if inf != nil {
					inf.Close(nil, err)
				}
				return nil, err
			}
		} else {
			if inf != nil {
				inf.Close(nil, err)
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

	if execErr == nil && resp != nil && !resp.IsObjectNull() {
		n.enrichStatePoller(method, req, resp)
	}
	if inf != nil {
		inf.Close(resp, nil)
	}
	return resp, nil
}

func (n *Network) EvmIsBlockFinalized(blockNumber int64) (bool, error) {
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

func (n *Network) shouldHandleMethod(method string, upsList []*upstream.Upstream) error {
	if method == "eth_newFilter" ||
		method == "eth_newBlockFilter" ||
		method == "eth_newPendingTransactionFilter" {
		if len(upsList) > 1 {
			return common.NewErrNotImplemented("eth_newFilter, eth_newBlockFilter and eth_newPendingTransactionFilter are not supported yet when there are more than 1 upstream defined")
		}
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
					if jrs != nil {
						res, err := jrs.ParsedResult()
						if err == nil {
							blk, ok := res.(map[string]interface{})
							if ok {
								bnh, ok := blk["number"].(string)
								if ok {
									blockNumber, err := common.HexToInt64(bnh)
									if err == nil {
										poller := n.evmStatePollers[resp.Upstream().Config().Id]
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
	}
}

func (n *Network) normalizeResponse(req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	switch n.Architecture() {
	case common.ArchitectureEvm:
		if resp != nil {
			// This ensures that even if upstream gives us wrong/missing ID we'll
			// use correct one from original incoming request.
			jrr, _ := resp.JsonRpcResponse()
			if jrr != nil {
				jrq, _ := req.JsonRpcRequest()
				if jrq != nil {
					jrr.ID = jrq.ID
				}
			}
		}

		if err == nil {
			return resp, nil
		}

		if common.HasErrorCode(err, common.ErrCodeJsonRpcExceptionInternal) {
			return resp, err
		} else if common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal) {
			return resp, common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorParseException,
				"failed to parse json-rpc request",
				err,
				nil,
			)
		}

		return resp, common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorServerSideException,
			fmt.Sprintf("failed request on evm network %s", n.NetworkId),
			err,
			nil,
		)
	default:
		return resp, err
	}
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
