package consensus

import (
	"context"
	"errors"
	"testing"
	"time"

	"sync/atomic"

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
		failureBehavior         *common.ConsensusFailureBehavior
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
				// Execute consensus, this mimicks the actual execution of the policy as defined in networks.go
				var i atomic.Int32
				result := executor.Apply(func(exec failsafe.Execution[*common.NormalizedResponse]) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
					currentIndex := i.Add(1) - 1
					if currentIndex >= int32(len(tt.responses)) {
						return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
							Error: errors.New("no more responses"),
						}
					}

					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Result: responses[currentIndex],
					}
				})(mockExec)

				// Verify results
				if tt.expectedError != nil {
					assert.EqualError(t, result.Error, *tt.expectedError)
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
	}
}

func (m *mockExecution) CopyForCancellableWithValue(key, value any) failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{
		responses: m.responses,
	}
}

func (m *mockExecution) CopyForHedge() failsafe.Execution[*common.NormalizedResponse] {
	return &mockExecution{
		responses: m.responses,
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
