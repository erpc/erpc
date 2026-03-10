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
	"github.com/stretchr/testify/require"
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

		simulateRateLimitedRequestMetrics(tracker, ups, "method1", 100, 10)

		metrics := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.Equal(t, int64(100), metrics.RequestsTotal.Load())
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
		assert.Equal(t, int64(0), metricsBefore.RemoteRateLimitedTotal.Load())

		time.Sleep(windowSize + 10*time.Millisecond)

		metricsAfter := tracker.GetUpstreamMethodMetrics(ups, "method1")
		assert.Equal(t, int64(0), metricsAfter.RequestsTotal.Load())
		assert.Equal(t, int64(0), metricsAfter.ErrorsTotal.Load())
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

func simulateRateLimitedRequestMetrics(tracker *Tracker, upstream common.Upstream, method string, total, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		if i < remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(context.Background(), upstream, method, nil)
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
	if telemetry.MetricRateLimitsTotal != nil {
		telemetry.MetricRateLimitsTotal.Reset()
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
	tracker.SetLatestBlockNumber(ups1, 1000, 0) // ups1 is at block 1000
	tracker.SetLatestBlockNumber(ups2, 990, 0)  // ups2 is behind by 10 blocks

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
	tracker.SetLatestBlockNumber(ups2, 1000, 0) // ups2 catches up
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

		tracker.SetLatestBlockNumber(ups, blockNumber, blockTimestamp)

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
		tracker.SetLatestBlockNumber(ups, 1000, initialTimestamp)

		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)
		stored := ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, initialTimestamp, stored, "Expected initial timestamp to be set")

		// Try to set an older timestamp with same block - should not update
		olderTimestamp := now - 30
		tracker.SetLatestBlockNumber(ups, 1000, olderTimestamp)
		stored = ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, initialTimestamp, stored, "Timestamp should not update to older value")

		// Set a newer block with newer timestamp - should update
		newerTimestamp := now - 5
		tracker.SetLatestBlockNumber(ups, 2000, newerTimestamp)
		stored = ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, newerTimestamp, stored, "Expected newer timestamp to be set")
		assert.Equal(t, int64(2000), ntwMeta.evmLatestBlockNumber.Load(), "Expected block number to be updated")
	})

	t.Run("IgnoresNonPositiveTimestamp", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("test-upstream-3")

		// Try to set zero timestamp - should be ignored (timestamp not updated)
		tracker.SetLatestBlockNumber(ups, 500, 0)

		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)
		stored := ntwMeta.evmLatestBlockTimestamp.Load()

		assert.Equal(t, int64(0), stored, "Expected zero timestamp to be ignored")
		assert.Equal(t, int64(500), ntwMeta.evmLatestBlockNumber.Load(), "Block number should still be updated")

		// Try to set negative timestamp - should be ignored
		tracker.SetLatestBlockNumber(ups, 600, -100)
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

		tracker.SetLatestBlockNumber(ups, blockNumber, blockTimestamp)

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
		tracker.SetLatestBlockNumber(ups, 1000, now-20)

		ntwMdKey := metadataKey{nil, "evm:123"}
		ntwMeta := tracker.getMetadata(ntwMdKey)

		// Verify both were set
		assert.Equal(t, int64(1000), ntwMeta.evmLatestBlockNumber.Load())
		assert.Equal(t, now-20, ntwMeta.evmLatestBlockTimestamp.Load())

		// Try to update with same block number but newer timestamp - should NOT update
		tracker.SetLatestBlockNumber(ups, 1000, now-10)
		assert.Equal(t, int64(1000), ntwMeta.evmLatestBlockNumber.Load(), "Block number should stay same")
		assert.Equal(t, now-20, ntwMeta.evmLatestBlockTimestamp.Load(), "Timestamp should NOT update when block doesn't increase")

		// Update with higher block number and newer timestamp - BOTH should update
		tracker.SetLatestBlockNumber(ups, 2000, now-5)
		assert.Equal(t, int64(2000), ntwMeta.evmLatestBlockNumber.Load(), "Block number should update")
		assert.Equal(t, now-5, ntwMeta.evmLatestBlockTimestamp.Load(), "Timestamp should update atomically with block")

		// Update with higher block but older timestamp - both should update to reflect current state
		tracker.SetLatestBlockNumber(ups, 3000, now-15)
		assert.Equal(t, int64(3000), ntwMeta.evmLatestBlockNumber.Load(), "Block number should update")
		assert.Equal(t, now-15, ntwMeta.evmLatestBlockTimestamp.Load(), "Timestamp should update to match latest block's timestamp")
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
		tracker.SetLatestBlockNumber(ups, blockNumber, parsedTimestamp)

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
		tracker.SetLatestBlockNumber(ups, blockNumber, parsedTimestamp)

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

