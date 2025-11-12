package erpc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type FailsafeExecutor struct {
	method                 string
	finalities             []common.DataFinalityState
	executor               failsafe.Executor[*common.NormalizedResponse]
	timeout                *time.Duration
	consensusPolicyEnabled bool
}

type Network struct {
	networkId                string
	networkLabel             string
	projectId                string
	logger                   *zerolog.Logger
	bootstrapOnce            sync.Once
	appCtx                   context.Context
	cfg                      *common.NetworkConfig
	inFlightRequests         *sync.Map
	failsafeExecutors        []*FailsafeExecutor
	rateLimitersRegistry     *upstream.RateLimitersRegistry
	cacheDal                 common.CacheDAL
	metricsTracker           *health.Tracker
	upstreamsRegistry        *upstream.UpstreamsRegistry
	selectionPolicyEvaluator *PolicyEvaluator
	initializer              *util.Initializer
}

func (n *Network) Bootstrap(ctx context.Context) error {
	// Initialize policy evaluator if configured
	if n.cfg.SelectionPolicy != nil {
		evaluator, e := NewPolicyEvaluator(n.networkId, n.logger, n.cfg.SelectionPolicy, n.upstreamsRegistry, n.metricsTracker)
		if e != nil {
			return fmt.Errorf("failed to create selection policy evaluator: %w", e)
		}
		if e := evaluator.Start(ctx); e != nil {
			return fmt.Errorf("failed to start selection policy evaluator: %w", e)
		}
		n.selectionPolicyEvaluator = evaluator
	}

	return nil
}

func (n *Network) Id() string {
	return n.networkId
}

func (n *Network) Label() string {
	if n == nil {
		return ""
	}
	if n.networkLabel != "" {
		return n.networkLabel
	}
	return n.networkId
}

func (n *Network) ProjectId() string {
	return n.projectId
}

func (n *Network) Architecture() common.NetworkArchitecture {
	if n.cfg.Architecture == "" {
		if n.cfg.Evm != nil {
			n.cfg.Architecture = common.ArchitectureEvm
		}
	}

	return n.cfg.Architecture
}

func (n *Network) ShadowUpstreams() []*upstream.Upstream {
	return n.upstreamsRegistry.GetNetworkShadowUpstreams(n.networkId)
}

func (n *Network) Logger() *zerolog.Logger {
	return n.logger
}

func (n *Network) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	ctx, span := common.StartDetailSpan(ctx, "Network.EvmHighestLatestBlockNumber")
	defer span.End()

	upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	var maxBlock int64 = 0
	for _, u := range upstreams {
		statePoller := u.EvmStatePoller()
		if statePoller == nil {
			continue
		}

		// Check if the node is syncing - skip syncing nodes as their block numbers may be unreliable
		if u.EvmSyncingState() == common.EvmSyncingStateSyncing {
			n.logger.Debug().Str("upstreamId", u.Id()).Msg("skipping syncing upstream for highest latest block calculation")
			continue
		}

		// Check if upstream is excluded by selection policy
		if n.selectionPolicyEvaluator != nil {
			// We use "eth_blockNumber" as it's a common method that would be used to get latest block
			if err := n.selectionPolicyEvaluator.AcquirePermit(n.logger, u, "eth_blockNumber"); err != nil {
				n.logger.Debug().Str("upstreamId", u.Id()).Err(err).Msg("skipping upstream excluded by selection policy for highest latest block calculation")
				continue
			}
		}

		upBlock := statePoller.LatestBlock()
		if upBlock > maxBlock {
			maxBlock = upBlock
		}
	}
	span.SetAttributes(attribute.Int64("highest_latest_block", maxBlock))
	return maxBlock
}

func (n *Network) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	ctx, span := common.StartDetailSpan(ctx, "Network.EvmHighestFinalizedBlockNumber", trace.WithAttributes(
		attribute.String("network.id", n.networkId),
	))
	defer span.End()

	upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	var maxBlock int64 = 0
	for _, u := range upstreams {
		statePoller := u.EvmStatePoller()
		if statePoller == nil {
			continue
		}

		// Check if the node is syncing - skip syncing nodes as their block numbers may be unreliable
		if u.EvmSyncingState() == common.EvmSyncingStateSyncing {
			n.logger.Debug().Str("upstreamId", u.Id()).Msg("skipping syncing upstream for highest finalized block calculation")
			continue
		}

		// Check if upstream is excluded by selection policy
		if n.selectionPolicyEvaluator != nil {
			// We use "eth_getBlockByNumber" as it's a common method that would be used to get finalized block
			if err := n.selectionPolicyEvaluator.AcquirePermit(n.logger, u, "eth_getBlockByNumber"); err != nil {
				n.logger.Debug().Str("upstreamId", u.Id()).Err(err).Msg("skipping upstream excluded by selection policy for highest finalized block calculation")
				continue
			}
		}

		upBlock := statePoller.FinalizedBlock()
		if upBlock > maxBlock {
			maxBlock = upBlock
		}
	}
	span.SetAttributes(attribute.Int64("highest_finalized_block", maxBlock))
	return maxBlock
}

