package integrity

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// covChecksFind returns the registered check with the given id.
func covChecksFind(t *testing.T, id string) *Check {
	t.Helper()
	for _, c := range allChecks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q is not registered", id)
	return nil
}

// covChecksRun runs one check directly against a crafted payload, wiring the
// optional history/resolver the way the engine does.
func covChecksRun(t *testing.T, id, method, raw string, params []any, hist History, r Resolver, cfg CheckConfig) *Violation {
	t.Helper()
	d := newDecoded(method, []byte(raw))
	d.reqParams = params
	ctx := withHistory(withResolver(context.Background(), r), hist)
	return covChecksFind(t, id).Run(ctx, d, cfg)
}

func covChecksAssertFail(t *testing.T, v *Violation, contains string) {
	t.Helper()
	require.NotNil(t, v, "expected a violation")
	require.NotSame(t, Skipped, v, "expected a violation, got skip")
	assert.Contains(t, v.Reason, contains)
}

// covChecksPanicResolver crashes when asked for a canonical header, so the
// engine's panic containment can be observed.
type covChecksPanicResolver struct{}

func (covChecksPanicResolver) IsFinalized(ctx context.Context, bn int64) (bool, bool) {
	return true, true
}
func (covChecksPanicResolver) CanonicalReceipts(ctx context.Context, ref string, blockNumber int64) ([]Receipt, bool) {
	return nil, false
}
func (covChecksPanicResolver) CanonicalHeader(ctx context.Context, ref string) (*Header, bool) {
	panic("resolver exploded")
}

func TestCovChecks_IdentitySkipsAndRejects(t *testing.T) {
	h := reqHash
	cases := []struct {
		name, id, method, raw string
		params                []any
		hist                  History
		want                  string // "skip", "pass" or a reason substring
	}{
		{"byHash no params", "blockByHashIdentity", MethodGetBlockByHash, `{"hash":"` + h + `"}`, nil, nil, "skip"},
		{"byHash block without hash", "blockByHashIdentity", MethodGetBlockByHash, `{"number":"0x1"}`, []any{h}, nil, "skip"},
		{"byNumber no params", "blockByNumberIdentity", MethodGetBlockByNumber, `{"number":"0x1"}`, nil, nil, "skip"},
		{"byNumber overflowing param", "blockByNumberIdentity", MethodGetBlockByNumber, `{"number":"0x1"}`, []any{"0xffffffffffffffff"}, nil, "skip"},
		{"byNumber non-object result", "blockByNumberIdentity", MethodGetBlockByNumber, `"oops"`, []any{"0x1"}, nil, "skip"},
		{"byNumber unparseable response number", "blockByNumberIdentity", MethodGetBlockByNumber, `{"number":"0xzz"}`, []any{"0x1"}, nil, "skip"},
		{"byNumber mismatch", "blockByNumberIdentity", MethodGetBlockByNumber, `{"number":"0x2"}`, []any{"0x1"}, nil, "block 2 but block 1 was requested"},
		{"tx without hash", "txByHashIdentity", MethodGetTransactionByHash, `{"from":"0x1"}`, []any{h}, nil, "skip"},
		{"tx non-object", "txByHashIdentity", MethodGetTransactionByHash, `[1]`, []any{h}, nil, "skip"},
		{"receipt with non-hash param", "receiptIdentity", MethodGetTransactionReceipt, `{"transactionHash":"` + h + `"}`, []any{"0x10"}, nil, "skip"},
		{"receipt without tx hash", "receiptIdentity", MethodGetTransactionReceipt, `{"blockHash":"0xbb"}`, []any{h}, nil, "skip"},
		{"receipt wrong tx", "receiptIdentity", MethodGetTransactionReceipt, `{"transactionHash":"` + otherHash + `"}`, []any{h}, nil, "not the requested"},
		{"txPin no history", "txPinConsistency", MethodGetTransactionByHash, `{"hash":"` + h + `","blockHash":"0xbb","blockNumber":"0x10"}`, []any{h}, nil, "skip"},
		{"txPin bad number", "txPinConsistency", MethodGetTransactionByHash, `{"hash":"` + h + `","blockHash":"0xbb","blockNumber":"0xzz"}`, []any{h}, mockHistory{0x10: "0xbb"}, "skip"},
		{"txPin mismatch", "txPinConsistency", MethodGetTransactionByHash, `{"hash":"` + h + `","blockHash":"0xcc","blockNumber":"0x10"}`, []any{h}, mockHistory{0x10: "0xbb"}, "committed block is 0xbb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := covChecksRun(t, tc.id, tc.method, tc.raw, tc.params, tc.hist, nil, CheckConfig{Enabled: true})
			switch tc.want {
			case "skip":
				assert.Same(t, Skipped, v)
			case "pass":
				assert.Nil(t, v)
			default:
				covChecksAssertFail(t, v, tc.want)
			}
		})
	}

	t.Run("txPin mismatch disputes the committed height", func(t *testing.T) {
		v := covChecksRun(t, "txPinConsistency", MethodGetTransactionByHash,
			`{"hash":"`+h+`","blockHash":"0xcc","blockNumber":"0x10"}`, []any{h}, mockHistory{0x10: "0xbb"}, nil, CheckConfig{})
		require.NotNil(t, v)
		assert.Equal(t, int64(0x10), v.DisputedPin)
	})
}

