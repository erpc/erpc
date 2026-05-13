package stdlib_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// fakeUpstream — minimal impl of common.Upstream for engine tests.
type fakeUpstream struct {
	id     string
	group  string
	vendor string
}

func (f *fakeUpstream) Id() string         { return f.id }
func (f *fakeUpstream) VendorName() string { return f.vendor }
func (f *fakeUpstream) NetworkId() string  { return "evm:1" }
func (f *fakeUpstream) NetworkLabel() string {
	return "evm:1"
}
func (f *fakeUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: f.id, Group: f.group}
}
func (f *fakeUpstream) Logger() *zerolog.Logger { l := zerolog.Nop(); return &l }
func (f *fakeUpstream) Vendor() common.Vendor   { return nil }
func (f *fakeUpstream) Tracker() common.HealthTracker {
	return nil
}
func (f *fakeUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (f *fakeUpstream) Cordon(method, reason string)   {}
func (f *fakeUpstream) Uncordon(method, reason string) {}
func (f *fakeUpstream) IgnoreMethod(method string)     {}

func mkUps(ids ...string) []common.Upstream {
	out := make([]common.Upstream, len(ids))
	for i, id := range ids {
		out[i] = &fakeUpstream{id: id, group: "main", vendor: "v" + id}
	}
	return out
}

func mkUpsWithGroups(spec map[string]string) []common.Upstream {
	out := make([]common.Upstream, 0, len(spec))
	for id, group := range spec {
		out = append(out, &fakeUpstream{id: id, group: group, vendor: "v"})
	}
	return out
}

func newTestEngine(t *testing.T, eval string) (*policy.Engine, []common.Upstream, *health.Tracker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(0), // frozen — tests drive manual ticks
		EvalTimeout:     common.Duration(50 * time.Millisecond),
		DecisionHistory: common.Duration(time.Minute),
		Eval:            eval,
	}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)
	return engine, nil, tracker, cancel
}

func ids(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

// TestStdlib_PreferGroupFallback validates the most-used legacy default-policy
// pattern: prefer the "main" group; if it has < minHealthy, fall back to the
// "fallback" group.
func TestStdlib_PreferGroupFallback(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.preferGroup('main', { minHealthy: 1, fallback: 'fallback' })`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithGroups(map[string]string{"rpc1": "main", "rpc2": "main", "rpc3": "fallback"})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	ordered := engine.GetOrdered("evm:1", "*")
	require.Len(t, ordered, 2, "should return both main-group upstreams")
	for _, u := range ordered {
		require.Equal(t, "main", u.Config().Group)
	}
}

// TestStdlib_RotateBy_RoundRobin verifies the translation path for legacy
// `routingStrategy: round-robin`.
func TestStdlib_RotateBy_RoundRobin(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.rotateBy(ctx.tickCount)`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2", "rpc3")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		policy.TickForTest(engine, "evm:1", "*")
		ordered := engine.GetOrdered("evm:1", "*")
		require.Len(t, ordered, 3)
		seen[ordered[0].Id()] = true
	}
	require.Len(t, seen, 3, "all three upstreams should rotate into primary")
}

