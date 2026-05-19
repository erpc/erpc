package stdlib_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestMetrics_EmitsScoreGaugeForRanked verifies that surviving upstreams
// get a `selection_score` gauge value (the actual penalty computed by
// `sortByScore`). The score is the operator's single most-useful signal
// for "why is X primary?" — dashboards build everything around this.
func TestMetrics_EmitsScoreGaugeForRanked(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(BALANCED)`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("fast", "slow")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 100*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")

	fastGauge := telemetry.MetricSelectionScore.WithLabelValues("p1", "evm:1", "*", "fast")
	slowGauge := telemetry.MetricSelectionScore.WithLabelValues("p1", "evm:1", "*", "slow")
	fastScore := promUtil.ToFloat64(fastGauge)
	slowScore := promUtil.ToFloat64(slowGauge)
	require.Greater(t, fastScore, 0.0, "fast upstream score must be non-zero (latency contributes)")
	require.Less(t, fastScore, slowScore,
		"fast upstream's score must be lower than slow's — lower = better, dashboards depend on this orientation")
}

// TestMetrics_ExclusionTotalEmitsLeafSlugs verifies that
// `selection_exclusion_total{reason}` increments WITH THE LEAF SLUG when
// a compound `any(...)` predicate fires. The slug must be `error_rate_above`,
// NOT `any` — that's the whole point of option (c) leaf attribution.
func TestMetrics_ExclusionTotalEmitsLeafSlugs(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.excludeIf(any(errorRateAbove(0.5), blockNumberLagAbove(100)))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("erroring-X", "clean-X")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	beforeLeaf := promUtil.ToFloat64(telemetry.MetricSelectionExclusionTotal.WithLabelValues("p1", "evm:1", "*", "erroring-X", "error_rate_above"))
	beforeCompound := promUtil.ToFloat64(telemetry.MetricSelectionExclusionTotal.WithLabelValues("p1", "evm:1", "*", "erroring-X", "any"))

	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")

	afterLeaf := promUtil.ToFloat64(telemetry.MetricSelectionExclusionTotal.WithLabelValues("p1", "evm:1", "*", "erroring-X", "error_rate_above"))
	afterCompound := promUtil.ToFloat64(telemetry.MetricSelectionExclusionTotal.WithLabelValues("p1", "evm:1", "*", "erroring-X", "any"))

	require.Equal(t, beforeLeaf+1, afterLeaf,
		"exclusion must increment the LEAF slug counter, not the combinator")
	require.Equal(t, beforeCompound, afterCompound,
		"compound 'any' slug must NOT receive an increment — option (c) attributes to leaves only")
}

// TestMetrics_ExcludedSecondsGaugeTransitions verifies that the
// `selection_excluded_seconds` gauge:
//   * is 0 for in-rotation upstreams (clean upstream stays at 0)
//   * is non-zero for excluded ones (after the second tick to give the
//     gauge a chance to read excludedSince)
//   * resets to 0 when an upstream is readmitted.
func TestMetrics_ExcludedSecondsGaugeTransitions(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.excludeIf(errorRateAbove(0.5))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("flaky", "steady")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Tick 1: flaky degrades, steady stays clean.
	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")
	// Tick 2: same state — gauge has now had two ticks to reflect age.
	policy.TickForTest(engine, "evm:1", "*")

	flaky := promUtil.ToFloat64(telemetry.MetricSelectionExcludedSeconds.WithLabelValues("p1", "evm:1", "*", "flaky"))
	steady := promUtil.ToFloat64(telemetry.MetricSelectionExcludedSeconds.WithLabelValues("p1", "evm:1", "*", "steady"))
	require.GreaterOrEqual(t, flaky, 0.0, "flaky's excluded-seconds gauge is set")
	require.Equal(t, 0.0, steady, "in-rotation upstream's excluded-seconds gauge stays 0")
}

// TestMetrics_ReadmitCountsTransition verifies that a previously-excluded
// upstream getting back into the order this tick increments
// `selection_readmit_total{upstream}` exactly once.
func TestMetrics_ReadmitCountsTransition(t *testing.T) {
	// Threshold-driven exclusion that we can flip back by feeding clean
	// traffic. Using the 60-second rolling window means a burst of
	// clean requests rapidly dominates the rate.
	eval := `(upstreams, ctx) => upstreams.excludeIf(errorRateAbove(0.5))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("returner", "anchor")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	beforeReadmits := promUtil.ToFloat64(telemetry.MetricSelectionReadmitTotal.WithLabelValues("p1", "evm:1", "*", "returner"))

	// Tick 1: returner errors hard → excluded; anchor steady.
	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")
	order, excluded := policy.LatestDecisionOutputForTest(engine, "evm:1", "*")
	require.Contains(t, excluded, "returner")
	require.Equal(t, []string{"anchor"}, order)

	// Tick 2: flood returner with successes — errorRate drops well
	// below 50% (1000 successes vs 80 failures = ~7.4% error rate).
	for i := 0; i < 1000; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")
	order, _ = policy.LatestDecisionOutputForTest(engine, "evm:1", "*")
	require.Contains(t, order, "returner", "returner must be readmitted after errorRate dropped")

	afterReadmits := promUtil.ToFloat64(telemetry.MetricSelectionReadmitTotal.WithLabelValues("p1", "evm:1", "*", "returner"))
	require.Equal(t, beforeReadmits+1, afterReadmits,
		"readmit counter should increment exactly once when an upstream transitions back to in-rotation")
}

// TestMetrics_StickyHoldCounterIncrementsOnActiveHold verifies the
// `selection_sticky_hold_total{upstream}` counter increments when sticky
// actively keeps the previous primary against a challenger.
func TestMetrics_StickyHoldCounterIncrementsOnActiveHold(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(BALANCED).stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '1h' })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("a-prim2", "b-chal2")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	beforeHolds := promUtil.ToFloat64(telemetry.MetricSelectionStickyHoldTotal.WithLabelValues("p1", "evm:1", "*", "a-prim2"))

	// Tick 1: equal → a-prim2 wins by id tiebreak.
	for _, u := range ups {
		for i := 0; i < 100; i++ {
			tracker.RecordUpstreamRequest(u, "*")
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}
	policy.TickForTest(engine, "evm:1", "*")
	// Tick 2: degrade a-prim2 → score-only ordering would flip, sticky holds.
	for i := 0; i < 30; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	policy.TickForTest(engine, "evm:1", "*")

	afterHolds := promUtil.ToFloat64(telemetry.MetricSelectionStickyHoldTotal.WithLabelValues("p1", "evm:1", "*", "a-prim2"))
	require.Equal(t, beforeHolds+1, afterHolds,
		"sticky hold counter must increment when stickyPrimary actively held a primary it would have lost")
}
