package util

import (
	"bytes"
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

func BenchmarkReadAll_Large(b *testing.B) {
	mb := benchEnvMB("ERPC_BENCH_READALL_MB", 16)
	payload := bytes.Repeat([]byte{'x'}, mb<<20)

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))

	var rr bytes.Reader
	for i := 0; i < b.N; i++ {
		rr.Reset(payload)
		data, ret, err := ReadAll(&rr, len(payload))
		if err != nil {
			b.Fatal(err)
		}
		if len(data) != len(payload) {
			b.Fatalf("unexpected len: got=%d want=%d", len(data), len(payload))
		}
		if ret != nil {
			ret()
		}
	}
}

func BenchmarkBufPool_CapAfterLargeReadAll(b *testing.B) {
	mb := benchEnvMB("ERPC_BENCH_BUFPOOL_MB", 16)
	payload := bytes.Repeat([]byte{'x'}, mb<<20)

	b.ReportAllocs()

	var sumCap int64
	var rr bytes.Reader
	for i := 0; i < b.N; i++ {
		rr.Reset(payload)
		_, ret, err := ReadAll(&rr, len(payload))
		if err != nil {
			b.Fatal(err)
		}
		if ret != nil {
			ret()
		}
		// Observe post-return pool behavior: large-buffer retention vs drop.
		buf := BorrowBuf()
		sumCap += int64(buf.Cap())
		ReturnBuf(buf)
	}

	b.ReportMetric(float64(sumCap)/float64(b.N), "pool_cap_bytes")
}
