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

// findExcluded returns the ExcludedUpstream with the given ID, or nil.
// Local helper kept here so the step-attribution tests stay
// self-contained — observability_test.go has its own inline scanner.
func findExcluded(d *policy.Decision, id string) *policy.ExcludedUpstream {
	for i := range d.Output.Excluded {
		if d.Output.Excluded[i].ID == id {
			return &d.Output.Excluded[i]
		}
	}
	return nil
}

// TestStepAttribution_ByTag_DropsAttributeToByTag verifies that
// `byTag('tier:main')` dropping an upstream tags `Step="byTag"`. Most
// production policies front their chain with a tag filter; before this
// fix every such drop landed on `step="eval"` (no annotation), so the
// "Rejection by Chain Step" panel was effectively blind.
func TestStepAttribution_ByTag_DropsAttributeToByTag(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.byTag('tier:main')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{id: "m1", vendor: "v1", tags: []string{"tier:main"}},
		{id: "f1", vendor: "v2", tags: []string{"tier:fallback"}},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", "*", 1)
	require.Len(t, decisions, 1)

	fallback := findExcluded(decisions[0], "f1")
	require.NotNil(t, fallback, "fallback-tier upstream should be excluded by byTag")
	require.Equal(t, "byTag", fallback.Step,
		"byTag is the primitive that dropped the upstream — Step must be the primitive name, not annotation text and not empty")
}

// TestStepAttribution_ExcludeIf_DropsAttributeToExcludeIf verifies
// that `excludeIf(errorRateAbove(0.5))` tags `Step="excludeIf"`. The
// per-leaf REASON ("error_rate_above") still lives in LeafReasons; Step
// is the orthogonal "which primitive" view.
func TestStepAttribution_ExcludeIf_DropsAttributeToExcludeIf(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.excludeIf(errorRateAbove(0.5))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("erroring", "clean")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[0], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	for i := 0; i < 20; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", "*", 1)
	require.Len(t, decisions, 1)

	erroring := findExcluded(decisions[0], "erroring")
	require.NotNil(t, erroring)
	require.Equal(t, "excludeIf", erroring.Step,
		"excludeIf is the chain primitive; Step is the primitive name, not the leaf reason — those live in LeafReasons")
	require.Equal(t, []string{"error_rate_above"}, erroring.LeafReasons,
		"LeafReasons is the orthogonal per-reason attribution (still populated)")
}

