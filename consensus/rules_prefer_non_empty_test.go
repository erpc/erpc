package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// The "prefer-non-empty over an at-threshold empty" rule is gated on group
// counts rather than getBestByCount() so a count tie between the empty and the
// non-empty group resolves the same way on every run (map iteration order used
// to decide it). These cases pin both sides of that gate: the tie must rescue
// the non-empty, and a non-empty group that strictly leads by count must fall
// through to the PreferLargerResponses rules instead.
func TestPreferNonEmptyOverAtThresholdEmpty_Gate(t *testing.T) {
	newGroup := func(t *testing.T, hash string, count int, rt ResponseType, body interface{}) *responseGroup {
		t.Helper()
		jrpc, err := common.NewJsonRpcResponse(1, body, nil)
		require.NoError(t, err)
		resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrpc)
		size := 0
		if s, ok := body.(string); ok {
			size = len(s)
		}
		return &responseGroup{
			Hash:          hash,
			Count:         count,
			ResponseType:  rt,
			ResponseSize:  size,
			LargestResult: resp,
			Results:       []*execResult{{Result: resp}},
		}
	}

	winnerBody := func(t *testing.T, a *consensusAnalysis) string {
		t.Helper()
		for i := range consensusRules {
			if !consensusRules[i].Condition(a) {
				continue
			}
			res := consensusRules[i].Action(a)
			require.NotNil(t, res)
			require.NoError(t, res.Error)
			jrpc, err := res.Result.JsonRpcResponse()
			require.NoError(t, err)
			return jrpc.GetResultString()
		}
		t.Fatal("no consensus rule matched")
		return ""
	}

	t.Run("count tie with an at-threshold empty rescues the non-empty", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			a := &consensusAnalysis{
				config: &config{
					maxParticipants:    4,
					agreementThreshold: 2,
					disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
					preferNonEmpty:     true,
				},
				groups: map[string]*responseGroup{
					"empty":  newGroup(t, "empty", 2, ResponseTypeEmpty, nil),
					"filled": newGroup(t, "filled", 2, ResponseTypeNonEmpty, "0x1"),
				},
				collectedResponses: 4,
				totalParticipants:  4,
				validParticipants:  4,
			}
			require.Equal(t, `"0x1"`, winnerBody(t, a))
		}
	})

	t.Run("non-empty leading by count falls through to prefer-larger", func(t *testing.T) {
		a := &consensusAnalysis{
			config: &config{
				maxParticipants:       7,
				agreementThreshold:    2,
				disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
				preferNonEmpty:        true,
				preferLargerResponses: true,
			},
			groups: map[string]*responseGroup{
				"empty": newGroup(t, "empty", 2, ResponseTypeEmpty, nil),
				"small": newGroup(t, "small", 3, ResponseTypeNonEmpty, "0x1"),
				"large": newGroup(t, "large", 2, ResponseTypeNonEmpty, "0xdeadbeefdeadbeef"),
			},
			collectedResponses: 7,
			totalParticipants:  7,
			validParticipants:  7,
		}
		require.Equal(t, `"0xdeadbeefdeadbeef"`, winnerBody(t, a))
	})
}
