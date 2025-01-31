package upstream

import (
	"errors"
	"fmt"
	"time"

	"github.com/erpc/erpc/arch/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/consensus"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/hedgepolicy"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/failsafe-go/failsafe-go/timeout"
	"github.com/rs/zerolog"
)

func CreateFailSafePolicies(logger *zerolog.Logger, scope common.Scope, entity string, fsCfg *common.FailsafeConfig) (map[string]failsafe.Policy[*common.NormalizedResponse], error) {
	// The order of policies below are important as per docs of failsafe-go
	var policies = map[string]failsafe.Policy[*common.NormalizedResponse]{}

	if fsCfg == nil {
		return policies, nil
	}

	lg := logger.With().Str("scope", string(scope)).Str("entity", entity).Logger()

	if fsCfg.Timeout != nil {
		var err error
		p, err := createTimeoutPolicy(&lg, entity, fsCfg.Timeout)
		if err != nil {
			return nil, err
		}

		policies["timeout"] = p
	}

	if fsCfg.Retry != nil {
		p, err := createRetryPolicy(scope, entity, fsCfg.Retry)
		if err != nil {
			return nil, err
		}
		policies["retry"] = p
	}

	if fsCfg.CircuitBreaker != nil {
		// CircuitBreaker does not make sense for network-level requests
		if scope != common.ScopeUpstream {
			return nil, common.NewErrFailsafeConfiguration(
				errors.New("circuit breaker does not make sense for network-level requests"),
				map[string]interface{}{
					"entity": entity,
					"policy": fsCfg.CircuitBreaker,
				},
			)
		}
		p, err := createCircuitBreakerPolicy(&lg, entity, fsCfg.CircuitBreaker)
		if err != nil {
			return nil, err
		}
		policies["circuitBreaker"] = p
	}

	if fsCfg.Hedge != nil {
		p, err := createHedgePolicy(&lg, entity, fsCfg.Hedge)
		if err != nil {
			return nil, err
		}
		policies["hedge"] = p
	}

	if fsCfg.Consensus != nil {
		if scope != common.ScopeNetwork {
			return nil, common.NewErrFailsafeConfiguration(
				errors.New("consensus does not make sense for upstream-level requests"),
				map[string]interface{}{
					"entity": entity,
					"policy": fsCfg.Consensus,
				},
			)
		}
		p, err := createConsensusPolicy(&lg, entity, fsCfg.Consensus)
		if err != nil {
			return nil, err
		}
		policies["consensus"] = p
	}

	return policies, nil
}

func ToPolicyArray(policies map[string]failsafe.Policy[*common.NormalizedResponse], preferredOrder ...string) []failsafe.Policy[*common.NormalizedResponse] {
	pls := make([]failsafe.Policy[*common.NormalizedResponse], 0, len(policies))

	for _, policy := range preferredOrder {
		if p, ok := policies[policy]; ok {
			pls = append(pls, p)
		}
	}

	for _, p := range policies {
		pls = append(pls, p)
	}

	return pls
}

func createCircuitBreakerPolicy(logger *zerolog.Logger, entity string, cfg *common.CircuitBreakerPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
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
				"entity": entity,
				"policy": cfg,
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
		if logger.GetLevel() <= zerolog.DebugLevel {
			lg := logger.Debug().Err(err).Object("response", res)
			if res != nil && !res.IsObjectNull() {
				rq := res.Request()
				if rq != nil {
					lg = lg.Object("request", rq)
					up := rq.LastUpstream()
					if up != nil {
						lg = lg.Str("upstreamId", up.Config().Id)
						cfg := up.Config()
						if cfg.Evm != nil {
							lg = lg.Interface("upstreamSyncingState", up.EvmSyncingState())
						}
					}
				}
			}
			lg.Msg("failure caught that will be considered for circuit breaker")
		}
		// TODO emit a custom prometheus metric to track CB root causes?
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
			if up.EvmSyncingState() == common.EvmSyncingStateSyncing {
				if result.IsResultEmptyish() {
					return true
				}
			}
		}

		// other errors must not open the circuit because it does not mean that the remote service is "bad"
		return false
	})

	return builder.Build(), nil
}

