package upstream

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/consensus"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/hedgepolicy"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/failsafe-go/failsafe-go/timeout"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func CreateFailSafePolicies(logger *zerolog.Logger, scope common.Scope, entity string, fsCfg *common.FailsafeConfig) (map[string]failsafe.Policy[*common.NormalizedResponse], error) {
	// The order of policies below are important as per docs of failsafe-go
	var policies = map[string]failsafe.Policy[*common.NormalizedResponse]{}

	if fsCfg == nil {
		return policies, nil
	}

	lg := logger.With().Str("scope", string(scope)).Str("entity", entity).Logger()

	if fsCfg.Timeout != nil {
		plc, err := createTimeoutPolicy(logger, fsCfg.Timeout)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(
				err,
				map[string]interface{}{
					"scope":    scope,
					"entity":   entity,
					"policy":   "timeout",
					"provider": fsCfg.Timeout,
				},
			)
		}
		policies["timeout"] = plc
	}

	if fsCfg.Retry != nil {
		p, err := createRetryPolicy(scope, fsCfg.Retry)
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
		p, err := createCircuitBreakerPolicy(&lg, fsCfg.CircuitBreaker)
		if err != nil {
			return nil, err
		}
		policies["circuitBreaker"] = p
	}

	if fsCfg.Hedge != nil && fsCfg.Hedge.MaxCount > 0 {
		p, err := createHedgePolicy(&lg, fsCfg.Hedge)
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
		p, err := createConsensusPolicy(&lg, fsCfg.Consensus)
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

	return pls
}

func createCircuitBreakerPolicy(logger *zerolog.Logger, cfg *common.CircuitBreakerPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
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

	if cfg.HalfOpenAfter > 0 {
		builder = builder.WithDelay(cfg.HalfOpenAfter.Duration())
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
						lg = lg.Str("upstreamId", up.Id())
						cfg := up.Config()
						if cfg.Evm != nil {
							if ups, ok := up.(common.EvmUpstream); ok {
								lg = lg.Interface("upstreamSyncingState", ups.EvmSyncingState())
							}
						}
					}
				}
			}
			lg.Msg("failure caught that will be considered for circuit breaker")
		}
		// TODO emit a custom prometheus metric to track CB root causes?
	})

	builder.HandleIf(func(exec failsafe.ExecutionAttempt[*common.NormalizedResponse], result *common.NormalizedResponse, err error) bool {
		ctx := exec.Context()
		ctx, span := common.StartDetailSpan(ctx, "CircuitBreaker.HandleIf")
		defer span.End()

		// 5xx or other non-retryable server-side errors -> open the circuit
		if common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
			span.SetAttributes(
				attribute.Bool("should_open", true),
				attribute.String("reason", "server_side_exception"),
				attribute.String("error_code", "ErrCodeEndpointServerSideException"),
			)
			return true
		}

		// 401 / 403 / RPC-RPC vendor auth -> open the circuit
		if common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized) {
			span.SetAttributes(
				attribute.Bool("should_open", true),
				attribute.String("reason", "unauthorized"),
				attribute.String("error_code", "ErrCodeEndpointUnauthorized"),
			)
			return true
		}

		// remote vendor billing issue -> open the circuit
		if common.HasErrorCode(err, common.ErrCodeEndpointBillingIssue) {
			span.SetAttributes(
				attribute.Bool("should_open", true),
				attribute.String("reason", "billing_issue"),
				attribute.String("error_code", "ErrCodeEndpointBillingIssue"),
			)
			return true
		}

		// if "syncing" and null/empty response -> open the circuit
		if result != nil && result.Request() != nil {
			up := result.Request().LastUpstream()
			if ups, ok := up.(common.EvmUpstream); ok {
				syncState := ups.EvmSyncingState()
				isEmpty := result.IsResultEmptyish()
				span.SetAttributes(
					attribute.String("upstream.id", ups.Id()),
					attribute.String("upstream.sync_state", syncState.String()),
					attribute.Bool("response.is_empty", isEmpty),
				)
				if syncState == common.EvmSyncingStateSyncing {
					if isEmpty {
						span.SetAttributes(
							attribute.Bool("should_open", true),
							attribute.String("reason", "syncing_with_empty_response"),
						)
						return true
					}
				}
			}
		}

		// other errors must not open the circuit because it does not mean that the remote service is "bad"
		span.SetAttributes(
			attribute.Bool("should_open", false),
			attribute.String("reason", "not_circuit_breaker_error"),
		)
		if err != nil {
			span.SetAttributes(attribute.String("error", err.Error()))
		}
		return false
	})

	return builder.Build(), nil
}

