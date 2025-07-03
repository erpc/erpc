package consensus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/failsafe-go/failsafe-go"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/failsafe-go/failsafe-go/policy"
	"github.com/failsafe-go/failsafe-go/ratelimiter"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// executor is a policy.Executor that handles failures according to a ConsensusPolicy.
type executor[R any] struct {
	*policy.BaseExecutor[R]
	*consensusPolicy[R]
}

var _ policy.Executor[any] = &executor[any]{}

type execResult[R any] struct {
	result   R
	err      error
	index    int
	upstream common.Upstream
}

// resultToHash converts a result to a string representation for comparison
func (e *executor[R]) resultToHash(result R, exec failsafe.Execution[R]) (string, error) {
	jr := e.resultToJsonRpcResponse(result, exec)
	if jr == nil {
		return "", fmt.Errorf("no json-rpc response available on result")
	}
	return jr.CanonicalHash()
}

// resultToRawString converts a result to its raw string representation
func (e *executor[R]) resultToJsonRpcResponse(result R, exec failsafe.Execution[R]) *common.JsonRpcResponse {
	resp, ok := any(result).(*common.NormalizedResponse)
	if !ok {
		return nil
	}

	jr, err := resp.JsonRpcResponse(exec.Context())
	if err != nil {
		return nil
	}

	return jr
}

// isConsensusValidError checks if an error is a valid consensus result (e.g., execution exception)
func (e *executor[R]) isConsensusValidError(err error) bool {
	// Only execution exceptions should trigger short-circuit consensus
	// This includes execution reverts and call exceptions
	return common.HasErrorCode(err, common.ErrCodeEndpointExecutionException)
}

// isAgreedUponError checks if an error should be considered for agreement after all upstreams respond
// This includes a broader set of errors like invalid params, missing data, unsupported methods
func (e *executor[R]) isAgreedUponError(err error) bool {
	// Include common errors that indicate agreement on request issues
	return common.HasErrorCode(err,
		common.ErrCodeEndpointClientSideException, // Invalid params, bad requests
		common.ErrCodeEndpointUnsupported,         // Method not supported
		common.ErrCodeEndpointMissingData,         // Missing data
	)
}

// errorToConsensusHash converts an error to a hash for consensus comparison
// For errors with BaseError.Code, it uses the code
// For JsonRpcExceptionInternal errors, it also includes the normalized code
func (e *executor[R]) errorToConsensusHash(err error) string {
	if err == nil {
		return ""
	}

	// First check if this is a StandardError to get the base error code
	if se, ok := err.(common.StandardError); ok {
		baseErr := se.Base()
		if baseErr != nil {
			hash := string(baseErr.Code)

			// For execution exceptions and other JSON-RPC errors, we need to look deeper
			// to find the JsonRpcExceptionInternal and include its normalized code
			if baseErr.Code == common.ErrCodeEndpointExecutionException {
				// Look for JsonRpcExceptionInternal in the cause chain
				var jre *common.ErrJsonRpcExceptionInternal
				if errors.As(err, &jre) {
					// Include the normalized code for more specific matching
					hash = fmt.Sprintf("%s:%d", hash, jre.NormalizedCode())
				}
			}

			return hash
		}
	}

	// For other errors that might have JsonRpcExceptionInternal
	var jre *common.ErrJsonRpcExceptionInternal
	if errors.As(err, &jre) {
		// Use normalized code for JSON-RPC errors
		return fmt.Sprintf("jsonrpc:%d", jre.NormalizedCode())
	}

	return ""
}

// resultOrErrorToHash converts either a successful result or an error to a hash for consensus comparison
func (e *executor[R]) resultOrErrorToHash(result R, err error, exec failsafe.Execution[R]) (string, error) {
	// If there's an error and it's consensus-valid, use the error hash
	if err != nil && e.isConsensusValidError(err) {
		return e.errorToConsensusHash(err), nil
	}

	// Otherwise, use the result hash (for successful responses)
	if err != nil {
		// Non-consensus-valid errors don't contribute to consensus
		return "", fmt.Errorf("error is not consensus-valid: %v", err)
	}

	return e.resultToHash(result, exec)
}

