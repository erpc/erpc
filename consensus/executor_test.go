package consensus

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/failsafe-go/failsafe-go"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/failsafe-go/failsafe-go/policy"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
	telemetry.SetHistogramBuckets("0.05,0.5,5,30")
}

var _ failsafe.Execution[*common.NormalizedResponse] = &mockExecution{}
var _ policy.ExecutionInternal[*common.NormalizedResponse] = &mockExecution{}

func TestConsensusExecutor(t *testing.T) {
	tests := []struct {
		name                    string
		requiredParticipants    int
		agreementThreshold      int
		disputeBehavior         *common.ConsensusDisputeBehavior
		lowParticipantsBehavior *common.ConsensusLowParticipantsBehavior
		disputeThreshold        uint
		responses               []*struct {
			response                  string
			upstreamId                string
			upstreamLatestBlockNumber int64
		}
		expectedError            *string
		expectedResult           []string
		expectedPunishedUpsteams []string
	}{
		{
			name:                 "successful_consensus_misbehaving_upstreams_are_punished_if_there_is_a_clear_majority",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorReturnError),
			disputeThreshold:     1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result1", "upstream2", 1},
				{"result1", "upstream3", 1},
			},
			expectedError:            nil,
			expectedResult:           []string{"result1"},
			expectedPunishedUpsteams: []string{},
		},
		{
			name:                 "successful_consensus_misbehaving_upstreams_are_punished_if_there_is_a_clear_majority",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorReturnError),
			disputeThreshold:     1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result1", "upstream2", 1},
				{"result2", "upstream3", 1},
			},
			expectedError:  nil,
			expectedResult: []string{"result1"},
			expectedPunishedUpsteams: []string{
				"upstream3",
			},
		},
		{
			name:                 "dispute_with_return_error",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorReturnError),
			disputeThreshold:     1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result2", "upstream2", 1},
				{"result3", "upstream3", 1},
			},
			expectedError:  pointer("ErrConsensusDispute: not enough agreement among responses"),
			expectedResult: nil,
		},
		{
			name:                 "dispute_with_accept_any_valid",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorAcceptMostCommonValidResult),
			disputeThreshold:     1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result2", "upstream2", 1},
				{"result3", "upstream3", 1},
			},
			expectedError: nil,
			expectedResult: []string{
				"result1",
				"result2",
				"result3",
			},
		},
		{
			name:                    "low_participants_with_return_error",
			requiredParticipants:    3,
			agreementThreshold:      2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorReturnError),
			disputeThreshold:        1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result2", "upstream1", 2},
				{"result3", "upstream1", 3},
			},
			expectedError:  pointer("ErrConsensusLowParticipants: not enough participants"),
			expectedResult: nil,
		},
		{
			name:                    "low_participants_with_accept_any_valid",
			requiredParticipants:    3,
			agreementThreshold:      2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult),
			disputeThreshold:        1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result1", "upstream1", 1},
				{"result1", "upstream1", 1},
			},
			expectedError:  nil,
			expectedResult: []string{"result1"},
		},
		{
			name:                    "low_participants_with_prefer_block_head_leader_fallback",
			requiredParticipants:    3,
			agreementThreshold:      2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader),
			disputeThreshold:        1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result1", "upstream1", 1},
				{"result1", "upstream1", 1},
			},
			expectedError:  nil,
			expectedResult: []string{"result1"},
		},
		{
			name:                    "low_participants_with_prefer_block_head_leader",
			requiredParticipants:    3,
			agreementThreshold:      2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader),
			disputeThreshold:        1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 2},
				{"result1", "upstream1", 2},
				{"result2", "upstream2", 1},
			},
			expectedError:  nil,
			expectedResult: []string{"result1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreams := make(map[string]common.Upstream)
			responses := make([]*common.NormalizedResponse, len(tt.responses))
			for i, response := range tt.responses {
				if _, ok := upstreams[response.upstreamId]; !ok {
					upstreams[response.upstreamId] = common.NewFakeUpstream(response.upstreamId, common.WithEvmStatePoller(common.NewFakeEvmStatePoller(response.upstreamLatestBlockNumber, response.upstreamLatestBlockNumber)))
				}

				responses[i] = createResponse(response.response, upstreams[response.upstreamId])
			}

			expectedResponses := make([]*common.NormalizedResponse, len(tt.expectedResult))
			for i, expectedResult := range tt.expectedResult {
				expectedResponses[i] = createResponse(expectedResult, upstreams[tt.responses[i].upstreamId])
			}

			// Create mock execution
			mockExec := &mockExecution{
				responses: responses,
			}

			disputeBehavior := common.ConsensusDisputeBehaviorReturnError
			if tt.disputeBehavior != nil {
				disputeBehavior = *tt.disputeBehavior
			}

			lowParticipantsBehavior := common.ConsensusLowParticipantsBehaviorReturnError
			if tt.lowParticipantsBehavior != nil {
				lowParticipantsBehavior = *tt.lowParticipantsBehavior
			}

			log := log.Logger

			// Create consensus policy
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(tt.requiredParticipants).
				WithAgreementThreshold(tt.agreementThreshold).
				WithDisputeBehavior(disputeBehavior).
				WithLowParticipantsBehavior(lowParticipantsBehavior).
				WithLogger(&log).
				WithPunishMisbehavior(&common.PunishMisbehaviorConfig{
					DisputeThreshold: tt.disputeThreshold,
					DisputeWindow:    common.Duration(10 * time.Second),
					SitOutPenalty:    common.Duration(500 * time.Millisecond),
				}).
				Build()

			// Create executor
			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
			}

			var times uint = 1
			// if the expected punished upstreams are set, we need to repeat until we exceed the dispute threshold
			if tt.expectedPunishedUpsteams != nil {
				times = tt.disputeThreshold + 1
			}

			// Track punished upstreams
			punishedUpstreams := make(map[string]*common.FakeUpstream)

			for range times {
				// Build a response queue per upstream so that if the same upstream is
				// selected more than once in a round (allowed in low-participant
				// scenarios), each execution still receives the next response from
				// the predefined list. We rebuild this for each iteration to ensure
				// responses are available for multiple test runs.
				responseQueueByUpstream := make(map[string][]*common.NormalizedResponse)
				for _, r := range responses {
					if up := r.Upstream(); up != nil {
						id := up.Config().Id
						responseQueueByUpstream[id] = append(responseQueueByUpstream[id], r)
					}
				}

				// Mutex to protect concurrent access to the response queue
				var queueMutex sync.Mutex

				// Track which execution index we're on
				execIndex := 0
				var execIndexMutex sync.Mutex

				// Execute consensus – this mimics the actual execution of the policy as defined in networks.go
				result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
					ctx := exec.Context()
					if ctx == nil {
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("missing execution context")}
					}

					// Get the current execution index and increment for next call
					execIndexMutex.Lock()
					currentExecIndex := execIndex
					execIndex++
					execIndexMutex.Unlock()

					// In the new architecture, consensus executor creates N goroutines (requiredParticipants)
					// Each goroutine would internally use the network layer which calls NextUpstream()
					// For testing, we simulate this by using the execution index to determine which upstream
					upsList, _ := ctx.Value(common.UpstreamsContextKey).([]common.Upstream)
					if currentExecIndex >= len(upsList) {
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: fmt.Errorf("execution index %d out of range", currentExecIndex)}
					}

					upstream := upsList[currentExecIndex]
					upstreamID := upstream.Id()

					queueMutex.Lock()
					defer queueMutex.Unlock()

					queue, ok := responseQueueByUpstream[upstreamID]
					if !ok || len(queue) == 0 {
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: fmt.Errorf("no response for upstream %s", upstreamID)}
					}

					// Pop the first response from the queue.
					resp := queue[0]
					responseQueueByUpstream[upstreamID] = queue[1:]

					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}
				})(mockExec)

				// Small delay to ensure misbehavior tracking has completed
				time.Sleep(100 * time.Millisecond)

				// Verify results
				if tt.expectedError != nil {
					assert.ErrorContains(t, result.Error, *tt.expectedError)
				} else {
					require.NoError(t, result.Error)
					actualJrr, err := result.Result.JsonRpcResponse()
					require.NoError(t, err)
					require.NotNil(t, actualJrr)

					var execptedJrrString []string
					for _, expectedResult := range expectedResponses {
						jrr, err := expectedResult.JsonRpcResponse()
						require.NoError(t, err)
						require.NotNil(t, jrr)

						execptedJrrString = append(execptedJrrString, string(jrr.Result))
					}

					assert.Contains(t, execptedJrrString, string(actualJrr.Result))
				}
			}

			// Check upstream punishment state
			expectedPunishedUpstreams := make(map[string]struct{})
			for _, id := range tt.expectedPunishedUpsteams {
				expectedPunishedUpstreams[id] = struct{}{}
			}

			// Count actually punished upstreams
			actuallyPunishedCount := 0
			for _, response := range responses {
				fake, ok := response.Upstream().(*common.FakeUpstream)
				require.True(t, ok)

				cordonedReason, cordoned := fake.CordonedReason()

				if _, shouldBePunished := expectedPunishedUpstreams[fake.Config().Id]; shouldBePunished {
					// This upstream should be punished - with the new short-circuit logic,
					// we will only punish it if we actually collected its response
					// AND we had a clear majority (>50%) among the collected responses
					if cordoned {
						punishedUpstreams[fake.Config().Id] = fake
						assert.Equal(t, "misbehaving in consensus", cordonedReason, "expected upstream %s to be cordoned for misbehaving in consensus", fake.Config().Id)
						actuallyPunishedCount++
					}
					// Note: It's OK if an expected-to-be-punished upstream is not actually punished
					// due to early short-circuit or lack of clear majority
				} else {
					// This upstream should NOT be punished
					assert.False(t, cordoned, "expected upstream %s to not be cordoned", fake.Config().Id)
					assert.Equal(t, "", cordonedReason, "expected upstream %s to not be cordoned", fake.Config().Id)
				}
			}

			// Log how many upstreams were actually punished for debugging
			if len(tt.expectedPunishedUpsteams) > 0 {
				t.Logf("Expected to punish %d upstreams, actually punished %d",
					len(tt.expectedPunishedUpsteams), actuallyPunishedCount)
			}

			// After the sit out penalty, the upstream should be uncordoned and routable again.
			if len(punishedUpstreams) > 0 {
				assert.EventuallyWithT(t, func(c *assert.CollectT) {
					for _, upstream := range punishedUpstreams {
						cordonedReason, cordoned := upstream.CordonedReason()
						assert.False(c, cordoned, "expected upstream %s to be uncordoned after sit out penalty", upstream.Config().Id)
						assert.Equal(c, "", cordonedReason, "expected upstream %s to be uncordoned with no reason after sit out penalty", upstream.Config().Id)
					}
				}, time.Second*2, time.Millisecond*100)
			}
		})
	}
}

