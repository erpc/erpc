package upstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/vendors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func TestUpstreamsRegistry(t *testing.T) {
	logger := zerolog.New(zerolog.NewConsoleWriter())
	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_call"
	windowSize := 3 * time.Second

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

	t.Run("CorrectOrderForBlockLag", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, 10*time.Hour)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 0)
		metricsTracker.SetLatestBlockNumber("upstream-a", networkID, 4000090)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 0)
		metricsTracker.SetLatestBlockNumber("upstream-b", networkID, 4000100)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 0)
		metricsTracker.SetLatestBlockNumber("upstream-c", networkID, 3005020)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"upstream-b", "upstream-a", "upstream-c"}
		// It should work for upstream's reference "*" item
		checkUpstreamScoreOrder(t, registry, networkID, "*", expectedOrder)
		// As well as for all method-specific metrics
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForFinalizationLag", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, 10*time.Hour)
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 0)
		metricsTracker.SetFinalizedBlockNumber("upstream-a", networkID, 4000090)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 0)
		metricsTracker.SetFinalizedBlockNumber("upstream-b", networkID, 3005020)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 0)
		metricsTracker.SetFinalizedBlockNumber("upstream-c", networkID, 4000100)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		// It should work for upstream's reference "*" item
		checkUpstreamScoreOrder(t, registry, networkID, "*", expectedOrder)
		// As well as for all method-specific metrics
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

		time.Sleep(100 * time.Millisecond)

		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-a", method, 100, 30, 30)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-b", method, 100, 15, 15)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-c", method, 100, 5, 5)

		expectedOrder := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForTotalRequests", func(t *testing.T) {
		registry, metricsTracker := createTestRegistry(projectID, &logger, windowSize)
		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 1000, 0)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 20000, 0)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 10, 0)

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
}

func TestUpstreamScoring(t *testing.T) {
	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_call"

	type upstreamMetrics struct {
		id           string
		latency      float64
		successRate  float64
		requestCount int
	}

	scenarios := []struct {
		name           string
		windowSize     time.Duration
		upstreamConfig []upstreamMetrics
		expectedOrder  []string
	}{
		{
			name:       "MixedLatencyAndFailureRatePreferLowErrorRate",
			windowSize: 10 * time.Second,
			upstreamConfig: []upstreamMetrics{
				{"upstream-a", 0.5, 0.8, 100},
				{"upstream-b", 1.0, 0.99, 100},
				{"upstream-c", 0.75, 0.9, 100},
			},
			expectedOrder: []string{"upstream-b", "upstream-c", "upstream-a"},
		},
		{
			name:       "ExtremeFailureRate",
			windowSize: 6 * time.Second,
			upstreamConfig: []upstreamMetrics{
				{"upstream-a", 1, 0.05, 100},
				{"upstream-b", 1, 0.1, 100},
				{"upstream-c", 1, 0.01, 100},
			},
			expectedOrder: []string{"upstream-b", "upstream-a", "upstream-c"},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			registry, metricsTracker := createTestRegistry(projectID, &log.Logger, scenario.windowSize)
			_, _ = registry.GetSortedUpstreams(networkID, method)

			for _, upstream := range scenario.upstreamConfig {
				successfulRequests := int(float64(upstream.requestCount) * upstream.successRate)
				failedRequests := upstream.requestCount - successfulRequests

				simulateRequestsWithLatency(metricsTracker, networkID, upstream.id, method, successfulRequests, upstream.latency)
				simulateFailedRequests(metricsTracker, networkID, upstream.id, method, failedRequests)
			}

			checkUpstreamScoreOrder(t, registry, networkID, method, scenario.expectedOrder)
		})
	}
}

