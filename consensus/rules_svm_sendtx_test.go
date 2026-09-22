package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SVM sendTransaction must get the same first-valid-response treatment
// as eth_sendRawTransaction: one accepted signature is final, waiting
// for agreement only multiplies broadcasts.
func TestSvmSendTransaction_ConsensusRule(t *testing.T) {
	sig := "5VERYFAKE58sigxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	jrpc, err := common.NewJsonRpcResponse(1, sig, nil)
	require.NoError(t, err)
	resp := common.NewNormalizedResponse()
	resp.WithJsonRpcResponse(jrpc)

	for _, method := range []string{"sendTransaction", "sendRawTransaction"} {
		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"hash1": {
					Hash:          "hash1",
					Count:         1,
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: resp,
					Results:       []*execResult{{Result: resp}},
				},
			},
			totalParticipants: 1,
			validParticipants: 1,
			method:            method,
		}

		var matchedRule *consensusRule
		for i := range consensusRules {
			if consensusRules[i].Condition(analysis) {
				matchedRule = &consensusRules[i]
				break
			}
		}
		require.NotNil(t, matchedRule, "%s should match the tx-broadcast rule", method)
		assert.Contains(t, matchedRule.Description, "tx broadcast")

		result := matchedRule.Action(analysis)
		require.NotNil(t, result)
		require.Nil(t, result.Error)
		require.NotNil(t, result.Result)
	}
}

func TestIsTxBroadcastMethod(t *testing.T) {
	t.Parallel()
	for m, want := range map[string]bool{
		"eth_sendRawTransaction": true,
		"sendTransaction":        true,
		"sendRawTransaction":     true,
		"requestAirdrop":         false, // not a broadcast; must not first-valid short-circuit
		"simulateTransaction":    false,
		"eth_call":               false,
	} {
		assert.Equal(t, want, isTxBroadcastMethod(m), m)
	}
}