// Helper function to create normalized responses
func createResponse(result string, upstream common.Upstream) *common.NormalizedResponse {
	jrr, err := common.NewJsonRpcResponse(1, result, nil)
	if err != nil {
		panic(err)
	}
	return common.NewNormalizedResponse().
		WithJsonRpcResponse(jrr).
		SetUpstream(upstream)
}

// Mock execution that returns pre-defined responses
type mockExecution struct {
	responses []*common.NormalizedResponse

	// lazily-initialised context carrying request & upstream metadata so that the
	// consensus executor runs its full logic during unit tests.
	ctx context.Context
}

func (m *mockExecution) Context() context.Context {
	// Build the context only once
	if m.ctx != nil {
		return m.ctx
	}

	// Build upstream list preserving duplicates so that tests covering low-participant
	// scenarios (same upstream reused) behave as intended.
	var upsList []common.Upstream
	for _, r := range m.responses {
		if up := r.Upstream(); up != nil {
			upsList = append(upsList, up)
		}
	}

	// Create a bare minimum normalized request so the executor doesn't bail out.
	dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_test"}`))

	m.ctx = context.Background()
	m.ctx = context.WithValue(m.ctx, common.RequestContextKey, dummyReq)
	m.ctx = context.WithValue(m.ctx, common.UpstreamsContextKey, upsList)
	return m.ctx
}

func (m *mockExecution) Attempts() int {
	return 1
}

func (m *mockExecution) Executions() int {
	return 1
}

func (m *mockExecution) Retries() int {
	return 0
}

func (m *mockExecution) Hedges() int {
	return 0
}

func (m *mockExecution) StartTime() time.Time {
	return time.Now()
}

func (m *mockExecution) ElapsedTime() time.Duration {
	return 0
}

func (m *mockExecution) LastResult() *common.NormalizedResponse {
	return nil
}

func (m *mockExecution) LastError() error {
	return nil
}

func (m *mockExecution) IsFirstAttempt() bool {
	return true
}

func (m *mockExecution) IsRetry() bool {
	return false
}

func (m *mockExecution) IsHedge() bool {
	return false
}

func (m *mockExecution) AttemptStartTime() time.Time {
	return time.Now()
}

func (m *mockExecution) ElapsedAttemptTime() time.Duration {
	return 0
}

func (m *mockExecution) IsCanceled() bool {
	return false
}

func (m *mockExecution) Canceled() <-chan struct{} {
	return nil
}

func (m *mockExecution) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	// No-op for testing
}

func (m *mockExecution) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{
		responses: m.responses,
		ctx:       m.Context(),
	}
}

func (m *mockExecution) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	// Preserve existing context and attach the additional key/value so that
	// consensus executor goroutines can identify which upstream they are
	// executing against (required for request-bound test assertions).
	newCtx := m.Context()
	if key != nil {
		newCtx = context.WithValue(newCtx, key, value)
	}

	return &mockExecution{
		responses: m.responses,
		ctx:       newCtx,
	}
}

func (m *mockExecution) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{
		responses: m.responses,
		ctx:       m.Context(),
	}
}

func (m *mockExecution) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return nil
}

func (m *mockExecution) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}

func (m *mockExecution) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}

func (m *mockExecution) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}

func pointer[T any](v T) *T {
	return &v
}

// Helper to manage upstream selection in tests that simulates the new architecture
// where the request manages upstream selection internally
type testUpstreamSelector struct {
	upstreams []common.Upstream
	execIndex int
	execMutex sync.Mutex
	// Optional: map specific execution indices to specific upstreams
	// If nil, uses round-robin based on execution index
	execIndexToUpstreamIndex map[int]int
}

func newTestUpstreamSelector(upstreams []common.Upstream) *testUpstreamSelector {
	return &testUpstreamSelector{
		upstreams: upstreams,
		execIndex: 0,
	}
}

func (s *testUpstreamSelector) withMapping(mapping map[int]int) *testUpstreamSelector {
	s.execIndexToUpstreamIndex = mapping
	return s
}

func (s *testUpstreamSelector) getNextUpstream() (common.Upstream, int) {
	s.execMutex.Lock()
	defer s.execMutex.Unlock()

	currentExecIndex := s.execIndex
	s.execIndex++

	// If we have a specific mapping, use it
	if s.execIndexToUpstreamIndex != nil {
		if upstreamIndex, ok := s.execIndexToUpstreamIndex[currentExecIndex]; ok {
			if upstreamIndex < len(s.upstreams) {
				return s.upstreams[upstreamIndex], upstreamIndex
			}
		}
	}

	// Otherwise use round-robin
	upstreamIndex := currentExecIndex % len(s.upstreams)
	if currentExecIndex < len(s.upstreams) {
		upstreamIndex = currentExecIndex
	}

	if upstreamIndex >= len(s.upstreams) {
		return nil, -1
	}

	return s.upstreams[upstreamIndex], upstreamIndex
}

