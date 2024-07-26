package upstream

import (
	"context"
	"fmt"
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
	windowSize := 1 * time.Second

	t.Run("RefreshScoresForRequests", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForLatency", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, 10, 0.20)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, 10, 0.30)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, 10, 0.10)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForErrorRate", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, 10*time.Hour)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 80)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForP90Latency", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, 10, 0.05)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, 10, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, 10, 0.01)

		expectedOrder := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForErrorRateOverTime", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		// Initial phase
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)

		// Simulate time passing and metrics reset
		time.Sleep(windowSize)

		// Second phase
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 15)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 5)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 20)

		expectedOrder = []string{"upstream-b", "upstream-a", "upstream-c"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForRateLimiting", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)
		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-a", method, 100, 10, 5)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-b", method, 100, 5, 2)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-c", method, 100, 2, 1)

		expectedOrder := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForTotalRequests", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)
		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 50, 0)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 0)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 25, 0)

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForMultipleMethodsRequests", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)

		methodGetLogs := "eth_getLogs"
		methodTraceTransaction := "eth_traceTransaction"
		_, _ = registry.GetSortedUpstreams(networkID, methodGetLogs)
		_, _ = registry.GetSortedUpstreams(networkID, methodTraceTransaction)

		// Simulate performance for eth_getLogs
		simulateRequests(metricsTracker, networkID, "upstream-a", methodGetLogs, 100, 10)
		simulateRequests(metricsTracker, networkID, "upstream-b", methodGetLogs, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", methodGetLogs, 100, 20)

		// Simulate performance for eth_traceTransaction
		simulateRequests(metricsTracker, networkID, "upstream-a", methodTraceTransaction, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", methodTraceTransaction, 100, 10)
		simulateRequests(metricsTracker, networkID, "upstream-c", methodTraceTransaction, 100, 30)

		expectedOrderGetLogs := []string{"upstream-a", "upstream-c", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, methodGetLogs, expectedOrderGetLogs)

		expectedOrderTraceTransaction := []string{"upstream-b", "upstream-a", "upstream-c"}
		checkUpstreamScoreOrder(t, registry, networkID, methodTraceTransaction, expectedOrderTraceTransaction)
	})

	t.Run("CorrectOrderForMultipleMethodsLatencyOverTime", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)

		method1 := "eth_call"
		method2 := "eth_getBalance"
		_, _ = registry.GetSortedUpstreams(networkID, method1)
		_, _ = registry.GetSortedUpstreams(networkID, method2)

		// Phase 1: Initial performance
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 5, 0.05)

		expectedOrderMethod1Phase1 := []string{"upstream-a", "upstream-c", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method1, expectedOrderMethod1Phase1)

		// Wait so that ltency averages are cycled out
		time.Sleep(windowSize)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method2, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method2, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method2, 5, 0.05)

		expectedOrderMethod2Phase1 := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamScoreOrder(t, registry, networkID, method2, expectedOrderMethod2Phase1)

		// Sleep for the duration of windowSize to ensure metrics from phase 1 have cycled out
		time.Sleep(windowSize)

		// Phase 2: Performance changes
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 5, 0.05)

		expectedOrderMethod1Phase2 := []string{"upstream-b", "upstream-c", "upstream-a"}
		checkUpstreamScoreOrder(t, registry, networkID, method1, expectedOrderMethod1Phase2)

		time.Sleep(windowSize)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method2, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method2, 5, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method2, 5, 0.05)

		expectedOrderMethod2Phase2 := []string{"upstream-a", "upstream-c", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method2, expectedOrderMethod2Phase2)
	})

	t.Run("CorrectOrderForLatencyVsReliability", func(t *testing.T) {
		largerWindowSize := 6 * time.Second
		registry, metricsTracker := createTestRegistry(projectID, &logger, largerWindowSize)

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

		expectedOrder := []string{"upstream-b", "upstream-c", "upstream-a"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})
}

func createTestRegistry(projectID string, logger *zerolog.Logger, windowSize time.Duration) (*UpstreamsRegistry, *health.Tracker) {
	metricsTracker := health.NewTracker(projectID, windowSize)
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
		tracker.RecordUpstreamRequest(upstream, network, method)
		if i < errors {
			tracker.RecordUpstreamFailure(upstream, network, method, "test-error")
		}
		timer := tracker.RecordUpstreamDurationStart(upstream, network, method)
		timer.ObserveDuration()
	}
}

func simulateRequestsWithRateLimiting(tracker *health.Tracker, network, upstream, method string, total, selfLimited, remoteLimited int) {
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

func simulateRequestsWithLatency(tracker *health.Tracker, network, upstream, method string, total int, latency float64) {
	wg := sync.WaitGroup{}
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordUpstreamRequest(upstream, network, method)
			tracker.RecordUpstreamDuration(upstream, network, method, time.Duration(latency*float64(time.Second)))
			// timer := tracker.RecordUpstreamDurationStart(upstream, network, method)
			// time.Sleep(time.Duration(latency * float64(time.Second)))
			// timer.ObserveDuration()
		}()
	}
	wg.Wait()
}

func simulateFailedRequests(tracker *health.Tracker, network, upstream, method string, count int) {
	for i := 0; i < count; i++ {
		tracker.RecordUpstreamRequest(upstream, network, method)
		tracker.RecordUpstreamFailure(upstream, network, method, "test-error")
	}
}

func checkUpstreamScoreOrder(t *testing.T, registry *UpstreamsRegistry, networkID, method string, expectedOrder []string) {
	registry.RefreshUpstreamNetworkMethodScores()
	scores := registry.upstreamScores
	fmt.Printf("Checking recorded scores: %v for order: %s\n", scores, expectedOrder)

	for i, ups := range expectedOrder {
		if i+1 < len(expectedOrder) {
			assert.Greater(
				t,
				scores[ups][networkID][method],
				scores[expectedOrder[i+1]][networkID][method],
				"Upstream %s should have a higher score than %s",
				ups,
				expectedOrder[i+1],
			)
		}
	}

	sortedUpstreams, err := registry.GetSortedUpstreams(networkID, method)
	fmt.Printf("Checking upstream order: %v\n", sortedUpstreams)

	assert.NoError(t, err)
	for i, ups := range sortedUpstreams {
		assert.Equal(t, expectedOrder[i], ups.Config().Id)
	}
}
