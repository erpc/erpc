package health

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func TestTracker(t *testing.T) {
	projectID := "test-project"
	windowSize := 2000 * time.Millisecond

	telemetry.SetHistogramBuckets("0.05,0.5,5,30")

	t.Run("BasicMetricsCollection", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups1 := common.NewFakeUpstream("a")
		ups2 := common.NewFakeUpstream("b")

		// Simulate requests
		simulateRequestMetrics(tracker, ups1, "method1", 100, 10)
		simulateRequestMetrics(tracker, ups2, "method1", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups1, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups2, "method1")

		assert.Equal(t, int64(100), metrics1.RequestsTotal.Load())
		assert.Equal(t, int64(10), metrics1.ErrorsTotal.Load())

		assert.Equal(t, int64(50), metrics2.RequestsTotal.Load())
		assert.Equal(t, int64(5), metrics2.ErrorsTotal.Load())
	})

	t.Run("MetricsOverTime", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")

		// First window
		simulateRequestMetrics(tracker, ups, "method1", 100, 10)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.Equal(t, int64(100), metrics1.RequestsTotal.Load())
		assert.Equal(t, int64(10), metrics1.ErrorsTotal.Load())

		time.Sleep(windowSize + 10*time.Millisecond)

		// Second window
		simulateRequestMetrics(tracker, ups, "method1", 50, 5)

		metrics2 := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.Equal(t, int64(50), metrics2.RequestsTotal.Load())
		assert.Equal(t, int64(5), metrics2.ErrorsTotal.Load())
	})

	t.Run("RateLimitingMetrics", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")

		simulateRateLimitedRequestMetrics(tracker, ups, "method1", 100, 20, 10)

		metrics := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.Equal(t, int64(100), metrics.RequestsTotal.Load())
		assert.Equal(t, int64(20), metrics.SelfRateLimitedTotal.Load())
		assert.Equal(t, int64(10), metrics.RemoteRateLimitedTotal.Load())
	})

	t.Run("LatencyMetrics", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)
		resetMetrics()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")

		simulateRequestMetricsWithLatency(tracker, ups, "method1", 10, 0.05)

		metrics := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.GreaterOrEqual(t, metrics.ResponseQuantiles.GetQuantile(0.90).Seconds(), 0.04)
		assert.LessOrEqual(t, metrics.ResponseQuantiles.GetQuantile(0.90).Seconds(), 0.06)
	})

	t.Run("MultipleUpstreamsComparison", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups1 := common.NewFakeUpstream("a")
		ups2 := common.NewFakeUpstream("b")
		ups3 := common.NewFakeUpstream("c")

		simulateRequestMetrics(tracker, ups1, "method1", 100, 10)
		simulateRequestMetrics(tracker, ups2, "method1", 80, 5)
		simulateRequestMetrics(tracker, ups3, "method1", 120, 15)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups1, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups2, "method1")
		metrics3 := tracker.GetUpstreamMethodMetrics(ups3, "method1")

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
		tracker := NewTracker(&log.Logger, projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")

		simulateRequestMetrics(tracker, ups, "method1", 100, 10)

		metricsBefore := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.Equal(t, int64(100), metricsBefore.RequestsTotal.Load())
		assert.Equal(t, int64(10), metricsBefore.ErrorsTotal.Load())
		assert.Equal(t, int64(0), metricsBefore.SelfRateLimitedTotal.Load())
		assert.Equal(t, int64(0), metricsBefore.RemoteRateLimitedTotal.Load())

		time.Sleep(windowSize + 10*time.Millisecond)

		metricsAfter := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.Equal(t, int64(0), metricsAfter.RequestsTotal.Load())
		assert.Equal(t, int64(0), metricsAfter.ErrorsTotal.Load())
		assert.Equal(t, int64(0), metricsAfter.SelfRateLimitedTotal.Load())
		assert.Equal(t, int64(0), metricsAfter.RemoteRateLimitedTotal.Load())
	})

	t.Run("DifferentMethods", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")

		simulateRequestMetrics(tracker, ups, "method1", 100, 10)
		simulateRequestMetrics(tracker, ups, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "method1")
		metrics2 := tracker.GetUpstreamMethodMetrics(ups, "method2")

		assert.Equal(t, int64(100), metrics1.RequestsTotal.Load())
		assert.Equal(t, int64(50), metrics2.RequestsTotal.Load())
	})

	t.Run("LongTermMetrics", func(t *testing.T) {
		longWindowSize := 500 * time.Millisecond
		tracker := NewTracker(&log.Logger, projectID, longWindowSize)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")

		for i := 0; i < 5; i++ {
			simulateRequestMetrics(tracker, ups, "method1", 20, 2)
			time.Sleep(50 * time.Millisecond)
		}

		metrics := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.Equal(t, int64(100), metrics.RequestsTotal.Load())
		assert.Equal(t, int64(10), metrics.ErrorsTotal.Load())
	})

	t.Run("LongTermMetricsReset", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)
		resetMetrics()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")

		for i := 0; i < 5; i++ {
			simulateRequestMetrics(tracker, ups, "method1", 20, 2)

			metrics := tracker.GetUpstreamMethodMetrics(ups, "method1")
			assert.Equal(t, int64(20), metrics.RequestsTotal.Load())
			assert.Equal(t, int64(2), metrics.ErrorsTotal.Load())

			time.Sleep(windowSize + 10*time.Millisecond)
		}
	})

	t.Run("MultipleMethodsRequestsIncreaseNetworkOverall", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")
		simulateRequestMetrics(tracker, ups, "method1", 100, 10)
		simulateRequestMetrics(tracker, ups, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "*")

		assert.Equal(t, int64(150), metrics1.RequestsTotal.Load())
	})

	t.Run("MultipleMethodsRequestsIncreaseUpstreamOverall", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")
		simulateRequestMetrics(tracker, ups, "method1", 100, 10)
		simulateRequestMetrics(tracker, ups, "method2", 50, 5)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "*")

		assert.Equal(t, int64(150), metrics1.RequestsTotal.Load())
	})

	t.Run("LatencyCorrectP90AcrossMethods", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")
		simulateRequestMetricsWithLatency(tracker, ups, "method1", 10, 0.06)
		simulateRequestMetricsWithLatency(tracker, ups, "method2", 90, 0.02)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "*")
		qt := metrics1.ResponseQuantiles.GetQuantile(0.90).Seconds()
		assert.GreaterOrEqual(t, qt, 0.02)
		assert.LessOrEqual(t, qt, 0.021)
	})

	t.Run("LatencyCorrectP90SingleMethod", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, projectID, windowSize)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("a")
		simulateRequestMetricsWithLatency(tracker, ups, "method1", 10, 0.06)
		simulateRequestMetricsWithLatency(tracker, ups, "method1", 90, 0.02)

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.GreaterOrEqual(t, metrics1.ResponseQuantiles.GetQuantile(0.90).Seconds(), 0.02)
		assert.LessOrEqual(t, metrics1.ResponseQuantiles.GetQuantile(0.90).Seconds(), 0.03)
	})
}