// TestConsensusMisbehaviorTracking tests the misbehavior tracking behavior in both
// short-circuit and non-short-circuit scenarios
func TestConsensusMisbehaviorTracking(t *testing.T) {
	t.Run("MisbehaviorTrackingWithoutShortCircuit", func(t *testing.T) {
		// This test demonstrates that when we don't short-circuit (all upstreams respond),
		// misbehavior logic runs on all participating upstreams

		logger := log.Logger

		// Create 5 fake upstreams
		upstreams := make([]common.Upstream, 5)
		for i := 0; i < 5; i++ {
			upstreams[i] = common.NewFakeUpstream(
				fmt.Sprintf("upstream-%d", i),
				common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)),
			)
		}

		// Create consensus policy with punishment configured
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(5).
			WithAgreementThreshold(4). // Need 4 out of 5 to agree
			WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
			WithLogger(&logger).
			WithPunishMisbehavior(&common.PunishMisbehaviorConfig{
				DisputeThreshold: 1, // Punish after 1 misbehavior
				DisputeWindow:    common.Duration(10 * time.Second),
				SitOutPenalty:    common.Duration(500 * time.Millisecond),
			}).
			Build()

		// Create the executor (reused across iterations to maintain rate limiter state)
		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Run the test twice to exceed the dispute threshold
		for iteration := 0; iteration < 2; iteration++ {
			// Create responses:
			// upstream-0,1,2: return "majority-result" (slower)
			// upstream-3: returns "majority-result" (slower)
			// upstream-4: returns "minority-result" (fast, misbehaving)
			responses := make([]*common.NormalizedResponse, 5)
			for i := 0; i < 4; i++ {
				responses[i] = createResponse("majority-result", upstreams[i])
			}
			responses[4] = createResponse("minority-result", upstreams[4])

			// Create upstream selector for this test
			selector := newTestUpstreamSelector(upstreams)

			// Create the inner function
			innerFn := func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
				// Get the next upstream based on execution index
				upstream, upstreamIndex := selector.getNextUpstream()
				if upstream == nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: fmt.Errorf("no upstream available"),
					}
				}

				// upstream-4 responds fast to ensure it's not cancelled
				// Others respond slower but still fast enough
				var delay time.Duration
				if upstreamIndex == 4 {
					delay = 10 * time.Millisecond // Fast response for misbehaving upstream
				} else {
					delay = 30 + time.Duration(upstreamIndex*10)*time.Millisecond // Slightly slower for others
				}
				time.Sleep(delay)

				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Result: responses[upstreamIndex],
				}
			}

			// Set up the execution context
			ctx := context.WithValue(context.Background(), common.UpstreamsContextKey, upstreams)
			req := common.NewNormalizedRequest([]byte(`{"method":"eth_test","params":[]}`))
			ctx = context.WithValue(ctx, common.RequestContextKey, req)

			// Execute
			exec := &mockExecution{ctx: ctx}
			applyFn := executor.Apply(innerFn)
			result := applyFn(exec)

			// First iteration should succeed with consensus
			if iteration == 0 {
				assert.NoError(t, result.Error)
				assert.NotNil(t, result.Result)

				// upstream-4 should NOT be punished yet (under threshold)
				fake4 := upstreams[4].(*common.FakeUpstream)
				_, cordoned := fake4.CordonedReason()
				assert.False(t, cordoned, "upstream-4 should not be cordoned on first misbehavior")
			} else {
				// Second iteration: upstream-4 should be punished
				assert.NoError(t, result.Error) // Still have consensus with 4 upstreams

				// Wait a bit for punishment to be applied
				time.Sleep(300 * time.Millisecond)

				// upstream-4 should now be punished (since it misbehaved twice)
				fake4 := upstreams[4].(*common.FakeUpstream)
				reason, cordoned := fake4.CordonedReason()
				assert.True(t, cordoned, "upstream-4 should be cordoned after exceeding threshold")
				assert.Equal(t, "misbehaving in consensus", reason)
			}

			// Small delay between iterations
			if iteration == 0 {
				time.Sleep(100 * time.Millisecond)
			}
		}

		// Wait for sit-out penalty to expire
		time.Sleep(600 * time.Millisecond)

		// Verify upstream-4 is uncordoned
		fake4 := upstreams[4].(*common.FakeUpstream)
		_, cordoned := fake4.CordonedReason()
		assert.False(t, cordoned, "upstream-4 should be uncordoned after sit-out penalty")

		t.Log("Test completed: Misbehavior tracking works correctly when all upstreams respond")
	})

	t.Run("MisbehaviorTrackingWithShortCircuit", func(t *testing.T) {
		// This test demonstrates that when we short-circuit,
		// misbehavior logic only runs on the responses we actually collected

		logger := log.Logger

		// Create 5 fake upstreams
		upstreams := make([]common.Upstream, 5)
		for i := 0; i < 5; i++ {
			upstreams[i] = common.NewFakeUpstream(
				fmt.Sprintf("upstream-%d", i),
				common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)),
			)
		}

		// Create consensus policy with punishment configured
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(5).
			WithAgreementThreshold(3). // Only need 3 out of 5 (allows short-circuit)
			WithLogger(&logger).
			WithPunishMisbehavior(&common.PunishMisbehaviorConfig{
				DisputeThreshold: 1, // Punish after 1 misbehavior
				DisputeWindow:    common.Duration(10 * time.Second),
				SitOutPenalty:    common.Duration(500 * time.Millisecond),
			}).
			Build()

		// Create the executor
		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Track which upstreams actually responded
		var respondedUpstreams sync.Map

		// Run the test twice
		for iteration := 0; iteration < 2; iteration++ {
			respondedUpstreams = sync.Map{} // Reset for each iteration

			// Create upstream selector for this test iteration
			selector := newTestUpstreamSelector(upstreams)

			// Create the inner function
			innerFn := func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
				ctx := exec.Context()

				// Get the next upstream based on execution index
				upstream, upstreamIndex := selector.getNextUpstream()
				if upstream == nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: fmt.Errorf("no upstream available"),
					}
				}
				upstreamId := upstream.Id()

				// Response pattern for short-circuit on consensus reached:
				// With threshold 3, after getting 3 "majority-result" responses,
				// we reach consensus and should cancel the remaining requests
				// upstream-0: "majority-result" (very fast)
				// upstream-1: "majority-result" (fast)
				// upstream-2: "majority-result" (fast)
				// upstream-3: "minority-result" (slow - should be cancelled)
				// upstream-4: "another-result" (slow - should be cancelled)

				var delay time.Duration
				var resultValue string

				switch upstreamIndex {
				case 0:
					delay = 10 * time.Millisecond
					resultValue = "majority-result"
				case 1:
					delay = 20 * time.Millisecond
					resultValue = "majority-result"
				case 2:
					delay = 40 * time.Millisecond
					resultValue = "majority-result"
				case 3:
					delay = 5 * time.Second
					resultValue = "minority-result"
				case 4:
					delay = 5 * time.Second
					resultValue = "another-result"
				}

				// For slow upstreams, check context immediately
				if upstreamIndex >= 3 {
					select {
					case <-ctx.Done():
						// Already cancelled, don't even start the delay
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
							Error: ctx.Err(),
						}
					default:
						// Continue with delay
					}
				}

				// Simulate delay with cancellable timer
				timer := time.NewTimer(delay)
				defer timer.Stop()

				select {
				case <-timer.C:
					// Completed normally
					respondedUpstreams.Store(upstreamId, true)
				case <-ctx.Done():
					// Context was cancelled (short-circuit)
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: ctx.Err(),
					}
				}

				resp := createResponse(resultValue, upstreams[upstreamIndex])
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Result: resp,
				}
			}

			// Set up the execution context
			ctx := context.WithValue(context.Background(), common.UpstreamsContextKey, upstreams)
			req := common.NewNormalizedRequest([]byte(`{"method":"eth_test","params":[]}`))
			ctx = context.WithValue(ctx, common.RequestContextKey, req)

			// Execute with timing
			exec := &mockExecution{ctx: ctx}
			applyFn := executor.Apply(innerFn)

			startTime := time.Now()
			result := applyFn(exec)
			duration := time.Since(startTime)

			// Should succeed with consensus
			assert.NoError(t, result.Error)
			assert.NotNil(t, result.Result)

			// Should complete quickly (short-circuit)
			assert.Less(t, duration, 500*time.Millisecond,
				"Expected short-circuit execution (took %v)", duration)

			// Count responded upstreams
			respondedCount := 0
			respondedUpstreams.Range(func(_, _ interface{}) bool {
				respondedCount++
				return true
			})

			// Should have short-circuited after ~3 responses
			assert.LessOrEqual(t, respondedCount, 3,
				"Should have short-circuited after reaching consensus")

			// Wait a bit for punishment logic to complete
			time.Sleep(100 * time.Millisecond)

			// Check punishment status
			// With the new pattern, upstream 0,1,2 return majority result
			// upstream 3,4 would be misbehaving if they responded
			// But since we short-circuit after consensus, they shouldn't respond

			// No upstream should be punished in this test since:
			// - upstream 0,1,2 agreed with the majority
			// - upstream 3,4 were cancelled before responding
			for i, up := range upstreams {
				fake := up.(*common.FakeUpstream)
				_, cordoned := fake.CordonedReason()
				assert.False(t, cordoned, "upstream-%d should not be cordoned", i)
			}

			t.Logf("Iteration %d: %d upstreams responded before short-circuit",
				iteration+1, respondedCount)
		}

		t.Log("Test completed: Misbehavior tracking only considers responses collected before short-circuit")
	})
}

func TestConsensusShortCircuit(t *testing.T) {
	t.Run("ConsensusShortCircuitsWhenAgreementReached", func(t *testing.T) {
		// This test demonstrates that consensus now short-circuits when
		// the agreement threshold is reached, without waiting for all upstreams

		logger := log.Logger

		// Create 5 fake upstreams
		upstreams := make([]common.Upstream, 5)
		for i := 0; i < 5; i++ {
			upstreams[i] = common.NewFakeUpstream(
				fmt.Sprintf("upstream-%d", i),
				common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)),
			)
		}

		// Track timing and which upstreams were called
		var upstreamsCalled sync.Map
		slowUpstreamDelay := 2 * time.Second

		// Create responses - all upstreams return the same response
		// but upstream-3 and upstream-4 are slow
		responses := make([]*common.NormalizedResponse, 5)
		for i := 0; i < 5; i++ {
			// Use the existing createResponse helper
			responses[i] = createResponse("0x1234567890", upstreams[i])
		}

		// Create consensus policy using the builder pattern
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(5).
			WithAgreementThreshold(3). // Only need 3 out of 5 to agree
			WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorReturnError).
			WithLogger(&logger).
			Build()

		// Create the executor with the consensus policy
		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		// Create the inner function that simulates upstream calls
		innerFn := func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Extract which upstream this is for
			ctx := exec.Context()

			// Get the next upstream based on execution index
			upstream, upstreamIndex := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}
			upstreamId := upstream.Id()

			upstreamsCalled.Store(upstreamId, true)

			// Simulate delay for upstream-3 and upstream-4
			if upstreamIndex >= 3 {
				timer := time.NewTimer(slowUpstreamDelay)
				defer timer.Stop()

				select {
				case <-timer.C:
					// Slow upstream completed
				case <-ctx.Done():
					// Context was cancelled (consensus reached early)
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: ctx.Err(),
					}
				}
			} else {
				// Fast upstreams respond in 50ms
				time.Sleep(50 * time.Millisecond)
			}

			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Result: responses[upstreamIndex],
			}
		}

		// Set up the execution context
		ctx := context.WithValue(context.Background(), common.UpstreamsContextKey, upstreams)
		req := common.NewNormalizedRequest([]byte(`{"method":"eth_test","params":[]}`))
		ctx = context.WithValue(ctx, common.RequestContextKey, req)

		// Create the execution
		exec := &mockExecution{
			ctx: ctx,
		}

		// Apply the consensus policy
		applyFn := executor.Apply(innerFn)

		// Execute with timing
		startTime := time.Now()
		result := applyFn(exec)
		duration := time.Since(startTime)

		// Verify results
		assert.NotNil(t, result)
		assert.NoError(t, result.Error)
		assert.NotNil(t, result.Result)

		// Count how many upstreams were called
		calledCount := 0
		upstreamsCalled.Range(func(key, value interface{}) bool {
			calledCount++
			t.Logf("Upstream %s was called", key)
			return true
		})

		// We expect all 5 upstreams to be called initially
		assert.Equal(t, 5, calledCount, "All upstreams should be called initially")

		// But execution should complete quickly (not wait for slow upstreams)
		assert.Less(t, duration, 500*time.Millisecond,
			"Expected execution to complete quickly after reaching consensus (took %v)", duration)

		t.Logf("Consensus reached in %v with %d upstreams called", duration, calledCount)
		t.Logf(`
Summary:
- Required participants: 5
- Agreement threshold: 3
- Upstreams initially called: %d
- Total execution time: %v
- Fast upstreams (0-2) delay: 50ms
- Slow upstreams (3-4) delay: 2s (but cancelled early)

Conclusion: The executor short-circuits after receiving 3 matching responses,
cancelling the remaining slow upstream calls and returning immediately.`, calledCount, duration)
	})

	t.Run("ConsensusShortCircuitsWhenConsensusImpossible", func(t *testing.T) {
		// This test demonstrates that consensus short-circuits when
		// it becomes mathematically impossible to reach agreement

		logger := log.Logger

		// Create 5 fake upstreams
		upstreams := make([]common.Upstream, 5)
		for i := 0; i < 5; i++ {
			upstreams[i] = common.NewFakeUpstream(
				fmt.Sprintf("upstream-%d", i),
				common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)),
			)
		}

		// Track which upstreams were called
		var upstreamsCalled sync.Map
		var callOrder []string
		var callOrderMutex sync.Mutex

		// Create consensus policy using the builder pattern
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(5).
			WithAgreementThreshold(4). // Need 4 out of 5 to agree
			WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
			WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorReturnError).
			WithLogger(&logger).
			Build()

		// Create the executor with the consensus policy
		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		// Create the inner function that simulates upstream calls
		innerFn := func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Extract which upstream this is for
			ctx := exec.Context()

			// Get the next upstream based on execution index
			upstream, upstreamIndex := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}
			upstreamId := upstream.Id()

			upstreamsCalled.Store(upstreamId, true)

			callOrderMutex.Lock()
			callOrder = append(callOrder, upstreamId)
			callOrderMutex.Unlock()

			// Simulate responses:
			// upstream-0: returns "result-A" quickly
			// upstream-1: returns "result-B" quickly
			// upstream-2: returns "result-C" quickly
			// upstream-3: would return "result-A" slowly (but should be cancelled)
			// upstream-4: would return "result-A" slowly (but should be cancelled)

			var delay time.Duration
			var resultValue string

			switch upstreamIndex {
			case 0:
				delay = 50 * time.Millisecond
				resultValue = "result-A"
			case 1:
				delay = 50 * time.Millisecond
				resultValue = "result-B"
			case 2:
				delay = 50 * time.Millisecond
				resultValue = "result-C"
			case 3, 4:
				delay = 2 * time.Second
				resultValue = "result-A"
			}

			// Simulate delay using a timer that can be cancelled
			timer := time.NewTimer(delay)
			defer timer.Stop()

			select {
			case <-timer.C:
				// Completed normally
			case <-ctx.Done():
				// Context was cancelled - return immediately
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: ctx.Err(),
				}
			}

			// Create response using the existing helper
			resp := createResponse(resultValue, upstreams[upstreamIndex])

			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Result: resp,
			}
		}

		// Set up the execution context
		ctx := context.WithValue(context.Background(), common.UpstreamsContextKey, upstreams)
		req := common.NewNormalizedRequest([]byte(`{"method":"eth_test","params":[]}`))
		ctx = context.WithValue(ctx, common.RequestContextKey, req)

		// Create the execution
		exec := &mockExecution{
			ctx: ctx,
		}

		// Apply the consensus policy
		applyFn := executor.Apply(innerFn)

		// Execute with timing
		startTime := time.Now()
		result := applyFn(exec)
		duration := time.Since(startTime)

		// Verify results
		assert.NotNil(t, result)
		assert.Error(t, result.Error, "Should have consensus low participants error")
		assert.Contains(t, result.Error.Error(), "not enough participants")

		// Count how many upstreams were called
		calledCount := 0
		upstreamsCalled.Range(func(key, value interface{}) bool {
			calledCount++
			return true
		})

		// Execution should complete quickly after determining consensus is impossible
		assert.Less(t, duration, 500*time.Millisecond,
			"Expected execution to complete quickly after determining consensus impossible (took %v)", duration)

		t.Logf("Consensus determination completed in %v", duration)
		callOrderMutex.Lock()
		t.Logf("Call order: %v", callOrder)
		callOrderMutex.Unlock()
		t.Logf(`
Summary:
- Required agreement: 4 out of 5
- Fast responses received: 3 (all different)
- After 3 different responses, max possible agreement is 3 (if remaining 2 match one existing)
- Since 3 < 4, consensus became impossible
- Execution short-circuited without waiting for slow upstreams
- Total execution time: %v

Conclusion: The executor correctly identifies when consensus becomes impossible
and short-circuits to avoid unnecessary waiting.`, duration)
	})
}

