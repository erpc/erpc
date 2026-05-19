package stdlib_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// TestStdlib_LeafReasons_LeafPredicate validates the baseline case:
// a plain factory predicate (`errorRateAbove(0.5)`) excluding an
// upstream tags the exclusion with that factory's stable slug —
// `error_rate_above`. The display Reason still carries the threshold
// for humans; the LeafReasons slugs are what Prometheus sees.
func TestStdlib_LeafReasons_LeafPredicate(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.excludeIf(errorRateAbove(0.5))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()
	// Step-log enables display-Reason enrichment from annotations. The
	// LeafReasons slug attribution is independent of this flag — it
	// always flows because Prometheus needs it — but display Reason
	// only carries the threshold when annotation capture is on.
	engine.SetStepLogEnabled(true)

	ups := mkUps("erroring", "clean")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	for i := 0; i < 20; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", 1)
	require.Len(t, decisions, 1)
	d := decisions[0]

	var excludedErroring *policy.ExcludedUpstream
	for i := range d.Output.Excluded {
		if d.Output.Excluded[i].ID == "erroring" {
			excludedErroring = &d.Output.Excluded[i]
			break
		}
	}
	require.NotNil(t, excludedErroring, "erroring upstream should be excluded")
	require.Contains(t, excludedErroring.Reason, "errorRate>0.5",
		"display Reason carries threshold for humans (step-log enabled)")
	require.Equal(t, []string{"error_rate_above"}, excludedErroring.LeafReasons,
		"leaf slug must be threshold-free and stable for metric label cardinality")
}

// TestStdlib_LeafReasons_AnyAttributesToTruthyLeaves verifies option (c):
// `any(A,B)` excluding an upstream attributes to the leaves that were
// ACTUALLY true on that upstream, not to a compound "any" slug.
// Operators see exactly which signal caused the drop.
func TestStdlib_LeafReasons_AnyAttributesToTruthyLeaves(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.excludeIf(any(errorRateAbove(0.5), blockNumberLagAbove(100)))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("erroring", "lagging", "both", "clean")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Setup block lag via SetLatestBlockNumber: network's tip is the max
	// across upstreams, lag is (tip - upstream's block). Clean & erroring
	// advertise the tip (lag=0); lagging & both advertise 200 blocks back.
	const tip = int64(1_000_000)
	tracker.SetLatestBlockNumber(ups[0], tip, 0)        // erroring at tip
	tracker.SetLatestBlockNumber(ups[1], tip-200, 0)    // lagging behind
	tracker.SetLatestBlockNumber(ups[2], tip-200, 0)    // both: behind
	tracker.SetLatestBlockNumber(ups[3], tip, 0)        // clean at tip

	// erroring: high error rate, low lag.
	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	// lagging: clean errors, high lag.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	// both: high error rate AND high lag — should attribute to both leaves.
	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*")
		tracker.RecordUpstreamFailure(ups[2], "*", fmt.Errorf("synth"))
	}
	// clean: nothing.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[3], "*")
		tracker.RecordUpstreamDuration(ups[3], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", 1)
	require.Len(t, decisions, 1)
	d := decisions[0]

	excludedByID := make(map[string][]string)
	for _, ex := range d.Output.Excluded {
		sorted := append([]string(nil), ex.LeafReasons...)
		sort.Strings(sorted)
		excludedByID[ex.ID] = sorted
	}

	require.Equal(t, []string{"error_rate_above"}, excludedByID["erroring"],
		"any() should attribute to errorRate when only that leaf is true")
	require.Equal(t, []string{"block_head_lag_above"}, excludedByID["lagging"],
		"any() should attribute to blockHeadLag when only that leaf is true")
	require.Equal(t, []string{"block_head_lag_above", "error_rate_above"}, excludedByID["both"],
		"any() with both leaves true should attribute to BOTH (not just first)")
	_, cleanExcluded := excludedByID["clean"]
	require.False(t, cleanExcluded, "clean upstream should stay in rotation")
}

// TestStdlib_LeafReasons_AllAttributesEveryLeaf verifies that `all(A,B)`
// excluding an upstream attributes to EVERY leaf (AND-semantics means
// every child must have been true).
func TestStdlib_LeafReasons_AllAttributesEveryLeaf(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.excludeIf(all(errorRateAbove(0.5), samplesAbove(10)))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("tripped", "low-samples")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// tripped: lots of samples, mostly errors → AND trips.
	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	// low-samples: just a few requests, all errors → errorRate trips but samplesAbove(10) does not.
	for i := 0; i < 5; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamFailure(ups[1], "*", fmt.Errorf("synth"))
	}

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", 1)
	require.Len(t, decisions, 1)
	d := decisions[0]

	excludedByID := make(map[string][]string)
	for _, ex := range d.Output.Excluded {
		sorted := append([]string(nil), ex.LeafReasons...)
		sort.Strings(sorted)
		excludedByID[ex.ID] = sorted
	}

	require.Equal(t, []string{"error_rate_above", "samples_above"}, excludedByID["tripped"],
		"all() must attribute to EVERY leaf when it trips")
	_, lowSamplesExcluded := excludedByID["low-samples"]
	require.False(t, lowSamplesExcluded,
		"all() must NOT trip when any leaf is false — `samples_above(10)` blocks attribution here")
}