func TestCovChecks_RecomputeSkipsUnmodeledData(t *testing.T) {
	cfg := CheckConfig{Enabled: true}

	t.Run("blockHashRecompute: no hash → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "blockHashRecompute", MethodGetBlockByNumber, `{"number":"0x1"}`, nil, nil, nil, cfg))
	})
	t.Run("blockHashRecompute: header missing required fields → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "blockHashRecompute", MethodGetBlockByNumber, `{"hash":"0xabc","number":"0x1"}`, nil, nil, nil, cfg))
	})

	t.Run("transactionsRootRecompute: no root → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "transactionsRootRecompute", MethodGetBlockByNumber, `{"number":"0x1"}`, nil, nil, nil, cfg))
	})
	t.Run("transactionsRootRecompute: empty tx list → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "transactionsRootRecompute", MethodGetBlockByNumber,
			`{"number":"0x1","transactionsRoot":"0x`+strings.Repeat("11", 32)+`","transactions":[]}`, nil, nil, nil, cfg))
	})
	t.Run("transactionsRootRecompute: undecodable tx object → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "transactionsRootRecompute", MethodGetBlockByNumber,
			`{"number":"0x1","transactionsRoot":"0x`+strings.Repeat("11", 32)+`","transactions":[{"foo":1}]}`, nil, nil, nil, cfg))
	})
	t.Run("transactionsRootRecompute: tx hash does not recompute → skip", func(t *testing.T) {
		raw, _, _ := signedTxJSON(t)
		raw = mutateField(t, raw, "hash", otherHash)
		body := `{"number":"0x1","transactionsRoot":"0x` + strings.Repeat("11", 32) + `","transactions":[` + string(raw) + `]}`
		assert.Same(t, Skipped, covChecksRun(t, "transactionsRootRecompute", MethodGetBlockByNumber, body, nil, nil, nil, cfg))
	})

	root := "0x" + strings.Repeat("ab", 32)
	withRoot := mockResolver{header: &Header{ReceiptsRoot: root}}
	t.Run("receiptsRootRecompute: result is not an array → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "receiptsRootRecompute", MethodGetBlockReceipts, `{"a":1}`, nil, nil, withRoot, cfg))
	})
	t.Run("receiptsRootRecompute: no block reference → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "receiptsRootRecompute", MethodGetBlockReceipts, `[{"status":"0x1"}]`, nil, nil, withRoot, cfg))
	})
	t.Run("receiptsRootRecompute: receipt missing required fields → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "receiptsRootRecompute", MethodGetBlockReceipts, `[{"blockHash":"0xbb"}]`, nil, nil, withRoot, cfg))
	})
}

func covChecksLog(idx, data string, topics ...string) Log {
	return Log{Address: "0xaddr", Topics: topics, Data: data, BlockHash: "0xbb", BlockNumber: "0x10", TransactionHash: "0xt1", LogIndex: idx}
}

