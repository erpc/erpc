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
	windowSize := 2 * time.Second
	refreshInterval := 1 * time.Second
	iterations := 1000

	t.Run("ErrorRateAffectsScoreAndOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize, refreshInterval)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamDistribution(t, registry, networkID, method, iterations, expectedOrder)
	})

	t.Run("P90LatencyAffectsSortOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize, refreshInterval)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, 10, 0.05)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, 10, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, 10, 0.01)

		time.Sleep(refreshInterval)

		expectedOrder := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamDistribution(t, registry, networkID, method, iterations, expectedOrder)
	})

	t.Run("DynamicErrorRateChangesAffectOrder", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize, refreshInterval)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Initial phase
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamDistribution(t, registry, networkID, method, iterations, expectedOrder)

		// Simulate time passing and metrics reset
		time.Sleep(windowSize)

		// Second phase
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 15)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 5)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 20)

		expectedOrder = []string{"upstream-b", "upstream-a", "upstream-c"}
		checkUpstreamDistribution(t, registry, networkID, method, iterations, expectedOrder)
	})

	t.Run("RateLimitingAffectsScore", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize, refreshInterval)
		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-a", method, 100, 10, 5)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-b", method, 100, 5, 2)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-c", method, 100, 2, 1)

		expectedOrder := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamDistribution(t, registry, networkID, method, iterations, expectedOrder)
	})

	t.Run("TotalRequestsAffectLoadBalancing", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize, refreshInterval)
		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 50, 0)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 0)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 25, 0)

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamDistribution(t, registry, networkID, method, iterations, expectedOrder)
	})

	t.Run("MethodSpecificErrorRatePerformance", func(t *testing.T) {
		largerWindowSize := windowSize * 2
		largerRefreshInterval := refreshInterval * 2
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize, largerRefreshInterval)

		methodGetLogs := "eth_getLogs"
		methodTraceTransaction := "eth_traceTransaction"
		_, _ = registry.GetSortedUpstreams(networkID, methodGetLogs)
		_, _ = registry.GetSortedUpstreams(networkID, methodTraceTransaction)

		// Simulate performance for eth_getLogs
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", methodGetLogs, 5, 0.01)
		simulateRequests(metricsTracker, networkID, "upstream-a", methodGetLogs, 5, 0)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", methodGetLogs, 5, 0.5)
		simulateRequests(metricsTracker, networkID, "upstream-b", methodGetLogs, 5, 2)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", methodGetLogs, 5, 0.1)
		simulateRequests(metricsTracker, networkID, "upstream-c", methodGetLogs, 5, 1)

		// Simulate performance for eth_traceTransaction
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", methodTraceTransaction, 5, 0.5)
		simulateRequests(metricsTracker, networkID, "upstream-a", methodTraceTransaction, 5, 2)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", methodTraceTransaction, 5, 0.01)
		simulateRequests(metricsTracker, networkID, "upstream-b", methodTraceTransaction, 5, 0)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", methodTraceTransaction, 5, 0.1)
		simulateRequests(metricsTracker, networkID, "upstream-c", methodTraceTransaction, 5, 1)

		time.Sleep(largerRefreshInterval)

		expectedOrderGetLogs := []string{"upstream-a", "upstream-c", "upstream-b"}
		checkUpstreamDistribution(t, registry, networkID, methodGetLogs, iterations, expectedOrderGetLogs)

		expectedOrderTraceTransaction := []string{"upstream-b", "upstream-c", "upstream-a"}
		checkUpstreamDistribution(t, registry, networkID, methodTraceTransaction, iterations, expectedOrderTraceTransaction)
	})

	t.Run("DynamicLatencyPerformanceChangesMultipleMethods", func(t *testing.T) {
		largerWindowSize := windowSize * 2
		largerRefreshInterval := refreshInterval * 2
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize, largerRefreshInterval)

		method1 := "eth_call"
		method2 := "eth_getBalance"
		_, _ = registry.GetSortedUpstreams(networkID, method1)
		_, _ = registry.GetSortedUpstreams(networkID, method2)

		// Phase 1: Initial performance
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 5, 0.05)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method2, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method2, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method2, 5, 0.05)

		time.Sleep(largerRefreshInterval)

		expectedOrderMethod1Phase1 := []string{"upstream-a", "upstream-b", "upstream-c"}
		checkUpstreamDistribution(t, registry, networkID, method1, iterations, expectedOrderMethod1Phase1)

		expectedOrderMethod2Phase1 := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamDistribution(t, registry, networkID, method2, iterations, expectedOrderMethod2Phase1)

		// Sleep for the duration of largerWindowSize to ensure metrics from phase 1 have cycled out
		time.Sleep(largerWindowSize)

		// Phase 2: Performance changes
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 5, 0.05)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method2, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method2, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method2, 5, 0.05)

		time.Sleep(largerRefreshInterval)

		expectedOrderMethod1Phase2 := []string{"upstream-b", "upstream-c", "upstream-a"}
		checkUpstreamDistribution(t, registry, networkID, method1, iterations, expectedOrderMethod1Phase2)

		expectedOrderMethod2Phase2 := []string{"upstream-a", "upstream-c", "upstream-b"}
		checkUpstreamDistribution(t, registry, networkID, method2, iterations, expectedOrderMethod2Phase2)
	})

	t.Run("LatencyVsReliabilityScoring", func(t *testing.T) {
		largerWindowSize := 6 * time.Second
		largerRefreshInterval := 2 * time.Second
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize, largerRefreshInterval)

		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Simulate upstream A: 500ms latency, 20% failure rate
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, 80, 0.5)
		simulateFailedRequests(metricsTracker, networkID, "upstream-a", method, 20)

		// Simulate upstream B: 1000ms latency, 1% failure rate
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, 99, 1.0)
		simulateFailedRequests(metricsTracker, networkID, "upstream-b", method, 1)

		// Simulate upstream C: 750ms latency, 10% failure rate
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, 90, 0.75)
		simulateFailedRequests(metricsTracker, networkID, "upstream-c", method, 10)

		time.Sleep(largerRefreshInterval)

		expectedOrder := []string{"upstream-b", "upstream-c", "upstream-a"}
		checkUpstreamDistribution(t, registry, networkID, method, iterations, expectedOrder)

		// Log the metrics for better understanding
		for _, ups := range expectedOrder {
			metrics := metricsTracker.GetUpstreamMethodMetrics(networkID, ups, method)
			t.Logf("Upstream %s - Latency: %.2f, Errors: %.0f, Requests: %.0f",
				ups, metrics.P90LatencySecs, metrics.ErrorsTotal, metrics.RequestsTotal)
		}
	})

	t.Run("ScoreReuseAcrossMethods", func(t *testing.T) {
		largerWindowSize := 8 * time.Second
		largerRefreshInterval := 2 * time.Second
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize, largerRefreshInterval)

		method1 := "eth_getLogs"
		method2 := "eth_blockNumber"

		_, _ = registry.GetSortedUpstreams(networkID, method1)
		_, _ = registry.GetSortedUpstreams(networkID, method2)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 90, 0.1)
		simulateFailedRequests(metricsTracker, networkID, "upstream-a", method1, 10)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 95, 0.2)
		simulateFailedRequests(metricsTracker, networkID, "upstream-b", method1, 5)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 98, 0.3)
		simulateFailedRequests(metricsTracker, networkID, "upstream-c", method1, 2)

		time.Sleep(largerRefreshInterval)

		expectedOrderMethod1 := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamDistribution(t, registry, networkID, method1, iterations, expectedOrderMethod1)

		// Log the metrics for method1
		t.Log("Metrics for", method1)
		for _, ups := range expectedOrderMethod1 {
			metrics := metricsTracker.GetUpstreamMethodMetrics(networkID, ups, method1)
			t.Logf("Upstream %s - Latency: %.2f, Errors: %.0f, Requests: %.0f",
				ups, metrics.P90LatencySecs, metrics.ErrorsTotal, metrics.RequestsTotal)
		}

		// Check if the order for method2 is the same as method1
		checkUpstreamDistribution(t, registry, networkID, method2, iterations, expectedOrderMethod1)

		// Log the metrics for method2
		t.Log("Metrics for", method2)
		for _, ups := range expectedOrderMethod1 {
			metrics := metricsTracker.GetUpstreamMethodMetrics(networkID, ups, method2)
			t.Logf("Upstream %s - Latency: %.2f, Errors: %.0f, Requests: %.0f",
				ups, metrics.P90LatencySecs, metrics.ErrorsTotal, metrics.RequestsTotal)
		}
	})
}

