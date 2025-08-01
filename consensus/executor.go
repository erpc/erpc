package consensus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	result         R
	err            error
	index          int
	upstream       common.Upstream
	cachedEmptyish *bool // Cache for IsResultEmptyish
}

// metricsLabels contains pre-extracted labels for metrics to avoid repeated extraction
type metricsLabels struct {
	method      string
	category    string
	networkId   string
	projectId   string
	finalityStr string
}

// extractMetricsLabels extracts metrics labels from request to avoid repeated extraction
func (e *executor[R]) extractMetricsLabels(ctx context.Context, req *common.NormalizedRequest) metricsLabels {
	method := "unknown"
	if m, err := req.Method(); err == nil {
		method = m
	}

	projectId := ""
	if req.Network() != nil {
		projectId = req.Network().ProjectId()
	}

	return metricsLabels{
		method:      method,
		category:    method, // category is same as method
		networkId:   req.NetworkId(),
		projectId:   projectId,
		finalityStr: req.Finality(ctx).String(),
	}
}

// resultToHash converts a result to a string representation for comparison
func (e *executor[R]) resultToHash(result R, exec failsafe.Execution[R]) (string, error) {
	jr := e.resultToJsonRpcResponse(result, exec)
	if jr == nil {
		return "", fmt.Errorf("no json-rpc response available on result")
	}

	// Check if this consensus policy has ignore fields configured
	if e.consensusPolicy.ignoreFields != nil {
		// Extract method from the request context
		val := exec.Context().Value(common.RequestContextKey)
		if originalReq, ok := val.(*common.NormalizedRequest); ok && originalReq != nil {
			if method, err := originalReq.Method(); err == nil {
				if fields, ok := e.consensusPolicy.ignoreFields[method]; ok && len(fields) > 0 {
					return jr.CanonicalHashWithIgnoredFields(fields, exec.Context())
				}
			}
		}
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

		// Extract metrics labels once
		labels := e.extractMetricsLabels(ctx, originalReq)

		// Start main consensus span
		ctx, consensusSpan := common.StartSpan(ctx, "Consensus.Apply",
			trace.WithAttributes(
				attribute.String("network.id", labels.networkId),
				attribute.String("request.method", labels.method),
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
			Str("networkId", labels.networkId).
			Logger()

		parentExecution := exec.(policy.ExecutionInternal[R])

		// Phase 1: Fire off requests and collect responses
		responses := e.collectResponses(ctx, &lg, originalReq, labels, parentExecution, innerFn)

		// Phase 2: Evaluate consensus
		result := e.evaluateConsensus(ctx, &lg, originalReq, labels, responses, exec)

		// Phase 3: Track misbehaving upstreams (only if we have a clear majority)
		e.checkAndPunishMisbehavingUpstreams(ctx, &lg, originalReq, labels, responses, parentExecution)

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
		telemetry.MetricConsensusTotal.WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr).Inc()
		telemetry.MetricConsensusDuration.WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr).Observe(duration)

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
	labels metricsLabels,
	parentExecution policy.ExecutionInternal[R],
	innerFn func(failsafe.Execution[R]) *failsafeCommon.PolicyResult[R],
) []*execResult[R] {

	// Start collection span
	ctx, collectionSpan := common.StartDetailSpan(ctx, "Consensus.CollectResponses",
		trace.WithAttributes(
			attribute.Int("participants.required", e.requiredParticipants),
		),
	)
	defer collectionSpan.End()

	// Context for canceling remaining requests
	cancellableCtx, cancelRemaining := context.WithCancel(ctx)
	defer cancelRemaining()

	// Channel to collect responses - buffered to prevent blocking
	responseChan := make(chan *execResult[R], e.requiredParticipants)

	// Start required number of participants
	actualGoroutineCount := e.requiredParticipants
	for execIdx := 0; execIdx < e.requiredParticipants; execIdx++ {
		lg.Debug().
			Interface("id", originalReq.ID()).
			Int("index", execIdx).
			Msg("launching consensus participant")

		// Use the original request directly - no cloning!
		execution := parentExecution.CopyForCancellableWithValue(
			common.RequestContextKey,
			originalReq,
		).(policy.ExecutionInternal[R])

		// Fire off the request
		go func(consensusExec policy.ExecutionInternal[R], execIdx int) {
			// Start a span for this consensus attempt
			attemptCtx, attemptSpan := common.StartDetailSpan(cancellableCtx, "Consensus.Attempt",
				trace.WithAttributes(
					attribute.Int("attempt.index", execIdx),
				),
			)
			defer attemptSpan.End()

			// Add panic recovery
			defer func() {
				if r := recover(); r != nil {
					lg.Error().
						Int("index", execIdx).
						Interface("panic", r).
						Msg("panic in consensus execution")

					panicErr := fmt.Errorf("panic in consensus execution: %v", r)
					common.SetTraceSpanError(attemptSpan, panicErr)

					// Record panic metric
					telemetry.MetricConsensusPanics.WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr).Inc()

					// Send error result
					select {
					case responseChan <- &execResult[R]{
						err:   panicErr,
						index: execIdx,
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
				lg.Debug().Int("index", execIdx).Msg("skipping execution, context cancelled")
				attemptSpan.SetAttributes(attribute.Bool("cancelled_before_execution", true))
				attemptSpan.SetStatus(codes.Error, "cancelled before execution")
				telemetry.MetricConsensusCancellations.WithLabelValues(labels.projectId, labels.networkId, labels.category, "before_execution", labels.finalityStr).Inc()
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
				lg.Debug().Int("index", execIdx).Msg("context cancelled before execution")
				attemptSpan.SetAttributes(attribute.Bool("cancelled_before_execution", true))
				attemptSpan.SetStatus(codes.Error, "cancelled before execution")
				telemetry.MetricConsensusCancellations.WithLabelValues(labels.projectId, labels.networkId, labels.category, "before_execution", labels.finalityStr).Inc()
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
				lg.Debug().Int("index", execIdx).Msg("discarding result, context cancelled after execution")
				attemptSpan.SetAttributes(attribute.Bool("cancelled_after_execution", true))
				attemptSpan.SetStatus(codes.Error, "cancelled after execution")
				telemetry.MetricConsensusCancellations.WithLabelValues(labels.projectId, labels.networkId, labels.category, "after_execution", labels.finalityStr).Inc()
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

			// Extract upstream from response (if available)
			var upstream common.Upstream
			if resp, ok := any(result.Result).(*common.NormalizedResponse); ok {
				upstream = resp.Upstream()
				if upstream != nil {
					attemptSpan.SetAttributes(attribute.String("upstream.id", upstream.Id()))
				}
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
				lg.Debug().Int("index", execIdx).Msg("discarding response due to context cancellation")
				attemptSpan.SetAttributes(attribute.Bool("response_discarded", true))
				telemetry.MetricConsensusCancellations.WithLabelValues(labels.projectId, labels.networkId, labels.category, "after_execution", labels.finalityStr).Inc()
				select {
				case responseChan <- nil:
				case <-attemptCtx.Done():
					// Already cancelled, safe to exit
				}
			}
		}(execution, execIdx)
	}

	// Collect responses with short-circuit logic
	responses := make([]*execResult[R], 0, e.requiredParticipants)
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
			if resp != nil && !shortCircuited && e.checkShortCircuit(lg, responses, parentExecution) {
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
				telemetry.MetricConsensusShortCircuit.WithLabelValues(labels.projectId, labels.networkId, labels.category, shortCircuitReason, labels.finalityStr).Inc()

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
			telemetry.MetricConsensusCancellations.WithLabelValues(labels.projectId, labels.networkId, labels.category, "collection", labels.finalityStr).Inc()
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
	telemetry.MetricConsensusResponsesCollected.WithLabelValues(labels.projectId, labels.networkId, labels.category, shortCircuitedStr, labels.finalityStr).Observe(float64(len(responses)))

	return responses
}

// isResultEmptyish checks if a result is emptyish, using cached value if available
func (e *executor[R]) isResultEmptyish(r *execResult[R], ctx context.Context) bool {
	// Return false for errors
	if r.err != nil {
		return false
	}

	// Check cache first
	if r.cachedEmptyish != nil {
		return *r.cachedEmptyish
	}

	// Calculate and cache
	resp, ok := any(r.result).(*common.NormalizedResponse)
	if !ok {
		// Not a normalized response, consider not emptyish
		emptyish := false
		r.cachedEmptyish = &emptyish
		return false
	}

	emptyish := resp.IsResultEmptyish(ctx)
	r.cachedEmptyish = &emptyish
	return emptyish
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
func (e *executor[R]) checkShortCircuit(lg *zerolog.Logger, responses []*execResult[R], exec policy.ExecutionInternal[R]) bool {
	// Count results by hash, always preferring non-empty results
	resultCounts := make(map[string]int)
	emptyishHashes := make(map[string]bool)

	for _, r := range responses {
		resultHash, err := e.resultOrErrorToHash(r.result, r.err, exec)
		if err != nil || resultHash == "" {
			continue
		}
		resultCounts[resultHash]++

		// Check if this result is emptyish (only for successful responses)
		if r.err == nil {
			emptyishHashes[resultHash] = e.isResultEmptyish(r, exec.Context())
		}
	}

	// Separate empty and non-empty results
	nonEmptyResults := make(map[string]int)
	emptyResults := make(map[string]int)

	for resultHash, count := range resultCounts {
		if emptyishHashes[resultHash] {
			emptyResults[resultHash] = count
		} else {
			nonEmptyResults[resultHash] = count
		}
	}

	// Always prefer non-empty results if they exist
	var activeResults map[string]int
	if len(nonEmptyResults) > 0 {
		activeResults = nonEmptyResults
	} else {
		activeResults = emptyResults
	}

	// Find the most common result from the active results
	mostCommonHash := ""
	mostCommonCount := 0
	var mostCommonIsEmpty bool

	for resultHash, count := range activeResults {
		if count > mostCommonCount {
			mostCommonHash = resultHash
			mostCommonCount = count
			mostCommonIsEmpty = emptyishHashes[resultHash]
		}
	}

	// Check if consensus is reached
	if mostCommonCount >= e.agreementThreshold {
		// Check if the consensus result is emptyish - if so, avoid short-circuiting
		// to allow more responses that might contain meaningful data
		if mostCommonIsEmpty {
			lg.Debug().
				Int("responseCount", len(responses)).
				Int("mostCommonCount", mostCommonCount).
				Int("threshold", e.agreementThreshold).
				Str("consensusHash", mostCommonHash).
				Bool("consensusIsEmpty", mostCommonIsEmpty).
				Msg("consensus reached on emptyish result, avoiding short-circuit to collect more data")
			return false
		}

		// Consensus reached on non-empty result - short-circuit
		lg.Debug().
			Int("responseCount", len(responses)).
			Int("mostCommonCount", mostCommonCount).
			Int("threshold", e.agreementThreshold).
			Str("consensusHash", mostCommonHash).
			Bool("consensusIsEmpty", mostCommonIsEmpty).
			Msg("consensus reached on non-empty result, short-circuiting")
		return true
	} else if len(emptyResults) > 0 {
		// Check if we would have consensus on empty results
		// This handles the case where we have both empty and non-empty results
		// but we're preferring non-empty (which might not meet threshold yet)
		for _, count := range emptyResults {
			if count >= e.agreementThreshold {
				lg.Debug().
					Int("responseCount", len(responses)).
					Int("emptyConsensusCount", count).
					Int("threshold", e.agreementThreshold).
					Bool("hasNonEmptyResults", len(nonEmptyResults) > 0).
					Msg("would have consensus on empty results, avoiding short-circuit to wait for more non-empty data")
				return false
			}
		}
	}

	// Check if consensus is impossible
	remainingResponses := e.requiredParticipants - len(responses)

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
	req *common.NormalizedRequest,
	labels metricsLabels,
	responses []*execResult[R],
	exec failsafe.Execution[R],
) *failsafeCommon.PolicyResult[R] {
	// Start evaluation span
	ctx, evalSpan := common.StartDetailSpan(ctx, "Consensus.Evaluate",
		trace.WithAttributes(
			attribute.Int("responses.count", len(responses)),
			attribute.Int("participants.required", e.requiredParticipants),
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
		telemetry.MetricConsensusAgreementCount.WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr).Observe(float64(mostCommonResultCount))
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
		Int("required", len(responses)).
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
			telemetry.MetricConsensusErrors.WithLabelValues(labels.projectId, labels.networkId, labels.category, "consensus_on_error", labels.finalityStr).Inc()

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

	isLowParticipants := e.isLowParticipants(participantCount)
	if isLowParticipants {
		lg.Debug().
			Int("participantCount", participantCount).
			Int("responses", len(responses)).
			Int("required", e.requiredParticipants).
			Msg("handling low participants")

		evalSpan.SetAttributes(
			attribute.Bool("consensus.low_participants", true),
			attribute.String("low_participants.behavior", string(e.lowParticipantsBehavior)),
		)

		result := e.handleLowParticipants(lg, responses, exec)
		if result.Error != nil {
			common.SetTraceSpanError(evalSpan, result.Error)
			telemetry.MetricConsensusErrors.WithLabelValues(labels.projectId, labels.networkId, labels.category, "low_participants", labels.finalityStr).Inc()
		}
		return result
	}

	// Consensus dispute
	e.logDispute(lg, req, responses, mostCommonResultCount, exec)

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
				telemetry.MetricConsensusErrors.WithLabelValues(labels.projectId, labels.networkId, labels.category, "agreed_error", labels.finalityStr).Inc()
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
		telemetry.MetricConsensusErrors.WithLabelValues(labels.projectId, labels.networkId, labels.category, "dispute", labels.finalityStr).Inc()
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
// It always prefers non-empty results over empty ones, regardless of count
func (e *executor[R]) countResponsesByHash(lg *zerolog.Logger, responses []*execResult[R], exec failsafe.Execution[R]) (map[string]int, string, int) {
	resultCounts := make(map[string]int)
	emptyishHashes := make(map[string]bool) // Track which hashes correspond to emptyish results
	errorCount := 0

	for _, r := range responses {
		resultHash, err := e.resultOrErrorToHash(r.result, r.err, exec)
		if err != nil || resultHash == "" {
			// Also try to hash agreed-upon errors
			if r.err != nil && e.isAgreedUponError(r.err) {
				if hash := e.errorToConsensusHash(r.err); hash != "" {
					resultCounts[hash]++
					// Errors are not considered emptyish for preference purposes
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

			// Check if this result is emptyish (only for successful responses)
			if r.err == nil {
				emptyishHashes[resultHash] = e.isResultEmptyish(r, exec.Context())
			}
		}
	}

	// Separate empty and non-empty results
	nonEmptyResults := make(map[string]int)
	emptyResults := make(map[string]int)

	for resultHash, count := range resultCounts {
		if emptyishHashes[resultHash] {
			emptyResults[resultHash] = count
		} else {
			nonEmptyResults[resultHash] = count
		}
	}

	// Always prefer non-empty results if they exist
	var activeResults map[string]int
	var preferenceReason string

	if len(nonEmptyResults) > 0 {
		activeResults = nonEmptyResults
		preferenceReason = "using non-empty results (ignoring empty responses)"
		if len(emptyResults) > 0 {
			lg.Debug().
				Interface("nonEmptyResults", nonEmptyResults).
				Interface("emptyResults", emptyResults).
				Msg("preferring non-empty results over empty results regardless of count")
		}
	} else {
		activeResults = emptyResults
		preferenceReason = "using empty results (no non-empty responses available)"
	}

	// Find the most common result from the active results
	var mostCommonResultHash string
	mostCommonResultCount := 0

	for resultHash, count := range activeResults {
		if count > mostCommonResultCount {
			mostCommonResultHash = resultHash
			mostCommonResultCount = count
		}
	}

	if mostCommonResultHash != "" {
		lg.Debug().
			Str("reason", preferenceReason).
			Str("mostCommonHash", mostCommonResultHash).
			Int("mostCommonCount", mostCommonResultCount).
			Bool("isEmptyish", emptyishHashes[mostCommonResultHash]).
			Msg("selected most common result with non-empty preference")
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

// isLowParticipants determines if we have enough valid responses from participants
func (e *executor[R]) isLowParticipants(participantCount int) bool {
	if participantCount < e.agreementThreshold {
		return true
	}

	if participantCount < e.requiredParticipants {
		return true
	}

	return false
}

// logDispute logs details about a consensus dispute
func (e *executor[R]) logDispute(lg *zerolog.Logger, req *common.NormalizedRequest, responses []*execResult[R], mostCommonResultCount int, exec failsafe.Execution[R]) {
	method, _ := req.Method()

	evt := lg.WithLevel(e.disputeLogLevel).
		Str("method", method).
		Object("request", req).
		Int("maxCount", mostCommonResultCount).
		Int("threshold", e.agreementThreshold).
		Int("totalResponses", len(responses))

	// Count responses by hash for the dispute log
	resultHashCounts := make(map[string]int)
	hashToUpstreams := make(map[string][]string)

	for _, resp := range responses {
		upstreamID := "unknown"
		if resp != nil && resp.upstream != nil && resp.upstream.Config() != nil {
			upstreamID = resp.upstream.Config().Id
		}

		evt.Str(fmt.Sprintf("upstream%d", resp.index), upstreamID)

		if resp.err != nil {
			evt.AnErr(fmt.Sprintf("error%d", resp.index), resp.err)

			// Get hash for errors that are consensus-valid
			if e.isConsensusValidError(resp.err) {
				hash := e.errorToConsensusHash(resp.err)
				if hash != "" {
					evt.Str(fmt.Sprintf("errorHash%d", resp.index), hash)
					resultHashCounts[hash]++
					hashToUpstreams[hash] = append(hashToUpstreams[hash], upstreamID)
				}
			}
			continue
		}

		// Log successful response details
		if jr := e.resultToJsonRpcResponse(resp.result, exec); jr != nil {
			evt.Object(fmt.Sprintf("response%d", resp.index), jr)

			// Include the hash for successful responses
			if hash, err := e.resultToHash(resp.result, exec); err == nil && hash != "" {
				evt.Str(fmt.Sprintf("hash%d", resp.index), hash)
				resultHashCounts[hash]++
				hashToUpstreams[hash] = append(hashToUpstreams[hash], upstreamID)
			}
		} else {
			evt.Interface(fmt.Sprintf("result%d", resp.index), resp.result)
		}
	}

	// Log the hash distribution
	evt.Interface("hashDistribution", resultHashCounts)
	evt.Interface("hashToUpstreams", hashToUpstreams)

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
		return e.handleAcceptMostCommon(exec.Context(), lg, responses, exec, func() error {
			return common.NewErrConsensusLowParticipants(
				"no clear most common result",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusLowParticipantsBehaviorAcceptAnyValidResult:
		return e.handleAcceptAnyValid(exec.Context(), lg, responses, func() error {
			return common.NewErrConsensusLowParticipants(
				"no valid results found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader:
		return e.handlePreferBlockHeadLeader(exec.Context(), lg, responses, exec, func() error {
			return common.NewErrConsensusLowParticipants(
				"no block head leader found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader:
		return e.handleOnlyBlockHeadLeader(exec.Context(), lg, responses, func() error {
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
		return e.handleAcceptMostCommon(exec.Context(), lg, responses, exec, func() error {
			return common.NewErrConsensusDispute(
				"no clear most common result",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusDisputeBehaviorAcceptAnyValidResult:
		return e.handleAcceptAnyValid(exec.Context(), lg, responses, func() error {
			return common.NewErrConsensusDispute(
				"no valid results found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusDisputeBehaviorPreferBlockHeadLeader:
		return e.handlePreferBlockHeadLeader(exec.Context(), lg, responses, exec, func() error {
			return common.NewErrConsensusDispute(
				"no block head leader found",
				e.extractParticipants(responses, exec),
				e.extractCauses(responses),
			)
		})
	case common.ConsensusDisputeBehaviorOnlyBlockHeadLeader:
		return e.handleOnlyBlockHeadLeader(exec.Context(), lg, responses, func() error {
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
	req *common.NormalizedRequest,
	labels metricsLabels,
	responses []*execResult[R],
	parentExecution policy.ExecutionInternal[R],
) {
	// Skip if punishment is not configured or has invalid values
	if e.punishMisbehavior == nil ||
		e.punishMisbehavior.DisputeThreshold == 0 ||
		e.punishMisbehavior.DisputeWindow.Duration() == 0 {
		return
	}

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
		e.trackMisbehavingUpstreams(ctx, lg, req, labels, responses, resultCounts, mostCommonResultHash, parentExecution)
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

func (e *executor[R]) trackMisbehavingUpstreams(ctx context.Context, logger *zerolog.Logger, req *common.NormalizedRequest, labels metricsLabels, responses []*execResult[R], resultCounts map[string]int, mostCommonResultHash string, execution policy.ExecutionInternal[R]) {
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

			// Log warning with detailed comparison
			e.logMisbehavingUpstreamWarning(logger, req, response, responses, mostCommonResultHash, upstreamId, execution)

			// Record misbehavior detection
			emptyish := "n/a"
			if response != nil && response.err == nil {
				emptyish = strconv.FormatBool(e.isResultEmptyish(response, ctx))
			}
			telemetry.MetricConsensusMisbehaviorDetected.WithLabelValues(labels.projectId, labels.networkId, upstreamId, labels.category, labels.finalityStr, emptyish).Inc()

			limiter := e.createRateLimiter(logger, upstreamId)
			if !limiter.TryAcquirePermit() {
				e.handleMisbehavingUpstream(logger, upstream, upstreamId, labels.projectId, labels.networkId)
			}
		}
	}

	trackingSpan.SetAttributes(
		attribute.Int("misbehaving.count", misbehavingCount),
		attribute.String("correct.hash", mostCommonResultHash),
	)
}

func (e *executor[R]) logMisbehavingUpstreamWarning(logger *zerolog.Logger, req *common.NormalizedRequest, misbehavingResponse *execResult[R], allResponses []*execResult[R], correctHash string, upstreamId string, execution policy.ExecutionInternal[R]) {

	// Find a correct response to compare against
	var correctResponse *execResult[R]
	for _, r := range allResponses {
		if r.err == nil {
			if hash, err := e.resultToHash(r.result, execution); err == nil && hash == correctHash {
				correctResponse = r
				break
			}
		} else if e.isConsensusValidError(r.err) {
			if hash := e.errorToConsensusHash(r.err); hash == correctHash {
				correctResponse = r
				break
			}
		}
	}

	// Build the warning log
	evt := logger.Warn().
		Str("upstream", upstreamId).
		Object("request", req)

	// Add request method if available
	if method, err := req.Method(); err == nil {
		evt = evt.Str("method", method)
	}

	// Add misbehaving response details
	if misbehavingResponse.err != nil {
		evt = evt.AnErr("misbehaving_error", misbehavingResponse.err)
		evt = evt.Str("misbehaving_error_hash", e.errorToConsensusHash(misbehavingResponse.err))
	} else {
		if jr := e.resultToJsonRpcResponse(misbehavingResponse.result, execution); jr != nil {
			evt = evt.Object("misbehaving_response", jr)
		}
		if hash, err := e.resultToHash(misbehavingResponse.result, execution); err == nil {
			evt = evt.Str("misbehaving_response_hash", hash)
		}
	}

	// Add correct response details for comparison
	if correctResponse != nil {
		evt = evt.Str("correct_upstream", "unknown")
		if correctResponse.upstream != nil && correctResponse.upstream.Config() != nil {
			evt = evt.Str("correct_upstream", correctResponse.upstream.Config().Id)
		}

		if correctResponse.err != nil {
			evt = evt.AnErr("correct_error", correctResponse.err)
			evt = evt.Str("correct_error_hash", e.errorToConsensusHash(correctResponse.err))
		} else {
			if jr := e.resultToJsonRpcResponse(correctResponse.result, execution); jr != nil {
				evt = evt.Object("correct_response", jr)
			}
			if hash, err := e.resultToHash(correctResponse.result, execution); err == nil {
				evt = evt.Str("correct_response_hash", hash)
			}
		}
	}

	evt = evt.Str("correct_hash", correctHash)
	evt.Msg("upstream response disagrees with consensus")
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

func (e *executor[R]) handleAcceptMostCommon(ctx context.Context, logger *zerolog.Logger, responses []*execResult[R], exec failsafe.Execution[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	logger.Debug().Msg("finding most common valid result preferring non-empty")

	// Separate empty and non-empty results
	nonEmptyResults := make(map[string]int)
	emptyResults := make(map[string]int)
	resultsByHash := make(map[string]R)

	for _, r := range responses {
		if r.err == nil {
			// Use the same hash calculation as the main consensus logic
			hash, err := e.resultToHash(r.result, exec)
			if err != nil || hash == "" {
				continue
			}

			// Check if result is emptyish
			isEmptyish := e.isResultEmptyish(r, ctx)

			if isEmptyish {
				emptyResults[hash]++
			} else {
				nonEmptyResults[hash]++
			}

			if _, exists := resultsByHash[hash]; !exists {
				resultsByHash[hash] = r.result
			}
		}
	}

	// Always prefer non-empty results if they exist
	var activeResults map[string]int
	hasNonEmptyResults := len(nonEmptyResults) > 0

	if hasNonEmptyResults {
		activeResults = nonEmptyResults
		logger.Debug().
			Interface("nonEmptyResults", nonEmptyResults).
			Interface("emptyResults", emptyResults).
			Msg("preferring non-empty results in dispute resolution")
	} else {
		activeResults = emptyResults
		logger.Debug().
			Interface("emptyResults", emptyResults).
			Msg("only empty results available")
	}

	// Find the most common result(s) from active results
	var maxCount int
	var mostCommonHashes []string

	for hash, count := range activeResults {
		if count > maxCount {
			maxCount = count
			mostCommonHashes = []string{hash}
		} else if count == maxCount {
			mostCommonHashes = append(mostCommonHashes, hash)
		}
	}

	// If there's a clear winner, return it
	if len(mostCommonHashes) == 1 && maxCount > 0 {
		logger.Debug().
			Str("hash", mostCommonHashes[0]).
			Int("count", maxCount).
			Msg("found clear most common result")
		return &failsafeCommon.PolicyResult[R]{
			Result: resultsByHash[mostCommonHashes[0]],
		}
	}

	// If there are multiple results with the same count, it's ambiguous
	if len(mostCommonHashes) > 1 {
		logger.Debug().
			Int("numTiedResults", len(mostCommonHashes)).
			Int("count", maxCount).
			Msg("multiple results have the same count, cannot determine most common")
		return &failsafeCommon.PolicyResult[R]{
			Error: errFn(),
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

func (e *executor[R]) handleAcceptAnyValid(ctx context.Context, logger *zerolog.Logger, responses []*execResult[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	logger.Debug().Msg("accepting any valid result (prefer non-empty)")

	// Only look for non-empty successful results
	// For acceptAnyValidResult in dispute scenarios, empty results are not considered "valid"
	for _, r := range responses {
		if r.err == nil && !e.isResultEmptyish(r, ctx) {
			// Found non-empty result, use it immediately
			upstreamId := ""
			if r.upstream != nil && r.upstream.Config() != nil {
				upstreamId = r.upstream.Config().Id
			}
			logger.Debug().
				Str("upstream", upstreamId).
				Msg("found non-empty result")
			return &failsafeCommon.PolicyResult[R]{
				Result: r.result,
			}
		}
	}

	logger.Debug().Msg("no non-empty results found, empty results not valid for acceptAnyValidResult in dispute")

	// If no successful result, check for consensus-valid errors
	for _, r := range responses {
		if r.err != nil && e.isConsensusValidError(r.err) {
			logger.Debug().Err(r.err).Msg("returning consensus-valid error")
			return &failsafeCommon.PolicyResult[R]{
				Error: r.err,
			}
		}
	}

	// No valid result found
	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}

func (e *executor[R]) findBlockHeadLeaderResult(ctx context.Context, responses []*execResult[R]) *execResult[R] {
	var network common.Network
	for _, r := range responses {
		if resp, ok := any(r.result).(*common.NormalizedResponse); ok {
			if req := resp.Request(); req != nil {
				if n := req.Network(); n != nil && n.Id() != "" {
					network = n
					break
				}
			}
		}
	}
	if network == nil {
		return nil
	}

	leaderUpstream := network.EvmLeaderUpstream(ctx)
	if leaderUpstream == nil {
		return nil
	}
	for _, r := range responses {
		if r.err == nil {
			if r.upstream == leaderUpstream {
				return r
			}
		}
	}
	return nil
}

func (e *executor[R]) handlePreferBlockHeadLeader(ctx context.Context, logger *zerolog.Logger, responses []*execResult[R], exec failsafe.Execution[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	blockLeaderResult := e.findBlockHeadLeaderResult(ctx, responses)
	if blockLeaderResult != nil {
		logger.Debug().Msg("sending block head leader result")
		if blockLeaderResult.err != nil {
			return &failsafeCommon.PolicyResult[R]{
				Error: blockLeaderResult.err,
			}
		}
		return &failsafeCommon.PolicyResult[R]{
			Result: blockLeaderResult.result,
		}
	}

	// Fall back to most common result
	return e.handleAcceptMostCommon(ctx, logger, responses, exec, errFn)
}

func (e *executor[R]) handleOnlyBlockHeadLeader(ctx context.Context, logger *zerolog.Logger, responses []*execResult[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	blockLeaderResult := e.findBlockHeadLeaderResult(ctx, responses)
	if blockLeaderResult != nil {
		logger.Debug().Msg("sending block head leader result")
		return &failsafeCommon.PolicyResult[R]{
			Result: blockLeaderResult.result,
		}
	}
	logger.Debug().Msg("no block head leader found, returning error")
	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}
