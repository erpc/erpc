package erpc

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func init() {
	util.ConfigureTestLogger()
}

// These benches isolate Network.Forward's success and failure code paths
// (selection + tracker state + prometheus recording). They share the
// httptest fixture in failsafe_perf_bench_test.go so HTTP is in-process
// and parallel-safe. Failsafe is kept to a single attempt so retry/hedge
// do not dominate the measurement.
//
// Run:
//
//	go test -run=^$ -bench='BenchmarkForward_' -benchmem -count=5 ./erpc

func newBareSuccessNetwork(b *testing.B) *benchNetwork {
	b.Helper()
	mock := newBenchUpstream(benchMethod, nil)
	return setupBenchNetwork(b, nil, []*benchMockUpstream{mock})
}

func newBareFailureNetwork(b *testing.B) *benchNetwork {
	b.Helper()
	mock := newBenchUpstream(benchMethod, func(_ int64) (int, string, time.Duration) {
		return 503, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"unavailable"}}`, 0
	})
	return setupBenchNetwork(b, []*common.FailsafeConfig{{
		Retry: &common.RetryPolicyConfig{MaxAttempts: 1},
	}}, []*benchMockUpstream{mock})
}

func warmupForward(bn *benchNetwork, n int) {
	body := benchRequestBody()
	for i := 0; i < n; i++ {
		req := common.NewNormalizedRequest(body)
		resp, _ := bn.ntw.Forward(context.Background(), req)
		if resp != nil {
			resp.Release()
		}
	}
}

func runForwardSerial(b *testing.B, bn *benchNetwork) {
	body := benchRequestBody()
	warmupForward(bn, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := common.NewNormalizedRequest(body)
		resp, _ := bn.ntw.Forward(context.Background(), req)
		if resp != nil {
			resp.Release()
		}
	}
}

func runForwardParallel(b *testing.B, bn *benchNetwork) {
	body := benchRequestBody()
	warmupForward(bn, 50)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := common.NewNormalizedRequest(body)
			resp, _ := bn.ntw.Forward(context.Background(), req)
			if resp != nil {
				resp.Release()
			}
		}
	})
}

func runForwardHeap(b *testing.B, bn *benchNetwork) {
	const reqs = 200
	body := benchRequestBody()
	warmupForward(bn, 50)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < reqs; j++ {
			req := common.NewNormalizedRequest(body)
			resp, _ := bn.ntw.Forward(context.Background(), req)
			if resp != nil {
				resp.Release()
			}
		}
	}
	b.StopTimer()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	total := int64(b.N) * reqs
	b.ReportMetric(float64(after.Mallocs-before.Mallocs)/float64(total), "mallocs/req")
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/float64(total), "heap-delta-B/req")
}

func BenchmarkForward_Success_Serial(b *testing.B) {
	bn := newBareSuccessNetwork(b)
	defer bn.Close()
	runForwardSerial(b, bn)
}

func BenchmarkForward_Success_Parallel(b *testing.B) {
	bn := newBareSuccessNetwork(b)
	defer bn.Close()
	runForwardParallel(b, bn)
}

func BenchmarkForward_Failure_Serial(b *testing.B) {
	bn := newBareFailureNetwork(b)
	defer bn.Close()
	runForwardSerial(b, bn)
}

func BenchmarkForward_Failure_Parallel(b *testing.B) {
	bn := newBareFailureNetwork(b)
	defer bn.Close()
	runForwardParallel(b, bn)
}

func BenchmarkForward_Success_Heap(b *testing.B) {
	bn := newBareSuccessNetwork(b)
	defer bn.Close()
	runForwardHeap(b, bn)
}

func BenchmarkForward_Failure_Heap(b *testing.B) {
	bn := newBareFailureNetwork(b)
	defer bn.Close()
	runForwardHeap(b, bn)
}
