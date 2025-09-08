package consensus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"runtime/debug"
	"strconv"

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
)

type metricsLabels struct {
	method      string
	category    string
	networkId   string
	projectId   string
	finalityStr string
}

// ResponseType classifies the type of response for clear decision making.
type ResponseType int

const (
	ResponseTypeNonEmpty ResponseType = iota
	ResponseTypeEmpty
	ResponseTypeConsensusError
	ResponseTypeInfrastructureError
)

func (rt ResponseType) String() string {
	switch rt {
	case ResponseTypeNonEmpty:
		return "non_empty"
	case ResponseTypeEmpty:
		return "empty"
	case ResponseTypeConsensusError:
		return "consensus_error"
	case ResponseTypeInfrastructureError:
		return "infrastructure_error"
	default:
		return "unknown"
	}
}

// executor implements the Failsafe policy executor for consensus.
type executor struct {
	*policy.BaseExecutor[*common.NormalizedResponse]
	*consensusPolicy
}

var _ policy.Executor[*common.NormalizedResponse] = &executor{}

// execResult holds the result from a single upstream execution with cached analysis.
type execResult struct {
	Result   *common.NormalizedResponse
	Err      error
	Upstream common.Upstream

	// Cached values to avoid re-computation
	CachedHash         string
	CachedResponseType ResponseType
	CachedResponseSize int
	// Index of the attempt that produced this result
	Index int
}

// Apply is the main entry point for the consensus policy. It orchestrates the collection,
// analysis, and decision phases.
func (e *executor) Apply(innerFn func(failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse]) func(failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
		startTime := time.Now()
		ctx := exec.Context()

		// Extract request and prepare tracing/logging context.
		originalReq, ok := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
		if !ok || originalReq == nil {
			e.logger.Error().Msg("Unexpected nil request in consensus policy")
			return innerFn(exec) // Fallback to simple execution
		}

		labels := e.extractMetricsLabels(ctx, originalReq)
		ctx, consensusSpan := e.startConsensusSpan(ctx, labels, exec)
		defer consensusSpan.End()

		lg := e.logger.With().
			Interface("id", originalReq.ID()).
			Str("component", "consensus").
			Str("networkId", labels.networkId).
			Logger()

		winner, analysis := e.executeConsensus(
			ctx,
			&lg,
			originalReq,
			labels,
			exec.(policy.ExecutionInternal[*common.NormalizedResponse]),
			innerFn,
		)

		// Track misbehaviors while responses are still available
		e.trackAndPunishMisbehavingUpstreams(&lg, originalReq, labels, winner, analysis)

		// Now release non-winning responses to free memory
		if analysis != nil {
			var winnerResp *common.NormalizedResponse
			if winner != nil {
				if wr, ok := any(winner.Result).(*common.NormalizedResponse); ok {
					winnerResp = wr
				}
			}
			// Release responses from the groups in analysis
			for _, group := range analysis.groups {
				for _, result := range group.Results {
					if result != nil && result.Result != nil {
						// Only release if it's not the winner
						if result.Result != winnerResp {
							result.Result.Release()
						}
					}
				}
			}
		}

		// --- Finalization ---
		e.recordMetricsAndTracing(originalReq, startTime, winner, analysis, labels, consensusSpan)

		return winner
	}
}

