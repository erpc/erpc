package data

import (
	"context"
	"errors"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/rs/zerolog"
)

// cacheExecutor applies retry / hedge / breaker / timeout policies to a
// single (method-pattern, finality) match per direction (get vs set) on
// a cache connector.
type cacheExecutor struct {
	cfg     *common.CacheFailsafeConfig
	logger  *zerolog.Logger
	timeout common.TimeoutFunc
	breaker *failsafe.Breaker

	method     string
	finalities []common.DataFinalityState
}

// NewCacheExecutor builds a per-(method, finality) cache executor.
func NewCacheExecutor(cfg *common.CacheFailsafeConfig, logger *zerolog.Logger) (*cacheExecutor, error) {
	if cfg == nil {
		return &cacheExecutor{method: "*", logger: logger}, nil
	}
	if cfg.Consensus != nil {
		return nil, common.NewErrFailsafeConfiguration(
			errors.New("consensus is not supported for connector-level failsafe"),
			map[string]interface{}{"policy": "consensus"},
		)
	}
	if cfg.Hedge != nil && cfg.Hedge.Delay != nil && cfg.Hedge.Delay.Quantile > 0 {
		return nil, common.NewErrFailsafeConfiguration(
			errors.New("hedge quantile is not supported for connector-level failsafe (no latency metric source)"),
			map[string]interface{}{"policy": "hedge.quantile"},
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
	if cfg.Timeout != nil {
		e.timeout = common.NewTimeoutFunc(logger, cfg.Timeout)
	}
	if cfg.CircuitBreaker != nil {
		e.breaker = failsafe.NewBreaker(cfg.CircuitBreaker, logger)
	}
	return e, nil
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
	for attempt := 0; attempt < maxAttempts; attempt++ {
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
	// Cache scope has no QuantileTracker, so Resolve(nil) returns
	// Base + Min (cold-start semantics). Static delays just yield Base.
	delay := e.cfg.Hedge.Delay.Resolve(nil)
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
	return failsafe.RunHedged[[]byte](
		ctx, e.cfg.Hedge.MaxCount, delayFn, wrap, keep, nil, failsafe.HedgeHooks{},
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
	if e.cfg != nil && e.cfg.Timeout != nil {
		td := e.cfg.Timeout.Duration.Resolve(nil)
		if td > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeoutCause(ctx, td, common.ErrDynamicTimeoutExceeded)
			defer cancel()
			hasTimeout = true
		}
	}
	data, err := inner(ctx)
	if hasTimeout && err != nil {
		// Translate context.DeadlineExceeded into ErrFailsafeTimeoutExceeded
		// when our own WithTimeoutCause fired (cause==ErrDynamicTimeoutExceeded).
		if cause := context.Cause(ctx); errors.Is(cause, common.ErrDynamicTimeoutExceeded) {
			startTime := time.Now()
			err = common.NewErrFailsafeTimeoutExceeded(scopeConnector, err, &startTime)
		}
	}
	if e.breaker != nil {
		e.breaker.Record(cacheBreakerOutcome(data, err))
	}
	return data, err
}

// cacheBreakerOutcome classifies a cache (data, err) pair. RecordNotFound
// and RecordExpired are ignored (not a breaker signal); transport errors
// are failures; success closes.
func cacheBreakerOutcome(_ []byte, err error) failsafe.Outcome {
	if err == nil {
		return failsafe.OutcomeSuccess
	}
	if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
		return failsafe.OutcomeIgnore
	}
	if isTransportError(err) {
		return failsafe.OutcomeFailure
	}
	return failsafe.OutcomeIgnore
}
