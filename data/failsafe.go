package data

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/hedgepolicy"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/failsafe-go/failsafe-go/timeout"
	"github.com/rs/zerolog"
)

var scopeConnector = common.Scope("connector")

type CacheFailsafeExecutor struct {
	method     string
	finalities []common.DataFinalityState
	executor   failsafe.Executor[[]byte]
}

type FailsafeConnector struct {
	wrapped      Connector
	logger       *zerolog.Logger
	getExecutors []*CacheFailsafeExecutor
	setExecutors []*CacheFailsafeExecutor
}

var _ Connector = (*FailsafeConnector)(nil)

func NewFailsafeConnector(
	logger *zerolog.Logger,
	wrapped Connector,
	getCfgs []*common.FailsafeConfig,
	setCfgs []*common.FailsafeConfig,
) (*FailsafeConnector, error) {
	lg := logger.With().Str("component", "failsafeConnector").Str("connectorId", wrapped.Id()).Logger()

	getExecutors, err := buildExecutors(&lg, wrapped.Id(), getCfgs)
	if err != nil {
		return nil, err
	}
	setExecutors, err := buildExecutors(&lg, wrapped.Id(), setCfgs)
	if err != nil {
		return nil, err
	}

	return &FailsafeConnector{
		wrapped:      wrapped,
		logger:       &lg,
		getExecutors: getExecutors,
		setExecutors: setExecutors,
	}, nil
}

func buildExecutors(logger *zerolog.Logger, connectorId string, cfgs []*common.FailsafeConfig) ([]*CacheFailsafeExecutor, error) {
	var executors []*CacheFailsafeExecutor

	for _, fsCfg := range cfgs {
		policiesMap, err := CreateCacheFailsafePolicies(logger, connectorId, fsCfg)
		if err != nil {
			return nil, err
		}
		policiesArray := toCachePolicyArray(policiesMap)

		method := fsCfg.MatchMethod
		if method == "" {
			method = "*"
		}

		executors = append(executors, &CacheFailsafeExecutor{
			method:     method,
			finalities: fsCfg.MatchFinality,
			executor:   failsafe.NewExecutor(policiesArray...),
		})
	}

	// Append a no-op fallback executor so unmatched operations always have an executor
	executors = append(executors, &CacheFailsafeExecutor{
		method:     "*",
		finalities: nil,
		executor:   failsafe.NewExecutor[[]byte](),
	})

	return executors, nil
}

func CreateCacheFailsafePolicies(
	logger *zerolog.Logger,
	connectorId string,
	fsCfg *common.FailsafeConfig,
) (map[string]failsafe.Policy[[]byte], error) {
	policies := map[string]failsafe.Policy[[]byte]{}

	if fsCfg == nil {
		return policies, nil
	}

	if fsCfg.Consensus != nil {
		return nil, common.NewErrFailsafeConfiguration(
			errors.New("consensus is not supported for connector-level failsafe"),
			map[string]interface{}{
				"connectorId": connectorId,
				"policy":      "consensus",
			},
		)
	}

	if fsCfg.Hedge != nil && fsCfg.Hedge.Quantile > 0 {
		return nil, common.NewErrFailsafeConfiguration(
			errors.New("hedge quantile is not supported for connector-level failsafe (no latency metric source)"),
			map[string]interface{}{
				"connectorId": connectorId,
				"policy":      "hedge",
			},
		)
	}

	if fsCfg.Timeout != nil {
		plc, err := createCacheTimeoutPolicy(logger, fsCfg.Timeout)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(
				err,
				map[string]interface{}{
					"connectorId": connectorId,
					"policy":      "timeout",
				},
			)
		}
		policies["timeout"] = plc
	}

	if fsCfg.Retry != nil {
		plc, err := createCacheRetryPolicy(logger, connectorId, fsCfg.Retry)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(
				err,
				map[string]interface{}{
					"connectorId": connectorId,
					"policy":      "retry",
				},
			)
		}
		policies["retry"] = plc
	}

	if fsCfg.CircuitBreaker != nil {
		plc, err := createCacheCircuitBreakerPolicy(logger, connectorId, fsCfg.CircuitBreaker)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(
				err,
				map[string]interface{}{
					"connectorId": connectorId,
					"policy":      "circuitBreaker",
				},
			)
		}
		policies["circuitBreaker"] = plc
	}

	if fsCfg.Hedge != nil && fsCfg.Hedge.MaxCount > 0 {
		plc, err := createCacheHedgePolicy(logger, fsCfg.Hedge)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(
				err,
				map[string]interface{}{
					"connectorId": connectorId,
					"policy":      "hedge",
				},
			)
		}
		policies["hedge"] = plc
	}

	return policies, nil
}

