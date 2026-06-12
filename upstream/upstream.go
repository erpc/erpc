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
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// classifyUpstreamOutcome maps a (resp, err) pair from one upstream
// call into the observability-friendly UpstreamAttemptOutcome enum.
// Used at the boundary of tryForward to populate ExecState +
// MetricUpstreamAttemptOutcomeTotal.
func classifyUpstreamOutcome(resp *common.NormalizedResponse, err error) common.UpstreamAttemptOutcome {
	if err != nil {
		switch {
		case common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled):
			return common.UpstreamOutcomeCancelled
		case errors.Is(err, context.Canceled):
			return common.UpstreamOutcomeCancelled
		case common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded):
			return common.UpstreamOutcomeTimeout
		case errors.Is(err, common.ErrDynamicTimeoutExceeded):
			return common.UpstreamOutcomeTimeout
		case common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen):
			return common.UpstreamOutcomeBreakerOpen
		case common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped),
			common.HasErrorCode(err, common.ErrCodeUpstreamMethodIgnored):
			return common.UpstreamOutcomeSkipped
		case common.HasErrorCode(err,
			common.ErrCodeEndpointCapacityExceeded,
			common.ErrCodeUpstreamRateLimitRuleExceeded,
			common.ErrCodeProjectRateLimitRuleExceeded,
			common.ErrCodeNetworkRateLimitRuleExceeded,
			common.ErrCodeAuthRateLimitRuleExceeded):
			return common.UpstreamOutcomeRateLimited
		case common.HasErrorCode(err, common.ErrCodeEndpointMissingData):
			return common.UpstreamOutcomeMissingData
		case common.HasErrorCode(err, common.ErrCodeEndpointExecutionException):
			return common.UpstreamOutcomeExecRevert
		case common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable):
			return common.UpstreamOutcomeBlockUnavailable
		case common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure):
			return common.UpstreamOutcomeTransportError
		case common.HasErrorCode(err, common.ErrCodeEndpointServerSideException):
			return common.UpstreamOutcomeServerError
		case common.HasErrorCode(err, common.ErrCodeEndpointClientSideException):
			return common.UpstreamOutcomeClientError
		}
		return common.UpstreamOutcomeServerError
	}
	if resp != nil && resp.IsResultEmptyish() {
		return common.UpstreamOutcomeEmpty
	}
	return common.UpstreamOutcomeSuccess
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// makeBreakerTransitionHook returns a closure that emits the
// `upstream_breaker_state_change_total` metric on every state
// transition. Used at NewUpstream time to wire each per-method
// breaker's OnTransition without polluting failsafe/.
func makeBreakerTransitionHook(projectId, upstreamId string) func(failsafe.State, failsafe.State, string) {
	return func(from, to failsafe.State, _ string) {
		telemetry.MetricUpstreamBreakerStateChange.WithLabelValues(
			projectId,
			upstreamId,
			from.String()+"_to_"+to.String(),
		).Inc()
	}
}

// AttemptErrorDetailMaxLen bounds the per-attempt error string that
// `RecordUpstreamAttempt` stores on the request's `ExecState`. The
// stored value powers diagnostics surfaces (admin endpoints, traces,
// the simulator's lifecycle drawer) — it does NOT affect the error
// the caller receives, which is always the full chain.
//
// 200 is the production default — it keeps the per-request execution
// log compact under high RPS. Tooling that wants the full nested
// `caused by` chain (e.g. cmd/erpc-simulator) can raise this via an
// `init()`-time write or zero it out to disable truncation entirely.
var AttemptErrorDetailMaxLen = 200