func createTestRegistry(projectID string, logger *zerolog.Logger, windowSize, refreshInterval time.Duration) (*UpstreamsRegistry, *health.Tracker) {
	metricsTracker := health.NewTracker(projectID, windowSize, refreshInterval)
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

// Helper function to check the distribution of upstream positions
func checkUpstreamDistribution(t *testing.T, registry *UpstreamsRegistry, networkID, method string, iterations int, expectedOrder []string) {
	results := make(map[string][]int)
	for _, id := range expectedOrder {
		results[id] = make([]int, 3) // Track positions 0, 1, 2
	}

	for i := 0; i < iterations; i++ {
		registry.RefreshUpstreamNetworkMethodScores()
		sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
		assert.NoError(t, err)
		assert.Len(t, sortedUpstreams, 3)

		for pos, ups := range sortedUpstreams[:3] {
			results[ups.Config().Id][pos]++
		}
	}

	// Log the distribution for manual inspection
	t.Logf("Distribution of positions: %v", results)

	// Check if the distribution is roughly as expected
	for i := 0; i < len(expectedOrder)-1; i++ {
		assert.Greater(t, results[expectedOrder[i]][0], results[expectedOrder[i+1]][0],
			"%s should be first more often than %s", expectedOrder[i], expectedOrder[i+1])
	}

	// Ensure each upstream appears in each position at least once
	for _, id := range expectedOrder {
		for pos := 0; pos < 3; pos++ {
			assert.Greater(t, results[id][pos], 0, "%s should appear in position %d at least once", id, pos)
		}
	}
}