func (e *executor[R]) Apply(innerFn func(failsafe.Execution[R]) *failsafeCommon.PolicyResult[R]) func(failsafe.Execution[R]) *failsafeCommon.PolicyResult[R] {
	return func(exec failsafe.Execution[R]) *failsafeCommon.PolicyResult[R] {
		startTime := time.Now()
		ctx := exec.Context()

		// Extract the original request and upstream list from the execution context.
		val := ctx.Value(common.RequestContextKey)
		originalReq, ok := val.(*common.NormalizedRequest)
		if !ok || originalReq == nil {
			// shouldn't happen, but fall back to simple execution
			e.logger.Warn().Object("request", originalReq).Msg("unexpecued nil request in consensus policy")
			return innerFn(exec)
		}

		// Extract method and network info for metrics
		method := "unknown"
		if m, err := originalReq.Method(); err == nil {
			method = m
		}
		category := method
		networkId := originalReq.NetworkId()
		projectId := ""
		if originalReq.Network() != nil {
			projectId = originalReq.Network().ProjectId()
		}
		finality := originalReq.Finality(ctx)
		finalityStr := finality.String()

		// Start main consensus span
		ctx, consensusSpan := common.StartSpan(ctx, "Consensus.Apply",
			trace.WithAttributes(
				attribute.String("network.id", networkId),
				attribute.String("request.method", method),
				attribute.Int("execution.attempts", exec.Attempts()),
			),
		)
		if common.IsTracingDetailed {
			consensusSpan.SetAttributes(
				attribute.String("request.id", fmt.Sprintf("%v", originalReq.ID())),
			)
		}
		defer consensusSpan.End()

		lg := e.logger.With().
			Interface("id", originalReq.ID()).
			Str("component", "consensus").
			Str("networkId", networkId).
			Logger()

		val = ctx.Value(common.UpstreamsContextKey)
		upsList, ok := val.([]common.Upstream)
		if !ok || len(upsList) == 0 {
			lg.Warn().Object("request", originalReq).Interface("upstreams", upsList).Msg("unexpecued incompatible or empty list of upstreams in consensus policy")
			common.SetTraceSpanError(consensusSpan, fmt.Errorf("invalid or empty upstreams list"))
			telemetry.MetricConsensusTotal.WithLabelValues(projectId, networkId, category, "error", finalityStr).Inc()
			telemetry.MetricConsensusErrors.WithLabelValues(projectId, networkId, category, "invalid_upstreams", finalityStr).Inc()
			return innerFn(exec)
		}

		lg.Debug().
			Interface("upstreams", func() []string {
				ids := make([]string, len(upsList))
				for i, up := range upsList {
					ids[i] = up.Id()
				}
				return ids
			}()).
			Int("upstreamsCount", len(upsList)).
			Msg("consensus policy received upstreams")

		// Pick the upstreams that will participate in the consensus round.
		_, selectionSpan := common.StartDetailSpan(ctx, "Consensus.SelectUpstreams")
		selectedUpstreams := e.selectUpstreams(upsList)
		selectionSpan.SetAttributes(
			attribute.Int("upstreams.total", len(upsList)),
			attribute.Int("upstreams.selected", len(selectedUpstreams)),
			attribute.Int("required_participants", e.requiredParticipants),
		)
		selectionSpan.End()

		parentExecution := exec.(policy.ExecutionInternal[R])

		// Phase 1: Fire off requests and collect responses
		responses := e.collectResponses(ctx, &lg, originalReq, selectedUpstreams, parentExecution, innerFn)

		// Phase 2: Evaluate consensus
		result := e.evaluateConsensus(ctx, &lg, responses, selectedUpstreams, exec)

		// Phase 3: Track misbehaving upstreams (only if we have a clear majority)
		e.checkAndPunishMisbehavingUpstreams(ctx, &lg, responses, parentExecution)

		// Set final consensus result attributes
		consensusSpan.SetAttributes(
			attribute.Int("responses.collected", len(responses)),
			attribute.Bool("consensus.achieved", result.Error == nil || !common.HasErrorCode(result.Error, common.ErrCodeConsensusDispute, common.ErrCodeConsensusLowParticipants)),
		)

		// Determine outcome for metrics
		outcome := "success"
		if result.Error != nil {
			// Check if this is a consensus-valid error (execution revert)
			if e.isConsensusValidError(result.Error) {
				outcome = "consensus_on_error"
			} else if e.isAgreedUponError(result.Error) {
				// Check if this is an agreed-upon error returned from consensus evaluation
				outcome = "agreed_error"
			} else if common.HasErrorCode(result.Error, common.ErrCodeConsensusDispute) {
				outcome = "dispute"
			} else if common.HasErrorCode(result.Error, common.ErrCodeConsensusLowParticipants) {
				outcome = "low_participants"
			} else {
				outcome = "error"
			}
			common.SetTraceSpanError(consensusSpan, result.Error)
		}

		// Record metrics
		duration := time.Since(startTime).Seconds()
		telemetry.MetricConsensusTotal.WithLabelValues(projectId, networkId, category, outcome, finalityStr).Inc()
		telemetry.MetricConsensusDuration.WithLabelValues(projectId, networkId, category, outcome, finalityStr).Observe(duration)

		// Update agreement rate (simplified moving average)
		if outcome == "success" || outcome == "consensus_on_error" || outcome == "agreed_error" {
			telemetry.MetricConsensusAgreementRate.WithLabelValues(projectId, networkId).Set(1.0)
		} else {
			telemetry.MetricConsensusAgreementRate.WithLabelValues(projectId, networkId).Set(0.0)
		}

		return result
	}
}