type Upstream struct {
	ProjectId string
	Client    clients.ClientInterface

	appCtx context.Context
	logger *zerolog.Logger
	config *common.UpstreamConfig
	cfgMu  sync.RWMutex
	vendor common.Vendor

	networkId            atomic.Value
	networkLabel         atomic.Value
	supportedMethods     sync.Map
	metricsTracker       *health.Tracker
	sharedStateRegistry  data.SharedStateRegistry
	failsafeExecutors    []*upstreamExecutor
	rateLimitersRegistry *RateLimitersRegistry
	rateLimiterAutoTuner *RateLimitAutoTuner
	evmStatePoller       common.EvmStatePoller
	svmStatePoller       common.SvmStatePoller
	// True after successful chainId detection/validation; enables short-circuit in EvmGetChainId.
	chainIdValidated atomic.Bool
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

	// Build one upstreamExecutor per Failsafe config entry, plus a no-op
	// catch-all so unmatched (method, finality) pairs always resolve.
	var failsafeExecutors []*upstreamExecutor
	if len(cfg.Failsafe) > 0 {
		for _, fsCfg := range cfg.Failsafe {
			ex, err := NewUpstreamExecutor(fsCfg, &lg)
			if err != nil {
				return nil, err
			}
			// Wire breaker-transition metric. Side-effect-only; failsafe/
			// package stays telemetry-free.
			if b := ex.Breaker(); b != nil {
				b.OnTransition = makeBreakerTransitionHook(projectId, cfg.Id)
			}
			failsafeExecutors = append(failsafeExecutors, ex)
		}
	}

	// Catch-all no-op executor.
	noop, _ := NewUpstreamExecutor(nil, &lg)
	failsafeExecutors = append(failsafeExecutors, noop)

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
		networkLabel:         atomic.Value{},
	}
	pup.networkLabel.Store("n/a")

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

	switch u.config.Type {
	case common.UpstreamTypeEvm:
		u.evmStatePoller = evm.NewEvmStatePoller(u.ProjectId, u.appCtx, u.logger, u, u.metricsTracker, u.sharedStateRegistry)
	case common.UpstreamTypeSvm:
		u.svmStatePoller = svm.NewSvmStatePoller(u.ProjectId, u.appCtx, u.logger, u, u.metricsTracker, u.sharedStateRegistry)
	}

	if u.evmStatePoller != nil {
		err = u.evmStatePoller.Bootstrap(ctx)
		if err != nil {
			// The reason we're not returning error is to allow upstream to still be registered
			// even if background block polling fails initially.
			u.logger.Error().Err(err).Msg("failed on initial bootstrap of evm state poller (will retry in background)")
		}
	}
	if u.svmStatePoller != nil {
		if err := u.svmStatePoller.Bootstrap(ctx); err != nil {
			u.logger.Error().Err(err).Msg("failed on initial bootstrap of svm state poller (will retry in background)")
		}
		if err := u.svmVerifyGenesisHash(ctx); err != nil {
			return err
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
		return "n/a"
	}
	id, ok := u.networkId.Load().(string)
	if !ok {
		return "n/a"
	}
	return id
}

