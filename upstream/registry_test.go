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
					t.Errorf("Expected both scores to be zero, got %v and %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Both upstreams have identical metrics",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.200, ErrorsTotal: 10, RequestsTotal: 100, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.200, ErrorsTotal: 10, RequestsTotal: 100, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score != upstreams["upstreamB"].Score {
					t.Errorf("Expected scores to be identical, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
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
					t.Errorf("Expected upstream A to have a non-zero score, got %v", upstreams["upstreamA"].Score)
				}
				if upstreams["upstreamB"].Score != 0 {
					t.Errorf("Expected upstream B to have a zero score, got %v", upstreams["upstreamB"].Score)
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
					t.Errorf("Expected upstream A to have a non-zero score, got %v", upstreams["upstreamA"].Score)
				}
				if upstreams["upstreamB"].Score != 0 {
					t.Errorf("Expected upstream B to have a zero score, got %v", upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Upstream A has higher errorRate and lower latency and upstream B has lower errorRate higher latency, both same amount of requests",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.100, ErrorsTotal: 30, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.300, ErrorsTotal: 10, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score, got %v", upstreams["upstreamA"].Score)
				}
				if upstreams["upstreamB"].Score == 0 {
					t.Errorf("Expected upstream B to have a non-zero score, got %v", upstreams["upstreamB"].Score)
				}
				if upstreams["upstreamA"].Score >= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a lower score than B, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Upstream A has higher errorRate and lower latency and upstream B has lower errorRate higher latency, A has significantly higher requests recorded",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.100, ErrorsTotal: 30, RequestsTotal: 500, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.300, ErrorsTotal: 10, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score, got %v", upstreams["upstreamA"].Score)
				}
				if upstreams["upstreamB"].Score == 0 {
					t.Errorf("Expected upstream B to have a non-zero score, got %v", upstreams["upstreamB"].Score)
				}
				if upstreams["upstreamA"].Score <= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a higher score than B, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Upstream A has higher errorRate and lower latency and upstream B has lower errorRate higher latency, B has significantly higher requests recorded",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.100, ErrorsTotal: 30, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.300, ErrorsTotal: 10, RequestsTotal: 500, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score, got %v", upstreams["upstreamA"].Score)
				}
				if upstreams["upstreamB"].Score == 0 {
					t.Errorf("Expected upstream B to have a non-zero score, got %v", upstreams["upstreamB"].Score)
				}
				if upstreams["upstreamA"].Score >= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a lower score than B, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Upstream A has failed requests with higher latency and lower total requests, upstream B has failed requests with lower latency and higher total requests",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.300, ErrorsTotal: 30, RequestsTotal: 50, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.100, ErrorsTotal: 30, RequestsTotal: 500, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score, got %v", upstreams["upstreamA"].Score)
				}
				if upstreams["upstreamB"].Score == 0 {
					t.Errorf("Expected upstream B to have a non-zero score, got %v", upstreams["upstreamB"].Score)
				}
				if upstreams["upstreamA"].Score >= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a lower score than B, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Upstream A has lower latency but higher throttled rate, upstream B has higher latency but lower throttled rate",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.100, ErrorsTotal: 0, RequestsTotal: 30, ThrottledTotal: 20, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.300, ErrorsTotal: 0, RequestsTotal: 30, ThrottledTotal: 5, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score, got %v", upstreams["upstreamA"].Score)
				}
				if upstreams["upstreamB"].Score == 0 {
					t.Errorf("Expected upstream B to have a non-zero score, got %v", upstreams["upstreamB"].Score)
				}
				if upstreams["upstreamA"].Score >= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a lower score than B, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Upstream A has lowest latency highest throttled rate, B has highest latency lowest throttled, C has lowest latency, lowest throttled, lowest total requests",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.100, ErrorsTotal: 0, RequestsTotal: 30, ThrottledTotal: 20, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.300, ErrorsTotal: 0, RequestsTotal: 30, ThrottledTotal: 5, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamC": {Id: "upstreamC", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.100, ErrorsTotal: 0, RequestsTotal: 15, ThrottledTotal: 5, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score == 0 {
					t.Errorf("Expected upstream A to have a non-zero score, got %v", upstreams["upstreamA"].Score)
				}
				if upstreams["upstreamB"].Score == 0 {
					t.Errorf("Expected upstream B to have a non-zero score, got %v", upstreams["upstreamB"].Score)
				}
				if upstreams["upstreamC"].Score == 0 {
					t.Errorf("Expected upstream C to have a non-zero score, got %v", upstreams["upstreamC"].Score)
				}
				if upstreams["upstreamA"].Score >= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a lower score than B, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
				if upstreams["upstreamC"].Score <= upstreams["upstreamA"].Score || upstreams["upstreamC"].Score <= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream C to have the highest score, got C: %v, A: %v, B: %v", upstreams["upstreamC"].Score, upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Upstream A has extremely high error rate but very low latency",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.050, ErrorsTotal: 90, RequestsTotal: 100, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.200, ErrorsTotal: 10, RequestsTotal: 100, ThrottledTotal: 0, BlocksLag: 0, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score >= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a lower score than B, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
				}
			},
		},
		{
			name: "Upstream A jas significant blocks lag compared to upstream B, both have identical metrics otherwise",
			upstreams: map[string]*PreparedUpstream{
				"upstreamA": {Id: "upstreamA", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.200, ErrorsTotal: 10, RequestsTotal: 100, ThrottledTotal: 0, BlocksLag: 50, LastCollect: time.Now()}},
				"upstreamB": {Id: "upstreamB", ProjectId: "test_project", NetworkIds: []string{"123"}, Metrics: &UpstreamMetrics{P90Latency: 0.200, ErrorsTotal: 10, RequestsTotal: 100, ThrottledTotal: 0, BlocksLag: 5, LastCollect: time.Now()}},
			},
			assertions: func(t *testing.T, upstreams map[string]*PreparedUpstream) {
				if upstreams["upstreamA"].Score >= upstreams["upstreamB"].Score {
					t.Errorf("Expected upstream A to have a lower score than B due to higher blocks lag, got A: %v, B: %v", upstreams["upstreamA"].Score, upstreams["upstreamB"].Score)
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
