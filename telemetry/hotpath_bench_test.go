package telemetry

import (
	"fmt"
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	util.ConfigureTestLogger()
}

// Isolates the prometheus recording layer used on every Forward:
// LabeledCounter.WithLabelValues, CounterHandle (sync.Map + idle atomic),
// and histogram Observe. Steady labels measure the hit path; rotating
// labels force cache/vec misses (mutex + series create).
//
// Run:
//
//	go test -run=^$ -bench='Benchmark(CounterHandle|WithLabelValues|HistogramObserve)_' -benchmem -count=5 ./telemetry

func initHotpathMetrics(b *testing.B) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	if err := SetHistogramBuckets(""); err != nil {
		b.Fatalf("SetHistogramBuckets: %v", err)
	}
}

var (
	reqLabels = []string{
		"bench", "vendor", "evm:123", "up-1", "eth_getBalance",
		"1", "none", "unknown", "n/a", "unknown",
	}
	errLabels = []string{
		"bench", "vendor", "evm:123", "up-1", "eth_getBalance",
		"ErrEndpointServerSideException", "major", "none", "unknown", "n/a", "unknown",
	}
	durLabels = []string{
		"bench", "vendor", "evm:123", "up-1", "eth_getBalance", "none", "unknown", "n/a",
	}
)

func prewarmCounterHandles() {
	CounterHandle(MetricUpstreamRequestTotal, reqLabels...).Inc()
	CounterHandle(MetricUpstreamErrorTotal, errLabels...).Inc()
	MetricUpstreamRequestTotal.WithLabelValues(reqLabels...).Inc()
	MetricUpstreamErrorTotal.WithLabelValues(errLabels...).Inc()
	ObserverHandle(MetricUpstreamRequestDuration, durLabels...).Observe(0.001)
	MetricUpstreamRequestDuration.WithLabelValues(durLabels...).Observe(0.001)
}

func rotatingReqLabels(i int) []string {
	return []string{
		"bench", "vendor", "evm:123", "up-1",
		fmt.Sprintf("eth_method_%d", i%1024),
		"1", "none", "unknown", "n/a", "unknown",
	}
}

func BenchmarkCounterHandle_SteadyLabels_Serial(b *testing.B) {
	initHotpathMetrics(b)
	prewarmCounterHandles()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CounterHandle(MetricUpstreamRequestTotal, reqLabels...).Inc()
	}
}

func BenchmarkCounterHandle_SteadyLabels_Parallel(b *testing.B) {
	initHotpathMetrics(b)
	prewarmCounterHandles()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			CounterHandle(MetricUpstreamRequestTotal, reqLabels...).Inc()
		}
	})
}

func BenchmarkWithLabelValues_SteadyLabels_Serial(b *testing.B) {
	initHotpathMetrics(b)
	prewarmCounterHandles()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MetricUpstreamRequestTotal.WithLabelValues(reqLabels...).Inc()
	}
}

func BenchmarkWithLabelValues_SteadyLabels_Parallel(b *testing.B) {
	initHotpathMetrics(b)
	prewarmCounterHandles()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			MetricUpstreamRequestTotal.WithLabelValues(reqLabels...).Inc()
		}
	})
}

func BenchmarkCounterHandle_ErrorTotal_Steady_Parallel(b *testing.B) {
	initHotpathMetrics(b)
	prewarmCounterHandles()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			CounterHandle(MetricUpstreamErrorTotal, errLabels...).Inc()
		}
	})
}

func BenchmarkCounterHandle_RotatingLabels_Parallel(b *testing.B) {
	initHotpathMetrics(b)
	prewarmCounterHandles()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			CounterHandle(MetricUpstreamRequestTotal, rotatingReqLabels(i)...).Inc()
			i++
		}
	})
}

func BenchmarkHistogramObserve_CachedHandle_Parallel(b *testing.B) {
	initHotpathMetrics(b)
	prewarmCounterHandles()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ObserverHandle(MetricUpstreamRequestDuration, durLabels...).Observe(0.003)
		}
	})
}

func BenchmarkHistogramObserve_WithLabelValues_Parallel(b *testing.B) {
	initHotpathMetrics(b)
	prewarmCounterHandles()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			MetricUpstreamRequestDuration.WithLabelValues(durLabels...).Observe(0.003)
		}
	})
}
