package erpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

var networkCacheHitLogSampler = &zerolog.BasicSampler{N: 100}

const (
	maxConcurrentNetworkCacheWrites       = 100
	networkPostCompletionCoalescingWindow = 40 * time.Millisecond
	networkDeterministicNegativeCacheTTL  = 200 * time.Millisecond
	networkFailsafeTimeoutSlack           = 30 * time.Millisecond
)

type FailsafeExecutor struct {
	method                 string
	finalities             []common.DataFinalityState
	upstreamGroup          string
	executor               failsafe.Executor[*common.NormalizedResponse]
	timeout                *time.Duration
	consensusPolicyEnabled bool
	// emptyResultAccept lists methods for which the first emptyish result
	// short-circuits the upstream loop. Without this the loop tries every
	// upstream before returning to failsafe, even when the retry policy
	// would accept the empty result anyway (wasting time on slow upstreams).
	emptyResultAccept []string
}

func logCacheHit(lg *zerolog.Logger, resp *common.NormalizedResponse) {
	if lg == nil || lg.GetLevel() > zerolog.DebugLevel {
		return
	}
	sampled := lg.Sample(networkCacheHitLogSampler)
	if lg.GetLevel() <= zerolog.TraceLevel {
		sampled.Debug().Object("response", resp).Msg("response served from cache")
		return
	}
	sampled.Debug().Msg("response served from cache")
}

func (n *Network) getCacheWriteSem() chan struct{} {
	n.cacheWriteSemInit.Do(func() {
		if n.cacheWriteSem == nil {
			n.cacheWriteSem = make(chan struct{}, maxConcurrentNetworkCacheWrites)
		}
	})
	return n.cacheWriteSem
}

func (n *Network) observeCacheWriteQueueDepth(sem chan struct{}) {
	if sem == nil {
		return
	}
	telemetry.MetricNetworkCacheWriteQueueDepth.WithLabelValues(
		n.projectId,
		n.Label(),
	).Set(float64(len(sem)))
}

func classifyAttemptReason(consensusEnabled bool, retries, hedges int) string {
	switch {
	case consensusEnabled:
		return telemetry.AttemptReasonConsensus
	case hedges > 0:
		return telemetry.AttemptReasonHedge
	case retries > 0:
		return telemetry.AttemptReasonRetry
	default:
		return ""
	}
}

type getSortedUpstreamsForNetworkFn func(
	ctx context.Context,
	registry *upstream.UpstreamsRegistry,
	networkID string,
	method string,
) ([]common.Upstream, error)

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
	getSortedUpstreamsFn     getSortedUpstreamsForNetworkFn
	cacheWriteSem            chan struct{}
	cacheWriteSemInit        sync.Once
	negativeResultCache      *sync.Map
	postCompletionResults    *sync.Map
}

type deterministicNegativeCacheEntry struct {
	err       error
	expiresAt int64
}

type postCompletionResultEntry struct {
	resp      *common.NormalizedResponse
	expiresAt int64
	released  int32
}

func (e *postCompletionResultEntry) release() {
	if e == nil {
		return
	}
	if atomic.CompareAndSwapInt32(&e.released, 0, 1) {
		if e.resp != nil {
			e.resp.Release()
		}
	}
}

type skipNetworkRateLimitKey struct{}

func defaultGetSortedUpstreamsForNetwork(
	ctx context.Context,
	registry *upstream.UpstreamsRegistry,
	networkID string,
	method string,
) ([]common.Upstream, error) {
	return registry.GetSortedUpstreams(ctx, networkID, method)
}

func withSkipNetworkRateLimit(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipNetworkRateLimitKey{}, true)
}

func shouldSkipNetworkRateLimit(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if v := ctx.Value(skipNetworkRateLimitKey{}); v != nil {
		if skip, ok := v.(bool); ok && skip {
			return true
		}
	}
	return false
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

		// Use effective latest block which considers blockAvailability.upper config
		// (e.g., if upstream has latestBlockMinus: 5, use latest-5 instead of latest)
		upBlock := u.EvmEffectiveLatestBlock()
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

		// Use effective finalized block which considers blockAvailability.upper config
		upBlock := u.EvmEffectiveFinalizedBlock()
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

		// Use effective finalized block which considers blockAvailability.upper config
		upBlock := u.EvmEffectiveFinalizedBlock()
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

	// Iterate through executors in config order and return the first match.
	// This respects the user-defined priority order in the config file.
	for _, fe := range n.failsafeExecutors {
		// Check if method matches (wildcard "*" matches any method)
		methodMatches := fe.method == "*"
		if !methodMatches {
			methodMatches, _ = common.WildcardMatch(fe.method, method)
		}

		// Check if finality matches (empty finalities = any finality)
		finalityMatches := len(fe.finalities) == 0 || slices.Contains(fe.finalities, finality)

		if methodMatches && finalityMatches {
			return fe
		}
	}

	return nil
}

