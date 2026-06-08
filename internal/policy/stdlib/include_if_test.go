package stdlib_test

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// mkReserveEngine builds a frozen-tick engine over the given eval with a
// pool of two "primary" upstreams (tier:main) and one "reserve" upstream
// (tier:reserve). Returns the engine + tracker so a test can shape metrics
// before ticking.
func mkReserveEngine(t *testing.T, eval string) (*policy.Engine, []common.Upstream, *health.Tracker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{id: "primary1", vendor: "cheap", tags: []string{"tier:main"}},
		{id: "primary2", vendor: "cheap", tags: []string{"tier:main"}},
		{id: "reserve1", vendor: "premium", tags: []string{"tier:reserve"}},
	})

	cfg := &common.SelectionPolicyConfig{
		EvalInterval: 0,
		EvalTimeout:  common.Duration(50 * time.Millisecond),
		EvalFunc:     eval,
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))
	return engine, ups, tracker, cancel
}

// setLag force-creates the metric entry for an upstream and pins its
// block-head lag (block-count) so the lag predicates are deterministic
// without depending on the block-time EMA.
func setLag(tracker *health.Tracker, u common.Upstream, lag int64) {
	tracker.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll).BlockHeadLag.Store(lag)
}

// ─── includeIf: core admit / no-admit behavior ──────────────────────────

// When the gate holds, the selected reserve upstream is unioned in at the
// tail (survivors keep priority). The condition uses a native `every` over a
// per-upstream predicate factory.
func TestIncludeIf_AdmitsReserveWhenConditionHolds(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.length > 0 && p.every(blockNumberLagAbove(16)), { tag: 'tier:reserve' })`
	engine, ups, tracker, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	// Both primaries lag past the threshold; reserve is fresh.
	setLag(tracker, ups[0], 50)
	setLag(tracker, ups[1], 50)
	setLag(tracker, ups[2], 0)

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"primary1", "primary2", "reserve1"}, got,
		"all survivors lag → reserve admitted at the tail, survivors keep priority")
}

// When the gate does NOT hold, the reserve stays out and the survivors are
// untouched — crucially, a degraded primary is never evicted by includeIf.
func TestIncludeIf_KeepsReserveOutWhenConditionFails(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.length > 0 && p.every(blockNumberLagAbove(16)), { tag: 'tier:reserve' })`
	engine, ups, tracker, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	// primary1 lags, primary2 is healthy → `every` is false.
	setLag(tracker, ups[0], 50)
	setLag(tracker, ups[1], 0)
	setLag(tracker, ups[2], 0)

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"primary1", "primary2"}, got,
		"one healthy survivor → reserve stays out AND the lagging primary is not evicted")
}

// A literal `true` condition always admits; the selector still scopes which
// upstreams come in.
func TestIncludeIf_BooleanTrueAlwaysAdmits(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf(true, { tag: 'tier:reserve' })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.ElementsMatch(t, []string{"primary1", "primary2", "reserve1"}, got,
		"condition=true admits the tag-selected reserve unconditionally")
}

// A literal `false` condition is a no-op.
func TestIncludeIf_BooleanFalseIsNoOp(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf(false, { tag: 'tier:reserve' })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.ElementsMatch(t, []string{"primary1", "primary2"}, got,
		"condition=false admits nothing")
}

// position:'head' puts the admitted reserve in front of the survivors.
func TestIncludeIf_PositionHead(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf(true, { tag: 'tier:reserve', position: 'head' })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"reserve1", "primary1", "primary2"}, got,
		"position:head places admitted upstreams ahead of survivors")
}

// An upstream already present is not duplicated.
func TestIncludeIf_DeduplicatesAlreadyPresent(t *testing.T) {
	// No preferTag → reserve1 is already in the pool. includeIf must not
	// add a second copy.
	eval := `(upstreams) => upstreams.includeIf(true, { tag: 'tier:reserve' })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"primary1", "primary2", "reserve1"}, got,
		"reserve1 already present → not duplicated")
}

// With no selector facet, includeIf is a no-op — it must never admit the
// entire universe.
func TestIncludeIf_NoSelectorIsNoOp(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf(true, {})`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.ElementsMatch(t, []string{"primary1", "primary2"}, got,
		"selector-less includeIf admits nothing (never the whole universe)")
}

// A present-but-empty `where` (and a `where` whose only keys resolve to
// nothing) must ALSO be a no-op — the selector gate keys on the resolved
// facets, not on `where != null`. Regression for the "empty where admits
// universe" class.
func TestIncludeIf_EmptyWhereIsNoOp(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf(true, { where: {} })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.ElementsMatch(t, []string{"primary1", "primary2"}, got,
		"where:{} resolves no facets → no-op, must NOT admit the universe")
}

