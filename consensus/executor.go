package consensus

import (
	"crypto/md5" //#nosec G501 -- Permit import of md5 for fast hashing responses
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/failsafe-go/failsafe-go/policy"
	"github.com/failsafe-go/failsafe-go/ratelimiter"
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
func (e *executor[R]) resultToHash(result R, exec failsafe.Execution[R]) string {
	return hashResult(e.resultToRawString(result, exec))
}

// resultToRawString converts a result to its raw string representation
func (e *executor[R]) resultToRawString(result R, exec failsafe.Execution[R]) string {
	if resp, ok := any(result).(*common.NormalizedResponse); ok {
		if jr, err := resp.JsonRpcResponse(exec.Context()); err == nil {
			return string(jr.Result)
		}
	}
	return fmt.Sprintf("%+v", result)
}

// hashResult creates an MD5 hash of a string
func hashResult(s string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(s))) //#nosec G401 -- Permit usage of md5 for fast hashing, not used for security
}

func (e *executor[R]) Apply(innerFn func(failsafe.Execution[R]) *failsafeCommon.PolicyResult[R]) func(failsafe.Execution[R]) *failsafeCommon.PolicyResult[R] {
	return func(exec failsafe.Execution[R]) *failsafeCommon.PolicyResult[R] {
		parentExecution := exec.(policy.ExecutionInternal[R])
		executions := make([]policy.ExecutionInternal[R], e.requiredParticipants)
		executionWaitGroup := sync.WaitGroup{}
		executionWaitGroup.Add(e.requiredParticipants)

		// Create a buffered channel for the single result
		resultChan := make(chan *failsafeCommon.PolicyResult[R], 1)

		// Track responses for consensus checking
		responses := make([]*execResult[R], 0, e.requiredParticipants)
		responseMu := sync.Mutex{}

		// Track used upstreams to ensure we don't use the same one twice
		usedUpstreams := make(map[common.Upstream]struct{})
		usedUpstreamsMu := sync.Mutex{}

		for execIdx := range e.requiredParticipants {
			// Prepare execution
			executions[execIdx] = parentExecution.CopyForCancellable().(policy.ExecutionInternal[R])

			// Perform execution
			go func(consensusExec policy.ExecutionInternal[R], execIdx int) {
				defer executionWaitGroup.Done()

				// Get the upstream from the response
				var upstream common.Upstream
				result := innerFn(consensusExec)
				if result == nil {
					return
				}

				// Get upstream from response if available
				if resp, ok := any(result.Result).(*common.NormalizedResponse); ok {
					if ups := resp.Upstream(); ups != nil {
						// Check if this upstream has already been used
						usedUpstreamsMu.Lock()
						if _, used := usedUpstreams[ups]; used {
							usedUpstreamsMu.Unlock()
							e.logger.Debug().Int("request", execIdx).Str("upstream", ups.Config().Id).Msg("skipping already used upstream")
							return
						}
						usedUpstreams[ups] = struct{}{}
						usedUpstreamsMu.Unlock()

						upstream = ups
						e.logger.Debug().Int("request", execIdx).Str("upstream", upstream.Config().Id).Msg("consensus request using upstream")
					}
				}

				responseMu.Lock()
				responses = append(responses, &execResult[R]{
					result:   result.Result,
					err:      result.Error,
					index:    execIdx,
					upstream: upstream,
				})
				responseMu.Unlock()
			}(executions[execIdx], execIdx)
		}

		go func() {
			// Wait for all executions to complete
			executionWaitGroup.Wait()

			// Count responses by result
			resultCounts := make(map[string]int)
			errorCount := 0
			for _, r := range responses {
				if r.err != nil {
					errorCount++
					e.logger.Debug().
						Err(r.err).
						Msg("response has error")
					continue
				}
				// Extract just the response data for comparison
				resultHash := e.resultToHash(r.result, exec)
				resultCounts[resultHash]++
				e.logger.Debug().
					Str("result_hash", resultHash).
					Int("count", resultCounts[resultHash]).
					Msg("counting response")
			}

			e.logger.Debug().
				Int("total_responses", len(responses)).
				Int("required", e.requiredParticipants).
				Int("error_count", errorCount).
				Interface("response_counts", resultCounts).
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

			e.logger.Debug().
				Str("most_common_result_hash", mostCommonResultHash).
				Int("count", mostCommonResultCount).
				Int("threshold", e.agreementThreshold).
				Bool("has_consensus", mostCommonResultCount >= e.agreementThreshold).
				Msg("consensus result")

			// Check if we have enough agreement
			if mostCommonResultCount >= e.agreementThreshold {
				// We have consensus
				e.logger.Debug().
					Str("source", "consensus").
					Msg("sending consensus result")

				if e.onAgreement != nil {
					e.onAgreement(failsafe.ExecutionEvent[R]{
						ExecutionAttempt: exec,
					})
				}

				// Track misbehaving upstreams
				e.trackMisbehavingUpstreams(responses, resultCounts, mostCommonResultHash, parentExecution)

				// Convert mostCommonResult back to type R
				var finalResult R
				for _, r := range responses {
					if r.err == nil {
						resultHash := e.resultToHash(r.result, exec)
						if resultHash == mostCommonResultHash {
							finalResult = r.result
							break
						}
					}
				}
				resultChan <- &failsafeCommon.PolicyResult[R]{
					Result: finalResult,
				}
			} else if len(responses) < e.requiredParticipants {
				e.logger.Debug().
					Int("responses", len(responses)).
					Int("required", e.requiredParticipants).
					Msg("handling low participants")

				if e.onLowParticipants != nil {
					e.onLowParticipants(failsafe.ExecutionEvent[R]{
						ExecutionAttempt: exec,
					})
				}

				switch e.lowParticipantsBehavior {
				case common.ConsensusLowParticipantsBehaviorReturnError:
					e.handleReturnError(resultChan, exec, func() error {
						return common.NewErrConsensusLowParticipants("not enough participants")
					})
				case common.ConsensusLowParticipantsBehaviorAcceptAnyValidResult:
					e.handleAcceptAnyValid(resultChan, responses, mostCommonResultHash, exec, func() error {
						return common.NewErrConsensusLowParticipants("no valid results found")
					})
				case common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader:
					e.handlePreferBlockHeadLeader(resultChan, responses, mostCommonResultHash, exec, func() error {
						return common.NewErrConsensusLowParticipants("no block head leader found")
					})
				case common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader:
					e.handleOnlyBlockHeadLeader(resultChan, responses, exec, func() error {
						return common.NewErrConsensusLowParticipants("no block head leader found")
					})
				}
			} else {
				// Handle dispute
				e.logger.Debug().
					Str("source", "dispute").
					Int("max_count", mostCommonResultCount).
					Int("threshold", e.agreementThreshold).
					Msg("handling dispute")

				if e.onDispute != nil {
					e.onDispute(failsafe.ExecutionEvent[R]{
						ExecutionAttempt: exec,
					})
				}

				// Track misbehaving upstreams
				e.trackMisbehavingUpstreams(responses, resultCounts, mostCommonResultHash, parentExecution)

				// Handle dispute according to behavior
				switch e.disputeBehavior {
				case common.ConsensusDisputeBehaviorReturnError:
					e.handleReturnError(resultChan, exec, func() error {
						return common.NewErrConsensusDispute("not enough agreement among responses")
					})
				case common.ConsensusDisputeBehaviorAcceptAnyValidResult:
					e.handleAcceptAnyValid(resultChan, responses, mostCommonResultHash, exec, func() error {
						return common.NewErrConsensusDispute("no valid results found")
					})
				case common.ConsensusDisputeBehaviorPreferBlockHeadLeader:
					e.handlePreferBlockHeadLeader(resultChan, responses, mostCommonResultHash, exec, func() error {
						return common.NewErrConsensusDispute("no block head leader found")
					})
				case common.ConsensusDisputeBehaviorOnlyBlockHeadLeader:
					e.handleOnlyBlockHeadLeader(resultChan, responses, exec, func() error {
						return common.NewErrConsensusDispute("no block head leader found")
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

func (e *executor[R]) createRateLimiter(upstreamId string) ratelimiter.RateLimiter[any] {
	// Try to get existing limiter
	if limiter, ok := e.misbehavingUpstreamsLimiter.Load(upstreamId); ok {
		return limiter.(ratelimiter.RateLimiter[any])
	}

	e.logger.Info().
		Str("upstream", upstreamId).
		Int("dispute_threshold", int(e.punishMisbehavior.DisputeThreshold)).
		Str("dispute_window", e.punishMisbehavior.DisputeWindow.String()).
		Msg("creating new dispute limiter")

	limiter := ratelimiter.
		BurstyBuilder[any](e.punishMisbehavior.DisputeThreshold, e.punishMisbehavior.DisputeWindow.Duration()).
		Build()

	// Store the limiter
	e.misbehavingUpstreamsLimiter.Store(upstreamId, limiter)
	return limiter
}

func (e *executor[R]) trackMisbehavingUpstreams(responses []*execResult[R], resultCounts map[string]int, mostCommonResultHash string, execution policy.ExecutionInternal[R]) {
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

		resultHash := e.resultToHash(response.result, execution)
		if resultHash == "" {
			continue
		}

		// If this result doesn't match the most common result, punish the upstream
		if resultHash != mostCommonResultHash {
			limiter := e.createRateLimiter(upstream.Config().Id)
			if !limiter.TryAcquirePermit() {
				e.handleMisbehavingUpstream(upstream, upstream.Config().Id)
			}
		}
	}
}

func (e *executor[R]) handleMisbehavingUpstream(upstream common.Upstream, upstreamId string) {
	// First check if we're the first to handle this upstream
	if _, loaded := e.misbehavingUpstreamsSitoutTimer.LoadOrStore(upstreamId, nil); loaded {
		e.logger.Debug().
			Str("upstream", upstreamId).
			Msg("upstream already in sitout, skipping")
		return
	}

	e.logger.Warn().
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

func (e *executor[R]) handleReturnError(resultChan chan<- *failsafeCommon.PolicyResult[R], exec failsafe.Execution[R], errFn func() error) {
	e.logger.Debug().
		Str("source", "error").
		Msg("sending error result")

	resultChan <- &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}

func (e *executor[R]) handleAcceptAnyValid(resultChan chan<- *failsafeCommon.PolicyResult[R], responses []*execResult[R], mostCommonResult string, exec failsafe.Execution[R], errFn func() error) {
	e.logger.Debug().
		Str("source", "accept_valid").
		Msg("sending first valid result")

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
				}
			}
		}
	}
	return highestBlock, highestBlockResult
}

func (e *executor[R]) handlePreferBlockHeadLeader(resultChan chan<- *failsafeCommon.PolicyResult[R], responses []*execResult[R], mostCommonResult string, exec failsafe.Execution[R], errFn func() error) {
	_, highestBlockResult := e.findBlockHeadLeader(responses)

	if highestBlockResult != nil {
		e.logger.Debug().
			Str("source", "block_head").
			Msg("sending block head leader result")

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
	e.handleAcceptAnyValid(resultChan, responses, mostCommonResult, exec, errFn)
}

func (e *executor[R]) handleOnlyBlockHeadLeader(resultChan chan<- *failsafeCommon.PolicyResult[R], responses []*execResult[R], exec failsafe.Execution[R], errFn func() error) {
	_, highestBlockResult := e.findBlockHeadLeader(responses)

	if highestBlockResult != nil {
		e.logger.Debug().
			Str("source", "only_block_head").
			Msg("sending block head leader result")
		resultChan <- &failsafeCommon.PolicyResult[R]{
			Result: highestBlockResult.result,
		}
		return
	}

	// If no block head leader found, return error
	e.logger.Debug().
		Str("source", "only_block_head_error").
		Msg("sending no block head leader error")

	resultChan <- &failsafeCommon.PolicyResult[R]{
		Error: errFn(),
	}
}
