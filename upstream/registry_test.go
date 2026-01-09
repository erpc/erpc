package upstream

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	telemetry.SetHistogramBuckets("0.05,0.5,5,30")
}

func getUpsByID(upsList []common.Upstream, ids ...string) []common.Upstream {
	var ups []common.Upstream
	for _, id := range ids {
		for _, u := range upsList {
			if u.Id() == id {
				ups = append(ups, u)
				break
			}
		}
	}
	return ups
}

func TestUpstreamsRegistry_Ordering(t *testing.T) {
	logger := log.Logger
	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_call"
	windowSize := 10000 * time.Millisecond

	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	t.Run("RefreshScoresForRequests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		simulateRequests(metricsTracker, upsList[0], method, 100, 20)
		simulateRequests(metricsTracker, upsList[1], method, 100, 30)
		simulateRequests(metricsTracker, upsList[2], method, 100, 10)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"rpc3", "rpc1", "rpc2"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForLatency", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		simulateRequestsWithLatency(metricsTracker, upsList[0], method, 10, 0.20)
		simulateRequestsWithLatency(metricsTracker, upsList[1], method, 10, 0.70)
		simulateRequestsWithLatency(metricsTracker, upsList[2], method, 10, 0.02)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"rpc3", "rpc1", "rpc2"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForErrorRate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 10*time.Hour)
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		simulateRequests(metricsTracker, upsList[0], method, 100, 30)
		simulateRequests(metricsTracker, upsList[1], method, 100, 80)
		simulateRequests(metricsTracker, upsList[2], method, 100, 10)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"rpc3", "rpc1", "rpc2"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForBlockLag", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 10*time.Hour)
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		simulateRequests(metricsTracker, upsList[0], method, 100, 0)
		metricsTracker.SetLatestBlockNumber(upsList[0], 4000090, 0)
		simulateRequests(metricsTracker, upsList[1], method, 100, 0)
		metricsTracker.SetLatestBlockNumber(upsList[1], 4000100, 0)
		simulateRequests(metricsTracker, upsList[2], method, 100, 0)
		metricsTracker.SetLatestBlockNumber(upsList[2], 3005020, 0)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"rpc2", "rpc1", "rpc3"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForFinalizationLag", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 10*time.Hour)
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		simulateRequests(metricsTracker, upsList[0], method, 100, 0)
		metricsTracker.SetFinalizedBlockNumber(upsList[0], 4000090)
		simulateRequests(metricsTracker, upsList[1], method, 100, 0)
		metricsTracker.SetFinalizedBlockNumber(upsList[1], 3005020)
		simulateRequests(metricsTracker, upsList[2], method, 100, 0)
		metricsTracker.SetFinalizedBlockNumber(upsList[2], 4000100)

		registry.RefreshUpstreamNetworkMethodScores()

		expectedOrder := []string{"rpc3", "rpc1", "rpc2"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForRespLatency", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		simulateRequestsWithLatency(metricsTracker, upsList[0], method, 10, 0.05)
		simulateRequestsWithLatency(metricsTracker, upsList[1], method, 10, 0.03)
		simulateRequestsWithLatency(metricsTracker, upsList[2], method, 10, 0.01)

		expectedOrder := []string{"rpc3", "rpc2", "rpc1"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForErrorRateOverTime", func(t *testing.T) {
		windowSize := 100 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		// Initial phase
		simulateRequests(metricsTracker, upsList[0], method, 100, 30)
		simulateRequests(metricsTracker, upsList[1], method, 100, 80)
		simulateRequests(metricsTracker, upsList[2], method, 100, 10)

		expectedOrder := []string{"rpc3", "rpc1", "rpc2"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)

		// Simulate time passing and metrics reset
		time.Sleep(windowSize + 10*time.Millisecond)

		// Second phase
		simulateRequests(metricsTracker, upsList[0], method, 100, 30)
		simulateRequests(metricsTracker, upsList[1], method, 100, 10)
		simulateRequests(metricsTracker, upsList[2], method, 100, 80)

		expectedOrder = []string{"rpc2", "rpc1", "rpc3"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForRateLimiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		method := "eth_call"
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		time.Sleep(100 * time.Millisecond)

		simulateRequestsWithRateLimiting(metricsTracker, upsList[0], method, 100, 30, 30)
		simulateRequestsWithRateLimiting(metricsTracker, upsList[1], method, 100, 15, 15)
		simulateRequestsWithRateLimiting(metricsTracker, upsList[2], method, 100, 5, 5)

		expectedOrder := []string{"rpc3", "rpc2", "rpc1"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForTotalRequests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)
		method := "eth_call"
		l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

		simulateRequests(metricsTracker, upsList[0], method, 1000, 0)
		simulateRequests(metricsTracker, upsList[1], method, 20000, 0)
		simulateRequests(metricsTracker, upsList[2], method, 10, 0)

		expectedOrder := []string{"rpc3", "rpc1", "rpc2"}
		checkUpstreamScoreOrder(t, registry, networkID, method, expectedOrder)
	})

	t.Run("CorrectOrderForMultipleMethodsRequests", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, windowSize)

		methodGetLogs := "eth_getLogs"
		methodTraceTransaction := "eth_traceTransaction"
		l, _ := registry.GetSortedUpstreams(ctx, networkID, methodGetLogs)
		upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")
		_, _ = registry.GetSortedUpstreams(ctx, networkID, methodTraceTransaction)

		// Simulate performance for eth_getLogs
		simulateRequests(metricsTracker, upsList[0], methodGetLogs, 100, 10)
		simulateRequests(metricsTracker, upsList[1], methodGetLogs, 100, 30)
		simulateRequests(metricsTracker, upsList[2], methodGetLogs, 100, 20)

		// Simulate performance for eth_traceTransaction
		simulateRequests(metricsTracker, upsList[0], methodTraceTransaction, 100, 20)
		simulateRequests(metricsTracker, upsList[1], methodTraceTransaction, 100, 10)
		simulateRequests(metricsTracker, upsList[2], methodTraceTransaction, 100, 30)

		expectedOrderGetLogs := []string{"rpc1", "rpc3", "rpc2"}
		checkUpstreamScoreOrder(t, registry, networkID, methodGetLogs, expectedOrderGetLogs)

		expectedOrderTraceTransaction := []string{"rpc2", "rpc1", "rpc3"}
		checkUpstreamScoreOrder(t, registry, networkID, methodTraceTransaction, expectedOrderTraceTransaction)
	})

	t.Run("CorrectOrderForMultipleMethodsLatencyOverTime", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Use a shorter window for this test to avoid long sleeps (500ms to allow EMA convergence)
		shortWindowSize := 500 * time.Millisecond
		registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, shortWindowSize)

		method1 := "eth_call"
		method2 := "eth_getBalance"
		l1, _ := registry.GetSortedUpstreams(ctx, networkID, method1)
		l2, _ := registry.GetSortedUpstreams(ctx, networkID, method2)
		upsList1 := getUpsByID(l1, "rpc1", "rpc2", "rpc3")
		upsList2 := getUpsByID(l2, "rpc1", "rpc2", "rpc3")

		// Phase 1: Initial performance - use extreme differences (10x-100x) to overcome EMA smoothing
		simulateRequestsWithLatency(metricsTracker, upsList1[0], method1, 100, 0.001) // 1ms - fastest
		simulateRequestsWithLatency(metricsTracker, upsList1[2], method1, 100, 0.1)   // 100ms
		simulateRequestsWithLatency(metricsTracker, upsList1[1], method1, 100, 1.0)   // 1000ms - slowest

		// Refresh multiple times to ensure EMA convergence
		for i := 0; i < 5; i++ {
			registry.RefreshUpstreamNetworkMethodScores()
		}

		expectedOrderMethod1Phase1 := []string{"rpc1", "rpc3", "rpc2"}
		checkUpstreamScoreOrder(t, registry, networkID, method1, expectedOrderMethod1Phase1)

		// Wait so that latency averages are cycled out
		time.Sleep(shortWindowSize + 50*time.Millisecond)

		simulateRequestsWithLatency(metricsTracker, upsList2[2], method2, 100, 0.001) // 1ms - fastest
		simulateRequestsWithLatency(metricsTracker, upsList2[1], method2, 100, 0.05)  // 50ms
		simulateRequestsWithLatency(metricsTracker, upsList2[0], method2, 100, 0.5)   // 500ms - slowest

		for i := 0; i < 5; i++ {
			registry.RefreshUpstreamNetworkMethodScores()
		}

		expectedOrderMethod2Phase1 := []string{"rpc3", "rpc2", "rpc1"}
		checkUpstreamScoreOrder(t, registry, networkID, method2, expectedOrderMethod2Phase1)

		// Sleep to ensure metrics from phase 1 have cycled out
		time.Sleep(shortWindowSize + 50*time.Millisecond)

		// Phase 2: Performance changes - use extreme differences
		simulateRequestsWithLatency(metricsTracker, upsList1[1], method1, 100, 0.001) // 1ms - now fastest
		simulateRequestsWithLatency(metricsTracker, upsList1[2], method1, 100, 0.05)  // 50ms
		simulateRequestsWithLatency(metricsTracker, upsList1[0], method1, 100, 0.5)   // 500ms - now slowest

		// Refresh multiple times to let EMA converge
		for i := 0; i < 5; i++ {
			registry.RefreshUpstreamNetworkMethodScores()
		}

		expectedOrderMethod1Phase2 := []string{"rpc2", "rpc3", "rpc1"}
		checkUpstreamScoreOrder(t, registry, networkID, method1, expectedOrderMethod1Phase2)

		// Sleep to ensure metrics from phase 2 for method1 have cycled out
		time.Sleep(shortWindowSize + 50*time.Millisecond)

		simulateRequestsWithLatency(metricsTracker, upsList2[0], method2, 100, 0.001) // 1ms - now fastest
		simulateRequestsWithLatency(metricsTracker, upsList2[2], method2, 100, 0.1)   // 100ms
		simulateRequestsWithLatency(metricsTracker, upsList2[1], method2, 100, 1.0)   // 1000ms - now slowest

		// Refresh multiple times to let EMA converge
		for i := 0; i < 5; i++ {
			registry.RefreshUpstreamNetworkMethodScores()
		}

		expectedOrderMethod2Phase2 := []string{"rpc1", "rpc3", "rpc2"}
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
			name:       "MixedLatencyAndFailureRatePreferLowLatency",
			windowSize: 10 * time.Second,
			upstreamConfig: []upstreamMetrics{
				{"rpc1", 0.5, 0.1, 100},
				{"rpc2", 1.0, 0.05, 100},
				{"rpc3", 0.75, 0.15, 100},
			},
			expectedOrder: []string{"rpc1", "rpc3", "rpc2"},
		},
		{
			name:       "ExtremeFailureRate",
			windowSize: 6 * time.Second,
			upstreamConfig: []upstreamMetrics{
				{"rpc1", 1, 0.2, 100},
				{"rpc2", 1, 0.8, 100},
				{"rpc3", 1, 0.01, 100},
			},
			expectedOrder: []string{"rpc2", "rpc1", "rpc3"},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()
			defer util.AssertNoPendingMocks(t, 0)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			registry, metricsTracker := createTestRegistry(ctx, projectID, &log.Logger, scenario.windowSize)
			l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
			upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")

			for idx, upstream := range scenario.upstreamConfig {
				successfulRequests := int(float64(upstream.requestCount) * upstream.successRate)
				failedRequests := upstream.requestCount - successfulRequests

				simulateRequestsWithLatency(metricsTracker, upsList[idx], method, successfulRequests, upstream.latency)
				simulateFailedRequests(metricsTracker, upsList[idx], method, failedRequests)
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
		respLatency     float64
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
				{0.63, 0.75}, // Adjusted lower bound for misbehavior scoring
				{0.25, 0.37}, // Adjusted upper bound for misbehavior scoring
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
				{0.69, 1.00}, // Adjusted lower bound for misbehavior scoring
				{0.00, 0.31}, // Adjusted upper bound for misbehavior scoring
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
					nil, // No finality context for this test
					ups.totalRequests,
					ups.respLatency,
					ups.errorRate,
					ups.throttledRate,
					ups.blockHeadLag,
					ups.finalizationLag,
					0,
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
			respLatency     float64
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
			networkId: "evm:123",
			method:    "eth_call",
			upstreams: []upstreamConfig{
				{
					id: "premium-provider",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:   "evm:123",
						Method:    "eth_call",
						Overall:   util.Float64Ptr(2.0), // Double the weight for this specific method
						ErrorRate: util.Float64Ptr(8.0), // Heavily penalize errors
					},
					metrics: struct {
						totalRequests, respLatency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						respLatency:   0.2,
						errorRate:     0.01, // Very low error rate
						throttledRate: 0.01,
					},
				},
				{
					id: "standard-provider",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:   "evm:123",
						Method:    "eth_call",
						Overall:   util.Float64Ptr(1.0),
						ErrorRate: util.Float64Ptr(2.0),
					},
					metrics: struct {
						totalRequests, respLatency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 800,
						respLatency:   0.15, // Actually faster
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
			networkId: "evm:123",
			method:    "eth_getLogs",
			upstreams: []upstreamConfig{
				{
					id: "archive-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:     "evm:123",
						Method:      "eth_getLogs",
						Overall:     util.Float64Ptr(3.0), // Heavily prefer for historical queries
						RespLatency: util.Float64Ptr(2.0), // Latency less important for historical data
					},
					metrics: struct {
						totalRequests, respLatency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 500,
						respLatency:   0.5, // Slower but more reliable
						errorRate:     0.01,
						throttledRate: 0.01,
					},
				},
				{
					id: "full-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:     "evm:123",
						Method:      "eth_getLogs",
						Overall:     util.Float64Ptr(1.0),
						RespLatency: util.Float64Ptr(2.0),
					},
					metrics: struct {
						totalRequests, respLatency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 200,
						respLatency:   0.2,
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
			networkId: "evm:123",
			method:    "eth_getBalance",
			upstreams: []upstreamConfig{
				{
					id: "fast-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:     "evm:123",
						Method:      "eth_getBalance",
						Overall:     util.Float64Ptr(1.0),
						RespLatency: util.Float64Ptr(8.0), // Heavily weight latency
					},
					metrics: struct {
						totalRequests, respLatency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						respLatency:   0.05, // Very fast
						errorRate:     0.02,
						throttledRate: 0.01,
					},
				},
				{
					id: "slow-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:     "evm:123",
						Method:      "eth_getBalance",
						Overall:     util.Float64Ptr(1.0),
						RespLatency: util.Float64Ptr(8.0), // Same latency weight
					},
					metrics: struct {
						totalRequests, respLatency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						respLatency:   0.2,  // Slower
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
			networkId: "evm:123",
			method:    "eth_getBlockByNumber",
			upstreams: []upstreamConfig{
				{
					id: "realtime-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:      "evm:123",
						Method:       "eth_getBlockByNumber",
						Overall:      util.Float64Ptr(1.0),
						BlockHeadLag: util.Float64Ptr(5.0), // Heavily weight block lag
					},
					metrics: struct {
						totalRequests, respLatency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						respLatency:   0.1,
						errorRate:     0.02,
						throttledRate: 0.01,
						blockHeadLag:  0.4, // Small lag
					},
				},
				{
					id: "delayed-node",
					priorityMultiplier: &common.ScoreMultiplierConfig{
						Network:      "evm:123",
						Method:       "eth_getBlockByNumber",
						Overall:      util.Float64Ptr(1.0),
						BlockHeadLag: util.Float64Ptr(5.0), // Same block lag weight
					},
					metrics: struct {
						totalRequests, respLatency, errorRate, throttledRate, blockHeadLag, finalizationLag float64
					}{
						totalRequests: 1000,
						respLatency:   0.1,
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
					nil, // No finality context for this test
					ups.metrics.totalRequests,
					ups.metrics.respLatency,
					ups.metrics.errorRate,
					ups.metrics.throttledRate,
					ups.metrics.blockHeadLag,
					ups.metrics.finalizationLag,
					0, // misbehaviorRate - no misbehavior in this test
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

func TestUpstreamsRegistry_FinalitySpecificScoreMultipliers(t *testing.T) {
	registry := &UpstreamsRegistry{
		scoreRefreshInterval: time.Second,
		logger:               &log.Logger,
	}

	scenarios := []struct {
		name        string
		networkId   string
		method      string
		finality    common.DataFinalityState
		description string
	}{
		{
			name:        "Realtime/Unfinalized prefer lowest block lag",
			networkId:   "evm:1",
			method:      "eth_blockNumber",
			finality:    common.DataFinalityStateRealtime,
			description: "For realtime/unfinalized data, the node with lowest block lag should win even if slower",
		},
		{
			name:        "Finalized requests prefer fastest response",
			networkId:   "evm:1",
			method:      "eth_getBalance",
			finality:    common.DataFinalityStateFinalized,
			description: "For finalized data, the fastest node should win even if lagging",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Create empty routing config - SetDefaults will add finality-aware defaults
			routingConfig := &common.RoutingConfig{}
			err := routingConfig.SetDefaults()
			assert.NoError(t, err)

			// Verify we got the finality-aware defaults (2 configs)
			assert.Len(t, routingConfig.ScoreMultipliers, 2, "Should have 2 finality-aware default configs")

			// Create two upstreams using these defaults
			syncedNode := &Upstream{
				config: &common.UpstreamConfig{
					Id:      "synced-node",
					Routing: routingConfig, // Uses finality-aware defaults
				},
			}

			fastNode := &Upstream{
				config: &common.UpstreamConfig{
					Id:      "fast-node",
					Routing: routingConfig, // Uses same finality-aware defaults
				},
			}

			// Calculate scores with opposite trade-offs:
			// Synced node: low lag (good for realtime), high latency (bad)
			// Fast node: high lag (bad for realtime), low latency (good)
			syncedScore := registry.calculateScore(
				syncedNode,
				scenario.networkId,
				scenario.method,
				[]common.DataFinalityState{scenario.finality},
				0.5,  // normTotalRequests (same for both)
				0.9,  // normRespLatency (slow)
				0.05, // normErrorRate (same for both)
				0.0,  // normThrottledRate
				0.1,  // normBlockHeadLag (low - synced)
				0.0,  // normFinalizationLag
				0.0,  // normMisbehaviorRate
			)

			fastScore := registry.calculateScore(
				fastNode,
				scenario.networkId,
				scenario.method,
				[]common.DataFinalityState{scenario.finality},
				0.5,  // normTotalRequests (same for both)
				0.1,  // normRespLatency (fast)
				0.05, // normErrorRate (same for both)
				0.0,  // normThrottledRate
				0.9,  // normBlockHeadLag (high - lagging)
				0.0,  // normFinalizationLag
				0.0,  // normMisbehaviorRate
			)

			totalScore := syncedScore + fastScore
			syncedPercent := (syncedScore / totalScore) * 100
			fastPercent := (fastScore / totalScore) * 100

			t.Logf("%s", scenario.description)
			t.Logf("  Synced node (low lag, slow): Score=%.2f, Percent=%.1f%%", syncedScore, syncedPercent)
			t.Logf("  Fast node (high lag, fast): Score=%.2f, Percent=%.1f%%", fastScore, fastPercent)

			// Assert based on finality expectations
			if scenario.finality == common.DataFinalityStateRealtime || scenario.finality == common.DataFinalityStateUnfinalized {
				// For realtime/unfinalized: synced node should WIN (block lag is prioritized)
				assert.Greater(t, syncedScore, fastScore,
					"For %s requests: synced node should beat fast node", scenario.finality)
			} else {
				// For finalized: fast node should WIN (latency is prioritized)
				assert.Greater(t, fastScore, syncedScore,
					"For %s requests: fast node should beat synced node", scenario.finality)
			}
		})
	}
}

func TestUpstreamsRegistry_ZeroLatencyHandling(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_call"

	// Create registry with existing test upstreams
	ctx := context.Background()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 10*time.Second)

	// Get the existing upstreams
	upstreams := registry.GetAllUpstreams()
	assert.Len(t, upstreams, 3)

	// Find our test upstreams
	var workingUpstream, failingUpstream *Upstream
	for _, ups := range upstreams {
		if ups.Id() == "rpc1" {
			workingUpstream = ups
		} else if ups.Id() == "rpc2" {
			failingUpstream = ups
		}
	}
	assert.NotNil(t, workingUpstream)
	assert.NotNil(t, failingUpstream)

	// Simulate metrics: one upstream has real latency, another has zero latency (100% error rate)
	// Working upstream: some latency, low error rate
	simulateRequestsWithLatency(metricsTracker, workingUpstream, method, 10, 0.1) // 100ms latency
	simulateRequests(metricsTracker, workingUpstream, method, 10, 1)              // 10% error rate

	// Failing upstream: zero latency (no successful requests), high error rate
	simulateRequests(metricsTracker, failingUpstream, method, 10, 10) // 100% error rate
	// No latency simulation for failing upstream - it will have zero latency

	// Initialize scores by calling GetSortedUpstreams first
	_, err := registry.GetSortedUpstreams(context.Background(), networkID, method)
	assert.NoError(t, err)

	// Refresh scores
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)

	// Get sorted upstreams for this specific method
	sortedUpstreams, err := registry.GetSortedUpstreams(context.Background(), networkID, method)
	assert.NoError(t, err)
	assert.Len(t, sortedUpstreams, 3)

	// Verify scores: working upstream should have higher score than failing one
	registry.upstreamsMu.RLock()
	workingScore := registry.upstreamScores["rpc1"][networkID][method]
	failingScore := registry.upstreamScores["rpc2"][networkID][method]
	registry.upstreamsMu.RUnlock()

	assert.Greater(t, workingScore, failingScore, "Working upstream should have higher score than failing upstream")

	t.Logf("Working upstream (rpc1) score: %f", workingScore)
	t.Logf("Failing upstream (rpc2) score: %f", failingScore)

	// Working upstream should be ranked higher than failing upstream
	workingRank := -1
	failingRank := -1
	for i, ups := range sortedUpstreams {
		if ups.Id() == "rpc1" {
			workingRank = i
		} else if ups.Id() == "rpc2" {
			failingRank = i
		}
	}
	assert.Less(t, workingRank, failingRank, "Working upstream should be ranked higher (lower index) than failing upstream")
}

func TestUpstreamsRegistry_EMASmoothingPreventsImmediateFlip(t *testing.T) {
	// This test verifies that EMA smoothing (prev weight 0.7) prevents a small
	// performance change from flipping the leader immediately; it should flip on
	// the second refresh when new metrics are sustained.
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_getBalance"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.Logger
	// Use a generous window so initial samples remain in window across refreshes
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	// Get upstreams
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")
	u1 := upsList[0] // rpc1
	u2 := upsList[1] // rpc2

	// Phase 1: rpc2 slightly faster than rpc1 (establish initial leader = rpc2)
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.060) // 60ms
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.050) // 50ms

	err := registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	ordered, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Equal(t, "rpc2", ordered[0].Id(), "rpc2 should lead after initial refresh")

	// Phase 2: rpc1 becomes just slightly faster than rpc2
	// Without smoothing, a small advantage might instantly flip to rpc1.
	// With smoothing (prev weight 0.7), rpc2 should remain leader for this refresh.
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.048) // 48ms
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.050) // 50ms

	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	ordered, _ = registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Equal(t, "rpc2", ordered[0].Id(), "EMA smoothing should keep rpc2 leading on first post-change refresh")

	// Phase 3: sustain the new advantage with more samples; after extra refreshes, rpc1 should take lead
	simulateRequestsWithLatency(metricsTracker, u1, method, 15, 0.045) // strengthen rpc1 advantage
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	// One more refresh to let EMA converge
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	ordered, _ = registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Equal(t, "rpc1", ordered[0].Id(), "After sustained improvement, rpc1 should take the lead")
}

