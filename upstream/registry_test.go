package upstream

import (
	"testing"
	"time"

	"github.com/flair-sdk/erpc/config"
)

func TestUpstreamsRegistry_ScoreCalculations(t *testing.T) {
	registry := &UpstreamsRegistry{}

	upA := &PreparedUpstream{
		Id:         "upstreamA",
		ProjectId:  "test_project",
		NetworkIds: []string{"123"},
		Metrics: &UpstreamMetrics{
			P90Latency:     0.100, // Seconds
			ErrorsTotal:    90,
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
			P90Latency:     0.500, // Seconds
			ErrorsTotal:    1,
			RequestsTotal:  100,
			ThrottledTotal: 0,
			BlocksLag:      0,
			LastCollect:    time.Now(),
		},
	}
	gp := &config.HealthCheckGroupConfig{
		Id:                  "test_group",
		MaxErrorRatePercent: 10,
		MaxP90Latency:       "1s",
		MaxBlocksLag:        10,
	}
	registry.refreshUpstreamGroupScores(gp, map[string]*PreparedUpstream{
		"upstreamA": upA,
		"upstreamB": upB,
	})

	t.Run("NonZeroScores", func(t *testing.T) {
		if upA.Score == 0 || upB.Score == 0 {
			t.Errorf("Expected non-zero scores for both upstream A & B, got %v and %v", upA.Score, upB.Score)
		}
	})

	// expecting upstream B to have a higher score than upstream A, because of lower error rate
	t.Run("UpstreamBHasHigherScoreWithLowerErrorRate", func(t *testing.T) {
		if upA.Score >= upB.Score {
			t.Errorf("Expected upstreamB to have a higher score than upstreamA (lower error rate), got %v and %v", upA.Score, upB.Score)
		}
	})
}