func (n *Network) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	startTime := time.Now()
	req.SetNetwork(n)
	req.SetCacheDal(n.cacheDal)

	// Apply default directives from config
	req.ApplyDirectiveDefaults(n.cfg.DirectiveDefaults)

	method, _ := req.Method()
	lg := n.logger.With().Str("method", method).Interface("id", req.ID()).Str("ptr", fmt.Sprintf("%p", req)).Logger()
	var upstreamCalls atomic.Int64
	defer func() {
		telemetry.ObserveNetworkUpstreamCallsPerRequest(
			n.projectId,
			n.Label(),
			method,
			int(upstreamCalls.Load()),
		)
	}()

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
			attribute.String("user.id", req.UserId()),
			attribute.String("agent.name", req.AgentName()),
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
		// When the original request is already fulfilled by multiplexer (follower path)
		forwardSpan.SetAttributes(
			attribute.Bool("multiplexed", true),
			attribute.String("multiplexer.role", "follower"),
		)
		if err != nil {
			common.SetTraceSpanError(forwardSpan, err)
		}
		return resp, err
	}
	if mlx != nil {
		forwardSpan.SetAttributes(
			attribute.String("multiplexer.hash", mlx.hash),
			attribute.String("multiplexer.role", "leader"),
		)
		defer n.cleanupMultiplexer(mlx)
	}

	if n.cacheDal != nil && !req.SkipCacheRead() {
		lg.Debug().Msgf("checking cache for request")
		resp, err := n.cacheDal.Get(ctx, req)
		if err != nil {
			lg.Debug().Err(err).Msgf("could not find response in cache")
		} else if resp != nil && !resp.IsObjectNull(ctx) {
			logCacheHit(&lg, resp)
			if mlx != nil {
				mlx.Close(ctx, resp, err)
			}
			forwardSpan.SetAttributes(attribute.Bool("cache.hit", true))
			return resp, err
		}
		forwardSpan.SetAttributes(attribute.Bool("cache.hit", false))
	}

	if negErr, negHit := n.loadDeterministicNegativeCache(ctx, req, method); negHit {
		if mlx != nil {
			mlx.Close(ctx, nil, negErr)
		}
		forwardSpan.SetAttributes(attribute.Bool("negative_cache.hit", true))
		common.SetTraceSpanError(forwardSpan, negErr)
		return nil, negErr
	}
	forwardSpan.SetAttributes(attribute.Bool("negative_cache.hit", false))

	// Get failsafe executor first to know if we need to filter upstreams by group
	failsafeExecutor := n.getFailsafeExecutor(ctx, req)
	if failsafeExecutor == nil {
		err := errors.New("no failsafe executor found for this request")
		if mlx != nil {
			mlx.Close(ctx, nil, err)
		}
		return nil, err
	}

	useUpstreamDirective := ""
	if req.Directives() != nil {
		useUpstreamDirective = req.Directives().UseUpstream
	}
	requiredCapabilities := n.requiredCapabilitiesForMethod(method)

	loadUpstreams := func() ([]common.Upstream, error) {
		_, upstreamSpan := common.StartDetailSpan(ctx, "GetSortedUpstreams")
		getSortedUpstreams := n.getSortedUpstreamsFn
		if getSortedUpstreams == nil {
			getSortedUpstreams = defaultGetSortedUpstreamsForNetwork
		}
		upsList, err := getSortedUpstreams(ctx, n.upstreamsRegistry, n.networkId, method)
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
			return nil, err
		}

		// Filter upstreams by group if the failsafe executor specifies a group.
		// Skip group filtering if UseUpstream directive is set - allows targeting any upstream for debugging.
		if failsafeExecutor.upstreamGroup != "" && useUpstreamDirective == "" {
			filteredUpstreams := make([]common.Upstream, 0, len(upsList))
			for _, u := range upsList {
				if cfg := u.Config(); cfg != nil && cfg.Group == failsafeExecutor.upstreamGroup {
					filteredUpstreams = append(filteredUpstreams, u)
				}
			}
			lg.Debug().
				Str("upstreamGroup", failsafeExecutor.upstreamGroup).
				Int("originalCount", len(upsList)).
				Int("filteredCount", len(filteredUpstreams)).
				Msgf("filtered upstreams by group for failsafe policy")
			if len(filteredUpstreams) == 0 {
				return nil, common.NewErrFailsafeConfiguration(
					fmt.Errorf("no upstreams match the configured group '%s' for failsafe policy (had %d upstreams before filtering, method=%s)",
						failsafeExecutor.upstreamGroup, len(upsList), method),
					map[string]interface{}{
						"upstreamGroup":  failsafeExecutor.upstreamGroup,
						"originalCount":  len(upsList),
						"method":         method,
						"failsafeMethod": failsafeExecutor.method,
					},
				)
			}
			upsList = filteredUpstreams

			forwardSpan.SetAttributes(
				attribute.Int("upstreams.filtered_count", len(upsList)),
				attribute.String("upstreams.filter_group", failsafeExecutor.upstreamGroup),
			)
		}

		if len(requiredCapabilities) > 0 {
			filteredByCapabilities := filterUpstreamsByRequiredCapabilities(upsList, requiredCapabilities)
			if len(filteredByCapabilities) == 0 {
				return nil, common.NewErrFailsafeConfiguration(
					fmt.Errorf("no upstreams satisfy required capabilities %v for method %s", requiredCapabilities, method),
					map[string]interface{}{
						"method":               method,
						"requiredCapabilities": requiredCapabilities,
						"originalCount":        len(upsList),
					},
				)
			}
			upsList = filteredByCapabilities
			forwardSpan.SetAttributes(
				attribute.Int("upstreams.capability_filtered_count", len(upsList)),
				attribute.String("upstreams.required_capabilities", strings.Join(requiredCapabilities, ",")),
			)
		}

		return upsList, nil
	}

	var upsList []common.Upstream
	batchSelectionCache := common.BatchUpstreamSelectionCacheFromContext(ctx)
	if batchSelectionCache != nil {
		key := common.BatchUpstreamSelectionKey{
			NetworkID:     n.networkId,
			Method:        method,
			Finality:      req.Finality(ctx),
			UseUpstream:   useUpstreamDirective,
			UpstreamGroup: failsafeExecutor.upstreamGroup,
		}
		var cacheHit bool
		upsList, cacheHit, err = batchSelectionCache.Resolve(key, loadUpstreams)
		forwardSpan.SetAttributes(
			attribute.Bool("upstreams.batch_selection_cache_enabled", true),
			attribute.Bool("upstreams.batch_selection_cache_hit", cacheHit),
		)
	} else {
		upsList, err = loadUpstreams()
	}

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
	if !shouldSkipNetworkRateLimit(ctx) {
		if err := n.acquireRateLimitPermit(ctx, req); err != nil {
			if mlx != nil {
				mlx.Close(ctx, nil, err)
			}
			return nil, err
		}
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
		upstreamCalls.Add(1)

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

	// Add tracing for which failsafe policy was selected
	forwardSpan.SetAttributes(
		attribute.String("failsafe.matched_method", failsafeExecutor.method),
		attribute.String("failsafe.matched_finalities", fmt.Sprintf("%v", failsafeExecutor.finalities)),
		attribute.String("failsafe.matched_upstream_group", failsafeExecutor.upstreamGroup),
	)

	// Track time from failsafe executor start to first callback invocation
	failsafeStartTime := time.Now()

	executeForward := func() (*common.NormalizedResponse, error) {
		return failsafeExecutor.executor.
			WithContext(ectx).
			GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
				lg.Trace().
					Int("attempt", exec.Attempts()).
					Int("retry", exec.Retries()).
					Int("hedge", exec.Hedges()).
					Dur("failsafe_init_latency", time.Since(failsafeStartTime)).
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
						//      A small slack ensures context carries timeout deadline (used when calling upstreams),
						//      but allow the failsafe execution to fail with timeout first for proper error handling.
						*failsafeExecutor.timeout+networkFailsafeTimeoutSlack,
					)

					defer cancelFn()
				}

				// Try all upstreams in a single execution before returning to failsafe.
				// This ensures delays (emptyResultDelay, blockUnavailableDelay) only
				// fire after a full round of upstream attempts.
				//
				// MarkUpstreamCompleted releases empty-result and error upstreams from
				// ConsumedUpstreams, so they're available for the next failsafe retry.
				// Because UpstreamIdx wraps via modular arithmetic, NextUpstream can
				// re-select freed upstreams within the same execution. The `attempted`
				// set below detects this and breaks the loop, ensuring each upstream
				// is called at most once per execution.
				//
				// Exception: consensus requires each execution to represent exactly one
				// upstream's response so the policy can compare N independent results.
				// Without this cap, one fast execution could consume multiple upstreams
				// (reserve → try → release empty → reserve next) before other consensus
				// goroutines get their first upstream, skewing the vote.
				var bestResp *common.NormalizedResponse
				var lastErr error
				maxLoopIterations := effectiveReq.UpstreamsCount()
				if failsafeExecutor.consensusPolicyEnabled {
					maxLoopIterations = 1
				}
				attempted := make(map[string]struct{}, maxLoopIterations)

				for loopIteration := 0; loopIteration < maxLoopIterations; loopIteration++ {
					loopCtx, loopSpan := common.StartDetailSpan(execSpanCtx, "Network.UpstreamLoop")
					if ctxErr := loopCtx.Err(); ctxErr != nil {
						cause := context.Cause(loopCtx)
						if cause == nil {
							cause = ctxErr
						}
						common.SetTraceSpanError(loopSpan, cause)
						loopSpan.End()
						return nil, cause
					}

					u, selErr := effectiveReq.NextUpstream()
					if selErr != nil {
						loopSpan.SetAttributes(
							attribute.Bool("upstreams_exhausted", true),
							attribute.String("error", selErr.Error()),
						)
						loopSpan.End()
						break
					}

					if _, seen := attempted[u.Id()]; seen {
						// Already tried in this execution — MarkUpstreamCompleted freed
						// it from ConsumedUpstreams (retryable error or empty result) and
						// UpstreamIdx wrapped around. Release the reservation so the
						// upstream is available for the next failsafe retry round.
						effectiveReq.ConsumedUpstreams.Delete(u)
						loopSpan.SetAttributes(attribute.Bool("duplicate_selection", true))
						loopSpan.End()
						break
					}
					attempted[u.Id()] = struct{}{}

					loopSpan.SetAttributes(attribute.String("upstream.id", u.Id()))
					if common.IsTracingDetailed {
						loopSpan.SetAttributes(
							attribute.Float64("upstream.score", n.upstreamsRegistry.GetUpstreamScore(u.Id(), n.networkId, method)),
						)
					}
					if eu, ok := u.(common.EvmUpstream); ok {
						if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
							loopSpan.SetAttributes(
								attribute.Int64("upstream.latest_block", sp.LatestBlock()),
								attribute.Int64("upstream.finalized_block", sp.FinalizedBlock()),
							)
						}
					}

					ulg := lg.With().Str("upstreamId", u.Id()).Logger()
					ulg.Debug().
						Interface("id", effectiveReq.ID()).
						Str("ptr", fmt.Sprintf("%p", effectiveReq)).
						Str("selectedUpstream", u.Id()).
						Msg("selected upstream from list")

					// Pre-forward: block availability gating → skip to next upstream
					if skipErr, isRetryable := n.checkUpstreamBlockAvailability(loopCtx, u, effectiveReq, method); skipErr != nil {
						n.handleBlockSkip(loopCtx, loopSpan, &ulg, u, effectiveReq, method, skipErr, isRetryable)
						loopSpan.End()
						continue
					}

					hedges := exec.Hedges()
					attempts := exec.Attempts()
					retries := exec.Retries()
					if hedges > 0 {
						finality := effectiveReq.Finality(loopCtx)
						telemetry.CounterHandle(telemetry.MetricNetworkHedgedRequestTotal,
							n.projectId, n.Label(), u.Id(), method, fmt.Sprintf("%d", hedges),
							finality.String(), effectiveReq.UserId(), effectiveReq.AgentName(),
						).Inc()
					}
					if reason := classifyAttemptReason(failsafeExecutor.consensusPolicyEnabled, retries, hedges); reason != "" {
						telemetry.IncNetworkAttemptReason(n.projectId, n.Label(), method, reason)
					}
					r, err := tryForward(u, effectiveReq, loopCtx, &ulg, hedges, attempts, retries)
					if e := n.normalizeResponse(loopCtx, effectiveReq, r); e != nil {
						ulg.Error().Err(e).Msgf("failed to normalize response")
						err = e
					}
					effectiveReq.MarkUpstreamCompleted(loopCtx, u, r, err)

					// Hedge cancelled → this execution lost the race, bail out
					if hedges > 0 && common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled) {
						n.recordHedgeDiscard(loopCtx, loopSpan, &ulg, u, effectiveReq, method, err, attempts, hedges)
						loopSpan.End()
						return nil, common.NewErrUpstreamHedgeCancelled(u.Id(), err)
					}

					if r != nil {
						r.SetUpstream(u)
						r.WithRequest(effectiveReq)
					}

					// Return immediately when the result is usable:
					//  - Non-empty success always qualifies.
					//  - Emptyish success qualifies when the method is in
					//    emptyResultAccept and consensus is not required,
					//    because failsafe would accept the empty result anyway
					//    so trying more upstreams just wastes time on slow ones.
					//  - Otherwise emptyish results continue to the next upstream.
					if err == nil && r != nil && !r.IsObjectNull() {
						emptyish := r.IsResultEmptyish()
						acceptEmpty := !emptyish ||
							(!failsafeExecutor.consensusPolicyEnabled &&
								slices.Contains(failsafeExecutor.emptyResultAccept, method))
						if acceptEmpty {
							r.SetAttempts(exec.Attempts())
							r.SetRetries(exec.Retries())
							r.SetHedges(exec.Hedges())
							loopSpan.SetStatus(codes.Ok, "")
							if emptyish {
								loopSpan.SetAttributes(attribute.Bool("emptyish_accepted", true))
							}
							loopSpan.End()
							return r, nil
						}
					}

					// Deterministic errors: client faults and execution reverts are the
					// same on every upstream — no point trying others.
					if common.IsClientError(err) || common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
						common.SetTraceSpanError(loopSpan, err)
						loopSpan.End()
						return nil, err
					}

					// Track best result and continue to next upstream.
					if err != nil {
						lastErr = err
						common.SetTraceSpanError(loopSpan, err)
					} else if r != nil {
						bestResp = r
						loopSpan.SetStatus(codes.Ok, "")
					}
					loopSpan.End()
				}

				// Check context after the loop — handles single-upstream case where
				// the loop cap is reached before a new iteration can check ctx.
				if ctxErr := execSpanCtx.Err(); ctxErr != nil {
					cause := context.Cause(execSpanCtx)
					if cause == nil {
						cause = ctxErr
					}
					return nil, cause
				}

				// All upstreams tried. Return the best result for failsafe to evaluate
				// delays and retries. Prefer a valid response over an error so the
				// delay function can detect empty results and apply emptyResultDelay.
				if bestResp != nil {
					bestResp.SetAttempts(exec.Attempts())
					bestResp.SetRetries(exec.Retries())
					bestResp.SetHedges(exec.Hedges())
					return bestResp, nil
				}

				// For consensus, return the raw upstream error so the consensus
				// policy receives the actual error type (e.g. server error, missing
				// data) rather than a wrapped ErrUpstreamsExhausted. The retry
				// policy around consensus can then evaluate the raw error directly.
				if failsafeExecutor.consensusPolicyEnabled && lastErr != nil {
					return nil, lastErr
				}

				// Wrap all errors as ErrUpstreamsExhausted. The delay function
				// uses HasErrorCode which traverses child errors, so it can still
				// detect blockUnavailable / missingData inside the wrapper.
				exhaustedErr := common.NewErrUpstreamsExhausted(
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
				common.SetTraceSpanError(execSpan, exhaustedErr)
				return nil, exhaustedErr
			})
	}

	type forwardExecOutcome struct {
		resp *common.NormalizedResponse
		err  error
	}

	execDone := make(chan forwardExecOutcome, 1)
	go func() {
		r, e := executeForward()
		execDone <- forwardExecOutcome{resp: r, err: e}
	}()

	var execErr error
	select {
	case out := <-execDone:
		resp = out.resp
		execErr = out.err
	case <-ctx.Done():
		// Forward is still running in background; drain its outcome to avoid
		// leaking a completed response when cancellation wins this select race.
		go func() {
			out := <-execDone
			if out.resp != nil {
				out.resp.Release()
			}
		}()
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		if mlx != nil {
			mlx.Close(ctx, nil, cause)
		}
		return nil, cause
	}

	req.RLockWithTrace(ctx)
	defer req.RUnlock()

	if execErr != nil {
		translatedErr := upstream.TranslateFailsafeError(common.ScopeNetwork, "", method, execErr, &startTime)
		// Don't override consensus results with last valid response from individual upstreams
		// For example if 1 upstream gives empty response another 3 give "reverted" error,
		// we should still return reverted error, even though there was an empty response before.
		if failsafeExecutor.consensusPolicyEnabled {
			n.storeDeterministicNegativeCache(ctx, req, method, translatedErr)
			if mlx != nil {
				mlx.Close(ctx, nil, translatedErr)
			}
			// LVR is a borrowed pointer — consensus executor owns releasing all participant
			// responses (winners and losers). Just drop our reference to avoid dangling pointer.
			req.ClearLastValidResponse()
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
			n.storeDeterministicNegativeCache(ctx, req, method, translatedErr)
			if mlx != nil {
				mlx.Close(ctx, nil, translatedErr)
			}
			return nil, translatedErr
		}
	}

	if resp != nil {
		if n.cacheDal != nil {
			sem := n.getCacheWriteSem()
			select {
			case sem <- struct{}{}:
				n.observeCacheWriteQueueDepth(sem)
				// Force-materialize jrr so the goroutine reads only via atomic pointer (no locks needed).
				// TODO For other architectures we might need a different approach
				_, _ = resp.JsonRpcResponse(ctx)
				resp.AddRef()

				go (func(resp *common.NormalizedResponse, forwardSpan trace.Span) {
					defer func() {
						<-sem
						n.observeCacheWriteQueueDepth(sem)
					}()
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
					defer resp.DoneRef()

					timeoutCtx, timeoutCtxCancel := context.WithTimeoutCause(n.appCtx, 10*time.Second, errors.New("cache driver timeout during set"))
					defer timeoutCtxCancel()
					tracedCtx := trace.ContextWithSpanContext(timeoutCtx, forwardSpan.SpanContext())
					err := n.cacheDal.Set(tracedCtx, req, resp)
					if err != nil {
						lg.Warn().Err(err).Msgf("could not store response in cache")
					}
				})(resp, forwardSpan)
			default:
				n.observeCacheWriteQueueDepth(sem)
				telemetry.MetricNetworkCacheWriteDroppedTotal.WithLabelValues(
					n.projectId,
					n.Label(),
					method,
				).Inc()
				lg.Warn().Msg("skipping cache-set due to backpressure")
			}
		}

		// Use the counters embedded earlier in the response
		forwardSpan.SetAttributes(
			attribute.Int("execution.attempts", int(resp.Attempts())),
			attribute.Int("execution.retries", int(resp.Retries())),
			attribute.Int("execution.hedges", int(resp.Hedges())),
		)

		hedgeWon := resp.Hedges() > 0
		hedgeObserved := hedgeWon || requestObservedHedgeCancellation(req)
		if hedgeObserved {
			finality := req.Finality(ctx)
			upstreamID := "unknown"
			if ups := resp.Upstream(); ups != nil {
				upstreamID = ups.Id()
			}

			labels := []string{
				n.projectId,
				n.Label(),
				upstreamID,
				method,
				finality.String(),
				req.UserId(),
				req.AgentName(),
			}
			if hedgeWon {
				telemetry.CounterHandle(telemetry.MetricNetworkHedgeWonTotal, labels...).Inc()
			} else {
				telemetry.CounterHandle(telemetry.MetricNetworkHedgeLostTotal, labels...).Inc()
			}
		}
	}

	isEmpty := resp == nil || resp.IsObjectNull(ctx) || resp.IsResultEmptyish(ctx)
	forwardSpan.SetAttributes(attribute.Bool("response.emptyish", isEmpty))
	if isEmpty {
		lg.Trace().Msgf("response is empty")
	}

	if execErr == nil && !isEmpty {
		n.enrichStatePoller(ctx, method, req, resp)

		// Extract block number from successful response for block availability bounds check below.
		var respBlockNumber int64
		if n.cfg.Architecture == common.ArchitectureEvm {
			if _, bn, err := evm.ExtractBlockReferenceFromResponse(ctx, resp); err == nil && bn > 0 {
				respBlockNumber = bn
			}
		}

		// If response is not empty, but at least one upstream responded empty we track in a metric.
		// Derived from ErrorsByUpstream entries with ErrEndpointMissingData code.
		req.ErrorsByUpstream.Range(func(key, value any) bool {
			upstreamErr, ok := value.(error)
			if !ok || !common.HasErrorCode(upstreamErr, common.ErrCodeEndpointMissingData) {
				return true
			}
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

			// If the response block number is known, check if it falls outside
			// this upstream's configured block availability range.
			// If so, the empty response was expected — skip misbehavior recording.
			if respBlockNumber > 0 {
				minBound, maxBound := upstream.EvmBlockAvailabilityBounds()
				if (minBound != math.MinInt64 && respBlockNumber < minBound) ||
					(maxBound != math.MaxInt64 && respBlockNumber > maxBound) {
					return true
				}
			}

			// Wrong-empty is a misbehavior (data disagreement with other upstreams),
			// not an error. The upstream responded correctly, it just lacked data
			// that others had. Only record misbehavior, not failure.
			if upstream != nil {
				if mt := upstream.MetricsTracker(); mt != nil {
					mt.RecordUpstreamMisbehavior(upstream, method)
				}
			}
			return true
		})
	}

	if resp != nil && !isEmpty && mlx != nil && atomic.LoadInt32(&mlx.followerCount) > 0 {
		n.storePostCompletionCoalescedResult(ctx, req, method, resp)
	}

	if mlx != nil {
		mlx.Close(ctx, resp, nil)
	}

	// LVR is a borrowed pointer — consensus executor owns releasing non-winner responses.
	// Just drop our reference so it doesn't outlive the response lifecycle.
	if failsafeExecutor.consensusPolicyEnabled {
		req.ClearLastValidResponse()
	}

	return resp, nil
}

