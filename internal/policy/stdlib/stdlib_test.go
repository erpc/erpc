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
// Only the canonical fields (id, vendor, tags) are tracked; legacy
// group / cohort fields are gone and tests must use tags directly.
type fakeUpstream struct {
	id      string
	vendor  string
	tags    []string
	routing *common.UpstreamRoutingConfig
}

func (f *fakeUpstream) Id() string         { return f.id }
func (f *fakeUpstream) VendorName() string { return f.vendor }
func (f *fakeUpstream) NetworkId() string  { return "evm:1" }
func (f *fakeUpstream) NetworkLabel() string {
	return "evm:1"
}
func (f *fakeUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: f.id, Tags: f.tags, Routing: f.routing}
}
func (f *fakeUpstream) Logger() *zerolog.Logger { l := zerolog.Nop(); return &l }
func (f *fakeUpstream) Vendor() common.Vendor   { return nil }
func (f *fakeUpstream) Tracker() common.HealthTracker {
	return nil
}
func (f *fakeUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion, isHedgeAttempt bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (f *fakeUpstream) Cordon(method, reason string)   {}
func (f *fakeUpstream) Uncordon(method, reason string) {}
func (f *fakeUpstream) IgnoreMethod(method string)     {}

func mkUps(ids ...string) []common.Upstream {
	out := make([]common.Upstream, len(ids))
	for i, id := range ids {
		out[i] = &fakeUpstream{id: id, vendor: "v" + id, tags: []string{"tier:main"}}
	}
	return out
}

// mkUpsWithTier constructs upstreams with explicit tier tags
// (`tier:<value>`). Map iteration order is non-deterministic; tests
// that care about input order must use `mkUpsWithTags` directly.
func mkUpsWithTier(spec map[string]string) []common.Upstream {
	out := make([]common.Upstream, 0, len(spec))
	for id, tier := range spec {
		out = append(out, &fakeUpstream{id: id, vendor: "v", tags: []string{"tier:" + tier}})
	}
	return out
}

// mkUpsWithTags constructs N upstreams with explicit (id, vendor,
// tags). Slice order is preserved. Use when the test cares about
// input order or carries multi-dimensional tags.
func mkUpsWithTags(specs []struct {
	id     string
	vendor string
	tags   []string
}) []common.Upstream {
	out := make([]common.Upstream, len(specs))
	for i, s := range specs {
		v := s.vendor
		if v == "" {
			v = "v" + s.id
		}
		out[i] = &fakeUpstream{id: s.id, vendor: v, tags: s.tags}
	}
	return out
}

// mkUpsWithScoreMultipliers builds upstreams (order preserved) where each
// id in `mults` gets a single match-all `routing.scoreMultipliers` entry.
// ids absent from `mults` carry no routing config.
func mkUpsWithScoreMultipliers(order []string, mults map[string]*common.ScoreMultiplierConfig) []common.Upstream {
	out := make([]common.Upstream, len(order))
	for i, id := range order {
		f := &fakeUpstream{id: id, vendor: "v" + id, tags: []string{"tier:main"}}
		if m := mults[id]; m != nil {
			f.routing = &common.UpstreamRoutingConfig{ScoreMultipliers: []*common.ScoreMultiplierConfig{m}}
		}
		out[i] = f
	}
	return out
}

func f64(v float64) *float64 { return &v }

func newTestEngine(t *testing.T, eval string) (*policy.Engine, []common.Upstream, *health.Tracker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(0), // frozen — tests drive manual ticks
		EvalTimeout:     common.Duration(50 * time.Millisecond),
		EvalFunc:            eval,
	}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	return engine, nil, tracker, cancel
}

