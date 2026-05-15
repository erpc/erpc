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
// The deprecated `group` / `cohort` fields drive buildJSUpstreams' back-
// compat path; the canonical `tags` field exercises the new code path.
type fakeUpstream struct {
	id     string
	group  string
	vendor string
	cohort string
	tags   []string
}

func (f *fakeUpstream) Id() string         { return f.id }
func (f *fakeUpstream) VendorName() string { return f.vendor }
func (f *fakeUpstream) NetworkId() string  { return "evm:1" }
func (f *fakeUpstream) NetworkLabel() string {
	return "evm:1"
}
func (f *fakeUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: f.id, Group: f.group, Cohort: f.cohort, Tags: f.tags}
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

// mkUpsRich constructs N upstreams with explicit (id, group, vendor,
// cohort). Slices must be the same length; missing fields default to
// id-derived placeholders. The ORDER of `ids` is preserved in the
// returned slice (unlike `mkUpsWithGroups`'s map-iteration order).
func mkUpsRich(specs []struct{ id, group, vendor, cohort string }) []common.Upstream {
	out := make([]common.Upstream, len(specs))
	for i, s := range specs {
		v := s.vendor
		if v == "" {
			v = "v" + s.id
		}
		out[i] = &fakeUpstream{id: s.id, group: s.group, vendor: v, cohort: s.cohort}
	}
	return out
}

// mkUpsWithTags is the tags-canonical sibling of mkUpsRich. Each spec
// declares an id, a vendor, and a tags slice. Use when the test wants
// to exercise the new `byTag` / `preferTag` / `spreadAcrossTags` path
// without touching the deprecated Group / Cohort fields.
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
// embedded `internal/policy/default_policy.js`. Primary tier is matched as
// `group !== 'fallback'`; rpc1 (group='default') is primary, rpc2 (group=
// 'fallback') is the second tier.
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
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// rpc1: 90% error rate (broken). rpc2: clean.
	for i := 0; i < 90; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	for i := 0; i < 10; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	ordered := ids(engine.GetOrdered("evm:1", "*"))
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
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Both upstreams broken.
	for _, u := range ups {
		for i := 0; i < 90; i++ {
			tracker.RecordUpstreamRequest(u, "*")
			tracker.RecordUpstreamFailure(u, "*", fmt.Errorf("synth"))
		}
		for i := 0; i < 10; i++ {
			tracker.RecordUpstreamRequest(u, "*")
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}

	policy.TickForTest(engine, "evm:1", "*")
	ordered := engine.GetOrdered("evm:1", "*")
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
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)
	defer engine.Stop()

	// All upstreams in 'fallback' group → primary tier (`!fallback`) is empty.
	ups := mkUpsWithGroups(map[string]string{"rpc1": "fallback", "rpc2": "fallback"})
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	ordered := engine.GetOrdered("evm:1", "*")
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
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Drive 100 clean requests for both; only rpc1 is lagging.
	for _, u := range ups {
		for i := 0; i < 100; i++ {
			tracker.RecordUpstreamRequest(u, "*")
			tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}
	// rpc1 lags 30 blocks behind tip (> 16 default threshold).
	tracker.GetUpstreamMethodMetrics(ups[0], "*").BlockHeadLag.Store(30)

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
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
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// rpc1: 100 requests, all SLOW (12s) → p95 ≈ 12s > 10s threshold.
	// rpc2: 100 requests, all fast (10ms) → p95 ≈ 10ms.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamDuration(ups[0], "*", 12*time.Second, true, "none", common.DataFinalityStateUnknown, "n/a")
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
	require.Equal(t, []string{"rpc2"}, got,
		"rpc1 has 0 errors but p95=12s > 10s → must be excluded by keepHealthy(maxP95Ms:10_000)")
}