func (n *Network) EvmLowestFinalizedBlockNumber(ctx context.Context) int64 {
	ctx, span := common.StartDetailSpan(ctx, "Network.EvmLowestFinalizedBlockNumber", trace.WithAttributes(
		attribute.String("network.id", n.networkId),
	))
	defer span.End()

	upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	var minBlock int64 = 0
	var initialized bool = false

	for _, u := range upstreams {
		statePoller := u.EvmStatePoller()
		if statePoller == nil {
			continue
		}

		// Check if the node is syncing - skip syncing nodes as their block numbers may be unreliable
		if u.EvmSyncingState() == common.EvmSyncingStateSyncing {
			n.logger.Debug().Str("upstreamId", u.Id()).Msg("skipping syncing upstream for lowest finalized block calculation")
			continue
		}

		// Check if upstream is excluded by selection policy
		if n.selectionPolicyEvaluator != nil {
			// We use "eth_getBlockByNumber" as it's a common method that would be used to get finalized block
			if err := n.selectionPolicyEvaluator.AcquirePermit(n.logger, u, "eth_getBlockByNumber"); err != nil {
				n.logger.Debug().Str("upstreamId", u.Id()).Err(err).Msg("skipping upstream excluded by selection policy for lowest finalized block calculation")
				continue
			}
		}

		upBlock := statePoller.FinalizedBlock()
		// Skip upstreams that haven't determined finalized block yet (returning 0)
		if upBlock > 0 {
			if !initialized || upBlock < minBlock {
				minBlock = upBlock
				initialized = true
			}
		}
	}

	span.SetAttributes(attribute.Int64("lowest_finalized_block", minBlock))
	return minBlock
}

func (n *Network) EvmLeaderUpstream(ctx context.Context) common.Upstream {
	var leader common.Upstream
	var leaderLastBlock int64 = 0
	upsList := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
	for _, u := range upsList {
		if statePoller := u.EvmStatePoller(); statePoller != nil {
			lastBlock := statePoller.LatestBlock()
			if lastBlock > leaderLastBlock {
				leader = u
				leaderLastBlock = lastBlock
			}
		}
	}
	return leader
}

func (n *Network) getFailsafeExecutor(ctx context.Context, req *common.NormalizedRequest) *FailsafeExecutor {
	method, _ := req.Method()
	finality := req.Finality(ctx)

	// First, try to find a specific match for both method and finality
	for _, fe := range n.failsafeExecutors {
		if fe.method != "*" && len(fe.finalities) > 0 {
			matched, _ := common.WildcardMatch(fe.method, method)
			if matched && slices.Contains(fe.finalities, finality) {
				return fe
			}
		}
	}

	// Then, try to find a match for method only (empty finalities means any finality)
	for _, fe := range n.failsafeExecutors {
		if fe.method != "*" && len(fe.finalities) == 0 {
			matched, _ := common.WildcardMatch(fe.method, method)
			if matched {
				return fe
			}
		}
	}

	// Then, try to find a match for finality only
	for _, fe := range n.failsafeExecutors {
		if fe.method == "*" && len(fe.finalities) > 0 {
			if slices.Contains(fe.finalities, finality) {
				return fe
			}
		}
	}

	// Return the first generic executor if no specific one is found (method = "*", finalities = nil)
	for _, fe := range n.failsafeExecutors {
		if fe.method == "*" && len(fe.finalities) == 0 {
			return fe
		}
	}

	return nil
}