// TestStdlib_LeafReasons_NotPrefixesSlug — `not(X)` excluding an upstream
// attributes to `"not_<X.slug>"` so the inversion is visible in metrics.
func TestStdlib_LeafReasons_NotPrefixesSlug(t *testing.T) {
	// Trip = "below samples" → exclude upstreams that DON'T have a minimum sample count.
	eval := `(upstreams, ctx) => upstreams.excludeIf(not(samplesAbove(10)))`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("starved", "mature")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	for i := 0; i < 3; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
	}
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
	}

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", 1)
	require.Len(t, decisions, 1)

	var starved *policy.ExcludedUpstream
	for i := range decisions[0].Output.Excluded {
		if decisions[0].Output.Excluded[i].ID == "starved" {
			starved = &decisions[0].Output.Excluded[i]
		}
	}
	require.NotNil(t, starved)
	require.Equal(t, []string{"not_samples_above"}, starved.LeafReasons)
}

// TestStdlib_LeafReasons_CustomInlinePredicate verifies that an inline
// arrow-function predicate (no `policySlug`) is attributed to `"custom"`.
// Operators who write ad-hoc predicates see exclusions count toward the
// `custom` bucket and can opt into a stable slug via the `reasonOverride`
// arg if they want their own label.
func TestStdlib_LeafReasons_CustomInlinePredicate(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.excludeIf(u => u.id === 'legacy')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("legacy", "modern")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", 1)
	require.Len(t, decisions, 1)
	d := decisions[0]

	var legacy *policy.ExcludedUpstream
	for i := range d.Output.Excluded {
		if d.Output.Excluded[i].ID == "legacy" {
			legacy = &d.Output.Excluded[i]
		}
	}
	require.NotNil(t, legacy)
	require.Equal(t, []string{"custom"}, legacy.LeafReasons,
		"inline predicate without a slug defaults to the bounded `custom` bucket")
}

// TestStdlib_LeafReasons_OverrideStringWins verifies the explicit
// `reasonOverride` arg becomes the metric slug — operators get a single
// well-known label for their custom rule instead of `"custom"`.
func TestStdlib_LeafReasons_OverrideStringWins(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.excludeIf(u => u.id === 'legacy', 'banned')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("legacy", "modern")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", 1)
	require.Len(t, decisions, 1)
	d := decisions[0]

	var legacy *policy.ExcludedUpstream
	for i := range d.Output.Excluded {
		if d.Output.Excluded[i].ID == "legacy" {
			legacy = &d.Output.Excluded[i]
		}
	}
	require.NotNil(t, legacy)
	require.Equal(t, []string{"banned"}, legacy.LeafReasons,
		"explicit reasonOverride wins for both display Reason AND leaf slug")
}

// TestStdlib_StickyHeld_FlagSetOnActiveHold validates that the
// `Diff.StickyHeld` flag is set when stickyPrimary actively holds a
// previous primary against a lower-scoring challenger, and NOT set when
// the score-based ordering already keeps the same primary.
func TestStdlib_StickyHeld_FlagSetOnActiveHold(t *testing.T) {
	// Long minSwitchInterval guarantees the cooldown branch keeps the
	// prior primary regardless of score gap. We use ids `a-prim` and
	// `b-chal` so the alphabetical tiebreak inside `sortByScore`
	// deterministically picks `a-prim` as the tick-1 primary; tick-2
	// degrades `a-prim` enough to flip the score order but the cooldown
	// holds the incumbent and sets `Diff.StickyHeld = true`.
	eval := `(upstreams, ctx) => upstreams.sortByScore(BALANCED).stickyPrimary({ hysteresis: 0.10, minSwitchInterval: '1h' })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("a-prim", "b-chal")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Tick 1: equal metrics → a-prim wins by id tiebreak. StickyHeld is
	// false because there's no prior primary to actively defend.
	for _, u := range ups {
		for i := 0; i < 100; i++ {
			tracker.RecordUpstreamRequest(u, "*")
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}
	policy.TickForTest(engine, "evm:1", "*")
	d1 := engine.RecentDecisions("evm:1", "*", 1)
	require.Len(t, d1, 1)
	require.Equal(t, "a-prim", d1[0].Output.Order[0])
	require.False(t, d1[0].Diff.StickyHeld, "no prior primary → sticky cannot have held")

	// Tick 2: degrade the incumbent so the score-only ordering would
	// flip. Errors stay under 50% (so excludeIf wouldn't trip — and we
	// don't even have an excludeIf in this eval anyway), just enough to
	// drag the score above the challenger's. Cooldown is still active
	// (1h minSwitchInterval, tick-1 just happened) → sticky must hold.
	for i := 0; i < 30; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	policy.TickForTest(engine, "evm:1", "*")
	d2 := engine.RecentDecisions("evm:1", "*", 1)
	require.Len(t, d2, 1)
	require.Equal(t, "a-prim", d2[0].Output.Order[0],
		"incumbent should be held by sticky despite worse score (cooldown still active)")
	require.True(t, d2[0].Diff.StickyHeld,
		"sticky actively holding a primary it would otherwise lose → flag must be true")
}