func (u *Upstream) NetworkLabel() string {
	if u == nil {
		return "n/a"
	}
	lbl, ok := u.networkLabel.Load().(string)
	if !ok {
		return u.NetworkId()
	}
	return lbl
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

func (u *Upstream) Tracker() common.HealthTracker {
	if u == nil {
		return nil
	}
	return u.metricsTracker
}

// CircuitBreakerState returns the live state of the circuit breaker on
// the catch-all (`matchMethod: "*"`) failsafe executor, or
// `StateClosed` if no such executor / breaker is configured. Read
// directly from the breaker's atomic state — no simulator-side cache.
// Diagnostic tooling (admin endpoints, simulator panels) consumes this
// for the "circuit" pill so the UI never drifts from reality.
//
// Per-method breakers exist when a config declares method-specific
// failsafe blocks, but for the simulator's per-upstream "row" view a
// single canonical state is what the operator wants — we surface the
// catch-all because that's what serves the vast majority of methods.
func (u *Upstream) CircuitBreakerState() failsafe.State {
	if u == nil {
		return failsafe.StateClosed
	}
	for _, fe := range u.failsafeExecutors {
		if fe.MatchMethod() == "*" && len(fe.MatchFinality()) == 0 {
			if br := fe.Breaker(); br != nil {
				return br.State()
			}
			return failsafe.StateClosed
		}
	}
	return failsafe.StateClosed
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
	if cfg == nil {
		u.logger.Warn().Msg("unexpectedly received nil network config")
		return
	}
	if cfg.Evm != nil && u.evmStatePoller != nil {
		// propagate alias to evm config so the poller can use it without a direct network reference
		u.evmStatePoller.SetNetworkConfig(cfg)
	}
	// Always set networkId from the provided config
	nid := cfg.NetworkId()
	if nid != "" {
		u.networkId.Store(nid)
	}

	// Prefer alias as label when provided; otherwise default to networkId if label is empty or "n/a"
	if cfg.Alias != "" {
		u.networkLabel.Store(cfg.Alias)
	} else {
		if lbl, ok := u.networkLabel.Load().(string); !ok || lbl == "" || lbl == "n/a" {
			u.networkLabel.Store(nid)
		}
	}
}

func (u *Upstream) getFailsafeExecutor(req *common.NormalizedRequest) *upstreamExecutor {
	method, _ := req.Method()
	finality := req.Finality(context.Background())

	// 4-tier priority: method+finality > method > finality > catch-all.
	for _, fe := range u.failsafeExecutors {
		mp, fl := fe.MatchMethod(), fe.MatchFinality()
		if mp != "*" && len(fl) > 0 {
			if matched, _ := common.WildcardMatch(mp, method); matched && slices.Contains(fl, finality) {
				return fe
			}
		}
	}
	for _, fe := range u.failsafeExecutors {
		mp, fl := fe.MatchMethod(), fe.MatchFinality()
		if mp != "*" && len(fl) == 0 {
			if matched, _ := common.WildcardMatch(mp, method); matched {
				return fe
			}
		}
	}
	for _, fe := range u.failsafeExecutors {
		mp, fl := fe.MatchMethod(), fe.MatchFinality()
		if mp == "*" && len(fl) > 0 {
			if slices.Contains(fl, finality) {
				return fe
			}
		}
	}
	for _, fe := range u.failsafeExecutors {
		mp, fl := fe.MatchMethod(), fe.MatchFinality()
		if mp == "*" && len(fl) == 0 {
			return fe
		}
	}
	return nil
}

func (u *Upstream) Forward(ctx context.Context, nrq *common.NormalizedRequest, byPassMethodExclusion, isHedgeAttempt bool) (*common.NormalizedResponse, error) {
	// TODO Should we move byPassMethodExclusion to directives? How do we prevent clients from setting it?
	startTime := time.Now()
	cfg := u.Config()

	method, err := nrq.Method()
	ctx, span := common.StartSpan(ctx, "Upstream.Forward",
		trace.WithAttributes(
			attribute.String("network.id", u.NetworkId()),
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
			u,
			u.NetworkId(),
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

	lg := u.logger.With().Str("method", method).Str("networkId", u.NetworkId()).Interface("id", nrq.ID()).Logger()

	if limitersBudget != nil {
		lg.Trace().Str("budget", cfg.RateLimitBudget).Msgf("checking upstream-level rate limiters budget")
		rules, err := limitersBudget.GetRulesByMethod(method)
		if err != nil {
			common.SetTraceSpanError(span, err)
			return nil, err
		}
		if len(rules) > 0 {
			allowed, err := limitersBudget.TryAcquirePermit(ctx, u.ProjectId, nrq, method, u.VendorName(), cfg.Id, "", "upstream")
			if err != nil {
				common.SetTraceSpanError(span, err)
				return nil, err
			}
			if !allowed {
				lg.Debug().Str("budget", cfg.RateLimitBudget).Msgf("upstream-level rate limit exceeded")
				err = common.NewErrUpstreamRateLimitRuleExceeded(
					cfg.Id,
					cfg.RateLimitBudget,
					fmt.Sprintf("method:%s", method),
				)
				common.SetTraceSpanError(span, err)
				return nil, err
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
			isHedge bool,
		) (resp *common.NormalizedResponse, retErr error) {
			st := nrq.ExecState()
			snap := st.Snapshot()
			attemptStart := time.Now()
			// Defer the participant record. We capture the named-return
			// (resp, retErr) at exit so the outcome reflects the actual
			// classification path.
			defer func() {
				if st == nil {
					return
				}
				outcome := classifyUpstreamOutcome(resp, retErr)
				reason := common.SelectionReasonPrimary
				if isHedge {
					reason = common.SelectionReasonHedge
				} else if snap.Retries > 0 {
					reason = common.SelectionReasonRetry
				}
				errCode := ""
				errDetail := ""
				if retErr != nil {
					errCode = string(common.ErrorFingerprint(retErr))
					es := retErr.Error()
					if max := AttemptErrorDetailMaxLen; max > 0 && len(es) > max {
						es = es[:max]
					}
					errDetail = es
				}
				st.RecordUpstreamAttempt(common.UpstreamAttempt{
					UpstreamId:  cfg.Id,
					VendorName:  u.VendorName(),
					StartedAt:   attemptStart,
					Duration:    time.Since(attemptStart),
					Outcome:     outcome,
					Reason:      reason,
					IsHedge:     isHedge || isHedgeAttempt,
					IsRetry:     snap.Retries > 0,
					AttemptIdx:  snap.Attempts,
					ErrorCode:   errCode,
					ErrorDetail: errDetail,
				})
				// Prometheus: one outcome counter increment per attempt.
				finality := nrq.Finality(ctx)
				telemetry.MetricUpstreamAttemptOutcomeTotal.WithLabelValues(
					u.ProjectId,
					nrq.NetworkLabel(),
					cfg.Id,
					method,
					string(outcome),
					boolStr(isHedge || isHedgeAttempt),
					boolStr(snap.Retries > 0),
					finality.String(),
				).Inc()
				telemetry.MetricUpstreamSelectionTotal.WithLabelValues(
					u.ProjectId,
					nrq.NetworkLabel(),
					cfg.Id,
					method,
					string(reason),
					finality.String(),
				).Inc()
			}()

			// Span to track pre-request overhead (metrics, finality calculation)
			_, preReqSpan := common.StartDetailSpan(ctx, "Upstream.tryForward.PreRequest")

			// Hedge attempts are excluded from the per-upstream rate counters
			// (RequestsTotal / ErrorsTotal). They are speculative extra fan-out
			// triggered by failsafe; counting them inflates the denominator on
			// every hedged request and, if their cancellations were also counted
			// as failures, would double-penalize slow-but-functional upstreams
			// (latency already feeds the score). Hedge activity stays observable
			// via MetricNetworkHedgeDiscardsTotal at the network layer, and
			// successful hedge responses still contribute to ResponseQuantiles
			// (filtered by isSuccess inside the tracker), so the latency signal
			// is preserved.
			//
			// isHedgeAttempt is the OR of the explicit network-passed flag
			// and any hedge fan-out from the upstream's own executor.
			hedgeAttempt := isHedgeAttempt || isHedge
			finality := nrq.Finality(ctx)
			if !hedgeAttempt {
				u.metricsTracker.RecordUpstreamRequest(
					u,
					method,
					finality,
				)
			}
			// TODO(memory): this and the matching MetricUpstreamErrorTotal /
			// MetricUpstreamWrongEmptyResponseTotal / MetricUpstreamCanceledTotal
			// sites in this file + erpc/networks.go all emit label-sets
			// keyed by user-controlled inputs (method, finality, userId,
			// agentName, etc.) WITHOUT going through a tracker cache, so
			// the Prometheus registry accumulates one series per unique
			// combo forever — even after the in-memory caches added in
			// the 826df9f5 idle-sweep get cleared.
			//
			// The fix is parallel to the urdObsCache / remoteRateLimited
			// pattern: wrap each WithLabelValues call in a cached*-style
			// indirection that remembers the label tuple + a
			// lastAccessedAtMs, then sweep on idle with
			// MetricVec.DeleteLabelValues. Each direct call site needs the
			// same retrofit. Out of scope for this PR — flagged for a
			// follow-up titled "sweep direct Prom emissions in
			// upstream/erpc hot paths".
			telemetry.MetricUpstreamRequestTotal.WithLabelValues(
				u.ProjectId,
				u.VendorName(),
				u.NetworkLabel(),
				cfg.Id,
				method,
				strconv.Itoa(snap.Attempts),
				nrq.CompositeType(),
				finality.String(),
				nrq.UserId(),
				nrq.AgentName(),
			).Inc()
			timer := u.metricsTracker.RecordUpstreamDurationStart(u, method, nrq.CompositeType(), finality, nrq.UserId())

			preReqSpan.End()

			// Span to track the actual client request
			ctx, sendSpan := common.StartDetailSpan(ctx, "Upstream.tryForward.SendRequest",
				trace.WithAttributes(
					attribute.String("client.type", string(clientType)),
				),
			)
			nrs, errCall := u.Client.SendRequest(ctx, nrq)
			sendSpan.End()
			isSuccess := false
			if errCall == nil && nrs != nil {
				nrs.SetUpstream(u)
				jrr, _ := nrs.JsonRpcResponse()
				if jrr != nil && jrr.Error == nil {
					nrq.SetLastValidResponse(ctx, nrs)
					isSuccess = true
					// Track decoded result-body size to surface networks/methods
					// that drive transient heap spikes via the JSON parse pipeline
					// (each response peaks at ~3-4× its size in transient allocs:
					// io.Copy → bytes.Buffer.grow → sonic.Unmarshal → []byte copy).
					// Labels intentionally exclude vendor/upstream/user — those
					// are correlatable via upstream_request_total and adding them
					// here multiplies cardinality without changing the diagnostic
					// answer (which net+method emits fat responses?).
					if size := jrr.ResultLength(); size > 0 {
						telemetry.ObserverHandle(
							telemetry.MetricUpstreamResponseSizeBytes,
							u.ProjectId,
							u.NetworkLabel(),
							method,
							finality.String(),
						).Observe(float64(size))
					}
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
					telemetry.MetricUpstreamSkippedTotal.WithLabelValues(
						u.ProjectId,
						u.VendorName(),
						u.NetworkLabel(),
						cfg.Id,
						method,
						finality.String(),
						nrq.UserId(),
						nrq.AgentName(),
					).Inc()
				} else if common.HasErrorCode(errCall, common.ErrCodeEndpointMissingData) {
					telemetry.MetricUpstreamMissingDataErrorTotal.WithLabelValues(
						u.ProjectId,
						u.VendorName(),
						u.NetworkLabel(),
						cfg.Id,
						method,
						finality.String(),
						nrq.UserId(),
						nrq.AgentName(),
					).Inc()
				} else if common.HasErrorCode(errCall, common.ErrCodeEndpointRequestCanceled) {
					// Cancelled request (hedge lost the race or client
					// disconnected). Not attributable to upstream quality
					// from THIS layer for the error-rate counter — skip
					// RecordUpstreamFailure and the generic error metric.
					// Hedge discards are accounted at the network layer
					// via MetricNetworkHedgeDiscardsTotal.
					//
					// Latency observation is also skipped (see the
					// ObserveDuration call below) — elapsed-by-cancel is
					// not a natural latency sample. Probe traffic from
					// `probeExcluded` provides successful samples for
					// upstreams that never win the hedge race.
				} else {
					if common.HasErrorCode(errCall, common.ErrCodeEndpointCapacityExceeded) {
						u.recordRemoteRateLimit(ctx, method, nrq)
					}
					if !hedgeAttempt {
						u.metricsTracker.RecordUpstreamFailure(
							u,
							method,
							finality,
							errCall,
						)
					}
					severity := common.ClassifySeverity(errCall)
					telemetry.MetricUpstreamErrorTotal.WithLabelValues(
						u.ProjectId,
						u.VendorName(),
						u.NetworkLabel(),
						cfg.Id,
						method,
						common.ErrorFingerprint(errCall),
						string(severity),
						nrq.CompositeType(),
						finality.String(),
						nrq.UserId(),
						nrq.AgentName(),
					).Inc()
				}

				// Only ExecutionException (EVM revert) feeds the latency
				// quantile — the upstream did real work; the duration
				// reflects actual response time.
				//
				// Cancellations (hedge-lost / client gave up / engine
				// timeout) are deliberately excluded: the elapsed-by-
				// cancel time is not natural latency. An upstream that
				// only ever loses hedge races (e.g. always sees
				// hedge-fired slow methods because the primary handles
				// the fast ones) would otherwise accumulate a quantile
				// of 100% cancellation observations, biasing its p70
				// toward "however long the winner of the race took
				// before cancellation." Cross-upstream comparisons
				// (e.g. `latencyDeviationAbove`) would trip on this
				// artificial signal even when per-method latencies are
				// identical.
				//
				// With probeExcluded in the chain (default), excluded
				// upstreams accumulate real successful samples via
				// shadow-mirrored traffic — so the "every-hedge-loser
				// stays optimistic forever" concern is handled without
				// polluting the quantile. Without probeExcluded, rely
				// on absolute thresholds (`latencyAbove(N)`) +
				// non-latency signals (error rate, throttle rate, lag).
				//
				// All other errors fired before / instead of a meaningful
				// response and would conflate "broken" with "slow" if
				// counted; they stay out of the quantile.
				isRevert := common.HasErrorCode(errCall, common.ErrCodeEndpointExecutionException)
				timer.ObserveDuration(isRevert)
				if isRevert {
					nrq.ClearLastValidResponse()
				}
				if nrs != nil {
					nrs.Release()
					nrs = nil
				}
				return nil, common.NewErrUpstreamRequest(
					errCall,
					u,
					u.NetworkId(),
					method,
					time.Since(startTime),
					snap.Attempts,
					snap.Retries,
					snap.Hedges,
				)
			} else {
				emptyish := nrs.IsResultEmptyish()
				if emptyish {
					telemetry.MetricUpstreamEmptyResponseTotal.WithLabelValues(
						u.ProjectId,
						u.VendorName(),
						u.NetworkLabel(),
						cfg.Id,
						method,
						finality.String(),
						nrq.UserId(),
						nrq.AgentName(),
					).Inc()
				}
				// Empty responses shouldn't contribute to latency quantiles — fast empty
				// returns reflect data absence, not superior upstream performance.
				timer.ObserveDuration(isSuccess && !emptyish)
			}

			if isSuccess {
				u.recordRequestSuccess(method)
			}

			return nrs, nil
		}

		failsafeExecutor := u.getFailsafeExecutor(nrq)
		if failsafeExecutor == nil {
			return nil, fmt.Errorf("no failsafe executor found for request")
		}

		// In-house executor: retry + hedge + breaker + timeout. Returns
		// typed errors directly (ErrFailsafeRetryExceeded /
		// ErrFailsafeCircuitBreakerOpen / ErrFailsafeTimeoutExceeded);
		// no translation pass needed.
		resp, execErr := failsafeExecutor.Run(ctx, nrq, tryForward)

		if _, ok := execErr.(common.StandardError); !ok && execErr != nil {
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
			// Timeout-attribution metric: emit only when this scope's policy
			// fired the timeout (cause is ErrDynamicTimeoutExceeded) and the
			// error is not retry-exhausted (which wins ordering).
			if failsafeExecutor.Timeout() != nil &&
				!common.HasErrorCode(execErr, common.ErrCodeFailsafeRetryExceeded) &&
				errors.Is(execErr, common.ErrDynamicTimeoutExceeded) {
				finality := nrq.Finality(ctx)
				telemetry.MetricNetworkTimeoutFiredTotal.WithLabelValues(
					u.ProjectId,
					nrq.NetworkLabel(),
					method,
					finality.String(),
					string(common.ScopeUpstream),
				).Inc()
			}
			// Wrap bare timeout sentinel as a typed error so downstream
			// callers can pattern-match on ErrCodeFailsafeTimeoutExceeded.
			if _, isStd := execErr.(common.StandardError); !isStd &&
				errors.Is(execErr, common.ErrDynamicTimeoutExceeded) {
				execErr = common.NewErrFailsafeTimeoutExceeded(common.ScopeUpstream, execErr, &startTime)
			}
			return nil, execErr
		}

		return resp, nil
	default:
		err := common.NewErrUpstreamClientInitialization(
			fmt.Errorf("unsupported client type during forward: %s", clientType),
			u,
		)
		common.SetTraceSpanError(span, err)
		return nil, err
	}
}

// resolveAvailabilityBounds computes effective [min,max] based on BlockAvailabilityConfig.
// Returns MinInt64/MaxInt64 when a side is unbounded.
func (u *Upstream) resolveAvailabilityBounds() (int64, int64) {
	cfg := u.Config()
	if cfg == nil || cfg.Evm == nil {
		return math.MinInt64, math.MaxInt64
	}
	// Legacy fallback: if explicit blockAvailability is not configured, but
	// MaxAvailableRecentBlocks is set, treat lower bound as latest - N.
	if cfg.Evm.BlockAvailability == nil {
		if cfg.Evm.MaxAvailableRecentBlocks > 0 {
			latest := int64(0)
			if u.evmStatePoller != nil {
				latest = u.evmStatePoller.LatestBlock()
			}
			if latest > 0 {
				return latest - cfg.Evm.MaxAvailableRecentBlocks, math.MaxInt64
			}
		}
		return math.MinInt64, math.MaxInt64
	}
	latest := int64(0)

	compute := func(bound *common.EvmAvailabilityBoundConfig, isLower bool) int64 {
		if bound == nil {
			if isLower {
				return math.MinInt64
			}
			return math.MaxInt64
		}
		probe := bound.Probe
		if probe == "" {
			probe = common.EvmProbeBlockHeader
		}

		// Compute new value
		var val int64
		if bound.ExactBlock != nil {
			val = *bound.ExactBlock
		} else if bound.EarliestBlockPlus != nil {
			eb := int64(0)
			if u.evmStatePoller != nil && !u.evmStatePoller.IsObjectNull() {
				eb = u.evmStatePoller.EarliestBlock(probe)
			}
			// eb == 0 means detection not yet complete - compute val = 0 + offset
			// This may create an invalid range (min > max) which triggers fail-open at the end
			// eb > 0 means detection successful - use the detected value
			if eb >= 0 {
				val = eb + *bound.EarliestBlockPlus
			} else {
				// If earliest is negative (shouldn't happen), treat as unbounded
				if isLower {
					val = math.MinInt64
				} else {
					val = math.MaxInt64
				}
			}
		} else if bound.LatestBlockMinus != nil {
			if latest == 0 {
				if u.evmStatePoller != nil {
					latest = u.evmStatePoller.LatestBlock()
				}
			}
			if latest > 0 {
				val = latest - *bound.LatestBlockMinus
				u.logger.Debug().
					Int64("latestBlock", latest).
					Int64("minus", *bound.LatestBlockMinus).
					Int64("computedBound", val).
					Str("boundType", map[bool]string{true: "lower", false: "upper"}[isLower]).
					Str("upstreamId", u.config.Id).
					Msg("computed bound from latest block")
			} else {
				u.logger.Debug().
					Str("boundType", map[bool]string{true: "lower", false: "upper"}[isLower]).
					Str("upstreamId", u.config.Id).
					Msg("latest block not available; treating bound as unbounded")
				if isLower {
					val = math.MinInt64
				} else {
					val = math.MaxInt64
				}
			}
		} else {
			// No-op bound
			if isLower {
				val = math.MinInt64
			} else {
				val = math.MaxInt64
			}
		}

		return val
	}

	minVal := compute(cfg.Evm.BlockAvailability.Lower, true)
	maxVal := compute(cfg.Evm.BlockAvailability.Upper, false)
	// If both sides are finite and the computed range is invalid (min > max),
	// fail-open for safety: treat as unbounded to avoid breaking production traffic.
	if minVal != math.MinInt64 && maxVal != math.MaxInt64 && minVal > maxVal {
		u.logger.Warn().
			Int64("computedMinBound", minVal).
			Int64("computedMaxBound", maxVal).
			Str("upstreamId", u.config.Id).
			Msg("computed block availability bounds are invalid (min > max); treating as unbounded for safety")
		return math.MinInt64, math.MaxInt64
	}
	return minVal, maxVal
}

// EvmEffectiveLatestBlock returns the latest block adjusted for the upstream's upper availability bound.
// If the upstream has a blockAvailability.upper config (e.g., latestBlockMinus: 5), this returns
// min(latestBlock, upperBound) instead of the raw latest block.
func (u *Upstream) EvmEffectiveLatestBlock() int64 {
	if u == nil || u.evmStatePoller == nil || u.evmStatePoller.IsObjectNull() {
		return 0
	}
	latestBlock := u.evmStatePoller.LatestBlock()
	if latestBlock <= 0 {
		return 0
	}

	_, maxBound := u.resolveAvailabilityBounds()
	if maxBound != math.MaxInt64 && maxBound < latestBlock {
		return maxBound
	}
	return latestBlock
}

// EvmEffectiveFinalizedBlock returns the finalized block adjusted for the upstream's upper availability bound.
// If the upstream has a blockAvailability.upper config, this returns min(finalizedBlock, upperBound).
func (u *Upstream) EvmEffectiveFinalizedBlock() int64 {
	if u == nil || u.evmStatePoller == nil || u.evmStatePoller.IsObjectNull() {
		return 0
	}
	finalizedBlock := u.evmStatePoller.FinalizedBlock()
	if finalizedBlock <= 0 {
		return 0
	}

	_, maxBound := u.resolveAvailabilityBounds()
	if maxBound != math.MaxInt64 && maxBound < finalizedBlock {
		return maxBound
	}
	return finalizedBlock
}

// EvmBlockAvailabilityBounds returns the resolved [min, max] block availability bounds
// for this upstream based on its BlockAvailability configuration.
// Returns (math.MinInt64, math.MaxInt64) when unbounded on either side.
// This is used post-forward to determine if an upstream's MissingData response
// is expected (block outside configured range) vs. a genuine wrong-empty misbehavior.
func (u *Upstream) EvmBlockAvailabilityBounds() (int64, int64) {
	return u.resolveAvailabilityBounds()
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
			u.NetworkLabel(),
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
			u.NetworkLabel(),
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

func (u *Upstream) recordRemoteRateLimit(ctx context.Context, method string, nrq *common.NormalizedRequest) {
	u.metricsTracker.RecordUpstreamRemoteRateLimited(
		ctx,
		u,
		method,
		nrq,
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
		nid, err := u.EvmGetChainId(ctx)
		if err != nil {
			// RPC / network failure — potentially transient (provider outage,
			// rate limit during startup, DNS blip). Leave the init error
			// unwrapped so the Initializer's auto-retry loop can keep trying;
			// the upstream will self-heal if the provider recovers.
			return common.NewErrUpstreamClientInitialization(
				&common.BaseError{
					Code:  "ErrUpstreamChainIdDetectionFailed",
					Cause: err,
				},
				u,
			)
		}
		realChainID, err := strconv.ParseInt(nid, 0, 64)
		if err != nil {
			// Upstream returned a non-numeric chainId — won't self-heal on
			// retry. Wrap with NewTaskFatal so the Initializer stops retrying.
			return common.NewTaskFatal(common.NewErrUpstreamClientInitialization(
				&common.BaseError{
					Code:  "ErrUpstreamChainIdDetectionFailed",
					Cause: err,
				},
				u,
			))
		}
		if cfg.Evm.ChainId > 0 && cfg.Evm.ChainId != realChainID {
			// Misconfiguration (wrong upstream for this network) — permanent.
			// Wrap with NewTaskFatal so the Initializer stops retrying.
			return common.NewTaskFatal(common.NewErrUpstreamClientInitialization(
				&common.BaseError{
					Code:  "ErrUpstreamChainIdMismatch",
					Cause: fmt.Errorf("chainId mismatch: configured %d, detected %d", cfg.Evm.ChainId, realChainID),
				},
				u,
			))
		}
		cfg.Evm.ChainId = realChainID
		u.networkId.Store(util.EvmNetworkId(cfg.Evm.ChainId))
		u.chainIdValidated.Store(true)

		// @deprecated: NodeType-specific logic removed; availability is handled by blockAvailability bounds.

		// TODO evm: check trace methods availability (by engine? erigon/geth/etc)
		// TODO evm: detect max eth_getLogs max block range
	} else if cfg.Type == common.UpstreamTypeSvm {
		if cfg.Svm == nil {
			return common.NewErrUpstreamClientInitialization(
				fmt.Errorf("svm upstream %q is missing svm config", cfg.Id), u,
			)
		}
		if cfg.Svm.Cluster == "" {
			return common.NewErrUpstreamClientInitialization(
				fmt.Errorf("svm upstream %q is missing svm.cluster", cfg.Id), u,
			)
		}
		u.networkId.Store(util.SvmNetworkId(cfg.Svm.Chain, cfg.Svm.Cluster))
		// Genesis-hash validation runs in Bootstrap (svmVerifyGenesisHash) after
		// the state poller is in place, so calls there can be made through the
		// upstream's normal Forward path.
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
		return "unknown-" + host
	}

	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return "unknown-" + host
	}

	// Return last two segments as a naive root domain (example.com).
	rooDomain := strings.Join(parts[len(parts)-2:], ".")
	if len(rooDomain) < 5 {
		// Above is a simple heuristic and might not work for multi-level TLDs like co.uk.
		return "unknown-" + strings.Join(parts[len(parts)-3:], ".")
	}

	return "unknown-" + rooDomain
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
	if dirs != nil && dirs.UseUpstream != "" {
		match, err := common.UpstreamMatchesSelector(dirs.UseUpstream, u)
		if err != nil {
			return err, true
		}
		if !match {
			return common.NewErrUpstreamNotAllowed(dirs.UseUpstream, u.config.Id), true
		}
	}

	// Block availability bound enforcement (lower/upper) lives in a single place:
	// Network.checkUpstreamBlockAvailability. It runs whenever the upstream has
	// BlockAvailability bounds configured (or EnforceBlockAvailability resolves
	// to true), classifies head-of-chain races within MaxRetryableBlockDistance
	// as retryable, and routes through handleBlockSkip so the failsafe retry
	// policy can apply blockUnavailableDelay. Centralising it there avoids the
	// duplicate-error-class footgun where an early upstream-level check would
	// short-circuit the retryable classification with a non-retryable
	// ErrUpstreamRequestSkipped.

	return nil, false
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

func (u *Upstream) Uncordon(method string, reason string) {
	u.metricsTracker.Uncordon(u, method, reason)
}

// CordonedReason returns the cordon reason and whether the (upstream,
// method) is currently cordoned. Pass `"*"` for the wildcard scope.
func (u *Upstream) CordonedReason(method string) (string, bool) {
	return u.metricsTracker.CordonedReason(u, method)
}
