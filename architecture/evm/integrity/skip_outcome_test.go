package integrity

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The skip/pass split: "pass" is reported only when a check actually verified
// something; a check that could not evaluate the response reports "skip". This
// is what turns "no rejects" on the dashboard into the positive statement
// "N responses verified against canonical, 0 mismatches".

func TestSkipOutcome_Continuity(t *testing.T) {
	t.Run("hashStability: no history wired → skip", func(t *testing.T) {
		res := validateBlockPolicy(t, blockResult("0x10", "0xaa", "0xparent"), only("hashStability", nil), nil, rejectAll)
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "hashStability"))
	})

	t.Run("hashStability: number never observed → skip", func(t *testing.T) {
		res := validateBlockPolicy(t, blockResult("0x10", "0xaa", "0xparent"), only("hashStability", nil), mockHistory{}, rejectAll)
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "hashStability"))
	})

	t.Run("hashStability: pin matches → pass (a real verification)", func(t *testing.T) {
		res := validateBlockPolicy(t, blockResult("0x10", "0xaa", "0xparent"), only("hashStability", nil), mockHistory{0x10: "0xaa"}, rejectAll)
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "hashStability"))
	})

	t.Run("parentHashLinkage: parent never observed → skip", func(t *testing.T) {
		res := validateBlockPolicy(t, blockResult("0x11", "0xbb", "0xaa"), only("parentHashLinkage", nil), mockHistory{}, rejectAll)
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "parentHashLinkage"))
	})

	t.Run("parentHashLinkage: link verified → pass", func(t *testing.T) {
		res := validateBlockPolicy(t, blockResult("0x11", "0xbb", "0xaa"), only("parentHashLinkage", nil), mockHistory{0x10: "0xaa"}, rejectAll)
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "parentHashLinkage"))
	})
}

func TestSkipOutcome_GetLogs(t *testing.T) {
	filter := `{"address":"0xaddr","fromBlock":"0x10","toBlock":"0x10"}`
	logJSON := `[{"address":"0xaddr","topics":["0xa"],"data":"0xd1","blockNumber":"0x10","blockHash":"0xbb","transactionHash":"0xt1","logIndex":"0x0"}]`

	t.Run("filterSanity: parseable filter with logs → pass", func(t *testing.T) {
		res := validateGetLogs(t, filter, logJSON, only("getLogsFilterSanity", nil), nil, nil)
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "getLogsFilterSanity"))
	})

	t.Run("filterSanity: unparseable filter → skip", func(t *testing.T) {
		res := validateGetLogs(t, `{"address":[1]}`, logJSON, only("getLogsFilterSanity", nil), nil, nil)
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "getLogsFilterSanity"))
	})

	t.Run("completeness: cold cache → skip, not pass", func(t *testing.T) {
		cold := histWithReceipts{pins: map[int64]string{}, receipts: map[string][]Receipt{}}
		res := validateGetLogs(t, filter, logJSON, only("getLogsCompleteness", nil), cold, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "getLogsCompleteness"))
	})

	t.Run("completeness: no receipts cache wired → skip", func(t *testing.T) {
		res := validateGetLogs(t, filter, logJSON, only("getLogsCompleteness", nil), mockHistory{}, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "getLogsCompleteness"))
	})

	t.Run("completeness: warm cache compared clean → pass", func(t *testing.T) {
		warm := histWithReceipts{
			pins: map[int64]string{0x10: "0xbb"},
			receipts: map[string][]Receipt{
				"0xbb": {{BlockHash: "0xbb", TransactionHash: "0xt1", Logs: []Log{
					{Address: "0xaddr", Topics: []string{"0xa"}, Data: "0xd1", BlockHash: "0xbb", BlockNumber: "0x10", TransactionHash: "0xt1", LogIndex: "0x0"},
				}}},
			},
		}
		res := validateGetLogs(t, filter, logJSON, only("getLogsCompleteness", nil), warm, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "getLogsCompleteness"))
	})
}