func (n *Network) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	startTime := time.Now()
	req.SetNetwork(n)
	req.SetCacheDal(n.cacheDal)

	method, _ := req.Method()
	lg := n.logger.With().Str("method", method).Interface("id", req.ID()).Str("ptr", fmt.Sprintf("%p", req)).Logger()

	// Start a span for network forwarding
	ctx, forwardSpan := common.StartSpan(ctx, "Network.Forward",
		trace.WithAttributes(
			attribute.String("network.id", n.networkId),
			attribute.String("request.method", method),
			attribute.String("request.finality", req.Finality(ctx).String()),
		),
	)
	if common.IsTracingDetailed {
		forwardSpan.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
		)
	}
	defer forwardSpan.End()

	if lg.GetLevel() == zerolog.TraceLevel {
		lg.Debug().Object("request", req).Msgf("forwarding request for network")
	} else {
		lg.Debug().Msgf("forwarding request for network")
	}

	mlx, resp, err := n.handleMultiplexing(ctx, &lg, req, startTime)
	if err != nil || resp != nil {
		// When the original request is already fulfilled by multiplexer
		forwardSpan.SetAttributes(attribute.Bool("multiplexed", true))
		if err != nil {
			common.SetTraceSpanError(forwardSpan, err)
		}
		return resp, err
	}
	if mlx != nil {
		// If we decided to multiplex, we need to make sure to clean up the multiplexer
		defer n.cleanupMultiplexer(mlx)
	}

	if n.cacheDal != nil && !req.SkipCacheRead() {
		lg.Debug().Msgf("checking cache for request")
		resp, err := n.cacheDal.Get(ctx, req)
		if err != nil {
			lg.Debug().Err(err).Msgf("could not find response in cache")
		} else if resp != nil && !resp.IsObjectNull(ctx) {
			if lg.GetLevel() <= zerolog.DebugLevel {
				lg.Debug().Object("response", resp).Msgf("response served from cache")
			} else {
				lg.Info().Msgf("response served from cache")
			}
			if mlx != nil {
				mlx.Close(ctx, resp, err)
			}
			forwardSpan.SetAttributes(attribute.Bool("cache.hit", true))
			return resp, err
		}
		forwardSpan.SetAttributes(attribute.Bool("cache.hit", false))
	}

	_, upstreamSpan := common.StartDetailSpan(ctx, "GetSortedUpstreams")
	upsList, err := n.upstreamsRegistry.GetSortedUpstreams(ctx, n.networkId, method)
	upstreamSpan.SetAttributes(attribute.Int("upstreams.count", len(upsList)))
	if common.IsTracingDetailed {
		names := make([]string, len(upsList))
		for i, u := range upsList {
			names[i] = u.Id()
		}
		upstreamSpan.SetAttributes(
			attribute.String("upstreams.list", strings.Join(names, ", ")),
		)
	}
	upstreamSpan.End()

	if err != nil {
		common.SetTraceSpanError(forwardSpan, err)
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	// Set upstreams on the request
	req.SetUpstreams(upsList)

	// Network-level pre-forward (executed after upstream selection) for upstream-aware logic
	if handled, resp, err := evm.HandleNetworkPreForward(ctx, n, upsList, req); handled {
		if err != nil {
			if mlx != nil {
				mlx.Close(ctx, nil, err)
			}
			return nil, err
		}
		if mlx != nil {
			mlx.Close(ctx, resp, nil)
		}
		return resp, nil
	}

	// 3) Check if we should handle this method on this network
	if err := n.shouldHandleMethod(req, method, upsList); err != nil {
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	// 3) Apply rate limits
	if err := n.acquireRateLimitPermit(ctx, req); err != nil {
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	// 4) Prepare the request
	if err := n.prepareRequest(ctx, req); err != nil {
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	// 5) Iterate over upstreams and forward the request until success or fatal failure
	tryForward := func(
		u common.Upstream,
		req *common.NormalizedRequest,
		execSpanCtx context.Context,
		lg *zerolog.Logger,
		hedge int,
		attempt int,
		retry int,
	) (resp *common.NormalizedResponse, err error) {
		ctx, span := common.StartDetailSpan(execSpanCtx, "Network.TryForward")
		defer span.End()

		lg.Debug().Int("hedge", hedge).Int("attempt", attempt).Int("retry", retry).Msgf("trying to forward request to upstream")

		if err := n.acquireSelectionPolicyPermit(ctx, lg, u, req); err != nil {
			return nil, err
		}

		resp, err = n.doForward(ctx, u, req, false)

		if err != nil && !common.IsNull(err) {
			// If upstream complains that the method is not supported let's dynamically add it ignoreMethods config
			if common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
				go u.IgnoreMethod(method)
			}

			lg.Debug().Object("response", resp).Err(err).Msgf("finished forwarding request to upstream with error")
			return nil, err
		}

		lg.Debug().Object("response", resp).Msgf("finished forwarding request to upstream with success")

		return resp, err
	}
	// This is the only way to pass additional values to failsafe policy executors context
	ectx := context.WithValue(ctx, common.RequestContextKey, req)

	failsafeExecutor := n.getFailsafeExecutor(ctx, req)
	if failsafeExecutor == nil {
		return nil, errors.New("no failsafe executor found for this request")
	}

	resp, execErr := failsafeExecutor.executor.
		WithContext(ectx).
		GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
			lg.Trace().
				Int("attempt", exec.Attempts()).
				Int("retry", exec.Retries()).
				Int("hedge", exec.Hedges()).
				Msgf("execution attempt for network forwarding")

			execSpanCtx, execSpan := common.StartSpan(exec.Context(), "Network.forwardAttempt",
				trace.WithAttributes(
					attribute.String("network.id", n.networkId),
					attribute.String("request.method", method),
					attribute.Int("execution.attempt", exec.Attempts()),
					attribute.Int("execution.retry", exec.Retries()),
					attribute.Int("execution.hedge", exec.Hedges()),
				),
			)
			defer execSpan.End()

			// Use a local variable to avoid overwriting the captured req variable
			// which can cause issues when multiple executions run concurrently (e.g., consensus)
			// Be defensive about the type assertion to avoid panics if the context value was not set properly.
			var effectiveReq *common.NormalizedRequest
			if or := execSpanCtx.Value(common.RequestContextKey); or != nil {
				if r, ok := or.(*common.NormalizedRequest); ok && r != nil {
					effectiveReq = r
				} else {
					effectiveReq = req
				}
			} else {
				effectiveReq = req
			}

			if common.IsTracingDetailed {
				execSpan.SetAttributes(
					attribute.String("request.id", fmt.Sprintf("%v", effectiveReq.ID())),
				)
			}

			if ctxErr := execSpanCtx.Err(); ctxErr != nil {
				cause := context.Cause(execSpanCtx)
				if cause != nil {
					common.SetTraceSpanError(execSpan, cause)
					return nil, cause
				} else {
					common.SetTraceSpanError(execSpan, ctxErr)
					return nil, ctxErr
				}
			}
			if failsafeExecutor.timeout != nil {
				var cancelFn context.CancelFunc
				execSpanCtx, cancelFn = context.WithTimeout(
					execSpanCtx,
					// TODO Carrying the timeout helps setting correct timeout on actual http request to upstream (during batch mode).
					//      Is there a way to do this cleanly? e.g. if failsafe lib works via context rather than Ticker?
					//      5ms is a workaround to ensure context carries the timeout deadline (used when calling upstreams),
					//      but allow the failsafe execution to fail with timeout first for proper error handling.
					*failsafeExecutor.timeout+5*time.Millisecond,
				)

				defer cancelFn()
			}

			var err error
			for {
				loopCtx, loopSpan := common.StartDetailSpan(execSpanCtx, "Network.UpstreamLoop")
				if ctxErr := loopCtx.Err(); ctxErr != nil {
					cause := context.Cause(loopCtx)
					if cause != nil {
						common.SetTraceSpanError(loopSpan, cause)
						loopSpan.End()
						return nil, cause
					} else {
						common.SetTraceSpanError(loopSpan, ctxErr)
						loopSpan.End()
						return nil, ctxErr
					}
				}

				// Get next available upstream
				u, err := effectiveReq.NextUpstream()
				if err != nil {
					// No more available upstreams
					loopSpan.SetAttributes(
						attribute.Bool("upstreams_exhausted", true),
						attribute.String("error", err.Error()),
					)
					loopSpan.End()
					break
				}

				loopSpan.SetAttributes(attribute.String("upstream.id", u.Id()))
				ulg := lg.With().Str("upstreamId", u.Id()).Logger()
				ulg.Debug().
					Interface("id", effectiveReq.ID()).
					Str("ptr", fmt.Sprintf("%p", effectiveReq)).
					Str("selectedUpstream", u.Id()).
					Msg("selected upstream from list")

				// Network-level per-upstream gating based on block availability (after block tag interpolation)
				if !n.shouldUseUpstream(ctx, u, effectiveReq, method) {
					ulg.Debug().Msg("skipping upstream due to block availability gating")
					loopSpan.End()
					continue
				}

				hedges := exec.Hedges()
				attempts := exec.Attempts()
				if hedges > 0 {
					finality := effectiveReq.Finality(loopCtx)
					telemetry.CounterHandle(telemetry.MetricNetworkHedgedRequestTotal,
						n.projectId, n.Label(), u.Id(), method, fmt.Sprintf("%d", hedges), finality.String(), effectiveReq.UserId(), effectiveReq.AgentName(),
					).Inc()
				}

				var r *common.NormalizedResponse
				r, err = tryForward(u, effectiveReq, loopCtx, &ulg, hedges, attempts, exec.Retries())

				if e := n.normalizeResponse(loopCtx, effectiveReq, r); e != nil {
					ulg.Error().Err(e).Msgf("failed to normalize response")
					err = e
				}

				effectiveReq.MarkUpstreamCompleted(loopCtx, u, r, err)

				isClientErr := common.IsClientError(err)
				if hedges > 0 && common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled) {
					ulg.Debug().Err(err).Msgf("discarding hedged request to upstream")
					finality := effectiveReq.Finality(loopCtx)
					telemetry.CounterHandle(telemetry.MetricNetworkHedgeDiscardsTotal,
						n.projectId, n.Label(), u.Id(), method, fmt.Sprintf("%d", attempts), fmt.Sprintf("%d", hedges), finality.String(), effectiveReq.UserId(), effectiveReq.AgentName(),
					).Inc()
					// Release any response associated with the discarded hedge to avoid retaining buffers
					if r != nil {
						r.Release()
						r = nil
					}
					err := common.NewErrUpstreamHedgeCancelled(u.Id(), err)
					common.SetTraceSpanError(loopSpan, err)
					return nil, err
				}

				if err != nil {
					loopSpan.SetAttributes(
						attribute.Bool("error.is_client", isClientErr),
					)
				}

				if r != nil {
					r.SetUpstream(u)
				}

				if !common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped) {
					if err == nil {
						loopSpan.SetStatus(codes.Ok, "")
					} else {
						common.SetTraceSpanError(loopSpan, err)
					}
					if r != nil {
						if err == nil {
							// Store execution metadata only for winning attempts
							r.SetAttempts(exec.Attempts())
							r.SetRetries(exec.Retries())
							r.SetHedges(exec.Hedges())
						} else {
							// On error, release non-winning responses
							r.Release()
							r = nil
						}
					}
					loopSpan.End()
					return r, err
				}

				// For skipped requests, ensure any response is not retained before continuing
				if r != nil {
					r.Release()
					r = nil
				}

				loopSpan.End()
			}

			err = common.NewErrUpstreamsExhausted(
				effectiveReq,
				&effectiveReq.ErrorsByUpstream,
				n.projectId,
				n.networkId,
				method,
				time.Since(startTime),
				exec.Attempts(),
				exec.Retries(),
				exec.Hedges(),
				len(upsList),
			)
			common.SetTraceSpanError(execSpan, err)
			return nil, err
		})

	req.RLockWithTrace(ctx)
	defer req.RUnlock()

	if execErr != nil {
		translatedErr := upstream.TranslateFailsafeError(common.ScopeNetwork, "", method, execErr, &startTime)
		// Don't override consensus results with last valid response from individual upstreams
		// For example if 1 upstream gives empty response another 3 give "reverted" error,
		// we should still return reverted error, even though there was an empty response before.
		if failsafeExecutor.consensusPolicyEnabled {
			if mlx != nil {
				mlx.Close(ctx, nil, translatedErr)
			}
			// Ensure any lastValidResponse kept on the request is released and cleared
			if lvr := req.LastValidResponse(); lvr != nil {
				lvr.Release()
				req.ClearLastValidResponse()
			}
			return nil, translatedErr
		}

		lvr := req.LastValidResponse()
		if lvr != nil && !lvr.IsObjectNull() {
			// A valid response is a json-rpc response without "error" object.
			// This mechanism is needed in these two scenarios:
			//
			// 1) If error is due to empty response be generous and accept it,
			// because this means after many retries or exhausting all upstreams still no data is available.
			// We don't need to worry about wrongly replying empty responses for unfinalized data
			// because cache layer has mechanism to deal with empty and/or unfinalized data.
			//
			// 2) For pending txs we can accept the response, if after retries it is still pending.
			// This avoids failing with "retry" error, when we actually do have a response but blockNumber is null since tx is pending.
			resp = lvr
			req.SetLastUpstream(resp.Upstream())
		} else {
			if mlx != nil {
				mlx.Close(ctx, nil, translatedErr)
			}
			return nil, translatedErr
		}
	}

	if resp != nil {
		if n.cacheDal != nil {
			resp.RLockWithTrace(ctx)
			// Hold a reference so Release waits for cache-set to complete
			resp.AddRef()

			go (func(resp *common.NormalizedResponse, forwardSpan trace.Span) {
				defer (func() {
					if rec := recover(); rec != nil {
						telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
							"cache-set",
							fmt.Sprintf("network:%s method:%s", n.networkId, method),
							common.ErrorFingerprint(rec),
						).Inc()
						lg.Error().
							Interface("panic", rec).
							Str("stack", string(debug.Stack())).
							Msgf("unexpected panic on cache-set")
					}
				})()
				defer resp.RUnlock()
				defer resp.DoneRef()

				timeoutCtx, timeoutCtxCancel := context.WithTimeoutCause(n.appCtx, 10*time.Second, errors.New("cache driver timeout during set"))
				defer timeoutCtxCancel()
				tracedCtx := trace.ContextWithSpanContext(timeoutCtx, forwardSpan.SpanContext())
				err := n.cacheDal.Set(tracedCtx, req, resp)
				if err != nil {
					lg.Warn().Err(err).Msgf("could not store response in cache")
				}
			})(resp, forwardSpan)
		}

		// Use the counters embedded earlier in the response
		forwardSpan.SetAttributes(
			attribute.Int("execution.attempts", int(resp.Attempts())),
			attribute.Int("execution.retries", int(resp.Retries())),
			attribute.Int("execution.hedges", int(resp.Hedges())),
		)
	}

	isEmpty := resp == nil || resp.IsObjectNull(ctx) || resp.IsResultEmptyish(ctx)
	if isEmpty {
		lg.Trace().Msgf("response is empty")
	}

	if execErr == nil && !isEmpty {
		n.enrichStatePoller(ctx, method, req, resp)

		// If response is not empty, but at least one upstream responded empty we track in a metric.
		req.EmptyResponses.Range(func(key, value any) bool {
			upstream := key.(*upstream.Upstream)
			finality := req.Finality(ctx)
			telemetry.MetricUpstreamWrongEmptyResponseTotal.WithLabelValues(
				n.projectId,
				upstream.VendorName(),
				n.Label(),
				upstream.Id(),
				method,
				finality.String(),
				req.UserId(),
				req.AgentName(),
			).Inc()
			if upstream != nil {
				if mt := upstream.MetricsTracker(); mt != nil {
					// We would like to penalize the upstream if another upstream responded with data,
					// but this upstream responded empty.
					if r, ok := value.(error); ok {
						mt.RecordUpstreamFailure(upstream, method, r)
					} else {
						mt.RecordUpstreamFailure(upstream, method, common.NewErrEndpointMissingData(fmt.Errorf("upstream responded emptyish"), upstream))
					}
				}
			}
			return true
		})
	}

	if mlx != nil {
		mlx.Close(ctx, resp, nil)
	}

	// After consensus success, ensure we don't keep a loser in req.LastValidResponse
	if failsafeExecutor.consensusPolicyEnabled {
		if lvr := req.LastValidResponse(); lvr != nil && lvr != resp {
			lvr.Release()
			req.ClearLastValidResponse()
		}
	}

	return resp, nil
}