func (e *executor) executeConsensus(
	ctx context.Context,
	lg *zerolog.Logger,
	originalReq *common.NormalizedRequest,
	labels metricsLabels,
	parentExecution policy.ExecutionInternal[*common.NormalizedResponse],
	innerFn func(failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse],
) (*failsafeCommon.PolicyResult[*common.NormalizedResponse], *consensusAnalysis) {
	ctx, collectionSpan := common.StartDetailSpan(ctx, "Consensus.CollectResponses")
	defer collectionSpan.End()

	cancellableCtx, cancelRemaining := context.WithCancel(ctx)
	defer cancelRemaining()

	// Spawn only as many participants as configured by policy
	maxToSpawn := e.maxParticipants
	if maxToSpawn <= 0 {
		maxToSpawn = 1
	}
	responseChan := make(chan *execResult, maxToSpawn)
	// Prepare and retain per-attempt executions so we can cancel losers explicitly
	attempts := make([]policy.ExecutionInternal[*common.NormalizedResponse], maxToSpawn)
	for i := 0; i < maxToSpawn; i++ {
		attempts[i] = parentExecution.CopyForCancellableWithValue(common.RequestContextKey, originalReq).(policy.ExecutionInternal[*common.NormalizedResponse])
		go e.executeParticipant(cancellableCtx, lg, attempts[i], labels, innerFn, i, responseChan)
	}

	responses := make([]*execResult, 0, maxToSpawn)
	var shortCircuited bool
	var shortCircuitReason string
	var analysis *consensusAnalysis
	var winner *failsafeCommon.PolicyResult[*common.NormalizedResponse]

collectLoop:
	for i := 0; i < maxToSpawn; i++ {
		select {
		case resp := <-responseChan:
			if resp != nil {
				responses = append(responses, resp)
				if !shortCircuited {
					analysis = newConsensusAnalysis(e.logger, parentExecution, e.config, responses)
					winner = e.determineWinner(lg, analysis)
					if reason, ok := e.shouldShortCircuit(winner, analysis); ok {
						shortCircuited = true
						shortCircuitReason = reason
						cancelRemaining()
						// Explicitly cancel all outstanding attempt executions to abort in-flight work
						for ai := range attempts {
							if attempts[ai] != nil {
								attempts[ai].Cancel(nil)
							}
						}
						go func() {
							for j := i + 1; j < maxToSpawn; j++ {
								er := <-responseChan // Drain remaining
								if er != nil && er.Result != nil {
									if releasable, ok := any(er.Result).(interface{ Release() }); ok && releasable != nil {
										releasable.Release()
									}
								}
							}
						}()
						break collectLoop
					}
				}
			}
		case <-ctx.Done():
			lg.Warn().Err(ctx.Err()).Msg("Context cancelled during response collection")
			cancelRemaining()
			// Record collection phase cancellation
			telemetry.MetricConsensusCancellations.
				WithLabelValues(labels.projectId, labels.networkId, labels.category, "collection", labels.finalityStr).
				Inc()
			// Best-effort cancel all attempts when parent context is done
			for ai := range attempts {
				if attempts[ai] != nil {
					attempts[ai].Cancel(nil)
				}
			}
			// Drain remaining responses and release any results to avoid retention
			go func(startIdx int) {
				for j := startIdx; j < maxToSpawn; j++ {
					er := <-responseChan
					if er != nil && er.Result != nil {
						if releasable, ok := any(er.Result).(interface{ Release() }); ok && releasable != nil {
							releasable.Release()
						}
					}
				}
			}(i + 1)
			break collectLoop
		}
	}

	if analysis == nil {
		analysis = newConsensusAnalysis(e.logger, parentExecution, e.config, responses)
		winner = e.determineWinner(lg, analysis)
	}

	collectionSpan.SetAttributes(
		attribute.Bool("short_circuited", shortCircuited),
		attribute.Int("responses.collected", len(responses)),
	)
	// Record how many responses were collected and whether we short-circuited
	telemetry.MetricConsensusResponsesCollected.
		WithLabelValues(labels.projectId, labels.networkId, labels.category, strconv.FormatBool(shortCircuited), labels.finalityStr).
		Observe(float64(len(responses)))
	if shortCircuited {
		reason := shortCircuitReason
		if reason == "" {
			reason = "unknown"
		}
		telemetry.MetricConsensusShortCircuit.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, reason, labels.finalityStr).
			Inc()
	}

	return winner, analysis
}