func TestCovChecks_GetLogsCompletenessEdges(t *testing.T) {
	cfg := CheckConfig{Enabled: true}
	canon := map[string][]Receipt{"0xbb": {{BlockHash: "0xbb", Logs: []Log{covChecksLog("0x0", "0xd1", "0xa")}}}}
	hist := histWithReceipts{pins: map[int64]string{0x10: "0xbb"}, receipts: canon}
	respLog := `{"address":"0xaddr","topics":["0xa"],"data":"0xd1","blockNumber":"0x10","blockHash":"0xbb","transactionHash":"0xt1","logIndex":"0x0"}`
	params := func(filter string) []any {
		var p []any
		require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[`+filter+`]`), &p))
		return p
	}

	t.Run("no filter params → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "getLogsCompleteness", MethodGetLogs, `[`+respLog+`]`, nil, hist, nil, cfg))
	})
	t.Run("removed and hashless logs are ignored; nothing else to compare → skip", func(t *testing.T) {
		body := `[{"address":"0xaddr","removed":true,"blockHash":"0xbb","logIndex":"0x0"},{"address":"0xaddr","logIndex":"0x1"}]`
		assert.Same(t, Skipped, covChecksRun(t, "getLogsCompleteness", MethodGetLogs, body, params(`{"address":"0xaddr"}`), hist, nil, cfg))
	})
	t.Run("non-concrete range: a warm block verifies → pass", func(t *testing.T) {
		assert.Nil(t, covChecksRun(t, "getLogsCompleteness", MethodGetLogs, `[`+respLog+`]`, params(`{"address":"0xaddr"}`), hist, nil, cfg))
	})
	t.Run("concrete range but no resolver: present-block result stands", func(t *testing.T) {
		assert.Nil(t, covChecksRun(t, "getLogsCompleteness", MethodGetLogs, `[`+respLog+`]`,
			params(`{"address":"0xaddr","fromBlock":"0x10","toBlock":"0x11"}`), hist, nil, cfg))
	})
	t.Run("canonical log with malformed index is not comparable → skip", func(t *testing.T) {
		bad := histWithReceipts{pins: hist.pins, receipts: map[string][]Receipt{"0xbb": {{Logs: []Log{covChecksLog("0xzz", "0xd1", "0xa")}}}}}
		assert.Same(t, Skipped, covChecksRun(t, "getLogsCompleteness", MethodGetLogs, `[`+respLog+`]`, params(`{"address":"0xaddr"}`), bad, nil, cfg))
	})
	t.Run("absent finalized block whose receipts are cold → skip", func(t *testing.T) {
		pinsOnly := histWithReceipts{pins: map[int64]string{0x10: "0xbb", 0x11: "0xcold"}, receipts: canon}
		v := covChecksRun(t, "getLogsCompleteness", MethodGetLogs, `[`+respLog+`]`,
			params(`{"address":"0xaddr","fromBlock":"0x10","toBlock":"0x11"}`), pinsOnly, mockResolver{finalized: true, known: true}, cfg)
		assert.Nil(t, v, "the present block verified; the cold absent block is simply not judged")
	})
	t.Run("range at the scan cap skips the absent sweep", func(t *testing.T) {
		v := covChecksRun(t, "getLogsCompleteness", MethodGetLogs, `[]`,
			params(fmt.Sprintf(`{"address":"0xaddr","fromBlock":"0x0","toBlock":"0x%x"}`, absentBlockScanCap)), hist, mockResolver{finalized: true, known: true}, cfg)
		assert.Same(t, Skipped, v)
	})
}

func TestCovChecks_CompareBlockLogs(t *testing.T) {
	f := logfilter.Parse([]any{map[string]any{}})

	t.Run("malformed response logIndex → skip", func(t *testing.T) {
		got := covChecksLog("0xzz", "0xd1", "0xa")
		v := compareBlockLogs(f, "0xbb", []Receipt{{Logs: []Log{covChecksLog("0x0", "0xd1", "0xa")}}}, []*Log{&got})
		assert.Same(t, Skipped, v)
	})
	t.Run("differing topic value → reject", func(t *testing.T) {
		got := covChecksLog("0x0", "0xd1", "0xa", "0xb")
		v := compareBlockLogs(f, "0xbb", []Receipt{{Logs: []Log{covChecksLog("0x0", "0xd1", "0xa", "0xc")}}}, []*Log{&got})
		covChecksAssertFail(t, v, "topic 1 differs from canonical")
	})
	t.Run("many missing logs are listed sorted and truncated", func(t *testing.T) {
		var logs []Log
		for i := 11; i >= 0; i-- {
			logs = append(logs, covChecksLog(fmt.Sprintf("0x%x", i), "0xd", "0xa"))
		}
		got := covChecksLog("0x0", "0xd", "0xa")
		v := compareBlockLogs(f, "0xbb", []Receipt{{Logs: logs}}, []*Log{&got})
		covChecksAssertFail(t, v, "missing 11 filter-matching canonical logs (logIndexes 1, 2, 3, 4, 5, 6, 7, 8, … +3 more)")
	})
	t.Run("fmtIndexes", func(t *testing.T) {
		assert.Equal(t, "", fmtIndexes(nil))
		assert.Equal(t, "7", fmtIndexes([]int64{7}))
		assert.Equal(t, "1, 2", fmtIndexes([]int64{1, 2}))
	})
}

func TestCovChecks_ReceiptStructuralChecks(t *testing.T) {
	cfg := CheckConfig{Enabled: true}
	addr := "0x27b26e88f007ec9109648c6da522fcaba06c74d7"
	cases := []struct {
		name, id, raw, want string
	}{
		{"sameBlockHash ignores receipts without a hash", "sameBlockHash", `[{"blockHash":""},{"blockHash":"0xaa"},{"blockHash":"0xAA"}]`, ""},
		{"sameBlockHash mixed", "sameBlockHash", `[{"blockHash":"0xaa"},{"blockHash":""},{"blockHash":"0xbb"}]`, "receipts span multiple blocks"},
		{"txIndex absent is tolerated", "transactionIndexConsistency", `[{"transactionIndex":""},{"transactionIndex":"0x1"}]`, ""},
		{"txIndex invalid hex", "transactionIndexConsistency", `[{"transactionIndex":"0xzz"}]`, "invalid transactionIndex hex at receipt 0"},
		{"logMetadata blockNumber mismatch", "logMetadata", `[{"blockHash":"0xaa","blockNumber":"0x10","logs":[{"blockHash":"0xaa","blockNumber":"0x11"}]}]`, "blockNumber mismatch"},
		{"logMetadata unparseable blockNumber", "logMetadata", `[{"blockHash":"0xaa","blockNumber":"0x10","logs":[{"blockHash":"0xaa","blockNumber":"0xzz"}]}]`, "blockNumber mismatch"},
		{"logMetadata transactionIndex mismatch", "logMetadata", `[{"transactionHash":"0x1","transactionIndex":"0x0","logs":[{"transactionHash":"0x1","transactionIndex":"0x1"}]}]`, "transactionIndex mismatch"},
		{"bloomMatch invalid log address", "bloomMatch", `[{"logsBloom":"0x00","logs":[{"address":"0xzz"}]}]`, "invalid log field for bloom recompute"},
		{"bloomMatch invalid topic", "bloomMatch", `[{"logsBloom":"0x00","logs":[{"address":"` + addr + `","topics":["0xqq"]}]}]`, "invalid log field for bloom recompute"},
		{"bloomMatch invalid bloom hex", "bloomMatch", `[{"logsBloom":"0xzz","logs":[{"address":"` + addr + `"}]}]`, "invalid logsBloom hex"},
		{"logIndexContiguity invalid hex", "logIndexContiguity", `[{"logs":[{"logIndex":"zz"}]}]`, "invalid logIndex hex at log 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := covChecksRun(t, tc.id, MethodGetBlockReceipts, tc.raw, nil, nil, nil, cfg)
			if tc.want == "" {
				assert.Nil(t, v)
				return
			}
			covChecksAssertFail(t, v, tc.want)
		})
	}

	t.Run("indexMagnitude flags an underflowed log transactionIndex", func(t *testing.T) {
		v := covChecksRun(t, "indexMagnitude", MethodGetLogs, `[{"logIndex":"0x1","transactionIndex":"0xfffffff7"}]`, nil, nil, nil, cfg)
		covChecksAssertFail(t, v, `log transactionIndex "0xfffffff7" exceeds`)
	})
}

func TestCovChecks_ConsecutiveSkips(t *testing.T) {
	cfg := CheckConfig{Enabled: true}
	segWith := func(parent *Header) History {
		return &segmentHistory{from: 100, to: 101, headers: map[int64]*Header{100: parent}}
	}
	full := &Header{Number: "0x64", Hash: "0xparent", BaseFeePerGas: "0x3b9aca00", GasLimit: "0x1c9c380", GasUsed: "0xe4e1c0", Timestamp: "0x10"}

	cases := []struct {
		name, id, raw string
		hist          History
	}{
		{"baseFee: genesis number", "baseFeeDerivation", `{"number":"0x0","baseFeePerGas":"0x1"}`, segWith(full)},
		{"baseFee: parent without gas fields", "baseFeeDerivation", `{"number":"0x65","parentHash":"0xparent","baseFeePerGas":"0x1"}`, segWith(&Header{Number: "0x64", Hash: "0xparent", BaseFeePerGas: "0x1"})},
		{"baseFee: parent zero gas limit", "baseFeeDerivation", `{"number":"0x65","parentHash":"0xparent","baseFeePerGas":"0x1"}`, segWith(&Header{Number: "0x64", Hash: "0xparent", BaseFeePerGas: "0x1", GasLimit: "0x0", GasUsed: "0x0"})},
		{"baseFee: unparseable child fee", "baseFeeDerivation", `{"number":"0x65","parentHash":"0xparent","baseFeePerGas":"0xzz"}`, segWith(full)},
		{"timestamp: absent", "timestampMonotonicity", `{"number":"0x65","parentHash":"0xparent"}`, segWith(full)},
		{"timestamp: unparseable", "timestampMonotonicity", `{"number":"0x65","parentHash":"0xparent","timestamp":"0xzz"}`, segWith(full)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Same(t, Skipped, covChecksRun(t, tc.id, MethodGetBlockByNumber, tc.raw, nil, tc.hist, nil, cfg))
		})
	}

	t.Run("fee model params override the mainnet defaults", func(t *testing.T) {
		m := feeModelFrom(CheckConfig{Params: map[string]string{"elasticity": "6", "denominator": "250", "minBaseFee": "7"}})
		assert.Equal(t, EIP1559Model{Derivable: true, Elasticity: 6, Denominator: 250, MinBaseFee: 7}, m)
		bad := feeModelFrom(CheckConfig{Params: map[string]string{"elasticity": "-1", "denominator": "x", "minBaseFee": "0"}})
		assert.Equal(t, EIP1559Model{Derivable: true, Elasticity: 2, Denominator: 8}, bad, "invalid params fall back")
	})

	t.Run("configured constants drive the verdict", func(t *testing.T) {
		parent := &Header{Number: "0x64", Hash: "0xparent", BaseFeePerGas: "0x3b9aca00", GasLimit: "0x1c9c380", GasUsed: "0x1c9c380"}
		opCfg := CheckConfig{Enabled: true, Params: map[string]string{"elasticity": "6", "denominator": "250"}}
		want, ok := NextBaseFee(parent, feeModelFrom(opCfg))
		require.True(t, ok)
		// The parent is found by parentHash, never by number (spec §12).
		good := fmt.Sprintf(`{"number":"0x65","parentHash":"0xparent","baseFeePerGas":"0x%x"}`, want)
		assert.Nil(t, covChecksRun(t, "baseFeeDerivation", MethodGetBlockByNumber, good, nil, segWith(parent), nil, opCfg))
		v := covChecksRun(t, "baseFeeDerivation", MethodGetBlockByNumber, good, nil, segWith(parent), nil, cfg)
		covChecksAssertFail(t, v, "elasticity 2 / denominator 8")
	})
}

func TestCovChecks_NextBaseFeeEdges(t *testing.T) {
	m := EIP1559Model{Derivable: true, Elasticity: 2, Denominator: 8}
	t.Run("unparseable parent", func(t *testing.T) {
		_, ok := NextBaseFee(&Header{BaseFeePerGas: "", GasLimit: "0x10", GasUsed: "0x1"}, m)
		assert.False(t, ok)
		_, ok = NextBaseFee(&Header{BaseFeePerGas: "0xzz", GasLimit: "0x10", GasUsed: "0x1"}, m)
		assert.False(t, ok)
	})
	t.Run("invalid model constants", func(t *testing.T) {
		_, ok := NextBaseFee(&Header{BaseFeePerGas: "0x10", GasLimit: "0x10", GasUsed: "0x1"}, EIP1559Model{Elasticity: 0, Denominator: 8})
		assert.False(t, ok)
	})
	t.Run("gas target rounds to zero", func(t *testing.T) {
		_, ok := NextBaseFee(&Header{BaseFeePerGas: "0x10", GasLimit: "0x1", GasUsed: "0x1"}, m)
		assert.False(t, ok)
	})
	t.Run("over target always raises by at least one wei", func(t *testing.T) {
		// target 50, used 51, fee 1: the proportional delta rounds to 0.
		got, ok := NextBaseFee(&Header{BaseFeePerGas: "0x1", GasLimit: "0x64", GasUsed: "0x33"}, m)
		require.True(t, ok)
		assert.Equal(t, "2", got.String())
	})
	t.Run("empty parent with denominator 1 drains the fee to zero", func(t *testing.T) {
		got, ok := NextBaseFee(&Header{BaseFeePerGas: "0x64", GasLimit: "0x64", GasUsed: "0x0"}, EIP1559Model{Elasticity: 2, Denominator: 1})
		require.True(t, ok)
		assert.Equal(t, "0", got.String())
	})
	t.Run("hexToBig", func(t *testing.T) {
		_, ok := hexToBig("0x")
		assert.False(t, ok)
		_, ok = hexToBig("0xg1")
		assert.False(t, ok)
		v, ok := hexToBig("0X1f")
		require.True(t, ok)
		assert.Equal(t, int64(31), v.Int64())
	})
}

func TestCovChecks_TraceEdges(t *testing.T) {
	const n = int64(1000)
	cfg := CheckConfig{Enabled: true}

	t.Run("followed segment without a header at the height → skip", func(t *testing.T) {
		d := traceDecoded(traceResponse("0x10"), n)
		ctx := withHistory(context.Background(), &followedHeaders{from: n - 1, to: n + 1, headers: map[int64]*Header{}})
		assert.Same(t, Skipped, runTraceBlockGasReconciliation(ctx, d, cfg))
	})
	t.Run("header gasUsed unparseable → skip", func(t *testing.T) {
		d := traceDecoded(traceResponse("0x10"), n)
		assert.Same(t, Skipped, runTraceBlockGasReconciliation(segmentAt(n, "0xzz", 1), d, cfg))
	})
	t.Run("frame gasUsed unparseable → skip", func(t *testing.T) {
		d := traceDecoded(traceResponse("zz"), n)
		assert.Same(t, Skipped, runTraceBlockGasReconciliation(segmentAt(n, "0x10", 1), d, cfg))
	})
	t.Run("non-callTracer entries are not decoded as block traces", func(t *testing.T) {
		d := traceDecoded([]byte(`[{"txHash":"0x1","result":{"from":"0x1"}}]`), n)
		_, ok := d.BlockTraces()
		assert.False(t, ok)
		assert.Same(t, Skipped, runTraceBlockGasReconciliation(segmentAt(n, "0x10", 1), d, cfg))
	})
	t.Run("an empty trace list has no frames to judge → skip", func(t *testing.T) {
		d := traceDecoded([]byte(`[]`), n)
		assert.Same(t, Skipped, runTraceFrameShape(context.Background(), d, cfg))
	})
	t.Run("frame without from → reject", func(t *testing.T) {
		d := traceDecoded([]byte(`[{"txHash":"0x1","result":{"type":"CALL","from":"0x1","gasUsed":"0x1","calls":[{"type":"STATICCALL","gasUsed":"0x1"}]}}]`), n)
		covChecksAssertFail(t, runTraceFrameShape(context.Background(), d, cfg), "STATICCALL frame at depth 1 has no from address")
	})
	t.Run("a null frame is a violation", func(t *testing.T) {
		frames := 0
		covChecksAssertFail(t, walkTraceFrame(nil, 0, &frames), "null call frame")
	})
	t.Run("trees deeper than the bound stop descending without a verdict", func(t *testing.T) {
		// A bad leaf below maxTraceDepth is never read.
		leaf := CallFrame{Type: "CALL"}
		for range maxTraceDepth + 2 {
			leaf = CallFrame{Type: "CALL", From: "0x1", GasUsed: "0x0", Calls: []CallFrame{leaf}}
		}
		frames := 0
		assert.Nil(t, walkTraceFrame(&leaf, 0, &frames))
		assert.Equal(t, maxTraceDepth+1, frames)
	})
	t.Run("oversized trees skip", func(t *testing.T) {
		calls := make([]CallFrame, maxTraceFrames+1)
		for i := range calls {
			calls[i] = CallFrame{Type: "CALL", From: "0x1", GasUsed: "0x0"}
		}
		d := newDecoded(MethodTraceTransaction, nil)
		d.callTraceDone = true
		d.callTrace = &CallFrame{Type: "CALL", From: "0x1", GasUsed: "0x0", Calls: calls}
		d.blockTracesDone = true
		assert.Same(t, Skipped, runTraceFrameShape(context.Background(), d, cfg))
	})
}

func TestCovChecks_BlockAndContinuityEdges(t *testing.T) {
	cfg := CheckConfig{Enabled: true}
	for _, id := range []string{"transactionsRootConsistency", "headerFieldShapes", "txBlockInfo"} {
		t.Run(id+": non-object result → no verdict", func(t *testing.T) {
			assert.Nil(t, covChecksRun(t, id, MethodGetBlockByNumber, `"0xdead"`, nil, nil, nil, cfg))
		})
	}
	t.Run("headerConsensusInvariants: non-object result → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "headerConsensusInvariants", MethodGetBlockByNumber, `"0xdead"`, nil, nil, nil,
			CheckConfig{Enabled: true, Params: map[string]string{"zeroDifficulty": "true"}}))
	})
	t.Run("headerConsensusInvariants: a bare 0x difficulty is not zero", func(t *testing.T) {
		v := covChecksRun(t, "headerConsensusInvariants", MethodGetBlockByNumber, `{"number":"0x1","difficulty":"0x"}`, nil, nil, nil,
			CheckConfig{Enabled: true, Params: map[string]string{"zeroDifficulty": "true"}})
		covChecksAssertFail(t, v, "difficulty 0x is non-zero")
	})
	t.Run("txFieldUniqueness: hashless txs are ignored", func(t *testing.T) {
		h := "0x" + strings.Repeat("aa", 32)
		assert.Nil(t, covChecksRun(t, "txFieldUniqueness", MethodGetBlockByNumber,
			`{"number":"0x1","transactions":[{"from":"0x1"},{"hash":"`+h+`"},{"from":"0x2"}]}`, nil, nil, nil, cfg))
	})
	t.Run("txBlockInfo: block number mismatch", func(t *testing.T) {
		covChecksAssertFail(t, covChecksRun(t, "txBlockInfo", MethodGetBlockByNumber,
			`{"hash":"0xaa","number":"0x10","transactions":[{"blockHash":"0xaa","blockNumber":"0x11"}]}`, nil, nil, nil, cfg), "tx 0: block number mismatch")
	})
	t.Run("txBlockInfo: invalid transactionIndex", func(t *testing.T) {
		covChecksAssertFail(t, covChecksRun(t, "txBlockInfo", MethodGetBlockByNumber,
			`{"hash":"0xaa","number":"0x10","transactions":[{"blockHash":"0xaa","blockNumber":"0x10","transactionIndex":"0xzz"}]}`, nil, nil, nil, cfg), "invalid transactionIndex hex")
	})
	t.Run("schemaConformance ignores methods it does not model", func(t *testing.T) {
		assert.Nil(t, covChecksRun(t, "schemaConformance", MethodGetLogs, `{garbage`, nil, nil, nil, cfg))
	})

	hist := mockHistory{0x10: "0xaaa", 0x11: "0xbbb"}
	cases := []struct{ name, id, raw string }{
		{"linkage: no parentHash", "parentHashLinkage", `{"number":"0x11","hash":"0xbbb"}`},
		{"linkage: genesis", "parentHashLinkage", `{"number":"0x0","hash":"0xbbb","parentHash":"0xaaa"}`},
		{"stability: no hash", "hashStability", `{"number":"0x11","parentHash":"0xaaa"}`},
		{"stability: unparseable number", "hashStability", `{"number":"0xzz","hash":"0xbbb"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Same(t, Skipped, covChecksRun(t, tc.id, MethodGetBlockByNumber, tc.raw, nil, hist, nil, cfg))
		})
	}
}