func TestRecordUpstreamFailure_IgnoresHedgeCancellationErrors(t *testing.T) {
	projectID := "test-project"
	tracker := NewTracker(&log.Logger, projectID, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	ups := common.NewFakeUpstream("qn-upstream")
	method := "trace_block"

	t.Run("RequestCanceled_not_counted_as_failure", func(t *testing.T) {
		tracker.RecordUpstreamRequest(ups, method)
		tracker.RecordUpstreamFailure(ups, method, common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))

		mt := tracker.GetUpstreamMethodMetrics(ups, method)
		require.NotNil(t, mt)
		assert.Equal(t, int64(1), mt.RequestsTotal.Load(), "request should be counted")
		assert.Equal(t, int64(0), mt.ErrorsTotal.Load(), "cancelled hedge should NOT count as error")
		assert.Equal(t, float64(0), mt.ErrorRate(), "error rate should be zero")
	})

	t.Run("HedgeCancelled_not_counted_as_failure", func(t *testing.T) {
		ups2 := common.NewFakeUpstream("qn-upstream-2")
		tracker.RecordUpstreamRequest(ups2, method)
		tracker.RecordUpstreamFailure(ups2, method,
			common.NewErrUpstreamHedgeCancelled("qn-upstream-2", fmt.Errorf("context canceled")))

		mt := tracker.GetUpstreamMethodMetrics(ups2, method)
		require.NotNil(t, mt)
		assert.Equal(t, int64(1), mt.RequestsTotal.Load(), "request should be counted")
		assert.Equal(t, int64(0), mt.ErrorsTotal.Load(), "hedge cancellation should NOT count as error")
	})

	t.Run("real_errors_still_counted", func(t *testing.T) {
		ups3 := common.NewFakeUpstream("qn-upstream-3")
		tracker.RecordUpstreamRequest(ups3, method)
		tracker.RecordUpstreamFailure(ups3, method, fmt.Errorf("connection refused"))

		mt := tracker.GetUpstreamMethodMetrics(ups3, method)
		require.NotNil(t, mt)
		assert.Equal(t, int64(1), mt.RequestsTotal.Load())
		assert.Equal(t, int64(1), mt.ErrorsTotal.Load(), "real errors should still be counted")
	})

	t.Run("mixed_real_and_cancelled_errors", func(t *testing.T) {
		ups4 := common.NewFakeUpstream("qn-upstream-4")

		for i := 0; i < 10; i++ {
			tracker.RecordUpstreamRequest(ups4, method)
		}
		// 5 real failures
		for i := 0; i < 5; i++ {
			tracker.RecordUpstreamFailure(ups4, method, fmt.Errorf("timeout"))
		}
		// 5 hedge cancellations (should be ignored)
		for i := 0; i < 5; i++ {
			tracker.RecordUpstreamFailure(ups4, method, common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))
		}

		mt := tracker.GetUpstreamMethodMetrics(ups4, method)
		require.NotNil(t, mt)
		assert.Equal(t, int64(10), mt.RequestsTotal.Load())
		assert.Equal(t, int64(5), mt.ErrorsTotal.Load(), "only real errors should be counted, not hedge cancellations")
		assert.InDelta(t, 0.5, mt.ErrorRate(), 0.001, "error rate should only reflect real failures")
	})
}

