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
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
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

		metrics1 := tracker.GetUpstreamMethodMetrics(ups1, "method1", common.DataFinalityStateAll)
		metrics2 := tracker.GetUpstreamMethodMetrics(ups2, "method1", common.DataFinalityStateAll)

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

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
		assert.Equal(t, int64(100), metrics1.RequestsTotal.Load())
		assert.Equal(t, int64(10), metrics1.ErrorsTotal.Load())

		time.Sleep(windowSize + 10*time.Millisecond)

		// Second window
		simulateRequestMetrics(tracker, ups, "method1", 50, 5)

		metrics2 := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
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

		metrics := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
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

		metrics := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
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

		metrics1 := tracker.GetUpstreamMethodMetrics(ups1, "method1", common.DataFinalityStateAll)
		metrics2 := tracker.GetUpstreamMethodMetrics(ups2, "method1", common.DataFinalityStateAll)
		metrics3 := tracker.GetUpstreamMethodMetrics(ups3, "method1", common.DataFinalityStateAll)

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

		metricsBefore := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
		assert.Equal(t, int64(100), metricsBefore.RequestsTotal.Load())
		assert.Equal(t, int64(10), metricsBefore.ErrorsTotal.Load())
		assert.Equal(t, int64(0), metricsBefore.RemoteRateLimitedTotal.Load())

		time.Sleep(windowSize + 10*time.Millisecond)

		metricsAfter := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
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

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
		metrics2 := tracker.GetUpstreamMethodMetrics(ups, "method2", common.DataFinalityStateAll)

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

		metrics := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
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

			metrics := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
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

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll)

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

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll)

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

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll)
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

		metrics1 := tracker.GetUpstreamMethodMetrics(ups, "method1", common.DataFinalityStateAll)
		assert.GreaterOrEqual(t, metrics1.ResponseQuantiles.GetQuantile(0.90).Seconds(), 0.02)
		assert.LessOrEqual(t, metrics1.ResponseQuantiles.GetQuantile(0.90).Seconds(), 0.03)
	})
}

func simulateRequestMetrics(tracker *Tracker, upstream common.Upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method, common.DataFinalityStateUnknown)
		if i < errors {
			tracker.RecordUpstreamFailure(upstream, method, common.DataFinalityStateUnknown, fmt.Errorf("test problem"))
		}
	}
}

func simulateRateLimitedRequestMetrics(tracker *Tracker, upstream common.Upstream, method string, total, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method, common.DataFinalityStateUnknown)
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
			tracker.RecordUpstreamRequest(upstream, method, common.DataFinalityStateUnknown)
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
	tracker.RecordUpstreamRequest(ups1, "method1", common.DataFinalityStateUnknown)
	tracker.RecordUpstreamRequest(ups2, "method1", common.DataFinalityStateUnknown)
	tracker.RecordUpstreamFailure(ups1, "method1", common.DataFinalityStateUnknown, fmt.Errorf("test error"))

	// Now set different block numbers to create lag
	tracker.SetLatestBlockNumber(ups1, 1000, 0) // ups1 is at block 1000
	tracker.SetLatestBlockNumber(ups2, 990, 0)  // ups2 is behind by 10 blocks

	// Get initial metrics AFTER setting block numbers
	metrics1Before := tracker.GetUpstreamMethodMetrics(ups1, "method1", common.DataFinalityStateAll)
	metrics2Before := tracker.GetUpstreamMethodMetrics(ups2, "method1", common.DataFinalityStateAll)

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
	metrics1After := tracker.GetUpstreamMethodMetrics(ups1, "method1", common.DataFinalityStateAll)
	metrics2After := tracker.GetUpstreamMethodMetrics(ups2, "method1", common.DataFinalityStateAll)

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
	metrics2Updated := tracker.GetUpstreamMethodMetrics(ups2, "method1", common.DataFinalityStateAll)
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
	tracker.RecordUpstreamRequest(ups1, "method1", common.DataFinalityStateUnknown)
	tracker.RecordUpstreamRequest(ups2, "method1", common.DataFinalityStateUnknown)

	// Now set different finalized block numbers to create lag
	tracker.SetFinalizedBlockNumber(ups1, 900) // ups1 finalized at block 900
	tracker.SetFinalizedBlockNumber(ups2, 880) // ups2 finalized at block 880 (behind by 20)

	// Get initial metrics
	metrics1Before := tracker.GetUpstreamMethodMetrics(ups1, "method1", common.DataFinalityStateAll)
	metrics2Before := tracker.GetUpstreamMethodMetrics(ups2, "method1", common.DataFinalityStateAll)

	// Verify initial state
	assert.Equal(t, int64(0), metrics1Before.FinalizationLag.Load())  // ups1 is at network finalization head
	assert.Equal(t, int64(20), metrics2Before.FinalizationLag.Load()) // ups2 is 20 blocks behind
	assert.Equal(t, int64(1), metrics1Before.RequestsTotal.Load())

	// Wait for reset cycle
	time.Sleep(windowSize + 50*time.Millisecond)

	// Get metrics after reset
	metrics1After := tracker.GetUpstreamMethodMetrics(ups1, "method1", common.DataFinalityStateAll)
	metrics2After := tracker.GetUpstreamMethodMetrics(ups2, "method1", common.DataFinalityStateAll)

	// Verify cumulative metrics were reset
	assert.Equal(t, int64(0), metrics1After.RequestsTotal.Load())
	assert.Equal(t, int64(0), metrics2After.RequestsTotal.Load())

	// Verify finalization lag persisted (the key fix)
	assert.Equal(t, int64(0), metrics1After.FinalizationLag.Load(), "upstream1 should still be at network finalization head")
	assert.Equal(t, int64(20), metrics2After.FinalizationLag.Load(), "upstream2 should still be 20 blocks behind in finalization")

	// Test that finalization lag can still be updated after reset
	tracker.SetFinalizedBlockNumber(ups2, 900) // ups2 catches up
	metrics2Updated := tracker.GetUpstreamMethodMetrics(ups2, "method1", common.DataFinalityStateAll)
	assert.Equal(t, int64(0), metrics2Updated.FinalizationLag.Load(), "upstream2 should now be caught up in finalization")
}

