package upstream

import (
	"sort"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/common"
)

// scoredUpstream represents an upstream with a score
type scoredUpstream struct {
	Id    string
	Score int
}

// assertScoreOrder checks that the scores are in non-decreasing order
func assertScoreOrder(t *testing.T, scoredUpstreams []scoredUpstream) {
	for i := 1; i < len(scoredUpstreams); i++ {
		if scoredUpstreams[i-1].Score < scoredUpstreams[i].Score {
			t.Errorf("Expected upstream %s to have a higher or equal score than upstream %s, got %v and %v",
				scoredUpstreams[i-1].Id, scoredUpstreams[i].Id, scoredUpstreams[i-1].Score, scoredUpstreams[i].Score)
		}
	}
}

// genMetrics generates UpstreamMetrics with provided parameters
func genMetrics(latency, errorsTotal, requestsTotal, throttledTotal, blocksLag float64) *UpstreamMetrics {
	return &UpstreamMetrics{
		P90LatencySecs: latency,
		ErrorsTotal:    errorsTotal,
		RequestsTotal:  requestsTotal,
		ThrottledTotal: throttledTotal,
		BlocksLag:      blocksLag,
		LastCollect:    time.Date(2023, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Test case struct
type testCase struct {
	id             string
	latency        float64
	errorsTotal    float64
	requestsTotal  float64
	throttledTotal float64
	blocksLag      float64
}

// Test group struct
type testGroup struct {
	groupId   string
	testCases []testCase
}

func TestUpstreamsRegistry_ScoreOrdering(t *testing.T) {
	// Define test groups with test cases
	testGroups := []testGroup{
		// Better (lower) latency
		{
			groupId: "BetterLateny1",
			testCases: []testCase{
				{"upstreamA", 100.0, 100000, 100000, 100, 100},
				{"upstreamB", 10.0, 10000, 10000, 50, 50},
				{"upstreamC", 1.0, 1000, 1000, 10, 10},
				{"upstreamD", 0.1, 100, 100, 1, 1},
				{"upstreamE", 0.01, 10, 10, 0, 0},
			},
		},
		// Give a chance to an upstream without any request
		{
			groupId: "WithoutRequest1",
			testCases: []testCase{
				{"upstreamA", 0.4, 0, 10000, 0, 0},
				{"upstreamB", 0.0, 0, 0, 0, 0},
			},
		},
		// Higher error rates
		{
			groupId: "HigherErrorRates1",
			testCases: []testCase{
				{"upstreamA", 10.0, 10000, 9000, 500, 50},
				{"upstreamB", 10.0, 10000, 8000, 400, 50},
				{"upstreamC", 10.0, 10000, 1000, 100, 50},
				{"upstreamD", 10.0, 10000, 0, 0, 50},
			},
		},
		// Higher throttled rates
		{
			groupId: "HigherThrottledRates1",
			testCases: []testCase{
				{"upstreamA", 10.0, 10000, 1000, 9000, 50},
				{"upstreamB", 10.0, 10000, 1000, 4000, 50},
				{"upstreamC", 10.0, 10000, 1000, 1000, 50},
				{"upstreamD", 10.0, 10000, 1000, 0, 50},
			},
		},
		// Higher block lags
		{
			groupId: "HigherBlockLags1",
			testCases: []testCase{
				{"upstreamA", 10.0, 10000, 1000, 100, 900},
				{"upstreamB", 10.0, 10000, 1000, 100, 400},
				{"upstreamC", 10.0, 10000, 1000, 100, 100},
				{"upstreamD", 10.0, 10000, 1000, 100, 0},
			},
		},
		// Mixed factors
		{
			groupId: "MixedFactors1",
			testCases: []testCase{
				{"upstreamA", 100.0, 100000, 10000, 10000, 100},
				{"upstreamB", 50.0, 50000, 5000, 5000, 50},
				{"upstreamC", 10.0, 10000, 1000, 1000, 10},
				{"upstreamD", 1.0, 1000, 100, 100, 1},
				{"upstreamE", 0.1, 100, 10, 10, 0},
			},
		},
		// Low error and throttled rates
		{
			groupId: "LowErrorAndThrottledRates",
			testCases: []testCase{
				{"upstreamA", 50.0, 50000, 500, 500, 50},
				{"upstreamB", 50.0, 50000, 100, 100, 50},
				{"upstreamC", 50.0, 50000, 50, 50, 50},
				{"upstreamD", 50.0, 50000, 10, 10, 50},
				{"upstreamE", 50.0, 50000, 0, 0, 50},
			},
		},
		// High requests but low latency
		{
			groupId: "HighRequestsLowLatency",
			testCases: []testCase{
				{"upstreamA", 0.1, 100000, 1000, 1000, 10},
				{"upstreamB", 0.1, 50000, 500, 500, 10},
				{"upstreamC", 0.1, 10000, 100, 100, 10},
				{"upstreamD", 0.1, 1000, 10, 10, 10},
				{"upstreamE", 0.1, 100, 1, 1, 10},
			},
		},
		// Balanced metrics
		{
			groupId: "BalancedMetrics",
			testCases: []testCase{
				{"upstreamA", 10.0, 10000, 500, 500, 50},
				{"upstreamB", 5.0, 5000, 250, 250, 25},
				{"upstreamC", 2.5, 2500, 125, 125, 12},
				{"upstreamD", 1.25, 1250, 62, 62, 6.},
				{"upstreamE", 0.625, 625, 31, 31, 3},
			},
		},
		// Very high errors and throttled rates
		{
			groupId: "VeryHighErrorsAndThrottledRates",
			testCases: []testCase{
				{"upstreamA", 100.0, 10000, 9000, 9000, 100},
				{"upstreamB", 100.0, 10000, 8000, 8000, 100},
				{"upstreamC", 100.0, 10000, 7000, 7000, 100},
				{"upstreamD", 100.0, 10000, 6000, 6000, 100},
				{"upstreamE", 100.0, 10000, 5000, 5000, 100},
			},
		},
		// High latency but zero errors and throttled rates
		{
			groupId: "HighLatencyZeroErrorsThrottled",
			testCases: []testCase{
				{"upstreamA", 100.0, 10000, 0, 0, 100},
				{"upstreamB", 90.0, 9000, 0, 0, 90},
				{"upstreamC", 80.0, 8000, 0, 0, 80},
				{"upstreamD", 70.0, 7000, 0, 0, 70},
				{"upstreamE", 60.0, 6000, 0, 0, 60},
			},
		},
		// Low block lags and errors
		{
			groupId: "LowBlockLagsAndErrors",
			testCases: []testCase{
				{"upstreamA", 100.0, 10000, 0, 100, 0},
				{"upstreamB", 90.0, 9000, 0, 90, 0},
				{"upstreamC", 80.0, 8000, 0, 80, 0},
				{"upstreamD", 70.0, 7000, 0, 70, 0},
				{"upstreamE", 60.0, 6000, 0, 60, 0},
			},
		},
		// High throttled rates but low latency and errors
		{
			groupId: "HighThrottledLowLatencyErrors",
			testCases: []testCase{
				{"upstreamA", 1.0, 1000, 1, 900, 1},
				{"upstreamB", 1.0, 1000, 1, 800, 1},
				{"upstreamC", 1.0, 1000, 1, 700, 1},
				{"upstreamD", 1.0, 1000, 1, 600, 1},
				{"upstreamE", 1.0, 1000, 1, 500, 1},
			},
		},
		// Zero errors, throttled rates, and block lags
		{
			groupId: "ZeroErrorsThrottledBlockLags",
			testCases: []testCase{
				{"upstreamA", 100.0, 10000, 0, 0, 0},
				{"upstreamB", 90.0, 9000, 0, 0, 0},
				{"upstreamC", 80.0, 8000, 0, 0, 0},
				{"upstreamD", 70.0, 7000, 0, 0, 0},
				{"upstreamE", 60.0, 6000, 0, 0, 0},
			},
		},
	}

	for _, tg := range testGroups {
		t.Run(tg.groupId, func(t *testing.T) {
			upstreams := make(map[string]*Upstream)
			for _, tc := range tg.testCases {
				upstreams[tc.id] = &Upstream{
					config: &common.UpstreamConfig{
						Id: tc.id,
					},
					ProjectId: "test_project",
					Metrics:   genMetrics(tc.latency, tc.errorsTotal, tc.requestsTotal, tc.throttledTotal, tc.blocksLag),
				}
			}

			// Create a new UpstreamsRegistry
			registry := &UpstreamsRegistry{}

			// HealthCheckGroupConfig for the test
			gp := &common.HealthCheckGroupConfig{
				Id:            "test_group",
				CheckInterval: "1s",
			}

			// Refresh scores
			registry.refreshUpstreamGroupScores(gp, upstreams)

			var scoredUpstreams []scoredUpstream
			for id, upstream := range upstreams {
				t.Logf("Metrics for %s: %+v", id, upstream.Metrics)
				t.Logf("Score for %s: %v", id, upstream.Score)
				scoredUpstreams = append(scoredUpstreams, scoredUpstream{Id: id, Score: upstream.Score})
			}

			// Sort scored upstreams based on their scores
			sort.Slice(scoredUpstreams, func(i, j int) bool {
				return scoredUpstreams[i].Score > scoredUpstreams[j].Score
			})

			// Assert the order
			assertScoreOrder(t, scoredUpstreams)
		})
	}
}
