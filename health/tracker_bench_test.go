package health

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	util.ConfigureTestLogger()
	// Initialize telemetry metrics
	if err := telemetry.SetHistogramBuckets(""); err != nil {
		panic(err)
	}
}

// MockUpstream implements the common.Upstream interface for benchmarking
type MockUpstream struct {
	id      string
	network string
	vendor  string
}

func (m *MockUpstream) Id() string                     { return m.id }
func (m *MockUpstream) NetworkId() string              { return m.network }
func (m *MockUpstream) NetworkLabel() string           { return m.network }
func (m *MockUpstream) VendorName() string             { return m.vendor }
func (m *MockUpstream) Logger() *zerolog.Logger        { return &log.Logger }
func (m *MockUpstream) Config() *common.UpstreamConfig { return nil }
func (m *MockUpstream) Vendor() common.Vendor          { return nil }
func (m *MockUpstream) Tracker() common.HealthTracker  { return nil }
func (m *MockUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (m *MockUpstream) Cordon(method string, reason string)   {}
func (m *MockUpstream) Uncordon(method string, reason string) {}
func (m *MockUpstream) IgnoreMethod(method string)            {}

// BenchmarkTrackerRecordUpstreamRequest benchmarks the most frequently called method
func BenchmarkTrackerRecordUpstreamRequest(b *testing.B) {
	tracker := NewTracker(&log.Logger, "test-project", 1*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	// Create a set of upstreams to simulate real usage
	upstreams := make([]common.Upstream, 10)
	for i := 0; i < 10; i++ {
		upstreams[i] = &MockUpstream{
			id:      fmt.Sprintf("upstream-%d", i),
			network: fmt.Sprintf("network-%d", i%3), // 3 different networks
			vendor:  "test-vendor",
		}
	}

	methods := []string{"eth_call", "eth_getBalance", "eth_getBlockByNumber", "eth_sendRawTransaction"}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			upstream := upstreams[i%len(upstreams)]
			method := methods[i%len(methods)]
			tracker.RecordUpstreamRequest(upstream, method)
			i++
		}
	})
}

// BenchmarkTrackerRecordUpstreamDuration benchmarks duration recording
func BenchmarkTrackerRecordUpstreamDuration(b *testing.B) {
	tracker := NewTracker(&log.Logger, "test-project", 1*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	upstreams := make([]common.Upstream, 10)
	for i := 0; i < 10; i++ {
		upstreams[i] = &MockUpstream{
			id:      fmt.Sprintf("upstream-%d", i),
			network: fmt.Sprintf("network-%d", i%3),
			vendor:  "test-vendor",
		}
	}

	methods := []string{"eth_call", "eth_getBalance", "eth_getBlockByNumber", "eth_sendRawTransaction"}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			upstream := upstreams[i%len(upstreams)]
			method := methods[i%len(methods)]
			tracker.RecordUpstreamDuration(
				upstream,
				method,
				100*time.Millisecond,
				true,
				"none",
				common.DataFinalityStateUnknown,
				"user-1",
			)
			i++
		}
	})
}

// BenchmarkTrackerRecordUpstreamFailure benchmarks failure recording
func BenchmarkTrackerRecordUpstreamFailure(b *testing.B) {
	tracker := NewTracker(&log.Logger, "test-project", 1*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	upstreams := make([]common.Upstream, 10)
	for i := 0; i < 10; i++ {
		upstreams[i] = &MockUpstream{
			id:      fmt.Sprintf("upstream-%d", i),
			network: fmt.Sprintf("network-%d", i%3),
			vendor:  "test-vendor",
		}
	}

	methods := []string{"eth_call", "eth_getBalance", "eth_getBlockByNumber", "eth_sendRawTransaction"}
	testErr := fmt.Errorf("test error")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			upstream := upstreams[i%len(upstreams)]
			method := methods[i%len(methods)]
			tracker.RecordUpstreamFailure(upstream, method, testErr)
			i++
		}
	})
}

// BenchmarkTrackerSetLatestBlockNumber benchmarks block number updates
func BenchmarkTrackerSetLatestBlockNumber(b *testing.B) {
	tracker := NewTracker(&log.Logger, "test-project", 1*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	// Pre-populate with some upstreams
	upstreams := make([]common.Upstream, 10)
	for i := 0; i < 10; i++ {
		upstreams[i] = &MockUpstream{
			id:      fmt.Sprintf("upstream-%d", i),
			network: fmt.Sprintf("network-%d", i%3),
			vendor:  "test-vendor",
		}
		// Initialize metrics for each upstream
		tracker.RecordUpstreamRequest(upstreams[i], "eth_call")
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			upstream := upstreams[i%len(upstreams)]
			blockNumber := int64(1000000 + i)
			tracker.SetLatestBlockNumber(upstream, blockNumber, 0)
			i++
		}
	})
}

// BenchmarkTrackerMixedOperations simulates a realistic mix of operations
func BenchmarkTrackerMixedOperations(b *testing.B) {
	tracker := NewTracker(&log.Logger, "test-project", 1*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	upstreams := make([]common.Upstream, 10)
	for i := 0; i < 10; i++ {
		upstreams[i] = &MockUpstream{
			id:      fmt.Sprintf("upstream-%d", i),
			network: fmt.Sprintf("network-%d", i%3),
			vendor:  "test-vendor",
		}
		// Initialize metrics
		tracker.RecordUpstreamRequest(upstreams[i], "eth_call")
	}

	methods := []string{"eth_call", "eth_getBalance", "eth_getBlockByNumber", "eth_sendRawTransaction"}
	testErr := fmt.Errorf("test error")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			upstream := upstreams[i%len(upstreams)]
			method := methods[i%len(methods)]

			// Simulate a realistic mix of operations
			switch i % 5 {
			case 0, 1, 2: // 60% are regular requests
				tracker.RecordUpstreamRequest(upstream, method)
				tracker.RecordUpstreamDuration(upstream, method, 100*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "user-1")
			case 3: // 20% are failures
				tracker.RecordUpstreamRequest(upstream, method)
				tracker.RecordUpstreamFailure(upstream, method, testErr)
			case 4: // 20% are remote rate limited
				tracker.RecordUpstreamRemoteRateLimited(context.Background(), upstream, method, nil)
			}

			// Occasionally update block numbers
			if i%100 == 0 {
				tracker.SetLatestBlockNumber(upstream, int64(1000000+i), 0)
			}

			i++
		}
	})
}

// BenchmarkTrackerRecordUpstreamDurationWithTimer benchmarks the timer-based duration recording
func BenchmarkTrackerRecordUpstreamDurationWithTimer(b *testing.B) {
	tracker := NewTracker(&log.Logger, "test-project", 1*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	upstreams := make([]common.Upstream, 10)
	for i := 0; i < 10; i++ {
		upstreams[i] = &MockUpstream{
			id:      fmt.Sprintf("upstream-%d", i),
			network: fmt.Sprintf("network-%d", i%3),
			vendor:  "test-vendor",
		}
	}

	methods := []string{"eth_call", "eth_getBalance", "eth_getBlockByNumber", "eth_sendRawTransaction"}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			upstream := upstreams[i%len(upstreams)]
			method := methods[i%len(methods)]

			// Start timer
			timer := tracker.RecordUpstreamDurationStart(upstream, method, "none", common.DataFinalityStateUnknown, "user-1")

			// Simulate some work
			time.Sleep(1 * time.Nanosecond)

			// Record duration
			timer.ObserveDuration(true)

			i++
		}
	})
}