func requestObservedHedgeCancellation(req *common.NormalizedRequest) bool {
	if req == nil {
		return false
	}
	observed := false
	req.ErrorsByUpstream.Range(func(_, value any) bool {
		err, ok := value.(error)
		if !ok {
			return true
		}
		if common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled) {
			observed = true
			return false
		}
		return true
	})
	return observed
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

func (n *Network) Cache() common.CacheDAL {
	return n.cacheDal
}

func (n *Network) AppCtx() context.Context {
	return n.appCtx
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

	// When the request alone doesn't carry a block number (e.g. tx-hash lookups
	// like eth_getTransactionReceipt, or block-hash lookups like eth_getBlockByHash),
	// try to extract it from the response body. This is critical for cache hits where
	// the response contains blockNumber but the request only has a hash.
	if blockNumber == 0 && resp != nil {
		if _, respBlockNumber, err := evm.ExtractBlockReferenceFromResponse(ctx, resp); err == nil && respBlockNumber > 0 {
			blockNumber = respBlockNumber
		}
	}

	// If we still have no block number and the request is a wildcard/hash-only ref
	// with no way to determine finality, return unknown early.
	if blockNumber == 0 && (blockRef == "" || blockRef == "*") {
		return finality // unknown
	}

	if blockRef != "" && blockRef != "*" && (blockRef[0] < '0' || blockRef[0] > '9') {
		finality = common.DataFinalityStateRealtime
		return finality
	}

	if blockNumber > 0 {
		// Try response's upstream first (direct upstream responses)
		if resp != nil {
			upstream := resp.Upstream()
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
		}

		// Try LastUpstream from the request (retries or subsequent attempts)
		if upstream := req.LastUpstream(); upstream != nil {
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

		// Fallback: use the network's lowest finalized block as a heuristic.
		// This is the primary path for cache hits where no upstream is available.
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

// resolveEnforceBlockAvailability resolves the effective enforcement flag for block availability
// using strict precedence: method-level > network-level > default method config > fallback (true).
func (n *Network) resolveEnforceBlockAvailability(method string) bool {
	// Highest precedence: method-level override from network config
	if n.cfg != nil && n.cfg.Methods != nil && n.cfg.Methods.Definitions != nil {
		if mc, ok := n.cfg.Methods.Definitions[method]; ok && mc != nil && mc.EnforceBlockAvailability != nil {
			return *mc.EnforceBlockAvailability
		}
	}
	// Next: network-level default
	if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.EnforceBlockAvailability != nil {
		return *n.cfg.Evm.EnforceBlockAvailability
	}
	// Lowest: common default method config
	if common.DefaultWithBlockCacheMethods != nil {
		if dmc, ok := common.DefaultWithBlockCacheMethods[method]; ok && dmc != nil && dmc.EnforceBlockAvailability != nil {
			return *dmc.EnforceBlockAvailability
		}
	}
	// Fallback default: enabled
	return true
}

// handleBlockSkip records telemetry when a block availability check causes an upstream to be skipped,
// and triggers an async state poller refresh for retryable cases so subsequent retries see fresh block numbers.
func (n *Network) handleBlockSkip(
	ctx context.Context,
	span trace.Span,
	ulg *zerolog.Logger,
	u common.Upstream,
	req *common.NormalizedRequest,
	method string,
	skipErr error,
	isRetryable bool,
) {
	ulg.Debug().Err(skipErr).Bool("retryable", isRetryable).Msg("skipping upstream due to block availability gating")
	span.SetAttributes(
		attribute.Bool("skipped", true),
		attribute.String("skip_reason", skipErr.Error()),
		attribute.Bool("skip_retryable", isRetryable),
	)
	finality := req.Finality(ctx)
	telemetry.MetricUpstreamErrorTotal.WithLabelValues(
		n.projectId, u.VendorName(), n.Label(), u.Id(), method,
		common.ErrorFingerprint(skipErr), string(common.SeverityInfo),
		req.CompositeType(), finality.String(),
		req.UserId(), req.AgentName(),
	).Inc()
	errToStore := skipErr
	if !isRetryable {
		errToStore = common.NewErrUpstreamRequestSkipped(skipErr, u.Id())
	}
	req.MarkUpstreamCompleted(ctx, u, nil, errToStore)

	// When the block is slightly ahead (retryable), trigger an async poll so the
	// state poller fetches the latest block number before the next retry fires.
	// Without this the retry loop sleeps for blockUnavailableDelay but the cached
	// latestBlock stays stale until the background ticker fires (often 10s+).
	// PollLatestBlockNumber respects its own debounce interval so concurrent
	// triggers from multiple upstreams/requests are coalesced safely.
	if isRetryable {
		if eu, ok := u.(common.EvmUpstream); ok {
			if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
				go func() {
					pollCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_, _ = sp.PollLatestBlockNumber(pollCtx)
				}()
			}
		}
	}
}

// recordHedgeDiscard handles telemetry when a hedged request is discarded because another hedge won.
func (n *Network) recordHedgeDiscard(
	ctx context.Context,
	span trace.Span,
	ulg *zerolog.Logger,
	u common.Upstream,
	req *common.NormalizedRequest,
	method string,
	err error,
	attempts int,
	hedges int,
) {
	ulg.Debug().Err(err).Msgf("discarding hedged request to upstream")
	finality := req.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkHedgeDiscardsTotal,
		n.projectId, n.Label(), u.Id(), method, fmt.Sprintf("%d", attempts),
		fmt.Sprintf("%d", hedges), finality.String(),
		req.UserId(), req.AgentName(),
	).Inc()
	telemetry.CounterHandle(telemetry.MetricNetworkHedgeDiscardTotal,
		n.projectId, n.Label(), u.Id(), method, finality.String(),
		req.UserId(), req.AgentName(),
	).Inc()
	common.SetTraceSpanError(span, common.NewErrUpstreamHedgeCancelled(u.Id(), err))
}

// checkUpstreamBlockAvailability performs per-upstream gating for the request based on block availability.
// It is invoked just before forwarding to an upstream to avoid copying/filtering the list.
// Returns (nil, false) if upstream has the block available.
// Returns (error, isRetryable) if block is not available:
//   - isRetryable=true: block is just slightly ahead (within MaxRetryableBlockDistance), upstream may catch up
//   - isRetryable=false: block is too far ahead or below lower bound, not worth retrying this upstream
//
// FAIL-OPEN BEHAVIOR: If we cannot determine block availability (e.g., state poller issues),
// we allow the request to proceed rather than blocking traffic.
func (n *Network) checkUpstreamBlockAvailability(ctx context.Context, u common.Upstream, req *common.NormalizedRequest, method string) (error, bool) {
	if n.cfg.Architecture != common.ArchitectureEvm {
		return nil, false
	}
	// Resolve enforcement using strict precedence
	enforce := n.resolveEnforceBlockAvailability(method)
	if !enforce {
		return nil, false
	}
	// Use cached block number from normalization to avoid re-extracting from mutated params
	var bn int64
	if v := req.EvmBlockNumber(); v != nil {
		if n64, ok := v.(int64); ok {
			bn = n64
		}
	}
	if bn <= 0 {
		// If still unknown, skip gating (fail-open)
		return nil, false
	}
	eu, ok := u.(common.EvmUpstream)
	if !ok {
		return nil, false
	}
	available, err := eu.EvmAssertBlockAvailability(ctx, method, common.AvailbilityConfidenceBlockHead, true, bn)
	if err != nil {
		// FAIL-OPEN: Error during availability check - allow the request to proceed
		// This prevents blocking traffic due to state poller or detection issues
		n.logger.Debug().
			Err(err).
			Str("upstreamId", u.Id()).
			Int64("blockNumber", bn).
			Str("method", method).
			Msg("block availability check failed; failing open to allow request")
		return nil, false
	}
	if !available {
		// Get poller state for detailed error
		var latestBlock, finalizedBlock int64
		if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
			latestBlock = sp.LatestBlock()
			finalizedBlock = sp.FinalizedBlock()
		}

		blockErr := common.NewErrUpstreamBlockUnavailable(u.Id(), bn, latestBlock, finalizedBlock)

		// Determine if this is retryable based on distance
		// Upper bound issue (block ahead of latest): retryable if within max distance
		// Lower bound issue (block too old): not retryable
		if bn > latestBlock && latestBlock > 0 {
			distance := bn - latestBlock
			maxDistance := int64(128) // default
			if n.cfg.Evm != nil && n.cfg.Evm.MaxRetryableBlockDistance != nil {
				maxDistance = *n.cfg.Evm.MaxRetryableBlockDistance
			}
			isRetryable := distance <= maxDistance
			return blockErr, isRetryable
		}

		// Lower bound issue or unknown state - not retryable
		return blockErr, false
	}
	return nil, false
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
	method, _ := req.Method()
	if !n.isMultiplexingEnabledForMethod(method) {
		return nil, nil, nil
	}

	mlxHash, err := req.CacheHash()
	lg.Trace().Str("hash", mlxHash).Object("request", req).Msgf("checking if multiplexing is possible")
	if err != nil || mlxHash == "" {
		lg.Debug().Str("hash", mlxHash).Err(err).Object("request", req).Msgf("could not get multiplexing hash for request")
		return nil, nil, nil
	}

	if resp, hit := n.loadPostCompletionCoalescedResult(ctx, req, method, mlxHash); hit {
		lg.Trace().Str("hash", mlxHash).Object("response", resp).Msgf("served from post-completion coalescing window")
		return nil, resp, nil
	}

	// Try to atomically register as the leader or become a follower.
	// Loop handles the edge case where we find a closed multiplexer during cleanup.
	for {
		mlx := NewMultiplexer(mlxHash)
		vinf, loaded := n.inFlightRequests.LoadOrStore(mlxHash, mlx)

		if !loaded {
			// Our multiplexer was stored, we're the leader
			return mlx, nil, nil
		}

		// Another request is already in flight - try to be a follower
		inf := vinf.(*Multiplexer)

		// Try to register as follower under lock to prevent race with cleanup's Wait()
		inf.mu.Lock()
		if inf.closed {
			// Leader already finished and cleanup started, can't be a follower.
			// Retry LoadOrStore - the old entry should be deleted soon, allowing
			// us to properly become the new leader (with our mlx stored in the map).
			inf.mu.Unlock()
			continue
		}

		inf.copyWg.Add(1)
		atomic.AddInt32(&inf.followerCount, 1)
		inf.mu.Unlock()

		finality := req.Finality(ctx)
		telemetry.CounterHandle(telemetry.MetricNetworkMultiplexedRequests,
			n.projectId, n.Label(), method, finality.String(), req.UserId(), req.AgentName(),
		).Inc()

		lg.Debug().Str("hash", mlxHash).Msgf("found identical request initiating multiplexer")

		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(attribute.String("multiplexer.hash", mlxHash))
		}

		resp, err := n.waitForMultiplexResult(ctx, inf, req, startTime)

		// Done() is called in waitForMultiplexResult via defer
		lg.Trace().Str("hash", mlxHash).Object("response", resp).Err(err).Msgf("multiplexed request result")

		if err != nil {
			return nil, nil, err
		}
		if resp != nil {
			return nil, resp, nil
		}

		// Shouldn't reach here normally, but if we do, retry
		lg.Warn().Str("hash", mlxHash).Msg("multiplexer follower got nil response and no error, retrying")
	}
}