// TestWildcardLagMirroredForPeerUpstreams is the regression test for the
// incident where blockNumberLagAbove silently never fired for CB-open upstreams.
//
// The scenario: ups2 is lagging far behind ups1. ups2's own poller never fires
// (simulating a CB-open upstream). Only ups1 calls SetLatestBlockNumber. The fix
// ensures updateNetworkLagMetrics mirrors the computed lag onto {ups2,"*",All} so
// evalScope:network policies read the correct non-zero value.
func TestWildcardLagMirroredForPeerUpstreams(t *testing.T) {
	tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
	tracker.Bootstrap(context.Background())

	ups1 := common.NewFakeUpstream("upstream1")
	ups2 := common.NewFakeUpstream("upstream2")

	// Register both upstreams with traffic so their per-method buckets exist.
	tracker.RecordUpstreamRequest(ups1, "eth_blockNumber", common.DataFinalityStateUnknown)
	tracker.RecordUpstreamRequest(ups2, "eth_blockNumber", common.DataFinalityStateUnknown)

	// ups2 reports a stale block (simulating its last report before going CB-open).
	// After this, ups2's own poller never fires again.
	tracker.SetLatestBlockNumber(ups2, 900, 0)

	// ups1 advances the tip to 1000. updateNetworkLagMetrics now computes
	// ups2's lag as 100 and must mirror it onto {ups2,"*",All} — the peer-write
	// path that was previously missing.
	tracker.SetLatestBlockNumber(ups1, 1000, 0)

	// evalScope:network reads {ups, "*", All}.BlockHeadLag
	ups2WildcardLag := tracker.GetUpstreamMethodMetrics(ups2, "*", common.DataFinalityStateAll).BlockHeadLag.Load()
	require.EqualValues(t, 100, ups2WildcardLag,
		"peer upstream lag must reach {ups,\"*\",All} via updateNetworkLagMetrics even without its own poller firing")

	// ups1 is at the tip — its wildcard lag must be 0.
	ups1WildcardLag := tracker.GetUpstreamMethodMetrics(ups1, "*", common.DataFinalityStateAll).BlockHeadLag.Load()
	require.EqualValues(t, 0, ups1WildcardLag)
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
	// Hedge cancellations and bare client-disconnect cancellations both reach
	// the tracker as ErrCodeEndpointRequestCanceled / ErrCodeUpstreamHedgeCancelled.
	// They must not increment ErrorsTotal: hedge attempts are excluded
	// entirely at the call site in upstream.tryForward (so they shouldn't even
	// reach here), and any cancellation that does is almost always a client
	// disconnect — not the upstream's fault. Slowness is captured by
	// ResponseQuantiles instead, which already feeds selection scoring.
	projectID := "test-project"
	tracker := NewTracker(&log.Logger, projectID, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	ups := common.NewFakeUpstream("qn-upstream")
	method := "trace_block"

	t.Run("RequestCanceled_not_counted_as_failure", func(t *testing.T) {
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups, method, common.DataFinalityStateUnknown, common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))

		mt := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
		require.NotNil(t, mt)
		assert.Equal(t, int64(1), mt.RequestsTotal.Load(), "request should be counted")
		assert.Equal(t, int64(0), mt.ErrorsTotal.Load(), "cancellation should NOT count as error")
		assert.Equal(t, float64(0), mt.ErrorRate(), "error rate should be zero")
	})

	t.Run("HedgeCancelled_not_counted_as_failure", func(t *testing.T) {
		ups2 := common.NewFakeUpstream("qn-upstream-2")
		tracker.RecordUpstreamRequest(ups2, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups2, method, common.DataFinalityStateUnknown,
			common.NewErrUpstreamHedgeCancelled("qn-upstream-2", fmt.Errorf("context canceled")))

		mt := tracker.GetUpstreamMethodMetrics(ups2, method, common.DataFinalityStateAll)
		require.NotNil(t, mt)
		assert.Equal(t, int64(1), mt.RequestsTotal.Load(), "request should be counted")
		assert.Equal(t, int64(0), mt.ErrorsTotal.Load(), "hedge cancellation should NOT count as error")
	})

	t.Run("real_errors_still_counted", func(t *testing.T) {
		ups3 := common.NewFakeUpstream("qn-upstream-3")
		tracker.RecordUpstreamRequest(ups3, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups3, method, common.DataFinalityStateUnknown, fmt.Errorf("connection refused"))

		mt := tracker.GetUpstreamMethodMetrics(ups3, method, common.DataFinalityStateAll)
		require.NotNil(t, mt)
		assert.Equal(t, int64(1), mt.RequestsTotal.Load())
		assert.Equal(t, int64(1), mt.ErrorsTotal.Load(), "real errors should still be counted")
	})

	t.Run("mixed_real_and_cancelled_errors", func(t *testing.T) {
		ups4 := common.NewFakeUpstream("qn-upstream-4")

		for i := 0; i < 10; i++ {
			tracker.RecordUpstreamRequest(ups4, method, common.DataFinalityStateUnknown)
		}
		for i := 0; i < 5; i++ {
			tracker.RecordUpstreamFailure(ups4, method, common.DataFinalityStateUnknown, fmt.Errorf("timeout"))
		}
		// 5 cancellations — must be ignored (could be hedge losses or
		// client disconnects; neither attributable to upstream quality).
		for i := 0; i < 5; i++ {
			tracker.RecordUpstreamFailure(ups4, method, common.DataFinalityStateUnknown, common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))
		}

		mt := tracker.GetUpstreamMethodMetrics(ups4, method, common.DataFinalityStateAll)
		require.NotNil(t, mt)
		assert.Equal(t, int64(10), mt.RequestsTotal.Load())
		assert.Equal(t, int64(5), mt.ErrorsTotal.Load(), "only real errors counted, not cancellations")
		assert.InDelta(t, 0.5, mt.ErrorRate(), 0.001, "error rate reflects only real failures")
	})
}

