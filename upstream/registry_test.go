package upstream

import (
	"testing"
	"time"

	"github.com/flair-sdk/erpc/config"
)

func TestUpstreamsRegistry_ScoreCalculations(t *testing.T) {
	type testCase struct {
		name       string
		upstreams  map[string]*PreparedUpstream
		assertions func(t *testing.T, upstreams map[string]*PreparedUpstream)
	}

	testCases := []testCase{
		{
			name: "None of upstreams have any requests",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: nil},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: nil},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score != 0 || upstreams["upstreamB"].Score != 0 {
					t.Errorf("Expected both scores to be zero")
				}
			},
		},
		{
			name: "Upstream A has some successful requests with some latency but B has no data yet",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.200, ErrorsTotal: 0, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: nil},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score")
				}
				if upstreams["upstreamB"].Score != 0 {
					t.Errorf("Expected upstream B to have a zero score")
				}
				if upstreams["upstreamA"].Score <= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a higher score than B")
				}
			},
		},
		{
			name: "Upstream A has some failed requests and B has no data yet",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.200, ErrorsTotal: 30, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: nil},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score")
				}
				if upstreams["upstreamB"].Score != 0 {
					t.Errorf("Expected upstream B to have a zero score")
				}
				if upstreams["upstreamA"].Score <= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a higher score than B")
				}
			},
		},
		{
			name: "upstream A has higher errorRate and lower latency and upstream B has lower errorRate higher latency, both same amount of requests",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.100, ErrorsTotal: 30, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.300, ErrorsTotal: 10, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score")
				}
				if upstreams["upstreamB"].Score == 0 {
					t.Errorf("Expected upstream B to have a non-zero score")
				}
				if upstreams["upstreamA"].Score >= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a lower score than B")
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
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
			registry.refreshUpstreamGroupScores(gp, tc.upstreams)

			// Run assertions
			tc.assertions(t, tc.upstreams)
		})
	}
}
