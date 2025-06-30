package consensus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/failsafe-go/failsafe-go/policy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ failsafe.Execution[*common.NormalizedResponse] = &mockExecution{}
var _ policy.ExecutionInternal[*common.NormalizedResponse] = &mockExecution{}

func TestConsensusExecutor(t *testing.T) {
	t.Parallel()
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
			name:                 "dispute_with_return_error_misbehaving_upstreams_are_punished_if_there_is_a_clear_majority",
			requiredParticipants: 5,
			agreementThreshold:   4,
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
				{"result4", "upstream4", 1},
				{"result5", "upstream5", 1},
			},
			expectedError:  pointer("ErrConsensusDispute: not enough agreement among responses"),
			expectedResult: nil,
			expectedPunishedUpsteams: []string{
				"upstream4",
				"upstream5",
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
			name:                 "dispute_with_prefer_block_head_leader",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorPreferBlockHeadLeader),
			disputeThreshold:     1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result2", "upstream2", 1},
				{"result3", "upstream3", 3},
			},
			expectedError:  nil,
			expectedResult: []string{"result3"},
		},
		{
			name:                 "dispute_with_only_block_head_leader",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorOnlyBlockHeadLeader),
			disputeThreshold:     1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result2", "upstream2", 1},
				{"result3", "upstream3", 3},
			},
			expectedError:  nil,
			expectedResult: []string{"result3"},
		},
		{
			name:                 "dispute_with_only_block_head_leader",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorOnlyBlockHeadLeader),
			disputeThreshold:     1,
			responses: []*struct {
				response                  string
				upstreamId                string
				upstreamLatestBlockNumber int64
			}{
				{"result1", "upstream1", 1},
				{"result2", "upstream2", 1},
				{"result3", "upstream3", 3},
			},
			expectedError:  nil,
			expectedResult: []string{"result3"},
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
			t.Parallel()

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

			log := zerolog.New(zerolog.NewTestWriter(t))

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

				// Execute consensus – this mimics the actual execution of the policy as defined in networks.go
				result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
					ctx := exec.Context()
					if ctx == nil {
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: errors.New("missing execution context")}
					}

					// Retrieve the cloned request to discover which upstream was selected for this execution.
					req, _ := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
					upstreamID := ""
					if req != nil && req.Directives() != nil {
						upstreamID = req.Directives().UseUpstream
					}

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

			for _, response := range responses {
				fake, ok := response.Upstream().(*common.FakeUpstream)
				require.True(t, ok)

				cordonedReason, cordoned := fake.CordonedReason()

				if _, shouldBePunished := expectedPunishedUpstreams[fake.Config().Id]; shouldBePunished {
					punishedUpstreams[fake.Config().Id] = fake
					assert.True(t, cordoned, "expected upstream %s to be cordoned", fake.Config().Id)
					assert.Equal(t, "misbehaving in consensus", cordonedReason, "expected upstream %s to be cordoned for misbehaving in consensus", fake.Config().Id)
				} else {
					assert.False(t, cordoned, "expected upstream %s to not be cordoned", fake.Config().Id)
					assert.Equal(t, "", cordonedReason, "expected upstream %s to not be cordoned", fake.Config().Id)
				}
			}

			// Verify that all expected punished upstreams were actually punished
			require.Equal(t, len(tt.expectedPunishedUpsteams), len(punishedUpstreams), "expected %d punished upstreams, got %d", len(tt.expectedPunishedUpsteams), len(punishedUpstreams))
			require.Equal(t, (len(tt.responses) - len(tt.expectedPunishedUpsteams)), len(tt.responses)-len(punishedUpstreams), "expected %d unpunished upstreams, got %d", len(tt.expectedPunishedUpsteams), len(punishedUpstreams))

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

