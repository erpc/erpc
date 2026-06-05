package stdlib_test

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestTracker_BlockHeadLag_SurvivesContinuousRotationEviction reproduces the
// continuous-operation conditions a one-shot test skips: a short metrics
// window (frequent Rotate()), aggressive idle eviction, and repeated
// record+poll cycles over time. The network-scope policy reads
// {ups, "*", All}.BlockHeadLag; this asserts a frozen upstream's lag stays
// visible there throughout, the way it must for blockNumberLagAbove to fire.
//
// If prod's missing lag-exclusion is a rotation/eviction-vs-lag-rollup
// interaction, this fails (lag at {*, All} collapses to 0).
func TestTracker_BlockHeadLag_SurvivesContinuousRotationEviction(t *testing.T) {
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", 100*time.Millisecond) // rotate ~every 10ms
	tracker.EnableFinalityTracking()
	tracker.SetIdleEvictionAfter(50 * time.Millisecond) // aggressive eviction

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.Bootstrap(ctx) // start rotation + idle-sweep loops

	ups := mkUps("rpc1", "rpc2")
	fin := common.DataFinalityStateUnfinalized

	// Churn for ~1.5s across many rotation + eviction cycles. rpc2 advances at
	// the tip; rpc1 stays frozen ~900+ blocks behind.
	deadline := time.Now().Add(1500 * time.Millisecond)
	tip := int64(1000)
	checks := 0
	for time.Now().Before(deadline) {
		for _, u := range ups {
			tracker.RecordUpstreamRequest(u, "eth_getLogs", fin)
			tracker.RecordUpstreamDuration(u, "eth_getLogs", 5*time.Millisecond, true, "none", fin, "n/a")
		}
		tracker.SetLatestBlockNumber(ups[1], tip, 0) // rpc2 at tip
		tracker.SetLatestBlockNumber(ups[0], 100, 0) // rpc1 frozen
		tip += 5

		// Mid-flight assertion: the {*, All} rollup must already reflect the lag.
		if m := tracker.GetUpstreamMethodMetrics(ups[0], "*", common.DataFinalityStateAll); m != nil {
			lag := m.BlockHeadLag.Load()
			require.Greater(t, lag, int64(16),
				"rpc1 lag must stay visible at {*, All} (got %d) — the policy reads this", lag)
			checks++
		}
		time.Sleep(12 * time.Millisecond)
	}

	require.Positive(t, checks, "expected at least one mid-flight check")

	// Final: still visible after the last rotation/eviction cycle.
	final := tracker.GetUpstreamMethodMetrics(ups[0], "*", common.DataFinalityStateAll)
	require.NotNil(t, final)
	require.Greater(t, final.BlockHeadLag.Load(), int64(16),
		"rpc1 lag must remain at {*, All} after continuous churn")
}
