package upstream

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	telemetry.SetHistogramBuckets("0.05,0.5,5,30")
}

func TestUpstreamsRegistry_Ordering(t *testing.T) {
	logger := log.Logger
	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_call"
	windowSize := 1000 * time.Millisecond

	t.Run("RefreshScoresForRequests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 20)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForLatency", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, 10, 0.20)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, 10, 0.70)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, 10, 0.02)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForErrorRate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 10*time.Hour)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 80)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForBlockLag", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 10*time.Hour)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 10*time.Hour)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method, 10, 0.05)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method, 10, 0.03)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method, 10, 0.01)

		expectedOrder := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForErrorRateOverTime", func(t *testing.T) {
		windowSize := 100 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

		// Initial phase
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 80)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 10)

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)

		// Simulate time passing and metrics reset
		time.Sleep(windowSize + 10*time.Millisecond)

		// Second phase
		simulateRequests(metricsTracker, networkID, "upstream-a", method, 100, 30)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 100, 10)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 100, 80)

		expectedOrder = []string{"upstream-b", "upstream-a", "upstream-c"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForRateLimiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

		time.Sleep(100 * time.Millisecond)

		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-a", method, 100, 30, 30)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-b", method, 100, 15, 15)
		simulateRequestsWithRateLimiting(metricsTracker, networkID, "upstream-c", method, 100, 5, 5)

		expectedOrder := []string{"upstream-c", "upstream-b", "upstream-a"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForTotalRequests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		method := "eth_call"
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

		simulateRequests(metricsTracker, networkID, "upstream-a", method, 1000, 0)
		simulateRequests(metricsTracker, networkID, "upstream-b", method, 20000, 0)
		simulateRequests(metricsTracker, networkID, "upstream-c", method, 10, 0)

		expectedOrder := []string{"upstream-c", "upstream-a", "upstream-b"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForMultipleMethodsRequests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)

		methodGetLogs := "eth_getLogs"
		methodTraceTransaction := "eth_traceTransaction"
		_, _ = registry.GetSortedUpstreams(ctx, networkID, methodGetLogs)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, methodTraceTransaction)

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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)

		method1 := "eth_call"
		method2 := "eth_getBalance"
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method1)
		_, _ = registry.GetSortedUpstreams(ctx, networkID, method2)

		// Phase 1: Initial performance
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-a", method1, 5, 0.01)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-c", method1, 5, 0.3)
		simulateRequestsWithLatency(metricsTracker, networkID, "upstream-b", method1, 5, 0.8)

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

func TestUpstreamsRegistry_Scoring(t *testing.T) {
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
				{"upstream-a", 1, 0.2, 100},
				{"upstream-b", 1, 0.8, 100},
				{"upstream-c", 1, 0.01, 100},
			},
			expectedOrder: []string{"upstream-b", "upstream-a", "upstream-c"},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			registry, metricsTracker := createTestRegistry(ctx, projectID, &log.Logger, scenario.windowSize)
			_, _ = registry.GetSortedUpstreams(ctx, networkID, method)

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

