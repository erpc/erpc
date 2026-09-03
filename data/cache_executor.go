package data

import (
	"context"
	"errors"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
)

// cacheLatencyWindow is the sliding window for per-executor latency
// quantiles feeding quantile-driven (dynamic) timeout and hedge. Matches
// the upstream score-metrics fallback window (erpc.ScoreMetricsWindowSize):
// with 10 sub-buckets one rotation happens every ~6s, so a degraded
// connector shows up in the resolved timeout/hedge within seconds and a
// recovered one fully clears the window after a minute.
const cacheLatencyWindow = 1 * time.Minute

// cacheExecutor applies retry / hedge / breaker / timeout policies to a
// single (method-pattern, finality) match per direction (get vs set) on
// a cache connector.
type cacheExecutor struct {
	cfg     *common.CacheFailsafeConfig
	logger  *zerolog.Logger
	breaker *failsafe.Breaker

	// timeoutSpec is cfg.Timeout.Duration with the quantile Min auto-floor
	// applied (same guard as common.NewTimeoutFunc): when Quantile > 0 and
	// Min is unset, Min defaults to Base/2 (or 500ms when Base is zero) so
	// the tracked quantile can't collapse to near-zero by every attempt
	// fast-failing at the previously resolved timeout.
	timeoutSpec *common.AdaptiveDuration

	// latency tracks per-attempt durations of exactly the slice of traffic
	// this executor governs (connector × direction × method-pattern ×
	// finality). It feeds quantile-driven timeout and hedge via
	// AdaptiveDuration.Resolve. Nil when no configured policy is
	// quantile-driven — static configs pay no tracking cost.
	latency *health.QuantileTracker

	method     string
	finalities []common.DataFinalityState
}

// NewCacheExecutor builds a per-(method, finality) cache executor. ctx
// bounds the lifetime of the background latency-window rotation (when a
// quantile-driven policy is configured).
func NewCacheExecutor(ctx context.Context, cfg *common.CacheFailsafeConfig, logger *zerolog.Logger) (*cacheExecutor, error) {
	if cfg == nil {
		return &cacheExecutor{method: "*", logger: logger}, nil
	}
	if cfg.Consensus != nil {
		return nil, common.NewErrFailsafeConfiguration(
			errors.New("consensus is not supported for connector-level failsafe"),
			map[string]interface{}{"policy": "consensus"},
		)
	}

	e := &cacheExecutor{
		cfg:        cfg,
		logger:     logger,
		method:     cfg.MatchMethod,
		finalities: cfg.MatchFinality,
	}
	if e.method == "" {
		e.method = "*"
	}
	if cfg.Timeout != nil && !cfg.Timeout.Duration.IsZero() {
		spec := *cfg.Timeout.Duration
		if spec.Quantile > 0 && spec.Min == 0 {
			if spec.Base > 0 {
				spec.Min = common.Duration(spec.Base.Duration() / 2)
			} else {
				spec.Min = common.Duration(500 * time.Millisecond)
			}
		}
		e.timeoutSpec = &spec
	}
	if cfg.CircuitBreaker != nil {
		e.breaker = failsafe.NewBreaker(cfg.CircuitBreaker, logger)
	}
	if e.needsLatencyTracker() {
		e.latency = health.NewQuantileTracker(logger)
		e.latency.StartRotation(ctx, cacheLatencyWindow)
	}
	return e, nil
}

// needsLatencyTracker reports whether any configured policy resolves a
// duration from tracked latency quantiles.
func (e *cacheExecutor) needsLatencyTracker() bool {
	if e.timeoutSpec != nil && e.timeoutSpec.Quantile > 0 {
		return true
	}
	return e.cfg.Hedge != nil && e.cfg.Hedge.Delay != nil && e.cfg.Hedge.Delay.Quantile > 0
}

// MatchMethod returns the configured method pattern.
func (e *cacheExecutor) MatchMethod() string { return e.method }

// MatchFinality returns the configured finality filter.
func (e *cacheExecutor) MatchFinality() []common.DataFinalityState { return e.finalities }

