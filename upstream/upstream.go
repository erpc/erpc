package upstream

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// FailsafeExecutor wraps a failsafe executor with method and finality filters
type FailsafeExecutor struct {
	method     string
	finalities []common.DataFinalityState
	executor   failsafe.Executor[*common.NormalizedResponse]
	timeout    *time.Duration
}

type Upstream struct {
	ProjectId string
	Client    clients.ClientInterface

	appCtx context.Context
	logger *zerolog.Logger
	config *common.UpstreamConfig
	cfgMu  sync.RWMutex
	vendor common.Vendor

	networkId            string
	supportedMethods     sync.Map
	metricsTracker       *health.Tracker
	sharedStateRegistry  data.SharedStateRegistry
	failsafeExecutors    []*FailsafeExecutor
	rateLimitersRegistry *RateLimitersRegistry
	rateLimiterAutoTuner *RateLimitAutoTuner
	evmStatePoller       common.EvmStatePoller
}

func NewUpstream(
	appCtx context.Context,
	projectId string,
	cfg *common.UpstreamConfig,
	cr *clients.ClientRegistry,
	rlr *RateLimitersRegistry,
	vr *thirdparty.VendorsRegistry,
	logger *zerolog.Logger,
	mt *health.Tracker,
	ssr data.SharedStateRegistry,
) (*Upstream, error) {
	lg := logger.With().Str("upstreamId", cfg.Id).Logger()

	// Create failsafe executors from configs
	var failsafeExecutors []*FailsafeExecutor
	if len(cfg.Failsafe) > 0 {
		for _, fsCfg := range cfg.Failsafe {
			policiesMap, err := CreateFailSafePolicies(&lg, common.ScopeUpstream, cfg.Id, fsCfg)
			if err != nil {
				return nil, err
			}
			policiesArray := ToPolicyArray(policiesMap, "retry", "circuitBreaker", "hedge", "timeout")

			var timeoutDuration *time.Duration
			if fsCfg.Timeout != nil {
				timeoutDuration = fsCfg.Timeout.Duration.DurationPtr()
			}

			method := fsCfg.MatchMethod
			if method == "" {
				method = "*"
			}
			failsafeExecutors = append(failsafeExecutors, &FailsafeExecutor{
				method:     method,
				finalities: fsCfg.MatchFinality,
				executor:   failsafe.NewExecutor(policiesArray...),
				timeout:    timeoutDuration,
			})
		}
	}

	failsafeExecutors = append(failsafeExecutors, &FailsafeExecutor{
		method:     "*", // "*" means match any method
		finalities: nil, // nil means match any finality
		executor:   failsafe.NewExecutor[*common.NormalizedResponse](),
		timeout:    nil,
	})

	vn := vr.LookupByUpstream(cfg)

	pup := &Upstream{
		ProjectId: projectId,

		logger:               &lg,
		appCtx:               appCtx,
		config:               cfg,
		vendor:               vn,
		metricsTracker:       mt,
		sharedStateRegistry:  ssr,
		failsafeExecutors:    failsafeExecutors,
		rateLimitersRegistry: rlr,
		supportedMethods:     sync.Map{},
	}

	pup.initRateLimitAutoTuner()

	if vn != nil {
		cfgs, err := vn.GenerateConfigs(appCtx, &lg, cfg, nil)
		if err != nil {
			return nil, err
		}
		if len(cfgs) == 0 {
			return nil, fmt.Errorf("no upstreams generated by vendor: %s", vn.Name())
		}
		pup.config = cfgs[0]
	}

	if pup.config.VendorName == "" {
		if vn != nil {
			pup.config.VendorName = vn.Name()
		} else {
			pup.config.VendorName = pup.guessVendorName()
		}
	}

	lg = pup.logger.With().Str("vendorName", pup.VendorName()).Logger()
	pup.logger = &lg

	if client, err := cr.GetOrCreateClient(appCtx, pup); err != nil {
		return nil, err
	} else {
		pup.Client = client
	}

	lg.Debug().Msgf("prepared upstream")

	return pup, nil
}