func createHedgePolicy(logger *zerolog.Logger, cfg *common.HedgePolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	var builder hedgepolicy.HedgePolicyBuilder[*common.NormalizedResponse]

	delay := cfg.Delay.Duration()
	if cfg.Quantile > 0 {
		minDelay := cfg.MinDelay.Duration()
		maxDelay := cfg.MaxDelay.Duration()

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

	builder = builder.OnHedge(func(event failsafe.ExecutionEvent[*common.NormalizedResponse]) bool {
		ctx := event.Context()
		ctx, span := common.StartDetailSpan(ctx, "HedgePolicy.OnHedge")
		defer span.End()

		var req *common.NormalizedRequest
		var method string
		r := event.Context().Value(common.RequestContextKey)
		if r != nil {
			var ok bool
			req, ok = r.(*common.NormalizedRequest)
			if ok && req != nil {
				if req.IsCompositeRequest() {
					span.SetAttributes(
						attribute.Bool("hedge", false),
						attribute.String("reason", "composite_request"),
						attribute.String("composite_type", req.CompositeType()),
					)
					logger.Debug().Str("method", method).Interface("id", req.ID()).Str("compositeType", req.CompositeType()).Msgf("ignoring hedge for composite request")
					return false
				}

				method, _ = req.Method()
				span.SetAttributes(attribute.String("method", method))
				if method != "" && evm.IsWriteMethod(method) {
					span.SetAttributes(
						attribute.Bool("hedge", false),
						attribute.String("reason", "write_method"),
					)
					logger.Debug().Str("method", method).Interface("id", req.ID()).Msgf("ignoring hedge for write request")
					return false
				}
			}
		}

		span.SetAttributes(
			attribute.Bool("hedge", true),
			attribute.String("reason", "allowed"),
			attribute.Int("attempts", event.Attempts()),
			attribute.Int("hedges", event.Hedges()),
		)
		logger.Trace().Str("method", method).Interface("id", req.ID()).Msgf("attempting to hedge request")

		// Continue with the next hedge
		return true
	})

	// We will only cancel other hedged requests if we have non-error response, otherwise we'll wait for other in-flight requests to complete.
	builder = builder.CancelIf(func(exec failsafe.ExecutionAttempt[*common.NormalizedResponse], result *common.NormalizedResponse, err error) bool {
		if err != nil || result == nil || result.IsObjectNull() {
			return false
		}
		jrr, err := result.JsonRpcResponse()
		if jrr == nil || err != nil {
			return false
		}
		if jrr.Error != nil {
			return false
		}
		return true
	})

	return builder.Build(), nil
}

func createRetryPolicy(scope common.Scope, cfg *common.RetryPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	builder := retrypolicy.Builder[*common.NormalizedResponse]()

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

	// Use default values if not set
	emptyResultConfidence := cfg.EmptyResultConfidence
	if emptyResultConfidence == 0 {
		emptyResultConfidence = common.AvailbilityConfidenceFinalized
	}

	emptyResultIgnore := cfg.EmptyResultIgnore
	if emptyResultIgnore == nil {
		emptyResultIgnore = []string{"eth_getLogs", "eth_call"}
	}

	builder = builder.HandleIf(func(exec failsafe.ExecutionAttempt[*common.NormalizedResponse], result *common.NormalizedResponse, err error) bool {
		ctx := exec.Context()
		ctx, span := common.StartDetailSpan(ctx, "RetryPolicy.HandleIf",
			trace.WithAttributes(
				attribute.String("scope", string(scope)),
			))
		defer span.End()

		// Node-level execution exceptions (e.g. reverted eth_call) -> No Retry
		if common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
			span.SetAttributes(
				attribute.Bool("retry", false),
				attribute.String("reason", "execution_exception"),
				attribute.String("error_code", "ErrCodeEndpointExecutionException"),
			)
			return false
		}

		if result != nil && result.Request() != nil && result.Request().IsCompositeRequest() {
			span.SetAttributes(
				attribute.Bool("retry", false),
				attribute.String("reason", "composite_request"),
			)
			return false
		}

		// Must not retry any 'write' methods
		if result != nil {
			if req := result.Request(); req != nil {
				if method, _ := req.Method(); method != "" && evm.IsWriteMethod(method) {
					span.SetAttributes(
						attribute.Bool("retry", false),
						attribute.String("reason", "write_method"),
						attribute.String("write_method", method),
					)
					return false
				}
			}
		}

		// We are only short-circuiting retry if the error is not retryable towards the upstream or network
		if scope == common.ScopeUpstream && err != nil {
			isRetryable := common.IsRetryableTowardsUpstream(err)
			span.SetAttributes(
				attribute.Bool("error.retryable_to_upstream", isRetryable),
			)
			if !isRetryable {
				span.SetAttributes(
					attribute.Bool("retry", false),
					attribute.String("reason", "not_retryable_to_upstream"),
				)
				return false
			}
		} else if scope == common.ScopeNetwork && err != nil {
			isRetryable := common.IsRetryableTowardNetwork(err)
			span.SetAttributes(
				attribute.Bool("error.retryable_to_network", isRetryable),
			)
			if isRetryable {
				span.SetAttributes(
					attribute.Bool("retry", true),
					attribute.String("reason", "retryable_to_network"),
				)
				return true
			}
		}

		if scope == common.ScopeNetwork && result != nil && !result.IsObjectNull() {
			req := result.Request()
			if req == nil {
				// If there's no request, we can't check directives or block availability
				// Only retry if there's an error
				shouldRetry := err != nil
				span.SetAttributes(
					attribute.Bool("retry", shouldRetry),
					attribute.String("reason", "no_request_context"),
					attribute.Bool("has_error", err != nil),
				)
				return shouldRetry
			}
			rds := req.Directives()

			// Retry empty responses on network-level to give a chance for another upstream to
			// try fetching the data as the current upstream is less likely to have the data ready on the next retry attempt.
			if rds != nil && rds.RetryEmpty {
				isEmpty := result.IsResultEmptyish()
				span.SetAttributes(
					attribute.Bool("directive.retry_empty", true),
					attribute.Bool("response.is_empty", isEmpty),
				)
				if isEmpty {
					method, _ := req.Method()
					span.SetAttributes(
						attribute.String("method", method),
						attribute.Bool("method_in_ignore_list", slices.Contains(emptyResultIgnore, method)),
					)
					// Check if method is in ignore list - if so, do NOT retry
					if slices.Contains(emptyResultIgnore, method) {
						span.SetAttributes(
							attribute.Bool("retry", false),
							attribute.String("reason", "method_in_empty_ignore_list"),
						)
						return false
					}
					ups := result.Upstream()
					// has Retry-Empty directive + "empty" response + upstream can handle the block -> No Retry
					if err == nil && ups != nil {
						upCfg := ups.Config()
						if upCfg != nil && upCfg.Type == common.UpstreamTypeEvm && upCfg.Evm != nil {
							if ups, ok := ups.(common.EvmUpstream); ok {
								syncState := ups.EvmSyncingState()
								span.SetAttributes(
									attribute.String("upstream.id", ups.Id()),
									attribute.String("upstream.sync_state", syncState.String()),
								)
								if syncState != common.EvmSyncingStateSyncing {
									str, bn, ebn := evm.ExtractBlockReferenceFromRequest(ctx, req)
									span.SetAttributes(
										attribute.String("extracted_block_number", fmt.Sprintf("%d", bn)),
										attribute.String("extracted_block_ref", fmt.Sprintf("%s", str)),
										attribute.String("extracted_block_error", fmt.Sprintf("%v", ebn)),
									)
									if ebn == nil && bn > 0 {
										// Use EvmAssertBlockAvailability to check if the upstream can handle the block
										if avail, err := ups.EvmAssertBlockAvailability(ctx, method, emptyResultConfidence, false, bn); err == nil && avail {
											// If the upstream can handle the block and returned empty, don't retry
											span.SetAttributes(
												attribute.Bool("block_available", true),
												attribute.Bool("retry", false),
												attribute.String("reason", "block_available_but_empty"),
											)
											return false
										} else {
											span.SetAttributes(
												attribute.Bool("block_available", false),
												attribute.Bool("retry", true),
												attribute.String("reason", "block_not_available"),
											)
										}
									}
								}
							}
						}
					}
					// Empty response and RetryEmpty is true, but block is not available or other conditions not met -> Retry
					span.SetAttributes(
						attribute.Bool("retry", true),
						attribute.String("reason", "empty_response_retry_directive"),
					)
					return true
				}
			}

			// For pending transactions retry on network-level to give a chance of receiving
			// the full TX data when it is available.
			if rds != nil && rds.RetryPending {
				req := result.Request()
				if req != nil {
					method, _ := req.Method()
					switch method {
					case "eth_getTransactionReceipt",
						"eth_getTransactionByHash",
						"eth_getTransactionByBlockHashAndIndex",
						"eth_getTransactionByBlockNumberAndIndex":
						_, blkNum, err := evm.ExtractBlockReferenceFromRequest(ctx, req)
						if err == nil {
							if blkNum == 0 {
								span.SetAttributes(
									attribute.Bool("retry", true),
									attribute.String("reason", "pending_transaction"),
									attribute.String("tx_method", method),
									attribute.Bool("directive.retry_pending", true),
								)
								return true
							}
						}
					}
				}
			}
		}

		// 5xx -> Retry
		shouldRetry := err != nil
		span.SetAttributes(
			attribute.Bool("retry", shouldRetry),
			attribute.String("reason", "error_present"),
			attribute.Bool("has_error", err != nil),
		)
		if err != nil {
			span.SetAttributes(attribute.String("error", err.Error()))
		}
		return shouldRetry
	})

	return builder.Build(), nil
}

