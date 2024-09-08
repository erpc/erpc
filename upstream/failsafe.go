package upstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/hedgepolicy"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/failsafe-go/failsafe-go/timeout"
	"github.com/rs/zerolog"
)

type Scope string

const (
	// Policies must be created with a "network" in mind,
	// assuming there will be many upstreams e.g. Retry might endup using a different upstream
	ScopeNetwork Scope = "network"

	// Policies must be created with one only "upstream" in mind
	// e.g. Retry with be towards the same upstream
	ScopeUpstream Scope = "upstream"
)

func CreateFailSafePolicies(logger *zerolog.Logger, scope Scope, component string, fsCfg *common.FailsafeConfig) ([]failsafe.Policy[*common.NormalizedResponse], error) {
	// The order of policies below are important as per docs of failsafe-go
	var policies = []failsafe.Policy[*common.NormalizedResponse]{}

	if fsCfg == nil {
		return policies, nil
	}

	// For network-level we want the timeout to apply to the overall lifecycle
	if fsCfg.Timeout != nil && scope == ScopeNetwork {
		var err error
		timeoutPolicy, err := createTimeoutPolicy(component, fsCfg.Timeout)
		if err != nil {
			return nil, err
		}

		policies = append(policies, timeoutPolicy)
	}

	if fsCfg.Retry != nil {
		p, err := createRetryPolicy(scope, component, fsCfg.Retry)
		if err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}

	// CircuitBreaker does not make sense for network-level requests
	if scope == ScopeUpstream {
		if fsCfg.CircuitBreaker != nil {
			p, err := createCircuitBreakerPolicy(logger, component, fsCfg.CircuitBreaker)
			if err != nil {
				return nil, err
			}
			policies = append(policies, p)
		}
	}

	if fsCfg.Hedge != nil {
		p, err := createHedgePolicy(component, fsCfg.Hedge)
		if err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}

	// For upstream-level we want the timeout to apply to each individual request towards upstream
	if fsCfg.Timeout != nil && scope == ScopeUpstream {
		var err error
		timeoutPolicy, err := createTimeoutPolicy(component, fsCfg.Timeout)
		if err != nil {
			return nil, err
		}

		policies = append(policies, timeoutPolicy)
	}

	return policies, nil
}

func createCircuitBreakerPolicy(logger *zerolog.Logger, component string, cfg *common.CircuitBreakerPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	builder := circuitbreaker.Builder[*common.NormalizedResponse]()

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

	if cfg.HalfOpenAfter != "" {
		dur, err := time.ParseDuration(cfg.HalfOpenAfter)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse circuitBreaker.halfOpenAfter: %v", err), map[string]interface{}{
				"component": component,
				"policy":    cfg,
			})
		}

		builder = builder.WithDelay(dur)
	}

	builder.OnStateChanged(func(event circuitbreaker.StateChangedEvent) {
		mt := event.Metrics()
		logger.Warn().
			Uint("executions", mt.Executions()).
			Uint("successes", mt.Successes()).
			Uint("failures", mt.Failures()).
			Uint("failureRate", mt.FailureRate()).
			Uint("successRate", mt.SuccessRate()).
			Msgf("circuit breaker state changed from %s to %s", event.OldState, event.NewState)
	})
	builder.OnFailure(func(event failsafe.ExecutionEvent[*common.NormalizedResponse]) {
		err := event.LastError()
		res := event.LastResult()
		lg := logger.Warn().Err(err).Interface("response", res)
		if res != nil && !res.IsObjectNull() {
			rq := res.Request()
			if rq != nil {
				lg = lg.Object("request", rq)
				up := rq.LastUpstream()
				if up != nil {
					lg = lg.Str("upstreamId", up.Config().Id)
					cfg := up.Config()
					if cfg.Evm != nil {
						lg = lg.Interface("upstreamSyncingState", cfg.Evm.Syncing)
					}
				}
			}
		}
		lg.Msg("failure caught that will be considered for circuit breaker")
	})

	builder.HandleIf(func(result *common.NormalizedResponse, err error) bool {
		// 5xx or other non-retryable server-side errors -> open the circuit
		if common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
			return true
		}

		// 401 / 403 / RPC-RPC vendor auth -> open the circuit
		if common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized) {
			return true
		}

		// remote vendor billing issue -> open the circuit
		if common.HasErrorCode(err, common.ErrCodeEndpointBillingIssue) {
			return true
		}

		if result != nil && result.Request() != nil {
			up := result.Request().LastUpstream()

			// if "syncing" and null/empty response -> open the circuit
			cfg := up.Config()
			if cfg.Evm != nil {
				if cfg.Evm.Syncing != nil && *cfg.Evm.Syncing {
					if result.IsResultEmptyish() {
						return true
					}
				}
			}
		}

		// other errors must not open the circuit because it does not mean that the remote service is "bad"
		return false
	})

	return builder.Build(), nil
}

