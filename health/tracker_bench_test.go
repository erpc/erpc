// tracker_bench_test.go
package health_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

var (
	testUpstreams = []common.Upstream{
		common.NewFakeUpstream("ups1"),
		common.NewFakeUpstream("ups2"),
		common.NewFakeUpstream("ups3"),
		common.NewFakeUpstream("ups4"),
	}
	testNetworks = []string{"eth", "polygon", "bsc", "arbitrum"}
	testMethods  = []string{"eth_call", "eth_getBalance", "eth_getBlockByNumber", "eth_getLogs"}
)

func init() {
	util.ConfigureTestLogger()
	telemetry.SetHistogramBuckets("0.001,0.005,0.01")
}

// Helper to get random test data
func getRandomTestData() (common.Upstream, string) {
	return testUpstreams[rand.Intn(len(testUpstreams))],
		testMethods[rand.Intn(len(testMethods))]
}

func BenchmarkRecordUpstreamRequest(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)
	ups, meth := getRandomTestData()

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.RecordUpstreamRequest(ups, meth)
		}
	})
}

func BenchmarkRecordUpstreamDuration(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)
	ups, meth := getRandomTestData()

	duration := 100 * time.Millisecond

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.RecordUpstreamDuration(ups, meth, duration, true, "none", common.DataFinalityStateUnknown)
		}
	})
}

func BenchmarkRecordUpstreamFailure(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)
	ups, meth := getRandomTestData()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.RecordUpstreamFailure(ups, meth, fmt.Errorf("test problem"))
		}
	})
}

func BenchmarkGetUpstreamMethodMetrics(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)
	ups, meth := getRandomTestData()

	// Pre-warm the tracker with some data
	tracker.RecordUpstreamRequest(ups, meth)
	tracker.RecordUpstreamDuration(ups, meth, time.Millisecond*10, true, "none", common.DataFinalityStateUnknown)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Hot path: read the metrics
			_ = tracker.GetUpstreamMethodMetrics(ups, meth)
		}
	})
}

func BenchmarkTrackerMixed(b *testing.B) {
	// Set up the tracker (single instance) with a 1-minute window
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Create a local RNG to avoid data races on the global rand source
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			// Pick a random upstream and method
			ups, meth := getRandomTestData()

			// Randomly pick one of the main "hot" operations
			action := rng.Intn(4) // possible values: 0..3
			switch action {
			case 0:
				// Record a request
				tracker.RecordUpstreamRequest(ups, meth)
			case 1:
				// Record a failure
				tracker.RecordUpstreamFailure(ups, meth, fmt.Errorf("test problem"))
			case 2:
				// Record a random duration (5ms–50ms)
				dur := time.Duration(5+rng.Intn(45)) * time.Millisecond
				tracker.RecordUpstreamDuration(ups, meth, dur, true, "none", common.DataFinalityStateUnknown)
			case 3:
				// Read the metrics
				_ = tracker.GetUpstreamMethodMetrics(ups, meth)
			}
		}
	})
}

// BenchmarkRecordAndGetMetrics tests the most common operations pattern:
// recording metrics and then reading them back
func BenchmarkRecordAndGetMetrics(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "test", time.Second)
	ctx := context.Background()
	tracker.Bootstrap(ctx)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ups, method := getRandomTestData()

			// Record some metrics
			tracker.RecordUpstreamRequest(ups, method)
			tracker.RecordUpstreamDuration(ups, method, 100*time.Millisecond, true, "none", common.DataFinalityStateUnknown)

			// Then read them back
			metrics := tracker.GetUpstreamMethodMetrics(ups, method)
			if metrics == nil {
				b.Fatal("expected metrics to exist")
			}
		}
	})
}

// BenchmarkReadHeavy simulates scenarios where reads vastly outnumber writes
func BenchmarkReadHeavy(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "test", time.Second)
	ctx := context.Background()
	tracker.Bootstrap(ctx)

	// Pre-populate some data
	for i := 0; i < 1000; i++ {
		ups, method := getRandomTestData()
		tracker.RecordUpstreamRequest(ups, method)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		reads := 0
		for pb.Next() {
			ups, method := getRandomTestData()

			// Do 9 reads for every write
			if reads < 9 {
				_ = tracker.GetUpstreamMethodMetrics(ups, method)
				_ = tracker.GetNetworkMethodMetrics(ups.NetworkId(), method)
				reads++
			} else {
				tracker.RecordUpstreamRequest(ups, method)
				reads = 0
			}
		}
	})
}