// executeParticipant runs a single upstream request within a goroutine.
func (e *executor) executeParticipant(
	ctx context.Context,
	lg *zerolog.Logger,
	attemptExecution policy.ExecutionInternal[*common.NormalizedResponse],
	labels metricsLabels,
	innerFn func(failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse],
	index int,
	responseChan chan<- *execResult,
) {
	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			lg.Error().
				Interface("panic", r).
				Int("index", index).
				Str("stack", string(debug.Stack())).
				Msg("Panic in consensus participant")
			telemetry.MetricConsensusPanics.WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr).Inc()
			responseChan <- &execResult{Err: errPanicInConsensus}
		}
	}()

	// Check for cancellation before execution
	if ctx.Err() != nil {
		telemetry.MetricConsensusCancellations.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, "before_execution", labels.finalityStr).
			Inc()
		responseChan <- nil
		return
	}

	// Execute using the pre-created cancellable attempt execution
	result := innerFn(attemptExecution)

	// Check for cancellation after execution; release any produced result before dropping it
	if ctx.Err() != nil {
		telemetry.MetricConsensusCancellations.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, "after_execution", labels.finalityStr).
			Inc()
		if result != nil {
			if releasable, ok := any(result.Result).(interface{ Release() }); ok && releasable != nil {
				releasable.Release()
			}
		}
		responseChan <- nil
		return
	}

	if result == nil {
		responseChan <- nil
		return
	}

	var upstream common.Upstream
	if resp, ok := any(result.Result).(*common.NormalizedResponse); ok {
		upstream = resp.Upstream()
	}
	if upstream == nil && result.Error != nil {
		var uae interface{ Upstream() common.Upstream }
		if errors.As(result.Error, &uae) {
			upstream = uae.Upstream()
		}
		var uxe *common.ErrUpstreamsExhausted
		if errors.As(result.Error, &uxe) {
			if ups := uxe.Upstreams(); len(ups) > 0 {
				upstream = ups[0]
			}
		}
	}

	// It is possible that result.Result is nil (pure error); in that case, we still propagate the error
	var nr *common.NormalizedResponse
	if rr, ok := any(result.Result).(*common.NormalizedResponse); ok {
		nr = rr
	}
	responseChan <- &execResult{
		Result:   nr,
		Err:      result.Error,
		Upstream: upstream,
		Index:    index,
	}
}

// shouldShortCircuit decides if remaining requests can be safely cancelled.
// This happens if one group's lead over the second-place group is greater
// than the number of remaining responses.
func (e *executor) shouldShortCircuit(winner *failsafeCommon.PolicyResult[*common.NormalizedResponse], analysis *consensusAnalysis) (string, bool) {
	for _, rule := range shortCircuitRules {
		if rule.Condition(winner, analysis) {
			return rule.Reason, true
		}
	}
	return "", false
}

// determineWinner applies configured policies to the analysis to produce a final result.
// It uses a rules-based approach for clear, maintainable decision logic.
func (e *executor) determineWinner(lg *zerolog.Logger, analysis *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	// Since we know R is *common.NormalizedResponse at runtime, we can safely work with it
	// Evaluate rules in priority order
	for _, rule := range consensusRules {
		// We need to check the condition with the proper type
		// Since the rules are defined for *common.NormalizedResponse, we need to handle this carefully
		if rule.Condition(analysis) {
			lg.Debug().
				Str("rule", rule.Description).
				Msg("consensus rule matched")
			return rule.Action(analysis)
		}
	}

	// Ultimate fallback (should never reach here due to no-winner rule)
	lg.Error().Msg("no consensus rule matched - using fallback")
	return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
		Error: common.NewErrConsensusDispute("no consensus rule matched", nil, nil),
	}
}

// --- Tracing, Metrics, and Punishment ---