func TestCovChecks_ReceiptVsBlockEdges(t *testing.T) {
	cfg := CheckConfig{Enabled: true}
	t.Run("non-object receipt → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "receiptVsBlock", MethodGetTransactionReceipt, `[1,2]`, nil, nil, mockResolver{}, cfg))
	})
	t.Run("pending receipt without block hash → skip", func(t *testing.T) {
		assert.Same(t, Skipped, covChecksRun(t, "receiptVsBlock", MethodGetTransactionReceipt, `{"transactionHash":"0xaa"}`, nil, nil, mockResolver{}, cfg))
	})
	t.Run("log count differs from canonical → reject", func(t *testing.T) {
		r := mockResolver{have: true, receipts: []Receipt{{TransactionHash: "0xaa", Logs: []Log{{LogIndex: "0x0"}, {LogIndex: "0x1"}}}}}
		v := covChecksRun(t, "receiptVsBlock", MethodGetTransactionReceipt,
			`{"blockHash":"0xbb","blockNumber":"0x10","transactionHash":"0xaa","logs":[{"logIndex":"0x0"}]}`, nil, nil, r, cfg)
		covChecksAssertFail(t, v, "receipt has 1 logs but canonical block has 2")
	})
}

func covChecksUnsignedTxJSON(t *testing.T, inner gethtypes.TxData, from string) string {
	t.Helper()
	b, err := gethtypes.NewTx(inner).MarshalJSON()
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	fb, _ := json.Marshal(from)
	m["from"] = fb
	out, err := json.Marshal(m)
	require.NoError(t, err)
	return string(out)
}

