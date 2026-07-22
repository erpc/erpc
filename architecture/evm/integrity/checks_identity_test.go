package integrity

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	reqHash   = "0x59d203e3c683df400be7440166d2939a887d54982fcede662861d3dfd7fe5910"
	otherHash = "0x7c9f61a71bf3541ff02f19af20dc3763158936770b7d9be1eb5e0bb3ecee913a"
)

func validateByHash(t *testing.T, method, paramHash, result string, cs CheckSet, hist History) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":["` + paramHash + `"]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(result), nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	var params []any
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(`["`+paramHash+`"]`), &params))
	return Validate(context.Background(), Input{
		Method:   method,
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
		Params:   params,
		History:  hist,
		Reorg:    rejectAll,
	})
}

func TestCheck_TxByHashIdentity(t *testing.T) {
	cs := only("txByHashIdentity", nil)

	t.Run("matching hash passes", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionByHash", reqHash,
			`{"hash":"`+reqHash+`","from":"0xabc","blockHash":"0xbb","blockNumber":"0x10"}`, cs, nil)
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "txByHashIdentity"))
	})

	t.Run("wrong transaction returned → reject (THE mixed-up-node catch)", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionByHash", reqHash,
			`{"hash":"`+otherHash+`","from":"0xabc","blockHash":"0xbb","blockNumber":"0x10"}`, cs, nil)
		require.Error(t, res.Err)
		assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
	})

	t.Run("non-hash params (unexpected shape) → skip", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionByHash", "0x10",
			`{"hash":"`+otherHash+`"}`, cs, nil)
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "txByHashIdentity"))
	})

	t.Run("case-insensitive hash compare", func(t *testing.T) {
		up := "0x59D203E3C683DF400BE7440166D2939A887D54982FCEDE662861D3DFD7FE5910"
		res := validateByHash(t, "eth_getTransactionByHash", reqHash,
			`{"hash":"`+up+`"}`, cs, nil)
		assert.NoError(t, res.Err)
	})
}

func TestCheck_ReceiptIdentity(t *testing.T) {
	cs := only("receiptIdentity", nil)

	t.Run("matching receipt passes", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionReceipt", reqHash,
			`{"transactionHash":"`+reqHash+`","blockHash":"0xbb","blockNumber":"0x10","logs":[]}`, cs, nil)
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "receiptIdentity"))
	})

	t.Run("wrong receipt returned → reject", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionReceipt", reqHash,
			`{"transactionHash":"`+otherHash+`","blockHash":"0xbb","blockNumber":"0x10","logs":[]}`, cs, nil)
		require.Error(t, res.Err)
	})
}

// blockByHashIdentity is the only check that can catch a node answering an
// eth_getBlockByHash lookup with a different block: continuity deliberately
// does not judge by-hash lookups, and blockHashRecompute only proves the
// returned block is self-consistent.
func TestCheck_BlockByHashIdentity(t *testing.T) {
	cs := only("blockByHashIdentity", nil)
	block := func(hash string) string {
		return `{"number":"0x10","hash":"` + hash + `","parentHash":"0xaa","transactions":[]}`
	}

	t.Run("the requested block passes", func(t *testing.T) {
		res := validateByHash(t, "eth_getBlockByHash", reqHash, block(reqHash), cs, nil)
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "blockByHashIdentity"))
	})

	t.Run("a different block returned → reject", func(t *testing.T) {
		res := validateByHash(t, "eth_getBlockByHash", reqHash, block(otherHash), cs, nil)
		require.Error(t, res.Err)
		assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
		assert.Equal(t, "reject", outcomeOf(res, "blockByHashIdentity"))
	})

	t.Run("case-insensitive hash comparison passes", func(t *testing.T) {
		res := validateByHash(t, "eth_getBlockByHash", reqHash, block(strings.ToUpper(reqHash)), cs, nil)
		assert.NoError(t, res.Err)
	})

	t.Run("an orphaned-but-requested block still passes (canonicality is not identity)", func(t *testing.T) {
		// The caller named this hash; that it lost a reorg is not this check's
		// business — and continuity no longer judges by-hash lookups either.
		res := validateByHash(t, "eth_getBlockByHash", reqHash, block(reqHash), cs, mockHistory{0x10: otherHash})
		assert.NoError(t, res.Err)
	})

	t.Run("unparseable params → skip, never guess", func(t *testing.T) {
		res := validateByHash(t, "eth_getBlockByHash", "0xnothash", block(reqHash), cs, nil)
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "blockByHashIdentity"))
	})
}

