package health

import (
	"context"
	// "fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestTracker(t *testing.T) {
	projectID := "test-project"
	networkID := "evm:123"
	windowSize := 2000 * time.Millisecond

	t.Run("BasicMetricsCollection", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups1 := newFakeUpstream("a")
		ups2 := newFakeUpstream("b")

		// Simulate requests
		simulateRequestMetrics(tracker, networkID, ups1.Config().Id, "method1", 100, 10)
		simulateRequestMetrics(tracker, networkID, ups2.Config().Id, "method1", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups1.Config().Id, networkID, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups2.Config().Id, networkID, "method1")

		assert.Equal(t, int64(100), metrics1.RequestsTotal.Load())
		assert.Equal(t, int64(10), metrics1.ErrorsTotal.Load())

		assert.Equal(t, int64(50), metrics2.RequestsTotal.Load())
		assert.Equal(t, int64(5), metrics2.ErrorsTotal.Load())
	})

	t.Run("MetricsOverTime", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		// First window
		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 100, 10)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		assert.Equal(t, int64(100), metrics1.RequestsTotal.Load())
		assert.Equal(t, int64(10), metrics1.ErrorsTotal.Load())

		time.Sleep(windowSize + 10*time.Millisecond)

		// Second window
		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 50, 5)

		metrics2 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		assert.Equal(t, int64(50), metrics2.RequestsTotal.Load())
		assert.Equal(t, int64(5), metrics2.ErrorsTotal.Load())
	})

	t.Run("RateLimitingMetrics", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		simulateRateLimitedRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 100, 20, 10)

		metrics := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		assert.Equal(t, int64(100), metrics.RequestsTotal.Load())
		assert.Equal(t, int64(20), metrics.SelfRateLimitedTotal.Load())
		assert.Equal(t, int64(10), metrics.RemoteRateLimitedTotal.Load())
	})

	t.Run("LatencyMetrics", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		resetMetrics()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		simulateRequestMetricsWithLatency(tracker, networkID, ups.Config().Id, "method1", 10, 0.05)

		metrics := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		assert.GreaterOrEqual(t, metrics.LatencySecs.P90(), 0.04)
		assert.LessOrEqual(t, metrics.LatencySecs.P90(), 0.06)
	})

	t.Run("MultipleUpstreamsComparison", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups1 := newFakeUpstream("a")
		ups2 := newFakeUpstream("b")
		ups3 := newFakeUpstream("c")

		simulateRequestMetrics(tracker, networkID, ups1.Config().Id, "method1", 100, 10)
		simulateRequestMetrics(tracker, networkID, ups2.Config().Id, "method1", 80, 5)
		simulateRequestMetrics(tracker, networkID, ups3.Config().Id, "method1", 120, 15)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups1.Config().Id, networkID, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups2.Config().Id, networkID, "method1")
		metrics3 := tracker.GetUpstreamMethodMetrics(ups3.Config().Id, networkID, "method1")

		// Check if metrics are collected correctly
		assert.Equal(t, int64(100), metrics1.RequestsTotal.Load())
		assert.Equal(t, int64(80), metrics2.RequestsTotal.Load())
		assert.Equal(t, int64(120), metrics3.RequestsTotal.Load())

		// Check if error rates are calculated correctly
		errorRate1 := float64(metrics1.ErrorsTotal.Load()) / float64(metrics1.RequestsTotal.Load())
		errorRate2 := float64(metrics2.ErrorsTotal.Load()) / float64(metrics2.RequestsTotal.Load())
		errorRate3 := float64(metrics3.ErrorsTotal.Load()) / float64(metrics3.RequestsTotal.Load())

		assert.True(t, errorRate2 < errorRate1 && errorRate1 < errorRate3, "Error rates should be ordered: ups2 < ups1 < ups3")
	})

	t.Run("ResetMetrics", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 100, 10)

		metricsBefore := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		assert.Equal(t, int64(100), metricsBefore.RequestsTotal.Load())
		assert.Equal(t, int64(10), metricsBefore.ErrorsTotal.Load())
		assert.Equal(t, int64(0), metricsBefore.SelfRateLimitedTotal.Load())
		assert.Equal(t, int64(0), metricsBefore.RemoteRateLimitedTotal.Load())

		time.Sleep(windowSize + 10*time.Millisecond)

		metricsAfter := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		assert.Equal(t, int64(0), metricsAfter.RequestsTotal.Load())
		assert.Equal(t, int64(0), metricsAfter.ErrorsTotal.Load())
		assert.Equal(t, int64(0), metricsAfter.SelfRateLimitedTotal.Load())
		assert.Equal(t, int64(0), metricsAfter.RemoteRateLimitedTotal.Load())
	})

	t.Run("DifferentMethods", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 100, 10)
		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method2")

		assert.Equal(t, int64(100), metrics1.RequestsTotal.Load())
		assert.Equal(t, int64(50), metrics2.RequestsTotal.Load())
	})

	t.Run("LongTermMetrics", func(t *testing.T) {
		longWindowSize := 500 * time.Millisecond
		tracker := NewTracker(projectID, longWindowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		for i := 0; i < 5; i++ {
			simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 20, 2)
			time.Sleep(50 * time.Millisecond)
		}

		metrics := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		assert.Equal(t, int64(100), metrics.RequestsTotal.Load())
		assert.Equal(t, int64(10), metrics.ErrorsTotal.Load())
	})

	t.Run("LongTermMetricsReset", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		resetMetrics()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		for i := 0; i < 5; i++ {
			simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 20, 2)

			metrics := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
			assert.Equal(t, int64(20), metrics.RequestsTotal.Load())
			assert.Equal(t, int64(2), metrics.ErrorsTotal.Load())

			time.Sleep(windowSize + 10*time.Millisecond)
		}
	})

	t.Run("MultipleMethodsRequestsIncreaseNetworkOverall", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 100, 10)
		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "*")

		assert.Equal(t, int64(150), metrics1.RequestsTotal.Load())
	})

	t.Run("MultipleMethodsRequestsIncreaseUpstreamOverall", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method1", 100, 10)
		simulateRequestMetrics(tracker, networkID, ups.Config().Id, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, "*", "*")

		assert.Equal(t, int64(150), metrics1.RequestsTotal.Load())
	})

	t.Run("LatencyCorrectP90AcrossMethods", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		simulateRequestMetricsWithLatency(tracker, networkID, ups.Config().Id, "method1", 10, 0.06)
		simulateRequestMetricsWithLatency(tracker, networkID, ups.Config().Id, "method2", 90, 0.02)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, "*", "*")
		assert.GreaterOrEqual(t, metrics1.LatencySecs.P90(), 0.02)
		assert.LessOrEqual(t, metrics1.LatencySecs.P90(), 0.03)
	})

	t.Run("LatencyCorrectP90SingleMethod", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		simulateRequestMetricsWithLatency(tracker, networkID, ups.Config().Id, "method1", 10, 0.06)
		simulateRequestMetricsWithLatency(tracker, networkID, ups.Config().Id, "method1", 90, 0.02)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		assert.GreaterOrEqual(t, metrics1.LatencySecs.P90(), 0.02)
		assert.LessOrEqual(t, metrics1.LatencySecs.P90(), 0.03)
	})
}

