package upstream

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// upstreamExecutor owns retry / hedge / breaker / timeout policies
// for one (method-pattern, finality) match at the per-upstream scope.
type upstreamExecutor struct {
	cfg     *common.UpstreamFailsafeConfig
	logger  *zerolog.Logger
	timeout common.TimeoutFunc
	breaker *failsafe.Breaker

	// Cached fields from cfg for hot-path access.
	method     string
	finalities []common.DataFinalityState

	// emptyResultAccept is the method list for hedge cancellation.
	emptyResultAccept []string
}

// NewUpstreamExecutor builds a per-(method, finality) executor for an
// upstream. Returns a no-op-shaped executor when cfg is nil.
func NewUpstreamExecutor(cfg *common.UpstreamFailsafeConfig, logger *zerolog.Logger) (*upstreamExecutor, error) {
	if cfg == nil {
		return &upstreamExecutor{
			method: "*",
			logger: logger,
		}, nil
	}

	if cfg.Consensus != nil {
		return nil, common.NewErrFailsafeConfiguration(
			errors.New("consensus does not make sense for upstream-level requests"),
			map[string]interface{}{
				"policy": cfg.Consensus,
			},
		)
	}

	e := &upstreamExecutor{
		cfg:        cfg,
		logger:     logger,
		method:     cfg.MatchMethod,
		finalities: cfg.MatchFinality,
	}
	if e.method == "" {
		e.method = "*"
	}
	if cfg.Timeout != nil {
		e.timeout = common.NewTimeoutFunc(logger, cfg.Timeout)
	}
	if cfg.CircuitBreaker != nil {
		e.breaker = failsafe.NewBreaker(cfg.CircuitBreaker, logger)
	}
	if cfg.Retry != nil && cfg.Retry.EmptyResultAccept != nil {
		e.emptyResultAccept = cfg.Retry.EmptyResultAccept
	} else {
		e.emptyResultAccept = common.DefaultEmptyResultAccept()
	}
	return e, nil
}

// MatchMethod returns the configured method pattern (or "*").
func (e *upstreamExecutor) MatchMethod() string { return e.method }

// MatchFinality returns the configured finality filter (or nil).
func (e *upstreamExecutor) MatchFinality() []common.DataFinalityState { return e.finalities }

// Timeout exposes the configured TimeoutFunc (nil when no timeout).
func (e *upstreamExecutor) Timeout() common.TimeoutFunc { return e.timeout }

// Breaker exposes the configured *failsafe.Breaker (nil when no
// breaker is configured).
func (e *upstreamExecutor) Breaker() *failsafe.Breaker { return e.breaker }

// hedgeCtxKey is a typed context key used to signal "this is a hedge
// attempt" from the hedge wrapper into the inner client-call path.
type hedgeCtxKey struct{}

func hedgeFromCtx(ctx context.Context) bool {
	v := ctx.Value(hedgeCtxKey{})
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// Run applies retry / hedge / breaker / timeout to `inner`. Internal
// requests (req.IsInternal()) bypass retry, hedge, and breaker — only
// the per-attempt timeout still applies.
func (e *upstreamExecutor) Run(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	if e == nil {
		return inner(ctx, false)
	}

	startTime := time.Now()

	// Internal requests bypass retry+hedge+breaker. The per-attempt
	// timeout still wraps inner.
	if req != nil && req.IsInternal() {
		resp, err := e.callWithTimeout(ctx, req, inner, false)
		return resp, e.wrapTimeout(err, startTime)
	}

	// Retry loop wraps hedge wrapper.
	resp, err := e.runRetry(ctx, req, func(ctx context.Context) (*common.NormalizedResponse, error) {
		return e.runHedge(ctx, req, inner)
	})
	return resp, e.wrapTimeout(err, startTime)
}

// wrapTimeout converts a bare context.Cause sentinel into a typed
// ErrFailsafeTimeoutExceeded so downstream callers can classify it.
// Wraps even if err is already a StandardError, provided this scope
// owns the timeout and the dynamic-timeout sentinel sits anywhere in
// the cause chain — the sentinel is what we promised the caller.
//
// startTime is when Run() began: the error message reports elapsed time
// since then. Passing time.Now() here would render a bogus nanosecond
// duration ("exceeded after 279ns") that reads like an instantly-expired
// deadline.
func (e *upstreamExecutor) wrapTimeout(err error, startTime time.Time) error {
	if err == nil {
		return nil
	}
	if e == nil || e.timeout == nil {
		return err
	}
	if !errors.Is(err, common.ErrDynamicTimeoutExceeded) {
		return err
	}
	// Skip double-wrapping if it's already classified as a failsafe
	// timeout (e.g. nested upstream-scope wrapper from a sub-call).
	if common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded) {
		return err
	}
	return common.NewErrFailsafeTimeoutExceeded(common.ScopeUpstream, err, &startTime)
}

func (e *upstreamExecutor) callWithTimeout(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error),
	isHedge bool,
) (*common.NormalizedResponse, error) {
	if e.timeout != nil {
		if td := e.timeout(ctx, req); td != nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeoutCause(ctx, *td, common.ErrDynamicTimeoutExceeded)
			defer cancel()
		}
	}
	return inner(ctx, isHedge)
}