// collectResponses fires off requests to all selected upstreams and collects responses with short-circuit logic
// When consensus is reached or becomes impossible, remaining requests are cancelled immediately to save resources.
// Misbehavior tracking (if configured) will only consider the responses collected before cancellation.
func (e *executor[R]) collectResponses(
	ctx context.Context,
	lg *zerolog.Logger,
	originalReq *common.NormalizedRequest,
	selectedUpstreams []common.Upstream,
	parentExecution policy.ExecutionInternal[R],
	innerFn func(failsafe.Execution[R]) *failsafeCommon.PolicyResult[R],
) []*execResult[R] {
	// Extract metrics labels
	method := "unknown"
	if m, err := originalReq.Method(); err == nil {
		method = m
	}
	category := method
	networkId := originalReq.NetworkId()
	projectId := ""
	if originalReq.Network() != nil {
		projectId = originalReq.Network().ProjectId()
	}
	finality := originalReq.Finality(ctx)
	finalityStr := finality.String()

	// Start collection span
	ctx, collectionSpan := common.StartDetailSpan(ctx, "Consensus.CollectResponses",
		trace.WithAttributes(
			attribute.Int("upstreams.selected", len(selectedUpstreams)),
		),
	)
	defer collectionSpan.End()

	// Context for canceling remaining requests
	cancellableCtx, cancelRemaining := context.WithCancel(ctx)
	defer cancelRemaining()

	// Channel to collect responses - buffered to prevent blocking
	responseChan := make(chan *execResult[R], len(selectedUpstreams))

	// Start all upstream requests
	actualGoroutineCount := 0
	for execIdx, up := range selectedUpstreams {
		cloneReq, err := originalReq.Clone()
		if err != nil {
			lg.Error().Err(err).Msg("error cloning request")
			// Send nil result immediately to maintain expected count
			go func(idx int) {
				select {
				case responseChan <- nil:
				case <-cancellableCtx.Done():
				}
			}(execIdx)
			actualGoroutineCount++
			continue
		}
		cloneReq.Directives().UseUpstream = up.Id()

		lg.Debug().
			Interface("id", cloneReq.ID()).
			Int("index", execIdx).
			Str("upstream", up.Id()).
			Object("request", cloneReq).
			Msg("prepared cloned request for consensus execution")

		execution := parentExecution.CopyForCancellableWithValue(
			common.RequestContextKey,
			cloneReq,
		).(policy.ExecutionInternal[R])

		// Fire off the request
		go func(consensusExec policy.ExecutionInternal[R], execIdx int, upstream common.Upstream) {
			// Start a span for this consensus attempt
			attemptCtx, attemptSpan := common.StartDetailSpan(cancellableCtx, "Consensus.Attempt",
				trace.WithAttributes(
					attribute.Int("attempt.index", execIdx),
					attribute.String("upstream.id", upstream.Id()),
				),
			)
			defer attemptSpan.End()

			// Add panic recovery
			defer func() {
				if r := recover(); r != nil {
					lg.Error().
						Int("index", execIdx).
						Str("upstream", upstream.Id()).
						Interface("panic", r).
						Msg("panic in consensus execution")

					panicErr := fmt.Errorf("panic in consensus execution: %v", r)
					common.SetTraceSpanError(attemptSpan, panicErr)

					// Record panic metric
					telemetry.MetricConsensusPanics.WithLabelValues(projectId, networkId, category, finalityStr).Inc()

					// Send error result
					select {
					case responseChan <- &execResult[R]{
						err:      panicErr,
						index:    execIdx,
						upstream: upstream,
					}:
					case <-cancellableCtx.Done():
						// Still try to send nil to unblock collector
						select {
						case responseChan <- nil:
						case <-cancellableCtx.Done():
						}
					}
				}
			}()

			// Check cancellation before execution
			select {
			case <-attemptCtx.Done():
				lg.Debug().Int("index", execIdx).Str("upstream", upstream.Id()).Msg("skipping execution, context cancelled")
				attemptSpan.SetAttributes(attribute.Bool("cancelled_before_execution", true))
				attemptSpan.SetStatus(codes.Error, "cancelled before execution")
				telemetry.MetricConsensusCancellations.WithLabelValues(projectId, networkId, category, "before_execution", finalityStr).Inc()
				// Still send a nil result so the collector knows this goroutine is done
				select {
				case responseChan <- nil:
				case <-attemptCtx.Done():
					// Already cancelled, safe to exit
				}
				return
			default:
			}

			lg.Trace().Int("index", execIdx).Object("request", originalReq).Msg("sending consensus request")

			// The execution should inherit the cancellable context from the parent
			// Double-check the context hasn't been cancelled before executing
			if attemptCtx.Err() != nil {
				lg.Debug().Int("index", execIdx).Str("upstream", upstream.Id()).Msg("context cancelled before execution")
				attemptSpan.SetAttributes(attribute.Bool("cancelled_before_execution", true))
				attemptSpan.SetStatus(codes.Error, "cancelled before execution")
				telemetry.MetricConsensusCancellations.WithLabelValues(projectId, networkId, category, "before_execution", finalityStr).Inc()
				select {
				case responseChan <- nil:
				case <-attemptCtx.Done():
				}
				return
			}

			// The consensus execution already has the correct context from the parent
			result := innerFn(consensusExec)

			// Check cancellation again after execution
			select {
			case <-attemptCtx.Done():
				lg.Debug().Int("index", execIdx).Str("upstream", upstream.Id()).Msg("discarding result, context cancelled after execution")
				attemptSpan.SetAttributes(attribute.Bool("cancelled_after_execution", true))
				attemptSpan.SetStatus(codes.Error, "cancelled after execution")
				telemetry.MetricConsensusCancellations.WithLabelValues(projectId, networkId, category, "after_execution", finalityStr).Inc()
				// Still send nil to unblock collector
				select {
				case responseChan <- nil:
				case <-attemptCtx.Done():
				}
				return
			default:
			}

			if result == nil {
				attemptSpan.SetAttributes(attribute.Bool("result.nil", true))
				// Send nil to indicate this goroutine is done
				select {
				case responseChan <- nil:
				case <-attemptCtx.Done():
				}
				return
			}

			// Set attempt result attributes
			if result.Error != nil {
				common.SetTraceSpanError(attemptSpan, result.Error)
			} else {
				attemptSpan.SetStatus(codes.Ok, "")
			}

			// Send response to channel
			select {
			case responseChan <- &execResult[R]{
				result:   result.Result,
				err:      result.Error,
				index:    execIdx,
				upstream: upstream,
			}:
			case <-attemptCtx.Done():
				// Context cancelled, still try to send nil to unblock collector
				lg.Debug().Int("index", execIdx).Str("upstream", upstream.Id()).Msg("discarding response due to context cancellation")
				attemptSpan.SetAttributes(attribute.Bool("response_discarded", true))
				telemetry.MetricConsensusCancellations.WithLabelValues(projectId, networkId, category, "after_execution", finalityStr).Inc()
				select {
				case responseChan <- nil:
				case <-attemptCtx.Done():
					// Already cancelled, safe to exit
				}
			}
		}(execution, execIdx, up)
		actualGoroutineCount++
	}

	// Collect responses with short-circuit logic
	responses := make([]*execResult[R], 0, len(selectedUpstreams))
	var shortCircuited bool
	var shortCircuitReason string

collectLoop:
	for i := 0; i < actualGoroutineCount; i++ {
		select {
		case resp := <-responseChan:
			// Skip nil responses (from cancelled goroutines)
			if resp != nil {
				responses = append(responses, resp)
			}

			// Check if we should short-circuit (only if we have a non-nil response)
			if resp != nil && !shortCircuited && e.checkShortCircuit(lg, responses, selectedUpstreams, parentExecution) {
				shortCircuited = true

				// Determine short-circuit reason
				if e.hasConsensus(responses, parentExecution) {
					shortCircuitReason = "consensus_reached"
				} else {
					shortCircuitReason = "consensus_impossible"
				}

				cancelRemaining()
				collectionSpan.SetAttributes(
					attribute.Bool("short_circuited", true),
					attribute.Int("responses.at_short_circuit", len(responses)),
				)

				// Record short-circuit metric
				telemetry.MetricConsensusShortCircuit.WithLabelValues(projectId, networkId, category, shortCircuitReason, finalityStr).Inc()

				// Drain any remaining responses in the background
				go e.drainResponses(responseChan, actualGoroutineCount-(i+1))
				break collectLoop
			}

		case <-ctx.Done():
			// Original context cancelled
			lg.Debug().
				Int("received", len(responses)).
				Int("expected", actualGoroutineCount).
				Msg("context cancelled while waiting for consensus responses")
			cancelRemaining()
			common.SetTraceSpanError(collectionSpan, ctx.Err())
			telemetry.MetricConsensusCancellations.WithLabelValues(projectId, networkId, category, "collection", finalityStr).Inc()
			break collectLoop
		}
	}

	collectionSpan.SetAttributes(
		attribute.Int("responses.collected", len(responses)),
		attribute.Int("responses.expected", actualGoroutineCount),
	)

	// Record responses collected metric
	shortCircuitedStr := "false"
	if shortCircuited {
		shortCircuitedStr = "true"
	}
	telemetry.MetricConsensusResponsesCollected.WithLabelValues(projectId, networkId, category, shortCircuitedStr, finalityStr).Observe(float64(len(responses)))

	return responses
}