func (e *executor) trackAndPunishMisbehavingUpstreams(lg *zerolog.Logger, req *common.NormalizedRequest, labels metricsLabels, winner *failsafeCommon.PolicyResult[*common.NormalizedResponse], analysis *consensusAnalysis) {
	// Skip tracking when there are no valid participants (all infra errors)
	if analysis.validParticipants == 0 {
		return
	}

	// Determine the consensus group based on the actual winner result
	// This ensures we track misbehavior against what was actually returned, not just the majority
	var consensusGroup *responseGroup

	// If we have a successful response, find the group that contains it
	if winner != nil && winner.Result != nil {
		if winnerResp, ok := any(winner.Result).(*common.NormalizedResponse); ok && winnerResp != nil {
			// Find the group containing this exact response
			for _, group := range analysis.groups {
				for _, result := range group.Results {
					if result != nil && result.Result == winnerResp {
						consensusGroup = group
						break
					}
				}
				if consensusGroup != nil {
					break
				}
			}
		}
	}

	// If no consensus group found from winner result, use the best group by count
	if consensusGroup == nil {
		for _, g := range analysis.getValidGroups() {
			if consensusGroup == nil || g.Count > consensusGroup.Count {
				consensusGroup = g
			}
		}
	}

	if consensusGroup == nil {
		return
	}

	// Track different types of disagreements
	consensusSize := consensusGroup.ResponseSize

	// Determine if the dispute log level is enabled; if not, avoid heavy preparation
	shouldLog := e.logger.GetLevel() <= e.disputeLogLevel

	// Only define participant tracking structures if we'll actually log
	type participantInfo struct {
		upstreamId          string
		upstream            common.Upstream
		responseType        ResponseType
		responseHash        string
		responseSize        int
		responseBody        []byte // Full response content for debugging disputes
		errorMessage        string
		agreesWithConsensus bool
	}
	var allParticipants []participantInfo
	misbehavingParticipants := ""
	if shouldLog {
		allParticipants = make([]participantInfo, 0, analysis.totalParticipants)
	}
	var misbehavingCount int

	for _, group := range analysis.groups {
		agreesWithConsensus := group.Hash == consensusGroup.Hash
		largerThanConsensus := group.ResponseSize > consensusSize
		largerThanConsensusStr := strconv.FormatBool(largerThanConsensus)

		for _, result := range group.Results {
			if result == nil || result.Upstream == nil {
				continue
			}

			upstreamId := result.Upstream.Id()

			// Only collect participant details if we'll actually log them
			if shouldLog {
				var responseHash string
				var responseSize int
				var responseBody []byte
				var errorMessage string

				if result.Result != nil {
					if jrr, err := result.Result.JsonRpcResponse(); err == nil && jrr != nil {
						responseSize = jrr.ResultLength()
						responseHash = result.CachedHash
						// Get the full response body for debugging
						responseBody = jrr.GetResultBytes()

						// Debug: Log if we have a mismatch between empty response and non-empty type
						if group.ResponseType == ResponseTypeNonEmpty && jrr.IsResultEmptyish() {
							lg.Warn().
								Str("upstream", upstreamId).
								Str("responseType", group.ResponseType.String()).
								RawJSON("result", responseBody).
								Msg("WARN: Response marked as non_empty but IsResultEmptyish returns true")
						}
					} else {
						// Couldn't extract JsonRpcResponse
						responseBody, _ = common.SonicCfg.Marshal(map[string]interface{}{
							"error": fmt.Sprintf("<error extracting response: %v>", err),
						})
					}
				} else if result.Err != nil {
					// For errors, include the full error details as the "body"
					responseBody, _ = common.SonicCfg.Marshal(map[string]interface{}{
						"error": fmt.Sprintf("<error: %v>", result.Err),
					})
				}

				if result.Err != nil {
					errorMessage = common.ErrorSummary(result.Err)
				}

				// Add to participants list for logging
				allParticipants = append(allParticipants, participantInfo{
					upstreamId:          upstreamId,
					upstream:            result.Upstream,
					responseType:        group.ResponseType,
					responseHash:        responseHash,
					responseSize:        responseSize,
					responseBody:        responseBody,
					errorMessage:        errorMessage,
					agreesWithConsensus: agreesWithConsensus,
				})

				if !agreesWithConsensus {
					if misbehavingParticipants == "" {
						misbehavingParticipants = upstreamId
					} else {
						misbehavingParticipants += fmt.Sprintf(",%s", upstreamId)
					}
				}
			}

			// Track errors separately - these are NOT misbehavior
			if group.ResponseType == ResponseTypeConsensusError || group.ResponseType == ResponseTypeInfrastructureError {
				// Extract error code
				errorCode := "unknown"
				if result.Err != nil {
					if common.HasErrorCode(result.Err, common.ErrCodeEndpointMissingData) {
						errorCode = "ErrEndpointMissingData"
					} else if common.HasErrorCode(result.Err, common.ErrCodeEndpointServerSideException) {
						errorCode = "ErrEndpointServerSideException"
					} else if se, ok := result.Err.(common.StandardError); ok {
						if base := se.Base(); base != nil {
							errorCode = string(base.Code)
						}
					} else {
						// Try to extract from error string
						errStr := result.Err.Error()
						if strings.Contains(errStr, "block not found") {
							errorCode = "block_not_found"
						} else if strings.Contains(errStr, "timeout") {
							errorCode = "timeout"
						} else {
							errorCode = common.ErrorFingerprint(result.Err)
						}
					}
				}

				if !agreesWithConsensus {
					// Track as error, not misbehavior
					telemetry.MetricConsensusUpstreamErrors.
						WithLabelValues(
							labels.projectId,
							labels.networkId,
							upstreamId,
							labels.category,
							labels.finalityStr,
							group.ResponseType.String(),
							errorCode,
						).Inc()
				}

				continue // Don't track as misbehavior
			}

			// Only track actual data disagreements as misbehavior
			// This includes: empty vs non-empty, or different non-empty responses
			if !agreesWithConsensus && (group.ResponseType == ResponseTypeEmpty || group.ResponseType == ResponseTypeNonEmpty) {
				// Only count as misbehavior if consensus is also data (not error)
				if consensusGroup.ResponseType == ResponseTypeEmpty || consensusGroup.ResponseType == ResponseTypeNonEmpty {
					misbehavingCount++

					// Record metric
					telemetry.MetricConsensusMisbehaviorDetected.
						WithLabelValues(
							labels.projectId,
							labels.networkId,
							upstreamId,
							labels.category,
							labels.finalityStr,
							group.ResponseType.String(),
							largerThanConsensusStr,
						).Inc()

					// Record misbehavior in tracker for score calculation
					if result.Upstream != nil && result.Upstream.Tracker() != nil {
						result.Upstream.Tracker().RecordUpstreamMisbehavior(result.Upstream, labels.method)
					}

					// Apply punishment only if configured and conditions are met
					if e.shouldPunishUpstream(lg, consensusGroup, analysis) {
						limiter := e.createRateLimiter(lg, upstreamId)
						if !limiter.TryAcquirePermit() {
							e.handleMisbehavingUpstream(lg, result.Upstream, upstreamId, labels.projectId, labels.networkId)
						}
					}
				}
			}
		}
	}

	// Log all participants in a single log entry if misbehavior was found and logging is enabled
	if misbehavingCount > 0 && shouldLog {
		// Get consensus response data (compact)
		consensusHash := consensusGroup.Hash
		consensusSize := consensusGroup.ResponseSize
		var consensusBody []byte

		// Get the consensus response body for comparison
		if consensusGroup.LargestResult != nil {
			if jrr, err := consensusGroup.LargestResult.JsonRpcResponse(); err == nil && jrr != nil {
				consensusBody = jrr.GetResultBytes()
			} else {
				consensusBody, _ = common.SonicCfg.Marshal(map[string]interface{}{
					"error": fmt.Sprintf("<error extracting consensus response: %v>", err),
				})
			}
		} else if consensusGroup.FirstError != nil {
			consensusBody, _ = common.SonicCfg.Marshal(map[string]interface{}{
				"error": fmt.Sprintf("<consensus error: %v>", consensusGroup.FirstError),
			})
		}

		logEvent := e.logger.WithLevel(e.disputeLogLevel).
			Str("projectId", labels.projectId).
			Str("networkId", labels.networkId).
			Str("category", labels.category).
			Str("finality", labels.finalityStr).
			Int("consensusCount", consensusGroup.Count).
			Int("totalParticipants", analysis.totalParticipants).
			Int("validParticipants", analysis.validParticipants).
			Int("misbehavingCount", misbehavingCount).
			Str("misbehavingParticipants", misbehavingParticipants).
			Str("consensusResponseType", consensusGroup.ResponseType.String()).
			Str("consensusHash", consensusHash).
			Int("consensusSize", consensusSize).
			RawJSON("consensusResponse", consensusBody).
			Object("request", req)

			// Add ALL participants with numbered keys
		for i, participant := range allParticipants {
			idx := strconv.Itoa(i + 1)
			logEvent = logEvent.
				Str("upstream"+idx, participant.upstreamId).
				Str("responseType"+idx, participant.responseType.String()).
				Bool("agreesWithConsensus"+idx, participant.agreesWithConsensus).
				Str("responseHash"+idx, participant.responseHash).
				Int("responseSize"+idx, participant.responseSize).
				RawJSON("response"+idx, participant.responseBody)

			if participant.errorMessage != "" {
				logEvent = logEvent.Str("error"+idx, participant.errorMessage)
			}
		}

		logEvent.Msg("consensus misbehavior detected - upstreams differ from consensus")
	}
}