func TestUpstreamsRegistry_ColdStartConfidenceWeighting(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_call"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	// Get upstreams
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")
	u1 := upsList[0] // rpc1
	u2 := upsList[1] // rpc2
	u3 := upsList[2] // rpc3 (cold)

	// Phase 1: rpc1 is faster than rpc2, rpc3 has no samples (cold start)
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.050) // 50ms
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.080) // 80ms
	// u3: no requests

	err := registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	ordered, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Equal(t, "rpc1", ordered[0].Id(), "Cold upstream should not win with zero samples")

	// Phase 2: u3 gathers a few samples but below confidence threshold
	simulateRequestsWithLatency(metricsTracker, u3, method, 3, 0.030) // 30ms, but only 3 samples
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	ordered, _ = registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Equal(t, "rpc1", ordered[0].Id(), "Below confidence threshold, cold upstream should still not lead")

	// Phase 3: u3 reaches/exceeds confidence samples with very fast latency, then allow smoothing to catch up
	simulateRequestsWithLatency(metricsTracker, u3, method, 10, 0.030) // now >= 10 samples
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	ordered, _ = registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Equal(t, "rpc3", ordered[0].Id(), "After enough samples, the fast upstream should lead")
}

func TestUpstreamsRegistry_PerMethodIsolation(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	methodA := "eth_call"
	methodB := "eth_getBalance"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	// Pre-warm both methods
	lA, _ := registry.GetSortedUpstreams(ctx, networkID, methodA)
	lB, _ := registry.GetSortedUpstreams(ctx, networkID, methodB)
	upsA := getUpsByID(lA, "rpc1", "rpc2", "rpc3")
	upsB := getUpsByID(lB, "rpc1", "rpc2", "rpc3")

	// Method A: rpc1 fastest
	simulateRequestsWithLatency(metricsTracker, upsA[0], methodA, 10, 0.040) // rpc1 40ms
	simulateRequestsWithLatency(metricsTracker, upsA[1], methodA, 10, 0.070) // rpc2 70ms
	simulateRequestsWithLatency(metricsTracker, upsA[2], methodA, 10, 0.060) // rpc3 60ms

	// Method B: rpc2 fastest
	simulateRequestsWithLatency(metricsTracker, upsB[0], methodB, 10, 0.070) // rpc1 70ms
	simulateRequestsWithLatency(metricsTracker, upsB[1], methodB, 10, 0.040) // rpc2 40ms
	simulateRequestsWithLatency(metricsTracker, upsB[2], methodB, 10, 0.060) // rpc3 60ms

	err := registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)

	orderedA, _ := registry.GetSortedUpstreams(ctx, networkID, methodA)
	orderedB, _ := registry.GetSortedUpstreams(ctx, networkID, methodB)
	assert.Equal(t, "rpc1", orderedA[0].Id(), "Method A ordering should be independent and prefer rpc1")
	assert.Equal(t, "rpc2", orderedB[0].Id(), "Method B ordering should be independent and prefer rpc2")
}