func (u *Upstream) Bootstrap(ctx context.Context) error {
	err := u.detectFeatures(ctx)
	if err != nil {
		return err
	}

	if u.config.Type == common.UpstreamTypeEvm {
		u.evmStatePoller = evm.NewEvmStatePoller(u.ProjectId, u.appCtx, u.logger, u, u.metricsTracker, u.sharedStateRegistry)
	}

	if u.evmStatePoller != nil {
		err = u.evmStatePoller.Bootstrap(ctx)
		if err != nil {
			// The reason we're not returning error is to allow upstream to still be registered
			// even if background block polling fails initially.
			u.logger.Error().Err(err).Msg("failed on initial bootstrap of evm state poller (will retry in background)")
		}
	}

	return nil
}

func (u *Upstream) Id() string {
	if u == nil {
		return ""
	}
	return u.config.Id
}

func (u *Upstream) NetworkId() string {
	if u == nil {
		return ""
	}
	return u.networkId
}

func (u *Upstream) VendorName() string {
	if u == nil {
		return "nil"
	}
	if u.vendor != nil {
		return u.vendor.Name()
	}
	if u.config != nil {
		return u.config.VendorName
	}
	return "n/a"
}

func (u *Upstream) Config() *common.UpstreamConfig {
	if u == nil {
		return nil
	}
	return u.config
}

func (u *Upstream) MetricsTracker() *health.Tracker {
	if u == nil {
		return nil
	}
	return u.metricsTracker
}

func (u *Upstream) Logger() *zerolog.Logger {
	return u.logger
}

func (u *Upstream) Vendor() common.Vendor {
	if u == nil {
		return nil
	}
	return u.vendor
}

func (u *Upstream) SetNetworkConfig(cfg *common.NetworkConfig) {
	// TODO Can we eliminate this circular dependency of upstream state poller <> network? e.g. by requiring network ID everywhere?
	if cfg != nil && cfg.Evm != nil && u.evmStatePoller != nil {
		u.evmStatePoller.SetNetworkConfig(cfg.Evm)
	}
}

func (u *Upstream) getFailsafeExecutor(req *common.NormalizedRequest) *FailsafeExecutor {
	method, _ := req.Method()
	finality := req.Finality(context.Background())

	// First, try to find a specific match for both method and finality
	for _, fe := range u.failsafeExecutors {
		if fe.method != "*" && len(fe.finalities) > 0 {
			matched, _ := common.WildcardMatch(fe.method, method)
			if matched && slices.Contains(fe.finalities, finality) {
				return fe
			}
		}
	}

	// Then, try to find a match for method only (empty finalities means any finality)
	for _, fe := range u.failsafeExecutors {
		if fe.method != "*" && (len(fe.finalities) == 0) {
			matched, _ := common.WildcardMatch(fe.method, method)
			if matched {
				return fe
			}
		}
	}

	// Then, try to find a match for finality only
	for _, fe := range u.failsafeExecutors {
		if fe.method == "*" && len(fe.finalities) > 0 {
			if slices.Contains(fe.finalities, finality) {
				return fe
			}
		}
	}

	// Return the first generic executor if no specific one is found (method = "*", finalities = nil)
	for _, fe := range u.failsafeExecutors {
		if fe.method == "*" && (len(fe.finalities) == 0) {
			return fe
		}
	}

	return nil
}

