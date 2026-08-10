package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Operator maps keyed by method name (preferHighestValueFor, ignoreFields) must
// resolve the same way dispatch does — case-insensitively. Otherwise a
// non-canonical casing silently drops the operator's policy and falls back to
// plain agreement, with nothing logged.
func TestMethodFieldsCaseInsensitive(t *testing.T) {
	t.Parallel()

	defs := map[string][]string{"eth_getTransactionCount": {"result"}}

	for _, casing := range []string{"eth_getTransactionCount", "ETH_GETTRANSACTIONCOUNT", "eth_gettransactioncount"} {
		fields, ok := methodFields(defs, casing)
		assert.True(t, ok, "casing %s must resolve the operator policy", casing)
		assert.Equal(t, []string{"result"}, fields)
	}

	_, ok := methodFields(defs, "eth_getBalance")
	assert.False(t, ok, "unrelated methods still miss")
	_, ok = methodFields(defs, "")
	assert.False(t, ok, "an unresolved method name never matches")
	_, ok = methodFields(nil, "eth_getTransactionCount")
	assert.False(t, ok)

	// Deterministic among duplicate casings: smallest key wins, independent of
	// map iteration order.
	dupes := map[string][]string{"ETH_CALL": {"a"}, "eTH_CALL": {"b"}}
	for i := 0; i < 50; i++ {
		fields, ok := methodFields(dupes, "eth_call")
		require.True(t, ok)
		assert.Equal(t, []string{"a"}, fields)
	}
}

// The raw-transaction broadcast exemptions must not turn on client casing.
func TestSendRawTransactionExemptionCaseInsensitive(t *testing.T) {
	t.Parallel()

	assert.True(t, isTxBroadcastMethod("eth_sendRawTransaction"))
	assert.True(t, isTxBroadcastMethod("ETH_SENDRAWTRANSACTION"))
	assert.True(t, isTxBroadcastMethod("Eth_SendRawTransaction"))
	assert.False(t, isTxBroadcastMethod("eth_call"))

	txHash := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	jrpc, err := common.NewJsonRpcResponse(1, txHash, nil)
	require.NoError(t, err)
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrpc)

	mkAnalysis := func(method string) *consensusAnalysis {
		return &consensusAnalysis{
			config: &config{maxParticipants: 3, agreementThreshold: 2},
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
	}

	// The first-valid-response rule fires for a mixed-case broadcast exactly as
	// it does for canonical casing — one accepted broadcast propagates network
	// wide, so waiting for a quorum proves nothing either way.
	for _, casing := range []string{"eth_sendRawTransaction", "Eth_SendRawTransaction"} {
		matched := false
		for i := range consensusRules {
			if consensusRules[i].Condition(mkAnalysis(casing)) {
				matched = consensusRules[i].Description == "eth_sendRawTransaction: return first valid tx hash response"
				break
			}
		}
		assert.True(t, matched, "casing %s must match the broadcast rule", casing)
	}
}