// TestStepAttribution_Take_DropsTailToTake verifies that `take(n)`
// chained after a sort drops the tail with Step="take". This is the
// "limit-N" pattern at the bottom of most production chains
// (`...sortByScore(...).take(5)`); attribution lets operators see how
// much the cap is filtering vs. the health primitives.
func TestStepAttribution_Take_DropsTailToTake(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST).take(2)`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("u1", "u2", "u3", "u4")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Identical baseline metrics so sortByScore tie-breaks alphabetically.
	for _, u := range ups {
		for i := 0; i < 100; i++ {
			tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", "*", 1)
	require.Len(t, decisions, 1)
	d := decisions[0]

	// u3 and u4 are the tail and must be attributed to `take`.
	for _, id := range []string{"u3", "u4"} {
		ex := findExcluded(d, id)
		require.NotNil(t, ex, "%s should be excluded after take(2)", id)
		require.Equal(t, "take", ex.Step, "%s was dropped by the take(2) cap, not by sortByScore (sort reorders, doesn't drop)", id)
	}
}

// TestStepAttribution_FirstStepWins verifies that when a chain runs
// multiple filter steps, the FIRST primitive that drops an upstream
// owns the attribution. Once filtered out, an upstream can't be
// dropped again — so first-wins is the only sensible semantic.
//
// Concretely: `byTag('tier:main')` runs FIRST and drops `f1`. The
// subsequent `excludeIf(errorRateAbove(0.5))` never sees `f1`. The
// metric must NOT incorrectly attribute `f1` to `excludeIf`.
func TestStepAttribution_FirstStepWins(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.byTag('tier:main').excludeIf(errorRateAbove(0.5))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{id: "m1", vendor: "v1", tags: []string{"tier:main"}},
		{id: "f1", vendor: "v2", tags: []string{"tier:fallback"}}, // dropped by byTag
		{id: "m_bad", vendor: "v3", tags: []string{"tier:main"}},  // erroring, dropped by excludeIf
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[2], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	for i := 0; i < 20; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[2], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", "*", 1)
	require.Len(t, decisions, 1)
	d := decisions[0]

	f1 := findExcluded(d, "f1")
	require.NotNil(t, f1)
	require.Equal(t, "byTag", f1.Step,
		"f1 was tier:fallback; byTag dropped it first — subsequent excludeIf never saw it, so attribution must be byTag")

	mBad := findExcluded(d, "m_bad")
	require.NotNil(t, mBad)
	require.Equal(t, "excludeIf", mBad.Step,
		"m_bad passed byTag (tier:main) but failed excludeIf — attribution must be excludeIf")
}

// TestStepAttribution_ReorderStepsDontAttribute verifies that pure
// reorder steps (`sortByScore`, `preferTag`, `rotateBy`, ...) don't
// pollute Step attribution. They don't drop anything; the survivors
// set equals the input set, so the helper short-circuits.
func TestStepAttribution_ReorderStepsDontAttribute(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST)`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("a", "b", "c")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	for _, u := range ups {
		for i := 0; i < 50; i++ {
			tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", "*", 1)
	require.Len(t, decisions, 1)
	require.Empty(t, decisions[0].Output.Excluded,
		"sortByScore alone reorders without dropping — no exclusions, no Step attribution")
}

// TestStepAttribution_RawFilterFallsBackToEval verifies that an eval
// using raw `Array.filter` (NOT a stdlib wrapper) leaves Step empty.
// The metric emitter then falls back to `step="eval"` — same as the
// pre-fix behavior, but only for chains that genuinely bypassed the
// stdlib. Operators get a clear signal that this network's policy is
// off the instrumented chain.
func TestStepAttribution_RawFilterFallsBackToEval(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.filter(u => u.id !== 'doomed')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("survivor", "doomed")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", "*", 1)
	require.Len(t, decisions, 1)

	doomed := findExcluded(decisions[0], "doomed")
	require.NotNil(t, doomed)
	require.Empty(t, doomed.Step,
		"raw Array.filter bypasses the define() wrapper; Step stays empty so the metric emitter falls back to step=\"eval\"")
}

// TestStepAttribution_RejectionMetricEmitsPrimitive verifies the
// end-to-end metric emission: with the step trail wired through
// EvalResult → Decision → emitMetrics, `selection_rejection_total`
// must increment under the PRIMITIVE label (`byTag`/`excludeIf`/...),
// NOT the old `"eval"` catch-all. Catches accidental regressions in
// either the JS wiring (`__policyStepReasons` not populated) or the
// Go-side `enrichExcluded` plumbing.
func TestStepAttribution_RejectionMetricEmitsPrimitive(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.byTag('tier:main').excludeIf(errorRateAbove(0.5))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{id: "m-keep", vendor: "v1", tags: []string{"tier:main"}},
		{id: "f-drop", vendor: "v2", tags: []string{"tier:fallback"}}, // byTag
		{id: "m-bad", vendor: "v3", tags: []string{"tier:main"}},      // excludeIf
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:rej", "rej-alias", func() []common.Upstream { return ups }, cfg))

	// m-bad → 80% error rate; m-keep clean.
	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[2], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	for i := 0; i < 20; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[2], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	// Capture baselines so the test is robust to other tests sharing the
	// global metric registry (Prometheus counters are process-global).
	beforeByTag := promUtil.ToFloat64(telemetry.MetricSelectionRejectionTotal.WithLabelValues("p1", "rej-alias", "*", "f-drop", "byTag"))
	beforeExcludeIf := promUtil.ToFloat64(telemetry.MetricSelectionRejectionTotal.WithLabelValues("p1", "rej-alias", "*", "m-bad", "excludeIf"))
	beforeEvalFallback := promUtil.ToFloat64(telemetry.MetricSelectionRejectionTotal.WithLabelValues("p1", "rej-alias", "*", "f-drop", "eval"))

	policy.TickForTest(engine, "evm:rej", "*")

	afterByTag := promUtil.ToFloat64(telemetry.MetricSelectionRejectionTotal.WithLabelValues("p1", "rej-alias", "*", "f-drop", "byTag"))
	afterExcludeIf := promUtil.ToFloat64(telemetry.MetricSelectionRejectionTotal.WithLabelValues("p1", "rej-alias", "*", "m-bad", "excludeIf"))
	afterEvalFallback := promUtil.ToFloat64(telemetry.MetricSelectionRejectionTotal.WithLabelValues("p1", "rej-alias", "*", "f-drop", "eval"))

	require.Equal(t, beforeByTag+1, afterByTag,
		"f-drop must register under step=\"byTag\" (the primitive that dropped it), not the legacy step=\"eval\" fallback")
	require.Equal(t, beforeExcludeIf+1, afterExcludeIf,
		"m-bad must register under step=\"excludeIf\"")
	require.Equal(t, beforeEvalFallback, afterEvalFallback,
		"the legacy step=\"eval\" series must NOT increment when the chain runs through stdlib wrappers")
}
