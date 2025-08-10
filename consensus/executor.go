package consensus

import (
	"context"
	"errors"
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

		e.checkAndPunishMisbehavingUpstreams(&lg, labels, analysis, exec)

		// --- Finalization ---
		e.recordMetricsAndTracing(ctx, startTime, winner, analysis, labels, consensusSpan)

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

	responseChan := make(chan *execResult, e.maxParticipants)
	for i := 0; i < e.maxParticipants; i++ {
		go e.executeParticipant(cancellableCtx, lg, originalReq, labels, parentExecution, innerFn, i, responseChan)
	}

	responses := make([]*execResult, 0, e.maxParticipants)
	var shortCircuited bool
	var analysis *consensusAnalysis
	var winner *failsafeCommon.PolicyResult[*common.NormalizedResponse]

collectLoop:
	for i := 0; i < e.maxParticipants; i++ {
		select {
		case resp := <-responseChan:
			if resp != nil {
				responses = append(responses, resp)
				if !shortCircuited {
					analysis = newConsensusAnalysis(e.logger, parentExecution, e.config, responses)
					winner = e.determineWinner(ctx, lg, analysis)
					if e.shouldShortCircuit(parentExecution, winner, analysis) {
						shortCircuited = true
						cancelRemaining()
						go func() {
							for j := i + 1; j < e.maxParticipants; j++ {
								<-responseChan // Drain remaining
							}
						}()
						break collectLoop
					}
				}
			}
		case <-ctx.Done():
			lg.Warn().Err(ctx.Err()).Msg("Context cancelled during response collection")
			cancelRemaining()
			break collectLoop
		}
	}

	if analysis == nil {
		analysis = newConsensusAnalysis(e.logger, parentExecution, e.config, responses)
		winner = e.determineWinner(ctx, lg, analysis)
	}

	collectionSpan.SetAttributes(
		attribute.Bool("short_circuited", shortCircuited),
		attribute.Int("responses.collected", len(responses)),
	)
	return winner, analysis
}

// executeParticipant runs a single upstream request within a goroutine.
func (e *executor) executeParticipant(
	ctx context.Context,
	lg *zerolog.Logger,
	originalReq *common.NormalizedRequest,
	labels metricsLabels,
	parentExecution policy.ExecutionInternal[*common.NormalizedResponse],
	innerFn func(failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse],
	index int,
	responseChan chan<- *execResult,
) {
	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			lg.Error().Interface("panic", r).Int("index", index).Msg("Panic in consensus participant")
			telemetry.MetricConsensusPanics.WithLabelValues(labels.projectId, labels.networkId, labels.category, labels.finalityStr).Inc()
			responseChan <- &execResult{Err: errPanicInConsensus}
		}
	}()

	// Check for cancellation before execution
	if ctx.Err() != nil {
		responseChan <- nil
		return
	}

	// Each goroutine needs its own copy of the execution context
	execution := parentExecution.CopyForCancellableWithValue(common.RequestContextKey, originalReq).(policy.ExecutionInternal[*common.NormalizedResponse])
	result := innerFn(execution)

	// Check for cancellation after execution
	if ctx.Err() != nil {
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

	responseChan <- &execResult{
		Result:   any(result.Result).(*common.NormalizedResponse),
		Err:      result.Error,
		Upstream: upstream,
	}
}

// shouldShortCircuit decides if remaining requests can be safely cancelled.
// This happens if one group's lead over the second-place group is greater
// than the number of remaining responses.
func (e *executor) shouldShortCircuit(exec failsafe.Execution[*common.NormalizedResponse], winner *failsafeCommon.PolicyResult[*common.NormalizedResponse], analysis *consensusAnalysis) bool {
	for _, rule := range shortCircuitRules {
		if rule.Condition(winner, analysis) {
			return true
		}
	}
	return false
}

// determineWinner applies configured policies to the analysis to produce a final result.
// It uses a rules-based approach for clear, maintainable decision logic.
func (e *executor) determineWinner(ctx context.Context, lg *zerolog.Logger, analysis *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
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

// (Implementation for punishment, metrics, and tracing helpers would go here)
// These are omitted for brevity in this rewrite but would follow the patterns
// from the original code, adapted for the new data structures.

func (e *executor) checkAndPunishMisbehavingUpstreams(lg *zerolog.Logger, labels metricsLabels, analysis *consensusAnalysis, exec failsafe.Execution[*common.NormalizedResponse]) {
	if e.punishMisbehavior == nil || e.punishMisbehavior.DisputeThreshold == 0 {
		return
	}
	// Guard against invalid DisputeWindow to avoid creating invalid rate limiters
	if e.punishMisbehavior.DisputeWindow.Duration() <= 0 {
		lg.Debug().Msg("punishment disabled: DisputeWindow is zero or negative")
		return
	}
	// Do not punish when there are no valid participants (all infra errors)
	if analysis.validParticipants == 0 {
		return
	}

	// Determine the winner among VALID groups only (exclude infrastructure errors)
	var winner *responseGroup
	for _, g := range analysis.getValidGroups() {
		if winner == nil || g.Count > winner.Count {
			winner = g
		}
	}
	if winner == nil {
		return
	}

	// Only punish if we have a clear majority winner (>50% of valid participants)
	if winner.Count <= analysis.validParticipants/2 {
		return
	}

	// Punish only upstreams from VALID groups that disagreed with the majority
	for _, group := range analysis.getValidGroups() {
		if group.Hash == winner.Hash {
			continue
		}
		for _, result := range group.Results {
			if result.Upstream == nil {
				continue
			}
			upstreamId := result.Upstream.Id()
			limiter := e.createRateLimiter(lg, upstreamId)
			if !limiter.TryAcquirePermit() {
				e.handleMisbehavingUpstream(lg, result.Upstream, upstreamId, labels.projectId, labels.networkId)
			}
		}
	}
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
		upstream.Uncordon("*")
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
		networkId:   req.NetworkId(),
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

func (e *executor) recordMetricsAndTracing(ctx context.Context, startTime time.Time, result *failsafeCommon.PolicyResult[*common.NormalizedResponse], analysis *consensusAnalysis, labels metricsLabels, span trace.Span) {
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
			outcome = "error"
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
}