// TestSelectUpstreams tests the selectUpstreams method with various configurations,
// specifically focusing on OnlyBlockHeadLeader and PreferBlockHeadLeader behaviors
func TestSelectUpstreams(t *testing.T) {
	tests := []struct {
		name                    string
		requiredParticipants    int
		lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
		upstreams               []struct {
			id          string
			latestBlock int64
			hasPoller   bool
		}
		expectedSelected []string // IDs of expected selected upstreams in order
	}{
		// OnlyBlockHeadLeader test cases
		{
			name:                    "OnlyBlockHeadLeader_leader_not_in_selected_set",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 100, true},
				{"upstream2", 200, true},
				{"upstream3", 300, true}, // This is the leader
			},
			expectedSelected: []string{"upstream3"}, // Only the leader should be selected
		},
		{
			name:                    "OnlyBlockHeadLeader_leader_already_in_selected_set",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 300, true}, // This is the leader
				{"upstream2", 200, true},
				{"upstream3", 100, true},
			},
			expectedSelected: []string{"upstream1", "upstream2"}, // Leader already in selected set, no change
		},
		{
			name:                    "OnlyBlockHeadLeader_no_leader_found",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 0, false}, // No poller
				{"upstream2", 0, false}, // No poller
				{"upstream3", 0, false}, // No poller
			},
			expectedSelected: []string{"upstream1", "upstream2"}, // Normal selection when no leader
		},
		{
			name:                    "OnlyBlockHeadLeader_empty_upstream_list",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{},
			expectedSelected: []string{}, // Empty result for empty input
		},
		// PreferBlockHeadLeader test cases
		{
			name:                    "PreferBlockHeadLeader_leader_not_in_selected_swap_last",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 100, true},
				{"upstream2", 200, true},
				{"upstream3", 300, true}, // This is the leader
			},
			expectedSelected: []string{"upstream1", "upstream3"}, // Leader swaps with last position
		},
		{
			name:                    "PreferBlockHeadLeader_leader_already_in_selected_no_change",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 300, true}, // This is the leader
				{"upstream2", 200, true},
				{"upstream3", 100, true},
			},
			expectedSelected: []string{"upstream1", "upstream2"}, // No change as leader is already selected
		},
		{
			name:                    "PreferBlockHeadLeader_single_upstream_selected_leader_not_selected",
			requiredParticipants:    1,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 100, true},
				{"upstream2", 300, true}, // This is the leader
			},
			expectedSelected: []string{"upstream2"}, // Leader replaces the single selected upstream
		},
		{
			name:                    "PreferBlockHeadLeader_empty_selected_set_add_leader",
			requiredParticipants:    0, // Will default to 2, but more upstreams than required
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 300, true}, // This is the leader
			},
			expectedSelected: []string{"upstream1"}, // Leader is added to empty set
		},
		{
			name:                    "PreferBlockHeadLeader_no_evm_upstreams",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 0, false}, // Not EVM upstream (no poller)
				{"upstream2", 0, false}, // Not EVM upstream (no poller)
			},
			expectedSelected: []string{"upstream1", "upstream2"}, // Normal selection when no leader found
		},
		// Edge cases
		{
			name:                    "Normal_behavior_no_leader_logic",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 100, true},
				{"upstream2", 200, true},
				{"upstream3", 300, true},
			},
			expectedSelected: []string{"upstream1", "upstream2"}, // Normal selection, first N upstreams
		},
		{
			name:                    "OnlyBlockHeadLeader_multiple_leaders_same_block",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 100, true},
				{"upstream2", 300, true}, // Tied for leader
				{"upstream3", 300, true}, // Tied for leader
			},
			expectedSelected: []string{"upstream1", "upstream2"}, // Leader already in selected set (upstream2), no change
		},
		{
			name:                    "PreferBlockHeadLeader_all_upstreams_required",
			requiredParticipants:    3,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", 100, true},
				{"upstream2", 200, true},
				{"upstream3", 300, true}, // This is the leader
			},
			expectedSelected: []string{"upstream1", "upstream2", "upstream3"}, // All selected, leader already included
		},
		{
			name:                    "OnlyBlockHeadLeader_negative_block_numbers",
			requiredParticipants:    2,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
			}{
				{"upstream1", -1, true},  // Invalid block
				{"upstream2", 0, true},   // Invalid block
				{"upstream3", 100, true}, // Valid leader
			},
			expectedSelected: []string{"upstream3"}, // Only valid leader selected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create the upstream list
			var upsList []common.Upstream
			for _, upCfg := range tt.upstreams {
				var upstream common.Upstream
				if upCfg.hasPoller {
					upstream = common.NewFakeUpstream(
						upCfg.id,
						common.WithEvmStatePoller(
							common.NewFakeEvmStatePoller(upCfg.latestBlock, upCfg.latestBlock),
						),
					)
				} else {
					upstream = common.NewFakeUpstream(upCfg.id)
				}
				upsList = append(upsList, upstream)
			}

			// Create executor with the test configuration
			log := zerolog.New(zerolog.NewTestWriter(t))
			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: &consensusPolicy[*common.NormalizedResponse]{
					config: &config[*common.NormalizedResponse]{
						requiredParticipants:    tt.requiredParticipants,
						lowParticipantsBehavior: tt.lowParticipantsBehavior,
						logger:                  &log,
					},
					logger: &log,
				},
			}

			// Call selectUpstreams
			selected := executor.selectUpstreams(upsList)

			// Verify the selected upstreams match expectations
			require.Len(t, selected, len(tt.expectedSelected), "Wrong number of selected upstreams")

			for i, expectedId := range tt.expectedSelected {
				assert.Equal(t, expectedId, selected[i].Id(),
					"Mismatch at position %d: expected %s, got %s",
					i, expectedId, selected[i].Id())
			}
		})
	}
}