func createTimeoutPolicy(logger *zerolog.Logger, cfg *common.TimeoutPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	builder := timeout.Builder[*common.NormalizedResponse](cfg.Duration.Duration())

	if logger.GetLevel() == zerolog.TraceLevel {
		builder.OnTimeoutExceeded(func(event failsafe.ExecutionDoneEvent[*common.NormalizedResponse]) {
			logger.Trace().Msgf("failsafe timeout policy: %v (start time: %v, elapsed: %v, attempts: %d, retries: %d, hedges: %d)", event.Error, event.StartTime().Format(time.RFC3339), event.ElapsedTime().String(), event.Attempts(), event.Retries(), event.Hedges())
		})
	}

	return builder.Build(), nil
}

func createConsensusPolicy(logger *zerolog.Logger, cfg *common.ConsensusPolicyConfig) (failsafe.Policy[*common.NormalizedResponse], error) {
	if cfg == nil {
		// No consensus config given, so no policy
		return nil, nil
	}

	builder := consensus.NewConsensusPolicyBuilder[*common.NormalizedResponse]()
	builder = builder.WithRequiredParticipants(cfg.RequiredParticipants)
	builder = builder.WithAgreementThreshold(cfg.AgreementThreshold)
	builder = builder.WithDisputeBehavior(cfg.DisputeBehavior)
	builder = builder.WithPunishMisbehavior(cfg.PunishMisbehavior)
	builder = builder.WithLowParticipantsBehavior(cfg.LowParticipantsBehavior)
	builder = builder.WithLogger(logger)

	// Set ignore fields if configured
	if cfg.IgnoreFields != nil {
		builder = builder.WithIgnoreFields(cfg.IgnoreFields)
	}

	// Parse dispute log level if specified
	if cfg.DisputeLogLevel != "" {
		level, err := zerolog.ParseLevel(cfg.DisputeLogLevel)
		if err != nil {
			logger.Warn().Str("disputeLogLevel", cfg.DisputeLogLevel).Err(err).Msg("invalid dispute log level, using default")
		} else {
			builder = builder.WithDisputeLogLevel(level)
		}
	}

	builder.OnAgreement(func(event failsafe.ExecutionEvent[*common.NormalizedResponse]) {
		logger.Debug().Msg("spawning additional consensus request")
	})

	p := builder.Build()
	return p, nil
}