func TestUpstreamsRegistry_ThrottlingPenalty(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_getLogs"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")
	u1 := upsList[0]
	u2 := upsList[1]

	// Equal latency for both
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.060)
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.060)
	// Apply throttling to u1 significantly more than u2
	simulateRequestsWithRateLimiting(metricsTracker, u1, method, 20, 10, 5) // more throttling
	simulateRequestsWithRateLimiting(metricsTracker, u2, method, 20, 1, 1)  // less throttling

	err := registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	ordered, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	// We only assert relative ordering between u2 and u1 to avoid interference from other peers
	var i1, i2 int
	for i, u := range ordered {
		if u.Id() == u1.Id() {
			i1 = i
		}
		if u.Id() == u2.Id() {
			i2 = i
		}
	}
	assert.Less(t, i2, i1, "Higher throttling should demote an upstream with equal latency")
}

func TestUpstreamsRegistry_AllPeersNoSamplesNeutral(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_maxPriorityFeePerGas"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, _ := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	// Do not record any metric samples
	err := registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)

	ordered, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	assert.Len(t, ordered, 3)
	// With all-equal effective metrics, ordering may be arbitrary; assert membership
	ids := []string{ordered[0].Id(), ordered[1].Id(), ordered[2].Id()}
	assert.ElementsMatch(t, []string{"rpc1", "rpc2", "rpc3"}, ids)
}

