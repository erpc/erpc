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

// A hash-only transaction list cannot settle whether an empty trie root is
// legitimate: phantom-ness lives in the transaction OBJECT (from/gas, or the
// synthetic signature) and none of it survives in a bare hash. Rejecting there
// asserts on data the check cannot evaluate — which is precisely what made
// hyperevm reject honest blocks across 5 of 5 upstreams.
func TestEmptyRootWithHashOnlyTxsSkips(t *testing.T) {
	body := []byte(`{"number":"0x282ab57","hash":"0xabc","parentHash":"0xpar",` +
		`"transactionsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",` +
		`"transactions":["0xe3867506e5d423fe54"]}`)
	res := runContinuity(t, "transactionsRootConsistency", body, nil)
	assert.NoError(t, res.Err, "an empty root beside a hash-only tx is not evidence of corruption")
	assert.Equal(t, "skip", outcomeOf(res, "transactionsRootConsistency"))
}

// The genuine catch must survive: a HYDRATED block with a real transaction and
// an empty trie root is the ~150k-catch class from base and still rejects.
func TestEmptyRootWithHydratedRealTxStillRejects(t *testing.T) {
	body := []byte(`{"number":"0x64","hash":"0xabc","parentHash":"0xpar",` +
		`"transactionsRoot":"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",` +
		`"transactions":[{"hash":"0xdead","from":"0x1111111111111111111111111111111111111111","gas":"0x5208","r":"0xabc","gasPrice":"0x3b9aca00"}]}`)
	res := runContinuity(t, "transactionsRootConsistency", body, nil)
	assert.Error(t, res.Err, "a real hydrated transaction with an empty trie root is the genuine catch")
}