func (n *Network) prepareRequest(ctx context.Context, nr *common.NormalizedRequest) error {
	switch n.Architecture() {
	case common.ArchitectureEvm:
		jsonRpcReq, err := nr.JsonRpcRequest(ctx)
		if err != nil {
			return common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorParseException,
				"failed to unmarshal json-rpc request",
				err,
				nil,
			)
		}
		evm.NormalizeHttpJsonRpc(ctx, nr, jsonRpcReq)
	default:
		return common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorServerSideException,
			fmt.Sprintf("unsupported architecture: %s for network: %s", n.Architecture(), n.Id()),
			nil,
			nil,
		)
	}

	return nil
}

func (n *Network) GetMethodMetrics(method string) common.TrackedMetrics {
	if method == "" {
		return nil
	}

	mt := n.metricsTracker.GetNetworkMethodMetrics(n.networkId, method)
	return mt
}

func (n *Network) Config() *common.NetworkConfig {
	return n.cfg
}

func (n *Network) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	ctx, span := common.StartDetailSpan(ctx, "Network.GetFinality")
	defer span.End()

	finality := common.DataFinalityStateUnknown

	if req == nil && resp == nil {
		return finality
	}

	method, _ := req.Method()
	if n.cfg.Methods != nil && n.cfg.Methods.Definitions != nil {
		if cfg, ok := n.cfg.Methods.Definitions[method]; ok {
			if cfg.Finalized {
				finality = common.DataFinalityStateFinalized
				return finality
			} else if cfg.Realtime {
				finality = common.DataFinalityStateRealtime
				return finality
			}
		}
	}

	blockRef, blockNumber, _ := evm.ExtractBlockReferenceFromRequest(ctx, req)

	if blockRef == "*" && blockNumber == 0 {
		finality = common.DataFinalityStateUnfinalized
		return finality
	} else if blockRef != "" && blockRef != "*" && (blockRef[0] < '0' || blockRef[0] > '9') {
		finality = common.DataFinalityStateRealtime
		return finality
	} else if blockNumber > 0 && resp != nil {
		upstream := resp.Upstream()
		if upstream != nil {
			if ups, ok := upstream.(common.EvmUpstream); ok {
				if isFinalized, err := ups.EvmIsBlockFinalized(ctx, blockNumber, false); err == nil {
					if isFinalized {
						finality = common.DataFinalityStateFinalized
					} else {
						finality = common.DataFinalityStateUnfinalized
					}
				}
			}
		}
	}

	if blockNumber > 0 && finality == common.DataFinalityStateUnknown {
		// When we have a block number but no response yet, try multiple fallbacks

		// First, try to use LastUpstream from the request (if this is a retry or subsequent attempt)
		upstream := req.LastUpstream()
		if upstream != nil {
			if ups, ok := upstream.(common.EvmUpstream); ok {
				if isFinalized, err := ups.EvmIsBlockFinalized(ctx, blockNumber, false); err == nil {
					if isFinalized {
						finality = common.DataFinalityStateFinalized
					} else {
						finality = common.DataFinalityStateUnfinalized
					}
					return finality
				}
			}
		}

		// If no LastUpstream, use the network's lowest finalized block as a heuristic
		// This provides a conservative estimate for early metrics collection
		if n.upstreamsRegistry != nil {
			lowestFinalized := n.EvmLowestFinalizedBlockNumber(ctx)
			if lowestFinalized > 0 {
				if blockNumber <= lowestFinalized {
					finality = common.DataFinalityStateFinalized
				} else {
					finality = common.DataFinalityStateUnfinalized
				}
				span.SetAttributes(
					attribute.Bool("used_network_finalized_heuristic", true),
					attribute.Int64("lowest_finalized", lowestFinalized),
				)
			}
		}
		// If we still can't determine, it remains unknown
	}

	return finality
}