func toCachePolicyArray(policies map[string]failsafe.Policy[[]byte]) []failsafe.Policy[[]byte] {
	order := []string{"retry", "circuitBreaker", "hedge", "timeout"}
	pls := make([]failsafe.Policy[[]byte], 0, len(policies))
	for _, name := range order {
		if p, ok := policies[name]; ok {
			pls = append(pls, p)
		}
	}
	return pls
}

// getFailsafeExecutor selects the best-matching executor using the same 4-tier
// priority as upstream.getFailsafeExecutor: method+finality → method → finality → default.
func getFailsafeExecutor(executors []*CacheFailsafeExecutor, ctx context.Context) *CacheFailsafeExecutor {
	var method string
	var finality common.DataFinalityState

	if r := ctx.Value(common.RequestContextKey); r != nil {
		if req, ok := r.(*common.NormalizedRequest); ok && req != nil {
			method, _ = req.Method()
			finality = req.Finality(ctx)
		}
	}

	// 1. Match both method AND finality
	for _, fe := range executors {
		if fe.method != "*" && len(fe.finalities) > 0 {
			matched, _ := common.WildcardMatch(fe.method, method)
			if matched && slices.Contains(fe.finalities, finality) {
				return fe
			}
		}
	}

	// 2. Match method only (empty finalities = any finality)
	for _, fe := range executors {
		if fe.method != "*" && len(fe.finalities) == 0 {
			matched, _ := common.WildcardMatch(fe.method, method)
			if matched {
				return fe
			}
		}
	}

	// 3. Match finality only (method = "*")
	for _, fe := range executors {
		if fe.method == "*" && len(fe.finalities) > 0 {
			if slices.Contains(fe.finalities, finality) {
				return fe
			}
		}
	}

	// 4. Default (method = "*", finalities = nil)
	for _, fe := range executors {
		if fe.method == "*" && len(fe.finalities) == 0 {
			return fe
		}
	}

	return nil
}

// ----- Connector interface implementation -----

func (f *FailsafeConnector) Id() string {
	return f.wrapped.Id()
}

func (f *FailsafeConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, metadata interface{}) ([]byte, error) {
	fe := getFailsafeExecutor(f.getExecutors, ctx)
	if fe == nil {
		return f.wrapped.Get(ctx, index, partitionKey, rangeKey, metadata)
	}
	result, err := fe.executor.WithContext(ctx).GetWithExecution(
		func(exec failsafe.Execution[[]byte]) ([]byte, error) {
			return f.wrapped.Get(exec.Context(), index, partitionKey, rangeKey, metadata)
		},
	)
	if err != nil {
		return nil, TranslateCacheFailsafeError(f.wrapped.Id(), err)
	}
	return result, nil
}

func (f *FailsafeConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	fe := getFailsafeExecutor(f.setExecutors, ctx)
	if fe == nil {
		return f.wrapped.Set(ctx, partitionKey, rangeKey, value, ttl)
	}
	_, err := fe.executor.WithContext(ctx).GetWithExecution(
		func(exec failsafe.Execution[[]byte]) ([]byte, error) {
			return nil, f.wrapped.Set(exec.Context(), partitionKey, rangeKey, value, ttl)
		},
	)
	if err != nil {
		return TranslateCacheFailsafeError(f.wrapped.Id(), err)
	}
	return nil
}

