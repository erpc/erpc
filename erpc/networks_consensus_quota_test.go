package erpc

import (
	"testing"

	"github.com/erpc/erpc/common"
)

// TestConsensusPolicy_RequiredParticipants drives a real request through the
// full network → consensus path and asserts that `requiredParticipants`
// changes which upstreams land in the consensus participant set.
//
// Setup: 3 upstreams, maxParticipants=2. By default consensus draws the first
// two in selection order (test-up-1, test-up-2), leaving test-up-3 out. We tag
// test-up-3 `region:backup` and require ≥1 participant carrying that tag — the
// quota must pull test-up-3 into the set, displacing test-up-2. All upstreams
// return the same result so whichever two participate reach agreement.
func TestConsensusPolicy_RequiredParticipants(t *testing.T) {
	sameResult := func() []mockResponse {
		return []mockResponse{
			{status: 200, body: jsonRpcSuccess("0xok")},
			{status: 200, body: jsonRpcSuccess("0xok")},
			{status: 200, body: jsonRpcSuccess("0xok")},
		}
	}
	taggedUpstreams := func() []*common.UpstreamConfig {
		ups := createTestUpstreams(3)
		ups[2].Tags = []string{"region:backup"} // only test-up-3 carries it
		return ups
	}

	t.Run("baseline: third upstream is left out without a quota", func(t *testing.T) {
		runConsensusTest(t, consensusTestCase{
			name:      "required_participants_baseline",
			upstreams: taggedUpstreams(),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    2,
				AgreementThreshold: 2,
			},
			mockResponses:  sameResult(),
			expectedCalls:  []int{1, 1, 0}, // up-1 + up-2 participate; tagged up-3 excluded
			expectedResult: &expectedResult{jsonRpcResult: `"0xok"`},
		})
	})

	t.Run("quota forces the tagged upstream into the participant set", func(t *testing.T) {
		runConsensusTest(t, consensusTestCase{
			name:      "required_participants_quota",
			upstreams: taggedUpstreams(),
			consensusConfig: &common.ConsensusPolicyConfig{
				MaxParticipants:    2,
				AgreementThreshold: 2,
				RequiredParticipants: []*common.ConsensusRequiredParticipant{
					{Tag: "region:backup", MinParticipants: 1},
				},
			},
			mockResponses:  sameResult(),
			expectedCalls:  []int{1, 0, 1}, // up-3 promoted in; up-2 displaced out
			expectedResult: &expectedResult{jsonRpcResult: `"0xok"`},
		})
	})
}
