package upstream

import (
	"errors"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/hedgepolicy"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/failsafe-go/failsafe-go/timeout"
	"github.com/rs/zerolog/log"
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

func CreateFailSafePolicies(scope Scope, component string, fsCfg *common.FailsafeConfig) ([]failsafe.Policy[common.NormalizedResponse], error) {
	// The order of policies below are important as per docs of failsafe-go
	var policies = []failsafe.Policy[common.NormalizedResponse]{}

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
			p, err := createCircuitBreakerPolicy(component, fsCfg.CircuitBreaker)
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

func createCircuitBreakerPolicy(component string, cfg *common.CircuitBreakerPolicyConfig) (failsafe.Policy[common.NormalizedResponse], error) {
	builder := circuitbreaker.Builder[common.NormalizedResponse]()

	if cfg.FailureThresholdCount > 0 {
		if cfg.FailureThresholdCapacity > 0 {
			builder = builder.WithFailureThresholdRatio(uint(cfg.FailureThresholdCount), uint(cfg.FailureThresholdCapacity))
		} else {
			builder = builder.WithFailureThreshold(uint(cfg.FailureThresholdCount))
		}
	}

	if cfg.SuccessThresholdCount > 0 {
		if cfg.SuccessThresholdCapacity > 0 {
			builder = builder.WithSuccessThresholdRatio(uint(cfg.SuccessThresholdCount), uint(cfg.SuccessThresholdCapacity))
		} else {
			builder = builder.WithSuccessThreshold(uint(cfg.SuccessThresholdCount))
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

	builder.OnHalfOpen(func(event circuitbreaker.StateChangedEvent) {
		log.Warn().Msgf("circuitBreaker half open: %v", event)
	})
	builder.OnOpen(func(event circuitbreaker.StateChangedEvent) {
		log.Warn().Msgf("circuitBreaker open: %v", event)
	})
	builder.OnClose(func(event circuitbreaker.StateChangedEvent) {
		log.Debug().Msgf("circuitBreaker close: %v", event)
	})

	builder.HandleIf(func(result common.NormalizedResponse, err error) bool {
		// 5xx or other non-retryable server-side errors -> open the circuit
		if common.HasCode(err, common.ErrCodeEndpointServerSideException) {
			return true
		}

		// 401 / 403 / RPC-RPC vendor auth -> open the circuit
		if common.HasCode(err, common.ErrCodeEndpointUnauthorized) {
			return true
		}

		// remote vendor capacity exceeded -> open the circuit
		if common.HasCode(err, common.ErrCodeEndpointCapacityExceeded) {
			return true
		}

		// remote vendor billing issue -> open the circuit
		if common.HasCode(err, common.ErrCodeEndpointBillingIssue) {
			return true
		}

		if result != nil && result.Request() != nil {
			up := result.Request().LastUpstream()

			// if "syncing" and null/empty response -> open the circuit
			cfg := up.Config()
			if cfg.Evm != nil {
				if cfg.Evm.Syncing {
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

func createHedgePolicy(component string, cfg *common.HedgePolicyConfig) (failsafe.Policy[common.NormalizedResponse], error) {
	delay, err := time.ParseDuration(cfg.Delay)
	if err != nil {
		return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse hedge.delay: %v", err), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}
	builder := hedgepolicy.BuilderWithDelay[common.NormalizedResponse](delay)

	if cfg.MaxCount > 0 {
		builder = builder.WithMaxHedges(cfg.MaxCount)
	}

	return builder.Build(), nil
}

func createRetryPolicy(scope Scope, component string, cfg *common.RetryPolicyConfig) (failsafe.Policy[common.NormalizedResponse], error) {
	builder := retrypolicy.Builder[common.NormalizedResponse]()

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

	builder.HandleIf(func(result common.NormalizedResponse, err error) bool {
		// 400 / 404 / 405 / 413 -> No Retry
		// RPC-RPC client-side error (invalid params) -> No Retry
		if common.HasCode(err, common.ErrCodeEndpointClientSideException) {
			return false
		}

		// Upstream-level + 401 / 403 -> No Retry
		// RPC-RPC vendor billing/capacity/auth -> No Retry
		if scope == ScopeUpstream && common.HasCode(err, common.ErrCodeEndpointUnauthorized) {
			return false
		}

		// Unsupported features and methods
		if scope == ScopeUpstream && common.HasCode(err, common.ErrCodeEndpointUnsupported) {
			return false
		}

		// Do not try when 3rd-party providers run out of monthly capacity
		if scope == ScopeUpstream && common.HasCode(err, common.ErrCodeEndpointCapacityExceeded) {
			return false
		}

		// Do not try when upstream returned ErrUpstreamRequestSkipped
		if scope == ScopeUpstream && common.HasCode(err, common.ErrCodeUpstreamRequestSkipped) {
			return false
		}

		// if all upstreams returned ErrUpstreamRequestSkipped then do not retry
		if scope == ScopeNetwork && common.HasCode(err, common.ErrCodeUpstreamsExhausted) {
			exher, ok := err.(*common.ErrUpstreamsExhausted)
			if ok {
				errs := exher.Errors()
				if len(errs) > 0 {
					shouldRetry := false
					for _, err := range errs {
						if !common.HasCode(err, common.ErrCodeUpstreamRequestSkipped) {
							shouldRetry = true
							break
						}
					}
					return shouldRetry
				}
			}
		}

		// Retry empty responses on network-level to give a chance for another upstream to
		// try fetching the data as the current upstream is less likely to have the data ready on the next retry attempt.
		if scope == ScopeNetwork {
			if result != nil && !result.IsObjectNull() {
				req := result.Request()
				isEmpty := result.IsResultEmptyish()

				// no Retry-Empty directive + "empty" response -> No Retry
				rds := req.Directives()
				if !rds.RetryEmpty && isEmpty {
					return false
				}

				ucfg := req.LastUpstream().Config()
				if ucfg.Evm != nil {
					// Retry-Empty directive + "empty" response + block is finalized -> No Retry
					if err == nil && rds.RetryEmpty && isEmpty {
						bn, ebn := req.EvmBlockNumber()
						if ebn == nil {
							fin, efin := req.Network().EvmIsBlockFinalized(bn)
							if efin == nil && fin {
								return false
							}
						}
					}
				}
				if rds.RetryEmpty && isEmpty {
					return true
				}
			}
		}

		// X-Empty directive + "empty" response + block is unfinalized -> Retry
		// X-ERPC-Retry-Empty directive + null response + block is unfinalized -> Retry
		// 429 / 408 -> Retry
		// 5xx -> Retry
		return err != nil
	})

	return builder.Build(), nil
}

func createTimeoutPolicy(component string, cfg *common.TimeoutPolicyConfig) (failsafe.Policy[common.NormalizedResponse], error) {
	if cfg.Duration == "" {
		return nil, common.NewErrFailsafeConfiguration(errors.New("missing timeout"), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}

	timeoutDuration, err := time.ParseDuration(cfg.Duration)
	builder := timeout.Builder[common.NormalizedResponse](timeoutDuration)

	if err != nil {
		return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse timeout: %v", err), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}

	return builder.Build(), nil
}

func TranslateFailsafeError(exec failsafe.Execution[common.NormalizedResponse], execErr error) error {
	var retryExceededErr *retrypolicy.ExceededError
	if errors.As(execErr, &retryExceededErr) {
		var attempts int
		var retries int
		if exec != nil {
			attempts = exec.Attempts()
			retries = exec.Retries()
		}
		ler := retryExceededErr.LastError()
		if common.IsNull(ler) {
			if lexr, ok := execErr.(common.StandardError); ok {
				ler = lexr.GetCause()
			}
		}
		return common.NewErrFailsafeRetryExceeded(
			ler,
			retryExceededErr.LastResult(),
			attempts,
			retries,
		)
	}

	if errors.Is(execErr, timeout.ErrExceeded) {
		return common.NewErrFailsafeTimeoutExceeded(execErr)
	}

	if errors.Is(execErr, circuitbreaker.ErrOpen) {
		return common.NewErrFailsafeCircuitBreakerOpen(execErr)
	}

	return execErr
}