func simulateRequestMetrics(tracker *Tracker, upstream common.Upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		if i < errors {
			tracker.RecordUpstreamFailure(upstream, method, fmt.Errorf("test problem"))
		}
	}
}

func simulateRateLimitedRequestMetrics(tracker *Tracker, upstream common.Upstream, method string, total, selfLimited, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		if i < selfLimited {
			tracker.RecordUpstreamSelfRateLimited(upstream, method)
		}
		if i >= selfLimited && i < selfLimited+remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(upstream, method)
		}
	}
}

func simulateRequestMetricsWithLatency(tracker *Tracker, upstream common.Upstream, method string, total int, latency float64) {
	wg := sync.WaitGroup{}
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordUpstreamRequest(upstream, method)
			tracker.RecordUpstreamDuration(upstream, method, time.Duration(latency*float64(time.Second)), true, "none", common.DataFinalityStateUnknown)
		}()
	}
	wg.Wait()
}

func resetMetrics() {
	if telemetry.MetricUpstreamRequestTotal != nil {
		telemetry.MetricUpstreamRequestTotal.Reset()
	}
	if telemetry.MetricUpstreamRequestDuration != nil {
		telemetry.MetricUpstreamRequestDuration.Reset()
	}
	if telemetry.MetricUpstreamErrorTotal != nil {
		telemetry.MetricUpstreamErrorTotal.Reset()
	}
	if telemetry.MetricUpstreamSelfRateLimitedTotal != nil {
		telemetry.MetricUpstreamSelfRateLimitedTotal.Reset()
	}
	if telemetry.MetricUpstreamRemoteRateLimitedTotal != nil {
		telemetry.MetricUpstreamRemoteRateLimitedTotal.Reset()
	}
}