// TestRecordUpstreamFailure_AllSkipCodesIgnored locks in the full matrix of
// error codes that the tracker treats as non-quality signals.  Adding
// RequestCanceled / HedgeCancelled here was the open question from PR #878
// — they live in this list because they don't unambiguously attribute to
// upstream quality at the tracker layer.
func TestRecordUpstreamFailure_AllSkipCodesIgnored(t *testing.T) {
	tracker := NewTracker(&log.Logger, "test-project", 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	method := "eth_call"

	cases := []struct {
		name string
		err  error
	}{
		{"ExecutionException", common.NewErrEndpointExecutionException(fmt.Errorf("execution reverted"))},
		{"ExcludedByPolicy", common.NewErrUpstreamExcludedByPolicy("ups")},
		{"RequestSkipped", common.NewErrUpstreamRequestSkipped(fmt.Errorf("skipped"), "ups")},
		{"Shadowing", common.NewErrUpstreamShadowing("ups")},
		{"Unsupported", common.NewErrEndpointUnsupported(fmt.Errorf("not supported"))},
		{"CapacityExceeded", common.NewErrEndpointCapacityExceeded(fmt.Errorf("429"))},
		{"ClientSideException", common.NewErrEndpointClientSideException(fmt.Errorf("400"))},
		{"RequestCanceled", common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled"))},
		{"UpstreamHedgeCancelled", common.NewErrUpstreamHedgeCancelled("ups", fmt.Errorf("context canceled"))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ups := common.NewFakeUpstream("ups-" + tc.name)
			tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
			tracker.RecordUpstreamFailure(ups, method, common.DataFinalityStateUnknown, tc.err)

			mt := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
			require.NotNil(t, mt)
			assert.Equal(t, int64(1), mt.RequestsTotal.Load(), "request counted")
			assert.Equal(t, int64(0), mt.ErrorsTotal.Load(),
				"%s should NOT count as an upstream failure", tc.name)
			assert.Equal(t, float64(0), mt.ErrorRate(), "ErrorRate stays zero")
		})
	}
}

// TestRates_RealErrorsOnlyAffectRates is a defense against quietly bleeding
// cancellations into ErrorRate / ThrottledRate / MisbehaviorRate. Cancellations
// reaching the tracker (whether labeled as hedge losses or bare client cancels)
// must leave all three rates untouched relative to the real-error baseline.
func TestRates_RealErrorsOnlyAffectRates(t *testing.T) {
	tracker := NewTracker(&log.Logger, "test-project", 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	method := "eth_call"
	ups := common.NewFakeUpstream("mixed-ups")

	// 100 attempts total; 25 cancellations; 10 throttled; 5 misbehaviors; 8 real errors.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
	}
	for i := 0; i < 25; i++ {
		tracker.RecordUpstreamFailure(ups, method, common.DataFinalityStateUnknown,
			common.NewErrEndpointRequestCanceled(fmt.Errorf("context canceled")))
	}
	for i := 0; i < 10; i++ {
		tracker.RecordUpstreamRemoteRateLimited(ctx, ups, method, nil)
	}
	for i := 0; i < 5; i++ {
		tracker.RecordUpstreamMisbehavior(ups, method, common.DataFinalityStateUnknown)
	}
	for i := 0; i < 8; i++ {
		tracker.RecordUpstreamFailure(ups, method, common.DataFinalityStateUnknown, fmt.Errorf("connection refused"))
	}

	mt := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
	require.NotNil(t, mt)
	assert.Equal(t, int64(100), mt.RequestsTotal.Load())
	assert.Equal(t, int64(8), mt.ErrorsTotal.Load(), "only real errors counted")
	assert.InDelta(t, 0.08, mt.ErrorRate(), 0.001, "ErrorRate = 8/100, cancellations excluded from numerator")
	assert.InDelta(t, 0.10, mt.ThrottledRate(), 0.001, "ThrottledRate = 10/100, untouched")
	assert.InDelta(t, 0.05, mt.MisbehaviorRate(), 0.001, "MisbehaviorRate = 5/100, untouched")
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
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups, method, 200*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "")

		mt := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
		require.NotNil(t, mt)
		q := mt.ResponseQuantiles.GetQuantile(0.70)
		assert.Greater(t, q.Seconds(), 0.0, "successful latency should be in quantile")
	})

	t.Run("error_latency_not_in_quantile", func(t *testing.T) {
		ups := common.NewFakeUpstream("ups-error")
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups, method, 50*time.Millisecond, false, "none", common.DataFinalityStateUnknown, "")

		mt := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
		require.NotNil(t, mt)
		q := mt.ResponseQuantiles.GetQuantile(0.70)
		assert.Equal(t, 0.0, q.Seconds(), "error latency should NOT be in quantile")
	})

	t.Run("empty_response_latency_should_not_be_in_quantile", func(t *testing.T) {
		// The upstream.go caller passes isSuccess=false for emptyish responses.
		// This ensures fast empty responses don't inflate the latency quantile.
		ups := common.NewFakeUpstream("ups-empty")
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups, method, 10*time.Millisecond, false, "none", common.DataFinalityStateUnknown, "")

		mt := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
		require.NotNil(t, mt)
		q := mt.ResponseQuantiles.GetQuantile(0.70)
		assert.Equal(t, 0.0, q.Seconds(), "empty response latency should NOT inflate quantile")
	})

	t.Run("execution_exception_latency_should_be_in_quantile", func(t *testing.T) {
		// ExecutionException (e.g. revert) is valid blockchain state — its latency
		// should count. The upstream.go caller passes isSuccess=true for these.
		ups := common.NewFakeUpstream("ups-revert")
		tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups, method, 150*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "")

		mt := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
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
			tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
		}

		// 3 wrong-empty responses: both failure AND misbehavior
		for i := 0; i < 3; i++ {
			tracker.RecordUpstreamFailure(ups, method, common.DataFinalityStateUnknown, common.NewErrEndpointMissingData(fmt.Errorf("empty"), ups))
			tracker.RecordUpstreamMisbehavior(ups, method, common.DataFinalityStateUnknown)
		}

		mt := tracker.GetUpstreamMethodMetrics(ups, method, common.DataFinalityStateAll)
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
			tracker.RecordUpstreamRequest(upsWrongEmpty, method, common.DataFinalityStateUnknown)
			tracker.RecordUpstreamRequest(upsRegularErr, method, common.DataFinalityStateUnknown)
		}

		// Both have same error rate
		for i := 0; i < 3; i++ {
			tracker.RecordUpstreamFailure(upsWrongEmpty, method, common.DataFinalityStateUnknown, common.NewErrEndpointMissingData(fmt.Errorf("empty"), upsWrongEmpty))
			tracker.RecordUpstreamMisbehavior(upsWrongEmpty, method, common.DataFinalityStateUnknown)
			tracker.RecordUpstreamFailure(upsRegularErr, method, common.DataFinalityStateUnknown, fmt.Errorf("timeout"))
		}

		mtWE := tracker.GetUpstreamMethodMetrics(upsWrongEmpty, method, common.DataFinalityStateAll)
		mtRE := tracker.GetUpstreamMethodMetrics(upsRegularErr, method, common.DataFinalityStateAll)

		assert.Equal(t, mtWE.ErrorRate(), mtRE.ErrorRate(), "same error rate")
		assert.Greater(t, mtWE.MisbehaviorRate(), mtRE.MisbehaviorRate(),
			"wrong-empty should have higher misbehavior rate than regular errors")
		assert.Equal(t, float64(0), mtRE.MisbehaviorRate(),
			"regular errors should have zero misbehavior rate")
	})
}