func (e *upstreamExecutor) runRetry(
	ctx context.Context,
	req *common.NormalizedRequest,
	hedgeWrapped func(ctx context.Context) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	startTime := time.Now()
	maxAttempts := 1
	if e.cfg != nil && e.cfg.Retry != nil && e.cfg.Retry.MaxAttempts > 0 {
		maxAttempts = e.cfg.Retry.MaxAttempts
	}

	var lastErr error
	var lastResp *common.NormalizedResponse
	retriesAttempted := 0
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if st := req.ExecState(); st != nil {
			st.UpstreamAttempts.Add(1)
			if attempt > 0 {
				st.UpstreamRetries.Add(1)
			}
		}
		resp, err := hedgeWrapped(ctx)
		if attempt+1 >= maxAttempts || !e.shouldRetry(req, resp, err, attempt) {
			if err != nil && retriesAttempted > 0 {
				if lastResp != nil {
					return lastResp, nil
				}
				return nil, common.NewErrFailsafeRetryExceeded(common.ScopeUpstream, err, &startTime)
			}
			return resp, err
		}
		lastErr = err
		retriesAttempted++
		if resp != nil {
			if lastResp != nil {
				lastResp.Release()
			}
			lastResp = resp
		}

		d := e.computeDelay(req, resp, err, attempt)
		if d > 0 {
			if serr := failsafe.SleepCtx(ctx, d); serr != nil {
				return lastResp, serr
			}
		}
	}

	if lastErr != nil {
		return lastResp, common.NewErrFailsafeRetryExceeded(common.ScopeUpstream, lastErr, &startTime)
	}
	return lastResp, nil
}

func (e *upstreamExecutor) shouldRetry(req *common.NormalizedRequest, _ *common.NormalizedResponse, err error, _ int) bool {
	if err == nil {
		return false
	}
	if req != nil && req.IsCompositeRequest() {
		return false
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
		// Check retryableTowardNetwork bool on the StandardError details.
		if se, ok := err.(common.StandardError); ok {
			if retryable, ok := se.DeepSearch("retryableTowardNetwork").(bool); ok && retryable {
				return true
			}
		}
		return false
	}
	if req != nil {
		if m, _ := req.Method(); m != "" && (evm.IsNonRetryableWriteMethod(m) || svm.IsNonRetryableWriteMethod(m)) {
			return false
		}
	}
	// Missing-data errors: respect the EXPLICIT RetryEmpty=false
	// directive (caller said "don't retry on empty/missing data").
	// When the directive is unset, fall through to IsRetryableTowardsUpstream.
	if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		if req != nil {
			if rds := req.Directives(); rds != nil && !rds.RetryEmpty {
				return false
			}
		}
	}
	return common.IsRetryableTowardsUpstream(err)
}

func (e *upstreamExecutor) computeDelay(_ *common.NormalizedRequest, _ *common.NormalizedResponse, _ error, attempt int) time.Duration {
	if e.cfg == nil || e.cfg.Retry == nil {
		return 0
	}
	return failsafe.ComputeBackoff(e.cfg.Retry, attempt)
}

func (e *upstreamExecutor) runHedge(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	if e.cfg == nil || e.cfg.Hedge == nil || e.cfg.Hedge.MaxCount <= 0 {
		return e.callBreakerWithTimeout(ctx, req, inner, false)
	}

	// Hedge is not used for composite or write-only requests.
	if req != nil {
		if req.IsCompositeRequest() {
			return e.callBreakerWithTimeout(ctx, req, inner, false)
		}
		if m, _ := req.Method(); m != "" && (evm.IsNonRetryableWriteMethod(m) || svm.IsNonRetryableWriteMethod(m)) {
			return e.callBreakerWithTimeout(ctx, req, inner, false)
		}
	}

	spec := e.cfg.Hedge.Delay

	var fireCount atomic.Int32
	delayFn := func(idx int) time.Duration { return spec.ResolveForRequest(req) }
	innerFn := func(hctx context.Context) (*common.NormalizedResponse, error) {
		isHedge := false
		if v := hctx.Value(hedgeCtxKey{}); v != nil {
			if b, ok := v.(bool); ok && b {
				isHedge = true
			}
		}
		return e.callBreakerWithTimeout(hctx, req, inner, isHedge)
	}
	wrapInner := func(hctx context.Context) (*common.NormalizedResponse, error) {
		// First call uses parent ctx; siblings get the hedge ctx tag.
		idx := fireCount.Add(1)
		if idx > 1 {
			hctx = context.WithValue(hctx, hedgeCtxKey{}, true)
		}
		return innerFn(hctx)
	}
	keep := func(r *common.NormalizedResponse, err error) bool {
		if err != nil {
			if common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted, common.ErrCodeNoUpstreamsLeftToSelect) {
				return true
			}
			return !common.IsRetryableTowardNetwork(err)
		}
		if r == nil || r.IsObjectNull(ctx) {
			return false
		}
		return true
	}
	release := func(r *common.NormalizedResponse) {
		if r != nil {
			r.Release()
		}
	}
	hooks := failsafe.HedgeHooks{
		OnFire: func(fireIdx int, d time.Duration) {
			if st := req.ExecState(); st != nil {
				// Hedge fire = one extra inner invocation at the upstream
				// scope: counts as both an attempt and a hedge.
				st.UpstreamAttempts.Add(1)
				st.UpstreamHedges.Add(1)
			}
		},
	}
	return failsafe.RunHedged[*common.NormalizedResponse](
		ctx,
		e.cfg.Hedge.MaxCount,
		delayFn,
		wrapInner,
		keep,
		release,
		hooks,
	)
}