func (u *Upstream) Forward(ctx context.Context, nrq *common.NormalizedRequest, byPassMethodExclusion bool) (*common.NormalizedResponse, error) {
	// TODO Should we move byPassMethodExclusion to directives? How do we prevent clients from setting it?
	startTime := time.Now()
	cfg := u.Config()

	method, err := nrq.Method()
	ctx, span := common.StartSpan(ctx, "Upstream.Forward",
		trace.WithAttributes(
			attribute.String("network.id", u.networkId),
			attribute.String("upstream.id", cfg.Id),
			attribute.String("request.method", method),
		),
	)
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", nrq.ID())),
		)
	}

	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, common.NewErrUpstreamRequest(
			err,
			cfg.Id,
			u.networkId,
			method,
			time.Since(startTime),
			0,
			0,
			0,
		)
	}
	if !byPassMethodExclusion {
		if reason, skip := u.shouldSkip(ctx, nrq); skip {
			span.SetAttributes(attribute.Bool("skipped", true))
			span.SetAttributes(attribute.String("skipped_reason", reason.Error()))
			return nil, common.NewErrUpstreamRequestSkipped(reason, cfg.Id)
		}
	}

	clientType := u.Client.GetType()

	//
	// Apply rate limits
	//
	var limitersBudget *RateLimiterBudget
	if cfg.RateLimitBudget != "" {
		var errLimiters error
		limitersBudget, errLimiters = u.rateLimitersRegistry.GetBudget(cfg.RateLimitBudget)
		if errLimiters != nil {
			common.SetTraceSpanError(span, errLimiters)
			return nil, errLimiters
		}
	}

	lg := u.logger.With().Str("method", method).Str("networkId", u.networkId).Interface("id", nrq.ID()).Logger()

	if limitersBudget != nil {
		lg.Trace().Str("budget", cfg.RateLimitBudget).Msgf("checking upstream-level rate limiters budget")
		rules, err := limitersBudget.GetRulesByMethod(method)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return nil, err
		}
		if len(rules) > 0 {
			for _, rule := range rules {
				if !rule.Limiter.TryAcquirePermit() {
					lg.Debug().Str("budget", cfg.RateLimitBudget).Msgf("upstream-level rate limit '%v' exceeded", rule.Config)
					u.metricsTracker.RecordUpstreamSelfRateLimited(
						u,
						method,
					)
					err = common.NewErrUpstreamRateLimitRuleExceeded(
						cfg.Id,
						cfg.RateLimitBudget,
						fmt.Sprintf("%+v", rule.Config),
					)
					common.SetTraceSpanError(span, err)
					return nil, err
				} else {
					lg.Trace().Str("budget", cfg.RateLimitBudget).Object("rule", rule.Config).Msgf("upstream-level rate limit passed")
				}
			}
		}
	}

	//
	// Prepare and normalize the request object
	//
	nrq.SetLastUpstream(u)

	//
	// Send the request based on client type
	//
	switch clientType {
	case clients.ClientTypeHttpJsonRpc, clients.ClientTypeGrpcBds:
		tryForward := func(
			ctx context.Context,
			exec failsafe.Execution[*common.NormalizedResponse],
		) (*common.NormalizedResponse, error) {
			u.metricsTracker.RecordUpstreamRequest(
				u,
				method,
			)
			finality := nrq.Finality(ctx)
			telemetry.MetricUpstreamRequestTotal.WithLabelValues(u.ProjectId, u.VendorName(), u.networkId, cfg.Id, method, strconv.Itoa(exec.Attempts()), nrq.CompositeType(), finality.String()).Inc()
			timer := u.metricsTracker.RecordUpstreamDurationStart(u, method, nrq.CompositeType(), finality)

			nrs, errCall := u.Client.SendRequest(ctx, nrq)
			isSuccess := false
			if nrs != nil {
				nrs.SetUpstream(u)
				jrr, _ := nrs.JsonRpcResponse()
				if jrr != nil && jrr.Error == nil {
					nrq.SetLastValidResponse(ctx, nrs)
					isSuccess = true
				} else {
					isSuccess = false
				}
				lg.Debug().Err(errCall).Object("response", nrs).Msgf("upstream request ended with non-nil response")
			} else {
				if errCall != nil {
					if lg.GetLevel() == zerolog.TraceLevel && errors.Is(errCall, context.Canceled) {
						lg.Trace().Err(errCall).Msgf("upstream request ended due to context cancellation")
					} else {
						lg.Debug().Err(errCall).Msgf("upstream request ended with error")
					}
				} else {
					lg.Warn().Msgf("upstream request ended with nil response and nil error")
				}
			}
			if errCall != nil {
				isSuccess = false
				if common.HasErrorCode(errCall, common.ErrCodeUpstreamRequestSkipped) {
					telemetry.MetricUpstreamSkippedTotal.WithLabelValues(u.ProjectId, u.VendorName(), u.networkId, cfg.Id, method, finality.String()).Inc()
				} else if common.HasErrorCode(errCall, common.ErrCodeEndpointMissingData) {
					telemetry.MetricUpstreamMissingDataErrorTotal.WithLabelValues(u.ProjectId, u.VendorName(), u.networkId, cfg.Id, method, finality.String()).Inc()
				} else {
					if common.HasErrorCode(errCall, common.ErrCodeEndpointCapacityExceeded) {
						u.recordRemoteRateLimit(method)
					}
					severity := common.ClassifySeverity(errCall)
					// if severity == common.SeverityCritical {
					// We only consider a subset of errors in metrics tracker (which is used for score calculation)
					// so that we only penalize upstreams for internal issues (not rate limits, or client-side, or method support issues, etc.)
					u.metricsTracker.RecordUpstreamFailure(
						u,
						method,
						errCall,
					)
					// }
					telemetry.MetricUpstreamErrorTotal.WithLabelValues(u.ProjectId, u.VendorName(), u.networkId, cfg.Id, method, common.ErrorFingerprint(errCall), string(severity), nrq.CompositeType(), finality.String()).Inc()
				}

				timer.ObserveDuration(false)
				if exec != nil {
					return nil, common.NewErrUpstreamRequest(
						errCall,
						cfg.Id,
						u.networkId,
						method,
						time.Since(startTime),
						exec.Attempts(),
						exec.Retries(),
						exec.Hedges(),
					)
				} else {
					return nil, common.NewErrUpstreamRequest(
						errCall,
						cfg.Id,
						u.networkId,
						method,
						time.Since(startTime),
						1,
						0,
						0,
					)
				}
			} else {
				if nrs.IsResultEmptyish() {
					telemetry.MetricUpstreamEmptyResponseTotal.WithLabelValues(u.ProjectId, u.VendorName(), u.networkId, cfg.Id, method, finality.String()).Inc()
				}
			}

			timer.ObserveDuration(isSuccess)
			if isSuccess {
				u.recordRequestSuccess(method)
			}

			return nrs, nil
		}

		failsafeExecutor := u.getFailsafeExecutor(nrq)
		if failsafeExecutor == nil {
			return nil, fmt.Errorf("no failsafe executor found for request")
		}

		resp, execErr := failsafeExecutor.executor.
			WithContext(ctx).
			GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
				ectx, execSpan := common.StartSpan(exec.Context(), "Upstream.forwardAttempt",
					trace.WithAttributes(
						attribute.String("network.id", u.networkId),
						attribute.String("upstream.id", cfg.Id),
						attribute.Int("execution.attempt", exec.Attempts()),
						attribute.Int("execution.retry", exec.Retries()),
						attribute.Int("execution.hedge", exec.Hedges()),
					),
				)
				defer execSpan.End()

				if common.IsTracingDetailed {
					execSpan.SetAttributes(
						attribute.String("request.id", fmt.Sprintf("%v", nrq.ID())),
					)
				}

				if ctxErr := ectx.Err(); ctxErr != nil {
					cause := context.Cause(ectx)
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
					ectx, cancelFn = context.WithTimeout(
						ectx,
						// TODO Carrying the timeout helps setting correct timeout on actual http request to upstream (during batch mode).
						//      Is there a way to do this cleanly? e.g. if failsafe lib works via context rather than Ticker?
						//      5ms is a workaround to ensure context carries the timeout deadline (used when calling upstreams),
						//      but allow the failsafe execution to fail with timeout first for proper error handling.
						*failsafeExecutor.timeout+5*time.Millisecond,
					)
					defer cancelFn()
				}

				nr, err := tryForward(ectx, exec)
				if err != nil {
					common.SetTraceSpanError(execSpan, err)
					return nil, err
				}
				return nr, nil
			})

		if _, ok := execErr.(common.StandardError); !ok {
			if ctxErr := ctx.Err(); ctxErr != nil {
				cause := context.Cause(ctx)
				if cause != nil {
					execErr = cause
				} else {
					execErr = ctxErr
				}
			}
		}

		if execErr != nil {
			common.SetTraceSpanError(span, execErr)
			return nil, TranslateFailsafeError(common.ScopeUpstream, u.config.Id, method, execErr, &startTime)
		}

		return resp, nil
	default:
		err := common.NewErrUpstreamClientInitialization(
			fmt.Errorf("unsupported client type during forward: %s", clientType),
			cfg.Id,
		)
		common.SetTraceSpanError(span, err)
		return nil, err
	}
}