// TestFindBlockHeadLeaderUpstream tests the findBlockHeadLeaderUpstream function
func TestFindBlockHeadLeaderUpstream(t *testing.T) {
	tests := []struct {
		name      string
		upstreams []struct {
			id          string
			latestBlock int64
			hasPoller   bool
			isEvm       bool
		}
		expectedLeaderId string
	}{
		{
			name: "single_leader",
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
				isEvm       bool
			}{
				{"upstream1", 100, true, true},
				{"upstream2", 200, true, true},
				{"upstream3", 150, true, true},
			},
			expectedLeaderId: "upstream2",
		},
		{
			name: "multiple_tied_leaders",
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
				isEvm       bool
			}{
				{"upstream1", 200, true, true},
				{"upstream2", 200, true, true},
				{"upstream3", 100, true, true},
			},
			expectedLeaderId: "upstream1", // First one wins
		},
		{
			name: "no_evm_upstreams",
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
				isEvm       bool
			}{
				{"upstream1", 100, false, false},
				{"upstream2", 200, false, false},
			},
			expectedLeaderId: "", // No leader
		},
		{
			name: "mixed_upstream_types",
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
				isEvm       bool
			}{
				{"upstream1", 100, false, false}, // Not EVM
				{"upstream2", 50, true, true},    // EVM with lower block
				{"upstream3", 150, true, true},   // EVM with higher block
			},
			expectedLeaderId: "upstream3",
		},
		{
			name: "upstream_without_poller",
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
				isEvm       bool
			}{
				{"upstream1", 100, false, true}, // EVM but no poller
				{"upstream2", 50, true, true},   // EVM with poller
			},
			expectedLeaderId: "upstream2",
		},
		{
			name: "empty_list",
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
				isEvm       bool
			}{},
			expectedLeaderId: "",
		},
		{
			name: "negative_and_zero_blocks",
			upstreams: []struct {
				id          string
				latestBlock int64
				hasPoller   bool
				isEvm       bool
			}{
				{"upstream1", -100, true, true},
				{"upstream2", 0, true, true},
				{"upstream3", 50, true, true},
			},
			expectedLeaderId: "upstream3", // Only positive block counts
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create the upstream list
			var upsList []common.Upstream
			for _, upCfg := range tt.upstreams {
				var upstream common.Upstream
				if upCfg.isEvm && upCfg.hasPoller {
					upstream = common.NewFakeUpstream(
						upCfg.id,
						common.WithEvmStatePoller(
							common.NewFakeEvmStatePoller(upCfg.latestBlock, upCfg.latestBlock),
						),
					)
				} else {
					upstream = common.NewFakeUpstream(upCfg.id)
				}
				upsList = append(upsList, upstream)
			}

			// Call findBlockHeadLeaderUpstream
			leader := findBlockHeadLeaderUpstream(upsList)

			// Verify the result
			if tt.expectedLeaderId == "" {
				assert.Nil(t, leader, "Expected no leader, but got %v", leader)
			} else {
				require.NotNil(t, leader, "Expected leader %s, but got nil", tt.expectedLeaderId)
				assert.Equal(t, tt.expectedLeaderId, leader.Id(),
					"Expected leader %s, but got %s", tt.expectedLeaderId, leader.Id())
			}
		})
	}
}
