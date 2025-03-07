// tracker_bench_test.go
package health_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
)

var (
	testUpstreams = []string{"ups1", "ups2", "ups3", "ups4"}
	testNetworks  = []string{"eth", "polygon", "bsc", "arbitrum"}
	testMethods   = []string{"eth_call", "eth_getBalance", "eth_getBlockByNumber", "eth_getLogs"}
)

func init() {
	util.ConfigureTestLogger()
}

// Helper to get random test data
func getRandomTestData() (string, string, string) {
	return testUpstreams[rand.Intn(len(testUpstreams))],
		testNetworks[rand.Intn(len(testNetworks))],
		testMethods[rand.Intn(len(testMethods))]
}

func BenchmarkRecordUpstreamRequest(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)
	ups, net, meth := getRandomTestData()

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.RecordUpstreamRequest(ups, net, meth)
		}
	})
}

func BenchmarkRecordUpstreamDuration(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)
	ups, net, meth := getRandomTestData()

	duration := 100 * time.Millisecond

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.RecordUpstreamDuration(ups, net, meth, duration)
		}
	})
}

func BenchmarkRecordUpstreamFailure(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)
	ups, net, meth := getRandomTestData()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tracker.RecordUpstreamFailure(ups, net, meth)
		}
	})
}

func BenchmarkGetUpstreamMethodMetrics(b *testing.B) {
	tracker := health.NewTracker(&log.Logger, "benchProj", time.Minute)
	ups, net, meth := getRandomTestData()

	// Pre-warm the tracker with some data
	tracker.RecordUpstreamRequest(ups, net, meth)
	tracker.RecordUpstreamDuration(ups, net, meth, time.Millisecond*10)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Hot path: read the metrics
			_ = tracker.GetUpstreamMethodMetrics(ups, net, meth)
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
			// Pick a random upstream, network, and method
			ups, net, meth := getRandomTestData()

			// Randomly pick one of the main “hot” operations
			action := rng.Intn(4) // possible values: 0..3
			switch action {
			case 0:
				// Record a request
				tracker.RecordUpstreamRequest(ups, net, meth)
			case 1:
				// Record a failure
				tracker.RecordUpstreamFailure(ups, net, meth)
			case 2:
				// Record a random duration (5ms–50ms)
				dur := time.Duration(5+rng.Intn(45)) * time.Millisecond
				tracker.RecordUpstreamDuration(ups, net, meth, dur)
			case 3:
				// Read the metrics
				_ = tracker.GetUpstreamMethodMetrics(ups, net, meth)
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
			ups, network, method := getRandomTestData()

			// Record some metrics
			tracker.RecordUpstreamRequest(ups, network, method)
			tracker.RecordUpstreamDuration(ups, network, method, 100*time.Millisecond)

			// Then read them back
			metrics := tracker.GetUpstreamMethodMetrics(ups, network, method)
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
		ups, network, method := getRandomTestData()
		tracker.RecordUpstreamRequest(ups, network, method)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		reads := 0
		for pb.Next() {
			ups, network, method := getRandomTestData()

			// Do 9 reads for every write
			if reads < 9 {
				_ = tracker.GetUpstreamMethodMetrics(ups, network, method)
				_ = tracker.GetNetworkMethodMetrics(network, method)
				reads++
			} else {
				tracker.RecordUpstreamRequest(ups, network, method)
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
			ups, network, method := getRandomTestData()

			// Do 9 writes for every read
			if writes < 9 {
				tracker.RecordUpstreamRequest(ups, network, method)
				tracker.RecordUpstreamDuration(ups, network, method, 100*time.Millisecond)
				writes++
			} else {
				_ = tracker.GetUpstreamMethodMetrics(ups, network, method)
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
						ups, network, method := getRandomTestData()

						// Mix of operations
						tracker.RecordUpstreamRequest(ups, network, method)
						_ = tracker.GetUpstreamMethodMetrics(ups, network, method)
						tracker.RecordUpstreamDuration(ups, network, method, 100*time.Millisecond)
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
			ups, network, _ := getRandomTestData()
			blockNum++
			tracker.SetLatestBlockNumber(ups, network, blockNum)
			tracker.SetFinalizedBlockNumber(ups, network, blockNum-100)
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
		hotUps     = "hot-ups"
		hotNetwork = "hot-network"
		hotMethod  = "hot-method"
	)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// All goroutines hammer the same key
			tracker.RecordUpstreamRequest(hotUps, hotNetwork, hotMethod)
			_ = tracker.GetUpstreamMethodMetrics(hotUps, hotNetwork, hotMethod)
			tracker.RecordUpstreamDuration(hotUps, hotNetwork, hotMethod, 100*time.Millisecond)
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
			ups, network, method := getRandomTestData()

			// Start timing
			timer := tracker.RecordUpstreamDurationStart(ups, network, method)

			// Record request
			tracker.RecordUpstreamRequest(ups, network, method)

			// Simulate some work
			time.Sleep(time.Millisecond)

			// Randomly record different types of outcomes
			switch rand.Intn(4) {
			case 0:
				tracker.RecordUpstreamFailure(ups, network, method)
			case 1:
				tracker.RecordUpstreamSelfRateLimited(ups, network, method)
			case 2:
				tracker.RecordUpstreamRemoteRateLimited(ups, network, method)
			}

			// End timing
			timer.ObserveDuration()

			// Get metrics
			metrics := tracker.GetUpstreamMethodMetrics(ups, network, method)
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
			ups, network, method := getRandomTestData()

			if rand.Float32() < 0.5 {
				tracker.Cordon(ups, network, method, "test reason")
			} else {
				tracker.Uncordon(ups, network, method)
			}

			_ = tracker.IsCordoned(ups, network, method)
		}
	})
}
