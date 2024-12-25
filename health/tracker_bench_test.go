// tracker_bench_test.go
package health_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/erpc/erpc/health"
)

func BenchmarkRecordUpstreamRequest(b *testing.B) {
    tracker := health.NewTracker("benchProj", time.Minute)
    ups := "upstreamA"
    net := "networkX"
    meth := "methodY"

    b.ResetTimer()

    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            tracker.RecordUpstreamRequest(ups, net, meth)
        }
    })
}

func BenchmarkRecordUpstreamDuration(b *testing.B) {
    tracker := health.NewTracker("benchProj", time.Minute)
    ups := "upstreamA"
    net := "networkX"
    meth := "methodY"

    duration := 100 * time.Millisecond

    b.ResetTimer()
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            tracker.RecordUpstreamDuration(ups, net, meth, duration)
        }
    })
}

func BenchmarkRecordUpstreamFailure(b *testing.B) {
    tracker := health.NewTracker("benchProj", time.Minute)
    ups := "upstreamA"
    net := "networkX"
    meth := "methodY"

    b.ResetTimer()
    b.RunParallel(func(pb *testing.PB) {
        for pb.Next() {
            tracker.RecordUpstreamFailure(ups, net, meth)
        }
    })
}

func BenchmarkGetUpstreamMethodMetrics(b *testing.B) {
    tracker := health.NewTracker("benchProj", time.Minute)
    ups := "upstreamA"
    net := "networkX"
    meth := "methodY"

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
    tracker := health.NewTracker("benchProj", time.Minute)

    // Example sets of upstreams, networks, methods
    upsList := []string{"upstreamA", "upstreamB", "upstreamC"}
    netList := []string{"networkX", "networkY"}
    methList := []string{"method1", "method2", "method3", "method4"}

    b.ResetTimer()
    b.RunParallel(func(pb *testing.PB) {
        // Create a local RNG to avoid data races on the global rand source
        rng := rand.New(rand.NewSource(time.Now().UnixNano()))
        for pb.Next() {
            // Pick a random upstream, network, and method
            ups := upsList[rng.Intn(len(upsList))]
            net := netList[rng.Intn(len(netList))]
            meth := methList[rng.Intn(len(methList))]

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