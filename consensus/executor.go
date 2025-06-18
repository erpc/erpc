package consensus

import (
	"fmt"
	"sync"
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
	return e.resultToJsonRpcResponse(result, exec).CanonicalHash()
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
		if !ok || upsList == nil {
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
		executions := make([]policy.ExecutionInternal[R], len(selectedUpstreams))
		executionWaitGroup := sync.WaitGroup{}
		executionWaitGroup.Add(len(selectedUpstreams))

		// Create a buffered channel for the single result
		resultChan := make(chan *failsafeCommon.PolicyResult[R], 1)

		// Track responses for consensus checking
		responses := make([]*execResult[R], 0, len(selectedUpstreams))
		responseMu := sync.Mutex{}

		for execIdx, up := range selectedUpstreams {
			cloneReq, err := originalReq.Clone()
			if err != nil {
				lg.Error().Err(err).Msg("error cloning request")
				return innerFn(exec)
			}
			cloneReq.Directives().UseUpstream = up.Id()

			lg.Debug().
				Interface("id", cloneReq.ID()).
				Int("index", execIdx).
				Str("upstream", up.Id()).
				Object("request", cloneReq).
				Msg("prepared cloned request for consensus execution")

			executions[execIdx] = parentExecution.CopyForCancellableWithValue(
				common.RequestContextKey,
				cloneReq,
			).(policy.ExecutionInternal[R])

			// Perform execution
			go func(consensusExec policy.ExecutionInternal[R], execIdx int, upstream common.Upstream) {
				defer executionWaitGroup.Done()

				lg.Trace().Int("index", execIdx).Object("request", originalReq).Msg("sending consensus request")
				result := innerFn(consensusExec)
				if result == nil {
					return
				}

				responseMu.Lock()
				responses = append(responses, &execResult[R]{
					result:   result.Result,
					err:      result.Error,
					index:    execIdx,
					upstream: selectedUpstreams[execIdx],
				})
				responseMu.Unlock()
			}(executions[execIdx], execIdx, selectedUpstreams[execIdx])
		}

		go func() {
			// Wait for all executions to complete
			executionWaitGroup.Wait()

			// Count unique participants
			nonErrorParticipants := make(map[string]struct{})
			for _, r := range responses {
				if r.err == nil && r.upstream != nil && r.upstream.Config() != nil {
					nonErrorParticipants[r.upstream.Config().Id] = struct{}{}
				}
			}
			nonErrorParticipantCount := len(nonErrorParticipants)

			lg.Debug().
				Int("nonErrorParticipantCount", nonErrorParticipantCount).
				Int("requiredParticipants", e.requiredParticipants).
				Msg("counting unique participants")

			// Count responses by result
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
				// Extract just the response data for comparison
				resultHash, err := e.resultToHash(r.result, exec)
				if err != nil {
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

			lg.Debug().
				Int("totalResponses", len(responses)).
				Int("required", len(selectedUpstreams)).
				Int("errorCount", errorCount).
				Interface("responseCounts", resultCounts).
				Msg("consensus check")

			// Find the most common result
			var mostCommonResultHash string
			mostCommonResultCount := 0
			for resultHash, count := range resultCounts {
				if count > mostCommonResultCount {
					mostCommonResultHash = resultHash
					mostCommonResultCount = count
				}
			}

			lg.Debug().
				Str("mostCommonResultHash", mostCommonResultHash).
				Int("count", mostCommonResultCount).
				Int("threshold", e.agreementThreshold).
				Bool("hasConsensus", mostCommonResultCount >= e.agreementThreshold).
				Msg("consensus result")

			// Check for low participants first
			if mostCommonResultCount >= e.agreementThreshold {
				// We have consensus
				lg.Debug().Msg("completed consensus execution")

				if e.onAgreement != nil {
					e.onAgreement(failsafe.ExecutionEvent[R]{
						ExecutionAttempt: exec,
					})
				}

				// Track misbehaving upstreams
				e.trackMisbehavingUpstreams(&lg, responses, resultCounts, mostCommonResultHash, parentExecution)

				// Convert mostCommonResult back to type R
				var finalResult R
				for _, r := range responses {
					if r.err == nil {
						resultHash, err := e.resultToHash(r.result, exec)
						if err != nil {
							lg.Error().
								Interface("result", r.result).
								Err(err).
								Msg("error converting result to hash")
							continue
						}
						if resultHash == mostCommonResultHash {
							finalResult = r.result
							break
						}
					}
				}
				resultChan <- &failsafeCommon.PolicyResult[R]{
					Result: finalResult,
				}
			} else if nonErrorParticipantCount < e.requiredParticipants {
				lg.Debug().
					Int("nonErrorParticipantCount", nonErrorParticipantCount).
					Int("responses", len(responses)).
					Int("required", e.requiredParticipants).
					Int("selectedUpstreams", len(selectedUpstreams)).
					Msg("handling low participants")

				if e.onLowParticipants != nil {
					e.onLowParticipants(failsafe.ExecutionEvent[R]{
						ExecutionAttempt: exec,
					})
				}

				switch e.lowParticipantsBehavior {
				case common.ConsensusLowParticipantsBehaviorReturnError:
					e.handleReturnError(&lg, resultChan, exec, func() error {
						return common.NewErrConsensusLowParticipants("not enough participants", e.extractParticipants(&lg, responses, exec))
					})
				case common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult:
					e.handleAcceptMostCommon(&lg, resultChan, responses, mostCommonResultHash, exec, func() error {
						return common.NewErrConsensusLowParticipants("no valid results found", e.extractParticipants(&lg, responses, exec))
					})
				case common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader:
					e.handlePreferBlockHeadLeader(&lg, resultChan, responses, mostCommonResultHash, exec, func() error {
						return common.NewErrConsensusLowParticipants("no block head leader found", e.extractParticipants(&lg, responses, exec))
					})
				case common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader:
					e.handleOnlyBlockHeadLeader(&lg, resultChan, responses, exec, func() error {
						return common.NewErrConsensusLowParticipants("no block head leader found", e.extractParticipants(&lg, responses, exec))
					})
				}
			} else {
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

				if e.onDispute != nil {
					e.onDispute(failsafe.ExecutionEvent[R]{
						ExecutionAttempt: exec,
					})
				}

				// Track misbehaving upstreams
				e.trackMisbehavingUpstreams(&lg, responses, resultCounts, mostCommonResultHash, parentExecution)

				// Handle dispute according to behavior
				switch e.disputeBehavior {
				case common.ConsensusDisputeBehaviorReturnError:
					e.handleReturnError(&lg, resultChan, exec, func() error {
						return common.NewErrConsensusDispute("not enough agreement among responses", e.extractParticipants(&lg, responses, exec))
					})
				case common.ConsensusDisputeBehaviorAcceptMostCommonValidResult:
					e.handleAcceptMostCommon(&lg, resultChan, responses, mostCommonResultHash, exec, func() error {
						return common.NewErrConsensusDispute("no valid results found", e.extractParticipants(&lg, responses, exec))
					})
				case common.ConsensusDisputeBehaviorPreferBlockHeadLeader:
					e.handlePreferBlockHeadLeader(&lg, resultChan, responses, mostCommonResultHash, exec, func() error {
						return common.NewErrConsensusDispute("no block head leader found", e.extractParticipants(&lg, responses, exec))
					})
				case common.ConsensusDisputeBehaviorOnlyBlockHeadLeader:
					e.handleOnlyBlockHeadLeader(&lg, resultChan, responses, exec, func() error {
						return common.NewErrConsensusDispute("no block head leader found", e.extractParticipants(&lg, responses, exec))
					})
				}
			}
		}()

		// Wait for a result
		result := <-resultChan

		// Cancel any outstanding attempts
		for _, execution := range executions {
			if execution != nil {
				execution.Cancel(nil)
			}
		}

		// Return the result
		return result
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

	// Store the limiter
	e.misbehavingUpstreamsLimiter.Store(upstreamId, limiter)
	return limiter
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
	// First check if we're the first to handle this upstream
	if _, loaded := e.misbehavingUpstreamsSitoutTimer.LoadOrStore(upstreamId, nil); loaded {
		logger.Debug().
			Str("upstream", upstreamId).
			Msg("upstream already in sitout, skipping")
		return
	}

	logger.Warn().
		Str("upstream", upstreamId).
		Msg("misbehaviour limit exhausted, punishing upstream")

	// We're the first to handle this upstream, cordon it
	upstream.Cordon("*", "misbehaving in consensus")

	// Now create and store the timer
	timer := time.AfterFunc(e.punishMisbehavior.SitOutPenalty.Duration(), func() {
		upstream.Uncordon("*")
		e.misbehavingUpstreamsSitoutTimer.Delete(upstreamId)
	})

	// Store the actual timer
	e.misbehavingUpstreamsSitoutTimer.Store(upstreamId, timer)
}

func (e *executor[R]) handleReturnError(logger *zerolog.Logger, resultChan chan<- *failsafeCommon.PolicyResult[R], _ failsafe.Execution[R], errFn func() error) {
	logger.Debug().Msg("sending error result")

	resultChan <- &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}

func (e *executor[R]) handleAcceptMostCommon(logger *zerolog.Logger, resultChan chan<- *failsafeCommon.PolicyResult[R], responses []*execResult[R], _ string, _ failsafe.Execution[R], errFn func() error) {
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
		resultChan <- &failsafeCommon.PolicyResult[R]{
			Result: finalResult,
		}
	} else {
		logger.Debug().Interface("responses", responses).Msg("no valid results found, sending error")
		resultChan <- &failsafeCommon.PolicyResult[R]{
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

func (e *executor[R]) handlePreferBlockHeadLeader(logger *zerolog.Logger, resultChan chan<- *failsafeCommon.PolicyResult[R], responses []*execResult[R], mostCommonResult string, exec failsafe.Execution[R], errFn func() error) {
	_, highestBlockResult := e.findBlockHeadLeader(responses)

	if highestBlockResult != nil {
		logger.Debug().Msg("sending block head leader result")

		if highestBlockResult.err != nil {
			resultChan <- &failsafeCommon.PolicyResult[R]{
				Error: highestBlockResult.err,
			}
			return
		}

		resultChan <- &failsafeCommon.PolicyResult[R]{
			Result: highestBlockResult.result,
		}
		return
	}

	// Fall back to most common result
	e.handleAcceptMostCommon(logger, resultChan, responses, mostCommonResult, exec, errFn)
}

func (e *executor[R]) handleOnlyBlockHeadLeader(logger *zerolog.Logger, resultChan chan<- *failsafeCommon.PolicyResult[R], responses []*execResult[R], _ failsafe.Execution[R], errFn func() error) {
	_, highestBlockResult := e.findBlockHeadLeader(responses)

	if highestBlockResult != nil {
		logger.Debug().Msg("sending block head leader result")
		resultChan <- &failsafeCommon.PolicyResult[R]{
			Result: highestBlockResult.result,
		}
		return
	}

	// If no block head leader found, return error
	logger.Debug().Msg("sending no block head leader error")

	resultChan <- &failsafeCommon.PolicyResult[R]{
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