func TestUpstreamsRegistry_DynamicScenarios(t *testing.T) {
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
				u := &Upstream{
					config: &common.UpstreamConfig{
						Id: fmt.Sprintf("upstream-%d", i),
					},
				}
				score := registry.calculateScore(
					u,
					"*",
					"*",
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

func TestUpstreamsRegistry_Multiplier(t *testing.T) {
	registry := &UpstreamsRegistry{
		scoreRefreshInterval: time.Second,
		logger:               &log.Logger,
	}

	type upstreamConfig struct {
		id                 string
		priorityMultiplier *common.ScoreMultiplierConfig
		metrics            struct {
			totalRequests   float64
			p90Latency      float64
			errorRate       float64
			throttledRate   float64
			blockHeadLag    float64
			finalizationLag float64
		}
	}

	type expectedRange struct {
		min float64
		max float64
	}

	scenarios := []struct {
		name             string
		networkId        string
		method           string
		upstreams        []upstreamConfig
		expectedPercents []expectedRange
		description      string
	}{
		{
			name:      "Premium provider preferred for eth_call",
			networkId: "evm:1",
			method:    "eth_call",
			upstreams: []upstreamConfig{
				{
					id: "premium-provider",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:   "evm:1",
						Method:    "eth_call",
						Overall:   2.0, // Double the weight for this specific method
						ErrorRate: 8.0, // Heavily penalize errors
					},
					metrics: struct {
						totalRequests, p90Latency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						p90Latency:    0.2,
						errorRate:     0.01, // Very low error rate
						throttledRate: 0.01,
					},
				},
				{
					id: "standard-provider",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:   "evm:1",
						Method:    "eth_call",
						Overall:   1.0,
						ErrorRate: 2.0,
					},
					metrics: struct {
						totalRequests, p90Latency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 800,
						p90Latency:    0.15, // Actually faster
						errorRate:     0.02,
						throttledRate: 0.02,
					},
				},
			},
			expectedPercents: []expectedRange{
				{0.85, 0.95}, // Premium provider should get majority of traffic
				{0.05, 0.15}, // Standard provider gets less despite better latency
			},
			description: "Premium provider should receive more traffic due to higher overall multiplier, even though standard provider has better latency",
		},
		{
			name:      "Archive node preferred for eth_getLogs",
			networkId: "evm:1",
			method:    "eth_getLogs",
			upstreams: []upstreamConfig{
				{
					id: "archive-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:    "evm:1",
						Method:     "eth_getLogs",
						Overall:    3.0, // Heavily prefer for historical queries
						P90Latency: 2.0, // Latency less important for historical data
					},
					metrics: struct {
						totalRequests, p90Latency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 500,
						p90Latency:    0.5, // Slower but more reliable
						errorRate:     0.01,
						throttledRate: 0.01,
					},
				},
				{
					id: "full-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:    "evm:1",
						Method:     "eth_getLogs",
						Overall:    1.0,
						P90Latency: 2.0,
					},
					metrics: struct {
						totalRequests, p90Latency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 200,
						p90Latency:    0.2,
						errorRate:     0.05, // Higher error rate for historical queries
						throttledRate: 0.02,
					},
				},
			},
			expectedPercents: []expectedRange{
				{0.50, 0.60}, // Archive node should get majority of historical queries
				{0.40, 0.50}, // Full node gets less traffic for historical data
			},
			description: "Archive node should receive more eth_getLogs traffic due to higher reliability despite slower response times",
		},
		{
			name:      "Low latency node preferred for eth_getBalance",
			networkId: "evm:1",
			method:    "eth_getBalance",
			upstreams: []upstreamConfig{
				{
					id: "fast-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:    "evm:1",
						Method:     "eth_getBalance",
						Overall:    1.0,
						P90Latency: 8.0, // Heavily weight latency
					},
					metrics: struct {
						totalRequests, p90Latency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						p90Latency:    0.05, // Very fast
						errorRate:     0.02,
						throttledRate: 0.01,
					},
				},
				{
					id: "slow-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:    "evm:1",
						Method:     "eth_getBalance",
						Overall:    1.0,
						P90Latency: 8.0, // Same latency weight
					},
					metrics: struct {
						totalRequests, p90Latency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						p90Latency:    0.2,  // Slower
						errorRate:     0.01, // Slightly better error rate
						throttledRate: 0.01,
					},
				},
			},
			expectedPercents: []expectedRange{
				{0.50, 0.60}, // Fast node should get more traffic
				{0.40, 0.50}, // Slow node gets less despite better error rate
			},
			description: "Fast node should receive more eth_getBalance traffic due to better latency, which is heavily weighted for this method",
		},
		{
			name:      "Latest block preference for eth_getBlockByNumber",
			networkId: "evm:1",
			method:    "eth_getBlockByNumber",
			upstreams: []upstreamConfig{
				{
					id: "realtime-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:      "evm:1",
						Method:       "eth_getBlockByNumber",
						Overall:      1.0,
						BlockHeadLag: 5.0, // Heavily weight block lag
					},
					metrics: struct {
						totalRequests, p90Latency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						p90Latency:    0.1,
						errorRate:     0.02,
						throttledRate: 0.01,
						blockHeadLag:  0.4, // Small lag
					},
				},
				{
					id: "delayed-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:      "evm:1",
						Method:       "eth_getBlockByNumber",
						Overall:      1.0,
						BlockHeadLag: 5.0, // Same block lag weight
					},
					metrics: struct {
						totalRequests, p90Latency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						p90Latency:    0.1,
						errorRate:     0.01,
						throttledRate: 0.01,
						blockHeadLag:  0.8, // Larger lag
					},
				},
			},
			expectedPercents: []expectedRange{
				{0.90, 100},  // Realtime node should get more traffic
				{0.00, 0.10}, // Delayed node gets less due to block lag
			},
			description: "Node with smaller block lag should receive more traffic for latest block queries",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			scores := make([]float64, len(scenario.upstreams))
			totalScore := 0.0

			for i, ups := range scenario.upstreams {
				u := &Upstream{
					config: &common.UpstreamConfig{
						Id: ups.id,
						Routing: &common.RoutingConfig{
							ScoreMultipliers: []*common.ScoreMultiplierConfig{ups.priorityMultiplier},
						},
					},
				}

				score := registry.calculateScore(
					u,
					scenario.networkId,
					scenario.method,
					ups.metrics.totalRequests,
					ups.metrics.p90Latency,
					ups.metrics.errorRate,
					ups.metrics.throttledRate,
					ups.metrics.blockHeadLag,
					ups.metrics.finalizationLag,
				)
				scores[i] = float64(score)
				totalScore += float64(score)
			}

			for i, score := range scores {
				percent := score / totalScore
				t.Logf("Upstream %s: Score: %f, Percent: %f", scenario.upstreams[i].id, score, percent)

				assert.GreaterOrEqual(t, percent, scenario.expectedPercents[i].min,
					"Upstream %s percent should be greater than or equal to %f", scenario.upstreams[i].id, scenario.expectedPercents[i].min)
				assert.LessOrEqual(t, percent, scenario.expectedPercents[i].max,
					"Upstream %s percent should be less than or equal to %f", scenario.upstreams[i].id, scenario.expectedPercents[i].max)
			}
		})
	}
}