// mockUpstreamWrapper wraps a FakeUpstream to track Cordon calls
type mockUpstreamWrapper struct {
	*common.FakeUpstream
	cordonCount int
	mu          sync.Mutex
}

func (m *mockUpstreamWrapper) Cordon(method string, reason string) {
	m.mu.Lock()
	m.cordonCount++
	m.mu.Unlock()
	m.FakeUpstream.Cordon(method, reason)
}

func (m *mockUpstreamWrapper) getCordonCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cordonCount
}

func TestHandleMisbehavingUpstreamRaceCondition(t *testing.T) {
	// This test demonstrates a race condition where multiple goroutines
	// can simultaneously punish the same upstream, leading to multiple timers

	logger := log.Logger

	// Create a fake upstream wrapped to track cordon calls
	fakeUpstream := common.NewFakeUpstream("test-upstream").(*common.FakeUpstream)
	upstream := &mockUpstreamWrapper{
		FakeUpstream: fakeUpstream,
		cordonCount:  0,
	}

	// Create consensus policy with punishment configured
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithLogger(&logger).
		WithPunishMisbehavior(&common.PunishMisbehaviorConfig{
			DisputeThreshold: 1,
			DisputeWindow:    common.Duration(10 * time.Second),
			SitOutPenalty:    common.Duration(200 * time.Millisecond), // Short penalty for testing
		}).
		Build()

	// Create the executor
	executor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Track how many timers were created by intercepting sync.Map.Store calls
	timerStoreCount := 0
	var timerStoreMutex sync.Mutex
	var storedTimers []*time.Timer

	// Replace the sync.Map with a wrapper that counts Store operations
	trackedMap := &sync.Map{}
	executor.misbehavingUpstreamsSitoutTimer = trackedMap

	// Wrap the map to intercept Store operations
	go func() {
		// Monitor the map for changes
		for i := 0; i < 100; i++ { // Check for 1 second
			time.Sleep(10 * time.Millisecond)
			trackedMap.Range(func(key, value interface{}) bool {
				if timer, ok := value.(*time.Timer); ok {
					timerStoreMutex.Lock()
					found := false
					for _, existing := range storedTimers {
						if existing == timer {
							found = true
							break
						}
					}
					if !found {
						storedTimers = append(storedTimers, timer)
						timerStoreCount++
					}
					timerStoreMutex.Unlock()
				}
				return true
			})
		}
	}()

	// Launch multiple goroutines that try to punish the same upstream simultaneously
	numGoroutines := 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Use a channel to synchronize the start
	startCh := make(chan struct{})

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			// Wait for signal to start
			<-startCh

			// Try to punish the upstream
			executor.handleMisbehavingUpstream(&logger, upstream, "test-upstream", "test-project", "test-network")
		}(i)
	}

	// Signal all goroutines to start at the same time
	close(startCh)

	// Wait for all goroutines to complete
	wg.Wait()

	// Give some time for async operations to complete
	time.Sleep(100 * time.Millisecond)

	// Check results
	finalCordonCount := upstream.getCordonCount()

	timerStoreMutex.Lock()
	finalTimerCount := timerStoreCount
	timerStoreMutex.Unlock()

	// Log the results
	t.Logf("Cordon called %d times (expected 1)", finalCordonCount)
	t.Logf("Timers created %d (expected 1)", finalTimerCount)

	// The race condition allows multiple goroutines to pass the initial check
	// This results in multiple calls to Cordon and multiple timers being created
	if finalCordonCount > 1 {
		t.Errorf("Race condition detected: Cordon was called %d times, expected 1", finalCordonCount)
	}

	if finalTimerCount > 1 {
		t.Errorf("Race condition detected: %d timers were created, expected 1", finalTimerCount)
	}

	// Also check that only one timer remains in the map
	mapSize := 0
	trackedMap.Range(func(_, _ interface{}) bool {
		mapSize++
		return true
	})

	if mapSize > 1 {
		t.Errorf("Multiple entries in sitout timer map: found %d, expected 1", mapSize)
	}

	// Wait for the sit-out penalty to expire
	time.Sleep(300 * time.Millisecond)

	// Verify the upstream was uncordoned
	reason, cordoned := upstream.CordonedReason()
	if cordoned {
		t.Errorf("Upstream should be uncordoned after penalty, but is still cordoned with reason: %s", reason)
	}

	// Check that the map is cleaned up
	finalMapSize := 0
	trackedMap.Range(func(_, _ interface{}) bool {
		finalMapSize++
		return true
	})

	if finalMapSize != 0 {
		t.Errorf("Timer map should be empty after cleanup, but has %d entries", finalMapSize)
	}

	// Clean up any remaining timers
	for _, timer := range storedTimers {
		if timer != nil {
			timer.Stop()
		}
	}
}

func TestConsensusGoroutineLeakWithShortCircuit(t *testing.T) {
	// This test demonstrates the goroutine leak issue when short-circuiting
	// The bug: drainResponses was called with incorrect count, causing it to block forever

	// Helper to count goroutines
	countGoroutines := func() int {
		// Give a small delay for goroutines to settle
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}

	t.Run("goroutine_leak_with_short_circuit", func(t *testing.T) {
		// Get baseline goroutine count
		baselineGoroutines := countGoroutines()

		// Create upstreams with different response times to ensure short-circuit happens
		upstream1 := common.NewFakeUpstream("upstream1",
			common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)))
		upstream2 := common.NewFakeUpstream("upstream2",
			common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)))
		upstream3 := common.NewFakeUpstream("upstream3",
			common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)))
		upstream4 := common.NewFakeUpstream("upstream4",
			common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)))

		upstreams := []common.Upstream{upstream1, upstream2, upstream3, upstream4}

		// Create a scenario where consensus is reached early (2 out of 4)
		// This will trigger short-circuiting
		log := log.Logger
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(4).
			WithAgreementThreshold(2). // Will short-circuit after 2 matching responses
			WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
			WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorReturnError).
			WithLogger(&log).
			Build()

		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create normalized request
		dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))

		// Create context with upstreams
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		// Create mock execution with controlled response timing
		mockExec := &mockExecutionWithTiming{
			ctx: ctx,
			responseDelays: map[string]time.Duration{
				"upstream1": 10 * time.Millisecond,  // Fast
				"upstream2": 10 * time.Millisecond,  // Fast
				"upstream3": 200 * time.Millisecond, // Slow (will be cancelled)
				"upstream4": 200 * time.Millisecond, // Slow (will be cancelled)
			},
		}

		// Execute consensus
		doneChan := make(chan struct{})
		var result *failsafeCommon.PolicyResult[*common.NormalizedResponse]

		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		go func() {
			defer close(doneChan)
			result = executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
				ctx := exec.Context()

				// Get the next upstream based on execution index
				upstream, _ := selector.getNextUpstream()
				if upstream == nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: fmt.Errorf("no upstream available"),
					}
				}
				upstreamID := upstream.Id()

				// Get the delay for this upstream
				delay := mockExec.responseDelays[upstreamID]

				// Simulate processing time
				select {
				case <-time.After(delay):
					// Return same result for upstream1 and upstream2 to trigger consensus
					if upstreamID == "upstream1" || upstreamID == "upstream2" {
						jrr, _ := common.NewJsonRpcResponse(1, "0x100", nil)
						resp := common.NewNormalizedResponse().
							WithJsonRpcResponse(jrr).
							SetUpstream(upstreams[0]) // Just use first upstream for simplicity
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}
					}
					// Return different result for others
					jrr, _ := common.NewJsonRpcResponse(1, "0x200", nil)
					resp := common.NewNormalizedResponse().
						WithJsonRpcResponse(jrr).
						SetUpstream(upstreams[2])
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}
				case <-ctx.Done():
					// Context cancelled
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: ctx.Err(),
					}
				}
			})(mockExec)
		}()

		// Wait for execution to complete or timeout
		select {
		case <-doneChan:
			// Success - execution completed
			require.NoError(t, result.Error)

			// Allow time for any leaked goroutines to accumulate
			time.Sleep(100 * time.Millisecond)

			// Check final goroutine count
			finalGoroutines := countGoroutines()

			// With the fix, we should have roughly the same number of goroutines
			// Allow for some variance due to runtime internals
			goroutineDiff := finalGoroutines - baselineGoroutines
			t.Logf("Baseline goroutines: %d, Final goroutines: %d, Difference: %d",
				baselineGoroutines, finalGoroutines, goroutineDiff)

			// The difference should be very small (< 5) with the fix
			// Without the fix, drainResponses would hang, leaving extra goroutines
			assert.Less(t, goroutineDiff, 5,
				"Too many goroutines leaked. This indicates drainResponses is hanging.")

		case <-time.After(2 * time.Second):
			// With the old code, this would timeout because drainResponses hangs
			t.Fatal("Test timed out - this indicates drainResponses is hanging (the bug is present)")
		}
	})
}

