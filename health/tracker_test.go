package health

import (
	"context"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestTracker(t *testing.T) {
	projectID := "test-project"
	networkID := "evm:123"
	windowSize := 100 * time.Millisecond
	refreshInterval := 50 * time.Millisecond

	t.Run("BasicMetricsCollection", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize, refreshInterval)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups1 := newFakeUpstream("a")
		ups2 := newFakeUpstream("b")
		tracker.RegisterUpstream(ups1)
		tracker.RegisterUpstream(ups2)

		// Simulate requests
		simulateRequests(tracker, networkID, ups1.Config().Id, "method1", 100, 10)
		simulateRequests(tracker, networkID, ups2.Config().Id, "method1", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(networkID, ups1.Config().Id, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(networkID, ups2.Config().Id, "method1")

		assert.Equal(t, float64(100), metrics1.RequestsTotal)
		assert.Equal(t, float64(10), metrics1.ErrorsTotal)
		assert.Equal(t, float64(50), metrics2.RequestsTotal)
		assert.Equal(t, float64(5), metrics2.ErrorsTotal)
	})

	t.Run("MetricsOverTime", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize, refreshInterval)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		tracker.RegisterUpstream(ups)

		// First window
		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 100, 10)

		metrics1 := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")
		assert.Equal(t, float64(100), metrics1.RequestsTotal)
		assert.Equal(t, float64(10), metrics1.ErrorsTotal)

		time.Sleep(windowSize + 1*time.Millisecond)

		// Second window
		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 50, 5)

		metrics2 := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")
		assert.Equal(t, float64(50), metrics2.RequestsTotal)
		assert.Equal(t, float64(5), metrics2.ErrorsTotal)
	})

	t.Run("RateLimitingMetrics", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize, refreshInterval)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		tracker.RegisterUpstream(ups)

		simulateRequestsWithRateLimiting(tracker, networkID, ups.Config().Id, "method1", 100, 20, 10)

		metrics := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")
		assert.Equal(t, float64(100), metrics.RequestsTotal)
		assert.Equal(t, float64(20), metrics.SelfRateLimitedTotal)
		assert.Equal(t, float64(10), metrics.RemoteRateLimitedTotal)
	})

	t.Run("LatencyMetrics", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize, refreshInterval)
		resetMetrics()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		tracker.RegisterUpstream(ups)

		simulateRequestsWithLatency(tracker, networkID, ups.Config().Id, "method1", 10, 0.05)

		time.Sleep(refreshInterval)

		metrics := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")
		assert.Greater(t, metrics.P90LatencySecs, 0.04)
		assert.Less(t, metrics.P90LatencySecs, 0.06)
	})

	t.Run("MultipleUpstreamsComparison", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize, refreshInterval)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups1 := newFakeUpstream("a")
		ups2 := newFakeUpstream("b")
		ups3 := newFakeUpstream("c")
		tracker.RegisterUpstream(ups1)
		tracker.RegisterUpstream(ups2)
		tracker.RegisterUpstream(ups3)

		simulateRequests(tracker, networkID, ups1.Config().Id, "method1", 100, 10)
		simulateRequests(tracker, networkID, ups2.Config().Id, "method1", 80, 5)
		simulateRequests(tracker, networkID, ups3.Config().Id, "method1", 120, 15)

		time.Sleep(refreshInterval)

		metrics1 := tracker.GetUpstreamMethodMetrics(networkID, ups1.Config().Id, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(networkID, ups2.Config().Id, "method1")
		metrics3 := tracker.GetUpstreamMethodMetrics(networkID, ups3.Config().Id, "method1")

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
		tracker := NewTracker(projectID, windowSize, refreshInterval)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		tracker.RegisterUpstream(ups)

		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 100, 10)

		time.Sleep(refreshInterval)
		metricsBefore := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")
		time.Sleep(windowSize)
		metricsAfter := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")

		assert.Equal(t, float64(100), metricsBefore.RequestsTotal)
		assert.Equal(t, float64(10), metricsBefore.ErrorsTotal)
		assert.Equal(t, float64(0), metricsBefore.SelfRateLimitedTotal)
		assert.Equal(t, float64(0), metricsBefore.RemoteRateLimitedTotal)

		assert.Equal(t, float64(0), metricsAfter.RequestsTotal)
		assert.Equal(t, float64(0), metricsAfter.ErrorsTotal)
		assert.Equal(t, float64(0), metricsAfter.SelfRateLimitedTotal)
		assert.Equal(t, float64(0), metricsAfter.RemoteRateLimitedTotal)
	})

	t.Run("DifferentMethods", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize, refreshInterval)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		tracker.RegisterUpstream(ups)

		simulateRequests(tracker, networkID, ups.Config().Id, "method1", 100, 10)
		simulateRequests(tracker, networkID, ups.Config().Id, "method2", 50, 5)

		time.Sleep(refreshInterval)

		metrics1 := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method2")

		assert.Equal(t, float64(100), metrics1.RequestsTotal)
		assert.Equal(t, float64(50), metrics2.RequestsTotal)
	})

	t.Run("LongTermMetrics", func(t *testing.T) {
		longWindowSize := 500 * time.Millisecond
		tracker := NewTracker(projectID, longWindowSize, refreshInterval)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		tracker.RegisterUpstream(ups)

		for i := 0; i < 5; i++ {
			simulateRequests(tracker, networkID, ups.Config().Id, "method1", 20, 2)
			time.Sleep(50 * time.Millisecond)
		}

		metrics := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")
		assert.Equal(t, float64(100), metrics.RequestsTotal)
		assert.Equal(t, float64(10), metrics.ErrorsTotal)
	})

	t.Run("LongTermMetricsReset", func(t *testing.T) {
		tracker := NewTracker(projectID, windowSize, refreshInterval)
		resetMetrics()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := newFakeUpstream("a")
		tracker.RegisterUpstream(ups)

		for i := 0; i < 5; i++ {
			simulateRequests(tracker, networkID, ups.Config().Id, "method1", 20, 2)

			metrics := tracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, "method1")
			assert.Equal(t, float64(20), metrics.RequestsTotal)
			assert.Equal(t, float64(2), metrics.ErrorsTotal)

			time.Sleep(windowSize + 1*time.Millisecond)
		}
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
		tracker.RecordUpstreamRequest(network, upstream, method)
		if i < errors {
			tracker.RecordUpstreamFailure(network, upstream, method, "test-error")
		}
		timer := tracker.RecordUpstreamDurationStart(network, upstream, method)
		timer.ObserveDuration()
	}
}

func simulateRequestsWithRateLimiting(tracker *Tracker, network, upstream, method string, total, selfLimited, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(network, upstream, method)
		if i < selfLimited {
			tracker.RecordUpstreamSelfRateLimited(network, upstream, method)
		}
		if i >= selfLimited && i < selfLimited+remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(network, upstream, method)
		}
		timer := tracker.RecordUpstreamDurationStart(network, upstream, method)
		timer.ObserveDuration()
	}
}

func simulateRequestsWithLatency(tracker *Tracker, network, upstream, method string, total int, latency float64) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(network, upstream, method)
		timer := tracker.RecordUpstreamDurationStart(network, upstream, method)
		time.Sleep(time.Duration(latency * float64(time.Second)))
		timer.ObserveDuration()
	}
}

func resetMetrics() {
	metricRequestTotal.Reset()
	metricRequestDuration.Reset()
	metricErrorTotal.Reset()
	metricSelfRateLimitedTotal.Reset()
	metricRemoteRateLimitedTotal.Reset()
}