func TestUpstreamsRegistry_EMAFromZero_IncreasesOnSecondRefresh(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_chainId"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	// Get upstreams and set two distinct latencies so instant score > 0 for the faster one
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	upsList := getUpsByID(l, "rpc1", "rpc2")
	faster := upsList[0]
	slower := upsList[1]

	// Assign latencies: faster < slower
	simulateRequestsWithLatency(metricsTracker, faster, method, 10, 0.040) // 40ms
	simulateRequestsWithLatency(metricsTracker, slower, method, 10, 0.100) // 100ms

	// First refresh → first smoothed score with prev==0
	err := registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	registry.upstreamsMu.RLock()
	s1 := registry.upstreamScores[faster.Id()][networkID][method]
	registry.upstreamsMu.RUnlock()
	assert.Greater(t, s1, 0.0, "first score should be > 0 for faster upstream")

	// Second refresh (same metrics) → EMA should increase compared to first
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)
	registry.upstreamsMu.RLock()
	s2 := registry.upstreamScores[faster.Id()][networkID][method]
	registry.upstreamsMu.RUnlock()
	assert.Greater(t, s2, s1, "EMA should increase on the second refresh with identical metrics (prev==0 applied)")
}

func createTestRegistry(ctx context.Context, projectID string, logger *zerolog.Logger, windowSize time.Duration) (*UpstreamsRegistry, *health.Tracker) {
	metricsTracker := health.NewTracker(logger, projectID, windowSize)
	metricsTracker.Bootstrap(ctx)

	upstreamConfigs := []*common.UpstreamConfig{
		{Id: "rpc1", Endpoint: "http://rpc1.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "rpc2", Endpoint: "http://rpc2.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "rpc3", Endpoint: "http://rpc3.localhost", Type: common.UpstreamTypeEvm, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
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
				MaxItems: 100_000, MaxTotalSize: "1GB",
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
		nil,
	)

	registry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	err = registry.PrepareUpstreamsForNetwork(ctx, "evm:123")
	if err != nil {
		panic(err)
	}

	return registry, metricsTracker
}

func simulateRequests(tracker *health.Tracker, upstream common.Upstream, method string, total, errors int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		if i < errors {
			tracker.RecordUpstreamFailure(upstream, method, fmt.Errorf("test problem"))
		}
	}
}