func (n *Network) waitForMultiplexResult(ctx context.Context, mlx *Multiplexer, req *common.NormalizedRequest, startTime time.Time) (*common.NormalizedResponse, error) {
	ctx, span := common.StartSpan(ctx, "Network.WaitForMultiplexResult")
	defer span.End()
	span.SetAttributes(attribute.String("multiplexer.hash", mlx.hash))

	// Caller already registered with copyWg.Add(1), ensure we signal completion
	defer mlx.copyWg.Done()

	// Wait for leader to complete (or check if already done)
	select {
	case <-mlx.done:
		// Leader finished - copy the response
		// Lock protects against concurrent reads, cleanup waits for copyWg so response is valid
		mlx.mu.RLock()
		out, err := common.CopyResponseForRequest(ctx, mlx.resp, req)
		resultErr := mlx.err
		mlx.mu.RUnlock()

		if err != nil {
			return nil, err
		}
		return out, resultErr
	case <-ctx.Done():
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, common.NewErrNetworkRequestTimeout(time.Since(startTime), err)
		}
		return nil, err
	}
}

func (n *Network) loadPostCompletionCoalescedResult(ctx context.Context, req *common.NormalizedRequest, method, hash string) (*common.NormalizedResponse, bool) {
	if n == nil || n.postCompletionResults == nil || hash == "" {
		return nil, false
	}
	if !n.shouldUsePostCompletionCoalescing(ctx, req, method) {
		return nil, false
	}

	raw, ok := n.postCompletionResults.Load(hash)
	if !ok {
		return nil, false
	}
	entry, ok := raw.(*postCompletionResultEntry)
	if !ok || entry == nil || entry.resp == nil {
		n.postCompletionResults.Delete(hash)
		if entry != nil {
			entry.release()
		}
		return nil, false
	}
	if time.Now().UnixNano() > atomic.LoadInt64(&entry.expiresAt) {
		n.postCompletionResults.Delete(hash)
		entry.release()
		return nil, false
	}

	entry.resp.AddRef()
	defer entry.resp.DoneRef()

	out, err := common.CopyResponseForRequest(ctx, entry.resp, req)
	if err != nil {
		n.postCompletionResults.Delete(hash)
		entry.release()
		return nil, false
	}
	if out == nil {
		return nil, false
	}

	finality := req.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkMultiplexedRequests,
		n.projectId, n.Label(), method, finality.String(), req.UserId(), req.AgentName(),
	).Inc()
	telemetry.CounterHandle(telemetry.MetricNetworkBurstCoalescedRequests,
		n.projectId, n.Label(), method, finality.String(), req.UserId(), req.AgentName(),
	).Inc()

	return out, true
}

