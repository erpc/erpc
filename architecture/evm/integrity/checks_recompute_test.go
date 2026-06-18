package integrity

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"

	"github.com/erpc/erpc/common"
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

func runReceipts(t *testing.T, result []byte, cs CheckSet, r Resolver) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x10"]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), result, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	in := Input{Method: "eth_getBlockReceipts", Upstream: common.NewFakeUpstream("u"), Response: rs, Checks: cs, Reorg: DefaultReorgPolicy()}
	if r != nil {
		in.Resolver = r
	}
	return Validate(context.Background(), in)
}

func TestCheck_ReceiptsRootRecompute(t *testing.T) {
	bh := gethcommon.HexToHash("0xabababababababababababababababababababababababababababababababab")
	mk := func(cumGas uint64, n uint64) *gethtypes.Receipt {
		return &gethtypes.Receipt{
			Type:              gethtypes.DynamicFeeTxType,
			Status:            gethtypes.ReceiptStatusSuccessful,
			CumulativeGasUsed: cumGas,
			Logs:              []*gethtypes.Log{},
			TxHash:            gethcommon.BigToHash(big.NewInt(int64(n))),
			BlockHash:         bh,
			BlockNumber:       big.NewInt(0x10),
		}
	}
	r0, r1 := mk(21000, 1), mk(42000, 2)
	root := gethtypes.DeriveSha(gethtypes.Receipts{r0, r1}, trie.NewStackTrie(nil)).Hex()
	j0, _ := r0.MarshalJSON()
	j1, _ := r1.MarshalJSON()
	resp := []byte("[" + string(j0) + "," + string(j1) + "]")
	cs := only("receiptsRootRecompute", nil)

	t.Run("matching receipts root passes", func(t *testing.T) {
		res := runReceipts(t, resp, cs, mockResolver{header: &Header{ReceiptsRoot: root}})
		assert.NoError(t, res.Err)
	})

	t.Run("tampered/foreign receipts root is rejected", func(t *testing.T) {
		res := runReceipts(t, resp, cs, mockResolver{header: &Header{ReceiptsRoot: "0xdeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddead"}})
		require.Error(t, res.Err)
	})

	t.Run("no resolver → skip", func(t *testing.T) {
		assert.NoError(t, runReceipts(t, resp, cs, nil).Err)
	})

	t.Run("header unavailable → skip", func(t *testing.T) {
		assert.NoError(t, runReceipts(t, resp, cs, mockResolver{}).Err)
	})

	t.Run("unknown receipt field → skip, never false-flagged", func(t *testing.T) {
		var arr []map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(resp, &arr))
		arr[0]["l1Fee"], _ = json.Marshal("0x1")
		custom, _ := json.Marshal(arr)
		res := runReceipts(t, custom, cs, mockResolver{header: &Header{ReceiptsRoot: "0xdeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddeaddead"}})
		assert.NoError(t, res.Err)
	})
}
