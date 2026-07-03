package integrity

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- LogsFilter matcher: go-ethereum filterLogs semantics ---------------------

func lg(addr string, topics ...string) *Log { return &Log{Address: addr, Topics: topics} }

func filt(t *testing.T, paramJSON string) *LogsFilter {
	t.Helper()
	var params []any
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[`+paramJSON+`]`), &params))
	return parseLogsFilter(params)
}

// The canonical examples from the Ethereum JSON-RPC spec: a log with topics
// [A, B] is matched by [] / [A] / [null, B] / [A, B] / [[A, C], [B, D]].
func TestLogsFilter_GethSemantics(t *testing.T) {
	logAB := lg("0xaddr", "0xa", "0xb")

	cases := []struct {
		name   string
		filter string
		match  bool
	}{
		{"empty topics matches", `{"topics":[]}`, true},
		{"absent topics matches", `{}`, true},
		{"[A] matches", `{"topics":["0xa"]}`, true},
		{"[null, B] matches", `{"topics":[null,"0xb"]}`, true},
		{"[A, B] matches", `{"topics":["0xa","0xb"]}`, true},
		{"[[A,C],[B,D]] OR matches", `{"topics":[["0xa","0xc"],["0xb","0xd"]]}`, true},
		{"[C] no match", `{"topics":["0xc"]}`, false},
		{"[A, D] no match", `{"topics":["0xa","0xd"]}`, false},
		{"filter longer than log topics never matches (geth)", `{"topics":["0xa","0xb",null]}`, false},
		{"case-insensitive topic compare", `{"topics":["0xA"]}`, true},
		{"address single match", `{"address":"0xADDR"}`, true},
		{"address OR-list match", `{"address":["0xother","0xaddr"]}`, true},
		{"address mismatch", `{"address":"0xother"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := filt(t, tc.filter)
			require.NotNil(t, f)
			assert.Equal(t, tc.match, f.Matches(logAB))
		})
	}
}

func TestLogsFilter_ParseEdges(t *testing.T) {
	t.Run("concrete range", func(t *testing.T) {
		f := filt(t, `{"fromBlock":"0x10","toBlock":"0x12"}`)
		require.NotNil(t, f)
		from, to, ok := f.ConcreteRange()
		require.True(t, ok)
		assert.Equal(t, int64(0x10), from)
		assert.Equal(t, int64(0x12), to)
	})
	t.Run("tag range is non-concrete", func(t *testing.T) {
		f := filt(t, `{"fromBlock":"0x10","toBlock":"latest"}`)
		require.NotNil(t, f)
		_, _, ok := f.ConcreteRange()
		assert.False(t, ok)
	})
	t.Run("blockHash variant", func(t *testing.T) {
		f := filt(t, `{"blockHash":"0xBB"}`)
		require.NotNil(t, f)
		assert.Equal(t, "0xbb", f.BlockHash)
	})
	t.Run("non-string address entry → unparseable (nil)", func(t *testing.T) {
		assert.Nil(t, filt(t, `{"address":[1]}`))
	})
	t.Run("more than 4 topic positions → unparseable", func(t *testing.T) {
		assert.Nil(t, filt(t, `{"topics":[null,null,null,null,null]}`))
	})
	t.Run("no params → nil", func(t *testing.T) {
		assert.Nil(t, parseLogsFilter(nil))
	})
}

// --- the two getLogs checks ---------------------------------------------------

// histWithReceipts is a History + ReceiptCache + Resolver-compatible fixture.
type histWithReceipts struct {
	pins     map[int64]string
	receipts map[string][]Receipt
}

func (h histWithReceipts) HashAt(n int64) (string, bool) { v, ok := h.pins[n]; return v, ok }
func (h histWithReceipts) CachedReceipts(bh string) ([]Receipt, bool) {
	r, ok := h.receipts[bh]
	return r, ok
}