func (u *Upstream) Executor() failsafe.Executor[*common.NormalizedResponse] {
	// TODO extend this to per-network and/or per-method because of either upstream performance diff
	// or if user wants diff policies (retry/cb/integrity) per network/method.

	// Return the default executor (the one with "*" method and no finality filters)
	for _, fe := range u.failsafeExecutors {
		if fe.method == "*" && (len(fe.finalities) == 0) {
			return fe.executor
		}
	}

	// If no default executor found, return the first one
	if len(u.failsafeExecutors) > 0 {
		return u.failsafeExecutors[0].executor
	}

	// Return a no-op executor if none configured
	return failsafe.NewExecutor[*common.NormalizedResponse]()
}

// TODO move to evm package
func (u *Upstream) EvmGetChainId(ctx context.Context) (string, error) {
	pr := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":75412,"method":"eth_chainId","params":[]}`))

	resp, err := u.Forward(ctx, pr, true)
	if err != nil {
		return "", err
	}

	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrr.Error != nil {
		return "", jrr.Error
	}
	var chainId string
	err = common.SonicCfg.Unmarshal(jrr.Result, &chainId)
	if err != nil {
		return "", err
	}
	hex, err := common.NormalizeHex(chainId)
	if err != nil {
		return "", err
	}
	dec, err := common.HexToUint64(hex)
	if err != nil {
		return "", err
	}

	return strconv.FormatUint(dec, 10), nil
}