// TestStdlib_DefaultPolicy_StickyPrimary_OnlyForRealtime (G3):
// the default's `stickyPrimary` is wrapped in
// `.if(ctx.finality === REALTIME, ...)`. On REALTIME requests, sticky
// behavior holds the primary across ticks. On FINALIZED requests, the
// challenger immediately takes over when scoring favors it.
//
// Setup: tick 1 with rpc1 clean and rpc2 broken → rpc1 is primary.
// Then make rpc1 worse than rpc2 (rpc2 already accumulated errors that
// kept it out; flip the script so rpc1 errors heavily and rpc2 is now
// clean). Under REALTIME the 30s minSwitchInterval should hold rpc1
// as primary. Under FINALIZED, no stickiness → rpc2 should take over.
func TestStdlib_DefaultPolicy_StickyPrimary_OnlyForRealtime(t *testing.T) {
	// Test the JS branching directly with a frozen ctx, so we don't need
	// to thread finality through the request path. Two evals — same
	// default body — driven against ctx.finality=REALTIME and
	// ctx.finality=FINALIZED with identical metrics and previousOrder.
	// Asserts that REALTIME respects stickiness, FINALIZED does not.
	defaultPolicy := policy.DefaultPolicySource()

	mkEngine := func(finality string) (*policy.Engine, []common.Upstream, *health.Tracker, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		logger := zerolog.Nop()
		tracker := health.NewTracker(&logger, "p1", time.Minute)
		cfg := &common.SelectionPolicyConfig{
			Eval: defaultPolicy,
		}
		require.NoError(t, cfg.SetDefaults())
		engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)

		ups := mkUps("rpc1", "rpc2")
		require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

		// Both clean enough to pass keepHealthy, but rpc1 is slightly
		// preferred. The challenger advantage we'll later introduce is
		// large (rpc1 errors heavily) so any non-sticky policy switches.
		for _, u := range ups {
			for i := 0; i < 100; i++ {
				tracker.RecordUpstreamRequest(u, "*")
				tracker.RecordUpstreamDuration(u, "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
			}
		}
		_ = finality // ctx.finality is set via policy.OverrideFinalityForTest below
		return engine, ups, tracker, cancel
	}

	for _, tc := range []struct {
		name           string
		finality       string
		expectPrimary  string
		expectSwitched bool
	}{
		{"REALTIME holds prior primary", "realtime", "rpc1", false},
		{"FINALIZED switches immediately", "finalized", "rpc2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, ups, tracker, cancel := mkEngine(tc.finality)
			defer cancel()
			defer engine.Stop()

			// Force finality on the engine's eval ctx so the .if() branches.
			policy.SetFinalityForTest(engine, "evm:1", "*", tc.finality)

			// Tick 1 — equal metrics → rpc1 wins by id tiebreak. Sticky
			// state now records rpc1 as primary at this timestamp.
			policy.TickForTest(engine, "evm:1", "*")
			first := ids(engine.GetOrdered("evm:1", "*"))
			require.Equal(t, "rpc1", first[0], "tick 1: rpc1 wins by id tiebreak")

			// Make rpc1 clearly worse than rpc2 (challenger advantage > hysteresis).
			for i := 0; i < 30; i++ {
				tracker.RecordUpstreamRequest(ups[0], "*")
				tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
			}

			// Tick 2 — under REALTIME, the 30s minSwitchInterval keeps
			// rpc1 as primary. Under FINALIZED, no stickiness, switch.
			policy.TickForTest(engine, "evm:1", "*")
			second := ids(engine.GetOrdered("evm:1", "*"))
			require.GreaterOrEqual(t, len(second), 1)
			require.Equal(t, tc.expectPrimary, second[0],
				"tick 2 primary under finality=%s should be %s", tc.finality, tc.expectPrimary)
		})
	}
}

