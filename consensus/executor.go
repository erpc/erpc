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

var (
	errNoJsonRpcResponse = errors.New("no json-rpc response available on result")
	errNotConsensusValid = errors.New("error is not consensus-valid")
	errPanicInConsensus  = errors.New("panic in consensus execution")

	labelRespTypeUnknown  = "unknown"
	labelRespTypeEmpty    = "empty"
	labelRespTypeNonEmpty = "non-empty"
	labelRespTypeError    = "error"

	decisionRules []DecisionRule = buildDecisionRules()
)

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

// responseGroup holds all responses with the same hash
type responseGroup[R any] struct {
	count        int
	isEmpty      bool
	isError      bool
	responses    []*execResult[R]
	firstResult  R     // Cached first successful result
	firstError   error // Cached first error (for error consensus)
	hasResult    bool  // Whether firstResult has been set
	responseSize int   // Cached response size in bytes (for larger response preference)
}

// consensusAnalysis contains all analyzed consensus data computed once
type consensusAnalysis[R any] struct {
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

// Decision Matrix Types for Short-Circuit Logic

// ResponseType classifies the type of response
type ResponseType int

const (
	ResponseTypeError ResponseType = iota
	ResponseTypeEmpty
	ResponseTypeNonEmpty
)

// ConsensusState describes the current consensus situation
type ConsensusState int

const (
	ConsensusStateNone ConsensusState = iota
	ConsensusStateThresholdMet
	ConsensusStateDominant // Winner is dominant but doesn't meet threshold
	ConsensusStateTie
)

// CompetitionState describes whether the current winner can be beaten
type CompetitionState int

const (
	CompetitionStateUnbeatable  CompetitionState = iota // Mathematically cannot be beaten
	CompetitionStateBeatable                            // Can be beaten by count
	CompetitionStateQualityRisk                         // Can be beaten by quality (larger/non-empty)
)

// DecisionDimensions captures all factors that influence short-circuit decisions
type DecisionDimensions struct {
	WinnerType            ResponseType
	ConsensusState        ConsensusState
	CompetitionState      CompetitionState
	HasRemaining          bool
	IsLowParticipants     bool
	PreferNonEmpty        bool
	PreferLargerResponses bool
	DisputeBehavior       common.ConsensusDisputeBehavior
}

// ShortCircuitDecision represents the outcome of the decision matrix
type ShortCircuitDecision struct {
	ShouldShortCircuit bool
	Reason             string
}

// DecisionRule defines a single rule in the decision matrix
type DecisionRule struct {
	Priority  int
	Name      string
	Condition func(dims DecisionDimensions) bool
	Decision  ShortCircuitDecision
}

// findBestGroup finds the best group in a category based on preference settings
// NEVER applies size preference to error or empty responses
func (a *consensusAnalysis[R]) findBestGroup(groups map[string]*responseGroup[R], preferLargerResponses bool, agreementThreshold int) (string, *responseGroup[R]) {
	var bestHash string
	var bestGroup *responseGroup[R]

	if preferLargerResponses {
		// Prefer larger responses among those that meet agreement threshold
		// BUT ONLY for non-empty, non-error responses
		var thresholdCandidates []*responseGroup[R]
		var thresholdHashes []string
		maxSizeAmongThreshold := 0
		maxCountOverall := 0

		// First pass: find all candidates that meet threshold and track max count
		for hash, group := range groups {
			if group.count > maxCountOverall {
				maxCountOverall = group.count
			}
			if group.count >= agreementThreshold {
				thresholdCandidates = append(thresholdCandidates, group)
				thresholdHashes = append(thresholdHashes, hash)
				// ONLY consider size for non-empty, non-error responses
				if !group.isError && !group.isEmpty && group.responseSize > maxSizeAmongThreshold {
					maxSizeAmongThreshold = group.responseSize
				}
			}
		}

		// If we have candidates that meet threshold, pick the best one
		if len(thresholdCandidates) > 0 {
			// First, try to find the largest non-empty response
			for i, group := range thresholdCandidates {
				// For non-empty success responses, use size preference
				if !group.isError && !group.isEmpty && group.responseSize == maxSizeAmongThreshold {
					// If we already have a best candidate at this size, pick the one with higher count
					if bestGroup == nil || group.count > bestGroup.count {
						bestGroup = group
						bestHash = thresholdHashes[i]
					}
				}
			}

			// If no size winner found (all same size or all error/empty), pick by highest count
			if bestGroup == nil {
				maxCountInThreshold := 0
				for i, group := range thresholdCandidates {
					if group.count > maxCountInThreshold {
						maxCountInThreshold = group.count
						bestGroup = group
						bestHash = thresholdHashes[i]
					}
				}
			}
		} else {
			// No candidate meets threshold, fall back to highest count
			for hash, group := range groups {
				if group.count == maxCountOverall {
					// For non-empty success responses, consider size as tiebreaker
					if !group.isError && !group.isEmpty {
						if bestGroup == nil || group.responseSize > bestGroup.responseSize {
							bestGroup = group
							bestHash = hash
						}
					} else {
						// For errors/empty, just pick first one with max count
						if bestGroup == nil {
							bestGroup = group
							bestHash = hash
						}
					}
				}
			}
		}
	} else {
		// Highest count wins, no size preference at all
		maxCount := 0

		// First pass: find the max count
		for _, group := range groups {
			if group.count > maxCount {
				maxCount = group.count
			}
		}

		// Second pass: among groups with max count, pick deterministically
		// Use lexicographic ordering of hash as tiebreaker for determinism
		for hash, group := range groups {
			if group.count == maxCount {
				if bestGroup == nil || hash < bestHash {
					bestHash = hash
					bestGroup = group
				}
			}
		}
	}

	return bestHash, bestGroup
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
func (e *executor[R]) isAgreedUponError(err error) bool {
	// Include all endpoint errors that indicate the upstream processed the request
	// These errors show the upstream is functioning and gave a definitive response
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

// Helper functions for decision matrix

// getResponseType classifies a response group
func getResponseType[R any](group *responseGroup[R]) ResponseType {
	if group == nil {
		return ResponseTypeEmpty
	}
	if group.isError {
		return ResponseTypeError
	}
	if group.isEmpty {
		return ResponseTypeEmpty
	}
	return ResponseTypeNonEmpty
}

// getConsensusState determines the current consensus state
func (e *executor[R]) getConsensusState(analysis *consensusAnalysis[R]) ConsensusState {
	if analysis.winnerGroup == nil {
		return ConsensusStateNone
	}

	// Check for tie
	hasTie := false
	for hash, group := range analysis.responsesByHash {
		if hash != analysis.winnerHash && group.count == analysis.winnerCount {
			hasTie = true
			break
		}
	}

	// Check for tie first
	if hasTie {
		return ConsensusStateTie
	}

	// Check if threshold is met
	if analysis.winnerCount >= e.agreementThreshold {
		return ConsensusStateThresholdMet
	}

	// Check if winner is dominant (for non-empty results)
	if !analysis.winnerGroup.isError && !analysis.winnerGroup.isEmpty {
		// Check dominance over other non-empty results
		maxOtherCount := 0
		for hash, group := range analysis.responsesByHash {
			if hash != analysis.winnerHash && !group.isError && !group.isEmpty {
				if group.count > maxOtherCount {
					maxOtherCount = group.count
				}
			}
		}
		if analysis.winnerCount > maxOtherCount {
			return ConsensusStateDominant
		}
	}

	return ConsensusStateNone
}

// analyzeCompetitionState determines if the winner can be beaten
func (e *executor[R]) analyzeCompetitionState(analysis *consensusAnalysis[R], remaining int) CompetitionState {
	if analysis.winnerGroup == nil {
		return CompetitionStateBeatable
	}

	// Check for quality risks first
	if e.preferLargerResponses || e.preferNonEmpty {
		// Check if winner is error/empty and non-empty responses could arrive
		if e.preferNonEmpty && remaining > 0 && (analysis.winnerGroup.isError || analysis.winnerGroup.isEmpty) {
			// Any non-empty response would beat current winner
			return CompetitionStateQualityRisk
		}

		// Check if there are competing groups that could provide better quality
		if e.preferLargerResponses && remaining > 0 {
			relevantGroups := analysis.responsesByHash
			if e.preferNonEmpty {
				relevantGroups = analysis.nonEmptyGroups
			}

			for hash, group := range relevantGroups {
				if hash != analysis.winnerHash && (group.count+remaining >= e.agreementThreshold) {
					// There's a competitor that could reach threshold
					return CompetitionStateQualityRisk
				}
			}
		}
	}

	// Check mathematical guarantee
	maxOtherCount := 0
	for hash, group := range analysis.responsesByHash {
		if hash != analysis.winnerHash && group.count > maxOtherCount {
			maxOtherCount = group.count
		}
	}

	if analysis.winnerCount > maxOtherCount+remaining {
		return CompetitionStateUnbeatable
	}

	return CompetitionStateBeatable
}

func buildDecisionRules() []DecisionRule {
	return []DecisionRule{
		// Priority 1: Empty winner with preferNonEmpty - always wait
		{
			Priority: 100,
			Name:     "empty-winner-wait",
			Condition: func(dims DecisionDimensions) bool {
				// Always wait on empty winner to collect more meaningful data
				return dims.WinnerType == ResponseTypeEmpty && dims.HasRemaining
			},
			Decision: ShortCircuitDecision{
				ShouldShortCircuit: false,
				Reason:             "empty winner, waiting for more meaningful data",
			},
		},

		// Priority 2: Quality risk - wait for potentially better responses
		{
			Priority: 90,
			Name:     "quality-risk-wait",
			Condition: func(dims DecisionDimensions) bool {
				return dims.CompetitionState == CompetitionStateQualityRisk && dims.HasRemaining
			},
			Decision: ShortCircuitDecision{
				ShouldShortCircuit: false,
				Reason:             "waiting for potentially better quality responses",
			},
		},

		// Priority 3: Tie with remaining - wait for tiebreaker
		{
			Priority: 85,
			Name:     "tie-wait",
			Condition: func(dims DecisionDimensions) bool {
				return dims.ConsensusState == ConsensusStateTie && dims.HasRemaining
			},
			Decision: ShortCircuitDecision{
				ShouldShortCircuit: false,
				Reason:             "tie detected, waiting for tiebreaker",
			},
		},

		// Priority 4: Mathematical guarantee
		{
			Priority: 80,
			Name:     "math-guarantee",
			Condition: func(dims DecisionDimensions) bool {
				// With AcceptBestValidResult, we should wait for all responses
				// to find the best one, even with a mathematical guarantee
				if dims.DisputeBehavior == common.ConsensusDisputeBehaviorAcceptBestValidResult && dims.HasRemaining {
					return false
				}
				return dims.CompetitionState == CompetitionStateUnbeatable &&
					!dims.IsLowParticipants &&
					dims.ConsensusState != ConsensusStateNone
			},
			Decision: ShortCircuitDecision{
				ShouldShortCircuit: true,
				Reason:             "mathematical guarantee - winner cannot be beaten",
			},
		},

		// Priority 5: Consensus reached on non-empty with threshold
		{
			Priority: 70,
			Name:     "consensus-non-empty",
			Condition: func(dims DecisionDimensions) bool {
				// With AcceptBestValidResult, wait for all responses
				if dims.DisputeBehavior == common.ConsensusDisputeBehaviorAcceptBestValidResult && dims.HasRemaining {
					return false
				}
				// Only short-circuit if we have mathematical guarantee or no remaining responses
				// This prevents premature short-circuit when preferences are disabled
				return dims.WinnerType == ResponseTypeNonEmpty &&
					dims.ConsensusState == ConsensusStateThresholdMet &&
					!dims.IsLowParticipants &&
					dims.CompetitionState == CompetitionStateUnbeatable
			},
			Decision: ShortCircuitDecision{
				ShouldShortCircuit: true,
				Reason:             "consensus reached on non-empty result",
			},
		},

		// Priority 6: Consensus impossible
		{
			Priority: 60,
			Name:     "consensus-impossible",
			Condition: func(dims DecisionDimensions) bool {
				// This needs to be determined by the caller
				// We'll handle this in the main function
				return false
			},
			Decision: ShortCircuitDecision{
				ShouldShortCircuit: true,
				Reason:             "consensus impossible to reach",
			},
		},

		// Default: Continue waiting
		{
			Priority: 0,
			Name:     "default-wait",
			Condition: func(dims DecisionDimensions) bool {
				return true // Always matches as fallback
			},
			Decision: ShortCircuitDecision{
				ShouldShortCircuit: false,
				Reason:             "waiting for more responses",
			},
		},
	}
}

// evaluateDecisionMatrix applies the decision rules to find the appropriate decision
func (e *executor[R]) evaluateDecisionMatrix(dims DecisionDimensions, consensusImpossible bool) ShortCircuitDecision {
	// Special handling for consensus impossible
	if consensusImpossible {
		// Check if we should still wait despite consensus being impossible
		if dims.WinnerType == ResponseTypeNonEmpty && e.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult {
			return ShortCircuitDecision{
				ShouldShortCircuit: false,
				Reason:             "consensus impossible but have non-empty results with AcceptMostCommonValidResult",
			}
		}
		// AcceptBestValidResult should wait for all responses to find the best one
		// But only if we have remaining responses to collect
		if dims.HasRemaining && e.disputeBehavior == common.ConsensusDisputeBehaviorAcceptBestValidResult {
			// With AcceptBestValidResult, we want to collect all responses to find the best one
			// according to priority ordering (non-empty > empty > errors)
			return ShortCircuitDecision{
				ShouldShortCircuit: false,
				Reason:             "consensus impossible but waiting for all responses with AcceptBestValidResult",
			}
		}
		// For PreferBlockHeadLeader and OnlyBlockHeadLeader behaviors, we need to continue
		// to evaluate potential block head leaders even when consensus is impossible
		if e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader ||
			e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader {
			// Continue to collect more responses to potentially find a block head leader
			if dims.HasRemaining {
				return ShortCircuitDecision{
					ShouldShortCircuit: false,
					Reason:             "consensus impossible but waiting for block head leader",
				}
			}
		}
		// Also check lowParticipantsBehavior for AcceptBestValidResult
		if dims.HasRemaining && e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptBestValidResult {
			return ShortCircuitDecision{
				ShouldShortCircuit: false,
				Reason:             "consensus impossible but waiting for all responses with AcceptBestValidResult (low participants)",
			}
		}
		return ShortCircuitDecision{
			ShouldShortCircuit: true,
			Reason:             "consensus impossible to reach",
		}
	}

	// Apply rules in priority order
	for _, rule := range decisionRules {
		if rule.Condition(dims) {
			return rule.Decision
		}
	}

	// Should never reach here due to default rule, but just in case
	return ShortCircuitDecision{
		ShouldShortCircuit: false,
		Reason:             "no matching rule, defaulting to wait",
	}
}

// checkShortCircuit determines if we can exit early based on current responses
func (e *executor[R]) checkShortCircuit(lg *zerolog.Logger, responses []*execResult[R], exec policy.ExecutionInternal[R]) bool {
	// Analyze current responses
	analysis := e.analyzeResponses(lg, responses, exec)
	remaining := e.maxParticipants - len(responses)

	// Build decision dimensions
	dims := DecisionDimensions{
		WinnerType:            getResponseType(analysis.winnerGroup),
		ConsensusState:        e.getConsensusState(analysis),
		CompetitionState:      e.analyzeCompetitionState(analysis, remaining),
		HasRemaining:          remaining > 0,
		IsLowParticipants:     analysis.isLowParticipants,
		PreferNonEmpty:        e.preferNonEmpty,
		PreferLargerResponses: e.preferLargerResponses,
		DisputeBehavior:       e.disputeBehavior,
	}

	// Check if consensus is impossible
	canReachConsensus := false
	for _, group := range analysis.responsesByHash {
		if group.count+remaining >= e.agreementThreshold {
			canReachConsensus = true
			break
		}
	}

	expectedUniqueParticipants := analysis.totalValidParticipants + remaining
	consensusImpossible := !canReachConsensus && expectedUniqueParticipants >= e.maxParticipants

	// Evaluate decision matrix
	decision := e.evaluateDecisionMatrix(dims, consensusImpossible)

	// Log decision with full context
	lg.Debug().
		Int("responseCount", len(responses)).
		Int("winnerCount", analysis.winnerCount).
		Int("remainingResponses", remaining).
		Str("winnerType", fmt.Sprintf("%v", dims.WinnerType)).
		Str("consensusState", fmt.Sprintf("%v", dims.ConsensusState)).
		Str("competitionState", fmt.Sprintf("%v", dims.CompetitionState)).
		Bool("isLowParticipants", dims.IsLowParticipants).
		Bool("consensusImpossible", consensusImpossible).
		Bool("preferNonEmpty", e.preferNonEmpty).
		Bool("preferLargerResponses", e.preferLargerResponses).
		Int("agreementThreshold", e.agreementThreshold).
		Bool("decision", decision.ShouldShortCircuit).
		Str("reason", decision.Reason).
		Msg("decision matrix short-circuit evaluation")

	return decision.ShouldShortCircuit
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

	// With AcceptBestValidResult, always use the best selection logic
	// regardless of whether consensus was achieved
	if hasConsensus && e.disputeBehavior != common.ConsensusDisputeBehaviorAcceptBestValidResult {
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
		// ALL responses count as participants, even generic errors
		// This ensures we have accurate participant counting
		// The priority ordering will handle which responses are selected
		if r.upstream != nil && r.upstream.Config() != nil {
			uniqueParticipants[r.upstream.Config().Id] = true
		}

		// Calculate hash for this response
		resultHash, err := e.resultOrErrorToHash(r.result, r.err, exec)

		// Handle unhashable responses (including generic/non-consensus errors)
		if err != nil || resultHash == "" {
			if r.err != nil {
				// ALL errors should be included, even generic ones
				// They have lowest priority but still participate
				resultHash = e.errorToConsensusHash(r.err)
				if resultHash == "" {
					// If we still can't get a hash, use a generic hash for this error type
					// This ensures the error is still counted
					resultHash = "generic-error"
				}
				lg.Debug().Err(r.err).Str("hash", resultHash).Msg("including generic/server-side error in consensus")
			} else {
				// Skip responses without error or result
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

				// Calculate and cache response size once per group
				if jr := e.resultToJsonRpcResponse(r.result, exec); jr != nil {
					if size, err := jr.Size(exec.Context()); err == nil {
						group.responseSize = size
					}
				}
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

	// Determine winner with configurable preferences
	analysis.determineWinner(lg, e)

	// Determine if we have low participants - check if we have enough participants
	// to potentially reach the agreement threshold (dispute vs low participants)
	analysis.isLowParticipants = participantCount < e.agreementThreshold

	return analysis
}

// determineWinner applies strict priority ordering to find the consensus winner
// Priority order (highest to lowest):
// 1. Largest non-empty result (if preferLargerResponses enabled)
// 2. Non-empty results (by count)
// 3. Empty results (by count)
// 4. Consensus-valid errors (by count)
// 5. Generic errors (by count)
func (a *consensusAnalysis[R]) determineWinner(lg *zerolog.Logger, e *executor[R]) {
	preferNonEmpty := e.preferNonEmpty
	preferLargerResponses := e.preferLargerResponses

	if preferNonEmpty {
		// Priority 1: Non-empty results (prefer larger if enabled)
		if len(a.nonEmptyGroups) > 0 {
			bestHash, bestGroup := a.findBestGroup(a.nonEmptyGroups, preferLargerResponses, e.agreementThreshold)
			if bestGroup != nil {
				a.winnerHash = bestHash
				a.winnerCount = bestGroup.count
				a.winnerGroup = bestGroup

				lg.Debug().
					Str("winnerHash", a.winnerHash).
					Int("winnerCount", a.winnerCount).
					Int("winnerSize", bestGroup.responseSize).
					Int("totalNonEmpty", len(a.nonEmptyGroups)).
					Int("agreementThreshold", e.agreementThreshold).
					Bool("preferLargerResponses", preferLargerResponses).
					Bool("metThreshold", bestGroup.count >= e.agreementThreshold).
					Msg("winner selected from non-empty results (highest priority)")
			}
			return
		}

		// Priority 2: Empty results (successful but empty)
		if len(a.emptyGroups) > 0 {
			bestHash, bestGroup := a.findBestGroup(a.emptyGroups, false, e.agreementThreshold) // Never use size preference for empty
			if bestGroup != nil {
				a.winnerHash = bestHash
				a.winnerCount = bestGroup.count
				a.winnerGroup = bestGroup

				lg.Debug().
					Str("winnerHash", a.winnerHash).
					Int("winnerCount", a.winnerCount).
					Int("totalEmpty", len(a.emptyGroups)).
					Msg("winner selected from empty results (priority over errors)")
			}
			return
		}

		// Priority 3: Errors (lowest priority)
		if len(a.errorGroups) > 0 {
			bestHash, bestGroup := a.findBestGroup(a.errorGroups, false, e.agreementThreshold) // Never use size preference for errors
			if bestGroup != nil {
				a.winnerHash = bestHash
				a.winnerCount = bestGroup.count
				a.winnerGroup = bestGroup

				lg.Debug().
					Str("winnerHash", a.winnerHash).
					Int("winnerCount", a.winnerCount).
					Int("totalErrors", len(a.errorGroups)).
					Msg("winner selected from errors (lowest priority)")
			}
		}
	} else {
		// When preferNonEmpty is false, treat all response types equally by count
		// But still never apply size preference to errors or empty responses
		allGroups := make(map[string]*responseGroup[R])

		// Collect all groups regardless of type
		for hash, group := range a.nonEmptyGroups {
			allGroups[hash] = group
		}
		for hash, group := range a.errorGroups {
			allGroups[hash] = group
		}
		for hash, group := range a.emptyGroups {
			allGroups[hash] = group
		}

		if len(allGroups) > 0 {
			// Only apply size preference to non-empty, non-error responses
			bestHash, bestGroup := a.findBestGroup(allGroups, preferLargerResponses, e.agreementThreshold)
			if bestGroup != nil {
				a.winnerHash = bestHash
				a.winnerCount = bestGroup.count
				a.winnerGroup = bestGroup

				responseType := labelRespTypeUnknown
				if !bestGroup.isError {
					if bestGroup.isEmpty {
						responseType = labelRespTypeEmpty
					} else {
						responseType = labelRespTypeNonEmpty
					}
				} else {
					responseType = labelRespTypeError
				}

				lg.Debug().
					Str("winnerHash", a.winnerHash).
					Int("winnerCount", a.winnerCount).
					Int("winnerSize", bestGroup.responseSize).
					Str("winnerType", responseType).
					Int("agreementThreshold", e.agreementThreshold).
					Bool("preferLargerResponses", preferLargerResponses).
					Bool("metThreshold", bestGroup.count >= e.agreementThreshold).
					Msg("winner selected from all results (no priority preference)")
			}
		}
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
	case common.ConsensusLowParticipantsBehaviorAcceptBestValidResult:
		return e.handleAcceptBestValid(lg, analysis, func() error {
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
	case common.ConsensusDisputeBehaviorAcceptBestValidResult:
		return e.handleAcceptBestValid(lg, analysis, func() error {
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
	logger.Debug().Msg("finding most common valid result following priority ordering WITH threshold requirement")

	// Priority ordering (MUST meet threshold):
	// 1. Non-empty results meeting threshold (prefer larger if enabled)
	// 2. Empty results meeting threshold
	// 3. Consensus-valid errors meeting threshold
	// If nothing meets threshold, return error

	// Priority 1: Non-empty results that MEET THRESHOLD
	if len(analysis.nonEmptyGroups) > 0 {
		var bestGroup *responseGroup[R]
		var bestHash string

		// Only consider non-empty results that meet threshold
		if e.preferLargerResponses {
			// Find the largest non-empty response that meets threshold
			maxSize := 0
			for hash, group := range analysis.nonEmptyGroups {
				if group.count >= e.agreementThreshold {
					if group.responseSize > maxSize || (group.responseSize == maxSize && (bestGroup == nil || group.count > bestGroup.count)) {
						maxSize = group.responseSize
						bestGroup = group
						bestHash = hash
					}
				}
			}
		} else {
			// Find the non-empty response with highest count that meets threshold
			maxCount := 0
			for hash, group := range analysis.nonEmptyGroups {
				if group.count >= e.agreementThreshold && group.count > maxCount {
					maxCount = group.count
					bestGroup = group
					bestHash = hash
				}
			}
		}

		if bestGroup != nil {
			// Check for ties at this count level among threshold-meeting groups
			tieCount := 0
			var tiedGroups []*responseGroup[R]
			for _, group := range analysis.nonEmptyGroups {
				if group.count == bestGroup.count && group.count >= e.agreementThreshold {
					tieCount++
					tiedGroups = append(tiedGroups, group)
				}
			}

			// If there's a tie and we can't break it with size preference, return error
			if tieCount > 1 {
				// With preferLargerResponses, we can break ties by size
				if e.preferLargerResponses {
					// Check if all tied groups have the same size
					allSameSize := true
					if len(tiedGroups) > 1 {
						firstSize := tiedGroups[0].responseSize
						for _, group := range tiedGroups[1:] {
							if group.responseSize != firstSize {
								allSameSize = false
								break
							}
						}
					}
					// If all have same size, it's still a tie
					if allSameSize {
						logger.Debug().
							Int("tieCount", tieCount).
							Int("count", bestGroup.count).
							Msg("multiple non-empty results have same count and size, cannot determine most common")
						return &failsafeCommon.PolicyResult[R]{
							Error: errFn(),
						}
					}
				} else {
					// Without size preference, any tie is unresolvable
					logger.Debug().
						Int("tieCount", tieCount).
						Int("count", bestGroup.count).
						Msg("multiple non-empty results have same count, cannot determine most common")
					return &failsafeCommon.PolicyResult[R]{
						Error: errFn(),
					}
				}
			}

			logger.Debug().
				Str("hash", bestHash).
				Int("count", bestGroup.count).
				Int("size", bestGroup.responseSize).
				Int("threshold", e.agreementThreshold).
				Msg("accepting most common non-empty result meeting threshold")
			return &failsafeCommon.PolicyResult[R]{
				Result: bestGroup.firstResult,
			}
		}
	}

	// Priority 2: Empty results that MEET THRESHOLD
	if len(analysis.emptyGroups) > 0 {
		var bestGroup *responseGroup[R]
		maxCount := 0

		// Find empty result with highest count that meets threshold
		for _, group := range analysis.emptyGroups {
			if group.count >= e.agreementThreshold && group.count > maxCount {
				maxCount = group.count
				bestGroup = group
			}
		}

		if bestGroup != nil {
			// Check for ties among threshold-meeting groups
			tieCount := 0
			for _, group := range analysis.emptyGroups {
				if group.count == bestGroup.count && group.count >= e.agreementThreshold {
					tieCount++
				}
			}

			if tieCount > 1 {
				logger.Debug().
					Int("tieCount", tieCount).
					Int("count", bestGroup.count).
					Msg("multiple empty results have same count, cannot determine most common")
				return &failsafeCommon.PolicyResult[R]{
					Error: errFn(),
				}
			}

			logger.Debug().
				Int("count", bestGroup.count).
				Int("threshold", e.agreementThreshold).
				Msg("accepting most common empty result meeting threshold")
			return &failsafeCommon.PolicyResult[R]{
				Result: bestGroup.firstResult,
			}
		}
	}

	// Priority 3: Errors that MEET THRESHOLD (lowest priority)
	if len(analysis.errorGroups) > 0 {
		var bestGroup *responseGroup[R]
		maxCount := 0

		// Find error with highest count that meets threshold
		for _, group := range analysis.errorGroups {
			if group.count >= e.agreementThreshold && group.count > maxCount {
				maxCount = group.count
				bestGroup = group
			}
		}

		if bestGroup != nil && bestGroup.firstError != nil {
			// Check for ties among threshold-meeting groups
			tieCount := 0
			for _, group := range analysis.errorGroups {
				if group.count == bestGroup.count && group.count >= e.agreementThreshold {
					tieCount++
				}
			}

			if tieCount > 1 {
				logger.Debug().
					Int("tieCount", tieCount).
					Int("count", bestGroup.count).
					Msg("multiple errors have same count, cannot determine most common")
				return &failsafeCommon.PolicyResult[R]{
					Error: errFn(),
				}
			}

			logger.Debug().
				Err(bestGroup.firstError).
				Int("count", bestGroup.count).
				Int("threshold", e.agreementThreshold).
				Msg("returning most common error meeting threshold (lowest priority)")
			return &failsafeCommon.PolicyResult[R]{
				Error: bestGroup.firstError,
			}
		}
	}

	// No result found
	logger.Debug().Msg("no most common result found")
	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}

func (e *executor[R]) handleAcceptBestValid(logger *zerolog.Logger, analysis *consensusAnalysis[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	logger.Debug().Msg("accepting best valid result following priority ordering")

	// Priority ordering (ignoring threshold):
	// 1. Largest non-empty (if preferLargerResponses enabled)
	// 2. Non-empty results (by count)
	// 3. Empty results (by count)
	// 4. Consensus-valid errors (by count)
	// 5. Generic errors (by count)

	// Priority 1 & 2: Non-empty results
	if len(analysis.nonEmptyGroups) > 0 {
		// Find the best non-empty result (largest if preferLargerResponses, otherwise highest count)
		var bestGroup *responseGroup[R]
		var bestHash string

		if e.preferLargerResponses {
			// Find the largest non-empty response
			maxSize := 0
			for hash, group := range analysis.nonEmptyGroups {
				if group.responseSize > maxSize || (group.responseSize == maxSize && (bestGroup == nil || group.count > bestGroup.count)) {
					maxSize = group.responseSize
					bestGroup = group
					bestHash = hash
				}
			}
		} else {
			// Find the non-empty response with highest count
			maxCount := 0
			for hash, group := range analysis.nonEmptyGroups {
				if group.count > maxCount {
					maxCount = group.count
					bestGroup = group
					bestHash = hash
				}
			}
		}

		if bestGroup != nil {
			upstreamId := ""
			if bestGroup.responses[0].upstream != nil && bestGroup.responses[0].upstream.Config() != nil {
				upstreamId = bestGroup.responses[0].upstream.Config().Id
			}
			logger.Debug().
				Str("upstream", upstreamId).
				Str("hash", bestHash).
				Int("count", bestGroup.count).
				Int("size", bestGroup.responseSize).
				Bool("preferLargerResponses", e.preferLargerResponses).
				Msg("selected best non-empty result (highest priority)")
			return &failsafeCommon.PolicyResult[R]{
				Result: bestGroup.firstResult,
			}
		}
	}

	// Priority 3: Empty results (by count)
	if len(analysis.emptyGroups) > 0 {
		// Find the empty response with highest count
		var bestGroup *responseGroup[R]
		maxCount := 0
		for _, group := range analysis.emptyGroups {
			if group.count > maxCount {
				maxCount = group.count
				bestGroup = group
			}
		}

		if bestGroup != nil {
			upstreamId := ""
			if bestGroup.responses[0].upstream != nil && bestGroup.responses[0].upstream.Config() != nil {
				upstreamId = bestGroup.responses[0].upstream.Config().Id
			}
			logger.Debug().
				Str("upstream", upstreamId).
				Int("count", bestGroup.count).
				Msg("selected best empty result (priority over errors)")
			return &failsafeCommon.PolicyResult[R]{
				Result: bestGroup.firstResult,
			}
		}
	}

	// Priority 4: Consensus-valid errors (by count)
	if len(analysis.errorGroups) > 0 {
		// Find the error with highest count
		var bestGroup *responseGroup[R]
		maxCount := 0
		for _, group := range analysis.errorGroups {
			if group.count > maxCount {
				maxCount = group.count
				bestGroup = group
			}
		}

		if bestGroup != nil && bestGroup.firstError != nil {
			logger.Debug().
				Err(bestGroup.firstError).
				Int("count", bestGroup.count).
				Msg("selected best error result (lowest priority)")
			return &failsafeCommon.PolicyResult[R]{
				Error: bestGroup.firstError,
			}
		}
	}

	// No valid result found at all
	logger.Debug().Msg("no valid results found")
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
	// PreferBlockHeadLeader: Use leader if exists, otherwise fallback to priority ordering
	blockLeaderResult := e.findBlockHeadLeaderResult(ctx, analysis)
	if blockLeaderResult != nil {
		logger.Debug().Msg("found block head leader result")
		if blockLeaderResult.err != nil {
			logger.Debug().Err(blockLeaderResult.err).Msg("returning block head leader error")
			return &failsafeCommon.PolicyResult[R]{
				Error: blockLeaderResult.err,
			}
		}
		logger.Debug().Msg("returning block head leader successful result")
		return &failsafeCommon.PolicyResult[R]{
			Result: blockLeaderResult.result,
		}
	}

	// No block head leader found - fallback to priority ordering with threshold
	logger.Debug().Msg("no block head leader found, falling back to priority ordering")
	return e.handleAcceptMostCommon(logger, analysis, errFn)
}

func (e *executor[R]) handleOnlyBlockHeadLeader(ctx context.Context, logger *zerolog.Logger, analysis *consensusAnalysis[R], errFn func() error) *failsafeCommon.PolicyResult[R] {
	// OnlyBlockHeadLeader: Use leader response or error if leader didn't participate
	blockLeaderResult := e.findBlockHeadLeaderResult(ctx, analysis)
	if blockLeaderResult != nil {
		logger.Debug().Msg("found block head leader result")
		if blockLeaderResult.err != nil {
			logger.Debug().Err(blockLeaderResult.err).Msg("returning block head leader error")
			return &failsafeCommon.PolicyResult[R]{
				Error: blockLeaderResult.err,
			}
		}
		logger.Debug().Msg("returning block head leader successful result")
		return &failsafeCommon.PolicyResult[R]{
			Result: blockLeaderResult.result,
		}
	}

	logger.Debug().Msg("block head leader did not participate, returning error")
	return &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}