// shouldPunishUpstream determines if punishment should be applied based on configuration and consensus strength
func (e *executor) shouldPunishUpstream(lg *zerolog.Logger, consensusGroup *responseGroup, analysis *consensusAnalysis) bool {
	// Check if punishment is configured
	if e.punishMisbehavior == nil || e.punishMisbehavior.DisputeThreshold == 0 {
		return false
	}

	// Guard against invalid DisputeWindow to avoid creating invalid rate limiters
	if e.punishMisbehavior.DisputeWindow.Duration() <= 0 {
		lg.Debug().Msg("punishment disabled: DisputeWindow is zero or negative")
		return false
	}

	// Only punish if we have a clear majority (>50% of valid participants)
	return consensusGroup.Count > analysis.validParticipants/2
}

func (e *executor) handleMisbehavingUpstream(logger *zerolog.Logger, upstream common.Upstream, upstreamId, projectId, networkId string) {
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
		upstream.Uncordon("*", "end of consensus penalty")
		e.misbehavingUpstreamsSitoutTimer.Delete(upstreamId)
	})

	// Replace the placeholder with the actual timer
	e.misbehavingUpstreamsSitoutTimer.Store(upstreamId, timer)
}

func (e *executor) createRateLimiter(logger *zerolog.Logger, upstreamId string) ratelimiter.RateLimiter[any] {
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

func (e *executor) extractMetricsLabels(ctx context.Context, req *common.NormalizedRequest) metricsLabels {
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
		category:    method,
		networkId:   req.NetworkLabel(),
		projectId:   projectId,
		finalityStr: req.Finality(ctx).String(),
	}
}