// hasConsensus checks if consensus has been reached in the current responses
func (e *executor[R]) hasConsensus(responses []*execResult[R], exec policy.ExecutionInternal[R]) bool {
	resultCounts := make(map[string]int)
	for _, r := range responses {
		if resultHash, err := e.resultOrErrorToHash(r.result, r.err, exec); err == nil && resultHash != "" {
			resultCounts[resultHash]++
			if resultCounts[resultHash] >= e.agreementThreshold {
				return true
			}
		}
	}
	return false
}

// checkShortCircuit determines if we can exit early based on current responses
func (e *executor[R]) checkShortCircuit(lg *zerolog.Logger, responses []*execResult[R], selectedUpstreams []common.Upstream, exec policy.ExecutionInternal[R]) bool {
	// Count results by hash
	resultCounts := make(map[string]int)
	for _, r := range responses {
		resultHash, err := e.resultOrErrorToHash(r.result, r.err, exec)
		if err != nil || resultHash == "" {
			continue
		}
		resultCounts[resultHash]++
	}

	// Find the most common result
	mostCommonCount := 0
	for _, count := range resultCounts {
		if count > mostCommonCount {
			mostCommonCount = count
		}
	}

	// Check if consensus is reached
	if mostCommonCount >= e.agreementThreshold {
		lg.Debug().
			Int("responseCount", len(responses)).
			Int("mostCommonCount", mostCommonCount).
			Int("threshold", e.agreementThreshold).
			Msg("consensus reached early, short-circuiting")
		return true
	}

	// Check if consensus is impossible
	remainingResponses := len(selectedUpstreams) - len(responses)

	// Count unique participants so far
	uniqueParticipants := make(map[string]struct{})
	for _, r := range responses {
		if (r.err == nil || e.isConsensusValidError(r.err)) && r.upstream != nil && r.upstream.Config() != nil {
			uniqueParticipants[r.upstream.Config().Id] = struct{}{}
		}
	}
	uniqueParticipantCount := len(uniqueParticipants)

	// We can short-circuit if:
	// 1. Consensus is mathematically impossible AND
	// 2. We have enough unique participants (or we know we'll have enough)
	canReachConsensus := false
	for _, count := range resultCounts {
		possibleCount := count + remainingResponses
		if possibleCount >= e.agreementThreshold {
			canReachConsensus = true
			break
		}
	}

	// Only short-circuit if we have enough unique participants or will have enough
	expectedUniqueParticipants := uniqueParticipantCount + remainingResponses
	if !canReachConsensus && expectedUniqueParticipants >= e.requiredParticipants {
		lg.Debug().
			Int("responseCount", len(responses)).
			Int("uniqueParticipants", uniqueParticipantCount).
			Int("mostCommonCount", mostCommonCount).
			Int("remainingResponses", remainingResponses).
			Msg("consensus impossible to reach, short-circuiting")
		return true
	}

	return false
}

// drainResponses drains remaining responses to prevent goroutine leaks
func (e *executor[R]) drainResponses(responseChan <-chan *execResult[R], remaining int) {
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()

	for i := 0; i < remaining; i++ {
		select {
		case <-responseChan:
			// Discard
		case <-timeout.C:
			// Give up after timeout
			return
		}
	}
}

