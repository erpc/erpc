package consensus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
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

// Static error instances to avoid allocations in hot paths
var (
	errNoJsonRpcResponse = errors.New("no json-rpc response available on result")
	errNotConsensusValid = errors.New("error is not consensus-valid")
	errPanicInConsensus  = errors.New("panic in consensus execution")
)

// executor is a policy.Executor that handles failures according to a ConsensusPolicy.
type executor[R any] struct {
	*policy.BaseExecutor[R]
	*consensusPolicy[R]

	// Sync pools for hot path map allocations
	simpleMapsPool    sync.Pool // interface{} - stores map[string]int and map[string]bool
	resultsByHashPool sync.Pool // map[string]R
}

var _ policy.Executor[any] = &executor[any]{}

type execResult[R any] struct {
	result         R
	err            error
	index          int
	upstream       common.Upstream
	cachedEmptyish *bool // Cache for IsResultEmptyish
}

// responseGroup holds all responses with the same hash
type responseGroup[R any] struct {
	count       int
	isEmpty     bool
	isError     bool
	responses   []*execResult[R]
	firstResult R     // Cached first successful result
	firstError  error // Cached first error (for error consensus)
	hasResult   bool  // Whether firstResult has been set
}

// consensusAnalysis contains all analyzed consensus data computed once
type consensusAnalysis[R any] struct {
	// Winner info (after applying non-empty preference)
	winnerHash  string
	winnerCount int
	winnerGroup *responseGroup[R]

	// Raw response groups by hash
	responsesByHash map[string]*responseGroup[R]

	// Categories
	totalResponses         int
	totalValidParticipants int
	nonEmptySuccessCount   int
	emptySuccessCount      int
	consensusErrorCount    int
	nonConsensusErrorCount int
	isLowParticipants      bool

	// Quick access to all groups by category
	nonEmptyGroups map[string]*responseGroup[R]
	emptyGroups    map[string]*responseGroup[R]
	errorGroups    map[string]*responseGroup[R]
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
		return "", errNoJsonRpcResponse
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
					hash = hash + ":" + strconv.Itoa(int(jre.NormalizedCode()))
				}
			}

			return hash
		}
	}

	// For other errors that might have JsonRpcExceptionInternal
	var jre *common.ErrJsonRpcExceptionInternal
	if errors.As(err, &jre) {
		// Use normalized code for JSON-RPC errors
		return "jsonrpc:" + strconv.Itoa(int(jre.NormalizedCode()))
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
		return "", errNotConsensusValid
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

		// Phase 2: Evaluate consensus (includes analysis)
		result, analysis := e.evaluateConsensus(ctx, &lg, originalReq, labels, responses, exec)

		// Phase 3: Track misbehaving upstreams (only if we have a clear majority)
		e.checkAndPunishMisbehavingUpstreams(ctx, &lg, originalReq, labels, analysis, parentExecution)

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
			attribute.Int("participants.required", e.maxParticipants),
		),
	)
	defer collectionSpan.End()

	// Context for canceling remaining requests
	cancellableCtx, cancelRemaining := context.WithCancel(ctx)
	defer cancelRemaining()

	// Channel to collect responses - buffered to prevent blocking
	responseChan := make(chan *execResult[R], e.maxParticipants)

	// Start required number of participants
	actualGoroutineCount := e.maxParticipants
	for execIdx := 0; execIdx < e.maxParticipants; execIdx++ {
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

					panicErr := errPanicInConsensus
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
	responses := make([]*execResult[R], 0, e.maxParticipants)
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
				// Quick check: if we short-circuited, analyze to see if it was due to consensus
				quickAnalysis := e.analyzeResponses(lg, responses, parentExecution)
				hasQuickConsensus := false
				if quickAnalysis.winnerGroup != nil {
					if !quickAnalysis.winnerGroup.isError && !quickAnalysis.winnerGroup.isEmpty {
						// Non-empty results: check if there's a CLEAR winner
						if quickAnalysis.winnerCount >= e.agreementThreshold {
							hasQuickConsensus = true
						} else {
							// Check if this non-empty result is clearly dominant
							maxOtherCount := 0
							for hash, group := range quickAnalysis.responsesByHash {
								if hash != quickAnalysis.winnerHash && !group.isError && !group.isEmpty {
									if group.count > maxOtherCount {
										maxOtherCount = group.count
									}
								}
							}
							hasQuickConsensus = quickAnalysis.winnerCount > maxOtherCount
						}
					} else {
						// Empty results or errors must meet threshold
						hasQuickConsensus = quickAnalysis.winnerCount >= e.agreementThreshold
					}
				}

				if hasQuickConsensus {
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

// checkShortCircuit determines if we can exit early based on current responses
func (e *executor[R]) checkShortCircuit(lg *zerolog.Logger, responses []*execResult[R], exec policy.ExecutionInternal[R]) bool {
	// Quick analysis for short-circuit decision
	analysis := e.analyzeResponses(lg, responses, exec)

	// Check if consensus is reached with proper logic
	hasConsensus := false
	if analysis.winnerGroup != nil {
		if !analysis.winnerGroup.isError && !analysis.winnerGroup.isEmpty {
			// Non-empty results: check if there's a CLEAR winner
			// If winner meets threshold, it's a clear winner regardless of threshold bypass
			// If winner doesn't meet threshold, check if it's truly dominant over other non-empty results
			if analysis.winnerCount >= e.agreementThreshold {
				hasConsensus = true
			} else {
				// Check if this non-empty result is clearly dominant
				// (i.e., more votes than any other single result type)
				maxOtherCount := 0
				for hash, group := range analysis.responsesByHash {
					if hash != analysis.winnerHash && !group.isError && !group.isEmpty {
						if group.count > maxOtherCount {
							maxOtherCount = group.count
						}
					}
				}
				// Only consensus if winner is clearly dominant over other non-empty results
				hasConsensus = analysis.winnerCount > maxOtherCount
			}
		} else {
			// Empty results or errors must meet threshold
			hasConsensus = analysis.winnerCount >= e.agreementThreshold
		}
	}

	if hasConsensus {
		// Check if the winner is empty - if so, avoid short-circuiting
		// to allow more responses that might contain meaningful data
		if analysis.winnerGroup != nil && analysis.winnerGroup.isEmpty {
			lg.Debug().
				Int("responseCount", len(responses)).
				Int("winnerCount", analysis.winnerCount).
				Int("threshold", e.agreementThreshold).
				Str("winnerHash", analysis.winnerHash).
				Bool("winnerIsEmpty", true).
				Msg("consensus reached on empty result, avoiding short-circuit to collect more data")
			return false
		}

		// For all dispute behaviors, use mathematical guarantee logic to avoid premature short-circuit
		remainingResponses := e.maxParticipants - len(responses)

		// Find the maximum count among non-winner results
		maxOtherResultCount := 0
		for hash, group := range analysis.responsesByHash {
			if hash != analysis.winnerHash && group.count > maxOtherResultCount {
				maxOtherResultCount = group.count
			}
		}

		// Can short-circuit if current winner cannot be beaten mathematically
		cannotBeBeaten := analysis.winnerCount > maxOtherResultCount+remainingResponses
		hasSufficientParticipants := !analysis.isLowParticipants

		// Short-circuit conditions:
		// 1. Mathematical guarantee (winner cannot be beaten) - works regardless of participant count
		//    BUT: Don't short-circuit on error/empty winners if remaining responses could be non-empty
		//    (non-empty preference means any success beats any number of errors)
		// 2. Consensus on non-empty results with sufficient participants and threshold met
		canShortCircuitOnMathGuarantee := cannotBeBeaten
		if cannotBeBeaten && remainingResponses > 0 && analysis.winnerGroup != nil &&
			(analysis.winnerGroup.isError || analysis.winnerGroup.isEmpty) {
			// Don't short-circuit on error/empty winners if remaining responses could contain non-empty results
			// that would win due to non-empty preference
			canShortCircuitOnMathGuarantee = false
		}
		canShortCircuitOnConsensus := hasSufficientParticipants &&
			analysis.winnerGroup != nil &&
			!analysis.winnerGroup.isError &&
			!analysis.winnerGroup.isEmpty &&
			analysis.winnerCount >= e.agreementThreshold

		if canShortCircuitOnMathGuarantee || canShortCircuitOnConsensus {
			lg.Debug().
				Int("responseCount", len(responses)).
				Int("winnerCount", analysis.winnerCount).
				Int("maxOtherResultCount", maxOtherResultCount).
				Int("remainingResponses", remainingResponses).
				Bool("canShortCircuitOnMathGuarantee", canShortCircuitOnMathGuarantee).
				Bool("canShortCircuitOnConsensus", canShortCircuitOnConsensus).
				Bool("hasSufficientParticipants", hasSufficientParticipants).
				Bool("winnerIsEmpty", analysis.winnerGroup != nil && analysis.winnerGroup.isEmpty).
				Int("agreementThreshold", e.agreementThreshold).
				Msg("short-circuiting: mathematical guarantee or consensus (both require sufficient participants)")
			return true
		} else {
			lg.Debug().
				Int("responseCount", len(responses)).
				Int("winnerCount", analysis.winnerCount).
				Int("maxOtherResultCount", maxOtherResultCount).
				Int("remainingResponses", remainingResponses).
				Bool("canShortCircuitOnMathGuarantee", canShortCircuitOnMathGuarantee).
				Bool("canShortCircuitOnConsensus", canShortCircuitOnConsensus).
				Bool("hasSufficientParticipants", hasSufficientParticipants).
				Bool("winnerIsEmpty", analysis.winnerGroup != nil && analysis.winnerGroup.isEmpty).
				Int("agreementThreshold", e.agreementThreshold).
				Msg("waiting for more responses: insufficient participants or no clear consensus/mathematical guarantee")
			return false
		}
	}

	// Check if consensus is impossible
	remainingResponses := e.maxParticipants - len(responses)

	// Can any result reach consensus?
	canReachConsensus := false
	for _, group := range analysis.responsesByHash {
		if group.count+remainingResponses >= e.agreementThreshold {
			canReachConsensus = true
			break
		}
	}

	// Only short-circuit if we have enough unique participants
	expectedUniqueParticipants := analysis.totalValidParticipants + remainingResponses
	if !canReachConsensus && expectedUniqueParticipants >= e.maxParticipants {
		// Don't short-circuit if we have non-empty results with AcceptMostCommonValidResult
		if analysis.nonEmptySuccessCount > 0 && e.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult {
			lg.Debug().
				Int("responseCount", len(responses)).
				Int("nonEmptyCount", analysis.nonEmptySuccessCount).
				Msg("consensus impossible but have non-empty results with AcceptMostCommonValidResult, continuing")
			return false
		}

		lg.Debug().
			Int("responseCount", len(responses)).
			Int("uniqueParticipants", analysis.totalValidParticipants).
			Int("winnerCount", analysis.winnerCount).
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
) (*failsafeCommon.PolicyResult[R], *consensusAnalysis[R]) {
	// Start evaluation span
	ctx, evalSpan := common.StartDetailSpan(ctx, "Consensus.Evaluate",
		trace.WithAttributes(
			attribute.Int("responses.count", len(responses)),
			attribute.Int("participants.required", e.maxParticipants),
		),
	)
	defer evalSpan.End()

	// Analyze all responses once
	analysis := e.analyzeResponses(lg, responses, exec)

	lg.Debug().
		Int("participantCount", analysis.totalValidParticipants).
		Int("maxParticipants", e.maxParticipants).
		Msg("counting unique participants")

	// Record agreement count metric
	if analysis.winnerCount > 0 {
		telemetry.MetricConsensusAgreementCount.WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr).Observe(float64(analysis.winnerCount))
	}

	evalSpan.SetAttributes(
		attribute.Int("participants.count", analysis.totalValidParticipants),
		attribute.Int("participants.required", e.maxParticipants),
		attribute.Int("results.unique_count", len(analysis.responsesByHash)),
		attribute.Int("results.winner_count", analysis.winnerCount),
		attribute.Int("results.agreement_threshold", e.agreementThreshold),
	)

	lg.Debug().
		Int("totalResponses", analysis.totalResponses).
		Int("nonEmptyCount", analysis.nonEmptySuccessCount).
		Int("emptyCount", analysis.emptySuccessCount).
		Int("errorCount", analysis.consensusErrorCount).
		Msg("consensus check")

	lg.Debug().
		Str("winnerHash", analysis.winnerHash).
		Int("count", analysis.winnerCount).
		Int("threshold", e.agreementThreshold).
		Msg("consensus result")

	// Check for mathematical guarantee first - if consensus is mathematically guaranteed,
	// we can proceed even with low participants
	hasMathematicalGuarantee := false
	if analysis.winnerGroup != nil && len(responses) < e.maxParticipants {
		// Calculate if the winner cannot be beaten by remaining responses
		remainingResponses := e.maxParticipants - len(responses)
		maxOtherCount := 0
		for hash, group := range analysis.responsesByHash {
			if hash != analysis.winnerHash {
				if group.count > maxOtherCount {
					maxOtherCount = group.count
				}
			}
		}
		hasMathematicalGuarantee = analysis.winnerCount > maxOtherCount+remainingResponses

		if hasMathematicalGuarantee {
			lg.Debug().
				Int("winnerCount", analysis.winnerCount).
				Int("maxOtherCount", maxOtherCount).
				Int("remainingResponses", remainingResponses).
				Msg("mathematical guarantee detected, bypassing participant requirement")
		}
	}

	// Check participants - must have sufficient participants unless mathematically guaranteed
	// AND using AcceptMostCommonValidResult behavior (mathematical guarantee doesn't override explicit ReturnError)
	allowMathematicalBypass := hasMathematicalGuarantee &&
		e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
	if analysis.isLowParticipants && !allowMathematicalBypass {
		lg.Debug().
			Int("participantCount", analysis.totalValidParticipants).
			Int("responses", len(responses)).
			Int("required", e.maxParticipants).
			Msg("handling low participants")

		evalSpan.SetAttributes(
			attribute.Bool("consensus.low_participants", true),
			attribute.String("low_participants.behavior", string(e.lowParticipantsBehavior)),
		)

		result := e.handleLowParticipants(lg, analysis, exec)
		if result.Error != nil {
			common.SetTraceSpanError(evalSpan, result.Error)
			telemetry.MetricConsensusErrors.WithLabelValues(labels.projectId, labels.networkId, labels.category, "low_participants", labels.finalityStr).Inc()
		}
		return result, analysis
	}

	// Now check consensus with standard logic
	hasConsensus := false
	if analysis.winnerGroup != nil {
		if !analysis.winnerGroup.isError && !analysis.winnerGroup.isEmpty {
			// Non-empty results: check threshold, allow mathematical guarantee to bypass for AcceptMostCommon only
			allowThresholdBypass := hasMathematicalGuarantee &&
				e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
			if analysis.winnerCount >= e.agreementThreshold || allowThresholdBypass {
				hasConsensus = true
				lg.Debug().
					Int("winnerCount", analysis.winnerCount).
					Int("threshold", e.agreementThreshold).
					Bool("hasMathematicalGuarantee", hasMathematicalGuarantee).
					Bool("allowThresholdBypass", allowThresholdBypass).
					Bool("isLowParticipants", analysis.isLowParticipants).
					Msg("non-empty winner: consensus achieved")
			} else {
				lg.Debug().
					Int("winnerCount", analysis.winnerCount).
					Int("threshold", e.agreementThreshold).
					Bool("hasMathematicalGuarantee", hasMathematicalGuarantee).
					Bool("allowThresholdBypass", allowThresholdBypass).
					Bool("isLowParticipants", analysis.isLowParticipants).
					Msg("non-empty winner: below threshold and no mathematical guarantee bypass allowed")
			}
		} else {
			// Empty results or errors must meet threshold AND have sufficient participants
			hasConsensus = !analysis.isLowParticipants && analysis.winnerCount >= e.agreementThreshold
			lg.Debug().
				Int("winnerCount", analysis.winnerCount).
				Int("threshold", e.agreementThreshold).
				Bool("isLowParticipants", analysis.isLowParticipants).
				Msg("empty/error winner: requires both sufficient participants and threshold")
		}
	}

	if hasConsensus {
		// We have consensus
		lg.Debug().Msg("completed consensus execution")
		evalSpan.SetAttributes(
			attribute.Bool("consensus.achieved", true),
			attribute.String("consensus.winner_hash", analysis.winnerHash),
		)

		if e.onAgreement != nil {
			e.onAgreement(failsafe.ExecutionEvent[R]{
				ExecutionAttempt: exec,
			})
		}

		// Return the winner result directly
		if analysis.winnerGroup != nil {
			if analysis.winnerGroup.isError {
				// Consensus on error
				lg.Debug().Err(analysis.winnerGroup.firstError).Msg("consensus reached on error")
				evalSpan.SetStatus(codes.Ok, "consensus achieved on error")
				telemetry.MetricConsensusErrors.WithLabelValues(labels.projectId, labels.networkId, labels.category, "consensus_on_error", labels.finalityStr).Inc()
				return &failsafeCommon.PolicyResult[R]{
					Error: analysis.winnerGroup.firstError,
				}, analysis
			} else {
				// Consensus on successful result
				evalSpan.SetStatus(codes.Ok, "consensus achieved")
				return &failsafeCommon.PolicyResult[R]{
					Result: analysis.winnerGroup.firstResult,
				}, analysis
			}
		}
	}

	// Consensus dispute
	e.logDispute(lg, req, analysis, exec)

	if e.onDispute != nil {
		e.onDispute(failsafe.ExecutionEvent[R]{
			ExecutionAttempt: exec,
		})
	}

	evalSpan.SetAttributes(
		attribute.Bool("consensus.dispute", true),
		attribute.String("dispute.behavior", string(e.disputeBehavior)),
	)

	// Before declaring dispute, check if the winner is an agreed-upon error
	if analysis.winnerGroup != nil && analysis.winnerGroup.isError && analysis.winnerCount >= 2 {
		if e.isAgreedUponError(analysis.winnerGroup.firstError) {
			lg.Debug().
				Err(analysis.winnerGroup.firstError).
				Str("errorHash", analysis.winnerHash).
				Int("agreementCount", analysis.winnerCount).
				Msg("consensus on agreed-upon error, returning agreed error")
			evalSpan.SetStatus(codes.Ok, "consensus achieved on agreed-upon error")
			telemetry.MetricConsensusErrors.WithLabelValues(labels.projectId, labels.networkId, labels.category, "agreed_error", labels.finalityStr).Inc()
			return &failsafeCommon.PolicyResult[R]{
				Error: analysis.winnerGroup.firstError,
			}, analysis
		}
	}

	// Handle dispute according to behavior
	result := e.handleDispute(lg, analysis, exec)
	if result.Error != nil {
		common.SetTraceSpanError(evalSpan, result.Error)
		telemetry.MetricConsensusErrors.WithLabelValues(labels.projectId, labels.networkId, labels.category, "dispute", labels.finalityStr).Inc()
	}
	return result, analysis
}

// analyzeResponses performs comprehensive analysis of all responses in a single pass
// Returns winner information considering non-empty preference and all categorized groups
func (e *executor[R]) analyzeResponses(lg *zerolog.Logger, responses []*execResult[R], exec failsafe.Execution[R]) *consensusAnalysis[R] {
	analysis := &consensusAnalysis[R]{
		responsesByHash: make(map[string]*responseGroup[R]),
		nonEmptyGroups:  make(map[string]*responseGroup[R]),
		emptyGroups:     make(map[string]*responseGroup[R]),
		errorGroups:     make(map[string]*responseGroup[R]),
		totalResponses:  len(responses),
	}

	// Track unique participants - count unique upstreams that returned usable responses
	// This represents the number of unique participants that contributed to consensus
	uniqueParticipants := make(map[string]bool)

	// Single pass through all responses
	for _, r := range responses {
		// Count this response as a participant if it can contribute to consensus
		canContribute := false
		if r.err == nil {
			// Successful responses always contribute
			canContribute = true
		} else if e.isConsensusValidError(r.err) || e.isAgreedUponError(r.err) {
			// Consensus-valid or agreed-upon errors contribute
			canContribute = true
		}

		if canContribute {
			// Count unique upstream participants
			if r.upstream != nil && r.upstream.Config() != nil {
				uniqueParticipants[r.upstream.Config().Id] = true
			}
		}

		// Calculate hash for this response
		resultHash, err := e.resultOrErrorToHash(r.result, r.err, exec)

		// Handle unhashable responses (non-consensus errors)
		if err != nil || resultHash == "" {
			// Try agreed-upon errors
			if r.err != nil && e.isAgreedUponError(r.err) {
				if hash := e.errorToConsensusHash(r.err); hash != "" {
					resultHash = hash
				} else {
					analysis.nonConsensusErrorCount++
					continue
				}
			} else {
				analysis.nonConsensusErrorCount++
				if r.err != nil && !e.isConsensusValidError(r.err) && !e.isAgreedUponError(r.err) {
					lg.Debug().Err(r.err).Msg("response has non-consensus/agreed-upon error")
				}
				continue
			}
		}

		// Get or create response group
		group, exists := analysis.responsesByHash[resultHash]
		if !exists {
			group = &responseGroup[R]{
				responses: make([]*execResult[R], 0),
			}
			analysis.responsesByHash[resultHash] = group
		}

		// Add response to group
		group.count++
		group.responses = append(group.responses, r)

		// Set group properties and cache first result/error
		if r.err != nil {
			group.isError = true
			if group.firstError == nil {
				group.firstError = r.err
			}
			analysis.consensusErrorCount++
			analysis.errorGroups[resultHash] = group
		} else {
			// Successful response
			if !group.hasResult {
				group.firstResult = r.result
				group.hasResult = true
			}

			// Check if empty
			isEmpty := e.isResultEmptyish(r, exec.Context())
			group.isEmpty = isEmpty

			if isEmpty {
				analysis.emptySuccessCount++
				analysis.emptyGroups[resultHash] = group
			} else {
				analysis.nonEmptySuccessCount++
				analysis.nonEmptyGroups[resultHash] = group
			}
		}
	}

	// Set the final participant count based on unique upstreams
	participantCount := len(uniqueParticipants)
	analysis.totalValidParticipants = participantCount

	// Determine winner with non-empty preference first
	analysis.determineWinner(lg)

	// Determine if we have low participants - check if we have enough participants
	// to potentially reach the agreement threshold (dispute vs low participants)
	analysis.isLowParticipants = participantCount < e.agreementThreshold

	return analysis
}

// determineWinner applies non-empty preference logic to find the consensus winner
func (a *consensusAnalysis[R]) determineWinner(lg *zerolog.Logger) {
	// Prefer non-empty successful results over everything else
	if len(a.nonEmptyGroups) > 0 {
		// Find winner among non-empty results - ensure we get the group with highest count
		maxCount := 0
		var bestHash string
		var bestGroup *responseGroup[R]

		for hash, group := range a.nonEmptyGroups {
			if group.count > maxCount {
				maxCount = group.count
				bestHash = hash
				bestGroup = group
			}
		}

		// Non-empty results always win over errors, regardless of count
		if bestGroup != nil {
			previousWinnerCount := a.winnerCount
			a.winnerHash = bestHash
			a.winnerCount = maxCount
			a.winnerGroup = bestGroup

			lg.Debug().
				Str("winnerHash", a.winnerHash).
				Int("winnerCount", a.winnerCount).
				Int("totalNonEmpty", len(a.nonEmptyGroups)).
				Int("previousWinnerCount", previousWinnerCount).
				Bool("overrodeHigherErrorCount", previousWinnerCount > maxCount).
				Msg("winner selected from non-empty results (non-empty preference)")
		}
		return
	}

	// No non-empty results, check consensus errors
	if len(a.errorGroups) > 0 {
		maxCount := a.winnerCount
		var bestHash string
		var bestGroup *responseGroup[R]

		for hash, group := range a.errorGroups {
			if group.count > maxCount {
				maxCount = group.count
				bestHash = hash
				bestGroup = group
			}
		}

		// Only update if we found a better winner
		if bestGroup != nil && maxCount > a.winnerCount {
			a.winnerHash = bestHash
			a.winnerCount = maxCount
			a.winnerGroup = bestGroup
		}

		lg.Debug().
			Str("winnerHash", a.winnerHash).
			Int("winnerCount", a.winnerCount).
			Msg("winner selected from consensus errors")
		return
	}

	// Finally, check empty results
	if len(a.emptyGroups) > 0 {
		maxCount := a.winnerCount
		var bestHash string
		var bestGroup *responseGroup[R]

		for hash, group := range a.emptyGroups {
			if group.count > maxCount {
				maxCount = group.count
				bestHash = hash
				bestGroup = group
			}
		}

		// Only update if we found a better winner
		if bestGroup != nil && maxCount > a.winnerCount {
			a.winnerHash = bestHash
			a.winnerCount = maxCount
			a.winnerGroup = bestGroup
		}

		lg.Debug().
			Str("winnerHash", a.winnerHash).
			Int("winnerCount", a.winnerCount).
			Msg("winner selected from empty results")
	}
}

// logDispute logs details about a consensus dispute
func (e *executor[R]) logDispute(lg *zerolog.Logger, req *common.NormalizedRequest, analysis *consensusAnalysis[R], exec failsafe.Execution[R]) {
	method, _ := req.Method()

	evt := lg.WithLevel(e.disputeLogLevel).
		Str("method", method).
		Object("request", req).
		Int("winnerCount", analysis.winnerCount).
		Int("threshold", e.agreementThreshold).
		Int("totalResponses", analysis.totalResponses)

	// Log hash distribution and upstreams
	hashToUpstreams := make(map[string][]string)
	hashCounts := make(map[string]int)

	// Iterate through all response groups
	for hash, group := range analysis.responsesByHash {
		hashCounts[hash] = group.count
		upstreams := make([]string, 0, len(group.responses))

		for _, resp := range group.responses {
			upstreamID := "unknown"
			if resp.upstream != nil && resp.upstream.Config() != nil {
				upstreamID = resp.upstream.Config().Id
			}
			upstreams = append(upstreams, upstreamID)

			// Log individual response details
			evt.Str(fmt.Sprintf("upstream%d", resp.index), upstreamID)

			if resp.err != nil {
				evt.AnErr(fmt.Sprintf("error%d", resp.index), resp.err)
				evt.Str(fmt.Sprintf("errorHash%d", resp.index), hash)
			} else {
				// Log successful response
				if jr := e.resultToJsonRpcResponse(resp.result, exec); jr != nil {
					evt.Object(fmt.Sprintf("response%d", resp.index), jr)
				} else {
					evt.Interface(fmt.Sprintf("result%d", resp.index), resp.result)
				}
				evt.Str(fmt.Sprintf("hash%d", resp.index), hash)
			}
		}

		hashToUpstreams[hash] = upstreams
	}

	// Log the winner information
	evt.Str("winnerHash", analysis.winnerHash).
		Bool("winnerIsError", analysis.winnerGroup != nil && analysis.winnerGroup.isError).
		Bool("winnerIsEmpty", analysis.winnerGroup != nil && analysis.winnerGroup.isEmpty)

	// Log the hash distribution
	evt.Interface("hashDistribution", hashCounts)
	evt.Interface("hashToUpstreams", hashToUpstreams)

	evt.Msg("consensus dispute")
}

// handleLowParticipants handles the low participants scenario based on configured behavior
func (e *executor[R]) handleLowParticipants(
	lg *zerolog.Logger,
	analysis *consensusAnalysis[R],
	exec failsafe.Execution[R],
) *failsafeCommon.PolicyResult[R] {
	// Gather all responses for extracting participants/causes
	allResponses := e.gatherAllResponses(analysis)

	errFn := func() error {
		return common.NewErrConsensusLowParticipants(
			"not enough participants",
			e.extractParticipants(allResponses, exec),
			e.extractCauses(allResponses),
		)
	}

	switch e.lowParticipantsBehavior {
	case common.ConsensusLowParticipantsBehaviorReturnError:
		return e.handleReturnError(lg, errFn)
	case common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult:
		return e.handleAcceptMostCommon(lg, analysis, func() error {
			return common.NewErrConsensusLowParticipants(
				"no clear most common result",
				e.extractParticipants(allResponses, exec),
				e.extractCauses(allResponses),
			)
		})
	case common.ConsensusLowParticipantsBehaviorAcceptAnyValidResult:
		return e.handleAcceptAnyValid(lg, analysis, func() error {
			return common.NewErrConsensusLowParticipants(
				"no valid results found",
				e.extractParticipants(allResponses, exec),
				e.extractCauses(allResponses),
			)
		})
	case common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader:
		return e.handlePreferBlockHeadLeader(exec.Context(), lg, analysis, func() error {
			return common.NewErrConsensusLowParticipants(
				"no block head leader found",
				e.extractParticipants(allResponses, exec),
				e.extractCauses(allResponses),
			)
		})
	case common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader:
		return e.handleOnlyBlockHeadLeader(exec.Context(), lg, analysis, func() error {
			return common.NewErrConsensusLowParticipants(
				"no block head leader found",
				e.extractParticipants(allResponses, exec),
				e.extractCauses(allResponses),
			)
		})
	default:
		return e.handleReturnError(lg, errFn)
	}
}

// gatherAllResponses collects all responses from the analysis groups
func (e *executor[R]) gatherAllResponses(analysis *consensusAnalysis[R]) []*execResult[R] {
	var allResponses []*execResult[R]
	for _, group := range analysis.responsesByHash {
		allResponses = append(allResponses, group.responses...)
	}
	return allResponses
}

// handleDispute handles the dispute scenario based on configured behavior
func (e *executor[R]) handleDispute(
	lg *zerolog.Logger,
	analysis *consensusAnalysis[R],
	exec failsafe.Execution[R],
) *failsafeCommon.PolicyResult[R] {
	// Gather all responses for extracting participants/causes
	allResponses := e.gatherAllResponses(analysis)

	errFn := func() error {
		return common.NewErrConsensusDispute(
			"not enough agreement among responses",
			e.extractParticipants(allResponses, exec),
			e.extractCauses(allResponses),
		)
	}

	switch e.disputeBehavior {
	case common.ConsensusDisputeBehaviorReturnError:
		return e.handleReturnError(lg, errFn)
	case common.ConsensusDisputeBehaviorAcceptMostCommonValidResult:
		return e.handleAcceptMostCommon(lg, analysis, func() error {
			return common.NewErrConsensusDispute(
				"no clear most common result",
				e.extractParticipants(allResponses, exec),
				e.extractCauses(allResponses),
			)
		})
	case common.ConsensusDisputeBehaviorAcceptAnyValidResult:
		return e.handleAcceptAnyValid(lg, analysis, func() error {
			return common.NewErrConsensusDispute(
				"no valid results found",
				e.extractParticipants(allResponses, exec),
				e.extractCauses(allResponses),
			)
		})
	case common.ConsensusDisputeBehaviorPreferBlockHeadLeader:
		return e.handlePreferBlockHeadLeader(exec.Context(), lg, analysis, func() error {
			return common.NewErrConsensusDispute(
				"no block head leader found",
				e.extractParticipants(allResponses, exec),
				e.extractCauses(allResponses),
			)
		})
	case common.ConsensusDisputeBehaviorOnlyBlockHeadLeader:
		return e.handleOnlyBlockHeadLeader(exec.Context(), lg, analysis, func() error {
			return common.NewErrConsensusDispute(
				"no block head leader found",
				e.extractParticipants(allResponses, exec),
				e.extractCauses(allResponses),
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
	analysis *consensusAnalysis[R],
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

	punishSpan.SetAttributes(
		attribute.Int("responses.count", analysis.totalResponses),
		attribute.Int("winner.count", analysis.winnerCount),
		attribute.Bool("has_clear_majority", analysis.winnerCount > analysis.totalResponses/2),
	)

	// Only track misbehaving upstreams if we have a clear majority (>50%)
	if analysis.winnerCount > analysis.totalResponses/2 {
		e.trackMisbehavingUpstreams(ctx, lg, req, labels, analysis, parentExecution)
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

func (e *executor[R]) trackMisbehavingUpstreams(ctx context.Context, logger *zerolog.Logger, req *common.NormalizedRequest, labels metricsLabels, analysis *consensusAnalysis[R], execution policy.ExecutionInternal[R]) {
	// Start tracking span
	_, trackingSpan := common.StartDetailSpan(ctx, "Consensus.TrackMisbehavior")
	defer trackingSpan.End()

	// Already validated in checkAndPunishMisbehavingUpstreams that we have clear majority

	misbehavingCount := 0
	// Check all groups that don't match the winner
	for hash, group := range analysis.responsesByHash {
		if hash == analysis.winnerHash {
			continue // Skip the winning group
		}

		// All responses in this group are misbehaving
		for _, response := range group.responses {
			upstream := response.upstream
			if upstream == nil {
				continue
			}

			misbehavingCount++
			upstreamId := upstream.Config().Id

			// Find a correct response from winner group for comparison
			var correctResponse *execResult[R]
			if analysis.winnerGroup != nil && len(analysis.winnerGroup.responses) > 0 {
				correctResponse = analysis.winnerGroup.responses[0]
			}

			// Log warning with detailed comparison
			e.logMisbehavingUpstreamWarning(logger, req, response, correctResponse, analysis.winnerHash, upstreamId, execution)

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
		attribute.String("winner.hash", analysis.winnerHash),
	)
}

func (e *executor[R]) logMisbehavingUpstreamWarning(logger *zerolog.Logger, req *common.NormalizedRequest, misbehavingResponse *execResult[R], correctResponse *execResult[R], correctHash string, upstreamId string, execution policy.ExecutionInternal[R]) {

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

func (e *executor[R]) handleAcceptMostCommon(logger *zerolog.Logger, analysis *consensusAnalysis[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	logger.Debug().Msg("finding most common valid result preferring non-empty")

	// Check if we have a winner
	if analysis.winnerGroup == nil {
		logger.Debug().Msg("no winner found, sending error")
		return &failsafeCommon.PolicyResult[R]{
			Error: errFn(),
		}
	}

	// Check for ties (multiple groups with same count as winner)
	tieCount := 0
	for _, group := range analysis.responsesByHash {
		if group.count == analysis.winnerCount {
			tieCount++
		}
	}

	if tieCount > 1 {
		logger.Debug().
			Int("tieCount", tieCount).
			Int("winnerCount", analysis.winnerCount).
			Msg("multiple results have the same count, cannot determine most common")
		return &failsafeCommon.PolicyResult[R]{
			Error: errFn(),
		}
	}

	// Handle winner based on type
	if analysis.winnerGroup.isError {
		logger.Debug().Err(analysis.winnerGroup.firstError).Msg("returning consensus-valid error as result")
		return &failsafeCommon.PolicyResult[R]{
			Error: analysis.winnerGroup.firstError,
		}
	}

	// Handle winner based on type - follow AcceptMostCommonValidResult behavior
	if !analysis.winnerGroup.isEmpty {
		// Non-empty result - accept regardless of threshold or participant count
		logger.Debug().
			Str("hash", analysis.winnerHash).
			Int("count", analysis.winnerCount).
			Bool("isLowParticipants", analysis.isLowParticipants).
			Msg("accepting non-empty most common result (ignore threshold and participants)")
		return &failsafeCommon.PolicyResult[R]{
			Result: analysis.winnerGroup.firstResult,
		}
	} else {
		// Empty result - must meet threshold AND have sufficient participants
		if !analysis.isLowParticipants && analysis.winnerCount >= e.agreementThreshold {
			logger.Debug().
				Str("hash", analysis.winnerHash).
				Int("count", analysis.winnerCount).
				Int("threshold", e.agreementThreshold).
				Bool("isLowParticipants", analysis.isLowParticipants).
				Msg("accepting empty most common result meeting threshold with sufficient participants")
			return &failsafeCommon.PolicyResult[R]{
				Result: analysis.winnerGroup.firstResult,
			}
		} else {
			logger.Debug().
				Str("hash", analysis.winnerHash).
				Int("count", analysis.winnerCount).
				Int("threshold", e.agreementThreshold).
				Bool("isLowParticipants", analysis.isLowParticipants).
				Msg("rejecting empty result: insufficient count for threshold or insufficient participants")
			return &failsafeCommon.PolicyResult[R]{
				Error: errFn(),
			}
		}
	}
}

func (e *executor[R]) handleAcceptAnyValid(logger *zerolog.Logger, analysis *consensusAnalysis[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	logger.Debug().Msg("accepting any valid result (prefer non-empty)")

	// Look for non-empty successful results first - accept regardless of participants
	for _, group := range analysis.nonEmptyGroups {
		if len(group.responses) > 0 {
			// Found non-empty result, use it immediately
			upstreamId := ""
			if group.responses[0].upstream != nil && group.responses[0].upstream.Config() != nil {
				upstreamId = group.responses[0].upstream.Config().Id
			}
			logger.Debug().
				Str("upstream", upstreamId).
				Bool("isLowParticipants", analysis.isLowParticipants).
				Msg("found non-empty result (accept regardless of participants)")
			return &failsafeCommon.PolicyResult[R]{
				Result: group.firstResult,
			}
		}
	}

	// No non-empty results, check participant sufficiency for empty results
	if analysis.isLowParticipants {
		logger.Debug().
			Bool("isLowParticipants", analysis.isLowParticipants).
			Msg("rejecting empty results: insufficient participants")
		return &failsafeCommon.PolicyResult[R]{
			Error: errFn(),
		}
	}

	// Look for empty results with sufficient participants
	for _, group := range analysis.emptyGroups {
		if len(group.responses) > 0 {
			upstreamId := ""
			if group.responses[0].upstream != nil && group.responses[0].upstream.Config() != nil {
				upstreamId = group.responses[0].upstream.Config().Id
			}
			logger.Debug().
				Str("upstream", upstreamId).
				Msg("found empty result with sufficient participants")
			return &failsafeCommon.PolicyResult[R]{
				Result: group.firstResult,
			}
		}
	}

	logger.Debug().Msg("no valid results found with sufficient participants")

	// If no successful result, check for consensus-valid errors
	for _, group := range analysis.errorGroups {
		if group.firstError != nil {
			logger.Debug().Err(group.firstError).Msg("returning consensus-valid error")
			return &failsafeCommon.PolicyResult[R]{
				Error: group.firstError,
			}
		}
	}

	// No valid result found
	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}

func (e *executor[R]) findBlockHeadLeaderResult(ctx context.Context, analysis *consensusAnalysis[R]) *execResult[R] {
	var network common.Network
	allResponses := e.gatherAllResponses(analysis)

	// Try to find network from responses first
	for _, r := range allResponses {
		if resp, ok := any(r.result).(*common.NormalizedResponse); ok {
			if req := resp.Request(); req != nil {
				if n := req.Network(); n != nil && n.Id() != "" {
					network = n
					break
				}
			}
		}
	}

	var leaderUpstream common.Upstream
	if network != nil {
		// Production path: use network's leader detection
		leaderUpstream = network.EvmLeaderUpstream(ctx)
	}

	if leaderUpstream == nil {
		return nil
	}

	for _, r := range allResponses {
		if r.err == nil {
			if r.upstream == leaderUpstream {
				return r
			}
		}
	}
	return nil
}

func (e *executor[R]) handlePreferBlockHeadLeader(ctx context.Context, logger *zerolog.Logger, analysis *consensusAnalysis[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	// Try to find block head leader first (regardless of participant count)
	blockLeaderResult := e.findBlockHeadLeaderResult(ctx, analysis)
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

	// No block head leader found - check participant sufficiency for fallback
	if analysis.isLowParticipants {
		logger.Debug().
			Bool("isLowParticipants", analysis.isLowParticipants).
			Msg("no block head leader found and insufficient participants")
		return &failsafeCommon.PolicyResult[R]{
			Error: errFn(),
		}
	}

	// Fall back to most common result
	return e.handleAcceptMostCommon(logger, analysis, errFn)
}

func (e *executor[R]) handleOnlyBlockHeadLeader(ctx context.Context, logger *zerolog.Logger, analysis *consensusAnalysis[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	// Check participant sufficiency first
	if analysis.isLowParticipants {
		logger.Debug().
			Bool("isLowParticipants", analysis.isLowParticipants).
			Msg("rejecting only block head leader: insufficient participants")
		return &failsafeCommon.PolicyResult[R]{
			Error: errFn(),
		}
	}

	blockLeaderResult := e.findBlockHeadLeaderResult(ctx, analysis)
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
	logger.Debug().Msg("no block head leader found, returning error")
	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}