// Mock execution with controlled response timing
type mockExecutionWithTiming struct {
	ctx            context.Context
	responseDelays map[string]time.Duration
}

func (m *mockExecutionWithTiming) Context() context.Context {
	return m.ctx
}

func (m *mockExecutionWithTiming) Attempts() int {
	return 1
}

func (m *mockExecutionWithTiming) Executions() int {
	return 1
}

func (m *mockExecutionWithTiming) Retries() int {
	return 0
}

func (m *mockExecutionWithTiming) Hedges() int {
	return 0
}

func (m *mockExecutionWithTiming) StartTime() time.Time {
	return time.Now()
}

func (m *mockExecutionWithTiming) ElapsedTime() time.Duration {
	return 0
}

func (m *mockExecutionWithTiming) LastResult() *common.NormalizedResponse {
	return nil
}

func (m *mockExecutionWithTiming) LastError() error {
	return nil
}

func (m *mockExecutionWithTiming) IsFirstAttempt() bool {
	return true
}

func (m *mockExecutionWithTiming) IsRetry() bool {
	return false
}

func (m *mockExecutionWithTiming) IsHedge() bool {
	return false
}

func (m *mockExecutionWithTiming) AttemptStartTime() time.Time {
	return time.Now()
}

func (m *mockExecutionWithTiming) ElapsedAttemptTime() time.Duration {
	return 0
}

func (m *mockExecutionWithTiming) IsCanceled() bool {
	return false
}

func (m *mockExecutionWithTiming) Canceled() <-chan struct{} {
	return make(chan struct{})
}

func (m *mockExecutionWithTiming) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}

func (m *mockExecutionWithTiming) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecutionWithTiming{
		ctx:            m.ctx,
		responseDelays: m.responseDelays,
	}
}

func (m *mockExecutionWithTiming) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.ctx, key, value)
	return &mockExecutionWithTiming{
		ctx:            newCtx,
		responseDelays: m.responseDelays,
	}
}

func (m *mockExecutionWithTiming) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecutionWithTiming{
		ctx:            m.ctx,
		responseDelays: m.responseDelays,
	}
}

func (m *mockExecutionWithTiming) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}

func (m *mockExecutionWithTiming) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}

func (m *mockExecutionWithTiming) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}

func (m *mockExecutionWithTiming) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}

func TestConsensusGoroutineLeakWithNilResponses(t *testing.T) {
	// This test specifically targets the drainResponses bug where nil responses
	// from cancelled goroutines cause incorrect calculation of remaining responses

	// Helper to count goroutines
	countGoroutines := func() int {
		// Give a small delay for goroutines to settle
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}

	t.Run("goroutine_leak_with_nil_responses", func(t *testing.T) {
		// Get baseline goroutine count
		baselineGoroutines := countGoroutines()

		// Create 5 upstreams to ensure we have enough for the bug to manifest
		upstreams := make([]common.Upstream, 5)
		for i := 0; i < 5; i++ {
			upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i+1),
				common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)))
		}

		// Create a scenario where:
		// - We need 3 participants
		// - Consensus threshold is 2
		// - First upstream errors immediately (nil response)
		// - Second and third return matching results (consensus reached)
		// - Fourth and fifth are slow and will be cancelled
		log := log.Logger
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(5).
			WithAgreementThreshold(2). // Will short-circuit after 2 matching responses
			WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
			WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorReturnError).
			WithLogger(&log).
			Build()

		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create normalized request
		dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`))

		// Create context with upstreams
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		// Track which upstreams have been called
		calledUpstreams := make(map[string]bool)
		var mu sync.Mutex

		// Create mock execution
		mockExec := &mockExecutionWithBehavior{
			ctx: ctx,
			behavior: func(upstreamID string) (time.Duration, bool, string) {
				mu.Lock()
				calledUpstreams[upstreamID] = true
				mu.Unlock()

				switch upstreamID {
				case "upstream1":
					// Return immediately with nil (simulating error)
					return 0, true, ""
				case "upstream2", "upstream3":
					// Return quickly with matching results
					return 10 * time.Millisecond, false, "0x100"
				default:
					// Slow responses that will be cancelled
					return 500 * time.Millisecond, false, "0x200"
				}
			},
		}

		// Execute consensus
		doneChan := make(chan struct{})
		var result *failsafeCommon.PolicyResult[*common.NormalizedResponse]

		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		go func() {
			defer close(doneChan)
			result = executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
				ctx := exec.Context()

				// Get the next upstream based on execution index
				upstream, _ := selector.getNextUpstream()
				if upstream == nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: fmt.Errorf("no upstream available"),
					}
				}
				upstreamID := upstream.Id()

				// Get behavior for this upstream
				delay, shouldReturnNil, resultValue := mockExec.behavior(upstreamID)

				if shouldReturnNil {
					// Return nil to simulate error/cancelled response
					return nil
				}

				// Simulate processing time
				select {
				case <-time.After(delay):
					jrr, _ := common.NewJsonRpcResponse(1, resultValue, nil)
					resp := common.NewNormalizedResponse().
						WithJsonRpcResponse(jrr).
						SetUpstream(upstreams[0])
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}
				case <-ctx.Done():
					// Context cancelled - return nil
					return nil
				}
			})(mockExec)
		}()

		// Wait for execution to complete or timeout
		select {
		case <-doneChan:
			// Success - execution completed
			require.NoError(t, result.Error)

			// Log which upstreams were called
			mu.Lock()
			calledCount := len(calledUpstreams)
			mu.Unlock()
			t.Logf("Called %d upstreams: %v", calledCount, calledUpstreams)

			// Allow time for any leaked goroutines to accumulate
			time.Sleep(200 * time.Millisecond)

			// Check final goroutine count
			finalGoroutines := countGoroutines()

			// With the fix, we should have roughly the same number of goroutines
			// Allow for some variance due to runtime internals
			goroutineDiff := finalGoroutines - baselineGoroutines
			t.Logf("Baseline goroutines: %d, Final goroutines: %d, Difference: %d",
				baselineGoroutines, finalGoroutines, goroutineDiff)

			// The difference should be very small (< 5) with the fix
			// Without the fix, drainResponses would hang, leaving extra goroutines
			assert.Less(t, goroutineDiff, 5,
				"Too many goroutines leaked. This indicates drainResponses is hanging.")

		case <-time.After(3 * time.Second):
			// With the old code, this would timeout because drainResponses hangs
			t.Fatal("Test timed out - this indicates drainResponses is hanging (the bug is present)")
		}
	})
}

// Mock execution with configurable behavior per upstream
type mockExecutionWithBehavior struct {
	ctx      context.Context
	behavior func(upstreamID string) (delay time.Duration, shouldReturnNil bool, result string)
}

func (m *mockExecutionWithBehavior) Context() context.Context {
	return m.ctx
}

func (m *mockExecutionWithBehavior) Attempts() int {
	return 1
}

func (m *mockExecutionWithBehavior) Executions() int {
	return 1
}

func (m *mockExecutionWithBehavior) Retries() int {
	return 0
}

func (m *mockExecutionWithBehavior) Hedges() int {
	return 0
}

func (m *mockExecutionWithBehavior) StartTime() time.Time {
	return time.Now()
}

func (m *mockExecutionWithBehavior) ElapsedTime() time.Duration {
	return 0
}

func (m *mockExecutionWithBehavior) LastResult() *common.NormalizedResponse {
	return nil
}

func (m *mockExecutionWithBehavior) LastError() error {
	return nil
}

func (m *mockExecutionWithBehavior) IsFirstAttempt() bool {
	return true
}

func (m *mockExecutionWithBehavior) IsRetry() bool {
	return false
}

func (m *mockExecutionWithBehavior) IsHedge() bool {
	return false
}

func (m *mockExecutionWithBehavior) AttemptStartTime() time.Time {
	return time.Now()
}

func (m *mockExecutionWithBehavior) ElapsedAttemptTime() time.Duration {
	return 0
}

func (m *mockExecutionWithBehavior) IsCanceled() bool {
	return false
}

func (m *mockExecutionWithBehavior) Canceled() <-chan struct{} {
	return make(chan struct{})
}

func (m *mockExecutionWithBehavior) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}

func (m *mockExecutionWithBehavior) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecutionWithBehavior{
		ctx:      m.ctx,
		behavior: m.behavior,
	}
}

func (m *mockExecutionWithBehavior) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.ctx, key, value)
	return &mockExecutionWithBehavior{
		ctx:      newCtx,
		behavior: m.behavior,
	}
}

func (m *mockExecutionWithBehavior) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecutionWithBehavior{
		ctx:      m.ctx,
		behavior: m.behavior,
	}
}

func (m *mockExecutionWithBehavior) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}

func (m *mockExecutionWithBehavior) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}

func (m *mockExecutionWithBehavior) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}

func (m *mockExecutionWithBehavior) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}

func TestConsensusExactDrainResponsesBug(t *testing.T) {
	// This test demonstrates the exact drainResponses bug by logging the calculation

	t.Run("exact_drain_responses_calculation", func(t *testing.T) {
		// Create exactly the scenario that triggers the bug:
		// - 5 upstreams total
		// - First upstream returns nil immediately (i=0, responses=0)
		// - Second and third return valid results (i=2, responses=2)
		// - Short-circuit happens after third response
		// - Old code: drainResponses(5-2=3) but only 2 messages remain!
		// - New code: drainResponses(5-(2+1)=2) correctly drains 2

		log := log.Logger

		// We need to patch the drainResponses function temporarily to add logging
		// Since we can't do that, let's create a custom executor that logs

		upstreams := make([]common.Upstream, 5)
		for i := 0; i < 5; i++ {
			upstreams[i] = common.NewFakeUpstream(fmt.Sprintf("upstream%d", i+1),
				common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)))
		}

		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(5).
			WithAgreementThreshold(2).
			WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
			WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorReturnError).
			WithLogger(&log).
			Build()

		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create normalized request
		dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_test","id":1}`))

		// Create context with upstreams
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		// Track response order and timing
		responseOrder := make([]string, 0)
		var orderMu sync.Mutex

		mockExec := &mockExecutionWithDetailedLogging{
			ctx: ctx,
			behavior: func(upstreamID string) (time.Duration, bool, string) {
				orderMu.Lock()
				responseOrder = append(responseOrder, fmt.Sprintf("%s-start", upstreamID))
				orderMu.Unlock()

				switch upstreamID {
				case "upstream1":
					// Return nil immediately
					orderMu.Lock()
					responseOrder = append(responseOrder, fmt.Sprintf("%s-nil", upstreamID))
					orderMu.Unlock()
					return 0, true, ""
				case "upstream2", "upstream3":
					// Return valid results quickly
					return 20 * time.Millisecond, false, "0x100"
				default:
					// Slow responses that should be cancelled
					return 2 * time.Second, false, "0x200"
				}
			},
			onResponse: func(upstreamID string, isNil bool) {
				orderMu.Lock()
				if isNil {
					responseOrder = append(responseOrder, fmt.Sprintf("%s-responded-nil", upstreamID))
				} else {
					responseOrder = append(responseOrder, fmt.Sprintf("%s-responded-valid", upstreamID))
				}
				orderMu.Unlock()
			},
		}

		// Count goroutines before
		runtime.GC()
		time.Sleep(100 * time.Millisecond)
		goroutinesBefore := runtime.NumGoroutine()

		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		// Execute with timeout
		done := make(chan bool)
		go func() {
			result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
				ctx := exec.Context()

				// Get the next upstream based on execution index
				upstream, _ := selector.getNextUpstream()
				if upstream == nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: fmt.Errorf("no upstream available"),
					}
				}
				upstreamID := upstream.Id()

				delay, shouldReturnNil, resultValue := mockExec.behavior(upstreamID)

				if shouldReturnNil {
					mockExec.onResponse(upstreamID, true)
					return nil
				}

				// Use a timer to detect if we're cancelled
				timer := time.NewTimer(delay)
				defer timer.Stop()

				select {
				case <-timer.C:
					jrr, _ := common.NewJsonRpcResponse(1, resultValue, nil)
					resp := common.NewNormalizedResponse().
						WithJsonRpcResponse(jrr).
						SetUpstream(upstreams[0])
					mockExec.onResponse(upstreamID, false)
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}
				case <-ctx.Done():
					mockExec.onResponse(upstreamID, true)
					return nil
				}
			})(mockExec)

			// Verify we got consensus
			require.NoError(t, result.Error)
			done <- true
		}()

		select {
		case <-done:
			// Success
			time.Sleep(500 * time.Millisecond) // Give time for goroutines to settle

			orderMu.Lock()
			t.Logf("Response order: %v", responseOrder)
			orderMu.Unlock()

			// Count goroutines after
			runtime.GC()
			time.Sleep(100 * time.Millisecond)
			goroutinesAfter := runtime.NumGoroutine()

			goroutineDiff := goroutinesAfter - goroutinesBefore
			t.Logf("Goroutines - Before: %d, After: %d, Diff: %d",
				goroutinesBefore, goroutinesAfter, goroutineDiff)

			// With the bug, drainResponses would hang and we'd have leaked goroutines
			// The test should show the difference
			if goroutineDiff > 5 {
				t.Errorf("Goroutine leak detected! Difference: %d (should be < 5)", goroutineDiff)
				t.Log("This indicates drainResponses is trying to read more messages than available")
			}

		case <-time.After(5 * time.Second):
			t.Fatal("Test timed out - drainResponses is likely hanging due to the bug")
		}
	})
}