func (n *Network) storePostCompletionCoalescedResult(ctx context.Context, req *common.NormalizedRequest, method string, resp *common.NormalizedResponse) {
	if n == nil || n.postCompletionResults == nil || resp == nil {
		return
	}
	if !n.shouldUsePostCompletionCoalescing(ctx, req, method) {
		return
	}
	hash, err := req.CacheHash(ctx)
	if err != nil || hash == "" {
		return
	}

	stagedResp, copyErr := common.CopyResponseForRequest(ctx, resp, req)
	if copyErr != nil || stagedResp == nil {
		return
	}

	entry := &postCompletionResultEntry{
		resp:      stagedResp,
		expiresAt: time.Now().Add(networkPostCompletionCoalescingWindow).UnixNano(),
	}

	previous, loaded := n.postCompletionResults.Swap(hash, entry)
	if loaded {
		if oldEntry, ok := previous.(*postCompletionResultEntry); ok {
			oldEntry.release()
		}
	}

	time.AfterFunc(networkPostCompletionCoalescingWindow, func() {
		raw, ok := n.postCompletionResults.Load(hash)
		if ok && raw == entry {
			n.postCompletionResults.Delete(hash)
			entry.release()
		}
	})
}

func (n *Network) shouldUsePostCompletionCoalescing(ctx context.Context, req *common.NormalizedRequest, method string) bool {
	if req == nil || method == "" {
		return false
	}
	if req.SkipCacheRead() {
		return false
	}
	if dr := req.Directives(); dr != nil && dr.UseUpstream != "" {
		return false
	}
	if !isReadMethodEligibleForNegativeCache(method) {
		return false
	}
	if isNonceSensitiveReadMethod(method) {
		return false
	}
	if hasPendingBlockReference(ctx, req) {
		return false
	}
	return true
}