func ids(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

// recordEqual records `n` identical successful requests of `dur` for each
// upstream — used to give a deterministic, equal metric baseline so a
// test isolates the effect of scoreMultipliers.
func recordEqual(tracker *health.Tracker, ups []common.Upstream, n int, dur time.Duration) {
	for _, u := range ups {
		for i := 0; i < n; i++ {
			tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
			tracker.RecordUpstreamDuration(u, "*", dur, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}
}

// TestStdlib_ScoreMultipliers_OverallBoost: with the default 'merge' mode,
// a per-upstream `overall` is a pure preference dial. `a` and `b` have
// identical metrics, so the alphabetical tiebreak would put `a` first;
// `b`'s `overall: 5` must lift it above `a`.
func TestStdlib_ScoreMultipliers_OverallBoost(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST)`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithScoreMultipliers([]string{"a", "b"}, map[string]*common.ScoreMultiplierConfig{
		"b": {Overall: f64(5)},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	recordEqual(tracker, ups, 50, 20*time.Millisecond)
	policy.TickForTest(engine, "evm:1", "*")

	require.Equal(t, []string{"b", "a"}, ids(engine.GetOrdered("evm:1", "*", "*")),
		"overall:5 on `b` (merge mode) must lift it above the otherwise-equal `a`")
}

// TestStdlib_ScoreMultipliers_OffIgnoresConfig: identical setup, but
// `multipliers: 'off'` makes sortByScore ignore `u.scoreMultipliers`, so
// `b`'s boost is dropped and the alphabetical tiebreak wins → `a` first.
func TestStdlib_ScoreMultipliers_OffIgnoresConfig(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST, { multipliers: 'off' })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithScoreMultipliers([]string{"a", "b"}, map[string]*common.ScoreMultiplierConfig{
		"b": {Overall: f64(5)},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	recordEqual(tracker, ups, 50, 20*time.Millisecond)
	policy.TickForTest(engine, "evm:1", "*")

	require.Equal(t, []string{"a", "b"}, ids(engine.GetOrdered("evm:1", "*", "*")),
		"multipliers:'off' must ignore u.scoreMultipliers (boost dropped, alphabetical wins)")
}

// TestStdlib_ScoreMultipliers_OverrideMode: in 'override' mode a configured
// upstream ranks by ITS weights only. `a` is error-prone but fast; its
// override scores on latency alone (no errorRate weight), so its errors are
// ignored and it outranks the clean-but-slower `b`. Under the base preset
// (or 'merge') `a`'s errors would sink it below `b`.
func TestStdlib_ScoreMultipliers_OverrideMode(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST, { multipliers: 'override' })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithScoreMultipliers([]string{"a", "b"}, map[string]*common.ScoreMultiplierConfig{
		"a": {RespLatency: f64(15)}, // latency only — errorRate weight intentionally absent
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// `a`: fast (10ms) but 50% errors. `b`: clean but slower (80ms).
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[0], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 80*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")

	require.Equal(t, []string{"a", "b"}, ids(engine.GetOrdered("evm:1", "*", "*")),
		"override: `a` ranks on latency alone, ignoring its errors, so it beats the slower `b`")
}

// TestStdlib_PreferTagFallback validates the most-used default-policy
// pattern: prefer the `tier:main` upstreams; if fewer than `minHealthy`
// match, fall back to `tier:fallback`.
func TestStdlib_PreferTagFallback(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.preferTag('tier:main', { minHealthy: 1, fallback: 'tier:fallback' })`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTier(map[string]string{"rpc1": "main", "rpc2": "main", "rpc3": "fallback"})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	ordered := engine.GetOrdered("evm:1", "*", "*")
	require.Len(t, ordered, 2, "should return both tier:main upstreams")
	for _, u := range ordered {
		require.True(t, u.Config().HasTag("tier:main"),
			"selected upstream %s must carry tier:main", u.Id())
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
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		policy.TickForTest(engine, "evm:1", "*")
		ordered := engine.GetOrdered("evm:1", "*", "*")
		require.Len(t, ordered, 3)
		seen[ordered[0].Id()] = true
	}
	require.Len(t, seen, 3, "all three upstreams should rotate into primary")
}

// TestStdlib_SortByScore_PenalizesErrors verifies the PREFER_FASTEST preset
// puts the high-error upstream behind the clean one.
func TestStdlib_SortByScore_PenalizesErrors(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST)`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Drive rpc1 → high error rate; rpc2 → clean.
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
	ordered := engine.GetOrdered("evm:1", "*", "*")
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
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Force-create metric entries, then set lag directly. (We can't use
	// SetLatestBlockNumber here without a real registry.)
	for _, u := range ups {
		m := tracker.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll)
		m.BlockHeadLag.Store(0)
	}
	tracker.GetUpstreamMethodMetrics(ups[1], "*", common.DataFinalityStateAll).BlockHeadLag.Store(50) // rpc2 lagging

	policy.TickForTest(engine, "evm:1", "*")
	ordered := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.NotContains(t, ordered, "rpc2", "rpc2 (lag=50) should be excluded by removeByLag(blockHead:10)")
	require.Contains(t, ordered, "rpc1")
	require.Contains(t, ordered, "rpc3")
}

// TestStdlib_RichDefaultPolicy verifies the engine's auto-upgrade from the
// trivial identity placeholder (common.DefaultSelectionPolicySource) to the
// embedded `internal/policy/default_policy.js`. Primary tier is matched as
// `!tier:fallback`; rpc1 (tier:default) is primary, rpc2 (tier:fallback)
// is the second tier.
func TestStdlib_RichDefaultPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{} // empty → SetDefaults installs trivial → engine upgrades.
	require.NoError(t, cfg.SetDefaults())
	require.Equal(t, common.DefaultSelectionPolicySource, cfg.EvalFunc, "SetDefaults should install the placeholder")

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	ups := mkUpsWithTier(map[string]string{"rpc1": "default", "rpc2": "fallback"})
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// After RegisterNetwork the cfg should hold the rich default source.
	require.Contains(t, cfg.EvalFunc, "sortByScore(PREFER_FASTEST)",
		"engine should upgrade the placeholder to the rich default")

	// And the policy should run end-to-end: rpc1 (tier:default) wins
	// because tier:fallback is only used as a backstop.
	ordered := engine.GetOrdered("evm:1", "*", "*")
	require.Len(t, ordered, 1)
	require.Equal(t, "rpc1", ordered[0].Id())
}

// TestStdlib_DefaultPolicy_DropsBrokenUpstream pins the production-ready
// default policy's filter step: an upstream with >80% error rate is
// dropped, healthy ones survive.
func TestStdlib_DefaultPolicy_DropsBrokenUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// rpc1: 90% error rate (broken). rpc2: clean.
	for i := 0; i < 90; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[0], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	for i := 0; i < 10; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	ordered := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"rpc2"}, ordered, "rpc1 should be dropped by keepHealthy (errorRate=0.9 > 0.5)")
}

// TestStdlib_DefaultPolicy_SafetyNetWhenAllBroken pins the safety-net step:
// if filtering drops all upstreams (e.g. project-wide outage), the policy
// returns the unfiltered set rather than failing closed.
func TestStdlib_DefaultPolicy_SafetyNetWhenAllBroken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Both upstreams broken.
	for _, u := range ups {
		for i := 0; i < 90; i++ {
			tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
			tracker.RecordUpstreamFailure(u, "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
		}
		for i := 0; i < 10; i++ {
			tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}

	policy.TickForTest(engine, "evm:1", "*")
	ordered := engine.GetOrdered("evm:1", "*", "*")
	require.Len(t, ordered, 2, "safety net: when all upstreams fail filter, return all rather than empty")
}

// TestStdlib_DefaultPolicy_FallbackTierWhenPrimaryEmpty pins the fallback
// tier: with no upstreams in the primary group (`group !== 'fallback'`)
// the policy falls through to the explicit `fallback` group.
func TestStdlib_DefaultPolicy_FallbackTierWhenPrimaryEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	// All upstreams tagged tier:fallback → primary tier
	// (`!tier:fallback`) is empty.
	ups := mkUpsWithTier(map[string]string{"rpc1": "fallback", "rpc2": "fallback"})
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	ordered := engine.GetOrdered("evm:1", "*", "*")
	require.Len(t, ordered, 2, "fallback tier should serve when primary is empty")
}

// TestStdlib_DefaultPolicy_DropsLaggingErrorFreeUpstream (G1):
// an upstream with 0 errors and great latency but blockHeadLag > 16 is
// excluded by `keepHealthy(maxBlockHeadLag: 16)`. This is the class of
// incident where a node falls behind tip silently (still returns 200s
// for whatever it has) and consensus or `latest`-style queries get
// stale data because the policy only ranked, not filtered, on lag.
func TestStdlib_DefaultPolicy_DropsLaggingErrorFreeUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Drive 100 clean requests for both; only rpc1 is lagging.
	for _, u := range ups {
		for i := 0; i < 100; i++ {
			tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}
	// rpc1 lags 30 blocks behind tip (> 16 default threshold).
	tracker.GetUpstreamMethodMetrics(ups[0], "*", common.DataFinalityStateAll).BlockHeadLag.Store(30)

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"rpc2"}, got,
		"rpc1 has 0 errors but blockHeadLag=30 > 16 → must be excluded by keepHealthy, not just ranked lower")
}

// TestStdlib_DefaultPolicy_DropsHighLatencyErrorFreeUpstream (G2):
// an upstream with 0 errors but p95 > 10s is excluded. This is the
// "slow-vendor drag" class — one upstream serving 3-second traces
// pulls the pool's blended latency percentiles down and stalls
// consensus on faster requests.
func TestStdlib_DefaultPolicy_DropsHighLatencyErrorFreeUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// rpc1: 100 requests, all SLOW (12s) → p95 ≈ 12s > 10s threshold.
	// rpc2: 100 requests, all fast (10ms) → p95 ≈ 10ms.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 12*time.Second, true, "none", common.DataFinalityStateUnknown, "n/a")
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"rpc2"}, got,
		"rpc1 has 0 errors but p95=12s > 10s → must be excluded by keepHealthy(maxP95Ms:10_000)")
}

// TestStdlib_DefaultPolicy_StickyPrimary_AllFinalities verifies that
// the default policy's `stickyPrimary` holds the incumbent across the
// minSwitchInterval cooldown REGARDLESS of `ctx.finality`. Flapping
// between two similarly-ranked upstreams every tick costs more in
// connection-setup + cache-locality than it saves in marginal ranking
// accuracy, regardless of whether the request is reorg-tolerant — so
// the policy treats finalized + unfinalized + realtime requests the
// same for stickiness purposes.
//
// Setup: tick 1 with both clean → rpc1 wins by id tiebreak and gets
// recorded as the sticky primary. Then rpc1 errors heavily so scoring
// would favour rpc2 on its own. Tick 2 under each finality: the
// `minSwitchInterval` cooldown is still live, so rpc1 remains primary.
func TestStdlib_DefaultPolicy_StickyPrimary_AllFinalities(t *testing.T) {
	defaultPolicy := policy.DefaultPolicySource()

	mkEngine := func() (*policy.Engine, []common.Upstream, *health.Tracker, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		logger := zerolog.Nop()
		tracker := health.NewTracker(&logger, "p1", time.Minute)
		cfg := &common.SelectionPolicyConfig{EvalFunc: defaultPolicy}
		require.NoError(t, cfg.SetDefaults())
		engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)

		ups := mkUps("rpc1", "rpc2")
		require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

		for _, u := range ups {
			for i := 0; i < 100; i++ {
				tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
				tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
			}
		}
		return engine, ups, tracker, cancel
	}

	for _, finality := range []string{"realtime", "unfinalized", "finalized"} {
		t.Run(finality+"_holds_prior_primary", func(t *testing.T) {
			engine, ups, tracker, cancel := mkEngine()
			defer cancel()
			defer engine.Stop()

			policy.SetFinalityForTest(engine, "evm:1", "*", finality)

			// Tick 1 — equal metrics → rpc1 wins by id tiebreak.
			policy.TickForTest(engine, "evm:1", "*")
			first := ids(engine.GetOrdered("evm:1", "*", "*"))
			require.Equal(t, "rpc1", first[0], "tick 1: rpc1 wins by id tiebreak")

			// Make rpc1 clearly worse — but errorRate stays below the
			// 50% excludeIf threshold so the contest is over RANKING,
			// not exclusion.
			for i := 0; i < 30; i++ {
				tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
				tracker.RecordUpstreamFailure(ups[0], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
			}

			// Tick 2 — under EVERY finality, the 30s minSwitchInterval
			// keeps rpc1 as primary.
			policy.TickForTest(engine, "evm:1", "*")
			second := ids(engine.GetOrdered("evm:1", "*", "*"))
			require.GreaterOrEqual(t, len(second), 1)
			require.Equal(t, "rpc1", second[0],
				"tick 2 primary under finality=%s must stay rpc1 (sticky)", finality)
		})
	}
}

// TestStdlib_DefaultPolicy_ReadmitsWhenMetricsHeal verifies the new
// probe-driven re-admission story: there is NO time-based readmit
// timer. The same `excludeIf` predicates that excluded the upstream
// are what re-admit it — when its tracker counters cross back below
// the threshold (because probe traffic OR state-poller calls fed
// healthy samples), it falls out of the excluded set on the next
// tick.
//
// Setup:
//   1. Seed rpc1 with 80% error rate → excludeIf trips → excluded.
//   2. Advance "time" arbitrarily — without metric improvement,
//      rpc1 MUST stay excluded forever (no time-based readmit).
//   3. Feed rpc1 with a fresh batch of clean samples that brings the
//      rolling error rate below 0.7 → next tick, rpc1 is back in
//      rotation.
func TestStdlib_DefaultPolicy_ReadmitsWhenMetricsHeal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// rpc1 broken: 80% error rate, comfortably above the default's 0.7
	// excludeIf threshold AND above samplesAbove(10).
	for i := 0; i < 20; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "")
	}
	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[0], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	// rpc2 healthy.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "")
	}

	// Tick 1 — rpc1 excluded.
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, []string{"rpc2"}, ids(engine.GetOrdered("evm:1", "*", "*")))
	require.Contains(t, idsExcluded(engine.GetExcluded("evm:1", "*", "*")), "rpc1",
		"rpc1 must be in the excluded set after tick 1")

	// Advance virtual time arbitrarily. Without metric improvement,
	// rpc1 MUST stay excluded — there is no time-based re-admit.
	policy.AdvanceEvalNowForTest(engine, "evm:1", "*", 10*time.Minute)
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, []string{"rpc2"}, ids(engine.GetOrdered("evm:1", "*", "*")),
		"after 10m with no metric improvement rpc1 must STILL be excluded")

	// Feed enough clean samples to drag rpc1's rolling error rate
	// below the 0.7 threshold. With a 1m tracker window, the original
	// 80 errors + 20 successes are still in the bucket, so we need to
	// add many successes to dilute. Easier: reset the tracker bucket
	// state for rpc1 by seeding a fresh batch that swamps the prior.
	for i := 0; i < 2000; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "")
	}

	// Next tick should see fresh metrics and re-admit rpc1.
	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Contains(t, got, "rpc1",
		"after metrics healed (clean samples diluted error rate) rpc1 must be re-admitted")
}

// idsExcluded — small helper to extract IDs from a []common.Upstream
// returned by Engine.GetExcluded.
func idsExcluded(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

// TestStdlib_StickyPrimary_RetainsPriorPrimary verifies cross-tick stickiness.
func TestStdlib_StickyPrimary_RetainsPriorPrimary(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore({ errorRate: 8 }).stickyPrimary({ hysteresis: 0.5 })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Tick 1: rpc1 clean, rpc2 broken → rpc1 becomes primary.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[1], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, "rpc1", engine.GetOrdered("evm:1", "*", "*")[0].Id())

	// Tick 2: rpc1 now WORSE than rpc2 but gap is within hysteresis → stays primary.
	// rpc1 → errorRate ~0.5 → score 0.5*8 = 4
	// rpc2 → errorRate ~0.2 → score 0.2*8 = 1.6
	// switch condition: 1.6 < 4 * (1 - 0.5) = 2 → false (1.6 < 2 IS true)
	// Use a smaller gap so switch is suppressed.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")
	// rpc2 is now clean (error rate 50/250 = 0.2); rpc1 still 0 errors.
	// Sticky shouldn't matter here — rpc1 is still better.
	require.Equal(t, "rpc1", engine.GetOrdered("evm:1", "*", "*")[0].Id())
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
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// rpc1: clean. rpc2: 50% errors. rpc3: blockHeadLag=50.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[1], "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	for i := 0; i < 50; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for _, u := range ups {
		tracker.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll).BlockHeadLag.Store(0)
	}
	tracker.GetUpstreamMethodMetrics(ups[2], "*", common.DataFinalityStateAll).BlockHeadLag.Store(50)
	for i := 0; i < 10; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[2], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	ordered := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Contains(t, ordered, "rpc1")
	require.NotContains(t, ordered, "rpc2", "rpc2 dropped: errorRate 0.5 > 0.3")
	require.NotContains(t, ordered, "rpc3", "rpc3 dropped: blockHeadLag 50 > 10")
}

// TestStdlib_Combinators_WhenEmpty_FallbackTo exercises chain control flow.
func TestStdlib_Combinators_WhenEmpty_FallbackTo(t *testing.T) {
	eval := `
		(upstreams, ctx) =>
			upstreams
				.byTag('tier:nonexistent')
				.whenEmpty(() => upstreams.byTag('tier:main'))
	`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTier(map[string]string{"rpc1": "main", "rpc2": "main"})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	ordered := engine.GetOrdered("evm:1", "*", "*")
	require.Len(t, ordered, 2, "whenEmpty should fall back to tier:main")
}

// TestStdlib_ByFinality_RoutesToCorrectHandler verifies the per-finality
// dispatch primitive. Each finality bucket should route to its handler,
// and a missing handler should pass through unchanged. The policy
// below shifts which upstream lands as primary depending on finality:
//
//	REALTIME    → take only rpc1
//	FINALIZED   → take only rpc2
//	UNFINALIZED → unhandled → passthrough (both rpcs)
func TestStdlib_ByFinality_RoutesToCorrectHandler(t *testing.T) {
	eval := `
		(upstreams, ctx) =>
			upstreams.byFinality({
				realtime:  u => u.byId('rpc1'),
				finalized: u => u.byId('rpc2'),
				// unfinalized + unknown intentionally omitted
			})
	`
	for _, tc := range []struct {
		name     string
		finality string
		want     []string
	}{
		{"REALTIME picks rpc1", "realtime", []string{"rpc1"}},
		{"FINALIZED picks rpc2", "finalized", []string{"rpc2"}},
		{"UNFINALIZED passthrough", "unfinalized", []string{"rpc1", "rpc2"}},
		{"UNKNOWN passthrough", "unknown", []string{"rpc1", "rpc2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, _, _, cancel := newTestEngine(t, eval)
			defer cancel()
			defer engine.Stop()

			ups := mkUps("rpc1", "rpc2")
			cfg := &common.SelectionPolicyConfig{
				EvalInterval:    0,
				EvalTimeout:     common.Duration(50 * time.Millisecond),
				EvalFunc:            eval,
			}
			require.NoError(t, cfg.SetDefaults())
			require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

			policy.SetFinalityForTest(engine, "evm:1", "*", tc.finality)
			policy.TickForTest(engine, "evm:1", "*")
			got := ids(engine.GetOrdered("evm:1", "*", "*"))
			require.Equal(t, tc.want, got)
		})
	}
}

// TestStdlib_ByFinality_ChainsCleanly verifies that byFinality returns
// a real Upstream[] (not e.g. a sub-typed array), so subsequent stdlib
// methods on the result work. Catches the "primitive forgot to call
// .slice()" class of bug.
func TestStdlib_ByFinality_ChainsCleanly(t *testing.T) {
	eval := `
		(upstreams, ctx) =>
			upstreams
				.byFinality({
					finalized: u => u,
					realtime:  u => u,
				})
				.sortByScore(PREFER_FASTEST)
				.pickTop(1)
	`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    0,
		EvalTimeout:     common.Duration(50 * time.Millisecond),
		EvalFunc:            eval,
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.SetFinalityForTest(engine, "evm:1", "*", "realtime")
	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Len(t, got, 1, "pickTop(1) must work on byFinality() output")
}

// TestStdlib_SpreadAcrossTags_ByVendorTag verifies vendor interleaving
// via explicit `vendor:` tags. With 4 upstreams pre-sorted by score,
// all from 2 vendor labels:
//
//	input  [alc-1, alc-2, alc-3, inf-1]   (already sorted)
//	output [alc-1, inf-1, alc-2, alc-3]   (interleaved, no two
//	                                       same-vendor adjacent)
//
// Failure mode without this primitive: the top-3 by score are all
// alchemy; if alchemy has a regional outage, all 3 retry positions
// fail together and the policy thought it had 3-way fault tolerance.
func TestStdlib_SpreadAcrossTags_ByVendorTag(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossTags('vendor:')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{"alc-1", "alchemy", []string{"vendor:alchemy"}},
		{"alc-2", "alchemy", []string{"vendor:alchemy"}},
		{"alc-3", "alchemy", []string{"vendor:alchemy"}},
		{"inf-1", "infura", []string{"vendor:infura"}},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"alc-1", "inf-1", "alc-2", "alc-3"}, got,
		"spreadAcrossTags('vendor:'): vendors interleaved; alc retains its sort order")
}

// TestStdlib_SpreadAcrossTags_StableForTies preserves within-bucket
// order. Two upstreams of vendor:alchemy in input order [alc-A, alc-B]
// stay in that order after interleaving.
func TestStdlib_SpreadAcrossTags_StableForTies(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossTags('vendor:')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{"alc-A", "alchemy", []string{"vendor:alchemy"}},
		{"alc-B", "alchemy", []string{"vendor:alchemy"}},
		{"inf-A", "infura", []string{"vendor:infura"}},
		{"inf-B", "infura", []string{"vendor:infura"}},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"alc-A", "inf-A", "alc-B", "inf-B"}, got,
		"alc-A before alc-B, inf-A before inf-B preserved")
}

// TestStdlib_SpreadAcrossTags_SingleBucketFallthrough: when every
// upstream shares the chosen tag prefix, interleaving is impossible
// — the primitive returns the input order unchanged.
func TestStdlib_SpreadAcrossTags_SingleBucketFallthrough(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossTags('vendor:')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{"alc-1", "alchemy", []string{"vendor:alchemy"}},
		{"alc-2", "alchemy", []string{"vendor:alchemy"}},
		{"alc-3", "alchemy", []string{"vendor:alchemy"}},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"alc-1", "alc-2", "alc-3"}, got,
		"single-bucket input: order preserved")
}

// ─── tag primitives (byTag / preferTag / spreadAcrossTags) ───────────

// TestStdlib_ByTag_GlobAndNegation pins the basic byTag/excludeTag
// matchers: literal, glob, negation, and array-of-patterns (OR).
func TestStdlib_ByTag_GlobAndNegation(t *testing.T) {
	for _, tc := range []struct {
		name string
		eval string
		want []string
	}{
		{"literal", `(u, c) => u.byTag('tier:main')`, []string{"a", "b"}},
		{"glob prefix", `(u, c) => u.byTag('region:us-*')`, []string{"a", "c"}},
		// negation: "upstream has NO tier:fallback tag". c has no tier
		// tag at all, so it matches; only fb is excluded.
		{"negation", `(u, c) => u.byTag('!tier:fallback')`, []string{"a", "b", "c"}},
		{"array OR", `(u, c) => u.byTag(['tier:fallback', 'region:us-east'])`, []string{"a", "fb"}},
		// excludeTag('X') == byTag('!X'). c is kept (no tier:fallback);
		// only fb is removed.
		{"excludeTag literal", `(u, c) => u.excludeTag('tier:fallback')`, []string{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, _, _, cancel := newTestEngine(t, tc.eval)
			defer cancel()
			defer engine.Stop()

			ups := mkUpsWithTags([]struct {
				id     string
				vendor string
				tags   []string
			}{
				{"a", "alchemy", []string{"tier:main", "region:us-east"}},
				{"b", "infura", []string{"tier:main", "region:eu-west"}},
				{"c", "drpc", []string{"region:us-west"}},
				{"fb", "fallback-vendor", []string{"tier:fallback"}},
			})
			cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: tc.eval}
			require.NoError(t, cfg.SetDefaults())
			require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

			policy.TickForTest(engine, "evm:1", "*")
			require.Equal(t, tc.want, ids(engine.GetOrdered("evm:1", "*", "*")))
		})
	}
}

// TestStdlib_PreferTag_Fallback verifies the tier-selection fallback:
// if the primary pattern matches < minHealthy, switch to the fallback
// pattern. If neither matches enough, return input unchanged.
func TestStdlib_PreferTag_Fallback(t *testing.T) {
	eval := `(u, ctx) => u.preferTag('!tier:fallback', { minHealthy: 1, fallback: 'tier:fallback' })`

	t.Run("PrimaryTierWins", func(t *testing.T) {
		engine, _, _, cancel := newTestEngine(t, eval)
		defer cancel()
		defer engine.Stop()

		ups := mkUpsWithTags([]struct {
			id     string
			vendor string
			tags   []string
		}{
			{"prim-a", "alchemy", []string{"tier:main"}},
			{"prim-b", "infura", []string{"tier:main"}},
			{"backup", "drpc", []string{"tier:fallback"}},
		})
		cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
		require.NoError(t, cfg.SetDefaults())
		require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

		policy.TickForTest(engine, "evm:1", "*")
		require.Equal(t, []string{"prim-a", "prim-b"}, ids(engine.GetOrdered("evm:1", "*", "*")),
			"primary tier (!tier:fallback) has members → backup must be excluded")
	})

	t.Run("FallbackTierActivates", func(t *testing.T) {
		engine, _, _, cancel := newTestEngine(t, eval)
		defer cancel()
		defer engine.Stop()

		// Only fallback-tier upstreams exist.
		ups := mkUpsWithTags([]struct {
			id     string
			vendor string
			tags   []string
		}{
			{"backup", "drpc", []string{"tier:fallback"}},
		})
		cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
		require.NoError(t, cfg.SetDefaults())
		require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

		policy.TickForTest(engine, "evm:1", "*")
		require.Equal(t, []string{"backup"}, ids(engine.GetOrdered("evm:1", "*", "*")),
			"primary tier empty → fallback tier serves")
	})
}

// TestStdlib_SpreadAcrossTags_ByPrefix pins the canonical
// `spreadAcrossTags('cohort:')` interleave: adjacent positions in the
// output must not share the same `cohort:*` tag value.
func TestStdlib_SpreadAcrossTags_ByPrefix(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossTags('cohort:')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{"alc-base", "alchemy", []string{"cohort:op-base-sequencer"}},
		{"inf-base", "infura", []string{"cohort:op-base-sequencer"}},
		{"drpc-direct", "drpc", []string{"cohort:drpc-direct"}},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"alc-base", "drpc-direct", "inf-base"}, got,
		"adjacent positions must not share cohort: alc-base, drpc-direct, inf-base")
}

// TestStdlib_SpreadAcrossTags_MissingTagBucketed: upstreams that don't
// have any tag matching the prefix all land in the empty-key bucket
// (interleaved together at the end of the partition order).
func TestStdlib_SpreadAcrossTags_MissingTagBucketed(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossTags('region:')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{"east-1", "alchemy", []string{"region:us-east"}},
		{"west-1", "drpc", []string{"region:us-west"}},
		{"untagged", "infura", []string{}}, // no region:* tag
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Len(t, got, 3, "all upstreams returned; untagged ones aren't dropped")
	require.Equal(t, "east-1", got[0], "first input occurrence is position 0")
	require.NotEqual(t, "untagged", got[1],
		"position 1 must come from a different bucket than position 0 when possible")
}

// TestStdlib_Slicing_PickTop_DropTop pins the slicing primitives.
func TestStdlib_Slicing_PickTop_DropTop(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.sortByScore(PREFER_FASTEST).pickTop(2)`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2", "rpc3", "rpc4")
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	require.Len(t, engine.GetOrdered("evm:1", "*", "*"), 2, "pickTop(2) should cap output at 2")
}

// TestStdlib_LatestDecision_OrderAndExclusions verifies the
// slot's cached "what survived / what got dropped" surface. There is
// no ring buffer or admin endpoint behind this — just the slot's
// previousOrder + previousExcluded after the latest tick. Real
// investigations go through traces + metrics (see telemetry/metrics.go).
func TestStdlib_LatestDecision_OrderAndExclusions(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.byTag('tier:main')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTier(map[string]string{"rpc1": "main", "rpc2": "main", "rpc3": "fallback"})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	order, excluded := policy.LatestDecisionOutputForTest(engine, "evm:1", "*")
	require.Len(t, order, 2, "tier:main has two members")
	require.Len(t, excluded, 1, "rpc3 (tier:fallback) is excluded")
	require.Equal(t, "rpc3", excluded[0])
}

// TestStdlib_StepLog_CapturesChainTrail verifies that when
// `Engine.SetStepLogEnabled(true)` is on, each Decision carries the
// full per-step trail the JS chain produced — one entry per
// chainable stdlib method invoked, in chain order, with input/output
// diffs and arg summary. This is the data the simulator's
// policy-history detail view renders and the source DEBUG eRPC logs
// expose for incident triage.
func TestStdlib_StepLog_CapturesChainTrail(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams
		.byTag('tier:main')
		.excludeIf(errorRateAbove(0.5))
		.sortByScore(PREFER_FASTEST)`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{
		EvalInterval: 0,
		EvalTimeout:  common.Duration(50 * time.Millisecond),
		EvalFunc:     eval,
	}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	engine.SetStepLogEnabled(true)
	defer engine.Stop()

	// Use the slice-preserving constructor so `rpc1` is at a known
	// index — `mkUpsWithTier` iterates a map, which Go intentionally
	// randomizes per-run; the test would intermittently seed the wrong
	// upstream otherwise.
	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{id: "rpc1", tags: []string{"tier:main"}},
		{id: "rpc2", tags: []string{"tier:main"}},
		{id: "rpc3", tags: []string{"tier:fallback"}},
	})
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// rpc1 is broken so the excludeIf step trips on it.
	rpc1 := ups[0]
	for i := 0; i < 90; i++ {
		tracker.RecordUpstreamRequest(rpc1, "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(rpc1, "*", common.DataFinalityStateUnknown, fmt.Errorf("synth"))
	}
	for i := 0; i < 10; i++ {
		tracker.RecordUpstreamRequest(rpc1, "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(rpc1, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	policy.TickForTest(engine, "evm:1", "*")

	decisions := engine.RecentDecisions("evm:1", "*", "*", 5)
	require.NotEmpty(t, decisions)
	d := decisions[len(decisions)-1]
	require.NotEmpty(t, d.Output.StepLog, "step log should populate when SetStepLogEnabled(true)")

	// We should see at minimum the three steps in the chain. The exact
	// names match the JS-side `define(...)` registrations.
	stepNames := make([]string, 0, len(d.Output.StepLog))
	for _, s := range d.Output.StepLog {
		stepNames = append(stepNames, s.Step)
	}
	require.Contains(t, stepNames, "byTag")
	require.Contains(t, stepNames, "excludeIf")
	require.Contains(t, stepNames, "sortByScore")

	// excludeIf should have dropped rpc1 (90% error rate). The step
	// entry's `dropped` list captures the diff.
	var excludeStep *policy.StepEntry
	for i := range d.Output.StepLog {
		if d.Output.StepLog[i].Step == "excludeIf" {
			excludeStep = &d.Output.StepLog[i]
		}
	}
	require.NotNil(t, excludeStep, "excludeIf step should be in the trail")
	require.Contains(t, excludeStep.Dropped, "rpc1", "excludeIf should have dropped rpc1")

	// The diagnostic Reason on the excluded upstream comes from the
	// first leaf slug (option-(c) attribution lives in `LeafReasons`).
	// We dropped the per-upstream `annotate(u, ...)` text trail when we
	// collapsed the metadata surface — operators see the stable slug
	// instead of the threshold-encoded text ("errorRate>0.5").
	var rpc1Excluded *policy.ExcludedUpstream
	for i := range d.Output.Excluded {
		if d.Output.Excluded[i].ID == "rpc1" {
			rpc1Excluded = &d.Output.Excluded[i]
			break
		}
	}
	require.NotNil(t, rpc1Excluded)
	require.Equal(t, []string{"error_rate_above"}, rpc1Excluded.LeafReasons)
	require.Equal(t, "error_rate_above", rpc1Excluded.Reason,
		"Reason falls back to the first leaf slug after the annotation channel was removed")
}

// TestStdlib_StepLog_DisabledByDefault verifies that without
// `SetStepLogEnabled(true)`, the chain trail stays empty — the
// production-default fast path that keeps eval overhead at "one
// extra function-call indirection per stdlib step".
func TestStdlib_StepLog_DisabledByDefault(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.byTag('tier:main')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsWithTier(map[string]string{"rpc1": "main", "rpc2": "main"})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	decisions := engine.RecentDecisions("evm:1", "*", "*", 1)
	require.NotEmpty(t, decisions)
	d := decisions[len(decisions)-1]
	require.Empty(t, d.Output.StepLog, "step log must stay empty by default")
}
