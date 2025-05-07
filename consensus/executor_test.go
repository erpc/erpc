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
		responses               []*common.NormalizedResponse
		expectedError           *string
		expectedResult          []*common.NormalizedResponse
	}{
		{
			name:                 "successful consensus",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorReturnError),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result1", "upstream2", 1),
				createResponse("result2", "upstream3", 1),
			},
			expectedError:  nil,
			expectedResult: []*common.NormalizedResponse{createResponse("result1", "upstream1", 1)},
		},
		{
			name:                 "dispute with return error",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorReturnError),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result2", "upstream2", 1),
				createResponse("result3", "upstream3", 1),
			},
			expectedError:  pointer("ErrConsensusDispute: not enough agreement among responses"),
			expectedResult: nil,
		},
		{
			name:                 "dispute with accept any valid",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorAcceptAnyValidResult),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result2", "upstream2", 1),
				createResponse("result3", "upstream3", 1),
			},
			expectedError: nil,
			expectedResult: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result2", "upstream2", 1),
				createResponse("result3", "upstream3", 1),
			},
		},
		{
			name:                 "dispute with prefer block head leader",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorPreferBlockHeadLeader),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result2", "upstream2", 1),
				createResponse("result3", "upstream3", 3),
			},
			expectedError:  nil,
			expectedResult: []*common.NormalizedResponse{createResponse("result3", "upstream3", 3)},
		},
		{
			name:                 "dispute with only block head leader",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorOnlyBlockHeadLeader),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result2", "upstream2", 1),
				createResponse("result3", "upstream3", 3),
			},
			expectedError:  nil,
			expectedResult: []*common.NormalizedResponse{createResponse("result3", "upstream3", 3)},
		},
		{
			name:                 "dispute with only block head leader",
			requiredParticipants: 3,
			agreementThreshold:   2,
			disputeBehavior:      pointer(common.ConsensusDisputeBehaviorOnlyBlockHeadLeader),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result2", "upstream2", 1),
				createResponse("result3", "upstream3", 3),
			},
			expectedError:  nil,
			expectedResult: []*common.NormalizedResponse{createResponse("result3", "upstream3", 3)},
		},
		{
			name:                    "low participants with return error",
			requiredParticipants:    3,
			agreementThreshold:      2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorReturnError),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result1", "upstream1", 1),
				createResponse("result1", "upstream1", 1),
			},
			expectedError:  pointer("ErrConsensusLowParticipants: not enough participants"),
			expectedResult: nil,
		},
		{
			name:                    "low participants with accept any valid",
			requiredParticipants:    3,
			agreementThreshold:      2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorAcceptAnyValidResult),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result1", "upstream1", 1),
				createResponse("result1", "upstream1", 1),
			},
			expectedError:  nil,
			expectedResult: []*common.NormalizedResponse{createResponse("result1", "upstream1", 1)},
		},
		{
			name:                    "low participants with prefer block head leader fallback",
			requiredParticipants:    3,
			agreementThreshold:      2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 1),
				createResponse("result1", "upstream1", 1),
				createResponse("result1", "upstream1", 1),
			},
			expectedError:  nil,
			expectedResult: []*common.NormalizedResponse{createResponse("result1", "upstream1", 1)},
		},
		{
			name:                    "low participants with prefer block head leader",
			requiredParticipants:    3,
			agreementThreshold:      2,
			lowParticipantsBehavior: pointer(common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader),
			responses: []*common.NormalizedResponse{
				createResponse("result1", "upstream1", 2),
				createResponse("result1", "upstream1", 2),
				createResponse("result2", "upstream2", 1),
			},
			expectedError:  nil,
			expectedResult: []*common.NormalizedResponse{createResponse("result1", "upstream1", 2)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Create mock execution
			mockExec := &mockExecution{
				responses: tt.responses,
			}

			disputeBehavior := common.ConsensusDisputeBehaviorReturnError
			if tt.disputeBehavior != nil {
				disputeBehavior = *tt.disputeBehavior
			}

			lowParticipantsBehavior := common.ConsensusLowParticipantsBehaviorReturnError
			if tt.lowParticipantsBehavior != nil {
				lowParticipantsBehavior = *tt.lowParticipantsBehavior
			}

			// Create consensus policy
			policy := NewConsensusPolicyBuilder[*common.NormalizedResponse]().
				WithRequiredParticipants(tt.requiredParticipants).
				WithAgreementThreshold(tt.agreementThreshold).
				WithDisputeBehavior(disputeBehavior).
				WithLowParticipantsBehavior(lowParticipantsBehavior).
				Build()

			// Create logger
			logger := zerolog.New(zerolog.NewTestWriter(t))

			// Create executor
			executor := &executor[*common.NormalizedResponse]{
				consensusPolicy: policy.(*consensusPolicy[*common.NormalizedResponse]),
				logger:          &logger,
			}

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
					Result: tt.responses[currentIndex],
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
				for _, expectedResult := range tt.expectedResult {
					jrr, err := expectedResult.JsonRpcResponse()
					require.NoError(t, err)
					require.NotNil(t, jrr)

					execptedJrrString = append(execptedJrrString, string(jrr.Result))
				}

				assert.Contains(t, execptedJrrString, string(actualJrr.Result))
			}
		})
	}
}

// Helper function to create normalized responses
func createResponse(result string, upstreamId string, upstreamLatestBlockNumber int64) *common.NormalizedResponse {
	jrr, err := common.NewJsonRpcResponse(1, result, nil)
	if err != nil {
		panic(err)
	}
	return common.NewNormalizedResponse().
		WithJsonRpcResponse(jrr).
		SetUpstream(
			common.NewFakeUpstream(upstreamId, common.WithEvmStatePoller(common.NewFakeEvmStatePoller(upstreamLatestBlockNumber, upstreamLatestBlockNumber))),
		)
}

// Mock execution that returns pre-defined responses
type mockExecution struct {
	responses []*common.NormalizedResponse
}

func (m *mockExecution) Context() context.Context {
	return context.Background()
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
