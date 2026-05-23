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

// makeMethodSlotEngine wires an engine with evalPerMethod=true so the
// test can exercise per-(network, method) slots concretely. The eval
// applies stickyPrimary with the caller-supplied scope; the test then
// ticks two method slots and asserts on cross-slot agreement.
func makeMethodSlotEngine(t *testing.T, eval string) (*policy.Engine, *health.Tracker, []common.Upstream, *common.SelectionPolicyConfig, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	cfg := &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(0), // frozen, manual ticks
		EvalTimeout:  common.Duration(50 * time.Millisecond),
		EvalFunc:     eval,
		EvalScope:    common.EvalScopeNetworkMethod,
	}
	require.NoError(t, cfg.SetDefaults())
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	ups := mkUps("alchemy", "drpc", "quicknode")
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))
	return engine, tracker, ups, cfg, cancel
}

// recordAcrossMethods seeds the tracker so the wildcard "*" aggregate
// for each upstream reflects a consistent global picture: `alchemy` is
// the wildcard-best (low errors + low latency), `drpc` is decent, and
// `quicknode` is poor. Per-method metrics may diverge — that's the
// whole point: cross-method sticky must use the wildcard aggregate to
// agree across slots.
func recordAcrossMethods(tracker *health.Tracker, ups []common.Upstream, methods []string) {
	// Distribution per upstream — across ALL methods aggregated. The
	// tracker writes BOTH (upstream, method) and (upstream, "*") on
	// each Record* call (getUpsKeys), so calling each method N times
	// populates both views uniformly.
	for _, m := range methods {
		// alchemy: 100% clean, 10ms.
		for i := 0; i < 100; i++ {
			tracker.RecordUpstreamRequest(ups[0], m)
			tracker.RecordUpstreamDuration(ups[0], m, 10*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
		// drpc: 95% clean, 40ms — decent but worse than alchemy.
		for i := 0; i < 95; i++ {
			tracker.RecordUpstreamRequest(ups[1], m)
			tracker.RecordUpstreamDuration(ups[1], m, 40*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
		for i := 0; i < 5; i++ {
			tracker.RecordUpstreamRequest(ups[1], m)
			tracker.RecordUpstreamFailure(ups[1], m, fmt.Errorf("synth"))
		}
		// quicknode: 50% errors, 200ms — poor.
		for i := 0; i < 50; i++ {
			tracker.RecordUpstreamRequest(ups[2], m)
			tracker.RecordUpstreamDuration(ups[2], m, 200*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
		}
		for i := 0; i < 50; i++ {
			tracker.RecordUpstreamRequest(ups[2], m)
			tracker.RecordUpstreamFailure(ups[2], m, fmt.Errorf("synth"))
		}
	}
}

// TestStickyScope_NetworkScope_ConvergesAcrossMethods verifies the
// core promise of `stickyPrimary({scope: NETWORK})`: two independent
// per-(network, method) slots ticked back-to-back agree on the same
// primary — picked from the wildcard `(upstream, "*")` aggregate they
// both see identically. Without the shared-register + wildcard-scoring
// design they'd flap (each method-slot would prefer whichever upstream
// scored best for its own method).
func TestStickyScope_NetworkScope_ConvergesAcrossMethods(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams
		.sortByScore(PREFER_FASTEST)
		.stickyPrimary({ scope: NETWORK, hysteresis: 0.10, minSwitchInterval: '30s' })`
	engine, tracker, ups, _, cancel := makeMethodSlotEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	recordAcrossMethods(tracker, ups, []string{"eth_call", "eth_getLogs"})

	// Trigger lazy slot creation by asking for an ordering at each
	// method, then drive a tick on each slot.
	engine.GetOrdered("evm:1", "eth_call", "*")
	engine.GetOrdered("evm:1", "eth_getLogs", "*")
	policy.TickForTest(engine, "evm:1", "eth_call")
	policy.TickForTest(engine, "evm:1", "eth_getLogs")

	callOrder := engine.GetOrdered("evm:1", "eth_call", "*")
	logsOrder := engine.GetOrdered("evm:1", "eth_getLogs", "*")
	require.NotEmpty(t, callOrder, "eth_call slot should have a result")
	require.NotEmpty(t, logsOrder, "eth_getLogs slot should have a result")
	require.Equal(t, callOrder[0].Id(), logsOrder[0].Id(),
		"scope=NETWORK must produce the SAME primary across method-slots — "+
			"that's the whole point of cross-method cohesion. Got %q for eth_call vs %q for eth_getLogs.",
		callOrder[0].Id(), logsOrder[0].Id())
	require.Equal(t, "alchemy", callOrder[0].Id(),
		"the wildcard aggregate makes `alchemy` the global best — both slots should land on it")
}

// TestStickyScope_NetworkMethodFinality_IndependentPerSlot is the
// negative case: with scope=NETWORK_METHOD_FINALITY (current behavior /
// no sharing), each slot picks its own primary based on its local
// score. If the per-method metrics diverge enough that the local best
// differs from the wildcard best, the two slots will disagree — and
// that's intentional under this scope.
func TestStickyScope_NetworkMethodFinality_IndependentPerSlot(t *testing.T) {
	// Scope NETWORK_METHOD_FINALITY → no cross-slot sharing.
	eval := `(upstreams, ctx) => upstreams
		.sortByScore(PREFER_FASTEST)
		.stickyPrimary({ scope: NETWORK_METHOD_FINALITY, hysteresis: 0.10, minSwitchInterval: '30s' })`
	engine, tracker, ups, _, cancel := makeMethodSlotEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	// Same data as the converge test — but THIS scope shouldn't care
	// about wildcard agreement. We just verify each slot picks the
	// score-best from its own per-method view (sortByScore reads
	// `u.metrics` per the slot's method).
	recordAcrossMethods(tracker, ups, []string{"eth_call", "eth_getLogs"})

	engine.GetOrdered("evm:1", "eth_call", "*")
	engine.GetOrdered("evm:1", "eth_getLogs", "*")
	policy.TickForTest(engine, "evm:1", "eth_call")
	policy.TickForTest(engine, "evm:1", "eth_getLogs")

	callOrder := engine.GetOrdered("evm:1", "eth_call", "*")
	logsOrder := engine.GetOrdered("evm:1", "eth_getLogs", "*")
	require.NotEmpty(t, callOrder)
	require.NotEmpty(t, logsOrder)
	// Because we seeded identical distributions per method, both slots
	// SHOULD agree on alchemy here too — the point is that with
	// scope=NETWORK_METHOD_FINALITY they're INDEPENDENTLY arriving at
	// that conclusion (not via the shared register). What we assert
	// here is that NETWORK_METHOD_FINALITY doesn't crash, populates a
	// primary, and converges on the deterministic local best.
	require.Equal(t, "alchemy", callOrder[0].Id())
	require.Equal(t, "alchemy", logsOrder[0].Id())
}

// TestStickyScope_NetworkScope_HoldsAcrossDivergence is the
// hysteresis-under-disagreement test: when one method's local score
// would prefer a different upstream (because that method's per-method
// health is noisy / divergent), the cross-method scope MUST keep the
// shared wildcard-best primary as long as the wildcard signal hasn't
// changed. This is the "no flapping" property.
func TestStickyScope_NetworkScope_HoldsAcrossDivergence(t *testing.T) {
	eval := `(upstreams, ctx) => upstreams
		.sortByScore(PREFER_FASTEST)
		.stickyPrimary({ scope: NETWORK, hysteresis: 0.10, minSwitchInterval: '30s' })`
	engine, tracker, ups, _, cancel := makeMethodSlotEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	// Seed identical "global" health: alchemy=best across methods.
	recordAcrossMethods(tracker, ups, []string{"eth_call", "eth_getLogs"})
	engine.GetOrdered("evm:1", "eth_call", "*")
	engine.GetOrdered("evm:1", "eth_getLogs", "*")
	policy.TickForTest(engine, "evm:1", "eth_call")
	require.Equal(t, "alchemy", engine.GetOrdered("evm:1", "eth_call", "*")[0].Id(),
		"first tick should seed the shared register with alchemy")

	// Now make alchemy LOCALLY worse for eth_getLogs only — its per-
	// method `errorRate` for eth_getLogs spikes, but the WILDCARD
	// aggregate is dragged down only a fraction. The eth_getLogs slot
	// MUST still keep alchemy as primary because the cross-method
	// scope reads the wildcard aggregate, not the per-method score.
	for i := 0; i < 80; i++ {
		tracker.RecordUpstreamRequest(ups[0], "eth_getLogs")
		tracker.RecordUpstreamFailure(ups[0], "eth_getLogs", fmt.Errorf("synth"))
	}
	policy.TickForTest(engine, "evm:1", "eth_getLogs")
	got := engine.GetOrdered("evm:1", "eth_getLogs", "*")[0].Id()
	require.Equal(t, "alchemy", got,
		"alchemy's per-method eth_getLogs score dropped, but its wildcard "+
			"aggregate is still the best — scope=NETWORK must hold across the divergence (got %q)", got)
}