func simulateRequestsWithRateLimiting(tracker *health.Tracker, upstream common.Upstream, method string, total, selfLimited, remoteLimited int) {
	for i := 0; i < total; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		if i < selfLimited {
			tracker.RecordUpstreamSelfRateLimited(upstream, method, nil)
		}
		if i >= selfLimited && i < selfLimited+remoteLimited {
			tracker.RecordUpstreamRemoteRateLimited(context.Background(), upstream, method, nil)
		}
	}
}

func simulateRequestsWithLatency(tracker *health.Tracker, upstream common.Upstream, method string, total int, latency float64) {
	wg := sync.WaitGroup{}
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.RecordUpstreamRequest(upstream, method)
			tracker.RecordUpstreamDuration(upstream, method, time.Duration(latency*float64(time.Second)), true, "none", common.DataFinalityStateUnknown, "n/a")
			// timer := tracker.RecordUpstreamDurationStart(upstream, network, method, "none", common.DataFinalityStateUnknown, "n/a")
			// time.Sleep(time.Duration(latency * float64(time.Second)))
			// timer.ObserveDuration()
		}()
	}
	wg.Wait()
}

func simulateFailedRequests(tracker *health.Tracker, upstream common.Upstream, method string, count int) {
	for i := 0; i < count; i++ {
		tracker.RecordUpstreamRequest(upstream, method)
		tracker.RecordUpstreamFailure(upstream, method, fmt.Errorf("test problem"))
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
		assert.Equal(t, expectedOrder[i], ups.Id())
	}
	registry.RUnlockUpstreams()
}

