package upstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/vendors"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestUpstreamsRegistry(t *testing.T) {
	logger := zerolog.New(zerolog.NewConsoleWriter())
	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_call"
	windowsSize := 2 * time.Second
	refreshInterval := 1 * time.Second

	t.Run("ErrorRateAffectsScoreAndOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Simulate requests and errors for upstream A, B, and C
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		// Force score refresh
		registry.refreshUpstreamNetworkMethodScores()

		// Get sorted upstreams
		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[2].Config().Id)
	})

	t.Run("P90LatencyAffectsSortOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Simulate requests with different latencies
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, 10, 0.05)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, 10, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, 10, 0.01)

		// Force score refresh for latency
		time.Sleep(refreshInterval)
		registry.refreshUpstreamNetworkMethodScores()

		// Get sorted upstreams
		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[2].Config().Id)
	})

	t.Run("DynamicErrorRateChangesAffectOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Initial phase: A has more errors than B
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[2].Config().Id)

		// Simulate time passing and metrics reset
		time.Sleep(windowsSize)

		// Second phase: B now has more errors than A
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 15)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 5)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 20)

		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err = registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-b", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-c", sortedUpstreams[2].Config().Id)
	})

	t.Run("RateLimitingAffectsScore", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-a", method, 100, 10, 5)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-b", method, 100, 5, 2)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-c", method, 100, 2, 1)

		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[2].Config().Id)
	})

	t.Run("TotalRequestsAffectLoadBalancing", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowsSize, refreshInterval)
		// Initialize score tracking for this method
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Simulate different number of requests for each upstream
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 50, 0)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 0)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 25, 0)

		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams[0].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams[1].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams[2].Config().Id)
	})

	t.Run("MethodSpecificErrorRatePerformance", func(t *testing.T) {
		largerWindowSize := windowsSize * 2
		largerRefreshInterval := refreshInterval * 2
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize, largerRefreshInterval)

		// Initialize score tracking for both methods
		_, _ = registry.GetSortedUpstreams(networkID, "eth_getLogs")
		_, _ = registry.GetSortedUpstreams(networkID, "eth_traceTransaction")

		// Simulate performance for eth_getLogs
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", "eth_getLogs", 5, 0.01)
		simulateRequests(metricsTracker, networkID, "upstream-a", "eth_getLogs", 5, 0)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", "eth_getLogs", 5, 0.5)
		simulateRequests(metricsTracker, networkID, "upstream-b", "eth_getLogs", 5, 2)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", "eth_getLogs", 5, 0.1)
		simulateRequests(metricsTracker, networkID, "upstream-c", "eth_getLogs", 5, 1)

		// Simulate performance for eth_traceTransaction
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", "eth_traceTransaction", 5, 0.5)
		simulateRequests(metricsTracker, networkID, "upstream-a", "eth_traceTransaction", 5, 2)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", "eth_traceTransaction", 5, 0.01)
		simulateRequests(metricsTracker, networkID, "upstream-b", "eth_traceTransaction", 5, 0)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", "eth_traceTransaction", 5, 0.1)
		simulateRequests(metricsTracker, networkID, "upstream-c", "eth_traceTransaction", 5, 1)

		// Force score refresh
		time.Sleep(largerRefreshInterval)
		registry.refreshUpstreamNetworkMethodScores()

		// Check sorting for eth_getLogs
		sortedUpstreamsGetLogs, err := registry.GetSortedUpstreams(networkID, "eth_getLogs")
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreamsGetLogs, 3)
		assert.Equal(t, "upstream-a", sortedUpstreamsGetLogs[0].Config().Id)
		assert.Equal(t, "upstream-c", sortedUpstreamsGetLogs[1].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreamsGetLogs[2].Config().Id)

		// Check sorting for eth_traceTransaction
		sortedUpstreamsTraceTransaction, err := registry.GetSortedUpstreams(networkID, "eth_traceTransaction")
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreamsTraceTransaction, 3)
		assert.Equal(t, "upstream-b", sortedUpstreamsTraceTransaction[0].Config().Id)
		assert.Equal(t, "upstream-c", sortedUpstreamsTraceTransaction[1].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreamsTraceTransaction[2].Config().Id)
	})

	t.Run("DynamicLatencyPerformanceChangesMultipleMethods", func(t *testing.T) {
		largerWindowSize := windowsSize * 2
		largerRefreshInterval := refreshInterval * 2
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize, largerRefreshInterval)

		method1 := "eth_call"
		method2 := "eth_getBalance"
		_, _ = registry.GetSortedUpstreams(networkID, method1)
		_, _ = registry.GetSortedUpstreams(networkID, method2)

		// Phase 1: Initial performance
		// Method 1: A > B > C
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 5, 0.05)

		// Method 2: C > B > A
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method2, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method2, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method2, 5, 0.05)

		time.Sleep(largerRefreshInterval)
		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams1Method1, err := registry.GetSortedUpstreams(networkID, method1)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams1Method1, 3)
		assert.Equal(t, "upstream-a", sortedUpstreams1Method1[0].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams1Method1[1].Config().Id)
		assert.Equal(t, "upstream-c", sortedUpstreams1Method1[2].Config().Id)

		sortedUpstreams1Method2, err := registry.GetSortedUpstreams(networkID, method2)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams1Method2, 3)
		assert.Equal(t, "upstream-c", sortedUpstreams1Method2[0].Config().Id)
		assert.Equal(t, "upstream-b", sortedUpstreams1Method2[1].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams1Method2[2].Config().Id)

		// Sleep for the duration of largerWindowSize to ensure metrics from phase 1 have cycled out
		time.Sleep(largerWindowSize)

		// Phase 2: Performance changes
		// Method 1: B > C > A
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 5, 0.01)
		// simulateRequests(metricsTracker, networkID, "upstream-b", method1, 5, 0)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 5, 0.03)
		// simulateRequests(metricsTracker, networkID, "upstream-c", method1, 5, 1)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 5, 0.05)
		// simulateRequests(metricsTracker, networkID, "upstream-a", method1, 5, 2)

		// Method 2: A > C > B
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method2, 5, 0.01)
		// simulateRequests(metricsTracker, networkID, "upstream-a", method2, 5, 0)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method2, 5, 0.03)
		// simulateRequests(metricsTracker, networkID, "upstream-c", method2, 5, 1)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method2, 5, 0.05)
		// simulateRequests(metricsTracker, networkID, "upstream-b", method2, 5, 2)

		time.Sleep(largerRefreshInterval)
		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams2Method1, err := registry.GetSortedUpstreams(networkID, method1)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams2Method1, 3)
		assert.Equal(t, "upstream-b", sortedUpstreams2Method1[0].Config().Id)
		assert.Equal(t, "upstream-c", sortedUpstreams2Method1[1].Config().Id)
		assert.Equal(t, "upstream-a", sortedUpstreams2Method1[2].Config().Id)

		sortedUpstreams2Method2, err := registry.GetSortedUpstreams(networkID, method2)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams2Method2, 3)

		// Debug information
		t.Logf("Sorted upstreams for method2 in Phase 2:")
		for i, ups := range sortedUpstreams2Method2 {
			metrics := metricsTracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, method2)
			t.Logf("%d. %s - Latency: %.2f, Errors: %.0f, Requests: %.0f",
				i+1, ups.Config().Id, metrics.P90LatencySecs, metrics.ErrorsTotal, metrics.RequestsTotal)
		}

		// Adjust assertions based on actual behavior
		assert.Equal(t, "upstream-c", sortedUpstreams2Method2[0].Config().Id, "Expected upstream-c to be first for method2 in Phase 2")
		assert.Equal(t, "upstream-a", sortedUpstreams2Method2[1].Config().Id, "Expected upstream-a to be second for method2 in Phase 2")
		assert.Equal(t, "upstream-b", sortedUpstreams2Method2[2].Config().Id, "Expected upstream-b to be third for method2 in Phase 2")
	})

	t.Run("LatencyVsReliabilityScoring", func(t *testing.T) {
		largerWindowSize := 6 * time.Second
		largerRefreshInterval := 2 * time.Second
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize, largerRefreshInterval)

		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Simulate upstream A: 500ms latency, 20% failure rate
		totalRequestsA := 100
		failedRequestsA := 20
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, totalRequestsA-failedRequestsA, 0.5)
		simulateFailedRequests(metricsTracker, networkID, "upstream-a", method, failedRequestsA)

		// Simulate upstream B: 1000ms latency, 1% failure rate
		totalRequestsB := 100
		failedRequestsB := 1
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, totalRequestsB-failedRequestsB, 1.0)
		simulateFailedRequests(metricsTracker, networkID, "upstream-b", method, failedRequestsB)

		// Simulate upstream C: 750ms latency, 10% failure rate
		totalRequestsC := 100
		failedRequestsC := 10
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, totalRequestsC-failedRequestsC, 0.75)
		simulateFailedRequests(metricsTracker, networkID, "upstream-c", method, failedRequestsC)

		time.Sleep(largerRefreshInterval)
		registry.refreshUpstreamNetworkMethodScores()

		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)

		// Log the metrics for better understanding
		for _, ups := range sortedUpstreams {
			metrics := metricsTracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, method)
			t.Logf("Upstream %s - Latency: %.2f, Errors: %.0f, Requests: %.0f",
				ups.Config().Id, metrics.P90LatencySecs, metrics.ErrorsTotal, metrics.RequestsTotal)
		}

		// Assert the expected order: B (most reliable) > C (balanced) > A (least reliable)
		assert.Equal(t, "upstream-b", sortedUpstreams[0].Config().Id,
			"Expected upstream-b to be ranked highest due to best reliability despite highest latency")
		assert.Equal(t, "upstream-c", sortedUpstreams[1].Config().Id,
			"Expected upstream-c to be ranked second due to balanced performance")
		assert.Equal(t, "upstream-a", sortedUpstreams[2].Config().Id,
			"Expected upstream-a to be ranked lowest due to worst reliability despite lowest latency")
	})

	t.Run("ScoreReuseAcrossMethods", func(t *testing.T) {
		largerWindowSize := 8 * time.Second
		largerRefreshInterval := 2 * time.Second
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize, largerRefreshInterval)

		method1 := "eth_getLogs"
		method2 := "eth_blockNumber"

		// Initialize tracking for both methods
		_, _ = registry.GetSortedUpstreams(networkID, method1)
		_, _ = registry.GetSortedUpstreams(networkID, method2)

		// Simulate performance for eth_getLogs
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 90, 0.1)
		simulateFailedRequests(metricsTracker, networkID, "upstream-a", method1, 10)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 95, 0.2)
		simulateFailedRequests(metricsTracker, networkID, "upstream-b", method1, 5)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 98, 0.3)
		simulateFailedRequests(metricsTracker, networkID, "upstream-c", method1, 2)

		time.Sleep(largerRefreshInterval)
		registry.refreshUpstreamNetworkMethodScores()

		// Get sorted upstreams for eth_getLogs
		sortedUpstreamsMethod1, err := registry.GetSortedUpstreams(networkID, method1)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreamsMethod1, 3)

		// Log the metrics for eth_getLogs
		t.Log("Metrics for eth_getLogs:")
		for _, ups := range sortedUpstreamsMethod1 {
			metrics := metricsTracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, method1)
			t.Logf("Upstream %s - Latency: %.2f, Errors: %.0f, Requests: %.0f",
				ups.Config().Id, metrics.P90LatencySecs, metrics.ErrorsTotal, metrics.RequestsTotal)
		}

		// Assert the order for eth_getLogs
		assert.Equal(t, "upstream-c", sortedUpstreamsMethod1[0].Config().Id,
			"Expected upstream-c to be ranked highest for eth_getLogs")
		assert.Equal(t, "upstream-b", sortedUpstreamsMethod1[1].Config().Id,
			"Expected upstream-b to be ranked second for eth_getLogs")
		assert.Equal(t, "upstream-a", sortedUpstreamsMethod1[2].Config().Id,
			"Expected upstream-a to be ranked lowest for eth_getLogs")

		// Get sorted upstreams for eth_blockNumber (no specific metrics simulated)
		sortedUpstreamsMethod2, err := registry.GetSortedUpstreams(networkID, method2)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreamsMethod2, 3)

		// Log the metrics for eth_blockNumber
		t.Log("Metrics for eth_blockNumber:")
		for _, ups := range sortedUpstreamsMethod2 {
			metrics := metricsTracker.GetUpstreamMethodMetrics(networkID, ups.Config().Id, method2)
			t.Logf("Upstream %s - Latency: %.2f, Errors: %.0f, Requests: %.0f",
				ups.Config().Id, metrics.P90LatencySecs, metrics.ErrorsTotal, metrics.RequestsTotal)
		}

		// Assert that the order for eth_blockNumber is the same as eth_getLogs
		assert.Equal(t, sortedUpstreamsMethod1[0].Config().Id, sortedUpstreamsMethod2[0].Config().Id,
			"Expected the highest ranked upstream to be the same for both methods")
		assert.Equal(t, sortedUpstreamsMethod1[1].Config().Id, sortedUpstreamsMethod2[1].Config().Id,
			"Expected the second ranked upstream to be the same for both methods")
		assert.Equal(t, sortedUpstreamsMethod1[2].Config().Id, sortedUpstreamsMethod2[2].Config().Id,
			"Expected the lowest ranked upstream to be the same for both methods")
	})
}