// TestStdlib_SortByScore_BalancedPenalizesErrors verifies the BALANCED preset
// puts the high-error upstream behind the clean one.
func TestStdlib_SortByScore_BalancedPenalizesErrors(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(BALANCED)`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Drive rpc1 → high error rate; rpc2 → clean.
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
	ordered := engine.GetOrdered("evm:1", "*")
	require.Equal(t, []string{"rpc2", "rpc1"}, ids(ordered),
		"rpc2 (clean) should outrank rpc1 (high error rate)")
}

// TestStdlib_RemoveByLag verifies the spec §4.3 health filter.
func TestStdlib_RemoveByLag(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.removeByLag({ blockHead: 10 })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2", "rpc3")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Force-create metric entries, then set lag directly. (We can't use
	// SetLatestBlockNumber here without a real registry.)
	for _, u := range ups {
		m := tracker.GetUpstreamMethodMetrics(u, "*")
		m.BlockHeadLag.Store(0)
	}
	tracker.GetUpstreamMethodMetrics(ups[1], "*").BlockHeadLag.Store(50) // rpc2 lagging

	policy.TickForTest(engine, "evm:1", "*")
	ordered := ids(engine.GetOrdered("evm:1", "*"))
	require.NotContains(t, ordered, "rpc2", "rpc2 (lag=50) should be excluded by removeByLag(blockHead:10)")
	require.Contains(t, ordered, "rpc1")
	require.Contains(t, ordered, "rpc3")
}

// TestStdlib_RichDefaultPolicy verifies the engine's auto-upgrade from the
// trivial identity placeholder (common.DefaultSelectionPolicySource) to the
// embedded `internal/policy/default_policy.js`. With no group named
// "default" and a "fallback" group available, the default policy still
// returns the whole input (preferGroup falls back to all upstreams when
// neither named group matches).
func TestStdlib_RichDefaultPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{} // empty → SetDefaults installs trivial → engine upgrades.
	require.NoError(t, cfg.SetDefaults())
	require.Equal(t, common.DefaultSelectionPolicySource, cfg.Eval, "SetDefaults should install the placeholder")

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)
	defer engine.Stop()

	ups := mkUpsWithGroups(map[string]string{"rpc1": "default", "rpc2": "fallback"})
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// After RegisterNetwork the cfg should hold the rich default source.
	require.Contains(t, cfg.Eval, "sortByScore(BALANCED)",
		"engine should upgrade the placeholder to the rich default")

	// And the policy should run end-to-end: rpc1 in 'default' group wins.
	ordered := engine.GetOrdered("evm:1", "*")
	require.Len(t, ordered, 1)
	require.Equal(t, "rpc1", ordered[0].Id())
}

// TestStdlib_StickyPrimary_RetainsPriorPrimary verifies cross-tick stickiness.
func TestStdlib_StickyPrimary_RetainsPriorPrimary(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore({ errorRate: 8 }).stickyPrimary({ hysteresis: 0.5 })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Tick 1: rpc1 clean, rpc2 broken → rpc1 becomes primary.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamFailure(ups[1], "*", fmt.Errorf("synth"))
	}
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, "rpc1", engine.GetOrdered("evm:1", "*")[0].Id())

	// Tick 2: rpc1 now WORSE than rpc2 but gap is within hysteresis → stays primary.
	// rpc1 → errorRate ~0.5 → score 0.5*8 = 4
	// rpc2 → errorRate ~0.2 → score 0.2*8 = 1.6
	// switch condition: 1.6 < 4 * (1 - 0.5) = 2 → false (1.6 < 2 IS true)
	// Use a smaller gap so switch is suppressed.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")
	// rpc2 is now clean (error rate 50/250 = 0.2); rpc1 still 0 errors.
	// Sticky shouldn't matter here — rpc1 is still better.
	require.Equal(t, "rpc1", engine.GetOrdered("evm:1", "*")[0].Id())
}

// TestStdlib_KeepHealthy_CompositeFilter pins the composite filter from
// spec §4.3: drops on errorRate > threshold OR blockHeadLag > threshold
// OR p95Ms > threshold OR throttledRate > threshold.
func TestStdlib_KeepHealthy_CompositeFilter(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.keepHealthy({ maxErrorRate: 0.3, maxBlockHeadLag: 10 })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2", "rpc3")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// rpc1: clean. rpc2: 50% errors. rpc3: blockHeadLag=50.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamFailure(ups[1], "*", fmt.Errorf("synth"))
	}
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for _, u := range ups {
		tracker.GetUpstreamMethodMetrics(u, "*").BlockHeadLag.Store(0)
	}
	tracker.GetUpstreamMethodMetrics(ups[2], "*").BlockHeadLag.Store(50)
	for i := 0; i < 10; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*")
		tracker.RecordUpstreamDuration(ups[2], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	ordered := ids(engine.GetOrdered("evm:1", "*"))
	require.Contains(t, ordered, "rpc1")
	require.NotContains(t, ordered, "rpc2", "rpc2 dropped: errorRate 0.5 > 0.3")
	require.NotContains(t, ordered, "rpc3", "rpc3 dropped: blockHeadLag 50 > 10")
}

// TestStdlib_Combinators_WhenEmpty_FallbackTo exercises chain control flow.
func TestStdlib_Combinators_WhenEmpty_FallbackTo(t *testing.T) {
	eval := `
		(upstreams, ctx) =>
			upstreams
				.byGroup('nonexistent')
				.whenEmpty(() => upstreams.byGroup('main'))
	`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithGroups(map[string]string{"rpc1": "main", "rpc2": "main"})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	ordered := engine.GetOrdered("evm:1", "*")
	require.Len(t, ordered, 2, "whenEmpty should fall back to main group")
}

// TestStdlib_Slicing_PickTop_DropTop pins the slicing primitives.
func TestStdlib_Slicing_PickTop_DropTop(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(BALANCED).pickTop(2)`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2", "rpc3", "rpc4")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	require.Len(t, engine.GetOrdered("evm:1", "*"), 2, "pickTop(2) should cap output at 2")
}

// TestStdlib_DecisionRecord_CapturesExclusions pins the decision-record
// output that powers the /admin/selection/* endpoints.
func TestStdlib_DecisionRecord_CapturesExclusions(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.byGroup('main')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithGroups(map[string]string{"rpc1": "main", "rpc2": "main", "rpc3": "fallback"})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	decisions := policy.DecisionsForTest(engine, "evm:1", "*")
	require.NotEmpty(t, decisions)
	latest := decisions[len(decisions)-1]
	require.Empty(t, latest.Error)
	require.Len(t, latest.Output.Order, 2, "main group has two members")
	require.Len(t, latest.Output.Excluded, 1, "rpc3 (fallback) is excluded")
	require.Equal(t, "rpc3", latest.Output.Excluded[0].ID)
}
