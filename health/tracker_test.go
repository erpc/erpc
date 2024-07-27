package health

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestTracker(t *testing.T) {
	projectID := "test-project"
	networkID := "evm:123"
	windowSize := 100 * time.Millisecond

	t.Run("BasicMetricsCollection", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups1 := newFakeUpstream("a")
		ups2 := newFakeUpstream("b")

		// Simulate requests
		simulateRequests(tracker, networkID, ups1.Config().Id, "method1", 100, 10)
		simulateRequests(tracker, networkID, ups2.Config().Id, "method1", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups1.Config().Id, networkID, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups2.Config().Id, networkID, "method1")

		metrics1.Mutex.RLock()
		assert.Equal(t, float64(100), metrics1.RequestsTotal)
		assert.Equal(t, float64(10), metrics1.ErrorsTotal)
		metrics1.Mutex.RUnlock()

		metrics2.Mutex.RLock()
		assert.Equal(t, float64(50), metrics2.RequestsTotal)
		assert.Equal(t, float64(5), metrics2.ErrorsTotal)
		metrics2.Mutex.RUnlock()
	})

	t.Run("MetricsOverTime", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		// First window
		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 100, 10)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metrics1.Mutex.RLock()
		assert.Equal(t, float64(100), metrics1.RequestsTotal)
		assert.Equal(t, float64(10), metrics1.ErrorsTotal)
		metrics1.Mutex.RUnlock()

		time.Sleep(windowSize + 1*time.Millisecond)

		// Second window
		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 50, 5)

		metrics2 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metrics2.Mutex.RLock()
		assert.Equal(t, float64(50), metrics2.RequestsTotal)
		assert.Equal(t, float64(5), metrics2.ErrorsTotal)
		metrics2.Mutex.RUnlock()
	})

	t.Run("RateLimitingMetrics", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		simulateRequestsWithRateLimiting(tracker, networkID, ups.Config().Id, "method1", 100, 20, 10)

		metrics := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metrics.Mutex.RLock()
		assert.Equal(t, float64(100), metrics.RequestsTotal)
		assert.Equal(t, float64(20), metrics.SelfRateLimitedTotal)
		assert.Equal(t, float64(10), metrics.RemoteRateLimitedTotal)
		metrics.Mutex.RUnlock()
	})

	t.Run("LatencyMetrics", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		resetMetrics()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		simulateRequestsWithLatency(tracker, networkID, ups.Config().Id, "method1", 10, 0.05)

		metrics := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metrics.Mutex.RLock()
		assert.GreaterOrEqual(t, metrics.LatencySecs.P90(), 0.04)
		assert.LessOrEqual(t, metrics.LatencySecs.P90(), 0.06)
		metrics.Mutex.RUnlock()
	})

	t.Run("MultipleUpstreamsComparison", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups1 := newFakeUpstream("a")
		ups2 := newFakeUpstream("b")
		ups3 := newFakeUpstream("c")

		simulateRequests(tracker, networkID, ups1.Config().Id, "method1", 100, 10)
		simulateRequests(tracker, networkID, ups2.Config().Id, "method1", 80, 5)
		simulateRequests(tracker, networkID, ups3.Config().Id, "method1", 120, 15)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups1.Config().Id, networkID, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups2.Config().Id, networkID, "method1")
		metrics3 := tracker.GetUpstreamMethodMetrics(ups3.Config().Id, networkID, "method1")

		// Check if metrics are collected correctly
		assert.Equal(t, float64(100), metrics1.RequestsTotal)
		assert.Equal(t, float64(80), metrics2.RequestsTotal)
		assert.Equal(t, float64(120), metrics3.RequestsTotal)

		// Check if error rates are calculated correctly
		errorRate1 := metrics1.ErrorsTotal / metrics1.RequestsTotal
		errorRate2 := metrics2.ErrorsTotal / metrics2.RequestsTotal
		errorRate3 := metrics3.ErrorsTotal / metrics3.RequestsTotal

		assert.True(t, errorRate2 < errorRate1 && errorRate1 < errorRate3, "Error rates should be ordered: ups2 < ups1 < ups3")
	})

	t.Run("ResetMetrics", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 100, 10)

		metricsBefore := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metricsBefore.Mutex.RLock()
		assert.Equal(t, float64(100), metricsBefore.RequestsTotal)
		assert.Equal(t, float64(10), metricsBefore.ErrorsTotal)
		assert.Equal(t, float64(0), metricsBefore.SelfRateLimitedTotal)
		assert.Equal(t, float64(0), metricsBefore.RemoteRateLimitedTotal)
		metricsBefore.Mutex.RUnlock()

		time.Sleep(windowSize + 1*time.Millisecond)
		metricsAfter := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metricsAfter.Mutex.RLock()
		assert.Equal(t, float64(0), metricsAfter.RequestsTotal)
		assert.Equal(t, float64(0), metricsAfter.ErrorsTotal)
		assert.Equal(t, float64(0), metricsAfter.SelfRateLimitedTotal)
		assert.Equal(t, float64(0), metricsAfter.RemoteRateLimitedTotal)
		metricsAfter.Mutex.RUnlock()
	})

	t.Run("DifferentMethods", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 100, 10)
		simulateRequests(tracker, networkID, ups.Config().Id, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method2")

		metrics1.Mutex.RLock()
		metrics2.Mutex.RLock()
		assert.Equal(t, float64(100), metrics1.RequestsTotal)
		assert.Equal(t, float64(50), metrics2.RequestsTotal)
		metrics1.Mutex.RUnlock()
		metrics2.Mutex.RUnlock()
	})

	t.Run("LongTermMetrics", func(t *testing.T) {
		longWindowSize := 500 * time.Millisecond
		tracker := NewTracker(projectID, longWindowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		for i := 0; i < 5; i++ {
			simulateRequests(tracker, networkID, ups.Config().Id, "method1", 20, 2)
			time.Sleep(50 * time.Millisecond)
		}

		metrics := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
		metrics.Mutex.RLock()
		assert.Equal(t, float64(100), metrics.RequestsTotal)
		assert.Equal(t, float64(10), metrics.ErrorsTotal)
		metrics.Mutex.RUnlock()
	})

	t.Run("LongTermMetricsReset", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		resetMetrics()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")

		for i := 0; i < 5; i++ {
			simulateRequests(tracker, networkID, ups.Config().Id, "method1", 20, 2)

			metrics := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "method1")
			metrics.Mutex.RLock()
			assert.Equal(t, float64(20), metrics.RequestsTotal)
			assert.Equal(t, float64(2), metrics.ErrorsTotal)
			metrics.Mutex.RUnlock()

			time.Sleep(windowSize + 1*time.Millisecond)
		}
	})

	t.Run("MultipleMethodsRequestsIncreaseNetworkOverall", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 100, 10)
		simulateRequests(tracker, networkID, ups.Config().Id, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, networkID, "*")

		metrics1.Mutex.RLock()
		assert.Equal(t, float64(150), metrics1.RequestsTotal)
		metrics1.Mutex.RUnlock()
	})

	t.Run("MultipleMethodsRequestsIncreaseUpstreamOverall", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 100, 10)
		simulateRequests(tracker, networkID, ups.Config().Id, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups.Config().Id, "*", "*")

		metrics1.Mutex.RLock()
		assert.Equal(t, float64(150), metrics1.RequestsTotal)
		metrics1.Mutex.RUnlock()
	})

	t.Run("LatencyCorrectP90AcrossMethods", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		simulateRequestsWithLatency(tracker, networkID, ups.Config().Id, "method1", 10, 0.06)
		simulateRequestsWithLatency(tracker, networkID, ups.Config().Id, "method2", 90, 0.02)

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
		simulateRequestsWithLatency(tracker, networkID, ups.Config().Id, "method1", 10, 0.06)
		simulateRequestsWithLatency(tracker, networkID, ups.Config().Id, "method1", 90, 0.02)

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

func (u *fakeUpstream) Vendor() common.Vendor {
	return nil
}

func (u *fakeUpstream) SupportsNetwork(networkId string) (bool, error) {
	return true, nil
}

func simulateRequests(tracker *Tracker, network, upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, network, method)
		if i < errors {
			tracker.RecordUpstreamFailure(upstream, network, method, "test-error")
		}
		timer := tracker.RecordUpstreamDurationStart(upstream, network, method)
		timer.ObserveDuration()
	}
}

func simulateRequestsWithRateLimiting(tracker *Tracker, network, upstream, method string, total, selfLimited, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, network, method)
		if i < selfLimited {
			tracker.RecordUpstreamSelfRateLimited(upstream, network, method)
		}
		if i >= selfLimited && i < selfLimited+remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(upstream, network, method)
		}
		timer := tracker.RecordUpstreamDurationStart(upstream, network, method)
		timer.ObserveDuration()
	}
}

func simulateRequestsWithLatency(tracker *Tracker, network, upstream, method string, total int, latency float64) {
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
	metricRequestTotal.Reset()
	metricRequestDuration.Reset()
	metricErrorTotal.Reset()
	metricSelfRateLimitedTotal.Reset()
	metricRemoteRateLimitedTotal.Reset()
}