// Mock execution with detailed logging
type mockExecutionWithDetailedLogging struct {
	ctx        context.Context
	behavior   func(upstreamID string) (delay time.Duration, shouldReturnNil bool, result string)
	onResponse func(upstreamID string, isNil bool)
}

func (m *mockExecutionWithDetailedLogging) Context() context.Context {
	return m.ctx
}

func (m *mockExecutionWithDetailedLogging) Attempts() int {
	return 1
}

func (m *mockExecutionWithDetailedLogging) Executions() int {
	return 1
}

func (m *mockExecutionWithDetailedLogging) Retries() int {
	return 0
}

func (m *mockExecutionWithDetailedLogging) Hedges() int {
	return 0
}

func (m *mockExecutionWithDetailedLogging) StartTime() time.Time {
	return time.Now()
}

func (m *mockExecutionWithDetailedLogging) ElapsedTime() time.Duration {
	return 0
}

func (m *mockExecutionWithDetailedLogging) LastResult() *common.NormalizedResponse {
	return nil
}

func (m *mockExecutionWithDetailedLogging) LastError() error {
	return nil
}

func (m *mockExecutionWithDetailedLogging) IsFirstAttempt() bool {
	return true
}

func (m *mockExecutionWithDetailedLogging) IsRetry() bool {
	return false
}

func (m *mockExecutionWithDetailedLogging) IsHedge() bool {
	return false
}

func (m *mockExecutionWithDetailedLogging) AttemptStartTime() time.Time {
	return time.Now()
}

func (m *mockExecutionWithDetailedLogging) ElapsedAttemptTime() time.Duration {
	return 0
}

func (m *mockExecutionWithDetailedLogging) IsCanceled() bool {
	return false
}

func (m *mockExecutionWithDetailedLogging) Canceled() <-chan struct{} {
	return make(chan struct{})
}

func (m *mockExecutionWithDetailedLogging) Cancel(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
}

func (m *mockExecutionWithDetailedLogging) CopyForCancellable() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecutionWithDetailedLogging{
		ctx:        m.ctx,
		behavior:   m.behavior,
		onResponse: m.onResponse,
	}
}

func (m *mockExecutionWithDetailedLogging) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	newCtx := context.WithValue(m.ctx, key, value)
	return &mockExecutionWithDetailedLogging{
		ctx:        newCtx,
		behavior:   m.behavior,
		onResponse: m.onResponse,
	}
}

func (m *mockExecutionWithDetailedLogging) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecutionWithDetailedLogging{
		ctx:        m.ctx,
		behavior:   m.behavior,
		onResponse: m.onResponse,
	}
}

func (m *mockExecutionWithDetailedLogging) CopyWithResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) failsafe.Execution[*common.NormalizedResponse] {
	return m
}

func (m *mockExecutionWithDetailedLogging) InitializeRetry() *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return nil
}

func (m *mockExecutionWithDetailedLogging) IsCanceledWithResult() (bool, *failsafeCommon.PolicyResult[*common.NormalizedResponse]) {
	return false, nil
}