func (e *upstreamExecutor) callBreakerWithTimeout(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error),
	isHedge bool,
) (*common.NormalizedResponse, error) {
	// Breaker eligibility check — internal probes and hedge attempts do NOT
	// count toward the breaker.
	if e.breaker != nil && upstreamBreakerEligible(req, isHedge) {
		if !e.breaker.TryAcquirePermit() {
			startTime := time.Now()
			return nil, common.NewErrFailsafeCircuitBreakerOpen(common.ScopeUpstream, failsafe.ErrCircuitOpen, &startTime)
		}
	}

	resp, err := e.callWithTimeout(ctx, req, inner, isHedge)

	if e.breaker != nil && upstreamBreakerEligible(req, isHedge) {
		e.breaker.Record(upstreamBreakerOutcome(resp, err))
	}
	return resp, err
}

// upstreamBreakerEligible decides whether (req, isHedge) should contribute
// to the breaker counters. Hedge attempts and internal probes are excluded.
// Composite requests are also excluded.
func upstreamBreakerEligible(req *common.NormalizedRequest, isHedge bool) bool {
	if isHedge {
		return false
	}
	if req == nil {
		return true
	}
	if req.IsInternal() {
		return false
	}
	if req.IsCompositeRequest() {
		return false
	}
	return true
}

// upstreamBreakerOutcome classifies a (resp, err) pair for the upstream
// breaker. Most ignorable errors return OutcomeIgnore so they don't move
// the breaker; transport / 5xx / sync-empty open the circuit; success
// closes it.
func upstreamBreakerOutcome(resp *common.NormalizedResponse, err error) failsafe.Outcome {
	if err != nil {
		// Cancellations / capacity / known-soft errors are ignored.
		if common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled) {
			return failsafe.OutcomeIgnore
		}
		if common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped) {
			return failsafe.OutcomeIgnore
		}
		// 5xx, transport, unauthorized, billing → breaker failure.
		if common.HasErrorCode(err,
			common.ErrCodeEndpointServerSideException,
			common.ErrCodeEndpointTransportFailure,
			common.ErrCodeEndpointUnauthorized,
			common.ErrCodeEndpointBillingIssue,
		) {
			return failsafe.OutcomeFailure
		}
		return failsafe.OutcomeIgnore
	}
	// Success path: syncing + emptyish opens, otherwise close.
	if resp != nil && resp.Request() != nil {
		up := resp.Request().LastUpstream()
		if ups, ok := up.(common.EvmUpstream); ok {
			if ups.EvmSyncingState() == common.EvmSyncingStateSyncing && resp.IsResultEmptyish() {
				return failsafe.OutcomeFailure
			}
		}
	}
	return failsafe.OutcomeSuccess
}

// shouldSkipForEmptyResultAccept checks whether the given method is in the
// caller-provided accept list. Helper used by callers.
func (e *upstreamExecutor) shouldSkipForEmptyResultAccept(method string) bool {
	return slices.Contains(e.emptyResultAccept, method)
}

// String describes the executor for logs. Not on the hot path.
func (e *upstreamExecutor) String() string {
	if e == nil {
		return "upstreamExecutor(nil)"
	}
	var b strings.Builder
	b.WriteString("upstreamExecutor{method=")
	b.WriteString(e.method)
	if len(e.finalities) > 0 {
		b.WriteString(",finalities=[")
		for i, f := range e.finalities {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(f.String())
		}
		b.WriteString("]")
	}
	if e.cfg != nil {
		if e.cfg.Retry != nil {
			b.WriteString(",retry=")
			b.WriteString(strconv.Itoa(e.cfg.Retry.MaxAttempts))
		}
		if e.cfg.CircuitBreaker != nil {
			b.WriteString(",cb=true")
		}
		if e.cfg.Hedge != nil {
			b.WriteString(",hedge=")
			b.WriteString(strconv.Itoa(e.cfg.Hedge.MaxCount))
		}
	}
	b.WriteString("}")
	return b.String()
}

// attempt-counting helper used by callers to attach exec.Attempts() to span.
func attemptSpanAttrs(span trace.Span, attempt, retries, hedges int) {
	if span == nil {
		return
	}
	span.SetAttributes(
		attribute.Int("execution.attempts", attempt),
		attribute.Int("execution.retries", retries),
		attribute.Int("execution.hedges", hedges),
	)
}