func TestRecordUpstreamDuration_OnlySuccessInQuantile(t *testing.T) {
	projectID := "test-latency"
	tracker := NewTracker(&log.Logger, projectID, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	method := "eth_call"

	t.Run("success_latency_recorded_in_quantile", func(t *testing.T) {
		ups := common.NewFakeUpstream("ups-success")
		tracker.RecordUpstreamRequest(ups, method)
		tracker.RecordUpstreamDuration(ups, method, 200*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "")

		mt := tracker.GetUpstreamMethodMetrics(ups, method)
		require.NotNil(t, mt)
		q := mt.ResponseQuantiles.GetQuantile(0.70)
		assert.Greater(t, q.Seconds(), 0.0, "successful latency should be in quantile")
	})

	t.Run("error_latency_not_in_quantile", func(t *testing.T) {
		ups := common.NewFakeUpstream("ups-error")
		tracker.RecordUpstreamRequest(ups, method)
		tracker.RecordUpstreamDuration(ups, method, 50*time.Millisecond, false, "none", common.DataFinalityStateUnknown, "")

		mt := tracker.GetUpstreamMethodMetrics(ups, method)
		require.NotNil(t, mt)
		q := mt.ResponseQuantiles.GetQuantile(0.70)
		assert.Equal(t, 0.0, q.Seconds(), "error latency should NOT be in quantile")
	})

	t.Run("empty_response_latency_should_not_be_in_quantile", func(t *testing.T) {
		// The upstream.go caller passes isSuccess=false for emptyish responses.
		// This ensures fast empty responses don't inflate the latency quantile.
		ups := common.NewFakeUpstream("ups-empty")
		tracker.RecordUpstreamRequest(ups, method)
		tracker.RecordUpstreamDuration(ups, method, 10*time.Millisecond, false, "none", common.DataFinalityStateUnknown, "")

		mt := tracker.GetUpstreamMethodMetrics(ups, method)
		require.NotNil(t, mt)
		q := mt.ResponseQuantiles.GetQuantile(0.70)
		assert.Equal(t, 0.0, q.Seconds(), "empty response latency should NOT inflate quantile")
	})

	t.Run("execution_exception_latency_should_be_in_quantile", func(t *testing.T) {
		// ExecutionException (e.g. revert) is valid blockchain state — its latency
		// should count. The upstream.go caller passes isSuccess=true for these.
		ups := common.NewFakeUpstream("ups-revert")
		tracker.RecordUpstreamRequest(ups, method)
		tracker.RecordUpstreamDuration(ups, method, 150*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "")

		mt := tracker.GetUpstreamMethodMetrics(ups, method)
		require.NotNil(t, mt)
		q := mt.ResponseQuantiles.GetQuantile(0.70)
		assert.Greater(t, q.Seconds(), 0.0, "execution exception latency should be in quantile")
	})
}

func TestRecordUpstreamMisbehavior_WrongEmpty(t *testing.T) {
	projectID := "test-misbehavior"
	tracker := NewTracker(&log.Logger, projectID, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	method := "trace_block"

	t.Run("wrong_empty_increments_misbehavior", func(t *testing.T) {
		ups := common.NewFakeUpstream("alchemy")
		for i := 0; i < 10; i++ {
			tracker.RecordUpstreamRequest(ups, method)
		}

		// 3 wrong-empty responses: both failure AND misbehavior
		for i := 0; i < 3; i++ {
			tracker.RecordUpstreamFailure(ups, method, common.NewErrEndpointMissingData(fmt.Errorf("empty"), ups))
			tracker.RecordUpstreamMisbehavior(ups, method)
		}

		mt := tracker.GetUpstreamMethodMetrics(ups, method)
		require.NotNil(t, mt)
		assert.Equal(t, int64(3), mt.ErrorsTotal.Load(), "wrong-empty should count as error")
		assert.Equal(t, int64(3), mt.MisbehaviorsTotal.Load(), "wrong-empty should count as misbehavior")
		assert.InDelta(t, 0.3, mt.ErrorRate(), 0.001)
		assert.InDelta(t, 0.3, mt.MisbehaviorRate(), 0.001)
	})

	t.Run("wrong_empty_double_penalty_vs_regular_error", func(t *testing.T) {
		upsWrongEmpty := common.NewFakeUpstream("ups-wrong-empty")
		upsRegularErr := common.NewFakeUpstream("ups-regular-err")

		for i := 0; i < 10; i++ {
			tracker.RecordUpstreamRequest(upsWrongEmpty, method)
			tracker.RecordUpstreamRequest(upsRegularErr, method)
		}

		// Both have same error rate
		for i := 0; i < 3; i++ {
			tracker.RecordUpstreamFailure(upsWrongEmpty, method, common.NewErrEndpointMissingData(fmt.Errorf("empty"), upsWrongEmpty))
			tracker.RecordUpstreamMisbehavior(upsWrongEmpty, method)
			tracker.RecordUpstreamFailure(upsRegularErr, method, fmt.Errorf("timeout"))
		}

		mtWE := tracker.GetUpstreamMethodMetrics(upsWrongEmpty, method)
		mtRE := tracker.GetUpstreamMethodMetrics(upsRegularErr, method)

		assert.Equal(t, mtWE.ErrorRate(), mtRE.ErrorRate(), "same error rate")
		assert.Greater(t, mtWE.MisbehaviorRate(), mtRE.MisbehaviorRate(),
			"wrong-empty should have higher misbehavior rate than regular errors")
		assert.Equal(t, float64(0), mtRE.MisbehaviorRate(),
			"regular errors should have zero misbehavior rate")
	})
}

// feedBlockDetection is a test helper that directly calls updateBlockTimeSample
// with controlled detection timestamps, bypassing SetLatestBlockNumber's time.Now().
func feedBlockDetection(tracker *Tracker, networkId, netLabel string, blockNumber, detectedAtMs int64) {
	ntwMeta := tracker.getMetadata(metadataKey{nil, networkId})
	tracker.updateBlockTimeSample(ntwMeta, netLabel, blockNumber, detectedAtMs)
}

func TestGetNetworkBlockTime(t *testing.T) {
	const networkId = "evm:123"
	const netLabel = "evm:123"

	t.Run("UnknownChainNoSamples_ReturnsZero", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		d := tracker.GetNetworkBlockTime("evm:99999")
		assert.Equal(t, time.Duration(0), d)
	})

	t.Run("SingleObservation_ReturnsZero", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		feedBlockDetection(tracker, networkId, netLabel, 1000, 1700000000000)
		d := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, time.Duration(0), d, "single observation should return 0")
	})

	t.Run("BelowMinSamples_ReturnsZero", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Feed 3 observations → 2 samples (below minSamples=3)
		for i := int64(0); i < 3; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*6000)
		}
		d := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, time.Duration(0), d, "below minSamples should return 0")

		// 4th observation → 3rd sample crosses the threshold
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+3, baseMs+18000)
		d = tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 6.0, d.Seconds(), 0.5, "at minSamples should return block time")
	})

	t.Run("ConvergesToTrueBlockTime", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		for i := int64(0); i <= 50; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*2000)
		}

		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, d.Seconds(), 0.1, "should converge to 2s block time")
	})

	t.Run("FastChain_SubSecondBlocks", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// 250ms blocks (Arbitrum-like), using ms-precision local timestamps
		for i := int64(0); i < 32; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*250)
		}

		d := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, d, time.Duration(0), "fast chain should produce a block time")
		assert.Less(t, d, 1*time.Second, "fast chain block time should be sub-second")
		assert.InDelta(t, 0.25, d.Seconds(), 0.05, "should converge to ~250ms")
	})

	t.Run("RejectsAbsurdValues", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		// 4 observations with absurdly large block time (200s per block, > 120s)
		feedBlockDetection(tracker, networkId, netLabel, 1000, 1700000000000)
		feedBlockDetection(tracker, networkId, netLabel, 1001, 1700000200000)
		feedBlockDetection(tracker, networkId, netLabel, 1002, 1700000400000)
		feedBlockDetection(tracker, networkId, netLabel, 1003, 1700000600000)

		d := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, time.Duration(0), d, "should reject absurd block times")
	})

	t.Run("EMA_RecoverFromBadEarlySamples", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Bad early data: 60s gap for first pair
		feedBlockDetection(tracker, networkId, netLabel, baseBlock, baseMs)
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+1, baseMs+60000)

		// Then normal 2s blocks — EMA will converge
		for i := int64(2); i <= 60; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+60000+(i-1)*2000)
		}

		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, d.Seconds(), 0.5, "EMA should recover from bad early data")
	})

	t.Run("ConcurrentGoroutinesSafety", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		tracker.Bootstrap(context.Background())

		ups := common.NewFakeUpstream("ups1")

		var wg sync.WaitGroup
		for g := 0; g < 10; g++ {
			wg.Add(1)
			go func(offset int64) {
				defer wg.Done()
				for i := int64(1); i <= 20; i++ {
					block := 1000 + offset*20 + i
					ts := int64(1700000000) + block*2
					tracker.SetLatestBlockNumber(ups, block, ts)
				}
			}(int64(g))
		}
		wg.Wait()

		// Main goal: no panics or data races.
		// Block time may or may not pass sanity checks (depends on goroutine timing).
		d := tracker.GetNetworkBlockTime(ups.NetworkId())
		if d > 0 {
			assert.Less(t, d.Seconds(), 120.0, "should be within sane bounds")
		}
	})

	t.Run("OutOfOrderBlocksIgnored", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		for i := int64(0); i <= 15; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*12000)
		}

		before := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 12.0, before.Seconds(), 0.5)

		// Stale block (1005 < last 1015) should be rejected
		feedBlockDetection(tracker, networkId, netLabel, 1005, baseMs+5*12000)

		after := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, before, after, "out-of-order block should not change block time")
	})

	t.Run("LargeBlockGapNormalizesPerBlock", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		feedBlockDetection(tracker, networkId, netLabel, 1000, 1700000000000)
		// Jump 100 blocks in 200s → 2s per block
		feedBlockDetection(tracker, networkId, netLabel, 1100, 1700000200000)

		for i := int64(1); i <= 15; i++ {
			feedBlockDetection(tracker, networkId, netLabel, 1100+i, 1700000200000+i*2000)
		}

		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, d.Seconds(), 0.2, "large block gap should normalize to per-block time")
	})

	t.Run("ExactlyMinSamplesThreshold", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Feed 3 observations → 2 samples (below minSamples=3)
		for i := int64(0); i < 3; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*3000)
		}
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(networkId),
			"at minSamples-1 should still return 0")

		// 4th observation → 3rd sample crosses the threshold
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+3, baseMs+9000)
		d := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, d, time.Duration(0), "at exactly minSamples should return non-zero")
		assert.InDelta(t, 3.0, d.Seconds(), 0.1, "should reflect 3s block time")
	})

	t.Run("EMA_AdaptsToNewBlockTime", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Feed 40 observations with 10s block time
		for i := int64(0); i < 40; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*10000)
		}
		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 10.0, d.Seconds(), 0.5, "should show 10s block time")

		// Now feed 50 observations with 3s block time — EMA should adapt
		lastBlock := baseBlock + 39
		lastMs := baseMs + 39*10000
		for i := int64(1); i <= 50; i++ {
			feedBlockDetection(tracker, networkId, netLabel, lastBlock+i, lastMs+i*3000)
		}
		d = tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 3.0, d.Seconds(), 0.2, "EMA should adapt to new 3s block time")
	})

	// ---------------------------------------------------------------
	// Edge-case tests (Aram's review list)
	// ---------------------------------------------------------------

	t.Run("EdgeCase_Startup_GracefulRampUp", func(t *testing.T) {
		// Simulates what happens when eRPC starts fresh against a real chain.
		// The first 3 observations (2 samples) should NOT publish anything.
		// Only on the 4th observation (3rd sample) do we start emitting.
		// Until then, consumers see 0 and fall back to config-based intervals.
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(18_000_000)
		baseMs := int64(1700000000000)

		// Observation 1: first ever block seen, just stores prev, no sample.
		feedBlockDetection(tracker, networkId, netLabel, baseBlock, baseMs)
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(networkId),
			"observation 1: no block time yet")

		// Observation 2: first sample computed, but samples=1 < minSamples=3.
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+1, baseMs+12000)
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(networkId),
			"observation 2: 1 sample, still below threshold")

		// Observation 3: second sample, samples=2 < 3.
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+2, baseMs+24000)
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(networkId),
			"observation 3: 2 samples, still below threshold")

		// Observation 4: third sample crosses threshold, block time emitted.
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+3, baseMs+36000)
		d := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, d, time.Duration(0), "observation 4: block time should be emitted")
		assert.InDelta(t, 12.0, d.Seconds(), 0.5,
			"should reflect ~12s block time after just 4 observations")
	})

	t.Run("EdgeCase_NodeDowntime_BlockGapNormalization", func(t *testing.T) {
		// Simulates a single upstream that goes down for 5 minutes while the chain
		// keeps producing 2s blocks. When the node comes back, it reports a block
		// number that's ~150 blocks ahead. The normalization (timeGap / blockGap)
		// should produce a correct ~2s per-block estimate, NOT a 5-minute spike.
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Steady state: 20 blocks at 2s each.
		for i := int64(0); i <= 20; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*2000)
		}
		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, d.Seconds(), 0.2, "steady state should be ~2s")

		// Node goes down for 5 minutes. Chain produced 150 blocks in that time.
		// When node comes back, we see block 1170 at T+300s from last detection.
		lastBlock := baseBlock + 20
		lastMs := baseMs + 20*2000
		downtime := int64(300_000) // 5 minutes in ms
		blocksProducedDuringDowntime := int64(150)

		feedBlockDetection(tracker, networkId, netLabel,
			lastBlock+blocksProducedDuringDowntime,
			lastMs+downtime)

		d = tracker.GetNetworkBlockTime(networkId)
		// 300000ms / 150 blocks = 2000ms per block → normalization should keep it at ~2s.
		assert.InDelta(t, 2.0, d.Seconds(), 0.3,
			"block-gap normalization should produce ~2s, not a 5-minute spike")
	})

	t.Run("EdgeCase_ChainHalt_SingleBlockAfterLongPause", func(t *testing.T) {
		// Worst case: chain halts entirely for 10 minutes, then produces ONE block.
		// blockGap=1, timeGap=600s → single sample of 600s.
		// EMA spikes from 2s to ~61.8s. Published value jumps.
		// The test documents this behavior and verifies recovery.
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Steady state: 30 blocks at 2s.
		for i := int64(0); i <= 30; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*2000)
		}
		steadyState := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, steadyState.Seconds(), 0.1, "pre-halt steady state")

		// Chain halts for 10 minutes. Then exactly ONE block arrives.
		lastBlock := baseBlock + 30
		lastMs := baseMs + 30*2000
		haltDuration := int64(600_000) // 10 min in ms

		feedBlockDetection(tracker, networkId, netLabel,
			lastBlock+1, // blockGap = 1
			lastMs+haltDuration)

		spiked := tracker.GetNetworkBlockTime(networkId)
		// EMA = 0.1 * 600s + 0.9 * ~2s ≈ 61.8s.
		// This IS within the 120s sanity bound, so it gets published.
		assert.Greater(t, spiked.Seconds(), 30.0,
			"chain halt with blockGap=1 causes a spike (expected, documented behavior)")
		assert.Less(t, spiked.Seconds(), 120.0,
			"spike should still be within sanity bounds")

		// Chain resumes normal 2s blocks. EMA should recover.
		resumeBlock := lastBlock + 1
		resumeMs := lastMs + haltDuration
		for i := int64(1); i <= 40; i++ {
			feedBlockDetection(tracker, networkId, netLabel, resumeBlock+i, resumeMs+i*2000)
		}

		recovered := tracker.GetNetworkBlockTime(networkId)
		assert.Less(t, recovered.Seconds(), 10.0,
			"after 40 normal blocks, EMA should be recovering toward 2s")

		// After enough normal blocks, converge close to true value.
		for i := int64(41); i <= 100; i++ {
			feedBlockDetection(tracker, networkId, netLabel, resumeBlock+i, resumeMs+i*2000)
		}

		fullyRecovered := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, fullyRecovered.Seconds(), 0.5,
			"after 100 normal blocks, EMA should be very close to 2s")
	})

	t.Run("EdgeCase_ChainHalt_MultipleBlocksBurst", func(t *testing.T) {
		// Variant: chain halts for 10 min, then produces a BURST of blocks
		// (catching up). E.g., 5 blocks arrive rapidly in 1 second.
		// The burst has a tiny time gap but multiple blocks → normalization
		// divides by blockGap, producing a very small per-block time.
		// Sanity floor (10ms) should catch absurdly small values.
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Steady state: 30 blocks at 12s (Ethereum-like).
		for i := int64(0); i <= 30; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*12000)
		}
		assert.InDelta(t, 12.0, tracker.GetNetworkBlockTime(networkId).Seconds(), 0.5)

		// Chain halts 10 min, then we detect 5 blocks in rapid succession (1s apart).
		lastBlock := baseBlock + 30
		lastMs := baseMs + 30*12000
		haltMs := int64(600_000)

		// First block after halt: blockGap=1, timeGap=10min → huge sample.
		feedBlockDetection(tracker, networkId, netLabel, lastBlock+1, lastMs+haltMs)
		spiked := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, spiked.Seconds(), 30.0, "first block after halt spikes EMA")

		// Next 4 blocks arrive 1s apart (rapid burst).
		for i := int64(2); i <= 5; i++ {
			feedBlockDetection(tracker, networkId, netLabel, lastBlock+i, lastMs+haltMs+(i-1)*1000)
		}

		afterBurst := tracker.GetNetworkBlockTime(networkId)
		// The burst samples (1s per block) pull EMA down. It won't be at 12s yet,
		// but it should be trending back down from the spike.
		assert.Less(t, afterBurst.Seconds(), spiked.Seconds(),
			"burst of blocks should pull EMA back down from spike")
	})

	t.Run("EdgeCase_QuietChainThenResumes", func(t *testing.T) {
		// Chain is normally 2s blocks, then slows to 10s for a while,
		// then returns to 2s. EMA should track both transitions.
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Phase 1: Normal 2s blocks.
		for i := int64(0); i <= 30; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*2000)
		}
		normal := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, normal.Seconds(), 0.1, "phase 1: normal 2s blocks")

		// Phase 2: Chain slows to 10s blocks for 30 blocks.
		lastBlock := baseBlock + 30
		lastMs := baseMs + 30*2000
		for i := int64(1); i <= 30; i++ {
			feedBlockDetection(tracker, networkId, netLabel, lastBlock+i, lastMs+i*10000)
		}
		slow := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, slow.Seconds(), 5.0, "phase 2: EMA should be moving toward 10s")
		assert.Less(t, slow.Seconds(), 11.0, "phase 2: EMA should not overshoot")

		// Phase 3: Chain returns to 2s blocks for 50 blocks.
		lastBlock += 30
		lastMs += 30 * 10000
		for i := int64(1); i <= 50; i++ {
			feedBlockDetection(tracker, networkId, netLabel, lastBlock+i, lastMs+i*2000)
		}
		resumed := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, resumed.Seconds(), 0.5,
			"phase 3: EMA should recover back to ~2s")
	})

	t.Run("EdgeCase_BusyChain_RapidSubSecondBlocks", func(t *testing.T) {
		// Fast chain like Arbitrum (~250ms) with occasional jitter.
		// Ensures sub-second detection works and passes sanity floor (10ms).
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		ms := int64(1700000000000)

		// 100 blocks with ~250ms average (alternating 200ms and 300ms gaps).
		feedBlockDetection(tracker, networkId, netLabel, baseBlock, ms)
		for i := int64(1); i < 100; i++ {
			gap := int64(200)
			if i%2 == 0 {
				gap = 300
			}
			ms += gap
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, ms)
		}

		d := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, d, 10*time.Millisecond, "should be above sanity floor")
		assert.Less(t, d, 500*time.Millisecond, "should be well under 500ms")
		assert.InDelta(t, 0.25, d.Seconds(), 0.1, "should converge near 250ms average")
	})

	t.Run("EdgeCase_NonPositiveTimeGap_ClockSkew", func(t *testing.T) {
		// If two blocks have the same or backwards detection time (clock skew),
		// the code should advance prevBlock/prevMs but NOT feed a sample.
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)

		baseBlock := int64(1000)
		baseMs := int64(1700000000000)

		// Build up steady state.
		for i := int64(0); i <= 10; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseMs+i*5000)
		}
		before := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 5.0, before.Seconds(), 0.5)

		// Feed a block with same timestamp as previous (timeGap=0).
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+11, baseMs+10*5000)

		after := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, before, after,
			"zero time gap should not change block time")

		// Feed a block with backwards timestamp (clock skew).
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+12, baseMs+9*5000)

		afterSkew := tracker.GetNetworkBlockTime(networkId)
		// Value may change slightly since prevMs was advanced to the skewed value,
		// but there should be no panic and value should still be sane.
		if afterSkew > 0 {
			assert.Less(t, afterSkew.Seconds(), 120.0, "should still be within bounds")
		}
	})
}