func createHedgePolicy(logger *zerolog.Logger, entity string, cfg *common.HedgePolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	var builder hedgepolicy.HedgePolicyBuilder[*common.NormalizedResponse]

	delay, err := time.ParseDuration(cfg.Delay)
	if err != nil {
		return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse hedge.delay: %v", err), map[string]interface{}{
			"entity": entity,
			"policy": cfg,
		})
	}

	if cfg.Quantile > 0 {
		minDelay, err := time.ParseDuration(cfg.MinDelay)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse hedge.minDelay: %v", err), map[string]interface{}{
				"entity": entity,
				"policy": cfg,
			})
		}
		maxDelay, err := time.ParseDuration(cfg.MaxDelay)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse hedge.maxDelay: %v", err), map[string]interface{}{
				"entity": entity,
				"policy": cfg,
			})
		}
		builder = hedgepolicy.BuilderWithDelayFunc(func(exec failsafe.ExecutionAttempt[*common.NormalizedResponse]) time.Duration {
			ctx := exec.Context()
			if ctx != nil {
				req := ctx.Value(common.RequestContextKey)
				if req != nil {
					if req, ok := req.(*common.NormalizedRequest); ok {
						ntw := req.Network()
						if ntw != nil {
							m, _ := req.Method()
							if m != "" {
								mt := ntw.GetMethodMetrics(m)
								if mt != nil {
									qt := mt.GetResponseQuantiles()
									dr := qt.GetQuantile(cfg.Quantile)
									// When quantile is specified, we add the delay to the quantile value,
									// and then clamp the value between minDelay and maxDelay.
									dr += delay
									if dr < minDelay {
										dr = minDelay
									}
									if dr > maxDelay {
										dr = maxDelay
									}
									logger.Trace().Object("request", req).Dur("delay", dr).Msgf("calculated hedge delay")
									return dr
								}
							}
						}
					}
				}
			}
			return delay
		})
	} else {
		builder = hedgepolicy.BuilderWithDelay[*common.NormalizedResponse](delay)
	}

	if cfg.MaxCount > 0 {
		builder = builder.WithMaxHedges(cfg.MaxCount)
	}

	builder.OnHedge(func(event failsafe.ExecutionEvent[*common.NormalizedResponse]) bool {
		var req *common.NormalizedRequest
		var method string
		r := event.Context().Value(common.RequestContextKey)
		if r != nil {
			var ok bool
			req, ok = r.(*common.NormalizedRequest)
			if ok && req != nil {
				method, _ = req.Method()
				if method != "" && common.IsEvmWriteMethod(method) {
					logger.Debug().Str("method", method).Interface("id", req.ID()).Msgf("ignoring hedge for write request")
					return false
				}
			}
		}

		logger.Trace().Str("method", method).Interface("id", req.ID()).Msgf("attempting to hedge request")

		// Continue with the next hedge
		return true
	})

	return builder.Build(), nil
}

func createRetryPolicy(scope common.Scope, entity string, cfg *common.RetryPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	builder := retrypolicy.Builder[*common.NormalizedResponse]()

	if cfg.MaxAttempts > 0 {
		builder = builder.WithMaxAttempts(cfg.MaxAttempts)
	}
	if cfg.Delay != "" {
		delayDuration, err := time.ParseDuration(cfg.Delay)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.delay: %v", err), map[string]interface{}{
				"entity": entity,
				"policy": cfg,
			})
		}

		if cfg.BackoffMaxDelay != "" {
			backoffMaxDuration, err := time.ParseDuration(cfg.BackoffMaxDelay)
			if err != nil {
				return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.backoffMaxDelay: %v", err), map[string]interface{}{
					"entity": entity,
					"policy": cfg,
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
				"entity": entity,
				"policy": cfg,
			})
		}

		if jitterDuration > 0 {
			builder = builder.WithJitter(jitterDuration)
		}
	}

	builder.HandleIf(func(result *common.NormalizedResponse, err error) bool {
		// 400 / 404 / 405 / 413 -> No Retry
		// RPC-RPC client-side error (invalid params) -> No Retry
		if common.IsClientError(err) {
			return false
		}

		// Any error that cannot be retried against an upstream
		if scope == common.ScopeUpstream {
			if !common.IsRetryableTowardsUpstream(err) || common.IsCapacityIssue(err) {
				return false
			}
		}

		// When error is "missing data" retry on network-level
		if scope == common.ScopeNetwork && common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			return true
		}

		// On network-level if all upstreams returned non-retryable errors then do not retry
		if scope == common.ScopeNetwork && common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted) {
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

		if scope == common.ScopeNetwork && result != nil && !result.IsObjectNull() {
			req := result.Request()
			rds := req.Directives()

			// Retry empty responses on network-level to give a chance for another upstream to
			// try fetching the data as the current upstream is less likely to have the data ready on the next retry attempt.
			if rds.RetryEmpty {
				isEmpty := result.IsResultEmptyish()
				if isEmpty {
					// no Retry-Empty directive + "empty" response -> No Retry
					if !rds.RetryEmpty {
						return false
					}
					ups := result.Upstream()
					// has Retry-Empty directive + "empty" response + node is synced + block is finalized -> No Retry
					if err == nil && rds.RetryEmpty && isEmpty {
						if ups.Config().Type == common.UpstreamTypeEvm {
							if ups.EvmSyncingState() == common.EvmSyncingStateNotSyncing {
								_, bn, ebn := evm.ExtractBlockReferenceFromRequest(req)
								if ebn == nil && bn > 0 {
									if isFinalized, err := ups.EvmIsBlockFinalized(bn); err == nil && isFinalized {
										return false
									}
								}
							}
						}
					}
					return true
				}
			}

			// For pending transactions retry on network-level to give a chance of receiving
			// the full TX data when it is available.
			if rds.RetryPending {
				req := result.Request()
				if req != nil {
					method, _ := req.Method()
					switch method {
					case "eth_getTransactionReceipt",
						"eth_getTransactionByHash",
						"eth_getTransactionByBlockHashAndIndex",
						"eth_getTransactionByBlockNumberAndIndex":
						_, blkNum, err := evm.ExtractBlockReferenceFromRequest(req)
						if err == nil {
							if blkNum == 0 {
								return true
							}
						}
					}
				}
			}
		}

		// Must not retry any 'write' methods
		if result != nil {
			if req := result.Request(); req != nil {
				if method, _ := req.Method(); method != "" && common.IsEvmWriteMethod(method) {
					return false
				}
			}
		}

		// 5xx -> Retry
		return err != nil
	})

	return builder.Build(), nil
}

