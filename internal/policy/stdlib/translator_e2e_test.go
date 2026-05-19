// Package stdlib_test — end-to-end tests of the legacy translator output.
//
// These tests build a `*common.NetworkConfig` exactly as the legacy
// translator would after consuming legacy YAML, then run the synthesized
// `selectionPolicy.eval` through the real engine + stdlib. The goal is
// to prove the four cells of the 2x2 matrix (selectionPolicy.evalFunction
// yes/no × upstream.routing.scoreMultipliers yes/no) all produce a
// working policy — not just one whose source contains the right
// substrings.
package stdlib_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/common/legacy"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// runTranslatedPolicy is shared boilerplate: translate `prj/legacyUps/nws`
// into a new-shape config, compile + register the eval, and return the
// engine for the caller to drive ticks against.
func runTranslatedPolicy(
	t *testing.T,
	prj legacy.WidenedProject,
	upCfgs []*common.UpstreamConfig,
	legacyUps []legacy.WidenedUpstream,
	nws []legacy.WidenedNetwork,
	ups []common.Upstream,
) (*policy.Engine, *health.Tracker, *common.SelectionPolicyConfig, context.CancelFunc) {
	t.Helper()

	nwCfgs := []*common.NetworkConfig{{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 1},
	}}
	_, err := legacy.Translate(prj, legacyUps, upCfgs, nws, nwCfgs)
	require.NoError(t, err, "legacy.Translate must succeed")
	require.NotNil(t, nwCfgs[0].SelectionPolicy, "translator must synthesize a selectionPolicy")

	cfg := nwCfgs[0].SelectionPolicy
	cfg.EvalInterval = 0 // frozen — tests drive manual ticks
	cfg.EvalTimeout = common.Duration(200 * time.Millisecond)
	require.NoError(t, cfg.SetDefaults(), "SetDefaults must compile the synthesized eval")
	require.NotNil(t, cfg.CompiledProgram, "synthesized eval must compile")

	ctx, cancel := context.WithCancel(context.Background())
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// RegisterNetwork synchronously runs an initial tick so the cache is
	// populated before the first request. With empty metrics that initial
	// tick picks the alphabetically-first upstream as primary AND records
	// `lastSwitchAt`. The translator emits `stickyPrimary({minSwitchInterval:
	// '30s'})` and the new (spec-correct) sticky impl enforces that cooldown,
	// so the test's subsequent TickForTest would be held by sticky regardless
	// of score gap. Push virtual time past the cooldown so each test
	// observes the post-cooldown order it wants to assert.
	policy.AdvanceEvalNowForTest(engine, "evm:1", "*", 31*time.Second)
	return engine, tracker, cfg, cancel
}

// --------------------------------------------------------------------
// Cell (NO eval × NO multipliers)
//
// Pure legacy `routingStrategy: score-based` with no per-upstream
// overrides. The translated policy is sortByScore(PREFER_FASTEST) +
// stickyPrimary. Clean upstreams should outrank broken ones.
// --------------------------------------------------------------------
func TestTranslator_E2E_ScoreBasedOnly(t *testing.T) {
	prj := legacy.WidenedProject{RoutingStrategy: "score-based"}
	upCfgs := []*common.UpstreamConfig{{Id: "rpc1"}, {Id: "rpc2"}}
	ups := mkUps("rpc1", "rpc2")
	legacyUps := []legacy.WidenedUpstream{{}, {}}

	engine, tracker, _, cancel := runTranslatedPolicy(t, prj, upCfgs, legacyUps, []legacy.WidenedNetwork{{}}, ups)
	defer cancel()
	defer engine.Stop()

	// rpc1 broken, rpc2 clean.
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
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"rpc2", "rpc1"}, got,
		"score-based default: clean upstream should outrank broken one")
}

// --------------------------------------------------------------------
// Cell (NO eval × YES multipliers)
//
// `scoreMultipliers` shifts the per-upstream weights. We give rpc1 high
// error-rate weight so a small error-rate gap suffices to swing the
// order. Without the multipliers, the gap wouldn't matter under PREFER_FASTEST.
// --------------------------------------------------------------------
func TestTranslator_E2E_ScoreMultipliersOnly(t *testing.T) {
	upCfgs := []*common.UpstreamConfig{{Id: "fast"}, {Id: "slow"}}
	legacyUps := []legacy.WidenedUpstream{
		// `fast` upstream — heavy latency penalty (it's expected to be fast).
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			ErrorRate: 4, RespLatency: 20,
		}}, 0),
		// `slow` upstream — light latency penalty, heavy error penalty.
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			ErrorRate: 4, RespLatency: 2,
		}}, 0),
	}
	ups := mkUps("fast", "slow")

	engine, tracker, _, cancel := runTranslatedPolicy(t, legacy.WidenedProject{}, upCfgs, legacyUps, []legacy.WidenedNetwork{{}}, ups)
	defer cancel()
	defer engine.Stop()

	// Both upstreams clean, but `fast` is faster.
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[0], "*")
		tracker.RecordUpstreamDuration(ups[0], "*", 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	for i := 0; i < 100; i++ {
		tracker.RecordUpstreamRequest(ups[1], "*")
		tracker.RecordUpstreamDuration(ups[1], "*", 200*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"fast", "slow"}, got,
		"per-upstream multipliers: latency-weighted sort should put `fast` first")
}