// TODO move to evm package
func (u *Upstream) EvmIsBlockFinalized(ctx context.Context, blockNumber int64, forceFreshIfStale bool) (bool, error) {
	if u.evmStatePoller == nil {
		return false, fmt.Errorf("evm state poller not initialized yet")
	}
	isFinalized, err := u.evmStatePoller.IsBlockFinalized(blockNumber)
	if err != nil {
		return false, err
	}
	if !isFinalized && forceFreshIfStale {
		newFinalizedBlock, err := u.evmStatePoller.PollFinalizedBlockNumber(ctx)
		if err != nil {
			return false, err
		}
		return newFinalizedBlock >= blockNumber, nil
	}
	return isFinalized, nil
}

// TODO move to evm package?
func (u *Upstream) EvmSyncingState() common.EvmSyncingState {
	if u.evmStatePoller == nil {
		return common.EvmSyncingStateUnknown
	}
	return u.evmStatePoller.SyncingState()
}

// TODO move to evm package?
func (u *Upstream) EvmLatestBlock() (int64, error) {
	if u.evmStatePoller == nil {
		return 0, fmt.Errorf("evm state poller not initialized yet")
	}
	return u.evmStatePoller.LatestBlock(), nil
}

// TODO move to evm package?
func (u *Upstream) EvmFinalizedBlock() (int64, error) {
	if u.evmStatePoller == nil {
		return 0, fmt.Errorf("evm state poller not initialized yet")
	}
	return u.evmStatePoller.FinalizedBlock(), nil
}

// TODO move to evm package?
func (u *Upstream) EvmStatePoller() common.EvmStatePoller {
	return u.evmStatePoller
}