func TestUpstreamsRegistry_NaNGuardsPreventPropagation(t *testing.T) {
	// This test verifies that NaN values in scores don't propagate through
	// EMA smoothing and don't get emitted to Prometheus metrics.
	// NaN can occur from edge cases in metrics collection and once present
	// would propagate indefinitely through EMA calculations without guards.
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_getBalance"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	// Get upstreams
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")
	u1 := upsList[0]
	u2 := upsList[1]
	u3 := upsList[2]

	// Simulate some requests to establish initial scores
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.050)
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.060)
	simulateRequestsWithLatency(metricsTracker, u3, method, 10, 0.070)

	// Run multiple refresh cycles to verify scores remain valid
	for i := 0; i < 10; i++ {
		err := registry.RefreshUpstreamNetworkMethodScores()
		assert.NoError(t, err)

		// Verify no scores are NaN after refresh
		for upsID, networkScores := range registry.upstreamScores {
			for netID, methodScores := range networkScores {
				for meth, score := range methodScores {
					assert.False(t, math.IsNaN(score),
						"Score for upstream %s, network %s, method %s should not be NaN (iteration %d)",
						upsID, netID, meth, i)
					assert.False(t, math.IsInf(score, 0),
						"Score for upstream %s, network %s, method %s should not be Inf (iteration %d)",
						upsID, netID, meth, i)
				}
			}
		}

		// Add more requests between refreshes to vary conditions
		simulateRequestsWithLatency(metricsTracker, u1, method, 5, 0.040+float64(i)*0.001)
		simulateRequestsWithLatency(metricsTracker, u2, method, 5, 0.055+float64(i)*0.002)
	}

	// Verify final sorted order is valid (no NaN-induced sorting issues)
	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	assert.NoError(t, err)
	assert.Len(t, ordered, 3, "Should have 3 upstreams in sorted order")

	// Verify scores are in descending order (higher score = higher priority)
	scores := registry.upstreamScores
	for i := 0; i < len(ordered)-1; i++ {
		curr := ordered[i]
		next := ordered[i+1]
		currScore := scores[curr.Id()][networkID][method]
		nextScore := scores[next.Id()][networkID][method]
		assert.GreaterOrEqual(t, currScore, nextScore,
			"Upstream %s (score %.4f) should have >= score than %s (score %.4f)",
			curr.Id(), currScore, next.Id(), nextScore)
	}
}

