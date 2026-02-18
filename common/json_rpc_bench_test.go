package common

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"testing"
)

func benchEnvMB(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func benchMakeLargeJsonRpcResponse(resultBytes int) []byte {
	// Keep structure stable; scale only "result" size.
	// {"jsonrpc":"2.0","id":1,"result":"aaaa..."}
	prefix := []byte(`{"jsonrpc":"2.0","id":1,"result":"`)
	suffix := []byte(`"}`)
	out := make([]byte, 0, len(prefix)+resultBytes+len(suffix))
	out = append(out, prefix...)
	out = append(out, bytes.Repeat([]byte{'a'}, resultBytes)...)
	out = append(out, suffix...)
	return out
}

func BenchmarkJsonRpcResponse_ParseFromStream_LargeResult(b *testing.B) {
	mb := benchEnvMB("ERPC_BENCH_RESULT_MB", 16)
	payload := benchMakeLargeJsonRpcResponse(mb << 20)

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))

	var rr bytes.Reader
	for i := 0; i < b.N; i++ {
		rr.Reset(payload)
		var resp JsonRpcResponse
		if err := resp.ParseFromStream([]context.Context{context.Background()}, &rr, len(payload)); err != nil {
			b.Fatal(err)
		}
	}
}