func (n *Network) cleanupMultiplexer(mlx *Multiplexer) {
	// Mark as closed under lock to prevent new followers from registering
	mlx.mu.Lock()
	mlx.closed = true
	mlx.mu.Unlock()

	// Remove from map so new requests create their own multiplexer
	n.inFlightRequests.Delete(mlx.hash)

	// Wait for all followers to finish copying before releasing.
	// Wait() is safe because closed=true prevents any new Add() calls.
	// This blocks the leader briefly, but the leader has already returned
	// its response to the caller, so this only delays cleanup.
	mlx.copyWg.Wait()

	// After Wait(), no followers are accessing the response.
	mlx.mu.Lock()
	// Do NOT call Release() here: the leader returns mlx.resp to the caller, and the
	// caller will release it after writing the response. Releasing here would
	// clear the JsonRpcResponse and break the leader response path.
	mlx.resp = nil
	mlx.err = nil
	mlx.mu.Unlock()
}

func (n *Network) loadDeterministicNegativeCache(ctx context.Context, req *common.NormalizedRequest, method string) (error, bool) {
	if n == nil || n.negativeResultCache == nil {
		return nil, false
	}
	if !n.shouldUseDeterministicNegativeCache(ctx, req, method) {
		return nil, false
	}

	key, ok := n.deterministicNegativeCacheKey(ctx, req, method)
	if !ok {
		return nil, false
	}

	raw, ok := n.negativeResultCache.Load(key)
	if !ok {
		return nil, false
	}
	entry, ok := raw.(*deterministicNegativeCacheEntry)
	if !ok || entry == nil || entry.err == nil {
		n.negativeResultCache.Delete(key)
		return nil, false
	}
	if time.Now().UnixNano() > atomic.LoadInt64(&entry.expiresAt) {
		n.negativeResultCache.Delete(key)
		return nil, false
	}

	finality := req.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkDeterministicNegativeCacheHitTotal,
		n.projectId, n.Label(), method, finality.String(), req.UserId(), req.AgentName(),
	).Inc()

	return entry.err, true
}