func TestCovChecks_SenderRecoveryEdges(t *testing.T) {
	cfg := CheckConfig{Enabled: true}
	to := gethcommon.HexToAddress("0x00000000000000000000000000000000000000aa")
	from := "0x1111111111111111111111111111111111111111"

	t.Run("non-object result is not judged", func(t *testing.T) {
		assert.Nil(t, covChecksRun(t, "senderRecovery", MethodGetTransactionByHash, `"0xdead"`, nil, nil, nil, cfg))
	})
	t.Run("protected tx with a zero chain id → skip", func(t *testing.T) {
		raw := covChecksUnsignedTxJSON(t, &gethtypes.DynamicFeeTx{
			ChainID: big.NewInt(0), Nonce: 1, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2), Gas: 21000, To: &to,
			Value: big.NewInt(1), V: big.NewInt(0), R: big.NewInt(1), S: big.NewInt(1),
		}, from)
		assert.Same(t, Skipped, covChecksRun(t, "senderRecovery", MethodGetTransactionByHash, raw, nil, nil, nil, cfg))
	})
	t.Run("unrecoverable signature is not judged", func(t *testing.T) {
		// s above secp256k1n/2 is rejected by the signer: no sender to compare.
		highS, _ := new(big.Int).SetString("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364140", 16)
		raw := covChecksUnsignedTxJSON(t, &gethtypes.DynamicFeeTx{
			ChainID: big.NewInt(1), Nonce: 1, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2), Gas: 21000, To: &to,
			Value: big.NewInt(1), V: big.NewInt(0), R: big.NewInt(1), S: highS,
		}, from)
		assert.Nil(t, covChecksRun(t, "senderRecovery", MethodGetTransactionByHash, raw, nil, nil, nil, cfg))
	})
}