// blockByNumberIdentity closes the by-number twin of the same gap: Layer-1
// enforceHighestBlock only guards the latest/finalized TAGS, and continuity
// anchors on the number the response claims — so a different-but-canonical
// block returned for an explicit height passes everything and gets cached
// under the requested key.
func TestCheck_BlockByNumberIdentity(t *testing.T) {
	cs := only("blockByNumberIdentity", nil)
	// validateByHash threads params[0] verbatim, which is what this check reads.
	call := func(t *testing.T, param, respNumber string) Result {
		t.Helper()
		return validateByHash(t, "eth_getBlockByNumber", param,
			`{"number":"`+respNumber+`","hash":"`+reqHash+`","parentHash":"0xaa"}`, cs, nil)
	}

	t.Run("the requested height passes", func(t *testing.T) {
		res := call(t, "0x123", "0x123")
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "blockByNumberIdentity"))
	})

	t.Run("a different height → reject", func(t *testing.T) {
		res := call(t, "0x123", "0x456")
		require.Error(t, res.Err)
		assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
	})

	t.Run("off-by-one → reject (the realistic bad-index shape)", func(t *testing.T) {
		res := call(t, "0x123", "0x124")
		require.Error(t, res.Err)
	})

	t.Run("leading zeros are the same height, not a violation", func(t *testing.T) {
		assert.NoError(t, call(t, "0x0123", "0x123").Err)
		assert.NoError(t, call(t, "0x123", "0x0123").Err)
	})

	t.Run("uppercase hex digits are the same height", func(t *testing.T) {
		assert.NoError(t, call(t, "0x1ab", "0x1AB").Err)
	})

	for _, tag := range []string{"latest", "finalized", "safe", "pending", "earliest"} {
		t.Run("tag "+tag+" → skip (no height was named)", func(t *testing.T) {
			res := call(t, tag, "0x999")
			assert.NoError(t, res.Err)
			assert.Equal(t, "skip", outcomeOf(res, "blockByNumberIdentity"))
		})
	}
}

func TestCheck_TxPinConsistency(t *testing.T) {
	cs := only("txPinConsistency", nil)
	tx := func(blockHash string) string {
		return `{"hash":"` + reqHash + `","blockHash":"` + blockHash + `","blockNumber":"0x10"}`
	}

	t.Run("tx on the committed block passes", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionByHash", reqHash, tx("0xbb"), cs, mockHistory{0x10: "0xbb"})
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "txPinConsistency"))
	})

	t.Run("tx claiming a different block than the pin → reject (strict)", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionByHash", reqHash, tx("0xevil"), cs, mockHistory{0x10: "0xbb"})
		require.Error(t, res.Err)
	})

	t.Run("stale pin → reconfirmed, pin adopts (reorg-safe)", func(t *testing.T) {
		hist := &reconfirmingHistory{
			pins:      map[int64]string{0x10: "0xold"},
			canonical: map[int64]string{0x10: "0xnew"},
		}
		res := validateByHash(t, "eth_getTransactionByHash", reqHash, tx("0xnew"), cs, hist)
		assert.NoError(t, res.Err)
		assert.Equal(t, "reconfirmed", outcomeOf(res, "txPinConsistency"))
		assert.Equal(t, "0xnew", hist.pins[0x10])
	})

	t.Run("unpinned number → skip", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionByHash", reqHash, tx("0xbb"), cs, mockHistory{})
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "txPinConsistency"))
	})

	t.Run("pending tx (no coords) → skip", func(t *testing.T) {
		res := validateByHash(t, "eth_getTransactionByHash", reqHash,
			`{"hash":"`+reqHash+`","blockHash":"","blockNumber":""}`, cs, mockHistory{0x10: "0xbb"})
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "txPinConsistency"))
	})
}