// evaluateConsensus performs the final consensus evaluation on collected responses
func (e *executor[R]) evaluateConsensus(
	ctx context.Context,
	lg *zerolog.Logger,
	responses []*execResult[R],
	selectedUpstreams []common.Upstream,
	exec failsafe.Execution[R],
) *failsafeCommon.PolicyResult[R] {
	// Get request metadata for metrics
	req := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
	method := "unknown"
	if m, err := req.Method(); err == nil {
		method = m
	}
	category := method
	networkId := req.NetworkId()
	projectId := ""
	if req.Network() != nil {
		projectId = req.Network().ProjectId()
	}
	finality := req.Finality(ctx)
	finalityStr := finality.String()

	// Start evaluation span
	ctx, evalSpan := common.StartDetailSpan(ctx, "Consensus.Evaluate",
		trace.WithAttributes(
			attribute.Int("responses.count", len(responses)),
			attribute.Int("selected_upstreams.count", len(selectedUpstreams)),
		),
	)
	defer evalSpan.End()

	// Count unique participants and responses
	participantCount := e.countUniqueParticipants(responses)

	lg.Debug().
		Int("participantCount", participantCount).
		Int("requiredParticipants", e.requiredParticipants).
		Msg("counting unique participants")

	// Count responses by result hash
	resultCounts, mostCommonResultHash, mostCommonResultCount := e.countResponsesByHash(lg, responses, exec)

	// Record agreement count metric
	if mostCommonResultCount > 0 {
		telemetry.MetricConsensusAgreementCount.WithLabelValues(projectId, networkId, category, finalityStr).Observe(float64(mostCommonResultCount))
	}

	evalSpan.SetAttributes(
		attribute.Int("participants.count", participantCount),
		attribute.Int("participants.required", e.requiredParticipants),
		attribute.Int("results.unique_count", len(resultCounts)),
		attribute.Int("results.most_common_count", mostCommonResultCount),
		attribute.Int("results.agreement_threshold", e.agreementThreshold),
	)

	lg.Debug().
		Int("totalResponses", len(responses)).
		Int("required", len(selectedUpstreams)).
		Int("errorCount", len(responses)-len(resultCounts)).
		Interface("responseCounts", resultCounts).
		Msg("consensus check")

	lg.Debug().
		Str("mostCommonResultHash", mostCommonResultHash).
		Int("count", mostCommonResultCount).
		Int("threshold", e.agreementThreshold).
		Bool("hasConsensus", mostCommonResultCount >= e.agreementThreshold).
		Msg("consensus result")

	// Determine the result based on consensus status
	if mostCommonResultCount >= e.agreementThreshold {
		// We have consensus
		lg.Debug().Msg("completed consensus execution")
		evalSpan.SetAttributes(
			attribute.Bool("consensus.achieved", true),
			attribute.String("consensus.result_hash", mostCommonResultHash),
		)

		if e.onAgreement != nil {
			e.onAgreement(failsafe.ExecutionEvent[R]{
				ExecutionAttempt: exec,
			})
		}

		// Check if consensus was reached on an error
		// Find a response that matches the consensus hash to determine if it's an error consensus
		var consensusError error
		for _, r := range responses {
			if r.err != nil && e.isConsensusValidError(r.err) {
				errorHash := e.errorToConsensusHash(r.err)
				if errorHash == mostCommonResultHash {
					consensusError = r.err
					break
				}
			}
		}

		if consensusError != nil {
			// Consensus was reached on an error (e.g., all nodes agree on execution revert)
			lg.Debug().Err(consensusError).Msg("consensus reached on error")
			evalSpan.SetStatus(codes.Ok, "consensus achieved on error")

			// Track this as a special type of successful consensus
			telemetry.MetricConsensusErrors.WithLabelValues(projectId, networkId, category, "consensus_on_error", finalityStr).Inc()

			return &failsafeCommon.PolicyResult[R]{
				Error: consensusError,
			}
		}

		// Find the actual result that matches the consensus
		if result := e.findResultByHash(responses, mostCommonResultHash, exec); result != nil {
			evalSpan.SetStatus(codes.Ok, "consensus achieved")
			return &failsafeCommon.PolicyResult[R]{
				Result: *result,
			}
		}
	}

	// Check if we have low participants
	isLowParticipants := e.isLowParticipants(participantCount, len(responses), len(selectedUpstreams))

	if isLowParticipants {
		lg.Debug().
			Int("participantCount", participantCount).
			Int("responses", len(responses)).
			Int("required", e.requiredParticipants).
			Int("selectedUpstreams", len(selectedUpstreams)).
			Msg("handling low participants")

		evalSpan.SetAttributes(
			attribute.Bool("consensus.low_participants", true),
			attribute.String("low_participants.behavior", string(e.lowParticipantsBehavior)),
		)

		result := e.handleLowParticipants(lg, responses, exec)
		if result.Error != nil {
			common.SetTraceSpanError(evalSpan, result.Error)
			telemetry.MetricConsensusErrors.WithLabelValues(projectId, networkId, category, "low_participants", finalityStr).Inc()
		}
		return result
	}

	// Consensus dispute
	e.logDispute(lg, responses, mostCommonResultCount, exec)

	if e.onDispute != nil {
		e.onDispute(failsafe.ExecutionEvent[R]{
			ExecutionAttempt: exec,
		})
	}

	evalSpan.SetAttributes(
		attribute.Bool("consensus.dispute", true),
		attribute.String("dispute.behavior", string(e.disputeBehavior)),
	)

	// Before declaring dispute, check if the most common result is an agreed-upon error
	// If multiple upstreams agree on the same error (like invalid params), return that error
	for _, r := range responses {
		if r.err != nil && e.isAgreedUponError(r.err) {
			errorHash := e.errorToConsensusHash(r.err)
			if errorHash == mostCommonResultHash && mostCommonResultCount >= 2 {
				// Multiple upstreams agree on this error, return it instead of dispute
				lg.Debug().
					Err(r.err).
					Str("errorHash", errorHash).
					Int("agreementCount", mostCommonResultCount).
					Msg("consensus on agreed-upon error, returning agreed error")
				evalSpan.SetStatus(codes.Ok, "consensus achieved on agreed-upon error")
				telemetry.MetricConsensusErrors.WithLabelValues(projectId, networkId, category, "agreed_error", finalityStr).Inc()
				return &failsafeCommon.PolicyResult[R]{
					Error: r.err,
				}
			}
		}
	}

	// Handle dispute according to behavior
	result := e.handleDispute(lg, responses, exec)
	if result.Error != nil {
		common.SetTraceSpanError(evalSpan, result.Error)
		telemetry.MetricConsensusErrors.WithLabelValues(projectId, networkId, category, "dispute", finalityStr).Inc()
	}
	return result
}

// countUniqueParticipants counts the number of unique participants that provided responses
func (e *executor[R]) countUniqueParticipants(responses []*execResult[R]) int {
	uniqueParticipants := make(map[string]struct{})
	for _, r := range responses {
		// Count all participants that provided a response (success or any error)
		if r.upstream != nil && r.upstream.Config() != nil {
			if r.err != nil {
				if e.isConsensusValidError(r.err) {
					uniqueParticipants[r.upstream.Config().Id] = struct{}{}
				}
			} else {
				uniqueParticipants[r.upstream.Config().Id] = struct{}{}
			}
		}
	}
	return len(uniqueParticipants)
}