func (f *FailsafeConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	fe := getFailsafeExecutor(f.setExecutors, ctx)
	if fe == nil {
		return f.wrapped.Delete(ctx, partitionKey, rangeKey)
	}
	_, err := fe.executor.WithContext(ctx).GetWithExecution(
		func(exec failsafe.Execution[[]byte]) ([]byte, error) {
			return nil, f.wrapped.Delete(exec.Context(), partitionKey, rangeKey)
		},
	)
	if err != nil {
		return TranslateCacheFailsafeError(f.wrapped.Id(), err)
	}
	return nil
}

func (f *FailsafeConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	return f.wrapped.List(ctx, index, limit, paginationToken)
}

func (f *FailsafeConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	return f.wrapped.Lock(ctx, key, ttl)
}

func (f *FailsafeConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error) {
	return f.wrapped.WatchCounterInt64(ctx, key)
}

func (f *FailsafeConnector) PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error {
	return f.wrapped.PublishCounterInt64(ctx, key, value)
}

// ----- Policy creators -----

func createCacheTimeoutPolicy(logger *zerolog.Logger, cfg *common.TimeoutPolicyConfig) (failsafe.Policy[[]byte], error) {
	builder := timeout.Builder[[]byte](cfg.Duration.Duration())

	if logger.GetLevel() == zerolog.TraceLevel {
		builder.OnTimeoutExceeded(func(event failsafe.ExecutionDoneEvent[[]byte]) {
			logger.Trace().Msgf(
				"cache failsafe timeout exceeded (start: %v, elapsed: %v, attempts: %d, retries: %d, hedges: %d)",
				event.StartTime().Format(time.RFC3339), event.ElapsedTime(), event.Attempts(), event.Retries(), event.Hedges(),
			)
		})
	}

	return builder.Build(), nil
}

func createCacheRetryPolicy(logger *zerolog.Logger, connectorId string, cfg *common.RetryPolicyConfig) (failsafe.Policy[[]byte], error) {
	builder := retrypolicy.Builder[[]byte]()

	if cfg.MaxAttempts > 0 {
		builder = builder.WithMaxAttempts(cfg.MaxAttempts)
	}
	if cfg.Delay > 0 {
		delayDuration := cfg.Delay.Duration()
		if cfg.BackoffMaxDelay > 0 {
			backoffMaxDuration := cfg.BackoffMaxDelay.Duration()
			if cfg.BackoffFactor > 0 {
				builder = builder.WithBackoffFactor(delayDuration, backoffMaxDuration, cfg.BackoffFactor)
			} else {
				builder = builder.WithBackoff(delayDuration, backoffMaxDuration)
			}
		} else {
			builder = builder.WithDelay(delayDuration)
		}
	}
	if cfg.Jitter > 0 {
		builder = builder.WithJitter(cfg.Jitter.Duration())
	}

	builder = builder.HandleIf(func(exec failsafe.ExecutionAttempt[[]byte], result []byte, err error) bool {
		if err == nil {
			return false
		}
		// Cache miss / expired are expected, not retriable
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound, common.ErrCodeRecordExpired) {
			return false
		}
		// Context cancellation is caller-initiated
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		// Retry all other errors (connection failures, server errors, etc.)
		return true
	})

	builder = builder.OnRetryScheduled(func(event failsafe.ExecutionScheduledEvent[[]byte]) {
		if logger.GetLevel() <= zerolog.DebugLevel {
			logger.Debug().
				Str("connectorId", connectorId).
				Int64("scheduledDelayMs", event.Delay.Milliseconds()).
				Int("attempts", event.Attempts()).
				Int("retries", event.Retries()).
				Msg("cache failsafe retry scheduled")
		}
	})

	return builder.Build(), nil
}