func TestUpstreamsRegistry_CalculateScoreEdgeCases(t *testing.T) {
	// Test that calculateScore handles edge cases without producing NaN
	registry := &UpstreamsRegistry{
		scoreRefreshInterval: time.Second,
		logger:               &log.Logger,
	}

	routingConfig := &common.RoutingConfig{}
	err := routingConfig.SetDefaults()
	assert.NoError(t, err)

	upstream := &Upstream{
		config: &common.UpstreamConfig{
			Id:      "test-upstream",
			Routing: routingConfig,
		},
	}

	testCases := []struct {
		name                string
		normTotalRequests   float64
		normRespLatency     float64
		normErrorRate       float64
		normThrottledRate   float64
		normBlockHeadLag    float64
		normFinalizationLag float64
		normMisbehaviorRate float64
	}{
		{
			name:                "All zeros",
			normTotalRequests:   0, normRespLatency: 0, normErrorRate: 0,
			normThrottledRate: 0, normBlockHeadLag: 0, normFinalizationLag: 0, normMisbehaviorRate: 0,
		},
		{
			name:                "All ones",
			normTotalRequests:   1, normRespLatency: 1, normErrorRate: 1,
			normThrottledRate: 1, normBlockHeadLag: 1, normFinalizationLag: 1, normMisbehaviorRate: 1,
		},
		{
			name:                "Mixed values",
			normTotalRequests:   0.5, normRespLatency: 0.3, normErrorRate: 0.1,
			normThrottledRate: 0.2, normBlockHeadLag: 0.4, normFinalizationLag: 0.05, normMisbehaviorRate: 0.01,
		},
		{
			name:                "Boundary high",
			normTotalRequests:   0.999, normRespLatency: 0.999, normErrorRate: 0.999,
			normThrottledRate: 0.999, normBlockHeadLag: 0.999, normFinalizationLag: 0.999, normMisbehaviorRate: 0.999,
		},
		{
			name:                "Boundary low",
			normTotalRequests:   0.001, normRespLatency: 0.001, normErrorRate: 0.001,
			normThrottledRate: 0.001, normBlockHeadLag: 0.001, normFinalizationLag: 0.001, normMisbehaviorRate: 0.001,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			score := registry.calculateScore(
				upstream,
				"evm:1",
				"eth_getBalance",
				[]common.DataFinalityState{common.DataFinalityStateFinalized},
				tc.normTotalRequests,
				tc.normRespLatency,
				tc.normErrorRate,
				tc.normThrottledRate,
				tc.normBlockHeadLag,
				tc.normFinalizationLag,
				tc.normMisbehaviorRate,
			)

			assert.False(t, math.IsNaN(score), "Score should not be NaN for test case: %s", tc.name)
			assert.False(t, math.IsInf(score, 0), "Score should not be Inf for test case: %s", tc.name)
			assert.GreaterOrEqual(t, score, 0.0, "Score should be non-negative for test case: %s", tc.name)
		})
	}
}

func TestNormalizeValues_HandlesNaNAndInf(t *testing.T) {
	// Test that normalizeValues handles NaN and Inf inputs correctly
	testCases := []struct {
		name     string
		input    []float64
		expected []float64
	}{
		{
			name:     "Normal values",
			input:    []float64{1.0, 2.0, 4.0},
			expected: []float64{0.25, 0.5, 1.0},
		},
		{
			name:     "With NaN",
			input:    []float64{1.0, math.NaN(), 4.0},
			expected: []float64{0.25, 0.0, 1.0},
		},
		{
			name:     "With Inf",
			input:    []float64{1.0, math.Inf(1), 4.0},
			expected: []float64{0.25, 0.0, 1.0},
		},
		{
			name:     "All NaN",
			input:    []float64{math.NaN(), math.NaN(), math.NaN()},
			expected: []float64{0.0, 0.0, 0.0},
		},
		{
			name:     "Mixed invalid",
			input:    []float64{math.NaN(), 2.0, math.Inf(-1), 4.0},
			expected: []float64{0.0, 0.5, 0.0, 1.0},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := normalizeValues(tc.input)
			assert.Equal(t, len(tc.expected), len(result))
			for i := range result {
				assert.False(t, math.IsNaN(result[i]), "Result[%d] should not be NaN", i)
				assert.False(t, math.IsInf(result[i], 0), "Result[%d] should not be Inf", i)
				assert.InDelta(t, tc.expected[i], result[i], 0.001, "Result[%d] mismatch", i)
			}
		})
	}
}