func (e *executor) startConsensusSpan(ctx context.Context, labels metricsLabels, exec failsafe.Execution[*common.NormalizedResponse]) (context.Context, trace.Span) {
	return common.StartSpan(ctx, "Consensus.Apply",
		trace.WithAttributes(
			attribute.String("network.id", labels.networkId),
			attribute.String("request.method", labels.method),
			attribute.Int("execution.attempts", exec.Attempts()),
		),
	)
}

func (e *executor) recordMetricsAndTracing(req *common.NormalizedRequest, startTime time.Time, result *failsafeCommon.PolicyResult[*common.NormalizedResponse], analysis *consensusAnalysis, labels metricsLabels, span trace.Span) {
	// Determine if consensus was achieved based on the highest count group
	best := analysis.getBestByCount()
	hasConsensus := best != nil && best.Count >= e.agreementThreshold
	isLowParticipants := analysis.isLowParticipants(e.agreementThreshold)
	isDispute := !hasConsensus && !isLowParticipants

	outcome := "success"
	if result.Error != nil {
		if hasConsensus {
			outcome = "consensus_on_error"
		} else if isDispute {
			outcome = "dispute"
		} else if isLowParticipants {
			outcome = "low_participants"
		} else {
			outcome = "generic_error"
		}
		common.SetTraceSpanError(span, result.Error)
	} else {
		span.SetStatus(codes.Ok, "Consensus successful")
	}

	span.SetAttributes(
		attribute.String("consensus.outcome", outcome),
		attribute.Bool("consensus.achieved", hasConsensus),
		attribute.Bool("consensus.low_participants", isLowParticipants),
		attribute.Bool("consensus.dispute", isDispute),
		attribute.Int("participants.total", analysis.totalParticipants),
		attribute.Int("participants.valid", analysis.validParticipants),
	)

	duration := time.Since(startTime).Seconds()
	telemetry.MetricConsensusTotal.WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr).Inc()
	telemetry.MetricConsensusDuration.WithLabelValues(labels.projectId, labels.networkId, labels.category, outcome, labels.finalityStr).Observe(duration)
	// Record agreement count histogram when available
	if best != nil && best.Count > 0 {
		telemetry.MetricConsensusAgreementCount.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr).
			Observe(float64(best.Count))
	}
	// Record categorized error counters for failure modes
	if result.Error != nil {
		errLabel := "generic_error"
		if hasConsensus {
			errLabel = "consensus_on_error"
		} else if isDispute {
			errLabel = "dispute"
		} else if isLowParticipants {
			errLabel = "low_participants"
		}
		telemetry.MetricConsensusErrors.
			WithLabelValues(labels.projectId, labels.networkId, labels.category, errLabel, labels.finalityStr).
			Inc()
	}
}