func createCacheCircuitBreakerPolicy(logger *zerolog.Logger, connectorId string, cfg *common.CircuitBreakerPolicyConfig) (failsafe.Policy[[]byte], error) {
	builder := circuitbreaker.Builder[[]byte]()

	if cfg.FailureThresholdCount > 0 {
		if cfg.FailureThresholdCapacity > 0 {
			builder = builder.WithFailureThresholdRatio(cfg.FailureThresholdCount, cfg.FailureThresholdCapacity)
		} else {
			builder = builder.WithFailureThreshold(cfg.FailureThresholdCount)
		}
	}

	if cfg.SuccessThresholdCount > 0 {
		if cfg.SuccessThresholdCapacity > 0 {
			builder = builder.WithSuccessThresholdRatio(cfg.SuccessThresholdCount, cfg.SuccessThresholdCapacity)
		} else {
			builder = builder.WithSuccessThreshold(cfg.SuccessThresholdCount)
		}
	}

	if cfg.HalfOpenAfter > 0 {
		builder = builder.WithDelay(cfg.HalfOpenAfter.Duration())
	}

	builder.OnStateChanged(func(event circuitbreaker.StateChangedEvent) {
		mt := event.Metrics()
		logger.Warn().
			Str("connectorId", connectorId).
			Uint("executions", mt.Executions()).
			Uint("successes", mt.Successes()).
			Uint("failures", mt.Failures()).
			Uint("failureRate", mt.FailureRate()).
			Uint("successRate", mt.SuccessRate()).
			Msgf("cache circuit breaker state changed from %s to %s", event.OldState, event.NewState)
	})

	builder.HandleIf(func(exec failsafe.ExecutionAttempt[[]byte], result []byte, err error) bool {
		if err == nil {
			return false
		}
		// Cache miss / expired are normal, not failures
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound, common.ErrCodeRecordExpired) {
			return false
		}
		// Context cancellation is client-side
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		// All other errors count as failures
		return true
	})

	return builder.Build(), nil
}

func createCacheHedgePolicy(logger *zerolog.Logger, cfg *common.HedgePolicyConfig) (failsafe.Policy[[]byte], error) {
	delay := cfg.Delay.Duration()
	builder := hedgepolicy.BuilderWithDelay[[]byte](delay)

	if cfg.MaxCount > 0 {
		builder = builder.WithMaxHedges(cfg.MaxCount)
	}

	return builder.Build(), nil
}

// TranslateCacheFailsafeError maps failsafe-go error types to eRPC standard errors.
func TranslateCacheFailsafeError(connectorId string, execErr error) error {
	if serr, ok := execErr.(common.StandardError); ok {
		return serr
	}

	var retryExceededErr retrypolicy.ExceededError
	if errors.As(execErr, &retryExceededErr) {
		ler := retryExceededErr.LastError
		if common.IsNull(ler) {
			if lexr, ok := execErr.(common.StandardError); ok {
				ler = lexr.GetCause()
			}
		}
		var translatedCause error
		if ler != nil {
			translatedCause = TranslateCacheFailsafeError(connectorId, ler)
		}
		return common.NewErrFailsafeRetryExceeded(scopeConnector, translatedCause, nil)
	}

	if errors.Is(execErr, timeout.ErrExceeded) {
		return common.NewErrFailsafeTimeoutExceeded(scopeConnector, execErr, nil)
	}

	if errors.Is(execErr, circuitbreaker.ErrOpen) {
		return common.NewErrFailsafeCircuitBreakerOpen(scopeConnector, execErr, nil)
	}

	// Unwrap joined errors from hedge
	if joinedErr, ok := execErr.(interface{ Unwrap() []error }); ok {
		errs := joinedErr.Unwrap()
		if len(errs) == 1 {
			return errs[0]
		} else if len(errs) > 1 {
			// All hedge attempts failed — return the first translated error
			return TranslateCacheFailsafeError(connectorId, errs[0])
		}
	}

	return execErr
}
