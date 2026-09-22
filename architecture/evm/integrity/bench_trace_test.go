package integrity

import (
	"context"
	"os"
	"testing"
)

// Real mainnet block trace (~1.5 MiB, 477 transactions) — the largest response
// shape these checks see, so the cost is measured on it rather than assumed.
func benchTraceBody(b *testing.B) []byte {
	body, err := os.ReadFile("testdata_trace_block.json")
	if err != nil {
		b.Skip("no captured trace body")
	}
	return body
}

func BenchmarkTraceBlockDecodeAndReconcile(b *testing.B) {
	body := benchTraceBody(b)
	ctx := segmentAt(1000, "0x0", 0)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d := traceDecoded(body, 1000)
		runTraceBlockGasReconciliation(ctx, d, CheckConfig{})
	}
}

func BenchmarkTraceFrameShapeFullWalk(b *testing.B) {
	body := benchTraceBody(b)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d := traceDecoded(body, 1000)
		runTraceFrameShape(context.Background(), d, CheckConfig{})
	}
}