func TestBlockHeadLagPersistsAcrossResets(t *testing.T) {
	projectID := "test-project"
	windowSize := 100 * time.Millisecond // Short window for faster testing

	tracker := NewTracker(&log.Logger, projectID, windowSize)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	ups1 := common.NewFakeUpstream("upstream1")
	ups2 := common.NewFakeUpstream("upstream2")

	// First, ensure TrackedMetrics exist by recording some requests
	tracker.RecordUpstreamRequest(ups1, "method1")
	tracker.RecordUpstreamRequest(ups2, "method1")
	tracker.RecordUpstreamFailure(ups1, "method1", fmt.Errorf("test error"))

	// Now set different block numbers to create lag
	tracker.SetLatestBlockNumber(ups1, 1000) // ups1 is at block 1000
	tracker.SetLatestBlockNumber(ups2, 990)  // ups2 is behind by 10 blocks

	// Get initial metrics AFTER setting block numbers
	metrics1Before := tracker.GetUpstreamMethodMetrics(ups1, "method1")
	metrics2Before := tracker.GetUpstreamMethodMetrics(ups2, "method1")

	// Debug: Print actual lag values
	t.Logf("Before reset - ups1 lag: %d, ups2 lag: %d",
		metrics1Before.BlockHeadLag.Load(),
		metrics2Before.BlockHeadLag.Load())

	// Verify initial state
	assert.Equal(t, int64(0), metrics1Before.BlockHeadLag.Load())  // ups1 is at network head
	assert.Equal(t, int64(10), metrics2Before.BlockHeadLag.Load()) // ups2 is 10 blocks behind
	assert.Equal(t, int64(1), metrics1Before.RequestsTotal.Load())
	assert.Equal(t, int64(1), metrics1Before.ErrorsTotal.Load())

	// Wait for reset cycle
	time.Sleep(windowSize + 50*time.Millisecond)

	// Get metrics after reset
	metrics1After := tracker.GetUpstreamMethodMetrics(ups1, "method1")
	metrics2After := tracker.GetUpstreamMethodMetrics(ups2, "method1")

	// Debug: Print actual lag values after reset
	t.Logf("After reset - ups1 lag: %d, ups2 lag: %d",
		metrics1After.BlockHeadLag.Load(),
		metrics2After.BlockHeadLag.Load())

	// Verify cumulative metrics were reset
	assert.Equal(t, int64(0), metrics1After.RequestsTotal.Load())
	assert.Equal(t, int64(0), metrics1After.ErrorsTotal.Load())
	assert.Equal(t, int64(0), metrics2After.RequestsTotal.Load())

	// Verify block head lag persisted (the key fix)
	assert.Equal(t, int64(0), metrics1After.BlockHeadLag.Load(), "upstream1 should still be at network head")
	assert.Equal(t, int64(10), metrics2After.BlockHeadLag.Load(), "upstream2 should still be 10 blocks behind")

	// Test that block lag can still be updated after reset
	tracker.SetLatestBlockNumber(ups2, 1000) // ups2 catches up
	metrics2Updated := tracker.GetUpstreamMethodMetrics(ups2, "method1")
	assert.Equal(t, int64(0), metrics2Updated.BlockHeadLag.Load(), "upstream2 should now be caught up")
}

func TestFinalizationLagPersistsAcrossResets(t *testing.T) {
	projectID := "test-project"
	windowSize := 100 * time.Millisecond // Short window for faster testing

	tracker := NewTracker(&log.Logger, projectID, windowSize)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	ups1 := common.NewFakeUpstream("upstream1")
	ups2 := common.NewFakeUpstream("upstream2")

	// First, ensure TrackedMetrics exist by recording some requests
	tracker.RecordUpstreamRequest(ups1, "method1")
	tracker.RecordUpstreamRequest(ups2, "method1")

	// Now set different finalized block numbers to create lag
	tracker.SetFinalizedBlockNumber(ups1, 900) // ups1 finalized at block 900
	tracker.SetFinalizedBlockNumber(ups2, 880) // ups2 finalized at block 880 (behind by 20)

	// Get initial metrics
	metrics1Before := tracker.GetUpstreamMethodMetrics(ups1, "method1")
	metrics2Before := tracker.GetUpstreamMethodMetrics(ups2, "method1")

	// Verify initial state
	assert.Equal(t, int64(0), metrics1Before.FinalizationLag.Load())  // ups1 is at network finalization head
	assert.Equal(t, int64(20), metrics2Before.FinalizationLag.Load()) // ups2 is 20 blocks behind
	assert.Equal(t, int64(1), metrics1Before.RequestsTotal.Load())

	// Wait for reset cycle
	time.Sleep(windowSize + 50*time.Millisecond)

	// Get metrics after reset
	metrics1After := tracker.GetUpstreamMethodMetrics(ups1, "method1")
	metrics2After := tracker.GetUpstreamMethodMetrics(ups2, "method1")

	// Verify cumulative metrics were reset
	assert.Equal(t, int64(0), metrics1After.RequestsTotal.Load())
	assert.Equal(t, int64(0), metrics2After.RequestsTotal.Load())

	// Verify finalization lag persisted (the key fix)
	assert.Equal(t, int64(0), metrics1After.FinalizationLag.Load(), "upstream1 should still be at network finalization head")
	assert.Equal(t, int64(20), metrics2After.FinalizationLag.Load(), "upstream2 should still be 20 blocks behind in finalization")

	// Test that finalization lag can still be updated after reset
	tracker.SetFinalizedBlockNumber(ups2, 900) // ups2 catches up
	metrics2Updated := tracker.GetUpstreamMethodMetrics(ups2, "method1")
	assert.Equal(t, int64(0), metrics2Updated.FinalizationLag.Load(), "upstream2 should now be caught up in finalization")
}