// TODO move to evm package?
// EvmAssertBlockAvailability checks if the upstream is supposed to have the data for a certain block number.
// For full nodes it will check the first available block number, and for archive nodes it will check if the block is less than the latest block number.
// If the requested block is beyond the current latest block, it will force-poll the latest block number once.
// This method also increments appropriate metrics when the upstream cannot handle the block.
func (u *Upstream) EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence common.AvailbilityConfidence, forceFreshIfStale bool, blockNumber int64) (bool, error) {
	if u == nil || u.config == nil {
		return false, fmt.Errorf("upstream or config is nil")
	}

	// Get the state poller
	statePoller := u.EvmStatePoller()
	if statePoller == nil || statePoller.IsObjectNull() {
		return false, fmt.Errorf("upstream evm state poller is not available")
	}

	cfg := u.config
	if cfg.Type != common.UpstreamTypeEvm || cfg.Evm == nil {
		// If not an EVM upstream, we can't determine block handling capability
		return false, fmt.Errorf("upstream is not an EVM type")
	}

	if confidence == common.AvailbilityConfidenceFinalized {
		//
		// UPPER BOUND: Check if the block is finalized
		//
		isFinalized, err := u.EvmIsBlockFinalized(ctx, blockNumber, forceFreshIfStale)
		if err != nil {
			return false, fmt.Errorf("failed to check if block is finalized: %w", err)
		}
		if !isFinalized {
			telemetry.MetricUpstreamStaleUpperBound.WithLabelValues(
				u.ProjectId,
				u.VendorName(),
				u.NetworkId(),
				u.Id(),
				forMethod,
				confidence.String(),
			).Inc()
			return false, nil
		}

		//
		// LOWER BOUND: For full nodes, also check if the block is within the available range
		//
		if cfg.Evm.MaxAvailableRecentBlocks > 0 {
			// First check with current data
			available, err := u.assertUpstreamLowerBound(ctx, statePoller, blockNumber, cfg.Evm.MaxAvailableRecentBlocks, forMethod, confidence)
			if err != nil {
				return false, err
			}
			if !available {
				// If it can't handle, return immediately
				return false, nil
			}
		}

		// Block is finalized and within range (or archive node)
		return true, nil
	} else if confidence == common.AvailbilityConfidenceBlockHead {
		//
		// UPPER BOUND: Check if block is before the latest block
		//
		latestBlock := statePoller.LatestBlock()
		// If the requested block is beyond the current latest block, try force-polling once
		if blockNumber > latestBlock && forceFreshIfStale {
			var err error
			latestBlock, err = statePoller.PollLatestBlockNumber(ctx)
			if err != nil {
				return false, fmt.Errorf("failed to poll latest block number: %w", err)
			}
		}
		// Check if the requested block is still beyond the latest known block
		if blockNumber > latestBlock {
			// Upper bound issue - block is beyond latest
			telemetry.MetricUpstreamStaleUpperBound.WithLabelValues(
				u.ProjectId,
				u.VendorName(),
				u.NetworkId(),
				u.Id(),
				forMethod,
				confidence.String(),
			).Inc()
			return false, nil
		}

		//
		// LOWER BOUND: For full nodes, check if the block is within the available range
		//
		if cfg.Evm.MaxAvailableRecentBlocks > 0 {
			available, err := u.assertUpstreamLowerBound(ctx, statePoller, blockNumber, cfg.Evm.MaxAvailableRecentBlocks, forMethod, confidence)
			if err != nil {
				return false, err
			}
			if !available {
				return false, nil
			}
		}

		// If MaxAvailableRecentBlocks is not configured, assume the node can handle the block if it's <= latest
		return blockNumber <= latestBlock, nil
	} else {
		return false, fmt.Errorf("unsupported block availability confidence: %s", confidence)
	}
}

// assertUpstreamLowerBound checks if a full node can handle a block based on its lower bound.
// It returns whether the block is within the available range and records metrics if not.
func (u *Upstream) assertUpstreamLowerBound(ctx context.Context, statePoller common.EvmStatePoller, blockNumber int64, maxAvailableRecentBlocks int64, forMethod string, confidence common.AvailbilityConfidence) (available bool, err error) {
	latestBlock := statePoller.LatestBlock()
	firstAvailableBlock := latestBlock - maxAvailableRecentBlocks
	available = blockNumber >= firstAvailableBlock

	if !available {
		telemetry.MetricUpstreamStaleLowerBound.WithLabelValues(
			u.ProjectId,
			u.VendorName(),
			u.NetworkId(),
			u.Id(),
			forMethod,
			confidence.String(),
		).Inc()
		return false, nil
	}

	// Only poll if near boundary
	// If we assume it is available, we need to poll a fresh latest block to check against potential pruning on full nodes
	shouldPoll := latestBlock > 0 && blockNumber > 0 && math.Abs(float64(blockNumber-(latestBlock-maxAvailableRecentBlocks))) <= 10
	if shouldPoll {
		latestBlock, err = statePoller.PollLatestBlockNumber(ctx)
		if err != nil {
			return false, fmt.Errorf("failed to poll latest block number: %w", err)
		}
		if latestBlock <= 0 {
			return false, fmt.Errorf("upstream latest block is not available")
		}
	}

	firstAvailableBlock = latestBlock - maxAvailableRecentBlocks
	available = blockNumber >= firstAvailableBlock
	if !available {
		telemetry.MetricUpstreamStaleLowerBound.WithLabelValues(
			u.ProjectId,
			u.VendorName(),
			u.NetworkId(),
			u.Id(),
			forMethod,
			confidence.String(),
		).Inc()
	}

	return available, nil
}

func (u *Upstream) IgnoreMethod(method string) {
	ai := u.config.AutoIgnoreUnsupportedMethods
	if ai == nil || !*ai {
		return
	}
	u.logger.Warn().Str("method", method).Msgf("upstream does not support method, dynamically adding to ignoreMethods")

	u.cfgMu.Lock()
	u.config.IgnoreMethods = append(u.config.IgnoreMethods, method)
	u.cfgMu.Unlock()
	u.supportedMethods.Store(method, false)
}

