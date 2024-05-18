package upstream

import (
	"testing"
	"time"
)

func TestUpstreamsRegistry_UpstreamALowerLatencyHigherErroRateUpstreamBHigherLatencyLowerErrorRate(t *testing.T) {
	// Create a new UpstreamsRegistry
	registry := &UpstreamsRegistry{}

	upA := &PreparedUpstream{
		Id:         "upstreamA",
		ProjectId:  "test_project",
		NetworkIds: []string{"123"},
		Metrics: &UpstreamMetrics{
			P90Latency:     0.100, // Seconds
			ErrorsTotal:    10,
			RequestsTotal:  100,
			ThrottledTotal: 0,
			BlocksLag:      0,
			LastCollect:    time.Now(),
		},
	}
	upB := &PreparedUpstream{
		Id:         "upstreamB",
		ProjectId:  "test_project",
		NetworkIds: []string{"123"},
		Metrics: &UpstreamMetrics{
			P90Latency:     0.200, // Seconds
			ErrorsTotal:    1,
			RequestsTotal:  100,
			ThrottledTotal: 0,
			BlocksLag:      0,
			LastCollect:    time.Now(),
		},
	}

	registry.refreshUpstreamGroupScores("test", map[string]*PreparedUpstream{
		"upstreamA": upA,
		"upstreamB": upB,
	})

	if upA.Score == 0 || upB.Score == 0 {
		t.Errorf("Expected non-zero scores, got %v and %v", upA.Score, upB.Score)
	}

	if upA.Score >= upB.Score {
		t.Errorf("Expected upstreamA to have a higher score than upstreamB, got %v and %v", upA.Score, upB.Score)
	}
}