// RunBytes applies retry / hedge / breaker / timeout to an inner function
// that returns []byte. Used for Get operations.
func (e *cacheExecutor) RunBytes(
	ctx context.Context,
	inner func(ctx context.Context) ([]byte, error),
) ([]byte, error) {
	if e == nil {
		return inner(ctx)
	}
	return e.runRetry(ctx, func(ctx context.Context) ([]byte, error) {
		return e.runHedgeBytes(ctx, inner)
	})
}

// RunVoid applies retry / hedge / breaker / timeout to an inner function
// that returns only error. Used for Set / Delete operations.
func (e *cacheExecutor) RunVoid(
	ctx context.Context,
	inner func(ctx context.Context) error,
) error {
	wrap := func(ctx context.Context) ([]byte, error) {
		return nil, inner(ctx)
	}
	_, err := e.RunBytes(ctx, wrap)
	return err
}

// execStateFromCtx returns the per-request ExecState attached to the
// context, or nil when the cache layer is being driven outside a
// request lifecycle (e.g. background prefetch). Cache-scope counter
// increments are no-ops in that case.
func execStateFromCtx(ctx context.Context) *common.ExecState {
	r := ctx.Value(common.RequestContextKey)
	if r == nil {
		return nil
	}
	req, ok := r.(*common.NormalizedRequest)
	if !ok || req == nil {
		return nil
	}
	return req.ExecState()
}

func (e *cacheExecutor) runRetry(
	ctx context.Context,
	hedged func(ctx context.Context) ([]byte, error),
) ([]byte, error) {
	maxAttempts := 1
	if e.cfg != nil && e.cfg.Retry != nil && e.cfg.Retry.MaxAttempts > 0 {
		maxAttempts = e.cfg.Retry.MaxAttempts
	}
	startTime := time.Now()

	var lastErr error
	for attempt := range maxAttempts {
		if st := execStateFromCtx(ctx); st != nil {
			st.CacheAttempts.Add(1)
			if attempt > 0 {
				st.CacheRetries.Add(1)
			}
		}
		data, err := hedged(ctx)
		if err == nil || !isTransportError(err) {
			return data, err
		}
		lastErr = err

		if attempt < maxAttempts-1 {
			d := failsafe.ComputeBackoff(e.cfg.Retry, attempt)
			if d > 0 {
				if serr := failsafe.SleepCtx(ctx, d); serr != nil {
					return nil, serr
				}
			}
		}
	}
	if lastErr != nil {
		return nil, common.NewErrFailsafeRetryExceeded(scopeConnector, lastErr, &startTime)
	}
	return nil, nil
}

func (e *cacheExecutor) runHedgeBytes(
	ctx context.Context,
	inner func(ctx context.Context) ([]byte, error),
) ([]byte, error) {
	if e.cfg == nil || e.cfg.Hedge == nil || e.cfg.Hedge.MaxCount <= 0 {
		return e.callBreaker(ctx, inner)
	}
	// Quantile-driven delays resolve against this executor's own latency
	// tracker; with no data yet (or a static config) Resolve falls back to
	// Base + Min (cold-start semantics) / plain Base respectively.
	delay := e.cfg.Hedge.Delay.Resolve(e.latency)
	delayFn := func(idx int) time.Duration { return delay }
	wrap := func(hctx context.Context) ([]byte, error) {
		return e.callBreaker(hctx, inner)
	}
	keep := func(data []byte, err error) bool {
		// For cache, any non-transport error is "kept" (not retryable);
		// success is kept too. Transport errors keep the race going.
		if err != nil {
			return !isTransportError(err)
		}
		return true
	}
	hooks := failsafe.HedgeHooks{
		OnFire: func(_ int, _ time.Duration) {
			if st := execStateFromCtx(ctx); st != nil {
				st.CacheAttempts.Add(1)
				st.CacheHedges.Add(1)
			}
		},
	}
	return failsafe.RunHedged[[]byte](
		ctx, e.cfg.Hedge.MaxCount, delayFn, wrap, keep, nil, hooks,
	)
}

