package erpc

import (
	"context"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
)

// runLatencyLoadTest drives the given network with `concurrency`
// goroutines firing eth_getBalance calls for `duration`, then reports
// throughput, p50 / p95 / p99 / max latency, and the peak heap delta.
func runLatencyLoadTest(b *testing.B, bn *benchNetwork, concurrency int, duration time.Duration) {
	body := benchRequestBody()

	// Warm-up to amortize lazy-init allocs.
	for i := 0; i < 100; i++ {
		req := common.NewNormalizedRequest(body)
		resp, _ := bn.ntw.Forward(context.Background(), req)
		if resp != nil {
			resp.Release()
		}
	}
	runtime.GC()
	var heapBefore runtime.MemStats
	runtime.ReadMemStats(&heapBefore)

	// Pre-allocate a wide latencies slice (50k slots is enough for ~10k RPS × 5s).
	const maxLatencies = 1 << 20 // 1Mi samples
	latencies := make([]int64, maxLatencies)
	var nextSlot atomic.Int64

	deadline := time.Now().Add(duration)

	var wg sync.WaitGroup
	var ops atomic.Int64
	var errs atomic.Int64

	b.ResetTimer()
	t0 := time.Now()

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if time.Now().After(deadline) {
					return
				}
				req := common.NewNormalizedRequest(body)
				start := time.Now()
				resp, err := bn.ntw.Forward(context.Background(), req)
				lat := time.Since(start).Nanoseconds()
				if err != nil {
					errs.Add(1)
				}
				if resp != nil {
					resp.Release()
				}
				ops.Add(1)
				if slot := nextSlot.Add(1) - 1; slot < int64(maxLatencies) {
					latencies[slot] = lat
				}
			}
		}()
	}
	wg.Wait()
	wall := time.Since(t0)
	b.StopTimer()

	runtime.GC()
	var heapAfter runtime.MemStats
	runtime.ReadMemStats(&heapAfter)

	total := int(nextSlot.Load())
	if total > maxLatencies {
		total = maxLatencies
	}
	captured := latencies[:total]
	sort.Slice(captured, func(i, j int) bool { return captured[i] < captured[j] })

	p := func(q float64) int64 {
		if len(captured) == 0 {
			return 0
		}
		idx := int(float64(len(captured)-1) * q)
		return captured[idx]
	}

	throughput := float64(ops.Load()) / wall.Seconds()
	errRate := 0.0
	if ops.Load() > 0 {
		errRate = 100.0 * float64(errs.Load()) / float64(ops.Load())
	}
	heapDelta := int64(heapAfter.HeapAlloc) - int64(heapBefore.HeapAlloc)

	b.ReportMetric(throughput, "req/s")
	b.ReportMetric(float64(p(0.50))/1000.0, "p50-µs")
	b.ReportMetric(float64(p(0.95))/1000.0, "p95-µs")
	b.ReportMetric(float64(p(0.99))/1000.0, "p99-µs")
	b.ReportMetric(float64(p(0.999))/1000.0, "p999-µs")
	if total > 0 {
		b.ReportMetric(float64(captured[total-1])/1000.0, "max-µs")
	}
	b.ReportMetric(errRate, "err%%")
	b.ReportMetric(float64(heapDelta)/1024.0, "heap-Δ-KiB")
}

// ============================================================================
// HTTP-layer concurrent load benchmarks. These run the executor under
// realistic concurrent pressure (`b.benchtime` of wall-clock, not iter
// count) and report percentile latencies + sustained throughput.
//
// Run with: go test -run=^$ -bench=BenchmarkLoad_ -benchtime=10s ./erpc
// ============================================================================

// 30s, 256-way concurrency against the typical realtime-read combo.
func BenchmarkLoad_Defi_Realtime_256w(b *testing.B) {
	mocks := []*benchMockUpstream{
		newBenchUpstream(benchMethod, nil),
		newBenchUpstream(benchMethod, nil),
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(2 * time.Second)},
		Retry:   &common.RetryPolicyConfig{MaxAttempts: 2, Delay: common.Duration(50 * time.Millisecond)},
		Hedge: &common.HedgePolicyConfig{
			Delay:    common.NewStaticDurationSpec(50 * time.Millisecond),
			MaxCount: 1,
		},
	}}, mocks)
	defer bn.Close()
	runLatencyLoadTest(b, bn, 256, 5*time.Second)
}

// 30s, 256-way concurrency against the consensus-with-retry combo.
func BenchmarkLoad_Consensus_3of5_256w(b *testing.B) {
	mocks := make([]*benchMockUpstream, 5)
	for i := range mocks {
		mocks[i] = newBenchUpstream(benchMethod, nil)
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(10 * time.Second)},
		Retry:   &common.RetryPolicyConfig{MaxAttempts: 2, Delay: common.Duration(0)},
		Consensus: &common.ConsensusPolicyConfig{
			MaxParticipants:    5,
			AgreementThreshold: 3,
		},
	}}, mocks)
	defer bn.Close()
	runLatencyLoadTest(b, bn, 256, 5*time.Second)
}

// 30s, 256-way concurrency against an UNRELIABLE primary — exercises
// retry rotation + hedge fan-out under sustained pressure.
func BenchmarkLoad_FailoverPrimary_256w(b *testing.B) {
	var counter atomic.Int64
	mocks := []*benchMockUpstream{
		newBenchUpstream(benchMethod, func(_ int64) (int, string, time.Duration) {
			// Deterministic: every 3rd request fails.
			if counter.Add(1)%3 == 0 {
				return 500, "", 0
			}
			return 0, "", 0
		}),
		newBenchUpstream(benchMethod, nil),
	}
	bn := setupBenchNetwork(b, []*common.FailsafeConfig{{
		Timeout: &common.TimeoutPolicyConfig{Duration: common.NewStaticDurationSpec(3 * time.Second)},
		Retry:   &common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(20 * time.Millisecond)},
		Hedge: &common.HedgePolicyConfig{
			Delay:    common.NewStaticDurationSpec(100 * time.Millisecond),
			MaxCount: 1,
		},
	}}, mocks)
	defer bn.Close()
	runLatencyLoadTest(b, bn, 256, 5*time.Second)
}