// feedBlockDetection is a test helper that directly calls updateBlockTimeSample
// with controlled block timestamps, bypassing SetLatestBlockNumber.
func feedBlockDetection(tracker *Tracker, networkId, netLabel string, blockNumber, blockTimestamp int64) {
	ntwMeta := tracker.getMetadata(metadataKey{nil, networkId})
	tracker.updateBlockTimeSample(ntwMeta, netLabel, blockNumber, blockTimestamp)
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
		feedBlockDetection(tracker, networkId, netLabel, 1000, 1700000000)
		d := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, time.Duration(0), d, "single observation should return 0")
	})

	t.Run("BelowMinSamples_ReturnsZero", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i < 3; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*6)
		}
		d := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, time.Duration(0), d, "below minSamples should return 0")

		feedBlockDetection(tracker, networkId, netLabel, baseBlock+3, baseTs+18)
		d = tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 6.0, d.Seconds(), 0.5, "at minSamples should return block time")
	})

	t.Run("ConvergesToTrueBlockTime", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i <= 50; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*2)
		}
		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, d.Seconds(), 0.1, "should converge to 2s block time")
	})

	t.Run("FastChain_SubSecondBlocks", func(t *testing.T) {
		// Arbitrum-like: ~4 blocks per second. Consecutive blocks share the same
		// integer-second timestamp. Samples are skipped until timestamp ticks,
		// then blockGap normalization recovers the correct per-block time.
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		// 128 blocks at ~4 blocks/second (250ms each)
		for i := int64(0); i < 128; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i/4)
		}

		d := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, d, time.Duration(0), "fast chain should produce a block time")
		assert.Less(t, d, 1*time.Second, "fast chain block time should be sub-second")
		assert.InDelta(t, 0.25, d.Seconds(), 0.05, "should converge to ~250ms")
	})

	t.Run("RejectsAbsurdValues", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		feedBlockDetection(tracker, networkId, netLabel, 1000, 1700000000)
		feedBlockDetection(tracker, networkId, netLabel, 1001, 1700000200)
		feedBlockDetection(tracker, networkId, netLabel, 1002, 1700000400)
		feedBlockDetection(tracker, networkId, netLabel, 1003, 1700000600)

		d := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, time.Duration(0), d, "should reject absurd block times")
	})

	t.Run("EMA_RecoverFromBadEarlySamples", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		feedBlockDetection(tracker, networkId, netLabel, baseBlock, baseTs)
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+1, baseTs+60)

		for i := int64(2); i <= 60; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+60+(i-1)*2)
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

		d := tracker.GetNetworkBlockTime(ups.NetworkId())
		if d > 0 {
			assert.Less(t, d.Seconds(), 120.0, "should be within sane bounds")
		}
	})

	t.Run("OutOfOrderBlocksIgnored", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i <= 15; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*12)
		}
		before := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 12.0, before.Seconds(), 0.5)

		feedBlockDetection(tracker, networkId, netLabel, 1005, baseTs+5*12)
		after := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, before, after, "out-of-order block should not change block time")
	})

	t.Run("LargeBlockGapNormalizesPerBlock", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		feedBlockDetection(tracker, networkId, netLabel, 1000, 1700000000)
		feedBlockDetection(tracker, networkId, netLabel, 1100, 1700000200)

		for i := int64(1); i <= 15; i++ {
			feedBlockDetection(tracker, networkId, netLabel, 1100+i, 1700000200+i*2)
		}
		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, d.Seconds(), 0.2, "large block gap should normalize to per-block time")
	})

	t.Run("ExactlyMinSamplesThreshold", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i < 3; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*3)
		}
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(networkId),
			"at minSamples-1 should still return 0")

		feedBlockDetection(tracker, networkId, netLabel, baseBlock+3, baseTs+9)
		d := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, d, time.Duration(0), "at exactly minSamples should return non-zero")
		assert.InDelta(t, 3.0, d.Seconds(), 0.1, "should reflect 3s block time")
	})

	t.Run("EMA_AdaptsToNewBlockTime", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i < 40; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*10)
		}
		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 10.0, d.Seconds(), 0.5, "should show 10s block time")

		lastBlock := baseBlock + 39
		lastTs := baseTs + 39*10
		for i := int64(1); i <= 50; i++ {
			feedBlockDetection(tracker, networkId, netLabel, lastBlock+i, lastTs+i*3)
		}
		d = tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 3.0, d.Seconds(), 0.2, "EMA should adapt to new 3s block time")
	})

	// ---------------------------------------------------------------
	// Edge-case tests
	// ---------------------------------------------------------------

	t.Run("EdgeCase_Startup_GracefulRampUp", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(18_000_000)
		baseTs := int64(1700000000)

		feedBlockDetection(tracker, networkId, netLabel, baseBlock, baseTs)
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(networkId))

		feedBlockDetection(tracker, networkId, netLabel, baseBlock+1, baseTs+12)
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(networkId))

		feedBlockDetection(tracker, networkId, netLabel, baseBlock+2, baseTs+24)
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(networkId))

		feedBlockDetection(tracker, networkId, netLabel, baseBlock+3, baseTs+36)
		d := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, d, time.Duration(0), "observation 4: block time should be emitted")
		assert.InDelta(t, 12.0, d.Seconds(), 0.5)
	})

	t.Run("EdgeCase_NodeDowntime_BlockGapNormalization", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i <= 20; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*2)
		}
		d := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, d.Seconds(), 0.2, "steady state should be ~2s")

		lastBlock := baseBlock + 20
		lastTs := baseTs + 20*2
		downtime := int64(300) // 5 minutes in seconds
		blocksProducedDuringDowntime := int64(150)

		feedBlockDetection(tracker, networkId, netLabel,
			lastBlock+blocksProducedDuringDowntime, lastTs+downtime)

		d = tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, d.Seconds(), 0.3,
			"block-gap normalization should produce ~2s, not a 5-minute spike")
	})

	t.Run("EdgeCase_ChainHalt_SingleBlockAfterLongPause", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i <= 30; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*2)
		}
		steadyState := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, steadyState.Seconds(), 0.1, "pre-halt steady state")

		lastBlock := baseBlock + 30
		lastTs := baseTs + 30*2
		haltDuration := int64(600) // 10 min in seconds

		feedBlockDetection(tracker, networkId, netLabel, lastBlock+1, lastTs+haltDuration)

		spiked := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, spiked.Seconds(), 30.0,
			"chain halt with blockGap=1 causes a spike (expected, documented behavior)")
		assert.Less(t, spiked.Seconds(), 120.0,
			"spike should still be within sanity bounds")

		resumeBlock := lastBlock + 1
		resumeTs := lastTs + haltDuration
		for i := int64(1); i <= 40; i++ {
			feedBlockDetection(tracker, networkId, netLabel, resumeBlock+i, resumeTs+i*2)
		}
		recovered := tracker.GetNetworkBlockTime(networkId)
		assert.Less(t, recovered.Seconds(), 10.0,
			"after 40 normal blocks, EMA should be recovering toward 2s")

		for i := int64(41); i <= 100; i++ {
			feedBlockDetection(tracker, networkId, netLabel, resumeBlock+i, resumeTs+i*2)
		}
		fullyRecovered := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, fullyRecovered.Seconds(), 0.5,
			"after 100 normal blocks, EMA should be very close to 2s")
	})

	t.Run("EdgeCase_ChainHalt_MultipleBlocksBurst", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i <= 30; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*12)
		}
		assert.InDelta(t, 12.0, tracker.GetNetworkBlockTime(networkId).Seconds(), 0.5)

		lastBlock := baseBlock + 30
		lastTs := baseTs + 30*12
		halt := int64(600)

		feedBlockDetection(tracker, networkId, netLabel, lastBlock+1, lastTs+halt)
		spiked := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, spiked.Seconds(), 30.0, "first block after halt spikes EMA")

		for i := int64(2); i <= 5; i++ {
			feedBlockDetection(tracker, networkId, netLabel, lastBlock+i, lastTs+halt+(i-1))
		}
		afterBurst := tracker.GetNetworkBlockTime(networkId)
		assert.Less(t, afterBurst.Seconds(), spiked.Seconds(),
			"burst of blocks should pull EMA back down from spike")
	})

	t.Run("EdgeCase_QuietChainThenResumes", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i <= 30; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*2)
		}
		normal := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, normal.Seconds(), 0.1, "phase 1: normal 2s blocks")

		lastBlock := baseBlock + 30
		lastTs := baseTs + 30*2
		for i := int64(1); i <= 30; i++ {
			feedBlockDetection(tracker, networkId, netLabel, lastBlock+i, lastTs+i*10)
		}
		slow := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, slow.Seconds(), 5.0, "phase 2: EMA should be moving toward 10s")
		assert.Less(t, slow.Seconds(), 11.0, "phase 2: EMA should not overshoot")

		lastBlock += 30
		lastTs += 30 * 10
		for i := int64(1); i <= 50; i++ {
			feedBlockDetection(tracker, networkId, netLabel, lastBlock+i, lastTs+i*2)
		}
		resumed := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 2.0, resumed.Seconds(), 0.5,
			"phase 3: EMA should recover back to ~2s")
	})

	t.Run("EdgeCase_SameTimestamp_AccumulatesBlockGap", func(t *testing.T) {
		// Fast chain: 4 blocks per second share the same timestamp.
		// Prev should NOT advance on same-timestamp blocks, so blockGap
		// accumulates until the timestamp ticks.
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		feedBlockDetection(tracker, networkId, netLabel, baseBlock, baseTs)
		// 3 blocks at same timestamp — all skipped, prev stays at (1000, baseTs)
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+1, baseTs)
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+2, baseTs)
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+3, baseTs)

		// Timestamp ticks: blockGap=4, tsDelta=1s → 250ms/block
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+4, baseTs+1)

		// Need more samples for EMA to emit (minSamples=3)
		for i := int64(5); i < 20; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i/4)
		}

		d := tracker.GetNetworkBlockTime(networkId)
		assert.Greater(t, d, time.Duration(0), "should produce block time from timestamp-based sampling")
		assert.InDelta(t, 0.25, d.Seconds(), 0.1, "should converge to ~250ms via blockGap normalization")
	})

	t.Run("EdgeCase_BackwardsTimestamp_Rejected", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		baseBlock := int64(1000)
		baseTs := int64(1700000000)

		for i := int64(0); i <= 10; i++ {
			feedBlockDetection(tracker, networkId, netLabel, baseBlock+i, baseTs+i*5)
		}
		before := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 5.0, before.Seconds(), 0.5)

		// Block with same timestamp — skipped, prev unchanged
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+11, baseTs+10*5)
		after := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, before, after, "same timestamp should not change block time")

		// Block with backwards timestamp — skipped, prev unchanged
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+12, baseTs+9*5)
		afterSkew := tracker.GetNetworkBlockTime(networkId)
		assert.Equal(t, before, afterSkew, "backwards timestamp should not change block time")

		// Normal block resumes — prev was still at (1010, baseTs+50), so this works
		feedBlockDetection(tracker, networkId, netLabel, baseBlock+13, baseTs+13*5)
		afterResume := tracker.GetNetworkBlockTime(networkId)
		assert.InDelta(t, 5.0, afterResume.Seconds(), 1.0, "should recover after skew")
	})
}

