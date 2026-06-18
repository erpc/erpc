package integrity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAllPhantomRawTxs(t *testing.T) {
	assert.True(t, allPhantomRawTxs(nil))
	assert.True(t, allPhantomRawTxs([]any{}))
	assert.True(t, allPhantomRawTxs([]any{map[string]any{"from": "0x0000000000000000000000000000000000000000", "gas": "0x0"}}))
	assert.True(t, allPhantomRawTxs([]any{map[string]any{"from": "0x0", "gas": "0x0"}})) // short zero form
	assert.False(t, allPhantomRawTxs([]any{map[string]any{"from": "0xdead000000000000000000000000000000000001", "gas": "0x5208"}}))
	// mix of phantom + real → not all phantom
	assert.False(t, allPhantomRawTxs([]any{
		map[string]any{"from": "0x0", "gas": "0x0"},
		map[string]any{"from": "0xdead000000000000000000000000000000000001", "gas": "0x5208"},
	}))
	// hash-only entry → conservatively not phantom
	assert.False(t, allPhantomRawTxs([]any{"0xabc"}))
	// non-zero gas or non-zero from → not phantom
	assert.False(t, allPhantomRawTxs([]any{map[string]any{"from": "0x0", "gas": "0x1"}}))
	assert.False(t, allPhantomRawTxs([]any{map[string]any{"from": "0x0000000000000000000000000000000000000001", "gas": "0x0"}}))
}

func TestIsZeroishHex(t *testing.T) {
	for _, z := range []string{"0x0", "0x00", "0x0000", "0x"} {
		assert.True(t, isZeroishHex(z), z)
	}
	for _, nz := range []string{"0x1", "0x10", ""} {
		assert.False(t, isZeroishHex(nz), nz)
	}
}

func TestCheck_TransactionsRootConsistency(t *testing.T) {
	cs := only("transactionsRootConsistency", nil)

	// Real-world scenario: a sidechain block with the empty trie root whose only
	// "transaction" is a phantom system tx (from=0x0, gas=0x0) must PASS.
	phantom := []byte(`{"transactionsRoot":"` + emptyTrieRoot + `","transactions":[{"from":"0x0000000000000000000000000000000000000000","gas":"0x0"}]}`)
	assert.NoError(t, run(t, MethodGetBlockByNumber, phantom, cs))

	// Empty trie root but a real transaction → inconsistent.
	realEmpty := []byte(`{"transactionsRoot":"` + emptyTrieRoot + `","transactions":[{"from":"0xdead000000000000000000000000000000000001","gas":"0x5208"}]}`)
	err := run(t, MethodGetBlockByNumber, realEmpty, cs)
	assertRejected(t, err)
	assert.Contains(t, err.Error(), "non-phantom")

	// Non-empty root but zero transactions → truncated data.
	nonEmptyZero := []byte(`{"transactionsRoot":"0x1111111111111111111111111111111111111111111111111111111111111111","transactions":[]}`)
	assertRejected(t, run(t, MethodGetBlockByNumber, nonEmptyZero, cs))

	// Non-empty root with transactions → fine.
	ok := []byte(`{"transactionsRoot":"0x1111111111111111111111111111111111111111111111111111111111111111","transactions":[{"hash":"0xabc"}]}`)
	assert.NoError(t, run(t, MethodGetBlockByNumber, ok, cs))
}

func TestCheck_HeaderFieldShapes(t *testing.T) {
	cs := only("headerFieldShapes", nil)
	h32 := "0x" + repeat("11", 32)
	bloom := "0x" + repeat("00", 256)
	good := []byte(`{"hash":"` + h32 + `","parentHash":"` + h32 + `","stateRoot":"` + h32 + `","transactionsRoot":"` + h32 + `","receiptsRoot":"` + h32 + `","logsBloom":"` + bloom + `"}`)
	assert.NoError(t, run(t, MethodGetBlockByNumber, good, cs))

	badHash := []byte(`{"hash":"0x1234"}`)
	assertRejected(t, run(t, MethodGetBlockByNumber, badHash, cs))

	badBloom := []byte(`{"hash":"` + h32 + `","logsBloom":"0xabcd"}`)
	assertRejected(t, run(t, MethodGetBlockByNumber, badBloom, cs))
}

func TestCheck_TxFieldUniqueness(t *testing.T) {
	cs := only("txFieldUniqueness", nil)
	h32a := "0x" + repeat("aa", 32)
	h32b := "0x" + repeat("bb", 32)
	good := []byte(`{"transactions":[{"hash":"` + h32a + `"},{"hash":"` + h32b + `"}]}`)
	assert.NoError(t, run(t, MethodGetBlockByNumber, good, cs))

	dup := []byte(`{"transactions":[{"hash":"` + h32a + `"},{"hash":"` + h32a + `"}]}`)
	assertRejected(t, run(t, MethodGetBlockByNumber, dup, cs))

	shortHash := []byte(`{"transactions":[{"hash":"0x1234"}]}`)
	assertRejected(t, run(t, MethodGetBlockByNumber, shortHash, cs))
}

func TestCheck_TxBlockInfo(t *testing.T) {
	cs := only("txBlockInfo", nil)
	good := []byte(`{"hash":"0xaa","number":"0x10","transactions":[{"blockHash":"0xAA","blockNumber":"0x10","transactionIndex":"0x0"}]}`)
	assert.NoError(t, run(t, MethodGetBlockByNumber, good, cs))

	wrongHash := []byte(`{"hash":"0xaa","number":"0x10","transactions":[{"blockHash":"0xbb","blockNumber":"0x10","transactionIndex":"0x0"}]}`)
	assertRejected(t, run(t, MethodGetBlockByNumber, wrongHash, cs))

	wrongIdx := []byte(`{"hash":"0xaa","number":"0x10","transactions":[{"blockHash":"0xaa","blockNumber":"0x10","transactionIndex":"0x3"}]}`)
	assertRejected(t, run(t, MethodGetBlockByNumber, wrongIdx, cs))
}

// repeat returns s repeated n times (small local helper for building fixtures).
func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