func (u *Upstream) initRateLimitAutoTuner() {
	if u.config.RateLimitBudget != "" && u.config.RateLimitAutoTune != nil {
		cfg := u.config.RateLimitAutoTune
		if cfg.Enabled != nil && *cfg.Enabled {
			budget, err := u.rateLimitersRegistry.GetBudget(u.config.RateLimitBudget)
			if err == nil {
				u.rateLimiterAutoTuner = NewRateLimitAutoTuner(
					u.logger,
					budget,
					cfg.AdjustmentPeriod.Duration(),
					cfg.ErrorRateThreshold,
					cfg.IncreaseFactor,
					cfg.DecreaseFactor,
					cfg.MinBudget,
					cfg.MaxBudget,
				)
			}
		}
	}
}

func (u *Upstream) recordRequestSuccess(method string) {
	if u.rateLimiterAutoTuner != nil {
		u.rateLimiterAutoTuner.RecordSuccess(method)
	}
}

func (u *Upstream) recordRemoteRateLimit(method string) {
	u.metricsTracker.RecordUpstreamRemoteRateLimited(
		u,
		method,
	)

	if u.rateLimiterAutoTuner != nil {
		u.rateLimiterAutoTuner.RecordError(method)
	}
}

func (u *Upstream) ShouldHandleMethod(method string) (v bool, err error) {
	cfg := u.Config()
	if s, ok := u.supportedMethods.Load(method); ok {
		return s.(bool), nil
	}

	v = true

	// First check if method is ignored, and then check if it is explicitly mentioned to be allowed.
	// This order allows an upstream for example to define "ignore all except eth_getLogs".

	u.cfgMu.RLock()
	defer u.cfgMu.RUnlock()

	if cfg.IgnoreMethods != nil {
		for _, m := range cfg.IgnoreMethods {
			match, err := common.WildcardMatch(m, method)
			if err != nil {
				return false, err
			}
			if match {
				v = false
				break
			}
		}
	}

	if cfg.AllowMethods != nil {
		for _, m := range cfg.AllowMethods {
			match, err := common.WildcardMatch(m, method)
			if err != nil {
				return false, err
			}
			if match {
				v = true
				break
			}
		}
	}

	u.supportedMethods.Store(method, v)
	u.logger.Debug().Bool("allowed", v).Str("method", method).Msg("method support result")

	return v, nil
}

func (u *Upstream) detectFeatures(ctx context.Context) error {
	cfg := u.Config()

	if cfg.Type == "" {
		return fmt.Errorf("upstream type not set yet")
	}

	if cfg.Type == common.UpstreamTypeEvm {
		// TODO Move the logic to evm package as a upstream-init hook?
		if cfg.Evm == nil {
			cfg.Evm = &common.EvmUpstreamConfig{}
		}
		if cfg.Evm.ChainId == 0 {
			nid, err := u.EvmGetChainId(ctx)
			if err != nil {
				return common.NewErrUpstreamClientInitialization(
					&common.BaseError{
						Code:  "ErrUpstreamChainIdDetectionFailed",
						Cause: err,
					},
					cfg.Id,
				)
			}
			cfg.Evm.ChainId, err = strconv.ParseInt(nid, 10, 64)
			if err != nil {
				return common.NewErrUpstreamClientInitialization(
					&common.BaseError{
						Code:  "ErrUpstreamChainIdDetectionFailed",
						Cause: err,
					},
					cfg.Id,
				)
			}
		}
		u.networkId = util.EvmNetworkId(cfg.Evm.ChainId)

		if cfg.Evm.MaxAvailableRecentBlocks == 0 && cfg.Evm.NodeType == common.EvmNodeTypeFull {
			cfg.Evm.MaxAvailableRecentBlocks = 128
		}

		// TODO evm: check trace methods availability (by engine? erigon/geth/etc)
		// TODO evm: detect max eth_getLogs max block range
	} else {
		return fmt.Errorf("upstream type not supported: %s", cfg.Type)
	}

	return nil
}