func (n *Network) doForward(execSpanCtx context.Context, u common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (*common.NormalizedResponse, error) {
	switch n.cfg.Architecture {
	case common.ArchitectureEvm:
		if handled, resp, err := evm.HandleUpstreamPreForward(execSpanCtx, n, u, req, skipCacheRead); handled {
			return evm.HandleUpstreamPostForward(execSpanCtx, n, u, req, resp, err, skipCacheRead)
		}
	}

	// If not handled, then fallback to the normal forward
	resp, err := u.Forward(execSpanCtx, req, false)
	return evm.HandleUpstreamPostForward(execSpanCtx, n, u, req, resp, err, skipCacheRead)
}

// shouldUseUpstream performs per-upstream gating for the request based on block availability.
// It is invoked just before forwarding to an upstream to avoid copying/filtering the list.
func (n *Network) shouldUseUpstream(ctx context.Context, u common.Upstream, req *common.NormalizedRequest, method string) bool {
	if n.cfg.Architecture != common.ArchitectureEvm {
		return true
	}
	// Check config toggles: method-level overrides network-level; default is enabled
	enforce := true
	if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.EnforceBlockAvailability != nil {
		enforce = *n.cfg.Evm.EnforceBlockAvailability
	}
	// Method-level override from network config
	if n.cfg != nil && n.cfg.Methods != nil && n.cfg.Methods.Definitions != nil {
		if mc, ok := n.cfg.Methods.Definitions[method]; ok && mc != nil && mc.EnforceBlockAvailability != nil {
			enforce = *mc.EnforceBlockAvailability
		}
	}
	// If still unset by method config, check default method definitions (common defaults)
	if enforce && common.DefaultWithBlockCacheMethods != nil {
		if dmc, ok := common.DefaultWithBlockCacheMethods[method]; ok && dmc != nil && dmc.EnforceBlockAvailability != nil {
			enforce = *dmc.EnforceBlockAvailability
		}
	}
	if !enforce {
		return true
	}
	// Use cached block number from normalization to avoid re-extracting from mutated params
	var bn int64
	if v := req.EvmBlockNumber(); v != nil {
		if n64, ok := v.(int64); ok {
			bn = n64
		}
	}
	if bn <= 0 {
		// If still unknown, skip gating
		return true
	}
	eu, ok := u.(common.EvmUpstream)
	if !ok {
		return true
	}
	available, err := eu.EvmAssertBlockAvailability(ctx, method, common.AvailbilityConfidenceBlockHead, true, bn)
	if err != nil {
		return false
	}
	return available
}

func (n *Network) acquireSelectionPolicyPermit(ctx context.Context, lg *zerolog.Logger, ups common.Upstream, req *common.NormalizedRequest) error {
	if n.cfg.SelectionPolicy == nil {
		return nil
	}
	_, span := common.StartDetailSpan(ctx, "Network.AcquireSelectionPolicyPermit")
	defer span.End()

	method, err := req.Method()
	if err != nil {
		common.SetTraceSpanError(span, err)
		return err
	}

	if dr := req.Directives(); dr != nil {
		// If directive is instructed to use specific upstream(s), bypass selection policy evaluation
		if dr.UseUpstream != "" {
			span.SetAttributes(attribute.String("force_use_upstream", dr.UseUpstream))
			return nil
		}
	}

	return n.selectionPolicyEvaluator.AcquirePermit(lg, ups, method)
}

func (n *Network) handleMultiplexing(ctx context.Context, lg *zerolog.Logger, req *common.NormalizedRequest, startTime time.Time) (*Multiplexer, *common.NormalizedResponse, error) {
	mlxHash, err := req.CacheHash()
	lg.Trace().Str("hash", mlxHash).Object("request", req).Msgf("checking if multiplexing is possible")
	if err != nil || mlxHash == "" {
		lg.Debug().Str("hash", mlxHash).Err(err).Object("request", req).Msgf("could not get multiplexing hash for request")
		return nil, nil, nil
	}

	// Check for existing multiplexer
	if vinf, exists := n.inFlightRequests.Load(mlxHash); exists {
		inf := vinf.(*Multiplexer)
		method, _ := req.Method()
		finality := req.Finality(ctx)
		telemetry.CounterHandle(telemetry.MetricNetworkMultiplexedRequests,
			n.projectId, n.Label(), method, finality.String(), req.UserId(), req.AgentName(),
		).Inc()

		lg.Debug().Str("hash", mlxHash).Msgf("found identical request initiating multiplexer")

		resp, err := n.waitForMultiplexResult(ctx, inf, req, startTime)

		lg.Trace().Str("hash", mlxHash).Object("response", resp).Err(err).Msgf("multiplexed request result")

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
	ctx, span := common.StartSpan(ctx, "Network.WaitForMultiplexResult")
	defer span.End()

	// Check if result is already available
	mlx.mu.Lock()
	if mlx.resp != nil || mlx.err != nil {
		// Clone the stored response while holding the lock to avoid races with cleanup
		out, err := common.CopyResponseForRequest(ctx, mlx.resp, req)
		if err != nil {
			return nil, err
		}
		mlx.mu.Unlock()
		return out, mlx.err
	}
	mlx.mu.Unlock()

	// Wait for result
	select {
	case <-mlx.done:
		// Need to lock when accessing mlx.resp to avoid race with cleanupMultiplexer
		mlx.mu.Lock()
		out, err := common.CopyResponseForRequest(ctx, mlx.resp, req)
		if err != nil {
			return nil, err
		}
		mlx.mu.Unlock()
		return out, mlx.err
	case <-ctx.Done():
		n.cleanupMultiplexer(mlx)
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, common.NewErrNetworkRequestTimeout(time.Since(startTime), err)
		}
		return nil, err
	}
}

func (n *Network) cleanupMultiplexer(mlx *Multiplexer) {
	mlx.mu.Lock()
	defer mlx.mu.Unlock()

	if mlx.resp != nil {
		mlx.resp.Release()
		mlx.resp = nil
	}

	n.inFlightRequests.Delete(mlx.hash)
}

func (n *Network) shouldHandleMethod(req *common.NormalizedRequest, method string, upsList []common.Upstream) error {
	// Check stateful methods policy
	// Methods.Definitions is guaranteed to be populated by SetDefaults() with all necessary stateful methods
	isStateful := false
	if n.cfg != nil && n.cfg.Methods != nil && n.cfg.Methods.Definitions != nil {
		if mc, ok := n.cfg.Methods.Definitions[method]; ok && mc != nil {
			isStateful = mc.Stateful
		}
	}
	if isStateful {
		// Determine targeted upstream count
		targeted := 0
		if dr := req.Directives(); dr != nil && dr.UseUpstream != "" {
			for _, u := range upsList {
				if match, _ := common.WildcardMatch(dr.UseUpstream, u.Id()); match {
					targeted++
				}
			}
		} else {
			targeted = len(upsList)
		}
		if targeted > 1 {
			return common.NewErrNotImplemented("stateful method requires a single targeted upstream; either configure only 1 upstream or set Use-Upstream to a single upstream id")
		}
	}

	if method == "eth_accounts" || method == "eth_sign" {
		return common.NewErrNotImplemented("eth_accounts and eth_sign are not supported")
	}

	return nil
}

func (n *Network) enrichStatePoller(ctx context.Context, method string, req *common.NormalizedRequest, resp *common.NormalizedResponse) {
	ctx, span := common.StartDetailSpan(ctx, "Network.EnrichStatePoller")
	defer span.End()

	switch n.Architecture() {
	case common.ArchitectureEvm:
		// TODO Move the logic to evm package as a post-forward hook?
		if method == "eth_getBlockByNumber" {
			jrq, err := req.JsonRpcRequest(ctx)
			if err != nil {
				return
			}
			jrq.RLock()
			defer jrq.RUnlock()
			if blkTag, ok := jrq.Params[0].(string); ok {
				if blkTag == "finalized" || blkTag == "latest" {
					jrs, _ := resp.JsonRpcResponse(ctx)
					bnh, err := jrs.PeekStringByPath(ctx, "number")
					if err == nil {
						blockNumber, err := common.HexToInt64(bnh)
						if err == nil {
							if ups := resp.Upstream(); ups != nil {
								if ups, ok := ups.(common.EvmUpstream); ok {
									// These methods are non-blocking and handle async updates internally
									switch blkTag {
									case "finalized":
										ups.EvmStatePoller().SuggestFinalizedBlock(blockNumber)
									case "latest":
										ups.EvmStatePoller().SuggestLatestBlock(blockNumber)
									}
								}
							}
						}
					}
				}
			}
		} else if method == "eth_blockNumber" {
			jrs, _ := resp.JsonRpcResponse(ctx)
			bnh, err := jrs.PeekStringByPath(ctx)
			if err == nil {
				blockNumber, err := common.HexToInt64(bnh)
				if err == nil {
					if ups := resp.Upstream(); ups != nil {
						if ups, ok := ups.(common.EvmUpstream); ok {
							// This method is non-blocking and handles async updates internally
							ups.EvmStatePoller().SuggestLatestBlock(blockNumber)
						}
					}
				}
			}
		}
	}
}

func (n *Network) normalizeResponse(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	ctx, span := common.StartDetailSpan(ctx, "Network.NormalizeResponse")
	defer span.End()

	switch n.Architecture() {
	case common.ArchitectureEvm:
		if resp != nil {
			// This ensures that even if upstream gives us wrong/missing ID we'll
			// use correct one from original incoming request.
			if jrr, err := resp.JsonRpcResponse(ctx); err == nil && jrr != nil {
				jrq, err := req.JsonRpcRequest(ctx)
				if err != nil {
					return err
				}
				if err := jrr.SetID(jrq.ID); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (n *Network) acquireRateLimitPermit(ctx context.Context, req *common.NormalizedRequest) error {
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
	lg := n.logger.With().Str("method", method).Logger()

	rules, errRules := rlb.GetRulesByMethod(method)
	if errRules != nil {
		return errRules
	}
	lg.Debug().Msgf("found %d network-level rate limiters", len(rules))

	if len(rules) > 0 {
		allowed, err := rlb.TryAcquirePermit(ctx, n.projectId, req, method, "", "", "", "network")
		if err != nil {
			return err
		}
		if !allowed {
			// Blocked event already recorded in budget.TryAcquirePermit; avoid double recording here
			return common.NewErrNetworkRateLimitRuleExceeded(
				n.projectId,
				n.networkId,
				n.cfg.RateLimitBudget,
				fmt.Sprintf("method:%s", method),
			)
		}
	}

	return nil
}