func (e *cacheExecutor) callBreaker(
	ctx context.Context,
	inner func(ctx context.Context) ([]byte, error),
) ([]byte, error) {
	if e.breaker != nil {
		if !e.breaker.TryAcquirePermit() {
			startTime := time.Now()
			return nil, common.NewErrFailsafeCircuitBreakerOpen(scopeConnector, failsafe.ErrCircuitOpen, &startTime)
		}
	}
	hasTimeout := false
	if e.timeoutSpec != nil {
		td := e.timeoutSpec.Resolve(e.latency)
		if td > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeoutCause(ctx, td, common.ErrDynamicTimeoutExceeded)
			defer cancel()
			hasTimeout = true
		}
	}
	start := time.Now()
	data, err := inner(ctx)
	dur := time.Since(start)
	ourTimeout := false
	if hasTimeout && err != nil {
		// Translate context.DeadlineExceeded into ErrFailsafeTimeoutExceeded
		// when our own WithTimeoutCause fired (cause==ErrDynamicTimeoutExceeded).
		if cause := context.Cause(ctx); errors.Is(cause, common.ErrDynamicTimeoutExceeded) {
			startTime := time.Now()
			err = common.NewErrFailsafeTimeoutExceeded(scopeConnector, err, &startTime)
			ourTimeout = true
		}
	}
	// interrupted: the surrounding context ended while the attempt ran
	// (hedge sibling won, cache fan-out winner, client disconnect, parent
	// deadline). The measured duration reflects the canceller's speed, not
	// this connector's — neither a latency sample nor a breaker signal.
	interrupted := !ourTimeout && ctx.Err() != nil
	if e.latency != nil && !interrupted {
		// Same spirit as health.Tracker RecordUpstreamDuration:
		// completions are latency signals — success, semantic misses
		// (the connector did real work), and our own timeout expiry
		// (lower bound, keeps a degraded connector's quantile honest).
		// Hard transport failures stay out so a connector failing fast
		// isn't crowned "fast". One deliberate deviation: the upstream
		// tracker records canceled attempts as lower bounds (it scores
		// upstreams that lose hedge races against each other); here a
		// hedge races the SAME connector, so an interrupted attempt's
		// duration measures the winner, not the target — excluded above.
		if err == nil || ourTimeout || !isTransportError(err) {
			e.latency.Add(dur.Seconds())
		}
	}
	if e.breaker != nil {
		e.breaker.Record(breakerOutcome(err, ourTimeout, interrupted))
	}
	return data, err
}

// breakerOutcome classifies a completed cache attempt for the breaker.
// Interrupted attempts (parent context canceled mid-flight) carry no
// signal about THIS connector and are ignored. The executor's own
// timeout expiry counts as a failure: on a best-effort layer a read the
// timeout had to kill is pure tax (the caller pays the wait AND falls
// through anyway), so sustained timeouts open the breaker and exclude
// the connector (selection-policy-like exclusion, fail-fast to upstream
// fallthrough) until half-open probes complete in time again. The
// timeout policy is the sole definition of "too slow" — there is no
// separate slowness threshold. Transport errors are failures.
//
// A semantic miss (not found / expired) is a SUCCESS, not an ignore. The
// breaker's question is "is this connector answering in time", and a miss
// returned inside the budget answers yes — it is a correct, timely reply
// that happens to be empty. Treating it as ignore was wrong twice over:
//
//   - in Closed state an ignored outcome is never pushed to the ring, so
//     the window held only hits and failures. The configured ratio then
//     measured failures against hits+failures rather than against all
//     traffic, making the breaker roughly 1/hit-ratio more sensitive than
//     its config says — ~5x on a connector serving 80% misses.
//   - in HalfOpen an ignored outcome makes no progress, so a trial needed
//     SuccessThresholdCount *hits*, spanning ~1/hit-ratio more requests
//     and giving a stray failure that much more room to re-open it. On a
//     miss-heavy connector the trial could not reliably conclude at all.
//
// Cancellations and non-transport oddities stay ignored: those carry no
// information about the connector either way.
func breakerOutcome(err error, ourTimeout, interrupted bool) failsafe.Outcome {
	if interrupted {
		return failsafe.OutcomeIgnore
	}
	if ourTimeout {
		return failsafe.OutcomeFailure
	}
	if err == nil {
		return failsafe.OutcomeSuccess
	}
	if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		return failsafe.OutcomeSuccess
	}
	if isTransportError(err) {
		return failsafe.OutcomeFailure
	}
	return failsafe.OutcomeIgnore
}