func (n *Network) storeDeterministicNegativeCache(ctx context.Context, req *common.NormalizedRequest, method string, err error) {
	if n == nil || n.negativeResultCache == nil || err == nil {
		return
	}
	if !n.shouldUseDeterministicNegativeCache(ctx, req, method) {
		return
	}
	if !shouldStoreDeterministicNegativeError(err) {
		return
	}

	key, ok := n.deterministicNegativeCacheKey(ctx, req, method)
	if !ok {
		return
	}

	now := time.Now()
	entry := &deterministicNegativeCacheEntry{
		err:       err,
		expiresAt: now.Add(networkDeterministicNegativeCacheTTL).UnixNano(),
	}
	n.negativeResultCache.Store(key, entry)

	finality := req.Finality(ctx)
	telemetry.CounterHandle(telemetry.MetricNetworkDeterministicNegativeCacheSetTotal,
		n.projectId, n.Label(), method, finality.String(), req.UserId(), req.AgentName(),
	).Inc()

	time.AfterFunc(networkDeterministicNegativeCacheTTL, func() {
		raw, ok := n.negativeResultCache.Load(key)
		if ok && raw == entry {
			n.negativeResultCache.Delete(key)
		}
	})
}

func (n *Network) deterministicNegativeCacheKey(ctx context.Context, req *common.NormalizedRequest, method string) (string, bool) {
	hash, err := req.CacheHash(ctx)
	if err != nil || hash == "" {
		return "", false
	}
	finality := req.Finality(ctx)
	return fmt.Sprintf("%s|%s|%s", method, finality.String(), hash), true
}