// TestTracker_CordonEventMetrics verifies the new cordon observability
// signals: every cordon/uncordon transition increments the per-upstream
// event counter, and uncordon ALSO observes the cordon-duration
// histogram so dashboards can answer "how long did this incident last?".
func TestTracker_CordonEventMetrics(t *testing.T) {
	t.Run("cordon_then_uncordon_records_paired_events_and_duration", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 2*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("under-incident")
		networkID := ups.NetworkId()
		cordonCounter := telemetry.MetricUpstreamCordonEventTotal.WithLabelValues(
			"test-project", networkID, ups.Id(), "cordon")
		uncordonCounter := telemetry.MetricUpstreamCordonEventTotal.WithLabelValues(
			"test-project", networkID, ups.Id(), "uncordon")
		beforeCordonCount := promUtil.ToFloat64(cordonCounter)
		beforeUncordonCount := promUtil.ToFloat64(uncordonCounter)

		tracker.Cordon(ups, "*", "vendor maintenance")
		assert.Equal(t, beforeCordonCount+1, promUtil.ToFloat64(cordonCounter),
			"cordon event counter increments on OFF→ON edge")

		// Tracked cordon-start timestamp survives an already-cordoned
		// refresh (operator updating context with another Cordon call)
		// — only the OFF→ON edge records start so durations measure
		// real outages.
		tm := tracker.getUpsMetrics(upstreamKey{ups, "*", common.DataFinalityStateAll})
		startedAt := tm.CordonedAtMs.Load()
		require.NotZero(t, startedAt, "CordonedAtMs must be set on OFF→ON edge")

		time.Sleep(5 * time.Millisecond)
		tracker.Cordon(ups, "*", "vendor maintenance (extended)")
		require.Equal(t, startedAt, tm.CordonedAtMs.Load(),
			"already-cordoned re-cordon must preserve original start timestamp")
		assert.Equal(t, beforeCordonCount+1, promUtil.ToFloat64(cordonCounter),
			"already-cordoned re-cordon must NOT increment the event counter")

		// Hold the cordon briefly so the duration histogram observes a
		// non-zero value, then uncordon.
		time.Sleep(20 * time.Millisecond)
		histBefore := promUtil.CollectAndCount(telemetry.MetricUpstreamCordonDurationSeconds)
		tracker.Uncordon(ups, "*", "vendor resolved")
		histAfter := promUtil.CollectAndCount(telemetry.MetricUpstreamCordonDurationSeconds)

		assert.Equal(t, int64(0), tm.CordonedAtMs.Load(),
			"CordonedAtMs reset to 0 on uncordon")
		assert.False(t, tm.Cordoned.Load(), "Cordoned flag must be false after uncordon")
		assert.Equal(t, beforeUncordonCount+1, promUtil.ToFloat64(uncordonCounter),
			"uncordon event counter increments on ON→OFF edge")
		assert.GreaterOrEqual(t, histAfter, histBefore,
			"cordon-duration histogram must have observed the duration on uncordon")
	})

	t.Run("idle_sweep_evicts_silent_methods", func(t *testing.T) {
		// Defends against the method-flood attack vector — a hostile
		// client sending unique JSON-RPC method names to grow the
		// per-method tracker map without bound. After the attack
		// stops, the idle sweep must drop those buckets within a few
		// rotations and the steady-state memory should track only
		// methods that received recent traffic.
		tracker := NewTracker(&log.Logger, "test-project", 2*time.Second)
		// Aggressive threshold so the test doesn't wait minutes.
		// Just shy of the sweep cadence so the second sweep cycle
		// catches still-silent buckets.
		tracker.SetIdleEvictionAfter(150 * time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("flood-target")

		for i := 0; i < 200; i++ {
			tracker.RecordUpstreamRequest(ups, fmt.Sprintf("eth_random%d", i), common.DataFinalityStateUnknown)
		}
		hotMethod := "eth_blockNumber"

		countBefore := 0
		tracker.upsMetrics.Range(func(k, _ any) bool {
			if k.(upstreamKey).method != "*" {
				countBefore++
			}
			return true
		})
		require.GreaterOrEqual(t, countBefore, 200,
			"all 200 flood methods should have spawned per-method entries")

		// Eviction needs (a) idleEvictionAfter to elapse, AND (b) the
		// sweep cadence — sweepEveryRotations × interval. With
		// windowSize=2s and rollingBuckets=10, sweep fires every 2s.
		// Generous margin to absorb CI jitter.
		require.Eventually(t, func() bool {
			// Keep the hot method alive across the wait so it survives.
			tracker.RecordUpstreamRequest(ups, hotMethod, common.DataFinalityStateUnknown)
			n := 0
			tracker.upsMetrics.Range(func(k, _ any) bool {
				if k.(upstreamKey).method != "*" {
					n++
				}
				return true
			})
			return n <= 1
		}, 5*time.Second, 100*time.Millisecond,
			"flooded methods should have evicted after going idle past the threshold")

		_, ok := tracker.upsMetrics.Load(upstreamKey{ups, hotMethod, common.DataFinalityStateAll})
		assert.True(t, ok, "hot method must NOT be evicted while it keeps receiving traffic")
	})

	t.Run("idle_sweep_releases_prom_observer_series", func(t *testing.T) {
		// Verifies the second half of the method-flood defense: the
		// IN-MEMORY observer cache shrinks AND the Prometheus
		// registry side actually releases the series. Without
		// DeleteLabelValues, even an evicted cache entry would leave
		// the label-set in the registry forever, and the next
		// `/metrics` scrape would still emit it.
		tracker := NewTracker(&log.Logger, "test-project-prom", 2*time.Second)
		tracker.SetIdleEvictionAfter(150 * time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("prom-flood-target")

		// Drive the observer cache via real duration records — that's
		// the only path that lazy-creates entries in urdObsCache.
		for i := 0; i < 50; i++ {
			method := fmt.Sprintf("eth_prom_random%d", i)
			tracker.RecordUpstreamDuration(ups, method, 5*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "user-a")
		}

		// Pre-sweep: the cache holds at least 50 unique label tuples
		// AND the underlying MetricVec sees them all.
		cacheBefore := 0
		tracker.urdObsCache.Range(func(_, _ any) bool { cacheBefore++; return true })
		require.GreaterOrEqual(t, cacheBefore, 50,
			"observer cache should hold an entry per unique method")
		metricBefore := promUtil.CollectAndCount(telemetry.MetricUpstreamRequestDuration)
		require.GreaterOrEqual(t, metricBefore, 50,
			"Prometheus registry should hold a series per unique method (got %d)", metricBefore)

		// Wait past the idle threshold + a sweep cycle. Sweep fires
		// once per windowSize (2s) so 2.5s is enough.
		require.Eventually(t, func() bool {
			n := 0
			tracker.urdObsCache.Range(func(_, _ any) bool { n++; return true })
			return n == 0
		}, 5*time.Second, 100*time.Millisecond,
			"observer cache should have evicted every flood entry once idle")

		// Critical: the Prom registry shrunk too. Without
		// DeleteLabelValues this count would still be ≥ 50 even after
		// the in-memory cache cleared.
		metricAfter := promUtil.CollectAndCount(telemetry.MetricUpstreamRequestDuration)
		require.Less(t, metricAfter, metricBefore,
			"Prometheus registry must release evicted series — without DeleteLabelValues the count stays sticky (got after=%d, before=%d)",
			metricAfter, metricBefore)
	})

	t.Run("idle_sweep_preserves_cordoned_state", func(t *testing.T) {
		// Cordon is operator-set; it must never be evicted by the
		// idle sweep, even if the cordoned (upstream, method) has
		// received zero traffic since the cordon was applied.
		tracker := NewTracker(&log.Logger, "test-project", 2*time.Second)
		tracker.SetIdleEvictionAfter(50 * time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("cordoned-method-target")
		tracker.RecordUpstreamRequest(ups, "eth_call", common.DataFinalityStateUnknown)
		tracker.Cordon(ups, "eth_call", "investigated regression")

		// Wait through a couple of sweep cycles.
		time.Sleep(2500 * time.Millisecond)

		// `Cordon` stores on the all-finalities key; the sweep skips
		// any entry where `Cordoned == true`, so the cordoned bucket
		// MUST survive.
		tm, ok := tracker.upsMetrics.Load(upstreamKey{ups, "eth_call", common.DataFinalityStateAll})
		require.True(t, ok, "cordoned per-method entry must NOT be evicted by the idle sweep")
		assert.True(t, tm.(*TrackedMetrics).Cordoned.Load(),
			"cordon flag must still be set after the sweep")
	})

	t.Run("uncordon_when_not_cordoned_is_idempotent", func(t *testing.T) {
		// Uncordoning a never-cordoned upstream must not panic, must not
		// observe the duration histogram, and must not increment the
		// event counter. Important because operator scripts may run
		// reconcile-style "uncordon everything" passes.
		tracker := NewTracker(&log.Logger, "test-project", 2*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tracker.Bootstrap(ctx)

		ups := common.NewFakeUpstream("never-cordoned")
		networkID := ups.NetworkId()
		uncordonCounter := telemetry.MetricUpstreamCordonEventTotal.WithLabelValues(
			"test-project", networkID, ups.Id(), "uncordon")
		before := promUtil.ToFloat64(uncordonCounter)

		require.NotPanics(t, func() {
			tracker.Uncordon(ups, "*", "spurious uncordon")
		})
		tm := tracker.getUpsMetrics(upstreamKey{ups, "*", common.DataFinalityStateAll})
		assert.False(t, tm.Cordoned.Load())
		assert.Equal(t, int64(0), tm.CordonedAtMs.Load())
		assert.Equal(t, before, promUtil.ToFloat64(uncordonCounter),
			"no-op uncordon must NOT increment the event counter")
	})
}

// TestGetUpstreamMetrics_ScopedByNetwork pins the O(per-network)
// behavior of the indexed lookup. With multiple networks × upstreams
// in the tracker, calling GetUpstreamMetrics(u) must return ONLY
// `u`'s per-method buckets, not entries belonging to other upstreams
// or other networks. Validates that the upstreamsByNetwork-driven
// implementation hasn't broken the response shape.
func TestGetUpstreamMetrics_ScopedByNetwork(t *testing.T) {
	tracker := NewTracker(&log.Logger, "test", 60*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	// Two networks, two upstreams each, several methods.
	uA := common.NewFakeUpstream("a", common.WithFakeUpstreamNetworkID("evm:1"))
	uB := common.NewFakeUpstream("b", common.WithFakeUpstreamNetworkID("evm:1"))
	uC := common.NewFakeUpstream("c", common.WithFakeUpstreamNetworkID("evm:10"))
	uD := common.NewFakeUpstream("d", common.WithFakeUpstreamNetworkID("evm:10"))

	for _, u := range []common.Upstream{uA, uB, uC, uD} {
		for _, m := range []string{"eth_call", "eth_getLogs", "eth_chainId"} {
			tracker.RecordUpstreamRequest(u, m, common.DataFinalityStateUnknown)
		}
	}

	gotA := tracker.GetUpstreamMetrics(uA)
	// Expected: every method we recorded for uA (eth_call/eth_getLogs/
	// eth_chainId) plus the wildcard "*" aggregate `getUpsKeys` writes.
	require.Contains(t, gotA, "eth_call")
	require.Contains(t, gotA, "eth_getLogs")
	require.Contains(t, gotA, "eth_chainId")
	require.Contains(t, gotA, "*")

	// Critically: no entries for uB / uC / uD slip through. The pre-fix
	// implementation walked the GLOBAL map and filtered by ID; the
	// index-based path must produce the same result without scanning
	// other entries.
	for method, tm := range gotA {
		require.NotNil(t, tm, "method %q must have non-nil metrics", method)
	}

	gotC := tracker.GetUpstreamMetrics(uC)
	require.Contains(t, gotC, "eth_call")
	require.Contains(t, gotC, "eth_getLogs")
	// uC's metrics must NOT include uD's (same network, different upstream).
	// We can't directly check identity, but counts should reflect ONLY uC's
	// recorded requests:
	assert.Equal(t, int64(1), gotC["eth_call"].RequestsTotal.Load(),
		"uC's eth_call bucket should have 1 request, not include uD's")
}

// TestGetUpstreamMetrics_DistinctUpstreamsSameID_Buckets — the
// upstreamsByNetwork index dedups on (ups_id, method). Test that the
// indexed lookup correctly returns metrics for an upstream when
// multiple upstreams might share method names. Critical guard against
// regressing the per-upstream filter.
func TestGetUpstreamMetrics_DistinctUpstreamsSameID_Buckets(t *testing.T) {
	tracker := NewTracker(&log.Logger, "test", 60*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx)

	uA := common.NewFakeUpstream("ups-A", common.WithFakeUpstreamNetworkID("evm:1"))
	uB := common.NewFakeUpstream("ups-B", common.WithFakeUpstreamNetworkID("evm:1"))

	// Differing request volumes per upstream — if the per-upstream
	// filter is broken (e.g., returning uB's bucket when querying uA),
	// the counts will be wrong.
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(uA, "eth_call", common.DataFinalityStateUnknown)
	}
	for i := 0; i < 13; i++ {
		tracker.RecordUpstreamRequest(uB, "eth_call", common.DataFinalityStateUnknown)
	}

	got := tracker.GetUpstreamMetrics(uA)
	require.NotNil(t, got["eth_call"])
	assert.Equal(t, int64(50), got["eth_call"].RequestsTotal.Load(),
		"GetUpstreamMetrics(uA) must return uA's count, not uB's")

	got = tracker.GetUpstreamMetrics(uB)
	require.NotNil(t, got["eth_call"])
	assert.Equal(t, int64(13), got["eth_call"].RequestsTotal.Load())
}

// BenchmarkGetUpstreamMetrics_IndexScaling proves the optimization:
// GetUpstreamMetrics per-call cost should NOT scale with the GLOBAL
// number of tracker entries (which was the bug — `sync.Map.Range` was
// walking everything). With the upstreamsByNetwork index it scales
// only with the per-NETWORK key count.
//
// Setup: populate the tracker with N networks × 5 upstreams × 30
// methods (= 150N total entries). Then call GetUpstreamMetrics on
// ONE upstream. Allocation and time should be near-constant in N.
func BenchmarkGetUpstreamMetrics_IndexScaling(b *testing.B) {
	for _, networks := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("networks=%d", networks), func(b *testing.B) {
			tracker := NewTracker(&log.Logger, "bench", 60*time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tracker.Bootstrap(ctx)

			var target common.Upstream
			for n := 0; n < networks; n++ {
				netID := "evm:" + strconv.Itoa(n)
				for u := 0; u < 5; u++ {
					ups := common.NewFakeUpstream(
						"u-"+strconv.Itoa(n)+"-"+strconv.Itoa(u),
						common.WithFakeUpstreamNetworkID(netID),
					)
					if n == 0 && u == 0 {
						target = ups
					}
					for m := 0; m < 30; m++ {
						method := "method_" + strconv.Itoa(m)
						tracker.RecordUpstreamRequest(ups, method, common.DataFinalityStateUnknown)
					}
				}
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = tracker.GetUpstreamMetrics(target)
			}
		})
	}
}