// TestStdlib_DefaultPolicy_ProbeReadmitsAt90s (G4): an upstream
// excluded by the filter is re-admitted at ~90s, not the prior 5m.
// Verifies the default's `probeExcluded({ reAdmitAfter: '90s' })`.
//
// Setup: rpc1 errors hard, gets filtered out. We drive enough ticks at
// frozen monotonic time to verify it isn't re-admitted in the first 89s
// and IS re-admitted at 91s.
func TestStdlib_DefaultPolicy_ProbeReadmitsAt90s(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install)
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// rpc1 broken; rpc2 healthy.
	for i := 0; i < 90; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamFailure(ups[0], "*", fmt.Errorf("synth"))
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	// Tick 1 — rpc1 excluded, excludedSince[rpc1] = now.
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, []string{"rpc2"}, ids(engine.GetOrdered("evm:1", "*")))

	// Advance virtual time 60s. probeExcluded(reAdmitAfter:'90s') must
	// NOT re-admit yet (need > 90s).
	policy.AdvanceEvalNowForTest(engine, "evm:1", "*", 60*time.Second)
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, []string{"rpc2"}, ids(engine.GetOrdered("evm:1", "*")),
		"at +60s rpc1 must still be excluded (reAdmitAfter='90s')")

	// Advance to +120s total (60s since registration + 60s more = 120s).
	policy.AdvanceEvalNowForTest(engine, "evm:1", "*", 60*time.Second)
	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
	require.Contains(t, got, "rpc1",
		"at +120s rpc1 must be re-admitted (reAdmitAfter='90s' elapsed)")
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
				DecisionHistory: common.Duration(time.Minute),
				Eval:            eval,
			}
			require.NoError(t, cfg.SetDefaults())
			require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

			policy.SetFinalityForTest(engine, "evm:1", "*", tc.finality)
			policy.TickForTest(engine, "evm:1", "*")
			got := ids(engine.GetOrdered("evm:1", "*"))
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
				.sortByScore(BALANCED)
				.pickTop(1)
	`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUps("rpc1", "rpc2")
	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    0,
		EvalTimeout:     common.Duration(50 * time.Millisecond),
		DecisionHistory: common.Duration(time.Minute),
		Eval:            eval,
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.SetFinalityForTest(engine, "evm:1", "*", "realtime")
	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
	require.Len(t, got, 1, "pickTop(1) must work on byFinality() output")
}

// TestStdlib_SpreadAcrossGroups_ByVendor verifies cohort interleaving.
// With 4 upstreams pre-sorted by score, all from 2 vendors:
//
//	input  [alc-1, alc-2, alc-3, inf-1]   (already sorted)
//	output [alc-1, inf-1, alc-2, alc-3]   (interleaved, no two
//	                                       same-vendor adjacent)
//
// Failure mode without this primitive: the top-3 by score are all
// alchemy; if alchemy has a regional outage, all 3 retry positions
// fail together and the policy thought it had 3-way fault tolerance.
func TestStdlib_SpreadAcrossGroups_ByVendor(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossGroups({ by: 'vendor' })`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsRich([]struct{ id, group, vendor, cohort string }{
		{"alc-1", "main", "alchemy", ""},
		{"alc-2", "main", "alchemy", ""},
		{"alc-3", "main", "alchemy", ""},
		{"inf-1", "main", "infura", ""},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
	require.Equal(t, []string{"alc-1", "inf-1", "alc-2", "alc-3"}, got,
		"spreadAcrossGroups: alchemy and infura interleaved; alc retains its sort order")
}

// TestStdlib_SpreadAcrossGroups_StableForTies preserves within-bucket
// order. Two upstreams of vendor=alc in input order [alc-A, alc-B]
// stay in that order after interleaving.
func TestStdlib_SpreadAcrossGroups_StableForTies(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossGroups({ by: 'vendor' })`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsRich([]struct{ id, group, vendor, cohort string }{
		{"alc-A", "main", "alchemy", ""},
		{"alc-B", "main", "alchemy", ""},
		{"inf-A", "main", "infura", ""},
		{"inf-B", "main", "infura", ""},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
	require.Equal(t, []string{"alc-A", "inf-A", "alc-B", "inf-B"}, got,
		"alc-A before alc-B, inf-A before inf-B preserved")
}

// TestStdlib_SpreadAcrossGroups_ByCohort verifies the explicit
// `cohort` field on UpstreamConfig. Two upstreams in cohort
// 'op-base-sequencer' (Alchemy + Infura both routing through the
// same L2 sequencer) should be interleaved with a non-shared
// upstream rather than ranked adjacent.
func TestStdlib_SpreadAcrossGroups_ByCohort(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossGroups({ by: 'cohort' })`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsRich([]struct{ id, group, vendor, cohort string }{
		{"alc-base", "main", "alchemy", "op-base-sequencer"},
		{"inf-base", "main", "infura", "op-base-sequencer"},
		{"drpc-direct", "main", "drpc", "drpc-direct"},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
	require.Equal(t, []string{"alc-base", "drpc-direct", "inf-base"}, got,
		"different cohorts adjacent: alc-base, drpc-direct, inf-base")
}

// TestStdlib_SpreadAcrossGroups_SingleBucketFallthrough: when every
// upstream shares the chosen key, interleaving is impossible — the
// primitive returns the input order unchanged.
func TestStdlib_SpreadAcrossGroups_SingleBucketFallthrough(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.spreadAcrossGroups({ by: 'vendor' })`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	ups := mkUpsRich([]struct{ id, group, vendor, cohort string }{
		{"alc-1", "main", "alchemy", ""},
		{"alc-2", "main", "alchemy", ""},
		{"alc-3", "main", "alchemy", ""},
	})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
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
			cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: tc.eval}
			require.NoError(t, cfg.SetDefaults())
			require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

			policy.TickForTest(engine, "evm:1", "*")
			require.Equal(t, tc.want, ids(engine.GetOrdered("evm:1", "*")))
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
		cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
		require.NoError(t, cfg.SetDefaults())
		require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

		policy.TickForTest(engine, "evm:1", "*")
		require.Equal(t, []string{"prim-a", "prim-b"}, ids(engine.GetOrdered("evm:1", "*")),
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
		cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
		require.NoError(t, cfg.SetDefaults())
		require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

		policy.TickForTest(engine, "evm:1", "*")
		require.Equal(t, []string{"backup"}, ids(engine.GetOrdered("evm:1", "*")),
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
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
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
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
	require.Len(t, got, 3, "all upstreams returned; untagged ones aren't dropped")
	require.Equal(t, "east-1", got[0], "first input occurrence is position 0")
	require.NotEqual(t, "untagged", got[1],
		"position 1 must come from a different bucket than position 0 when possible")
}

// TestStdlib_TagsViaDeprecatedFields verifies the back-compat path: Go
// code that programmatically sets the deprecated `Group` / `Cohort`
// fields (instead of `Tags`) still works through the stdlib.
//
// Without this back-compat layer, `byGroup('main')` would return an
// empty set whenever a test fixture is built with `group: "main"`
// instead of `tags: ["tier:main"]`.
func TestStdlib_TagsViaDeprecatedFields(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams.byTag('tier:main')`
	engine, _, _, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	// mkUpsWithGroups sets the deprecated `group` field directly.
	ups := mkUpsWithGroups(map[string]string{"alc": "main", "drpc": "fallback"})
	cfg := &common.SelectionPolicyConfig{EvalInterval: 0, EvalTimeout: common.Duration(50 * time.Millisecond), DecisionHistory: common.Duration(time.Minute), Eval: eval}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*"))
	require.Equal(t, []string{"alc"}, got,
		"byTag('tier:main') must match upstream with deprecated group:main "+
			"(buildJSUpstreams promotes it to a tier:main tag)")
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