func createTestRegistry(ctx context.Context, projectID string, logger *zerolog.Logger, windowSize time.Duration) (*UpstreamsRegistry, *health.Tracker) {
	metricsTracker := health.NewTracker(logger, projectID, windowSize)
	metricsTracker.Bootstrap(ctx)

	upstreamConfigs := []*common.UpstreamConfig{
		{Id: "upstream-a", Endpoint: "http://upstream-a.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "upstream-b", Endpoint: "http://upstream-b.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "upstream-c", Endpoint: "http://upstream-c.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(
		logger,
		vr,
		nil,
		nil,
	)
	if err != nil {
		panic(err)
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	registry := NewUpstreamsRegistry(
		ctx,
		logger,
		projectID,
		upstreamConfigs,
		ssr,
		nil, // RateLimitersRegistry not needed for these tests
		vr,
		pr,
		nil, // ProxyPoolRegistry
		metricsTracker,
		1*time.Second,
	)

	err = registry.Bootstrap(ctx)
	if err != nil {
		panic(err)
	}

	err = registry.PrepareUpstreamsForNetwork(ctx, "evm:123")
	if err != nil {
		panic(err)
	}

	return registry, metricsTracker
}

func simulateRequests(tracker *health.Tracker, network, upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, network, method)
		if i < errors {
			tracker.RecordUpstreamFailure(upstream, network, method)
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
			tracker.RecordUpstreamDuration(upstream, network, method, time.Duration(latency*float64(time.Second)), "none")
			// timer := tracker.RecordUpstreamDurationStart(upstream, network, method, "none")
			// time.Sleep(time.Duration(latency * float64(time.Second)))
			// timer.ObserveDuration()
		}()
	}
	wg.Wait()
}

func simulateFailedRequests(tracker *health.Tracker, network, upstream, method string, count int) {
	for i := 0; i < count; i++ {
		tracker.RecordUpstreamRequest(upstream, network, method)
		tracker.RecordUpstreamFailure(upstream, network, method)
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

	sortedUpstreams, err := registry.GetSortedUpstreams(context.Background(), networkID, method)

	assert.NoError(t, err)
	registry.RLockUpstreams()
	for i, ups := range sortedUpstreams {
		assert.Equal(t, expectedOrder[i], ups.Config().Id)
	}
	registry.RUnlockUpstreams()
}