func createTimeoutPolicy(logger *zerolog.Logger, entity string, cfg *common.TimeoutPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	if cfg.Duration == "" {
		return nil, common.NewErrFailsafeConfiguration(errors.New("missing timeout"), map[string]interface{}{
			"entity": entity,
			"policy": cfg,
		})
	}

	timeoutDuration, err := time.ParseDuration(cfg.Duration)
	builder := timeout.Builder[*common.NormalizedResponse](timeoutDuration)

	if logger.GetLevel() == zerolog.TraceLevel {
		builder.OnTimeoutExceeded(func(event failsafe.ExecutionDoneEvent[*common.NormalizedResponse]) {
			logger.Trace().Msgf("failsafe timeout policy: %v (start time: %v, elapsed: %v, attempts: %d, retries: %d, hedges: %d)", event.Error, event.StartTime().Format(time.RFC3339), event.ElapsedTime().String(), event.Attempts(), event.Retries(), event.Hedges())
		})
	}

	if err != nil {
		return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse timeout: %v", err), map[string]interface{}{
			"entity": entity,
			"policy": cfg,
		})
	}

	return builder.Build(), nil
}

func createConsensusPolicy(logger *zerolog.Logger, entity string, cfg *common.ConsensusPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	if cfg == nil {
		// No consensus config given, so no policy
		return nil, nil
	}

	builder := consensus.NewConsensusPolicyBuilder[*common.NormalizedResponse]()
	builder = builder.WithRequiredParticipants(cfg.RequiredParticipants)
	builder = builder.WithAgreementThreshold(cfg.AgreementThreshold)
	builder = builder.WithDisputeBehavior(cfg.DisputeBehavior)
	builder = builder.WithPunishMisbehavior(cfg.PunishMisbehavior)
	builder = builder.WithFailureBehavior(cfg.FailureBehavior)
	builder = builder.WithLowParticipantsBehavior(cfg.LowParticipantsBehavior)

	builder.OnAgreement(func(event failsafe.ExecutionEvent[*common.NormalizedResponse]) {
		logger.Debug().
			Str("entity", entity).
			Msg("spawning additional consensus request")
	})

	p := builder.Build()
	return p, nil
}

func TranslateFailsafeError(scope common.Scope, upstreamId string, method string, execErr error, startTime *time.Time) error {
	var err error
	var retryExceededErr retrypolicy.ExceededError
	if common.HasErrorCode(execErr, common.ErrCodeUpstreamsExhausted) {
		err = execErr
	} else if errors.As(execErr, &retryExceededErr) {
		ler := retryExceededErr.LastError
		if common.IsNull(ler) {
			if lexr, ok := execErr.(common.StandardError); ok {
				ler = lexr.GetCause()
			}
		}
		var translatedCause error
		if ler != nil {
			translatedCause = TranslateFailsafeError(scope, "", "", ler, startTime)
		}
		err = common.NewErrFailsafeRetryExceeded(scope, translatedCause, startTime)
	} else if errors.Is(execErr, timeout.ErrExceeded) {
		err = common.NewErrFailsafeTimeoutExceeded(scope, execErr, startTime)
	} else if errors.Is(execErr, circuitbreaker.ErrOpen) {
		err = common.NewErrFailsafeCircuitBreakerOpen(scope, execErr, startTime)
	}

	if err != nil {
		if ser, ok := execErr.(common.StandardError); ok {
			be := ser.Base()
			if be != nil {
				if upstreamId != "" && method != "" {
					be.Details = map[string]interface{}{
						"upstreamId": upstreamId,
						"method":     method,
					}
				} else if method != "" {
					be.Details = map[string]interface{}{
						"method": method,
					}
				} else if upstreamId != "" {
					be.Details = map[string]interface{}{
						"upstreamId": upstreamId,
					}
				}
			}
		}
		return err
	}

	return execErr
}
