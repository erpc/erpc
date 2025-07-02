package consensus

import (
	"context"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/failsafe-go/failsafe-go/policy"
	"github.com/failsafe-go/failsafe-go/ratelimiter"
	"github.com/rs/zerolog"
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

func (e *executor[R]) Apply(innerFn func(failsafe.Execution[R]) *failsafeCommon.PolicyResult[R]) func(failsafe.Execution[R]) *failsafeCommon.PolicyResult[R] {
	return func(exec failsafe.Execution[R]) *failsafeCommon.PolicyResult[R] {
		ctx := exec.Context()
		// Extract the original request and upstream list from the execution context.
		val := ctx.Value(common.RequestContextKey)
		originalReq, ok := val.(*common.NormalizedRequest)
		if !ok || originalReq == nil {
			// shouldn't happen, but fall back to simple execution
			e.logger.Warn().Object("request", originalReq).Msg("unexpecued nil request in consensus policy")
			return innerFn(exec)
		}

		lg := e.logger.With().
			Interface("id", originalReq.ID()).
			Str("component", "consensus").
			Str("networkId", originalReq.NetworkId()).
			Logger()

		val = ctx.Value(common.UpstreamsContextKey)
		upsList, ok := val.([]common.Upstream)
		if !ok || upsList == nil || len(upsList) == 0 {
			lg.Warn().Object("request", originalReq).Interface("upstreams", upsList).Msg("unexpecued incompatible or empty list of upstreams in consensus policy")
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
		selectedUpstreams := e.selectUpstreams(upsList)

		parentExecution := exec.(policy.ExecutionInternal[R])

		// Phase 1: Fire off requests and collect responses
		responses := e.collectResponses(ctx, &lg, originalReq, selectedUpstreams, parentExecution, innerFn)

		// Phase 2: Evaluate consensus
		result := e.evaluateConsensus(&lg, responses, selectedUpstreams, exec)

		// Phase 3: Track misbehaving upstreams (only if we have a clear majority)
		e.checkAndPunishMisbehavingUpstreams(&lg, responses, parentExecution)

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
			// Add panic recovery
			defer func() {
				if r := recover(); r != nil {
					lg.Error().
						Int("index", execIdx).
						Str("upstream", upstream.Id()).
						Interface("panic", r).
						Msg("panic in consensus execution")

					// Send error result
					select {
					case responseChan <- &execResult[R]{
						err:      fmt.Errorf("panic in consensus execution: %v", r),
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
			case <-cancellableCtx.Done():
				lg.Debug().Int("index", execIdx).Str("upstream", upstream.Id()).Msg("skipping execution, context cancelled")
				// Still send a nil result so the collector knows this goroutine is done
				select {
				case responseChan <- nil:
				case <-cancellableCtx.Done():
					// Already cancelled, safe to exit
				}
				return
			default:
			}

			lg.Trace().Int("index", execIdx).Object("request", originalReq).Msg("sending consensus request")

			// The execution should inherit the cancellable context from the parent
			// Double-check the context hasn't been cancelled before executing
			if cancellableCtx.Err() != nil {
				lg.Debug().Int("index", execIdx).Str("upstream", upstream.Id()).Msg("context cancelled before execution")
				select {
				case responseChan <- nil:
				case <-cancellableCtx.Done():
				}
				return
			}

			result := innerFn(consensusExec)

			// Check cancellation again after execution
			select {
			case <-cancellableCtx.Done():
				lg.Debug().Int("index", execIdx).Str("upstream", upstream.Id()).Msg("discarding result, context cancelled after execution")
				// Still send nil to unblock collector
				select {
				case responseChan <- nil:
				case <-cancellableCtx.Done():
				}
				return
			default:
			}

			if result == nil {
				// Send nil to indicate this goroutine is done
				select {
				case responseChan <- nil:
				case <-cancellableCtx.Done():
				}
				return
			}

			// Send response to channel
			select {
			case responseChan <- &execResult[R]{
				result:   result.Result,
				err:      result.Error,
				index:    execIdx,
				upstream: upstream,
			}:
			case <-cancellableCtx.Done():
				// Context cancelled, still try to send nil to unblock collector
				lg.Debug().Int("index", execIdx).Str("upstream", upstream.Id()).Msg("discarding response due to context cancellation")
				select {
				case responseChan <- nil:
				case <-cancellableCtx.Done():
					// Already cancelled, safe to exit
				}
			}
		}(execution, execIdx, up)
		actualGoroutineCount++
	}

	// Collect responses with short-circuit logic
	responses := make([]*execResult[R], 0, len(selectedUpstreams))
	var shortCircuited bool

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
				cancelRemaining()

				// Drain any remaining responses in the background
				go e.drainResponses(responseChan, actualGoroutineCount-(i+1))
				return responses
			}

		case <-ctx.Done():
			// Original context cancelled
			lg.Debug().
				Int("received", len(responses)).
				Int("expected", actualGoroutineCount).
				Msg("context cancelled while waiting for consensus responses")
			cancelRemaining()
			return responses
		}
	}

	return responses
}

// checkShortCircuit determines if we can exit early based on current responses
func (e *executor[R]) checkShortCircuit(lg *zerolog.Logger, responses []*execResult[R], selectedUpstreams []common.Upstream, exec policy.ExecutionInternal[R]) bool {
	// Count results by hash
	resultCounts := make(map[string]int)
	for _, r := range responses {
		if r.err != nil {
			continue
		}
		resultHash, err := e.resultToHash(r.result, exec)
		if err != nil {
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
		if r.err == nil && r.upstream != nil && r.upstream.Config() != nil {
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
	lg *zerolog.Logger,
	responses []*execResult[R],
	selectedUpstreams []common.Upstream,
	exec failsafe.Execution[R],
) *failsafeCommon.PolicyResult[R] {
	// Count unique participants and responses
	nonErrorParticipantCount := e.countUniqueParticipants(responses)

	lg.Debug().
		Int("nonErrorParticipantCount", nonErrorParticipantCount).
		Int("requiredParticipants", e.requiredParticipants).
		Msg("counting unique participants")

	// Count responses by result hash
	resultCounts, mostCommonResultHash, mostCommonResultCount := e.countResponsesByHash(lg, responses, exec)

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

		if e.onAgreement != nil {
			e.onAgreement(failsafe.ExecutionEvent[R]{
				ExecutionAttempt: exec,
			})
		}

		// Find the actual result that matches the consensus
		if result := e.findResultByHash(responses, mostCommonResultHash, exec); result != nil {
			return &failsafeCommon.PolicyResult[R]{
				Result: *result,
			}
		}
	}

	// Check if we have low participants
	isLowParticipants := e.isLowParticipants(nonErrorParticipantCount, len(responses), len(selectedUpstreams))

	if isLowParticipants {
		lg.Debug().
			Int("nonErrorParticipantCount", nonErrorParticipantCount).
			Int("responses", len(responses)).
			Int("required", e.requiredParticipants).
			Int("selectedUpstreams", len(selectedUpstreams)).
			Msg("handling low participants")

		return e.handleLowParticipants(lg, responses, exec)
	}

	// Consensus dispute
	e.logDispute(lg, responses, mostCommonResultCount, exec)

	if e.onDispute != nil {
		e.onDispute(failsafe.ExecutionEvent[R]{
			ExecutionAttempt: exec,
		})
	}

	// Handle dispute according to behavior
	return e.handleDispute(lg, responses, exec)
}

// countUniqueParticipants counts the number of unique non-error participants
func (e *executor[R]) countUniqueParticipants(responses []*execResult[R]) int {
	nonErrorParticipants := make(map[string]struct{})
	for _, r := range responses {
		if r.err == nil && r.upstream != nil && r.upstream.Config() != nil {
			nonErrorParticipants[r.upstream.Config().Id] = struct{}{}
		}
	}
	return len(nonErrorParticipants)
}

// countResponsesByHash counts responses by their hash and returns the counts, most common hash and count
func (e *executor[R]) countResponsesByHash(lg *zerolog.Logger, responses []*execResult[R], exec failsafe.Execution[R]) (map[string]int, string, int) {
	resultCounts := make(map[string]int)
	errorCount := 0
	for _, r := range responses {
		if r.err != nil {
			errorCount++
			lg.Debug().
				Err(r.err).
				Msg("response has error")
			continue
		}
		resultHash, err := e.resultToHash(r.result, exec)
		if err != nil {
			errorCount++
			lg.Error().
				Err(err).
				Msg("error converting result to hash")
			continue
		}
		resultCounts[resultHash]++
		if lg.GetLevel() <= zerolog.DebugLevel {
			upstreamId := "unknown"
			if r.upstream != nil && r.upstream.Config() != nil {
				upstreamId = r.upstream.Config().Id
			}
			lg.Debug().
				Str("resultHash", resultHash).
				Int("count", resultCounts[resultHash]).
				Str("upstream", upstreamId).
				Object("response", e.resultToJsonRpcResponse(r.result, exec)).
				Msg("counting response")
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

// findResultByHash finds a result that matches the given hash
func (e *executor[R]) findResultByHash(responses []*execResult[R], targetHash string, exec failsafe.Execution[R]) *R {
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
func (e *executor[R]) isLowParticipants(nonErrorParticipantCount, responseCount, selectedUpstreamCount int) bool {
	if selectedUpstreamCount < e.requiredParticipants {
		return true
	}

	if nonErrorParticipantCount < e.requiredParticipants {
		// Check if we short-circuited and would have had enough participants
		if responseCount < selectedUpstreamCount {
			potentialUniqueParticipants := nonErrorParticipantCount + (selectedUpstreamCount - responseCount)
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
			e.extractParticipants(lg, responses, exec),
		)
	}

	switch e.lowParticipantsBehavior {
	case common.ConsensusLowParticipantsBehaviorReturnError:
		return e.handleReturnError(lg, errFn)
	case common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult:
		return e.handleAcceptMostCommon(lg, responses, func() error {
			return common.NewErrConsensusLowParticipants(
				"no valid results found",
				e.extractParticipants(lg, responses, exec),
			)
		})
	case common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader:
		return e.handlePreferBlockHeadLeader(lg, responses, func() error {
			return common.NewErrConsensusLowParticipants(
				"no block head leader found",
				e.extractParticipants(lg, responses, exec),
			)
		})
	case common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader:
		return e.handleOnlyBlockHeadLeader(lg, responses, func() error {
			return common.NewErrConsensusLowParticipants(
				"no block head leader found",
				e.extractParticipants(lg, responses, exec),
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
			e.extractParticipants(lg, responses, exec),
		)
	}

	switch e.disputeBehavior {
	case common.ConsensusDisputeBehaviorReturnError:
		return e.handleReturnError(lg, errFn)
	case common.ConsensusDisputeBehaviorAcceptMostCommonValidResult:
		return e.handleAcceptMostCommon(lg, responses, func() error {
			return common.NewErrConsensusDispute(
				"no valid results found",
				e.extractParticipants(lg, responses, exec),
			)
		})
	case common.ConsensusDisputeBehaviorPreferBlockHeadLeader:
		return e.handlePreferBlockHeadLeader(lg, responses, func() error {
			return common.NewErrConsensusDispute(
				"no block head leader found",
				e.extractParticipants(lg, responses, exec),
			)
		})
	case common.ConsensusDisputeBehaviorOnlyBlockHeadLeader:
		return e.handleOnlyBlockHeadLeader(lg, responses, func() error {
			return common.NewErrConsensusDispute(
				"no block head leader found",
				e.extractParticipants(lg, responses, exec),
			)
		})
	default:
		return e.handleReturnError(lg, errFn)
	}
}

// checkAndPunishMisbehavingUpstreams tracks misbehaving upstreams if there's a clear majority
func (e *executor[R]) checkAndPunishMisbehavingUpstreams(
	lg *zerolog.Logger,
	responses []*execResult[R],
	parentExecution policy.ExecutionInternal[R],
) {
	// Skip if punishment is not configured
	if e.punishMisbehavior == nil {
		return
	}

	// First, count results to find the most common one
	resultCounts := make(map[string]int)
	for _, r := range responses {
		if r.err != nil {
			continue
		}
		resultHash, err := e.resultToHash(r.result, parentExecution)
		if err != nil {
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

	// Only track misbehaving upstreams if we have a clear majority (>50%)
	if mostCommonResultCount > len(responses)/2 {
		e.trackMisbehavingUpstreams(lg, responses, resultCounts, mostCommonResultHash, parentExecution)
	}
}

func (e *executor[R]) extractParticipants(lg *zerolog.Logger, responses []*execResult[R], exec failsafe.Execution[R]) []common.ParticipantInfo {
	participants := make([]common.ParticipantInfo, 0, len(responses))
	for _, resp := range responses {
		participant := common.ParticipantInfo{}
		if resp.upstream != nil && resp.upstream.Config() != nil {
			participant.Upstream = resp.upstream.Config().Id
		}
		if resp.err != nil {
			participant.ErrSummary = common.ErrorSummary(resp.err)
		}
		if jr := e.resultToJsonRpcResponse(resp.result, exec); jr != nil {
			hash, err := jr.CanonicalHash()
			if err != nil {
				lg.Error().Err(err).Msg("error converting result to hash")
				continue
			}
			participant.ResultHash = hash
		}
		participants = append(participants, participant)
	}
	return participants
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

func (e *executor[R]) trackMisbehavingUpstreams(logger *zerolog.Logger, responses []*execResult[R], resultCounts map[string]int, mostCommonResultHash string, execution policy.ExecutionInternal[R]) {
	// Count total valid responses
	totalValidResponses := 0
	for _, count := range resultCounts {
		totalValidResponses += count
	}

	// Only proceed with punishment if we have a clear majority (>50%)
	mostCommonCount := resultCounts[mostCommonResultHash]
	if mostCommonCount <= totalValidResponses/2 {
		return
	}

	for _, response := range responses {
		if response.err != nil {
			continue
		}

		upstream := response.upstream
		if upstream == nil {
			continue
		}

		resultHash, err := e.resultToHash(response.result, execution)
		if err != nil {
			logger.Error().
				Err(err).
				Msg("error converting result to hash")
			continue
		}
		if resultHash == "" {
			continue
		}

		// If this result doesn't match the most common result, punish the upstream
		if resultHash != mostCommonResultHash {
			limiter := e.createRateLimiter(logger, upstream.Config().Id)
			if !limiter.TryAcquirePermit() {
				e.handleMisbehavingUpstream(logger, upstream, upstream.Config().Id)
			}
		}
	}
}

func (e *executor[R]) handleMisbehavingUpstream(logger *zerolog.Logger, upstream common.Upstream, upstreamId string) {
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
	} else {
		logger.Debug().Interface("responses", responses).Msg("no valid results found, sending error")
		return &failsafeCommon.PolicyResult[R]{
			Error: errFn(),
		}
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