// countResponsesByHash counts responses by their hash and returns the counts, most common hash and count
func (e *executor[R]) countResponsesByHash(lg *zerolog.Logger, responses []*execResult[R], exec failsafe.Execution[R]) (map[string]int, string, int) {
	resultCounts := make(map[string]int)
	errorCount := 0
	for _, r := range responses {
		resultHash, err := e.resultOrErrorToHash(r.result, r.err, exec)
		if err != nil || resultHash == "" {
			// Also try to hash agreed-upon errors
			if r.err != nil && e.isAgreedUponError(r.err) {
				if hash := e.errorToConsensusHash(r.err); hash != "" {
					resultCounts[hash]++
					continue
				}
			}
			errorCount++
			if r.err != nil && !e.isConsensusValidError(r.err) && !e.isAgreedUponError(r.err) {
				lg.Debug().
					Err(r.err).
					Msg("response has non-consensus/agreed-upon error")
			} else if err != nil {
				lg.Error().
					Err(err).
					Msg("failed to hash result")
			}
		} else {
			resultCounts[resultHash]++
		}
	}

	// Find the most common result
	var mostCommonResultHash string
	mostCommonResultCount := 0
	for resultHash, count := range resultCounts {
		if count > mostCommonResultCount {
			mostCommonResultHash = resultHash
			mostCommonResultCount = count
		}
	}

	return resultCounts, mostCommonResultHash, mostCommonResultCount
}

// findResultByHash finds a successful result that matches the given hash.
// This function is only called after error consensus has been handled,
// so it only needs to look for successful results.
func (e *executor[R]) findResultByHash(responses []*execResult[R], targetHash string, exec failsafe.Execution[R]) *R {
	// First try to find a successful result with matching hash
	for _, r := range responses {
		if r.err == nil {
			resultHash, err := e.resultToHash(r.result, exec)
			if err != nil {
				continue
			}
			if resultHash == targetHash {
				return &r.result
			}
		}
	}

	return nil
}

// isLowParticipants determines if we have low participants based on various conditions
func (e *executor[R]) isLowParticipants(participantCount, responseCount, selectedUpstreamCount int) bool {
	if selectedUpstreamCount < e.requiredParticipants {
		return true
	}

	if participantCount < e.requiredParticipants {
		// Check if we short-circuited and would have had enough participants
		if responseCount < selectedUpstreamCount {
			potentialUniqueParticipants := participantCount + (selectedUpstreamCount - responseCount)
			return potentialUniqueParticipants < e.requiredParticipants
		}
		return true
	}

	return false
}

// logDispute logs details about a consensus dispute
func (e *executor[R]) logDispute(lg *zerolog.Logger, responses []*execResult[R], mostCommonResultCount int, exec failsafe.Execution[R]) {
	evt := lg.Warn().
		Int("maxCount", mostCommonResultCount).
		Int("threshold", e.agreementThreshold)

	for _, resp := range responses {
		upstreamID := "unknown"
		if resp != nil && resp.upstream != nil && resp.upstream.Config() != nil {
			upstreamID = resp.upstream.Config().Id
		}

		evt.Str(fmt.Sprintf("upstream%d", resp.index), upstreamID)

		if resp.err != nil {
			evt.AnErr(fmt.Sprintf("error%d", resp.index), resp.err)
			continue
		}

		if jr := e.resultToJsonRpcResponse(resp.result, exec); jr != nil {
			evt.Object(fmt.Sprintf("response%d", resp.index), jr)
		} else {
			evt.Interface(fmt.Sprintf("result%d", resp.index), resp.result)
		}
	}
	evt.Msg("consensus dispute")
}

