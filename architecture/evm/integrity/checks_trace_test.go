package integrity

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// followedHeaders is a ChainSegment holding one verified height.
type followedHeaders struct {
	from, to int64
	headers  map[int64]*Header
}

func (f *followedHeaders) HashAt(n int64) (string, bool) {
	h, ok := f.headers[n]
	if !ok {
		return "", false
	}
	return h.Hash, true
}
func (f *followedHeaders) FollowedRange() (int64, int64, bool) { return f.from, f.to, true }
func (f *followedHeaders) HeaderAt(n int64) (*Header, bool) {
	h, ok := f.headers[n]
	return h, ok
}

// segmentAt builds a followed segment whose block at `number` used `gasUsed`
// gas across `ntx` transactions.
func segmentAt(number int64, gasUsed string, ntx int) context.Context {
	txs := make([]any, ntx)
	for i := range txs {
		txs[i] = fmt.Sprintf("0x%064x", i)
	}
	return withHistory(context.Background(), &followedHeaders{
		from: number - 10, to: number + 10,
		headers: map[int64]*Header{number: {
			Hash: "0xabc", Number: fmt.Sprintf("0x%x", number),
			GasUsed: gasUsed, RawTransactions: txs,
		}},
	})
}

func traceResponse(gasUsed ...string) []byte {
	var b strings.Builder
	b.WriteString("[")
	for i, g := range gasUsed {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"txHash":"0x%064x","result":{"type":"CALL","from":"0x1","to":"0x2","gas":"0xffff","gasUsed":"%s"}}`, i, g)
	}
	b.WriteString("]")
	return []byte(b.String())
}

func traceDecoded(raw []byte, number int64) *Decoded {
	d := newDecoded("debug_traceblockbynumber", raw)
	d.reqParams = []any{fmt.Sprintf("0x%x", number), map[string]any{"tracer": "callTracer"}}
	return d
}

// The block header commits the gas its transactions consumed. Traces claiming
// to describe that block must add up to it — measured exact on 88 consecutive
// blocks across 6 chains, so any deviation is real.
func TestTraceBlockGasReconciliation(t *testing.T) {
	const n = int64(1000)

	t.Run("traces that reconcile with the header pass", func(t *testing.T) {
		// 0x10 + 0x20 + 0x30 == 0x60
		d := traceDecoded(traceResponse("0x10", "0x20", "0x30"), n)
		v := runTraceBlockGasReconciliation(segmentAt(n, "0x60", 3), d, CheckConfig{})
		assert.Nil(t, v)
	})

	t.Run("a gas shortfall is a violation anchored to the pin", func(t *testing.T) {
		d := traceDecoded(traceResponse("0x10", "0x20", "0x10"), n)
		v := runTraceBlockGasReconciliation(segmentAt(n, "0x60", 3), d, CheckConfig{})
		require.NotNil(t, v)
		require.NotEqual(t, Skipped, v)
		assert.Contains(t, v.Reason, "missing gas")
		assert.EqualValues(t, n, v.DisputedPin,
			"a fork at this height looks identical to corruption until the pin is re-confirmed")
	})

	t.Run("dropping every trace for a non-empty block is caught", func(t *testing.T) {
		d := traceDecoded([]byte("[]"), n)
		v := runTraceBlockGasReconciliation(segmentAt(n, "0x60", 3), d, CheckConfig{})
		require.NotNil(t, v)
		require.NotEqual(t, Skipped, v)
		assert.Contains(t, v.Reason, "trace returned 0")
	})

	t.Run("an empty block reconciles at zero", func(t *testing.T) {
		d := traceDecoded([]byte("[]"), n)
		assert.Nil(t, runTraceBlockGasReconciliation(segmentAt(n, "0x0", 0), d, CheckConfig{}))
	})

	// Each of these is a reason the check cannot know the answer. None is
	// evidence of corruption, so each must skip rather than reject.
	t.Run("skips when it cannot know", func(t *testing.T) {
		t.Run("height outside the verified segment", func(t *testing.T) {
			d := traceDecoded(traceResponse("0x10"), 99999)
			assert.Equal(t, Skipped, runTraceBlockGasReconciliation(segmentAt(n, "0x60", 3), d, CheckConfig{}))
		})
		t.Run("no follower at all", func(t *testing.T) {
			d := traceDecoded(traceResponse("0x10"), n)
			assert.Equal(t, Skipped, runTraceBlockGasReconciliation(context.Background(), d, CheckConfig{}))
		})
		t.Run("a tag form names no height", func(t *testing.T) {
			d := newDecoded("debug_traceblockbynumber", traceResponse("0x10"))
			d.reqParams = []any{"latest", map[string]any{"tracer": "callTracer"}}
			assert.Equal(t, Skipped, runTraceBlockGasReconciliation(segmentAt(n, "0x60", 3), d, CheckConfig{}))
		})
		t.Run("a tracer this check does not model", func(t *testing.T) {
			// struct-logger output: no per-transaction call frames at all.
			d := traceDecoded([]byte(`{"gas":123,"failed":false,"structLogs":[]}`), n)
			assert.Equal(t, Skipped, runTraceBlockGasReconciliation(segmentAt(n, "0x60", 3), d, CheckConfig{}),
				"judging a prestate/structlog response against callTracer semantics would reject every one of them")
		})
	})
}

func TestTraceFrameShape(t *testing.T) {
	const n = int64(1000)

	t.Run("well-formed nested frames pass", func(t *testing.T) {
		raw := []byte(`[{"txHash":"0x1","result":{"type":"CALL","from":"0xa","to":"0xb","gasUsed":"0x10",
			"calls":[{"type":"STATICCALL","from":"0xb","to":"0xc","gasUsed":"0x8",
			"calls":[{"type":"DELEGATECALL","from":"0xc","to":"0xd","gasUsed":"0x4"}]}]}}]`)
		assert.Nil(t, runTraceFrameShape(context.Background(), traceDecoded(raw, n), CheckConfig{}))
	})

	t.Run("a garbled nested frame is caught", func(t *testing.T) {
		raw := []byte(`[{"txHash":"0x1","result":{"type":"CALL","from":"0xa","gasUsed":"0x10",
			"calls":[{"type":"","from":"0xb","gasUsed":"0x8"}]}}]`)
		v := runTraceFrameShape(context.Background(), traceDecoded(raw, n), CheckConfig{})
		require.NotNil(t, v)
		require.NotEqual(t, Skipped, v)
		assert.Contains(t, v.Reason, "no type")
	})

	t.Run("unusable gasUsed is caught", func(t *testing.T) {
		raw := []byte(`[{"txHash":"0x1","result":{"type":"CALL","from":"0xa","gasUsed":"banana"}}]`)
		v := runTraceFrameShape(context.Background(), traceDecoded(raw, n), CheckConfig{})
		require.NotNil(t, v)
		assert.Contains(t, v.Reason, "unusable gasUsed")
	})

	t.Run("a single-transaction trace is validated too", func(t *testing.T) {
		d := newDecoded("debug_tracetransaction", []byte(`{"type":"CALL","from":"0xa","gasUsed":"0x10"}`))
		assert.Nil(t, runTraceFrameShape(context.Background(), d, CheckConfig{}))
	})

	t.Run("a tracer this check does not model is skipped", func(t *testing.T) {
		d := newDecoded("debug_tracetransaction", []byte(`{"gas":1,"failed":false,"structLogs":[]}`))
		assert.Equal(t, Skipped, runTraceFrameShape(context.Background(), d, CheckConfig{}))
	})
}

// A regression guard for an invariant that LOOKS obviously true and is not.
// Measured on live traffic: sum(child gasUsed) exceeded the parent's on 75 of
// 12,400 honest frames, because gas refunds and gas returned by reverted
// sub-calls are netted at the parent. Encoding it would reject ~0.6% of real
// traces. This test fails if anyone adds it back.
func TestChildGasIsNotBoundedByParentGas(t *testing.T) {
	raw := []byte(`[{"txHash":"0x1","result":{"type":"CALL","from":"0xa","gasUsed":"0x10",
		"calls":[{"type":"CALL","from":"0xb","gasUsed":"0x40"}]}}]`)
	d := traceDecoded(raw, 1000)

	assert.Nil(t, runTraceFrameShape(context.Background(), d, CheckConfig{}),
		"children out-summing the parent is normal on real chains — refunds and reverted-call gas are netted at the parent")

	seg := segmentAt(1000, "0x10", 1)
	assert.Nil(t, runTraceBlockGasReconciliation(seg, traceDecoded(raw, 1000), CheckConfig{}),
		"reconciliation judges TOP-LEVEL gas against the header and must ignore the nesting entirely")
}

// Traced gas may legitimately run ABOVE the header on any chain, so only a
// shortfall is judged. Learned from live traffic, not the sweep: on Polygon
// block 91375456 all three vendors traced +341,453 over the header, and on
// 91374960 they disagreed with EACH OTHER (Alchemy exact, QuickNode and
// Chainstack +147,758). Receipts meter gas after EIP-3529 refunds while some
// clients report frame gas before them. Enforcing equality rejected honest
// responses from two independent vendors in production.
func TestTracedGasIsOnlyBoundedFromBelow(t *testing.T) {
	const n = int64(1000)

	t.Run("a sum above the header is honest", func(t *testing.T) {
		// the real shape: 174 traces summing over a header that nets refunds
		d := traceDecoded(traceResponse("0x30", "0xb033"), n)
		assert.Nil(t, runTraceBlockGasReconciliation(segmentAt(n, "0x30", 2), d, CheckConfig{}),
			"two independent vendors produced exactly this; it cannot be corruption")
	})

	t.Run("a shortfall is still caught", func(t *testing.T) {
		d := traceDecoded(traceResponse("0x10"), n)
		v := runTraceBlockGasReconciliation(segmentAt(n, "0x60", 1), d, CheckConfig{})
		require.NotNil(t, v)
		require.NotEqual(t, Skipped, v)
		assert.Contains(t, v.Reason, "missing gas")
	})

	t.Run("the transaction count stays exact", func(t *testing.T) {
		// count held on every sweep and on both live rejects (174/174, 122/122),
		// so it remains the strict half of the check.
		d := traceDecoded(traceResponse("0x10", "0x20"), n)
		v := runTraceBlockGasReconciliation(segmentAt(n, "0x30", 3), d, CheckConfig{})
		require.NotNil(t, v)
		assert.Contains(t, v.Reason, "transactions but the trace returned")
	})
}
