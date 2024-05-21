package upstream

import (
	"sort"
	"testing"
	"time"

	"github.com/flair-sdk/erpc/config"
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
func genMetrics(latency float64, errorsTotal, requestsTotal, throttledTotal, blocksLag int) *UpstreamMetrics {
	return &UpstreamMetrics{
		P90Latency:     latency,
		ErrorsTotal:    float64(errorsTotal),
		RequestsTotal:  float64(requestsTotal),
		ThrottledTotal: float64(throttledTotal),
		BlocksLag:      float64(blocksLag),
		LastCollect:    time.Date(2023, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Test case struct
type testCase struct {
	id             string
	latency        float64
	errorsTotal    int
	requestsTotal  int
	throttledTotal int
	blocksLag      int
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
	}

	for _, tg := range testGroups {
		t.Run(tg.groupId, func(t *testing.T) {
			upstreams := make(map[string]*PreparedUpstream)
			for _, tc := range tg.testCases {
				upstreams[tc.id] = &PreparedUpstream{
					Id:         tc.id,
					ProjectId:  "test_project",
					NetworkIds: []string{"123"},
					Metrics:    genMetrics(tc.latency, tc.errorsTotal, tc.requestsTotal, tc.throttledTotal, tc.blocksLag),
				}
			}

			// Create a new UpstreamsRegistry
			registry := &UpstreamsRegistry{}

			// HealthCheckGroupConfig for the test
			gp := &config.HealthCheckGroupConfig{
				Id:                  "test_group",
				MaxErrorRatePercent: 10,
				MaxP90Latency:       "1s",
				MaxBlocksLag:        10,
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