// BenchmarkWriteHeavy simulates scenarios where writes vastly outnumber reads
func BenchmarkWriteHeavy(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "test", time.Second)
	ctx := context.Background()
	tracker.Bootstrap(ctx)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		writes := 0
		for pb.Next() {
			ups, method := getRandomTestData()

			// Do 9 writes for every read
			if writes < 9 {
				tracker.RecordUpstreamRequest(ups, method)
				tracker.RecordUpstreamDuration(ups, method, 100*time.Millisecond, true, "none", common.DataFinalityStateUnknown)
				writes++
			} else {
				_ = tracker.GetUpstreamMethodMetrics(ups, method)
				writes = 0
			}
		}
	})
}

// BenchmarkHighConcurrency tests behavior under different goroutine counts
func BenchmarkHighConcurrency(b *testing.B) {
	for _, numG := range []int{10, 50, 100, 200, 500} {
		b.Run(fmt.Sprintf("goroutines-%d", numG), func(b *testing.B) {
			tracker := health.NewTracker(&log.Logger, "test", time.Second)
			ctx := context.Background()
			tracker.Bootstrap(ctx)

			var wg sync.WaitGroup
			wg.Add(numG)

			b.ResetTimer()
			for i := 0; i < numG; i++ {
				go func() {
					defer wg.Done()
					for j := 0; j < b.N; j++ {
						ups, method := getRandomTestData()

						// Mix of operations
						tracker.RecordUpstreamRequest(ups, method)
						_ = tracker.GetUpstreamMethodMetrics(ups, method)
						tracker.RecordUpstreamDuration(ups, method, 100*time.Millisecond, true, "none", common.DataFinalityStateUnknown)
					}
				}()
			}
			wg.Wait()
		})
	}
}

// BenchmarkBlockNumberUpdates tests the block number tracking performance
func BenchmarkBlockNumberUpdates(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "test", time.Second)
	ctx := context.Background()
	tracker.Bootstrap(ctx)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var blockNum int64
		for pb.Next() {
			ups, _ := getRandomTestData()
			blockNum++
			tracker.SetLatestBlockNumber(ups, blockNum)
			tracker.SetFinalizedBlockNumber(ups, blockNum-100)
		}
	})
}

// BenchmarkHotKeyAccess tests performance when many goroutines access the same metrics
func BenchmarkHotKeyAccess(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "test", time.Second)
	ctx := context.Background()
	tracker.Bootstrap(ctx)

	// Use fixed keys to create contention
	const (
		hotMethod = "hot-method"
	)
	hotUps := common.NewFakeUpstream("hot-ups")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// All goroutines hammer the same key
			tracker.RecordUpstreamRequest(hotUps, hotMethod)
			_ = tracker.GetUpstreamMethodMetrics(hotUps, hotMethod)
			tracker.RecordUpstreamDuration(hotUps, hotMethod, 100*time.Millisecond, true, "none", common.DataFinalityStateUnknown)
		}
	})
}

// BenchmarkFullRequestFlow simulates a complete request flow
func BenchmarkFullRequestFlow(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "test", time.Second)
	ctx := context.Background()
	tracker.Bootstrap(ctx)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ups, method := getRandomTestData()

			// Start timing
			timer := tracker.RecordUpstreamDurationStart(ups, method, "none", common.DataFinalityStateUnknown)

			// Record request
			tracker.RecordUpstreamRequest(ups, method)

			// Simulate some work
			time.Sleep(time.Millisecond)

			// Randomly record different types of outcomes
			switch rand.Intn(4) {
			case 0:
				tracker.RecordUpstreamFailure(ups, method, fmt.Errorf("test problem"))
			case 1:
				tracker.RecordUpstreamSelfRateLimited(ups, method)
			case 2:
				tracker.RecordUpstreamRemoteRateLimited(ups, method)
			}

			// End timing
			timer.ObserveDuration(true)

			// Get metrics
			metrics := tracker.GetUpstreamMethodMetrics(ups, method)
			if metrics == nil {
				b.Fatal("expected metrics to exist")
			}
		}
	})
}

// BenchmarkConcurrentCordonOperations tests cordon/uncordon operations
func BenchmarkConcurrentCordonOperations(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "test", time.Second)
	ctx := context.Background()
	tracker.Bootstrap(ctx)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ups, method := getRandomTestData()

			if rand.Float32() < 0.5 {
				tracker.Cordon(ups, method, "test reason")
			} else {
				tracker.Uncordon(ups, method)
			}

			_ = tracker.IsCordoned(ups, method)
		}
	})
}