// id and vendor selectors resolve against the universe.
func TestIncludeIf_SelectorsByIdAndVendor(t *testing.T) {
	byId := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf(true, { id: 'reserve1' })`
	engine, _, _, cancel := mkReserveEngine(t, byId)
	defer cancel()
	defer engine.Stop()
	policy.TickForTest(engine, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2", "reserve1"},
		ids(engine.GetOrdered("evm:1", "*", "*")), "id selector admits reserve1")

	byVendor := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf(true, { vendor: 'premium' })`
	engine2, _, _, cancel2 := mkReserveEngine(t, byVendor)
	defer cancel2()
	defer engine2.Stop()
	policy.TickForTest(engine2, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2", "reserve1"},
		ids(engine2.GetOrdered("evm:1", "*", "*")), "vendor selector admits the premium reserve")
}

// The type facet is honored, not silently ignored. The fake upstreams carry
// an empty type, so a `type: 'evm'` selector must match NOTHING — proving the
// facet is actually applied (an ignored facet would make matchFn match-all
// and wrongly admit the reserve).
func TestIncludeIf_TypeSelectorIsApplied(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf(true, { type: 'evm' })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2"},
		ids(engine.GetOrdered("evm:1", "*", "*")),
		"type:'evm' matches no upstream (all have empty type) → reserve not admitted")
}

// where{} facet form is equivalent to the flat facets.
func TestIncludeIf_WhereSelectorForm(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.length < 3, { where: { tag: 'tier:reserve' } })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2", "reserve1"},
		ids(engine.GetOrdered("evm:1", "*", "*")),
		"where:{tag} behaves like the flat tag facet")
}

// A throwing custom condition must NOT crash the eval (which would fall the
// network back to the engine default) — it degrades to "do not include".
func TestIncludeIf_ThrowingConditionDegradesToNoOp(t *testing.T) {
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => { throw new Error('boom'); }, { tag: 'tier:reserve' })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.ElementsMatch(t, []string{"primary1", "primary2"}, got,
		"a throwing condition is swallowed → no admission, eval still produces the survivors")
}