// handleLowParticipants handles the low participants scenario based on configured behavior
func (e *executor[R]) handleLowParticipants(
	lg *zerolog.Logger,
	responses []*execResult[R],
	exec failsafe.Execution[R],
) *failsafeCommon.PolicyResult[R] {
	errFn := func() error {
		return common.NewErrConsensusLowParticipants(
			"not enough participants",
			e.extractParticipants(responses, exec),
			e.extractCauses(responses),
		)
	}

	switch e.lowParticipantsBehavior {
	case common.ConsensusLowParticipantsBehaviorReturnError:
		return e.handleReturnError(lg, errFn)
	case common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult:
		return e.handleAcceptMostCommon(lg, responses, func() error {
			return common.NewErrConsensusLowParticipants(
				"no valid results found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader:
		return e.handlePreferBlockHeadLeader(lg, responses, func() error {
			return common.NewErrConsensusLowParticipants(
				"no block head leader found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader:
		return e.handleOnlyBlockHeadLeader(lg, responses, func() error {
			return common.NewErrConsensusLowParticipants(
				"no block head leader found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	default:
		return e.handleReturnError(lg, errFn)
	}
}

// handleDispute handles the dispute scenario based on configured behavior
func (e *executor[R]) handleDispute(
	lg *zerolog.Logger,
	responses []*execResult[R],
	exec failsafe.Execution[R],
) *failsafeCommon.PolicyResult[R] {
	errFn := func() error {
		return common.NewErrConsensusDispute(
			"not enough agreement among responses",
			e.extractParticipants(responses, exec),
			e.extractCauses(responses),
		)
	}

	switch e.disputeBehavior {
	case common.ConsensusDisputeBehaviorReturnError:
		return e.handleReturnError(lg, errFn)
	case common.ConsensusDisputeBehaviorAcceptMostCommonValidResult:
		return e.handleAcceptMostCommon(lg, responses, func() error {
			return common.NewErrConsensusDispute(
				"no valid results found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusDisputeBehaviorPreferBlockHeadLeader:
		return e.handlePreferBlockHeadLeader(lg, responses, func() error {
			return common.NewErrConsensusDispute(
				"no block head leader found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusDisputeBehaviorOnlyBlockHeadLeader:
		return e.handleOnlyBlockHeadLeader(lg, responses, func() error {
			return common.NewErrConsensusDispute(
				"no block head leader found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	default:
		return e.handleReturnError(lg, errFn)
	}
}

// checkAndPunishMisbehavingUpstreams tracks misbehaving upstreams if there's a clear majority
func (e *executor[R]) checkAndPunishMisbehavingUpstreams(
	ctx context.Context,
	lg *zerolog.Logger,
	responses []*execResult[R],
	parentExecution policy.ExecutionInternal[R],
) {
	// Skip if punishment is not configured
	if e.punishMisbehavior == nil {
		return
	}

	// Get request metadata for metrics
	req := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
	method := "unknown"
	if m, err := req.Method(); err == nil {
		method = m
	}
	category := method
	networkId := req.NetworkId()
	projectId := ""
	if req.Network() != nil {
		projectId = req.Network().ProjectId()
	}
	finality := req.Finality(ctx)
	finalityStr := finality.String()

	// Start punishment span
	ctx, punishSpan := common.StartDetailSpan(ctx, "Consensus.CheckMisbehavior")
	defer punishSpan.End()

	// First, count results to find the most common one
	resultCounts := make(map[string]int)
	for _, r := range responses {
		resultHash, err := e.resultOrErrorToHash(r.result, r.err, parentExecution)
		if err != nil || resultHash == "" {
			continue
		}
		resultCounts[resultHash]++
	}

	// Find the most common result
	var mostCommonResultHash string
	mostCommonResultCount := 0
	for resultHash, count := range resultCounts {
		if count > mostCommonResultCount {
			mostCommonResultHash = resultHash
			mostCommonResultCount = count
		}
	}

	punishSpan.SetAttributes(
		attribute.Int("responses.count", len(responses)),
		attribute.Int("most_common.count", mostCommonResultCount),
		attribute.Bool("has_clear_majority", mostCommonResultCount > len(responses)/2),
	)

	// Only track misbehaving upstreams if we have a clear majority (>50%)
	if mostCommonResultCount > len(responses)/2 {
		e.trackMisbehavingUpstreams(ctx, lg, responses, resultCounts, mostCommonResultHash, parentExecution, projectId, networkId, category, finalityStr)
	}
}

func (e *executor[R]) extractParticipants(responses []*execResult[R], exec failsafe.Execution[R]) []common.ParticipantInfo {
	participants := make([]common.ParticipantInfo, 0, len(responses))
	for _, resp := range responses {
		participant := common.ParticipantInfo{}
		if resp.upstream != nil && resp.upstream.Config() != nil {
			participant.Upstream = resp.upstream.Config().Id
		}

		// Get the hash for this response (could be from result or consensus-valid error)
		if hash, err := e.resultOrErrorToHash(resp.result, resp.err, exec); err == nil && hash != "" {
			participant.ResultHash = hash
		}

		// Always include error summary if there's an error
		if resp.err != nil {
			participant.ErrSummary = common.ErrorSummary(resp.err)
		}

		participants = append(participants, participant)
	}
	return participants
}

// extractCauses extracts all errors from responses (including consensus-valid errors)
func (e *executor[R]) extractCauses(responses []*execResult[R]) []error {
	causes := make([]error, 0, len(responses))
	for _, resp := range responses {
		if resp.err != nil {
			causes = append(causes, resp.err)
		}
	}
	return causes
}

func (e *executor[R]) createRateLimiter(logger *zerolog.Logger, upstreamId string) ratelimiter.RateLimiter[any] {
	// Try to get existing limiter
	if limiter, ok := e.misbehavingUpstreamsLimiter.Load(upstreamId); ok {
		return limiter.(ratelimiter.RateLimiter[any])
	}

	logger.Info().
		Str("upstream", upstreamId).
		Uint("disputeThreshold", e.punishMisbehavior.DisputeThreshold).
		Str("disputeWindow", e.punishMisbehavior.DisputeWindow.String()).
		Msg("creating new dispute limiter")

	limiter := ratelimiter.
		BurstyBuilder[any](e.punishMisbehavior.DisputeThreshold, e.punishMisbehavior.DisputeWindow.Duration()).
		Build()

	// Use LoadOrStore to handle concurrent creation
	actual, _ := e.misbehavingUpstreamsLimiter.LoadOrStore(upstreamId, limiter)
	return actual.(ratelimiter.RateLimiter[any])
}

func (e *executor[R]) trackMisbehavingUpstreams(ctx context.Context, logger *zerolog.Logger, responses []*execResult[R], resultCounts map[string]int, mostCommonResultHash string, execution policy.ExecutionInternal[R], projectId, networkId, category, finalityStr string) {
	// Start tracking span
	_, trackingSpan := common.StartDetailSpan(ctx, "Consensus.TrackMisbehavior")
	defer trackingSpan.End()

	// Count total valid responses
	totalValidResponses := 0
	for _, count := range resultCounts {
		totalValidResponses += count
	}

	// Only proceed with punishment if we have a clear majority (>50%)
	mostCommonCount := resultCounts[mostCommonResultHash]
	if mostCommonCount <= totalValidResponses/2 {
		trackingSpan.SetAttributes(attribute.Bool("punishment.skipped", true))
		return
	}

	misbehavingCount := 0
	for _, response := range responses {
		upstream := response.upstream
		if upstream == nil {
			continue
		}

		resultHash, err := e.resultOrErrorToHash(response.result, response.err, execution)
		if err != nil || resultHash == "" {
			// Skip responses that can't be hashed (non-consensus-valid errors)
			logger.Debug().
				Err(err).
				Str("resultHash", resultHash).
				Str("mostCommonResultHash", mostCommonResultHash).
				Msg("skipping response that cannot be hashed for misbehavior tracking")
			continue
		}

		// If this result doesn't match the most common result, punish the upstream
		if resultHash != mostCommonResultHash {
			misbehavingCount++
			upstreamId := upstream.Config().Id

			// Record misbehavior detection
			telemetry.MetricConsensusMisbehaviorDetected.WithLabelValues(projectId, networkId, upstreamId, category, finalityStr).Inc()

			limiter := e.createRateLimiter(logger, upstreamId)
			if !limiter.TryAcquirePermit() {
				e.handleMisbehavingUpstream(logger, upstream, upstreamId, projectId, networkId)
			}
		}
	}

	trackingSpan.SetAttributes(
		attribute.Int("misbehaving.count", misbehavingCount),
		attribute.String("correct.hash", mostCommonResultHash),
	)
}

func (e *executor[R]) handleMisbehavingUpstream(logger *zerolog.Logger, upstream common.Upstream, upstreamId, projectId, networkId string) {
	// Create a placeholder value to claim ownership atomically
	placeholder := &struct{}{}

	// Try to claim ownership of punishing this upstream
	if _, loaded := e.misbehavingUpstreamsSitoutTimer.LoadOrStore(upstreamId, placeholder); loaded {
		logger.Debug().
			Str("upstream", upstreamId).
			Msg("upstream already in sitout, skipping")
		return
	}

	logger.Warn().
		Str("upstream", upstreamId).
		Msg("misbehaviour limit exhausted, punishing upstream")

	// Record punishment metric
	telemetry.MetricConsensusUpstreamPunished.WithLabelValues(projectId, networkId, upstreamId).Inc()

	// Cordon the upstream first
	upstream.Cordon("*", "misbehaving in consensus")

	// Create the timer
	timer := time.AfterFunc(e.punishMisbehavior.SitOutPenalty.Duration(), func() {
		upstream.Uncordon("*")
		e.misbehavingUpstreamsSitoutTimer.Delete(upstreamId)
	})

	// Replace the placeholder with the actual timer
	e.misbehavingUpstreamsSitoutTimer.Store(upstreamId, timer)
}

func (e *executor[R]) handleReturnError(logger *zerolog.Logger, errFn func() error) *failsafeCommon.PolicyResult[R] {
	logger.Debug().Msg("sending error result")

	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}

func (e *executor[R]) handleAcceptMostCommon(logger *zerolog.Logger, responses []*execResult[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	logger.Debug().Msg("sending most common valid result")

	// First try to find a successful result
	var finalResult R
	foundValidResult := false
	for _, r := range responses {
		if r.err == nil {
			finalResult = r.result
			foundValidResult = true
			break
		}
	}

	if foundValidResult {
		return &failsafeCommon.PolicyResult[R]{
			Result: finalResult,
		}
	}

	// If no successful result, check for consensus-valid errors
	for _, r := range responses {
		if r.err != nil && e.isConsensusValidError(r.err) {
			logger.Debug().Err(r.err).Msg("returning consensus-valid error as result")
			return &failsafeCommon.PolicyResult[R]{
				Error: r.err,
			}
		}
	}

	// No valid results found
	logger.Debug().Interface("responses", responses).Msg("no valid results found, sending error")
	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}

func (e *executor[R]) findBlockHeadLeader(responses []*execResult[R]) (int64, *execResult[R]) {
	var highestBlock int64
	var highestBlockResult *execResult[R]
	for _, r := range responses {
		if r.err == nil {
			upstream := r.upstream
			if upstream != nil {
				if evmUpstream, ok := upstream.(common.EvmUpstream); ok {
					statePoller := evmUpstream.EvmStatePoller()
					if statePoller != nil {
						latestBlock := statePoller.LatestBlock()
						if latestBlock > highestBlock {
							highestBlock = latestBlock
							highestBlockResult = r
						}
					}
				} else {
					e.logger.Warn().
						Str("upstream", upstream.Config().Id).
						Msg("upstream does not support block head leader detection")
				}
			}
		}
	}
	return highestBlock, highestBlockResult
}

func (e *executor[R]) handlePreferBlockHeadLeader(logger *zerolog.Logger, responses []*execResult[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	_, highestBlockResult := e.findBlockHeadLeader(responses)

	if highestBlockResult != nil {
		logger.Debug().Msg("sending block head leader result")

		if highestBlockResult.err != nil {
			return &failsafeCommon.PolicyResult[R]{
				Error: highestBlockResult.err,
			}
		}

		return &failsafeCommon.PolicyResult[R]{
			Result: highestBlockResult.result,
		}
	}

	// Fall back to most common result
	return e.handleAcceptMostCommon(logger, responses, errFn)
}

func (e *executor[R]) handleOnlyBlockHeadLeader(logger *zerolog.Logger, responses []*execResult[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	_, highestBlockResult := e.findBlockHeadLeader(responses)

	if highestBlockResult != nil {
		logger.Debug().Msg("sending block head leader result")
		return &failsafeCommon.PolicyResult[R]{
			Result: highestBlockResult.result,
		}
	}

	// If no block head leader found, return error
	logger.Debug().Msg("sending no block head leader error")

	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}

// selectUpstreams chooses which upstreams will participate in the consensus round based
// on the configured policy and the currently available upstream list. The logic is
// adapted from the former Network.runConsensus implementation so that the decision
// lives entirely inside the consensus executor.
func (e *executor[R]) selectUpstreams(upsList []common.Upstream) []common.Upstream {
	if len(upsList) == 0 {
		return nil
	}

	required := e.requiredParticipants
	if required <= 0 {
		required = 2
	}

	selected := upsList
	if len(selected) > required {
		selected = selected[:required]
	}

	// If behaviour cares about block-head leader, make sure it is in the set.
	if e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader ||
		e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader {
		leader := findBlockHeadLeaderUpstream(upsList)
		if leader != nil {
			found := false
			for _, u := range selected {
				if u == leader {
					found = true
					break
				}
			}
			if !found {
				if e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader {
					selected = []common.Upstream{leader}
				} else { // prefer -> swap last
					if len(selected) > 0 {
						selected[len(selected)-1] = leader
					} else {
						selected = []common.Upstream{leader}
					}
				}
			}
		}
	}

	return selected
}

// findBlockHeadLeaderUpstream inspects the provided upstream list and returns the
// upstream with the highest latest block number (best head). When no suitable
// upstream can be found it returns nil.
func findBlockHeadLeaderUpstream(ups []common.Upstream) common.Upstream {
	var leader common.Upstream
	var highest int64
	for _, u := range ups {
		if evmUp, ok := u.(common.EvmUpstream); ok {
			sp := evmUp.EvmStatePoller()
			if sp == nil {
				continue
			}
			lb := sp.LatestBlock()
			if lb > highest {
				highest = lb
				leader = u
			}
		}
	}
	return leader
}