func TestSkipOutcome_ReceiptVsBlock(t *testing.T) {
	receipt := `{"transactionHash":"0xaa","blockHash":"0xbb","blockNumber":"0x10","transactionIndex":"0x0","logs":[]}`
	validate := func(t *testing.T, hist History, r Resolver) Result {
		t.Helper()
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xaa"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(receipt), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		return Validate(context.Background(), Input{
			Method:   "eth_gettransactionreceipt",
			Upstream: common.NewFakeUpstream("u"),
			Response: rs,
			Checks:   only("receiptVsBlock", nil),
			History:  hist,
			Resolver: r,
			Reorg:    rejectAll,
		})
	}

	t.Run("no resolver → skip even when the pin branch matched", func(t *testing.T) {
		res := validate(t, mockHistory{0x10: "0xbb"}, nil)
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "receiptVsBlock"), "pin consistency alone is not canonical corroboration")
	})

	t.Run("canonical unavailable (tip-lag) → skip", func(t *testing.T) {
		res := validate(t, mockHistory{0x10: "0xbb"}, mockResolver{finalized: true, known: true, have: false})
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "receiptVsBlock"))
	})

	t.Run("full canonical corroboration → pass", func(t *testing.T) {
		canonical := []Receipt{{TransactionHash: "0xaa", BlockHash: "0xbb", Logs: []Log{}}}
		res := validate(t, mockHistory{0x10: "0xbb"}, mockResolver{finalized: true, known: true, receipts: canonical, have: true})
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "receiptVsBlock"))
	})
}

func TestSkipOutcome_ReceiptsRootRecompute(t *testing.T) {
	// No resolver wired → the committed root can't be fetched → skip.
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x10"]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`[{"transactionHash":"0xaa","blockHash":"0xbb","logs":[]}]`), nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	res := Validate(context.Background(), Input{
		Method:   "eth_getblockreceipts",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   only("receiptsRootRecompute", nil),
		Reorg:    rejectAll,
	})
	assert.NoError(t, res.Err)
	assert.Equal(t, "skip", outcomeOf(res, "receiptsRootRecompute"))
}

// A Skipped sentinel must never be treated as a violation, even under the
// strictest policy and even for a Deterministic check.
func TestSkipOutcome_NeverRejects(t *testing.T) {
	res := validateGetLogs(t, `{"address":[1]}`,
		`[{"address":"0xEVIL","topics":["0xa"],"blockNumber":"0x10","blockHash":"0xbb","logIndex":"0x0","transactionHash":"0xt1"}]`,
		only("getLogsFilterSanity", nil), nil, nil)
	require.NoError(t, res.Err)
	assert.Empty(t, res.Recorded)
}

// A rejecting check's class rides the Result so callers can feed PROVABLE
// (deterministic) corruption into upstream health scoring while leaving
// possibly-transient reorg-sensitive rejects out of it.
func TestResult_RejectedClass(t *testing.T) {
	t.Run("deterministic reject carries Deterministic", func(t *testing.T) {
		res := validateGetLogs(t, `{"address":"0xaddr"}`,
			`[{"address":"0xEVIL","topics":["0xa"],"blockNumber":"0x10","blockHash":"0xbb","logIndex":"0x0","transactionHash":"0xt1"}]`,
			only("getLogsFilterSanity", nil), nil, nil)
		require.Error(t, res.Err)
		assert.Equal(t, Deterministic, res.RejectedClass)
	})
	t.Run("reorg-sensitive reject carries ReorgSensitive", func(t *testing.T) {
		hist := mockHistory{0x10: "0xaa"}
		res := validateBlockPolicy(t, blockResult("0x10", "0xDIFFERENT", "0xparent"), only("hashStability", nil), hist, rejectAll)
		require.Error(t, res.Err)
		assert.Equal(t, ReorgSensitive, res.RejectedClass)
	})
}