// The function condition receives the CURRENT (post-preferTag) pool, not the
// original full universe — proving aggregate questions see only survivors.
func TestIncludeIf_ConditionSeesSurvivingPool(t *testing.T) {
	// After preferTag, only the 2 primaries remain. `pool.length === 2`
	// proves the condition sees the survivors, not the full 3-upstream set.
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.length === 2, { tag: 'tier:reserve' })`
	engine, _, _, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	policy.TickForTest(engine, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2", "reserve1"},
		ids(engine.GetOrdered("evm:1", "*", "*")),
		"condition observed pool.length==2 (survivors only), so it admitted the reserve")
}

// ─── conditions via native array methods ────────────────────────────────

// `every` admits only when EVERY survivor trips; `some` admits if at least
// one does.
func TestIncludeIf_EveryVsSome(t *testing.T) {
	// One primary lags, one is healthy.
	shape := func(tracker *health.Tracker, ups []common.Upstream) {
		setLag(tracker, ups[0], 50) // primary1 lags
		setLag(tracker, ups[1], 0)  // primary2 healthy
		setLag(tracker, ups[2], 0)
	}

	everyEval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.length > 0 && p.every(blockNumberLagAbove(16)), { tag: 'tier:reserve' })`
	engine, ups, tracker, cancel := mkReserveEngine(t, everyEval)
	defer cancel()
	defer engine.Stop()
	shape(tracker, ups)
	policy.TickForTest(engine, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2"},
		ids(engine.GetOrdered("evm:1", "*", "*")),
		"every: not every survivor lags → no admission")

	someEval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.some(blockNumberLagAbove(16)), { tag: 'tier:reserve' })`
	engine2, ups2, tracker2, cancel2 := mkReserveEngine(t, someEval)
	defer cancel2()
	defer engine2.Stop()
	shape(tracker2, ups2)
	policy.TickForTest(engine2, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2", "reserve1"},
		ids(engine2.GetOrdered("evm:1", "*", "*")),
		"some: at least one survivor lags → reserve admitted")
}

// Native `every` is TRUE on an empty pool — so a bare `p.every(...)` admits
// the reserve when the pool is empty, while a `p.length > 0 && p.every(...)`
// guard suppresses that. Both behaviors are useful; the test pins them.
func TestIncludeIf_EmptyPoolEveryNativeSemantics(t *testing.T) {
	// Bare every over an empty pool → true → admit the reserve.
	bare := `(upstreams) => upstreams
		.byTag('tier:main')
		.excludeId('primary*')
		.includeIf((p) => p.every(blockNumberLagAbove(16)), { tag: 'tier:reserve' })`
	engine, _, _, cancel := mkReserveEngine(t, bare)
	defer cancel()
	defer engine.Stop()
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, []string{"reserve1"}, ids(engine.GetOrdered("evm:1", "*", "*")),
		"native every is true on [] → empty pool admits the reserve")

	// Guarded every over an empty pool → false → no admit. Use `length < 1`
	// instead if you DO want the empty pool to admit.
	guarded := `(upstreams) => upstreams
		.byTag('tier:main')
		.excludeId('primary*')
		.includeIf((p) => p.length > 0 && p.every(blockNumberLagAbove(16)), { tag: 'tier:reserve' })`
	engine2, _, _, cancel2 := mkReserveEngine(t, guarded)
	defer cancel2()
	defer engine2.Stop()
	policy.TickForTest(engine2, "evm:1", "*")
	require.Empty(t, ids(engine2.GetOrdered("evm:1", "*", "*")),
		"length>0 guard suppresses the empty-pool admit")
}

// `length` gates purely on pool size.
func TestIncludeIf_SizeGates(t *testing.T) {
	// length < 3: 2 survivors → admit.
	below := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.length < 3, { tag: 'tier:reserve' })`
	e1, _, _, c1 := mkReserveEngine(t, below)
	defer c1()
	defer e1.Stop()
	policy.TickForTest(e1, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2", "reserve1"},
		ids(e1.GetOrdered("evm:1", "*", "*")), "length<3: 2<3 → admit")

	// length >= 3: only 2 survivors → false → no admit.
	atLeast := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.length >= 3, { tag: 'tier:reserve' })`
	e2, _, _, c2 := mkReserveEngine(t, atLeast)
	defer c2()
	defer e2.Stop()
	policy.TickForTest(e2, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2"},
		ids(e2.GetOrdered("evm:1", "*", "*")), "length>=3: only 2 → no admit")
}

// `filter(...).length` counts matching survivors for an N-of-M condition.
func TestIncludeIf_FilterCount(t *testing.T) {
	// Both primaries lag → 2 match. Admit when at least 2 lag.
	eval := `(upstreams) => upstreams
		.preferTag('!tier:reserve', { fallback: 'tier:reserve' })
		.includeIf((p) => p.filter(blockNumberLagAbove(16)).length >= 2, { tag: 'tier:reserve' })`
	engine, ups, tracker, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()
	setLag(tracker, ups[0], 50)
	setLag(tracker, ups[1], 50)
	policy.TickForTest(engine, "evm:1", "*")
	require.ElementsMatch(t, []string{"primary1", "primary2", "reserve1"},
		ids(engine.GetOrdered("evm:1", "*", "*")), "2 of 2 survivors lag → admit")
}

// ─── end-to-end: the break-glass reserve-tier pattern ───────────────────

// Full realistic chain: a reserve tier kept out of rotation via excludeTag,
// then ranked in only when every serving upstream is lagging. Verifies the
// property that matters operationally — the reserve is consulted ONLY under
// collective degradation, and the survivors are never dropped.
func TestIncludeIf_EndToEnd_ReserveTierBreakGlass(t *testing.T) {
	eval := `(upstreams) => upstreams
		.removeCordoned()
		.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
		.excludeTag('tier:reserve')
		.includeIf((p) => p.length > 0 && p.every(blockNumberLagAbove(16)), { tag: 'tier:reserve' })
		.includeIf((p) => p.length < 1, { tag: 'tier:reserve' })
		.sortByScore(PREFER_FASTEST)`

	// Phase 1: primaries healthy → reserve excluded.
	engine, ups, tracker, cancel := mkReserveEngine(t, eval)
	defer cancel()
	defer engine.Stop()
	for _, u := range ups {
		for i := 0; i < 50; i++ {
			tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}
	setLag(tracker, ups[0], 0)
	setLag(tracker, ups[1], 0)
	setLag(tracker, ups[2], 0)
	policy.TickForTest(engine, "evm:1", "*")
	require.NotContains(t, ids(engine.GetOrdered("evm:1", "*", "*")), "reserve1",
		"healthy primaries → reserve stays out of rotation (no premium spend)")

	// Phase 2: both primaries fall behind tip → reserve ranked in.
	setLag(tracker, ups[0], 40)
	setLag(tracker, ups[1], 40)
	policy.TickForTest(engine, "evm:1", "*")
	require.Contains(t, ids(engine.GetOrdered("evm:1", "*", "*")), "reserve1",
		"all primaries lagging → reserve admitted as break-glass")
}