func TestNormalizeValuesLog_HandlesNaNAndInf(t *testing.T) {
	// Test that normalizeValuesLog handles NaN and Inf inputs correctly
	testCases := []struct {
		name  string
		input []float64
	}{
		{
			name:  "Normal values",
			input: []float64{1.0, 10.0, 100.0},
		},
		{
			name:  "With NaN",
			input: []float64{1.0, math.NaN(), 100.0},
		},
		{
			name:  "With Inf",
			input: []float64{1.0, math.Inf(1), 100.0},
		},
		{
			name:  "All NaN",
			input: []float64{math.NaN(), math.NaN(), math.NaN()},
		},
		{
			name:  "Mixed invalid",
			input: []float64{math.NaN(), 10.0, math.Inf(-1), 100.0},
		},
		{
			name:  "NaN at start",
			input: []float64{math.NaN(), 1.0, 10.0},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := normalizeValuesLog(tc.input)
			assert.Equal(t, len(tc.input), len(result))
			for i := range result {
				assert.False(t, math.IsNaN(result[i]), "Result[%d] should not be NaN for input %v", i, tc.input)
				assert.False(t, math.IsInf(result[i], 0), "Result[%d] should not be Inf for input %v", i, tc.input)
				assert.GreaterOrEqual(t, result[i], 0.0, "Result[%d] should be >= 0", i)
				assert.LessOrEqual(t, result[i], 1.0, "Result[%d] should be <= 1", i)
			}
		})
	}
}

func TestUpstreamsRegistry_EMANaNInjection(t *testing.T) {
	// This test directly injects NaN values into the previous scores map
	// to verify that the EMA smoothing guards correctly handle them.
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_getBalance"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	// Get upstreams
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")
	u1 := upsList[0]
	u2 := upsList[1]
	u3 := upsList[2]

	// Simulate some requests to establish initial scores
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.050)
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.060)
	simulateRequestsWithLatency(metricsTracker, u3, method, 10, 0.070)

	// Run initial refresh to populate scores
	err := registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)

	// Inject NaN into the scores map to simulate corrupted previous scores
	// This tests the guard at the EMA smoothing level
	for upsID := range registry.upstreamScores {
		if registry.upstreamScores[upsID][networkID] == nil {
			registry.upstreamScores[upsID][networkID] = make(map[string]float64)
		}
		registry.upstreamScores[upsID][networkID][method] = math.NaN()
	}

	// Add more requests so calculateScore produces valid instant scores
	simulateRequestsWithLatency(metricsTracker, u1, method, 5, 0.040)
	simulateRequestsWithLatency(metricsTracker, u2, method, 5, 0.050)
	simulateRequestsWithLatency(metricsTracker, u3, method, 5, 0.060)

	// Run refresh - NaN guards should reset prev to 0
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)

	// Verify all scores are now valid (not NaN/Inf)
	for upsID, networkScores := range registry.upstreamScores {
		for netID, methodScores := range networkScores {
			for meth, score := range methodScores {
				assert.False(t, math.IsNaN(score),
					"Score for upstream %s, network %s, method %s should not be NaN after NaN injection recovery",
					upsID, netID, meth)
				assert.False(t, math.IsInf(score, 0),
					"Score for upstream %s, network %s, method %s should not be Inf after NaN injection recovery",
					upsID, netID, meth)
				assert.GreaterOrEqual(t, score, 0.0,
					"Score for upstream %s, network %s, method %s should be non-negative",
					upsID, netID, meth)
			}
		}
	}

	// Verify sorting still works correctly
	ordered, err := registry.GetSortedUpstreams(ctx, networkID, method)
	assert.NoError(t, err)
	assert.Len(t, ordered, 3, "Should have 3 upstreams in sorted order after NaN recovery")
}

func TestUpstreamsRegistry_EMAInfInjection(t *testing.T) {
	// This test directly injects Inf values into the previous scores map
	// to verify that the EMA smoothing guards correctly handle them.
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	projectID := "test-project"
	networkID := "evm:123"
	method := "eth_getBalance"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.Logger
	registry, metricsTracker := createTestRegistry(ctx, projectID, &logger, 5*time.Second)

	// Get upstreams
	l, _ := registry.GetSortedUpstreams(ctx, networkID, method)
	upsList := getUpsByID(l, "rpc1", "rpc2", "rpc3")
	u1 := upsList[0]
	u2 := upsList[1]
	u3 := upsList[2]

	// Simulate some requests to establish initial scores
	simulateRequestsWithLatency(metricsTracker, u1, method, 10, 0.050)
	simulateRequestsWithLatency(metricsTracker, u2, method, 10, 0.060)
	simulateRequestsWithLatency(metricsTracker, u3, method, 10, 0.070)

	// Run initial refresh to populate scores
	err := registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)

	// Inject positive and negative Inf into the scores map
	i := 0
	for upsID := range registry.upstreamScores {
		if registry.upstreamScores[upsID][networkID] == nil {
			registry.upstreamScores[upsID][networkID] = make(map[string]float64)
		}
		if i%2 == 0 {
			registry.upstreamScores[upsID][networkID][method] = math.Inf(1) // +Inf
		} else {
			registry.upstreamScores[upsID][networkID][method] = math.Inf(-1) // -Inf
		}
		i++
	}

	// Add more requests
	simulateRequestsWithLatency(metricsTracker, u1, method, 5, 0.040)
	simulateRequestsWithLatency(metricsTracker, u2, method, 5, 0.050)
	simulateRequestsWithLatency(metricsTracker, u3, method, 5, 0.060)

	// Run refresh - Inf guards should reset prev to 0
	err = registry.RefreshUpstreamNetworkMethodScores()
	assert.NoError(t, err)

	// Verify all scores are now valid (not NaN/Inf)
	for upsID, networkScores := range registry.upstreamScores {
		for netID, methodScores := range networkScores {
			for meth, score := range methodScores {
				assert.False(t, math.IsNaN(score),
					"Score for upstream %s, network %s, method %s should not be NaN after Inf injection recovery",
					upsID, netID, meth)
				assert.False(t, math.IsInf(score, 0),
					"Score for upstream %s, network %s, method %s should not be Inf after Inf injection recovery",
					upsID, netID, meth)
			}
		}
	}
}
