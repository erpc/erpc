package integrity

import (
	"encoding/json"
	"fmt"
	"math/big"
	"testing"

	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleHeader() *gethtypes.Header {
	return &gethtypes.Header{
		ParentHash:  gethcommon.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
		UncleHash:   gethtypes.EmptyUncleHash,
		Coinbase:    gethcommon.HexToAddress("0xabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"),
		Root:        gethcommon.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222"),
		TxHash:      gethcommon.HexToHash("0x4444444444444444444444444444444444444444444444444444444444444444"),
		ReceiptHash: gethcommon.HexToHash("0x5555555555555555555555555555555555555555555555555555555555555555"),
		Bloom:       gethtypes.Bloom{},
		Difficulty:  big.NewInt(1),
		Number:      big.NewInt(15040019),
		GasLimit:    30000000,
		GasUsed:     21000,
		Time:        1700000000,
		Extra:       []byte{},
		MixDigest:   gethcommon.Hash{},
		Nonce:       gethtypes.BlockNonce{},
	}
}

// blockJSON renders an eth_getBlockByNumber result from a header, forcing the
// "hash" field to claimed and optionally injecting/overriding fields.
func blockJSON(t *testing.T, h *gethtypes.Header, claimed string, override map[string]string) []byte {
	t.Helper()
	raw, err := h.MarshalJSON()
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	set := func(k, v string) {
		b, _ := json.Marshal(v)
		m[k] = b
	}
	set("hash", claimed)
	for k, v := range override {
		set(k, v)
	}
	out, err := json.Marshal(m)
	require.NoError(t, err)
	return out
}

func TestCheck_BlockHashRecompute(t *testing.T) {
	h := sampleHeader()
	realHash := h.Hash().Hex()
	cs := only("blockHashRecompute", nil)

	t.Run("matching hash passes", func(t *testing.T) {
		err := run(t, MethodGetBlockByNumber, blockJSON(t, h, realHash, nil), cs)
		assert.NoError(t, err)
	})

	t.Run("wrong claimed hash is rejected", func(t *testing.T) {
		err := run(t, MethodGetBlockByNumber, blockJSON(t, h, "0xdeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddead", nil), cs)
		require.Error(t, err)
	})

	t.Run("corrupted header field makes the recompute mismatch", func(t *testing.T) {
		// Claimed hash stays real, but stateRoot is altered → recompute differs.
		raw := blockJSON(t, h, realHash, map[string]string{
			"stateRoot": "0x3333333333333333333333333333333333333333333333333333333333333333",
		})
		require.Error(t, run(t, MethodGetBlockByNumber, raw, cs))
	})

	t.Run("unknown custom field → skipped, never false-flagged", func(t *testing.T) {
		// A chain with a header field the reference encoder doesn't know: even
		// with a deliberately wrong hash, the check must skip rather than reject.
		raw := blockJSON(t, h, "0xdeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddead",
			map[string]string{"l2CustomRoot": "0x6666666666666666666666666666666666666666666666666666666666666666"})
		assert.NoError(t, run(t, MethodGetBlockByNumber, raw, cs))
	})
}

func TestCheck_TransactionsRootRecompute(t *testing.T) {
	tx1, _ := signTx(t, 0)
	tx2, _ := signTx(t, 1)
	root := gethtypes.DeriveSha(gethtypes.Transactions{tx1, tx2}, trie.NewStackTrie(nil)).Hex()
	j1, _ := tx1.MarshalJSON()
	j2, _ := tx2.MarshalJSON()
	cs := only("transactionsRootRecompute", nil)

	block := func(rootHex, txsJSON string) []byte {
		return []byte(fmt.Sprintf(`{"transactionsRoot":"%s","transactions":[%s]}`, rootHex, txsJSON))
	}

	t.Run("matching root passes", func(t *testing.T) {
		assert.NoError(t, run(t, MethodGetBlockByNumber, block(root, string(j1)+","+string(j2)), cs))
	})

	t.Run("tampered root is rejected", func(t *testing.T) {
		bad := block("0xdeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddead", string(j1)+","+string(j2))
		require.Error(t, run(t, MethodGetBlockByNumber, bad, cs))
	})

	t.Run("a substituted/missing tx changes the root → rejected", func(t *testing.T) {
		// Claim the two-tx root but deliver only one → recompute differs.
		require.Error(t, run(t, MethodGetBlockByNumber, block(root, string(j1)), cs))
	})

	t.Run("hashes-only response is skipped", func(t *testing.T) {
		raw := block(root, fmt.Sprintf(`"%s","%s"`, tx1.Hash().Hex(), tx2.Hash().Hex()))
		assert.NoError(t, run(t, MethodGetBlockByNumber, raw, cs))
	})
}