func TranslateFailsafeError(scope common.Scope, upstreamId string, method string, execErr error, startTime *time.Time) error {
	var err error
	var retryExceededErr retrypolicy.ExceededError

	// Our own standard error is returned when failsafe execution is returned and for example retry policy
	// logic above decided it does not need to retry (e.g. reverted transaction error).
	// Another case is an UpstreamExhausted error which is not going to be retried due to all errors being unretryable.
	// In those cases we return the standard error object as is.
	if serr, ok := execErr.(common.StandardError); ok {
		err = serr
	} else if errors.As(execErr, &retryExceededErr) {
		// When retry policy is exceeded (i.e. we wanted to retry based on the policy but it ultimately failed)
		// we want to fetch the "last error" from the retry policy and wrap in our own standard error type of FailsafeRetryExceeded.
		// This allows consistent error handling on http server level.
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
		if exr, ok := translatedCause.(*common.ErrUpstreamsExhausted); ok {
			// In this case we already have a grouping of all errors encountered via upstreams,
			// also this means errors are not due to other reasons (like self-imposed rate limiting).
			err = exr
		} else {
			err = common.NewErrFailsafeRetryExceeded(scope, translatedCause, startTime)
		}
	} else if errors.Is(execErr, timeout.ErrExceeded) {
		// Simply translate the failsafe library timeout error type to our own standard error type.
		// And keep the original error as "cause" so it can be logged.
		err = common.NewErrFailsafeTimeoutExceeded(scope, execErr, startTime)
	} else if errors.Is(execErr, circuitbreaker.ErrOpen) {
		// Simply translate the failsafe library circuit breaker error type to our own standard error type.
		// And keep the original error as "cause" so it can be logged.
		err = common.NewErrFailsafeCircuitBreakerOpen(scope, execErr, startTime)
	}

	if err != nil {
		if ser, ok := execErr.(common.StandardError); ok {
			be := ser.Base()
			if be != nil {
				var dts map[string]interface{}
				if be.Details != nil {
					dts = be.Details
				} else {
					dts = make(map[string]interface{})
				}
				if method != "" {
					dts["method"] = method
				}
				if upstreamId != "" {
					dts["upstreamId"] = upstreamId
				}
				be.Details = dts
			}
		}
		return err
	}

	if joinedErr, ok := execErr.(interface{ Unwrap() []error }); ok {
		errs := joinedErr.Unwrap()
		if len(errs) == 1 {
			return errs[0]
		} else if len(errs) > 1 {
			return common.NewErrUpstreamsExhaustedWithCause(execErr)
		}
	}

	// For unknown errors we return as is so we're not wrongly wrapping with an inappropriate error type.
	// An example can be deadline exceeded error which must be handled properly on http server level (e.g. wrap with http timeout error).
	return execErr
}