func validateGetLogs(t *testing.T, paramJSON, result string, cs CheckSet, hist History, r Resolver) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[` + paramJSON + `]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(result), nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	var params []any
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[`+paramJSON+`]`), &params))
	return Validate(context.Background(), Input{
		Method:   "eth_getLogs",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   cs,
		Params:   params,
		History:  hist,
		Resolver: r,
		Reorg:    ReorgPolicy{Finalized: BehaviorError, Unfinalized: BehaviorError},
	})
}

func TestCheck_GetLogsFilterSanity(t *testing.T) {
	cs := only("getLogsFilterSanity", nil)

	t.Run("matching logs pass", func(t *testing.T) {
		res := validateGetLogs(t, `{"address":"0xaddr","fromBlock":"0x10","toBlock":"0x11"}`,
			`[{"address":"0xaddr","topics":["0xa"],"blockNumber":"0x10","blockHash":"0xbb","logIndex":"0x0","transactionHash":"0xt1"}]`,
			cs, nil, nil)
		assert.NoError(t, res.Err)
	})

	t.Run("log not matching the filter address → reject", func(t *testing.T) {
		res := validateGetLogs(t, `{"address":"0xaddr"}`,
			`[{"address":"0xEVIL","topics":["0xa"],"blockNumber":"0x10","blockHash":"0xbb","logIndex":"0x0","transactionHash":"0xt1"}]`,
			cs, nil, nil)
		require.Error(t, res.Err)
		assert.True(t, common.HasErrorCode(res.Err, common.ErrCodeEndpointContentValidation))
	})

	t.Run("log outside the requested range → reject", func(t *testing.T) {
		res := validateGetLogs(t, `{"fromBlock":"0x10","toBlock":"0x11"}`,
			`[{"address":"0xaddr","topics":["0xa"],"blockNumber":"0x20","blockHash":"0xbb","logIndex":"0x0","transactionHash":"0xt1"}]`,
			cs, nil, nil)
		require.Error(t, res.Err)
	})

	t.Run("log from a different block than the requested blockHash → reject", func(t *testing.T) {
		res := validateGetLogs(t, `{"blockHash":"0xbb"}`,
			`[{"address":"0xaddr","topics":["0xa"],"blockNumber":"0x10","blockHash":"0xcc","logIndex":"0x0","transactionHash":"0xt1"}]`,
			cs, nil, nil)
		require.Error(t, res.Err)
	})

	t.Run("removed logs are exempt", func(t *testing.T) {
		res := validateGetLogs(t, `{"address":"0xaddr"}`,
			`[{"address":"0xother","removed":true,"topics":["0xa"],"blockNumber":"0x10","blockHash":"0xbb","logIndex":"0x0","transactionHash":"0xt1"}]`,
			cs, nil, nil)
		assert.NoError(t, res.Err)
	})

	t.Run("unparseable filter → skip", func(t *testing.T) {
		res := validateGetLogs(t, `{"address":[1]}`,
			`[{"address":"0xEVIL","topics":["0xa"],"blockNumber":"0x10","blockHash":"0xbb","logIndex":"0x0","transactionHash":"0xt1"}]`,
			cs, nil, nil)
		assert.NoError(t, res.Err)
	})
}

func TestCheck_GetLogsCompleteness(t *testing.T) {
	cs := only("getLogsCompleteness", nil)
	// Canonical block 0xbb (number 0x10) has two filter-matching logs (0x0, 0x1)
	// for address 0xaddr, plus one non-matching log (0x2, other address).
	canonical := map[string][]Receipt{
		"0xbb": {{
			BlockHash:       "0xbb",
			TransactionHash: "0xt1",
			Logs: []Log{
				{Address: "0xaddr", Topics: []string{"0xa"}, Data: "0xd1", BlockHash: "0xbb", BlockNumber: "0x10", TransactionHash: "0xt1", LogIndex: "0x0"},
				{Address: "0xaddr", Topics: []string{"0xa"}, Data: "0xd2", BlockHash: "0xbb", BlockNumber: "0x10", TransactionHash: "0xt1", LogIndex: "0x1"},
				{Address: "0xother", Topics: []string{"0xz"}, Data: "0xd3", BlockHash: "0xbb", BlockNumber: "0x10", TransactionHash: "0xt1", LogIndex: "0x2"},
			},
		}},
	}
	hist := histWithReceipts{pins: map[int64]string{0x10: "0xbb"}, receipts: canonical}
	filter := `{"address":"0xaddr","fromBlock":"0x10","toBlock":"0x10"}`
	logJSON := func(idx, data string) string {
		return `{"address":"0xaddr","topics":["0xa"],"data":"` + data + `","blockNumber":"0x10","blockHash":"0xbb","transactionHash":"0xt1","logIndex":"` + idx + `"}`
	}

	t.Run("complete response passes", func(t *testing.T) {
		res := validateGetLogs(t, filter, `[`+logJSON("0x0", "0xd1")+`,`+logJSON("0x1", "0xd2")+`]`, cs, hist, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
	})

	t.Run("dropped log → reject (THE missing-logs catch)", func(t *testing.T) {
		res := validateGetLogs(t, filter, `[`+logJSON("0x0", "0xd1")+`]`, cs, hist, mockResolver{finalized: true, known: true})
		require.Error(t, res.Err)
		assert.Contains(t, res.Err.Error(), "missing 1 filter-matching canonical logs")
	})

	t.Run("fabricated extra log → reject", func(t *testing.T) {
		res := validateGetLogs(t, filter, `[`+logJSON("0x0", "0xd1")+`,`+logJSON("0x1", "0xd2")+`,`+logJSON("0x7", "0xfe")+`]`, cs, hist, mockResolver{finalized: true, known: true})
		require.Error(t, res.Err)
		assert.Contains(t, res.Err.Error(), "not among the block's")
	})

	t.Run("corrupted log data → reject", func(t *testing.T) {
		res := validateGetLogs(t, filter, `[`+logJSON("0x0", "0xd1")+`,`+logJSON("0x1", "0xBAD")+`]`, cs, hist, mockResolver{finalized: true, known: true})
		require.Error(t, res.Err)
		assert.Contains(t, res.Err.Error(), "differs from the canonical")
	})

	t.Run("duplicated logIndex → reject", func(t *testing.T) {
		res := validateGetLogs(t, filter, `[`+logJSON("0x0", "0xd1")+`,`+logJSON("0x0", "0xd1")+`]`, cs, hist, mockResolver{finalized: true, known: true})
		require.Error(t, res.Err)
		assert.Contains(t, res.Err.Error(), "more than once")
	})

	t.Run("uncached block → skip (opportunistic)", func(t *testing.T) {
		cold := histWithReceipts{pins: map[int64]string{}, receipts: map[string][]Receipt{}}
		res := validateGetLogs(t, filter, `[`+logJSON("0x0", "0xd1")+`]`, cs, cold, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
	})

	t.Run("different fork hash in response → hash-anchored skip, no false reject", func(t *testing.T) {
		forkLog := `{"address":"0xaddr","topics":["0xa"],"data":"0xd1","blockNumber":"0x10","blockHash":"0xFORK","transactionHash":"0xt1","logIndex":"0x0"}`
		res := validateGetLogs(t, filter, `[`+forkLog+`]`, cs, hist, mockResolver{finalized: false, known: true})
		assert.NoError(t, res.Err, "logs from another fork must be skipped (no cached receipts for that hash), not rejected")
	})

	t.Run("no History/cache wired → no-op", func(t *testing.T) {
		res := validateGetLogs(t, filter, `[`+logJSON("0x0", "0xd1")+`]`, cs, nil, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
	})
}

func TestCheck_GetLogsCompleteness_AbsentBlock(t *testing.T) {
	cs := only("getLogsCompleteness", nil)
	// Range [0x10, 0x11]. Block 0x10 (0xb0) has one matching log; block 0x11
	// (0xb1) has one matching log too. Response only covers 0x10 → block 0x11's
	// logs were dropped wholesale.
	hist := histWithReceipts{
		pins: map[int64]string{0x10: "0xb0", 0x11: "0xb1"},
		receipts: map[string][]Receipt{
			"0xb0": {{BlockHash: "0xb0", TransactionHash: "0xt0", Logs: []Log{
				{Address: "0xaddr", Topics: []string{"0xa"}, Data: "0xd0", BlockHash: "0xb0", BlockNumber: "0x10", TransactionHash: "0xt0", LogIndex: "0x0"},
			}}},
			"0xb1": {{BlockHash: "0xb1", TransactionHash: "0xt9", Logs: []Log{
				{Address: "0xaddr", Topics: []string{"0xa"}, Data: "0xd9", BlockHash: "0xb1", BlockNumber: "0x11", TransactionHash: "0xt9", LogIndex: "0x0"},
			}}},
		},
	}
	filter := `{"address":"0xaddr","fromBlock":"0x10","toBlock":"0x11"}`
	resp := `[{"address":"0xaddr","topics":["0xa"],"data":"0xd0","blockNumber":"0x10","blockHash":"0xb0","transactionHash":"0xt0","logIndex":"0x0"}]`

	t.Run("finalized absent block with matching canonical logs → reject", func(t *testing.T) {
		res := validateGetLogs(t, filter, resp, cs, hist, mockResolver{finalized: true, known: true})
		require.Error(t, res.Err)
		assert.Contains(t, res.Err.Error(), "no logs for finalized block 17")
	})

	t.Run("unfinalized absent block → skip (reorg safety)", func(t *testing.T) {
		res := validateGetLogs(t, filter, resp, cs, hist, mockResolver{finalized: false, known: true})
		assert.NoError(t, res.Err)
	})

	t.Run("absent block whose canonical has no matching logs → pass", func(t *testing.T) {
		empt := histWithReceipts{
			pins: hist.pins,
			receipts: map[string][]Receipt{
				"0xb0": hist.receipts["0xb0"],
				"0xb1": {{BlockHash: "0xb1", TransactionHash: "0xt9", Logs: []Log{
					{Address: "0xother", Topics: []string{"0xz"}, BlockHash: "0xb1", BlockNumber: "0x11", TransactionHash: "0xt9", LogIndex: "0x0"},
				}}},
			},
		}
		res := validateGetLogs(t, filter, resp, cs, empt, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
	})

	t.Run("stale pin on the absent block disputes the pin (reconfirm applies)", func(t *testing.T) {
		// The engine's corroborate-before-verdict must kick in: DisputedPin set.
		// With a plain (non-reconfirming) History the strict verdict stands —
		// verifying the violation carries the dispute is covered by the reconfirm
		// suite; here we just assert the reject persists without a reconfirmer.
		res := validateGetLogs(t, filter, resp, cs, hist, mockResolver{finalized: true, known: true})
		require.Error(t, res.Err)
	})
}

// The everything-dropped shape: an empty [] response over a finalized range
// whose cached canonical receipts contain filter-matching logs must reject —
// previously the engine's emptyish gate made this invisible to all checks.
func TestCheck_GetLogsCompleteness_EmptyResponse(t *testing.T) {
	cs := only("getLogsCompleteness", nil)
	warm := histWithReceipts{
		pins: map[int64]string{0x10: "0xb0"},
		receipts: map[string][]Receipt{
			"0xb0": {{BlockHash: "0xb0", TransactionHash: "0xt0", Logs: []Log{
				{Address: "0xaddr", Topics: []string{"0xa"}, Data: "0xd0", BlockHash: "0xb0", BlockNumber: "0x10", TransactionHash: "0xt0", LogIndex: "0x0"},
			}}},
		},
	}
	filter := `{"address":"0xaddr","fromBlock":"0x10","toBlock":"0x10"}`

	t.Run("empty response, finalized block has matching canonical logs → reject", func(t *testing.T) {
		res := validateGetLogs(t, filter, `[]`, cs, warm, mockResolver{finalized: true, known: true})
		require.Error(t, res.Err)
		assert.Contains(t, res.Err.Error(), "no logs for finalized block")
	})

	t.Run("empty response, canonical has no matching logs → pass (verified empty)", func(t *testing.T) {
		none := histWithReceipts{
			pins: warm.pins,
			receipts: map[string][]Receipt{
				"0xb0": {{BlockHash: "0xb0", TransactionHash: "0xt0", Logs: []Log{
					{Address: "0xother", Topics: []string{"0xz"}, BlockHash: "0xb0", BlockNumber: "0x10", TransactionHash: "0xt0", LogIndex: "0x0"},
				}}},
			},
		}
		res := validateGetLogs(t, filter, `[]`, cs, none, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "getLogsCompleteness"))
	})

	t.Run("empty response, unfinalized → skip (reorg safety)", func(t *testing.T) {
		res := validateGetLogs(t, filter, `[]`, cs, warm, mockResolver{finalized: false, known: true})
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "getLogsCompleteness"))
	})

	t.Run("empty response, cold cache → skip", func(t *testing.T) {
		cold := histWithReceipts{pins: map[int64]string{}, receipts: map[string][]Receipt{}}
		res := validateGetLogs(t, filter, `[]`, cs, cold, mockResolver{finalized: true, known: true})
		assert.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "getLogsCompleteness"))
	})

	t.Run("filterSanity does NOT run on an empty response (no outcome)", func(t *testing.T) {
		both := CheckSet{}.Enable("getLogsFilterSanity", nil).Enable("getLogsCompleteness", nil)
		res := validateGetLogs(t, filter, `[]`, both, warm, mockResolver{finalized: true, known: true})
		assert.Equal(t, "", outcomeOf(res, "getLogsFilterSanity"), "non-opted-in checks keep the emptyish short-circuit")
	})

	t.Run("empty response on a non-getLogs method still short-circuits entirely", func(t *testing.T) {
		res := validateBlockPolicy(t, []byte(`null`), only("hashStability", nil), mockHistory{0x10: "0xbb"}, rejectAll)
		assert.NoError(t, res.Err)
		assert.Empty(t, res.Outcomes)
	})
}
