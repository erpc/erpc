package integrity

import (
	"encoding/json"
	"math/big"
	"testing"

	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
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