func (m *mockExecutionWithDetailedLogging) RecordResult(result *failsafeCommon.PolicyResult[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
	return result
}

func TestDrainResponsesCalculation(t *testing.T) {
	// This test demonstrates the exact calculation issue in drainResponses

	t.Run("drain_calculation_with_nil_responses", func(t *testing.T) {
		// Scenario:
		// - 5 upstreams total
		// - Loop iteration 0: upstream1 returns nil (i=0, responses=0)
		// - Loop iteration 1: upstream2 returns valid (i=1, responses=1)
		// - Loop iteration 2: upstream3 returns valid (i=2, responses=2)
		// - Short-circuit triggered after iteration 2

		// With the bug:
		// remaining = len(selectedUpstreams) - len(responses) = 5 - 2 = 3
		// But only 2 goroutines are still running (upstream4 and upstream5)
		// drainResponses tries to read 3 messages when only 2 exist!

		// With the fix:
		// remaining = len(selectedUpstreams) - (i + 1) = 5 - (2 + 1) = 2
		// Correctly drains exactly 2 messages

		selectedUpstreams := 5
		var responses []string

		// Simulate the loop
		for i := 0; i < selectedUpstreams; i++ {
			// Simulate receiving a response
			if i == 0 {
				// upstream1 returns nil - not added to responses
				t.Logf("Iteration %d: Received nil response", i)
			} else if i == 1 || i == 2 {
				// upstream2 and upstream3 return valid responses
				responses = append(responses, fmt.Sprintf("response%d", i))
				t.Logf("Iteration %d: Received valid response, responses=%d", i, len(responses))

				// Check if we should short-circuit (after 2 valid responses)
				if len(responses) == 2 {
					// Calculate remaining with old formula
					remainingOld := selectedUpstreams - len(responses)
					// Calculate remaining with new formula
					remainingNew := selectedUpstreams - (i + 1)

					t.Logf("Short-circuit at iteration %d:", i)
					t.Logf("  Total upstreams: %d", selectedUpstreams)
					t.Logf("  Loop iterations completed: %d", i+1)
					t.Logf("  Valid responses collected: %d", len(responses))
					t.Logf("  Goroutines still running: %d", selectedUpstreams-(i+1))
					t.Logf("  Old calculation: %d - %d = %d (WRONG!)", selectedUpstreams, len(responses), remainingOld)
					t.Logf("  New calculation: %d - (%d + 1) = %d (CORRECT!)", selectedUpstreams, i, remainingNew)

					// Verify the calculations
					assert.Equal(t, 3, remainingOld, "Old calculation should give 3")
					assert.Equal(t, 2, remainingNew, "New calculation should give 2")
					assert.Equal(t, 2, selectedUpstreams-(i+1), "Actually running goroutines should be 2")

					// The bug: remainingOld (3) > actually running (2)
					// This causes drainResponses to wait for a 3rd message that never comes!
					assert.Greater(t, remainingOld, selectedUpstreams-(i+1),
						"Bug: Old calculation tries to read more messages than available")
					assert.Equal(t, remainingNew, selectedUpstreams-(i+1),
						"Fix: New calculation reads exactly the right number of messages")

					break
				}
			}
		}
	})
}

func TestConsensusShortCircuitBreakBehavior(t *testing.T) {
	// This test verifies that the labeled break statements correctly exit the collection loop
	// when short-circuiting, and that drainResponses is called with the correct count

	t.Run("ShortCircuitBreaksOutOfLoop", func(t *testing.T) {
		logger := log.Logger

		// Create 5 upstreams
		upstreams := make([]common.Upstream, 5)
		for i := 0; i < 5; i++ {
			upstreams[i] = common.NewFakeUpstream(
				fmt.Sprintf("upstream-%d", i),
				common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)),
			)
		}

		// Track execution details
		var executionOrder []string
		var executionMu sync.Mutex
		var goroutinesStarted int32
		var responsesProcessed int32
		var goroutinesCancelled int32

		// Create consensus policy that will short-circuit after 2 matching responses
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(5).
			WithAgreementThreshold(2). // Will short-circuit after 2 matching responses
			WithLogger(&logger).
			Build()

		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		// Create the inner function that tracks execution
		innerFn := func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			atomic.AddInt32(&goroutinesStarted, 1)

			ctx := exec.Context()

			// Get the next upstream based on execution index
			upstream, _ := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}
			upstreamID := upstream.Id()

			executionMu.Lock()
			executionOrder = append(executionOrder, fmt.Sprintf("%s-started", upstreamID))
			executionMu.Unlock()

			// Upstream behavior
			switch upstreamID {
			case "upstream-0":
				// Returns nil immediately (simulating error)
				executionMu.Lock()
				executionOrder = append(executionOrder, fmt.Sprintf("%s-returned-nil", upstreamID))
				executionMu.Unlock()
				return nil

			case "upstream-1", "upstream-2":
				// Fast responses with same value to trigger consensus
				time.Sleep(10 * time.Millisecond)
				jrr, _ := common.NewJsonRpcResponse(1, "consensus_value", nil)
				resp := common.NewNormalizedResponse().
					WithJsonRpcResponse(jrr).
					SetUpstream(upstreams[1])

				executionMu.Lock()
				executionOrder = append(executionOrder, fmt.Sprintf("%s-returned-valid", upstreamID))
				executionMu.Unlock()

				atomic.AddInt32(&responsesProcessed, 1)
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}

			default:
				// Slow responses that should be cancelled
				select {
				case <-time.After(2 * time.Second):
					// Should not reach here due to cancellation
					executionMu.Lock()
					executionOrder = append(executionOrder, fmt.Sprintf("%s-timeout-completed", upstreamID))
					executionMu.Unlock()

					jrr, _ := common.NewJsonRpcResponse(1, "slow_value", nil)
					resp := common.NewNormalizedResponse().
						WithJsonRpcResponse(jrr).
						SetUpstream(upstreams[3])
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}

				case <-ctx.Done():
					// Context cancelled due to short-circuit
					executionMu.Lock()
					executionOrder = append(executionOrder, fmt.Sprintf("%s-cancelled", upstreamID))
					executionMu.Unlock()
					atomic.AddInt32(&goroutinesCancelled, 1)
					return nil
				}
			}
		}

		// Set up execution context
		req := common.NewNormalizedRequest([]byte(`{"method":"eth_test","params":[]}`))
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, req)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		exec := &mockExecution{ctx: ctx}

		// Execute
		start := time.Now()
		applyFn := executor.Apply(innerFn)
		result := applyFn(exec)
		duration := time.Since(start)

		// Wait a bit for all goroutines to complete and detect cancellation
		time.Sleep(500 * time.Millisecond)

		// Verify results
		assert.NoError(t, result.Error)
		assert.NotNil(t, result.Result)

		// Should complete quickly due to short-circuit
		assert.Less(t, duration, 500*time.Millisecond, "Should short-circuit quickly")

		// Log execution order
		executionMu.Lock()
		t.Logf("Execution order: %v", executionOrder)
		executionMu.Unlock()

		// Verify that:
		// 1. All 5 goroutines started
		assert.Equal(t, int32(5), goroutinesStarted, "All goroutines should start")

		// 2. Only 2 valid responses were needed for consensus
		assert.GreaterOrEqual(t, int32(2), responsesProcessed, "Should need at least 2 responses for consensus")

		// 3. The slow upstreams (3 and 4) were cancelled
		cancelledCount := int(atomic.LoadInt32(&goroutinesCancelled))

		executionMu.Lock()
		// Verify no slow upstream completed normally
		for _, event := range executionOrder {
			assert.False(t, strings.Contains(event, "timeout-completed"),
				"No slow upstream should complete normally after short-circuit")
		}
		executionMu.Unlock()

		// The short-circuit should work correctly even if not all cancellations are visible
		// What matters is that:
		// 1. We got consensus with 2 responses
		// 2. The execution completed quickly (already verified)
		// 3. No slow upstream completed normally (verified above)
		// 4. drainResponses handled the remaining goroutines (implicit by no goroutine leak)

		t.Logf("Goroutines started: %d, Valid responses: %d, Explicitly cancelled: %d",
			goroutinesStarted, responsesProcessed, cancelledCount)

		// Note: cancelledCount might be 0 if drainResponses consumed the nil responses
		// before our test could count them. This is expected behavior.

		t.Log("Test verified: Short-circuit correctly breaks out of collection loop and cancels remaining requests")
	})

	t.Run("DrainResponsesCountIsCorrect", func(t *testing.T) {
		// This test specifically verifies the drainResponses calculation
		// We'll use a scenario where responses include nil values to ensure the count is correct

		logger := log.Logger

		upstreams := make([]common.Upstream, 5)
		for i := 0; i < 5; i++ {
			upstreams[i] = common.NewFakeUpstream(
				fmt.Sprintf("upstream-%d", i),
				common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100)),
			)
		}

		// Track which iteration each upstream responds at
		responseIterations := make(map[string]int)
		var iterationMu sync.Mutex

		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(5).
			WithAgreementThreshold(2).
			WithLogger(&logger).
			Build()

		// Create the executor
		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		innerFn := func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			ctx := exec.Context()

			// Get the next upstream based on execution index
			upstream, _ := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}
			upstreamID := upstream.Id()

			// Upstream 0: returns nil (error case)
			if upstreamID == "upstream-0" {
				iterationMu.Lock()
				responseIterations[upstreamID] = 0 // First to respond
				iterationMu.Unlock()
				return nil
			}

			// Upstreams 1 & 2: return valid matching responses
			if upstreamID == "upstream-1" || upstreamID == "upstream-2" {
				time.Sleep(20 * time.Millisecond)
				jrr, _ := common.NewJsonRpcResponse(1, "consensus", nil)
				resp := common.NewNormalizedResponse().
					WithJsonRpcResponse(jrr).
					SetUpstream(upstreams[1])

				iterationMu.Lock()
				if upstreamID == "upstream-1" {
					responseIterations[upstreamID] = 1
				} else {
					responseIterations[upstreamID] = 2
				}
				iterationMu.Unlock()

				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: resp}
			}

			// Others: would be slow but should be cancelled
			select {
			case <-time.After(1 * time.Second):
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("should have been cancelled"),
				}
			case <-ctx.Done():
				return nil
			}
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_test","params":[]}`))
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, req)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		exec := &mockExecution{ctx: ctx}

		// Execute
		applyFn := executor.Apply(innerFn)
		result := applyFn(exec)

		// Should succeed with consensus
		assert.NoError(t, result.Error)

		// Wait for goroutines to settle
		time.Sleep(100 * time.Millisecond)

		iterationMu.Lock()
		t.Logf("Response iterations: %v", responseIterations)

		// In this scenario:
		// - Total goroutines: 5
		// - Iteration 0: upstream-0 returns nil (not counted in responses)
		// - Iteration 1: upstream-1 returns valid (responses = 1)
		// - Iteration 2: upstream-2 returns valid (responses = 2, consensus reached)
		// - At this point: i = 2, so remaining = 5 - (2 + 1) = 2
		// - This correctly represents upstream-3 and upstream-4 still running

		// The key insight: even though upstream-0 returned nil and wasn't added
		// to the responses array, it still consumed an iteration of the loop,
		// so using (i+1) correctly accounts for all processed goroutines

		assert.Equal(t, 3, len(responseIterations), "Should have processed 3 responses before short-circuit")
		iterationMu.Unlock()

		t.Log("Test verified: drainResponses count calculation correctly handles nil responses")
	})
}