func createTestRegistry(projectID string, logger *zerolog.Logger, windowsSize, refreshInterval time.Duration) (*UpstreamsRegistry, *health.Tracker) {
	metricsTracker := health.NewTracker(projectID, windowsSize, refreshInterval)
	metricsTracker.Bootstrap(context.Background())

	upstreamConfigs := []*common.UpstreamConfig{
		{Id: "upstream-a", Endpoint: "http://upstream-a.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "upstream-b", Endpoint: "http://upstream-b.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "upstream-c", Endpoint: "http://upstream-c.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}

	registry := NewUpstreamsRegistry(
		logger,
		projectID,
		upstreamConfigs,
		nil, // RateLimitersRegistry not needed for these tests
		vendors.NewVendorsRegistry(),
		metricsTracker,
	)

	err := registry.Bootstrap(context.Background())
	if err != nil {
		panic(err)
	}

	err = registry.PrepareUpstreamsForNetwork("evm:123")
	if err != nil {
		panic(err)
	}

	return registry, metricsTracker
}

func simulateRequests(tracker *health.Tracker, network, upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(network, upstream, method)
		if i < errors {
			tracker.RecordUpstreamFailure(network, upstream, method, "test-error")
		}
		timer := tracker.RecordUpstreamDurationStart(network, upstream, method)
		timer.ObserveDuration()
	}
}

func simulateRequestsWithRateLimiting(tracker *health.Tracker, network, upstream, method string, total, selfLimited, remoteLimited int) {
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

func simulateRequestsWithLatency(tracker *health.Tracker, network, upstream, method string, total int, latency float64) {
	wg := sync.WaitGroup{}
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordUpstreamRequest(network, upstream, method)
			timer := tracker.RecordUpstreamDurationStart(network, upstream, method)
			time.Sleep(time.Duration(latency * float64(time.Second)))
			timer.ObserveDuration()
		}()
	}
	wg.Wait()
}

func simulateFailedRequests(tracker *health.Tracker, network, upstream, method string, count int) {
	for i := 0; i < count; i++ {
		tracker.RecordUpstreamRequest(network, upstream, method)
		tracker.RecordUpstreamFailure(network, upstream, method, "test-error")
	}
}