func (n *Network) shouldUseDeterministicNegativeCache(ctx context.Context, req *common.NormalizedRequest, method string) bool {
	if req == nil || method == "" {
		return false
	}
	if req.SkipCacheRead() {
		return false
	}
	if dr := req.Directives(); dr != nil && dr.UseUpstream != "" {
		return false
	}
	if !isReadMethodEligibleForNegativeCache(method) {
		return false
	}
	if n != nil && n.cfg != nil && n.cfg.Methods != nil && n.cfg.Methods.Definitions != nil {
		if mc, ok := n.cfg.Methods.Definitions[method]; ok && mc != nil && mc.Stateful {
			return false
		}
	}
	if isNonceSensitiveReadMethod(method) {
		return false
	}
	if hasPendingBlockReference(ctx, req) {
		return false
	}

	finality := req.Finality(ctx)
	return finality == common.DataFinalityStateFinalized
}

func hasPendingBlockReference(ctx context.Context, req *common.NormalizedRequest) bool {
	if req == nil {
		return false
	}
	if br := req.EvmBlockRef(); br != nil {
		if s, ok := br.(string); ok && strings.EqualFold(s, "pending") {
			return true
		}
	}
	ref, _, err := evm.ExtractBlockReferenceFromRequest(ctx, req)
	return err == nil && strings.EqualFold(ref, "pending")
}

func shouldStoreDeterministicNegativeError(err error) bool {
	if err == nil {
		return false
	}
	if common.IsRetryableTowardNetwork(err) {
		return false
	}
	return common.IsClientError(err) ||
		common.HasErrorCode(err, common.ErrCodeEndpointExecutionException, common.ErrCodeEndpointUnsupported)
}

func isReadMethodEligibleForNegativeCache(method string) bool {
	if method == "" {
		return false
	}
	if evm.IsNonRetryableWriteMethod(method) || method == "eth_sendRawTransaction" {
		return false
	}
	if strings.HasPrefix(method, "eth_send") ||
		strings.HasPrefix(method, "engine_") ||
		strings.HasPrefix(method, "personal_") ||
		strings.HasPrefix(method, "wallet_") {
		return false
	}
	return true
}

func (n *Network) isMultiplexingEnabledForMethod(method string) bool {
	if n == nil || n.cfg == nil {
		return true
	}
	if method != "" && n.cfg.Methods != nil && n.cfg.Methods.Definitions != nil {
		if mc, ok := n.cfg.Methods.Definitions[method]; ok && mc != nil && mc.Multiplex != nil {
			return *mc.Multiplex
		}
	}
	return n.cfg.MultiplexingEnabled()
}

func isNonceSensitiveReadMethod(method string) bool {
	return method == "eth_getTransactionCount" || strings.HasPrefix(method, "txpool_")
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
			// Prefer the original block reference preserved by normalization.
			// This stays "latest"/"finalized" even if params were interpolated to hex.
			var blkTag string
			if ref := req.EvmBlockRef(); ref != nil {
				if s, ok := ref.(string); ok && (s == "latest" || s == "finalized") {
					blkTag = s
				}
			}
			if lg := n.logger; lg != nil && lg.GetLevel() <= zerolog.TraceLevel {
				lg.Trace().
					Str("blkTagFromEvmBlockRef", blkTag).
					Interface("evmBlockRefRaw", req.EvmBlockRef()).
					Msg("enrichStatePoller: resolved block tag from request EvmBlockRef")
			}
			// Fallback to inspecting the request param only if we couldn't resolve from EvmBlockRef
			if blkTag == "" {
				jrq, err := req.JsonRpcRequest(ctx)
				if err != nil {
					return
				}
				jrq.RLock()
				if len(jrq.Params) > 0 {
					if s, ok := jrq.Params[0].(string); ok && (s == "latest" || s == "finalized") {
						blkTag = s
					}
				}
				jrq.RUnlock()
				if lg := n.logger; lg != nil {
					lg.Trace().
						Str("blkTagFromParams", blkTag).
						Msg("enrichStatePoller: resolved block tag from request params")
				}
			}
			if blkTag == "" {
				return
			}
			jrs, _ := resp.JsonRpcResponse(ctx)
			bnh, err := jrs.PeekStringByPath(ctx, "number")
			if err != nil {
				return
			}
			blockNumber, err := common.HexToInt64(bnh)
			if err != nil {
				return
			}
			if lg := n.logger; lg != nil {
				lg.Trace().
					Str("blkTag", blkTag).
					Int64("blockNumber", blockNumber).
					Str("method", method).
					Msg("enrichStatePoller: suggesting block number to state poller")
			}
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