type fakeUpstream struct {
	id string
}

func newFakeUpstream(id string) common.Upstream {
	return &fakeUpstream{
		id: id,
	}
}

func (u *fakeUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id: u.id,
	}
}

func (u *fakeUpstream) EvmGetChainId(context.Context) (string, error) {
	return "123", nil
}

func (u *fakeUpstream) EvmSyncingState() common.EvmSyncingState {
	return common.EvmSyncingStateUnknown
}

func (u *fakeUpstream) Vendor() common.Vendor {
	return nil
}

func (u *fakeUpstream) SupportsNetwork(ctx context.Context, networkId string) (bool, error) {
	return true, nil
}

func simulateRequestMetrics(tracker *Tracker, network, upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, network, method)
		if i < errors {
			tracker.RecordUpstreamFailure(upstream, network, method)
		}
	}
}

func simulateRateLimitedRequestMetrics(tracker *Tracker, network, upstream, method string, total, selfLimited, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, network, method)
		if i < selfLimited {
			tracker.RecordUpstreamSelfRateLimited(upstream, network, method)
		}
		if i >= selfLimited && i < selfLimited+remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(upstream, network, method)
		}
	}
}

func simulateRequestMetricsWithLatency(tracker *Tracker, network, upstream, method string, total int, latency float64) {
	wg := sync.WaitGroup{}
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordUpstreamRequest(upstream, network, method)
			tracker.RecordUpstreamDuration(upstream, network, method, time.Duration(latency*float64(time.Second)))
		}()
	}
	wg.Wait()
}

func resetMetrics() {
	MetricUpstreamRequestTotal.Reset()
	MetricUpstreamRequestDuration.Reset()
	MetricUpstreamErrorTotal.Reset()
	MetricUpstreamSelfRateLimitedTotal.Reset()
	MetricUpstreamRemoteRateLimitedTotal.Reset()
}