// --------------------------------------------------------------------
// Cell (YES eval × NO multipliers)
//
// User wrote a custom `evalFunction` that filters by errorRate. The
// wrapped legacy fn should still run and exclude high-error upstreams.
// --------------------------------------------------------------------
func TestTranslator_E2E_EvalFunctionOnly(t *testing.T) {
	legacyFn := "(upstreams, method) => upstreams.filter(u => u.metrics.errorRate < 0.5)"
	upCfgs := []*common.UpstreamConfig{{Id: "rpc1"}, {Id: "rpc2"}}
	legacyUps := []legacy.WidenedUpstream{{}, {}}
	ups := mkUps("rpc1", "rpc2")
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction: legacyFn,
		}),
	}}

	engine, tracker, _, cancel := runTranslatedPolicy(t, legacy.WidenedProject{}, upCfgs, legacyUps, nws, ups)
	defer cancel()
	defer engine.Stop()

	// rpc1 broken (>50% errors), rpc2 clean.
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
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"rpc2"}, got,
		"wrapped legacy fn should filter out the high-error upstream")
}

// --------------------------------------------------------------------
// Cell (YES eval × YES multipliers)
//
// User wrote both an evalFunction and per-upstream multipliers. The
// translator must pre-sort by the multipliers BEFORE invoking the
// legacy fn — otherwise the multiplier configuration is silently lost.
//
// Test setup:
//   - 3 upstreams; all clean (no error-rate filter trip).
//   - Multipliers: `slow` upstream gets a heavy latency penalty;
//     others get a light one. Latency-based ordering would put `fast`
//     first, then `mid`, then `slow`.
//   - Legacy fn: `(ups, m) => ups.slice(0, 2)` — keeps only the first
//     two upstreams in WHATEVER order it received them.
//
// If the translator pre-sorts (correct), the legacy fn sees
//
//	[fast, mid, slow]
//
// and keeps [fast, mid]. If it doesn't pre-sort, the legacy fn sees the
// raw registration order [slow, fast, mid] and keeps [slow, fast] —
// which loses the multiplier effect.
// --------------------------------------------------------------------
func TestTranslator_E2E_EvalFunction_PlusMultipliers_PreservesOrder(t *testing.T) {
	legacyFn := "(upstreams, method) => upstreams.slice(0, 2)"
	upCfgs := []*common.UpstreamConfig{
		{Id: "slow"}, // intentionally registered first to expose the bug
		{Id: "fast"},
		{Id: "mid"},
	}
	legacyUps := []legacy.WidenedUpstream{
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			RespLatency: 30,
		}}, 0),
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			RespLatency: 1,
		}}, 0),
		legacy.WidenedUpstreamForTest([]legacy.ScoreMultiplierSnapshot{{
			Network: "*", Method: "*",
			RespLatency: 5,
		}}, 0),
	}
	ups := mkUps("slow", "fast", "mid")
	nws := []legacy.WidenedNetwork{{
		SelectionPolicy: legacy.WidenedSelectionPolicyForTest(legacy.LegacyPolicySnapshot{
			EvalFunction: legacyFn,
		}),
	}}

	engine, tracker, _, cancel := runTranslatedPolicy(t, legacy.WidenedProject{}, upCfgs, legacyUps, nws, ups)
	defer cancel()
	defer engine.Stop()

	// Drive latencies so the multipliers have something to penalize.
	// slow: 300 ms, mid: 100 ms, fast: 10 ms.
	latencies := map[string]time.Duration{
		"slow": 300 * time.Millisecond,
		"mid":  100 * time.Millisecond,
		"fast": 10 * time.Millisecond,
	}
	for _, u := range ups {
		for i := 0; i < 100; i++ {
			tracker.RecordUpstreamRequest(u, "*")
			tracker.RecordUpstreamDuration(u, "*", latencies[u.Id()], true, "none", common.DataFinalityStateUnknown, "n/a")
		}
	}

	policy.TickForTest(engine, "evm:1", "*")
	got := ids(engine.GetOrdered("evm:1", "*", "*"))
	require.Equal(t, []string{"fast", "mid"}, got,
		"pre-sort by multipliers (fast < mid < slow), then legacy slice(0,2) keeps fast & mid")
}

// --------------------------------------------------------------------
// Bonus: round-robin path doesn't care about multipliers, but should
// still produce all upstreams in a rotating order. Pinning here so we
// catch the "translator silently fell off the round-robin path" class
// of regression.
// --------------------------------------------------------------------
func TestTranslator_E2E_RoundRobin(t *testing.T) {
	prj := legacy.WidenedProject{RoutingStrategy: "round-robin"}
	upCfgs := []*common.UpstreamConfig{{Id: "a"}, {Id: "b"}, {Id: "c"}}
	legacyUps := []legacy.WidenedUpstream{{}, {}, {}}
	ups := mkUps("a", "b", "c")

	engine, _, _, cancel := runTranslatedPolicy(t, prj, upCfgs, legacyUps, []legacy.WidenedNetwork{{}}, ups)
	defer cancel()
	defer engine.Stop()

	seenPrimary := map[string]bool{}
	for i := 0; i < 6; i++ {
		policy.TickForTest(engine, "evm:1", "*")
		ordered := engine.GetOrdered("evm:1", "*", "*")
		require.Len(t, ordered, 3)
		seenPrimary[ordered[0].Id()] = true
	}
	require.Len(t, seenPrimary, 3, "round-robin: every upstream should rotate into the primary slot")
}
