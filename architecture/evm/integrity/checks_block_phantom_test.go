package integrity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Chains inject system transactions into the RPC response that never enter the
// transactions trie, so a block of only those legitimately reports the empty
// trie root on a non-empty list. Recognising only the Polygon/BSC shape is what
// made this check unusable on HyperEVM: its system txs carry a real-looking
// sender and non-zero gas, so they read as ordinary transactions and such
// blocks were rejected as inconsistent — which is why the check had to be
// switched off there rather than fixed.
func TestPhantomTransactionShapes(t *testing.T) {
	polygon := map[string]any{"from": "0x0000000000000000000000000000000000000000", "gas": "0x0"}
	hyper := map[string]any{"from": "0x2222222222222222222222222222222222222222", "gas": "0x5208", "r": "0x1", "gasPrice": "0x0"}
	real := map[string]any{"from": "0x1111111111111111111111111111111111111111", "gas": "0x5208", "r": "0xabc", "gasPrice": "0x3b9aca00"}

	assert.True(t, isPhantomRawTx(polygon), "Polygon/BSC system tx")
	assert.True(t, isPhantomRawTx(hyper), "HyperEVM native/L1 system tx (r=0x1, gasPrice=0)")
	assert.False(t, isPhantomRawTx(real), "an ordinary transaction is never phantom")

	assert.True(t, allPhantomRawTxs([]any{polygon, hyper}))
	assert.False(t, allPhantomRawTxs([]any{polygon, real}),
		"one real transaction means the trie root must not be empty")
	assert.False(t, allPhantomRawTxs([]any{"0xhashonly"}),
		"a hash-only entry cannot be shown to be phantom")
}

// With the shape recognised, HyperEVM runs the check instead of disabling it.
func TestHyperEVMRunsTransactionsRootConsistency(t *testing.T) {
	assert.NotContains(t, ChainProfileDisables(999), "transactionsRootConsistency",
		"the check works on HyperEVM now that its phantom shape is recognised")
	assert.Contains(t, ChainProfileDisables(999), "transactionsRootRecompute",
		"the cryptographic recompute still cannot work: committed txs are absent from the response")
}
