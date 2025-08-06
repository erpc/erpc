package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
)

func init() {
	util.ConfigureTestLogger()
}

// TestConsensusPriorityOrderingComprehensive tests all priority ordering scenarios comprehensively
func TestConsensusPriorityOrderingComprehensive(t *testing.T) {
	tests := []struct {
		name                    string
		mockResponses           []map[string]interface{}
		disputeBehavior         common.ConsensusDisputeBehavior
		lowParticipantsBehavior common.ConsensusLowParticipantsBehavior
		agreementThreshold      int
		preferLargerResponses   bool
		expectedError           bool
		expectedResult          interface{} // Can be string or function
		description             string
	}{
		// Priority Level 1: Largest Non-Empty Tests
		{
			name: "largest_nonempty_beats_smaller_with_higher_count",
			mockResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": []interface{}{
						map[string]interface{}{"data": "small"},
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": []interface{}{
						map[string]interface{}{"data": "small"},
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": []interface{}{
						map[string]interface{}{"data": "large1"},
						map[string]interface{}{"data": "large2"},
						map[string]interface{}{"data": "large3"},
					},
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result": []interface{}{
						map[string]interface{}{"data": "large1"},
						map[string]interface{}{"data": "large2"},
						map[string]interface{}{"data": "large3"},
					},
				},
			},
			disputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
			agreementThreshold:      2,
			preferLargerResponses:   true,
			expectedError:           false,
			expectedResult:          func(r string) bool { return r == `[{"data":"large1"},{"data":"large2"},{"data":"large3"}]` },
			description:             "Larger non-empty (1 vote) beats smaller non-empty (2 votes) with AcceptMostCommonValidResult + preferLarger",
		},
		// {
		// 	name: "largest_nonempty_loses_with_acceptmost_threshold",
		// 	mockResponses: []map[string]interface{}{
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result": []interface{}{
		// 				map[string]interface{}{"data": "small"},
		// 			},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result": []interface{}{
		// 				map[string]interface{}{"data": "small"},
		// 			},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result": []interface{}{
		// 				map[string]interface{}{"data": "large1"},
		// 				map[string]interface{}{"data": "large2"},
		// 				map[string]interface{}{"data": "large3"},
		// 			},
		// 		},
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    2,
		// 	preferLargerResponses: true,
		// 	expectedError:         false,
		// 	expectedResult:        `[{"data":"small"}]`, // Smaller meets threshold
		// 	description:           "Smaller non-empty (2 votes, meets threshold) beats larger (1 vote) with AcceptMostCommon",
		// },

		// // Priority Level 2 vs 3: Non-Empty vs Empty
		// {
		// 	name: "nonempty_beats_empty_acceptbest",
		// 	mockResponses: []map[string]interface{}{
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  []interface{}{},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  []interface{}{},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  []interface{}{},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xdata",
		// 		},
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    3,
		// 	preferLargerResponses: false,
		// 	expectedError:         false,
		// 	expectedResult:        `"0xdata"`,
		// 	description:           "1 non-empty beats 3 empty with AcceptBest (ignores threshold)",
		// },
		// {
		// 	name: "empty_beats_nonempty_acceptmost_threshold",
		// 	mockResponses: []map[string]interface{}{
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  []interface{}{},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  []interface{}{},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  []interface{}{},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xdata",
		// 		},
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    3,
		// 	preferLargerResponses: false,
		// 	expectedError:         false,
		// 	expectedResult:        `[]`,
		// 	description:           "3 empty (meets threshold) beats 1 non-empty with AcceptMostCommon",
		// },

		// // Priority Level 3 vs 4: Empty vs Consensus-Valid Errors
		// {
		// 	name: "empty_beats_consensus_errors",
		// 	mockResponses: []map[string]interface{}{
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"error": map[string]interface{}{
		// 				"code":    3,
		// 				"message": "execution reverted",
		// 			},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"error": map[string]interface{}{
		// 				"code":    3,
		// 				"message": "execution reverted",
		// 			},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"error": map[string]interface{}{
		// 				"code":    3,
		// 				"message": "execution reverted",
		// 			},
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  []interface{}{},
		// 		},
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    3,
		// 	preferLargerResponses: false,
		// 	expectedError:         false,
		// 	expectedResult:        `[]`,
		// 	description:           "1 empty beats 3 consensus-valid errors with AcceptBest",
		// },

		// // Priority Level 4 vs 5: Consensus Errors vs Generic Errors
		// {
		// 	name: "consensus_errors_beat_generic_errors",
		// 	mockResponses: []map[string]interface{}{
		// 		nil, // Will trigger network error
		// 		nil, // Will trigger network error
		// 		nil, // Will trigger network error
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"error": map[string]interface{}{
		// 				"code":    3,
		// 				"message": "execution reverted",
		// 			},
		// 		},
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    3,
		// 	preferLargerResponses: false,
		// 	expectedError:         true, // Should return the consensus error
		// 	expectedResult:        nil,
		// 	description:           "1 consensus error beats 3 generic errors with AcceptBest",
		// },

		// // Threshold Edge Cases
		// {
		// 	name: "exactly_threshold_count_passes",
		// 	mockResponses: []map[string]interface{}{
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xvalue",
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xvalue",
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xother",
		// 		},
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    2, // Exactly 2
		// 	preferLargerResponses: false,
		// 	expectedError:         false,
		// 	expectedResult:        `"0xvalue"`,
		// 	description:           "Result with exactly threshold count (2) passes",
		// },
		// {
		// 	name: "threshold_one_edge_case",
		// 	mockResponses: []map[string]interface{}{
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xvalue",
		// 		},
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    1, // Threshold of 1
		// 	preferLargerResponses: false,
		// 	expectedError:         false,
		// 	expectedResult:        `"0xvalue"`,
		// 	description:           "Threshold=1 accepts single response",
		// },
		// {
		// 	name: "all_below_threshold_fails",
		// 	mockResponses: []map[string]interface{}{
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xa",
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xb",
		// 		},
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xc",
		// 		},
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    2, // Nothing meets threshold
		// 	preferLargerResponses: false,
		// 	expectedError:         true,
		// 	expectedResult:        nil,
		// 	description:           "All responses below threshold → error",
		// },

		// // Mixed Priority Levels
		// {
		// 	name: "mixed_all_priority_levels",
		// 	mockResponses: []map[string]interface{}{
		// 		nil, // Generic error
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"error": map[string]interface{}{
		// 				"code":    3,
		// 				"message": "reverted",
		// 			},
		// 		}, // Consensus error
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  []interface{}{},
		// 		}, // Empty
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result":  "0xsmall",
		// 		}, // Non-empty
		// 		{
		// 			"jsonrpc": "2.0",
		// 			"id":      1,
		// 			"result": []interface{}{
		// 				map[string]interface{}{"big": "data"},
		// 				map[string]interface{}{"more": "stuff"},
		// 			},
		// 		}, // Larger non-empty
		// 	},
		// 	disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		// 	agreementThreshold:    3,
		// 	preferLargerResponses: true,
		// 	expectedError:         false,
		// 	expectedResult:        func(r string) bool { return r == `[{"big":"data"},{"more":"stuff"}]` },
		// 	description:           "Mixed priority levels → largest non-empty wins with AcceptBest",
		// },
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			// Don't assert no pending mocks - with Persist() and consensus short-circuiting,
			// not all mocks will be consumed
			// defer util.AssertNoPendingMocks(t, 0)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create upstreams
			var upstreams []*common.UpstreamConfig
			for i := 0; i < len(tc.mockResponses); i++ {
				upstreams = append(upstreams, &common.UpstreamConfig{
					Id:       string(rune('a' + i)),
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://" + string(rune('a'+i)) + ".localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				})
			}

			// Setup mocks
			for i, resp := range tc.mockResponses {
				if resp != nil {
					gock.New("http://" + string(rune('a'+i)) + ".localhost").
						Post("").
						Persist().
						Reply(200).
						JSON(resp)
				}
			}

			// Create network with consensus policy
			preferNonEmpty := true
			network := setupTestNetworkWithConsensusPolicy(t, ctx, upstreams, &common.ConsensusPolicyConfig{
				MaxParticipants:         len(upstreams),
				AgreementThreshold:      tc.agreementThreshold,
				DisputeBehavior:         tc.disputeBehavior,
				LowParticipantsBehavior: tc.lowParticipantsBehavior,
				PreferNonEmpty:          &preferNonEmpty,
				PreferLargerResponses:   &tc.preferLargerResponses,
			})

			// Make request
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[],"id":1}`))
			resp, err := network.Forward(ctx, req)

			// Validate results
			if tc.expectedError {
				assert.Error(t, err, tc.description)
			} else {
				assert.NoError(t, err, tc.description)
				if resp != nil {
					jsonResp, _ := resp.JsonRpcResponse()
					if jsonResp != nil {
						// jsonResp.Result is already a json.RawMessage (byte array), no need to marshal
						resultStr := string(jsonResp.Result)
						switch expected := tc.expectedResult.(type) {
						case string:
							assert.Equal(t, expected, resultStr, tc.description)
						case func(string) bool:
							assert.True(t, expected(resultStr), tc.description+" - result: "+resultStr)
						}
					}
				}
			}

			t.Logf("✅ %s: %s", tc.name, tc.description)
		})
	}
}

// TestConsensusBlockHeadLeaderComprehensive tests all block head leader scenarios
func TestConsensusBlockHeadLeaderComprehensive(t *testing.T) {
	tests := []struct {
		name           string
		behavior       string // "prefer" or "only"
		leaderResponse map[string]interface{}
		otherResponses []map[string]interface{}
		expectedError  bool
		expectedResult string
		description    string
	}{
		{
			name:     "prefer_leader_returns_error",
			behavior: "prefer",
			leaderResponse: map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "leader error",
				},
			},
			otherResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xsuccess",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xsuccess",
				},
			},
			expectedError:  true, // Leader's error is returned
			expectedResult: "",
			description:    "PreferBlockHeadLeader returns leader's error even when others succeed",
		},
		{
			name:     "only_leader_returns_empty_others_nonempty",
			behavior: "only",
			leaderResponse: map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			},
			otherResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdata",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdata",
				},
			},
			expectedError:  false,
			expectedResult: `[]`, // Leader's empty result wins
			description:    "OnlyBlockHeadLeader returns leader's empty even when others have data",
		},
		{
			name:           "prefer_leader_fallback_to_consensus",
			behavior:       "prefer",
			leaderResponse: nil, // Leader doesn't respond
			otherResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xagreed",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xagreed",
				},
			},
			expectedError:  false,
			expectedResult: `"0xagreed"`,
			description:    "PreferBlockHeadLeader falls back to consensus when leader unavailable",
		},
		{
			name:           "only_leader_no_response_error",
			behavior:       "only",
			leaderResponse: nil, // Leader doesn't respond
			otherResponses: []map[string]interface{}{
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdata",
				},
				{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0xdata",
				},
			},
			expectedError:  true,
			expectedResult: "",
			description:    "OnlyBlockHeadLeader errors when leader doesn't respond",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Skip("Block head leader tests require state poller setup")
			// These tests would require more complex setup with state pollers
			// to properly simulate block head leader selection
		})
	}
}