func (u *Upstream) guessVendorName() string {
	endpoint := u.config.Endpoint
	if endpoint == "" {
		return ""
	}

	pu, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}

	// Strip the port if present and get only the hostname part.
	host := pu.Hostname()

	// If the host is an IP address, return it as-is (no root domain concept).
	if ip := net.ParseIP(host); ip != nil {
		return host
	}

	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}

	// Return last two segments as a naive root domain (example.com).
	rooDomain := strings.Join(parts[len(parts)-2:], ".")
	if len(rooDomain) < 5 {
		// Above is a simple heuristic and might not work for multi-level TLDs like co.uk.
		return strings.Join(parts[len(parts)-3:], ".")
	}

	return rooDomain
}

func (u *Upstream) shouldSkip(ctx context.Context, req *common.NormalizedRequest) (reason error, skip bool) {
	if u.config.Shadow != nil && u.config.Shadow.Enabled {
		return common.NewErrUpstreamShadowing(u.config.Id), true
	}
	method, _ := req.Method()

	if u.config.Evm != nil && u.config.Evm.SkipWhenSyncing != nil && *u.config.Evm.SkipWhenSyncing {
		if u.EvmSyncingState() == common.EvmSyncingStateSyncing {
			return common.NewErrUpstreamSyncing(u.config.Id), true
		}
	}

	allowed, err := u.ShouldHandleMethod(method)
	if err != nil {
		return err, true
	}
	if !allowed {
		u.logger.Debug().Str("method", method).Msg("method not allowed or ignored by upstread")
		return common.NewErrUpstreamMethodIgnored(method, u.config.Id), true
	}

	dirs := req.Directives()
	if dirs.UseUpstream != "" {
		match, err := common.WildcardMatch(dirs.UseUpstream, u.config.Id)
		if err != nil {
			return err, true
		}
		if !match {
			return common.NewErrUpstreamNotAllowed(dirs.UseUpstream, u.config.Id), true
		}
	}

	// if block can be determined from request and upstream is only full-node and block is historical skip
	if u.config.Evm != nil && u.config.Evm.MaxAvailableRecentBlocks > 0 {
		_, bn, ebn := evm.ExtractBlockReferenceFromRequest(ctx, req)
		if ebn != nil || bn <= 0 {
			return nil, false
		}

		if lb := u.evmStatePoller.LatestBlock(); lb > 0 && bn < lb-u.config.Evm.MaxAvailableRecentBlocks {
			// Allow requests for block numbers greater than the latest known block
			if bn > lb {
				return nil, false
			}
			return common.NewErrUpstreamNodeTypeMismatch(fmt.Errorf("block number (%d) in request will not yield result for a fullNodeType upstream since it is not recent enough (must be >= %d", bn, lb-u.config.Evm.MaxAvailableRecentBlocks), common.EvmNodeTypeArchive, common.EvmNodeTypeFull), true
		}
	}

	// TODO if block number is extracted from request, check against evm poller's latest block number (force-refresh if stale) and skip if block is after upstream's latest block
	// TODO then we can remove the similar "eth_getLogs upper-bound" check in eth_getLogs.go PreForward hook.

	return nil, false
}

func (u *Upstream) getScoreMultipliers(networkId, method string) *common.ScoreMultiplierConfig {
	if u.config.Routing != nil {
		for _, mul := range u.config.Routing.ScoreMultipliers {
			matchNet, err := common.WildcardMatch(mul.Network, networkId)
			if err != nil {
				continue
			}
			matchMeth, err := common.WildcardMatch(mul.Method, method)
			if err != nil {
				continue
			}
			if matchNet && matchMeth {
				return mul
			}
		}
	}

	return common.DefaultScoreMultiplier
}

func (u *Upstream) MarshalJSON() ([]byte, error) {
	type upstreamPublic struct {
		Id        string                            `json:"id"`
		Metrics   map[string]*health.TrackedMetrics `json:"metrics"`
		NetworkId string                            `json:"networkId"`
	}

	metrics := u.metricsTracker.GetUpstreamMetrics(u)

	uppub := upstreamPublic{
		Id:        u.config.Id,
		Metrics:   metrics,
		NetworkId: u.NetworkId(),
	}

	return sonic.Marshal(uppub)
}

func (u *Upstream) Cordon(method string, reason string) {
	u.metricsTracker.Cordon(u, method, reason)
}

func (u *Upstream) Uncordon(method string) {
	u.metricsTracker.Uncordon(u, method)
}
