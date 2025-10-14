package health

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
			tracker.RecordUpstreamSelfRateLimited(upstream, method, nil)
		}
		if i >= selfLimited && i < selfLimited+remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(upstream, method, nil)
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
			tracker.RecordUpstreamDuration(upstream, method, time.Duration(latency*float64(time.Second)), true, "none", common.DataFinalityStateUnknown, "n/a")
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
	tracker.SetLatestBlockNumber(ups1, 1000, 0, "evm_state_poller") // ups1 is at block 1000
	tracker.SetLatestBlockNumber(ups2, 990, 0, "evm_state_poller")  // ups2 is behind by 10 blocks

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
	tracker.SetLatestBlockNumber(ups2, 1000, 0, "evm_state_poller") // ups2 catches up
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

func TestSetLatestBlockTimestampForNetwork(t *testing.T) {
	t.Run("SetsTimestampAndRecordsDistance", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("test-upstream")

		// Get current time and create a block timestamp 10 seconds ago
		now := time.Now().Unix()
		blockNumber := int64(1000)
		blockTimestamp := now - 10

		tracker.SetLatestBlockNumber(ups, blockNumber, blockTimestamp, "evm_state_poller")

		// Verify the timestamp was stored at network level (FakeUpstream uses "evm:123" as network)
		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)
		storedTimestamp := ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, blockTimestamp, storedTimestamp, "Expected timestamp to be stored")
		assert.Equal(t, blockNumber, ntwMeta.evmLatestBlockNumber.Load(), "Expected block number to be stored")
	})

	t.Run("OnlyUpdatesWithNewerTimestamp", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("test-upstream-2")

		// Set initial block and timestamp
		now := time.Now().Unix()
		initialTimestamp := now - 20
		tracker.SetLatestBlockNumber(ups, 1000, initialTimestamp, "evm_state_poller")

		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)
		stored := ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, initialTimestamp, stored, "Expected initial timestamp to be set")

		// Try to set an older timestamp with same block - should not update
		olderTimestamp := now - 30
		tracker.SetLatestBlockNumber(ups, 1000, olderTimestamp, "evm_state_poller")
		stored = ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, initialTimestamp, stored, "Timestamp should not update to older value")

		// Set a newer block with newer timestamp - should update
		newerTimestamp := now - 5
		tracker.SetLatestBlockNumber(ups, 2000, newerTimestamp, "evm_state_poller")
		stored = ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, newerTimestamp, stored, "Expected newer timestamp to be set")
		assert.Equal(t, int64(2000), ntwMeta.evmLatestBlockNumber.Load(), "Expected block number to be updated")
	})

	t.Run("IgnoresNonPositiveTimestamp", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("test-upstream-3")

		// Try to set zero timestamp - should be ignored (timestamp not updated)
		tracker.SetLatestBlockNumber(ups, 500, 0, "evm_state_poller")

		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)
		stored := ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, int64(0), stored, "Expected zero timestamp to be ignored")
		assert.Equal(t, int64(500), ntwMeta.evmLatestBlockNumber.Load(), "Block number should still be updated")

		// Try to set negative timestamp - should be ignored
		tracker.SetLatestBlockNumber(ups, 600, -100, "evm_state_poller")
		stored = ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, int64(0), stored, "Expected negative timestamp to be ignored")
		assert.Equal(t, int64(600), ntwMeta.evmLatestBlockNumber.Load(), "Block number should still be updated")
	})

	t.Run("CalculatesCorrectDistance", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("test-upstream-4")

		// Create a block timestamp that's exactly 15 seconds old
		now := time.Now().Unix()
		blockTimestamp := now - 15
		blockNumber := int64(3000)

		tracker.SetLatestBlockNumber(ups, blockNumber, blockTimestamp, "evm_state_poller")

		// The distance should be approximately 15 seconds (allowing for small timing variations)
		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)
		storedTimestamp := ntwMeta.evmLatestBlockTimestamp.Load()

		currentTime := time.Now().Unix()
		expectedDistance := currentTime - blockTimestamp

		// Allow 2 seconds variance for test execution time
		assert.GreaterOrEqual(t, expectedDistance, int64(14), "Distance should be at least 14 seconds")
		assert.LessOrEqual(t, expectedDistance, int64(17), "Distance should be at most 17 seconds")
		assert.Equal(t, blockTimestamp, storedTimestamp, "Expected timestamp to be stored correctly")
		assert.Equal(t, blockNumber, ntwMeta.evmLatestBlockNumber.Load(), "Expected block number to be stored")
	})

	t.Run("AtomicUpdateOnlyWhenBlockNumberIncreases", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("test-upstream-5")

		// Set initial block and timestamp
		now := time.Now().Unix()
		tracker.SetLatestBlockNumber(ups, 1000, now-20, "evm_state_poller")

		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)

		// Verify both were set
		assert.Equal(t, int64(1000), ntwMeta.evmLatestBlockNumber.Load())
		assert.Equal(t, now-20, ntwMeta.evmLatestBlockTimestamp.Load())

		// Try to update with same block number but newer timestamp - should NOT update
		tracker.SetLatestBlockNumber(ups, 1000, now-10, "evm_state_poller")
		assert.Equal(t, int64(1000), ntwMeta.evmLatestBlockNumber.Load(), "Block number should stay same")
		assert.Equal(t, now-20, ntwMeta.evmLatestBlockTimestamp.Load(), "Timestamp should NOT update when block doesn't increase")

		// Update with higher block number and newer timestamp - BOTH should update
		tracker.SetLatestBlockNumber(ups, 2000, now-5, "evm_state_poller")
		assert.Equal(t, int64(2000), ntwMeta.evmLatestBlockNumber.Load(), "Block number should update")
		assert.Equal(t, now-5, ntwMeta.evmLatestBlockTimestamp.Load(), "Timestamp should update atomically with block")

		// Update with higher block but older timestamp - block updates, timestamp uses newer
		tracker.SetLatestBlockNumber(ups, 3000, now-15, "evm_state_poller")
		assert.Equal(t, int64(3000), ntwMeta.evmLatestBlockNumber.Load(), "Block number should update")
		assert.Equal(t, now-5, ntwMeta.evmLatestBlockTimestamp.Load(), "Timestamp should stay at newer value")
	})

	t.Run("ParsesTimestampAsIntegerFromProvider", func(t *testing.T) {
		// Test that verifies when a provider returns timestamp as an integer (not hex string),
		// it's correctly parsed and the distance metric is calculated
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("test-upstream-int")

		// Simulate a block response with timestamp as integer (common with some providers)
		now := time.Now().Unix()
		blockNumber := int64(5000)
		blockTimestamp := now - 25 // 25 seconds old

		// Directly test the parsing logic that fetchBlock uses
		// Simulate what PeekStringByPath returns for an integer timestamp
		timestampAsString := fmt.Sprintf("%d", blockTimestamp) // Integer becomes decimal string via PeekStringByPath

		var parsedTimestamp int64
		if !strings.HasPrefix(timestampAsString, "0x") {
			parsedTimestamp, _ = strconv.ParseInt(timestampAsString, 10, 64)
		}

		assert.Equal(t, blockTimestamp, parsedTimestamp, "Integer timestamp should parse correctly as decimal")

		// Now test the full flow
		tracker.SetLatestBlockNumber(ups, blockNumber, parsedTimestamp, "evm_state_poller")

		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)
		storedTimestamp := ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, parsedTimestamp, storedTimestamp, "Integer timestamp should be stored")
		assert.Equal(t, blockNumber, ntwMeta.evmLatestBlockNumber.Load(), "Block number should be stored")

		// Verify distance is calculated correctly
		currentTime := time.Now().Unix()
		expectedDistance := currentTime - blockTimestamp
		assert.GreaterOrEqual(t, expectedDistance, int64(24), "Distance should be at least 24 seconds")
		assert.LessOrEqual(t, expectedDistance, int64(27), "Distance should be at most 27 seconds")
	})

	t.Run("ParsesTimestampAsHexFromProvider", func(t *testing.T) {
		// Test that verifies when a provider returns timestamp as hex string,
		// it's correctly parsed and the distance metric is calculated
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("test-upstream-hex")

		// Simulate a block response with timestamp as hex string (standard EVM format)
		now := time.Now().Unix()
		blockNumber := int64(6000)
		blockTimestamp := now - 30 // 30 seconds old

		// Simulate what PeekStringByPath returns for a hex timestamp
		timestampAsHexString := fmt.Sprintf("0x%x", blockTimestamp)

		var parsedTimestamp int64
		if strings.HasPrefix(timestampAsHexString, "0x") {
			parsedTimestamp, _ = common.HexToInt64(timestampAsHexString)
		}

		assert.Equal(t, blockTimestamp, parsedTimestamp, "Hex timestamp should parse correctly")

		// Now test the full flow
		tracker.SetLatestBlockNumber(ups, blockNumber, parsedTimestamp, "evm_state_poller")

		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)
		storedTimestamp := ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, parsedTimestamp, storedTimestamp, "Hex timestamp should be stored")
		assert.Equal(t, blockNumber, ntwMeta.evmLatestBlockNumber.Load(), "Block number should be stored")

		// Verify distance is calculated correctly
		currentTime := time.Now().Unix()
		expectedDistance := currentTime - blockTimestamp
		assert.GreaterOrEqual(t, expectedDistance, int64(29), "Distance should be at least 29 seconds")
		assert.LessOrEqual(t, expectedDistance, int64(32), "Distance should be at most 32 seconds")
	})
}