func TestCovChecks_LogsFilterRejectsUnmodeledShapes(t *testing.T) {
	for _, p := range []string{
		`"0xnotanobject"`,
		`{"address":12}`,
		`{"topics":[["0xa",1]]}`,
		`{"topics":[12]}`,
		`{"topics":"0xa"}`,
		`{"topics":[null,null,null,null,null]}`,
	} {
		t.Run(p, func(t *testing.T) {
			var params []any
			require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[`+p+`]`), &params))
			assert.Nil(t, logfilter.Parse(params))
		})
	}
	t.Run("toBlock below fromBlock is not a concrete range", func(t *testing.T) {
		var params []any
		require.NoError(t, common.SonicCfg.Unmarshal([]byte(`[{"fromBlock":"0x12","toBlock":"0x10"}]`), &params))
		f := logfilter.Parse(params)
		require.NotNil(t, f)
		_, _, ok := f.ConcreteRange()
		assert.False(t, ok)
	})
}

func TestCovChecks_FieldHelpers(t *testing.T) {
	v := checkByteLen("block hash", "0xzz", hashLen)
	covChecksAssertFail(t, v, "block hash: invalid hex")
	assert.Nil(t, checkByteLen("block hash", "", hashLen))

	_, err := bloomFromLogs([]Log{{Address: "0xnothex"}})
	assert.Error(t, err)
	_, err = bloomFromLogs([]Log{{Topics: []string{"0xnothex"}}})
	assert.Error(t, err)
	_, err = bloomsEqual("0xnothex", make([]byte, bloomLen))
	assert.Error(t, err)
	eq, err := bloomsEqual("0x"+strings.Repeat("00", bloomLen), make([]byte, bloomLen))
	require.NoError(t, err)
	assert.True(t, eq)

	assert.False(t, isZeroHex("0x"))
	assert.True(t, isZeroHex("0x0000"))
}

func TestCovChecks_DecodedBlockLocators(t *testing.T) {
	cases := []struct {
		name, method, raw string
		ref               string
		number            int64
	}{
		{"header hash", MethodGetBlockByNumber, `{"hash":"0xh","number":"0x10"}`, "0xh", 16},
		{"header with unparseable number", MethodGetBlockByNumber, `{"hash":"0xh","number":"0xzz"}`, "0xh", -1},
		{"receipt block hash", MethodGetBlockReceipts, `[{"blockHash":"0xr","blockNumber":"0x11"}]`, "0xr", 17},
		{"receipt block number only", MethodGetTransactionReceipt, `{"blockNumber":"0x12"}`, "0x12", 18},
		{"log block hash", MethodGetLogs, `[{"blockHash":"0xl","blockNumber":"0x13"}]`, "0xl", 19},
		{"log block number only", MethodGetLogs, `[{"blockNumber":"0x14"}]`, "0x14", 20},
		{"nothing to locate", MethodGetLogs, `[]`, "", -1},
		{"unrelated method", MethodTraceTransaction, `{"type":"CALL"}`, "", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDecoded(tc.method, []byte(tc.raw))
			assert.Equal(t, tc.ref, d.BlockRef())
			assert.Equal(t, tc.number, d.BlockNumber())
		})
	}
	t.Run("block transactions of an undecodable block are empty", func(t *testing.T) {
		d := newDecoded(MethodGetBlockByHash, []byte(`"0xdead"`))
		assert.Empty(t, d.Transactions())
		assert.Empty(t, d.Transactions(), "memoized")
	})
}

func TestCovChecks_EngineContainment(t *testing.T) {
	t.Run("a panicking check yields a panic outcome and no verdict", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["0x10"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(`[{"blockHash":"0xbb","logs":[]}]`), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		res := Validate(context.Background(), Input{
			Method: "eth_getBlockReceipts", Upstream: common.NewFakeUpstream("u"), Response: rs,
			Checks: only("receiptsRootRecompute", nil), Resolver: covChecksPanicResolver{}, Reorg: DefaultReorgPolicy(),
		})
		assert.NoError(t, res.Err)
		assert.Equal(t, "panic", outcomeOf(res, "receiptsRootRecompute"))
	})
	t.Run("a response without a JSON-RPC body yields no outcomes", func(t *testing.T) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{}]}`))
		rs := common.NewNormalizedResponse().WithRequest(req)
		res := Validate(context.Background(), Input{
			Method: "eth_getLogs", Upstream: common.NewFakeUpstream("u"), Response: rs,
			Checks: only("getLogsCompleteness", nil), Reorg: DefaultReorgPolicy(),
		})
		assert.NoError(t, res.Err)
		assert.Empty(t, res.Outcomes)
	})
}

func TestCovChecks_ArchitectureLookup(t *testing.T) {
	a, ok := ArchitectureByName("arbitrum-nitro")
	require.True(t, ok)
	assert.Equal(t, ProfileFor(42161).Disable, a.Disable)
	_, ok = ArchitectureByName("no-such-architecture")
	assert.False(t, ok)
}