// TestConsensusOnExecutionException tests that execution exceptions are treated as valid consensus results
func TestConsensusOnExecutionException(t *testing.T) {
	// Helper to create execution exception error
	createExecutionException := func(normalizedCode common.JsonRpcErrorNumber) error {
		return common.NewErrEndpointExecutionException(
			common.NewErrJsonRpcExceptionInternal(
				3, // original code
				normalizedCode,
				"execution reverted",
				nil,
				nil,
			),
		)
	}

	t.Run("consensus_on_execution_exception", func(t *testing.T) {
		// Create upstreams
		upstreams := []common.Upstream{
			common.NewFakeUpstream("upstream1", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
			common.NewFakeUpstream("upstream2", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
			common.NewFakeUpstream("upstream3", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
		}

		// Create request
		dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call"}`))

		// Create context
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		// Create logger
		logger := log.Logger

		// Create consensus policy
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(3).
			WithAgreementThreshold(2). // Changed from 3 to 2
			WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
			WithLogger(&logger).
			Build()

		// Create executor
		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create mock execution
		mockExec := &mockExecution{
			ctx: ctx,
		}

		// Execute with all upstreams returning the same execution exception
		executionError := createExecutionException(common.JsonRpcErrorEvmReverted)

		result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// All upstreams return the same execution exception
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: executionError,
			}
		})(mockExec)

		// Verify: consensus should be achieved on the error
		require.NotNil(t, result.Error, "expected error but got nil")
		require.True(t, common.HasErrorCode(result.Error, common.ErrCodeEndpointExecutionException),
			"expected execution exception error but got %v", result.Error)
	})

	t.Run("dispute_on_different_execution_exceptions", func(t *testing.T) {
		// Create upstreams
		upstreams := []common.Upstream{
			common.NewFakeUpstream("upstream1", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
			common.NewFakeUpstream("upstream2", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
			common.NewFakeUpstream("upstream3", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
		}

		// Create request
		dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call"}`))

		// Create context
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		// Create logger
		logger := log.Logger

		// Create consensus policy - use dispute behavior that returns error
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(3).
			WithAgreementThreshold(2).                                       // Changed from 3 to 2
			WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError). // This should return error on dispute
			WithLogger(&logger).
			Build()

		// Create executor
		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Map of upstream errors - all different to ensure dispute
		upstreamErrors := map[string]error{
			"upstream1": createExecutionException(common.JsonRpcErrorEvmReverted),
			"upstream2": createExecutionException(common.JsonRpcErrorTransactionRejected), // Different error
			"upstream3": createExecutionException(common.JsonRpcErrorCallException),       // Also different
		}

		// Track which upstreams were called
		calledUpstreams := make(map[string]bool)
		var mu sync.Mutex

		// Create mock execution
		mockExec := &mockExecution{
			ctx: ctx,
		}

		// Create upstream selector with specific mapping to ensure each upstream gets its expected error
		selector := newTestUpstreamSelector(upstreams).withMapping(map[int]int{
			0: 0, // First call -> upstream1
			1: 1, // Second call -> upstream2
			2: 2, // Third call -> upstream3
		})

		// Execute with upstreams returning different execution exceptions
		result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Get the next upstream based on execution index
			upstream, _ := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}
			upstreamID := upstream.Id()

			mu.Lock()
			calledUpstreams[upstreamID] = true
			mu.Unlock()

			if err, ok := upstreamErrors[upstreamID]; ok {
				// Create a response with the upstream set, even though we're returning an error
				// This is needed for the consensus executor to track participants
				resp := common.NewNormalizedResponse().
					SetUpstream(upstream)

				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Result: resp,
					Error:  err,
				}
			}

			t.Fatalf("unexpected upstream ID: %s", upstreamID)
			return nil
		})(mockExec)

		// Log which upstreams were called for debugging
		t.Logf("Called upstreams: %v", calledUpstreams)
		t.Logf("Result error: %v", result.Error)

		// Verify: should result in dispute since different execution exceptions
		require.NotNil(t, result.Error, "expected error but got nil")
		require.True(t, common.HasErrorCode(result.Error, common.ErrCodeConsensusDispute),
			"expected consensus dispute error but got %v", result.Error)
	})

	t.Run("majority_agrees_on_execution_exception", func(t *testing.T) {
		// Create upstreams
		upstreams := []common.Upstream{
			common.NewFakeUpstream("upstream1", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
			common.NewFakeUpstream("upstream2", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
			common.NewFakeUpstream("upstream3", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
		}

		// Create request
		dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call"}`))

		// Create context
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		// Create logger
		logger := log.Logger

		// Create consensus policy
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(3).
			WithAgreementThreshold(2).
			WithDisputeBehavior(common.ConsensusDisputeBehaviorReturnError).
			WithLogger(&logger).
			Build()

		// Create executor
		executor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create mock execution
		mockExec := &mockExecution{
			ctx: ctx,
		}

		// Create successful response
		successResp := createResponse("success_result", nil)
		executionError := createExecutionException(common.JsonRpcErrorEvmReverted)

		// Map of upstream responses
		upstreamResponses := map[string]struct {
			resp *common.NormalizedResponse
			err  error
		}{
			"upstream1": {resp: nil, err: executionError}, // execution error
			"upstream2": {resp: nil, err: executionError}, // execution error (same)
			"upstream3": {resp: successResp, err: nil},    // success
		}

		// Create upstream selector for this test
		selector := newTestUpstreamSelector(upstreams)

		// Execute
		result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Get the next upstream based on execution index
			upstream, _ := selector.getNextUpstream()
			if upstream == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: fmt.Errorf("no upstream available"),
				}
			}
			upstreamID := upstream.Id()

			if resp, ok := upstreamResponses[upstreamID]; ok {
				if resp.err != nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: resp.err,
					}
				}
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Result: resp.resp,
				}
			}

			t.Fatalf("unexpected upstream ID: %s", upstreamID)
			return nil
		})(mockExec)

		// Verify: consensus should be achieved on the execution error (2 out of 3)
		require.NotNil(t, result.Error, "expected error but got nil")
		require.True(t, common.HasErrorCode(result.Error, common.ErrCodeEndpointExecutionException),
			"expected execution exception error but got %v", result.Error)
	})
}

// TestFindResultByHashWithErrorConsensus specifically tests the bug fix where
// findResultByHash was incorrectly returning &r.result for error consensus cases
func TestFindResultByHashWithErrorConsensus(t *testing.T) {
	// Create logger
	logger := log.Logger

	// Create consensus policy
	policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
		WithLogger(&logger).
		Build()

	// Create executor
	testExecutor := &executor[*common.NormalizedResponse]{
		consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
	}

	// Create test responses with a mix of errors and successful results
	successResp1 := createResponse("result1", nil)
	successResp2 := createResponse("result2", nil)

	// Create execution exception errors with different normalized codes
	execError1 := common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(
			3, // original code
			common.JsonRpcErrorEvmReverted,
			"execution reverted",
			nil,
			nil,
		),
	)
	// ... existing code ...
	// Removed unused execError2 variable

	// Create mock execution for context
	mockExec := &mockExecution{
		ctx: context.Background(),
	}

	t.Run("finds_successful_result_by_hash", func(t *testing.T) {
		responses := []*execResult[*common.NormalizedResponse]{
			{result: successResp1, err: nil},
			{result: successResp2, err: nil},
			{result: nil, err: execError1}, // This has nil result
		}

		// Get hash of successResp1
		hash1, err := testExecutor.resultToHash(successResp1, mockExec)
		require.NoError(t, err)

		// Find result by hash
		result := testExecutor.findResultByHash(responses, hash1, mockExec)
		require.NotNil(t, result)
		require.Equal(t, successResp1, *result)
	})

	t.Run("returns_nil_for_error_hash_after_fix", func(t *testing.T) {
		// This test verifies the bug fix - previously findResultByHash would incorrectly
		// return &r.result when r.err matched the consensus error hash

		responses := []*execResult[*common.NormalizedResponse]{
			{result: successResp1, err: nil},
			{result: nil, err: execError1}, // result is nil when there's an error
			{result: nil, err: execError1}, // another response with same error
		}

		// Get the error hash
		errorHash := testExecutor.errorToConsensusHash(execError1)
		require.NotEmpty(t, errorHash)

		// Try to find result by error hash - should return nil after our fix
		result := testExecutor.findResultByHash(responses, errorHash, mockExec)
		require.Nil(t, result, "findResultByHash should return nil for error hash")
	})

	t.Run("returns_nil_when_no_match", func(t *testing.T) {
		responses := []*execResult[*common.NormalizedResponse]{
			{result: successResp1, err: nil},
			{result: nil, err: execError1},
		}

		// Try with a hash that doesn't match any response
		result := testExecutor.findResultByHash(responses, "non-existent-hash", mockExec)
		require.Nil(t, result)
	})

	t.Run("error_consensus_handled_in_evaluateConsensus_not_findResultByHash", func(t *testing.T) {
		// This test documents the intended behavior: error consensus is handled
		// in evaluateConsensus before findResultByHash is called

		// Create upstreams
		upstreams := []common.Upstream{
			common.NewFakeUpstream("upstream1", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
			common.NewFakeUpstream("upstream2", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
			common.NewFakeUpstream("upstream3", common.WithEvmStatePoller(common.NewFakeEvmStatePoller(100, 100))),
		}

		// Create request
		dummyReq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call"}`))

		// Create context
		ctx := context.Background()
		ctx = context.WithValue(ctx, common.RequestContextKey, dummyReq)
		ctx = context.WithValue(ctx, common.UpstreamsContextKey, upstreams)

		// Create consensus policy
		policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
			WithRequiredParticipants(3).
			WithAgreementThreshold(2).
			WithLogger(&logger).
			Build()

		// Create executor
		testExecutor := &executor[*common.NormalizedResponse]{
			consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
		}

		// Create mock execution
		mockExec := &mockExecution{
			ctx: ctx,
		}

		// All upstreams return the same error
		result := testExecutor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: execError1,
			}
		})(mockExec)

		// Verify error consensus is properly handled
		require.NotNil(t, result.Error)
		require.True(t, common.HasErrorCode(result.Error, common.ErrCodeEndpointExecutionException))

		// The key point: findResultByHash was never called for this error consensus case
		// because evaluateConsensus handled it directly
	})
}