func createHedgePolicy(component string, cfg *common.HedgePolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	delay, err := time.ParseDuration(cfg.Delay)
	if err != nil {
		return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse hedge.delay: %v", err), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}
	builder := hedgepolicy.BuilderWithDelay[*common.NormalizedResponse](delay)

	if cfg.MaxCount > 0 {
		builder = builder.WithMaxHedges(cfg.MaxCount)
	}

	return builder.Build(), nil
}

func createRetryPolicy(scope Scope, component string, cfg *common.RetryPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	builder := retrypolicy.Builder[*common.NormalizedResponse]()

	if cfg.MaxAttempts > 0 {
		builder = builder.WithMaxAttempts(cfg.MaxAttempts)
	}
	if cfg.Delay != "" {
		delayDuration, err := time.ParseDuration(cfg.Delay)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.delay: %v", err), map[string]interface{}{
				"component": component,
				"policy":    cfg,
			})
		}

		if cfg.BackoffMaxDelay != "" {
			backoffMaxDuration, err := time.ParseDuration(cfg.BackoffMaxDelay)
			if err != nil {
				return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.backoffMaxDelay: %v", err), map[string]interface{}{
					"component": component,
					"policy":    cfg,
				})
			}

			if cfg.BackoffFactor > 0 {
				builder = builder.WithBackoffFactor(delayDuration, backoffMaxDuration, cfg.BackoffFactor)
			} else {
				builder = builder.WithBackoff(delayDuration, backoffMaxDuration)
			}
		} else {
			builder = builder.WithDelay(delayDuration)
		}
	}
	if cfg.Jitter != "" {
		jitterDuration, err := time.ParseDuration(cfg.Jitter)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.jitter: %v", err), map[string]interface{}{
				"component": component,
				"policy":    cfg,
			})
		}

		builder = builder.WithJitter(jitterDuration)
	}

	builder.HandleIf(func(result *common.NormalizedResponse, err error) bool {
		// 400 / 404 / 405 / 413 -> No Retry
		// RPC-RPC client-side error (invalid params) -> No Retry
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
			return false
		}

		// Any error that cannot be retried against an upstream
		if scope == ScopeUpstream {
			if !common.IsRetryableTowardsUpstream(err) || common.IsCapacityIssue(err) {
				return false
			}
		}

		// When error is "missing data" retry on network-level
		if scope == ScopeNetwork && common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			return true
		}

		// On network-level if all upstreams returned non-retryable errors then do not retry
		if scope == ScopeNetwork && common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted) {
			exher, ok := err.(*common.ErrUpstreamsExhausted)
			if ok {
				errs := exher.Errors()
				if len(errs) > 0 {
					shouldRetry := false
					for _, err := range errs {
						if common.IsRetryableTowardsUpstream(err) && !common.IsCapacityIssue(err) {
							shouldRetry = true
							break
						}
					}
					return shouldRetry
				}
			}
		}

		if scope == ScopeNetwork && result != nil && !result.IsObjectNull() {
			req := result.Request()
			rds := req.Directives()

			// Retry empty responses on network-level to give a chance for another upstream to
			// try fetching the data as the current upstream is less likely to have the data ready on the next retry attempt.
			if rds.RetryEmpty {
				isEmpty := result.IsResultEmptyish()
				// no Retry-Empty directive + "empty" response -> No Retry
				if !rds.RetryEmpty && isEmpty {
					return false
				}
				ups := result.Upstream()
				ucfg := ups.Config()
				if ucfg.Evm != nil {
					// has Retry-Empty directive + "empty" response + node is synced + block is finalized -> No Retry
					if err == nil && rds.RetryEmpty && isEmpty && (ucfg.Evm.Syncing != nil && !*ucfg.Evm.Syncing) {
						bn, ebn := req.EvmBlockNumber()
						if ebn == nil && bn > 0 {
							// TODO Should only use "ups"'s state_poller vs all upstreams?
							fin, efin := req.Network().EvmIsBlockFinalized(bn)
							if efin == nil && fin {
								return false
							}
						}
					}
				}
				if isEmpty {
					return true
				}
			}

			// For pending transactions retry on network-level to give a chance of receiving
			// the full TX data when it is available.
			if rds.RetryPending {
				method, _ := result.Request().Method()
				switch method {
				case "eth_getTransactionReceipt",
					"eth_getTransactionByHash",
					"eth_getTransactionByBlockHashAndIndex",
					"eth_getTransactionByBlockNumberAndIndex":
					blkNum, err := result.EvmBlockNumber()
					if err == nil {
						if blkNum == 0 {
							return true
						}
					}
				}
			}
		}

		// 5xx -> Retry
		return err != nil
	})

	return builder.Build(), nil
}

func createTimeoutPolicy(component string, cfg *common.TimeoutPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	if cfg.Duration == "" {
		return nil, common.NewErrFailsafeConfiguration(errors.New("missing timeout"), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}

	timeoutDuration, err := time.ParseDuration(cfg.Duration)
	builder := timeout.Builder[*common.NormalizedResponse](timeoutDuration)

	if err != nil {
		return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse timeout: %v", err), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}

	return builder.Build(), nil
}

func TranslateFailsafeError(upstreamId, method string, execErr error) error {
	var err error
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
			translatedCause = TranslateFailsafeError("", "", ler)
		}
		err = common.NewErrFailsafeRetryExceeded(translatedCause)
	} else if errors.Is(execErr, timeout.ErrExceeded) ||
		errors.Is(execErr, context.DeadlineExceeded) {
		err = common.NewErrFailsafeTimeoutExceeded(execErr)
	} else if errors.Is(execErr, circuitbreaker.ErrOpen) {
		err = common.NewErrFailsafeCircuitBreakerOpen(execErr)
	}

	if err != nil {
		if method != "" {
			if ser, ok := execErr.(common.StandardError); ok {
				be := ser.Base()
				if be != nil {
					if upstreamId != "" {
						be.Details = map[string]interface{}{
							"upstreamId": upstreamId,
							"method":     method,
						}
					} else {
						be.Details = map[string]interface{}{
							"method": method,
						}
					}
				}
			}
		}
		return err
	}

	return execErr
}