func TestCalculateScoreDynamicScenarios(t *testing.T) {
	registry := &UpstreamsRegistry{
		scoreRefreshInterval: time.Second,
		logger:               &log.Logger,
	}

	type upstreamMetrics struct {
		totalRequests   float64
		p90Latency      float64
		errorRate       float64
		throttledRate   float64
		blockHeadLag    float64
		finalizationLag float64
	}

	type percentRange struct {
		min float64
		max float64
	}

	type testScenario struct {
		name             string
		upstreams        []upstreamMetrics
		expectedPercents []percentRange
	}

	scenarios := []testScenario{
		{
			name: "Two upstreams with significant difference",
			upstreams: []upstreamMetrics{
				{1, 0.1, 0.01, 0.02, 0, 0},
				{0.8, 0.8, 0.4, 0.1, 0, 0},
			},
			expectedPercents: []percentRange{
				{0.65, 0.75},
				{0.25, 0.35},
			},
		},
		{
			name: "Three upstreams with varying performance",
			upstreams: []upstreamMetrics{
				{1, 0.2, 0.02, 0.01, 0, 0},
				{0.7, 0.5, 0.1, 0.05, 0, 0},
				{0.3, 1.0, 0.3, 0.2, 0, 0},
			},
			expectedPercents: []percentRange{
				{0.40, 0.55},
				{0.30, 0.40},
				{0.10, 0.30},
			},
		},
		{
			name: "Four upstreams with similar performance",
			upstreams: []upstreamMetrics{
				{0.9, 0.3, 0.05, 0.03, 0, 0.0},
				{0.8, 0.4, 0.06, 0.04, 0, 0.0},
				{1.0, 0.2, 0.04, 0.02, 0, 0.0},
				{0.7, 0.5, 0.07, 0.05, 0, 0.0},
			},
			expectedPercents: []percentRange{
				{0.20, 0.30},
				{0.20, 0.30},
				{0.25, 0.35},
				{0.15, 0.25},
			},
		},
		{
			name: "Two upstreams with extreme differences",
			upstreams: []upstreamMetrics{
				{1.0, 0.05, 0.001, 0.001, 0, 0.0},
				{1.0, 1.0, 0.5, 0.5, 0, 0.0},
			},
			expectedPercents: []percentRange{
				{0.70, 1.00},
				{0.00, 0.30},
			},
		},
		{
			name: "Two upstreams with extreme block lags",
			upstreams: []upstreamMetrics{
				{1.0, 0.05, 0.001, 0.001, 1.0, 0.0},
				{1.0, 0.05, 0.001, 0.001, 0.1, 0.0},
			},
			expectedPercents: []percentRange{
				{0.30, 0.50},
				{0.50, 0.60},
			},
		},
		{
			name: "Two upstreams with extreme finalization lags",
			upstreams: []upstreamMetrics{
				{1.0, 0.05, 0.001, 0.001, 0.0, 1.0},
				{1.0, 0.05, 0.001, 0.001, 0.0, 0.1},
			},
			expectedPercents: []percentRange{
				{0.30, 0.50},
				{0.50, 0.60},
			},
		},
		{
			name: "Two upstreams with small block lag",
			upstreams: []upstreamMetrics{
				{1.0, 0.05, 0.001, 0.001, 0.4, 0.0},
				{1.0, 0.05, 0.001, 0.001, 0.5, 0.0},
			},
			expectedPercents: []percentRange{
				{0.5, 0.6},
				{0.4, 0.5},
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			scores := make([]float64, len(scenario.upstreams))
			totalScore := 0.0

			for i, ups := range scenario.upstreams {
				score := registry.calculateScore(
					ups.totalRequests,
					ups.p90Latency,
					ups.errorRate,
					ups.throttledRate,
					ups.blockHeadLag,
					ups.finalizationLag,
				)
				scores[i] = float64(score)
				totalScore += float64(score)
			}

			for i, score := range scores {
				percent := score / totalScore
				// fmt.Printf("Upstream %d: Score: %f, Percent: %f\n", i+1, score, percent)

				assert.GreaterOrEqual(t, percent, scenario.expectedPercents[i].min,
					"Upstream %d percent should be greater than or equal to %f", i+1, scenario.expectedPercents[i].min)
				assert.LessOrEqual(t, percent, scenario.expectedPercents[i].max,
					"Upstream %d percent should be less than or equal to %f", i+1, scenario.expectedPercents[i].max)
			}
		})
	}
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
		1*time.Second,
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

	assert.NoError(t, err)
	registry.RLockUpstreams()
	for i, ups := range sortedUpstreams {
		assert.Equal(t, expectedOrder[i], ups.Config().Id)
	}
	registry.RUnlockUpstreams()
}
