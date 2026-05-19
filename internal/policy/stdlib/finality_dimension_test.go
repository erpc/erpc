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

// TestStdlib_EvalPerFinality_SeparateSlots verifies that when
// `EvalPerFinality: true` is set, the engine creates one slot per
// finality bucket and each slot's eval sees `ctx.finality` set to its
// bucket value. The same `(network, method)` returns DIFFERENT orderings
// for different finalities — the whole point of the dimension.
func TestStdlib_EvalPerFinality_SeparateSlots(t *testing.T) {
	// Different preset per finality bucket. The eval branches on
	// `ctx.finality` and picks an ordering keyed by the finality
	// itself — easy to assert which slot the engine routed us to.
	eval := `(upstreams, ctx) => {
		if (ctx.finality === 'realtime')   return upstreams.byId('rt-only');
		if (ctx.finality === 'finalized')  return upstreams.byId('fin-only');
		return upstreams;
	}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{id: "rt-only"},
		{id: "fin-only"},
		{id: "unknown-fallback"},
	})

	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(0), // frozen — drive manually
		EvalTimeout:     common.Duration(50 * time.Millisecond),
		EvalFunc:        eval,
		EvalPerFinality: true,
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Three finality buckets — each `GetOrdered` lazy-creates its own
	// slot. Without EvalPerFinality every call would resolve to the
	// wildcard slot and the eval would see ctx.finality="unknown" for
	// all of them.
	for _, finality := range []string{"realtime", "finalized", "unknown"} {
		// Trigger lazy slot creation. Initial call may return the
		// wildcard fallback (which sees ctx.finality="unknown" → returns
		// all 3 upstreams), so we ALSO tick the newly-created slot.
		_ = engine.GetOrdered("evm:1", "*", finality)
		// Wait a beat then look up again — by now the slot exists and
		// its ticker is running. With EvalInterval=0 we need to drive a
		// tick manually; lookupSlotForTest only finds method-scoped
		// slots, so use the RecentDecisions accessor which honors the
		// finality dimension.
	}

	// At this point all three finality slots exist. Drive a tick on
	// each by calling GetOrdered again — for EvalInterval=0 slots, the
	// initial sync tick at registration runs only on the wildcard, so
	// per-finality slots need an explicit poke. We do that via the
	// admin-style `RecentDecisions(network, "*", finality)` path which
	// resolves to the right slot.
	//
	// Actually the simpler path: drive each per-finality slot via an
	// internal hook. The engine's `lookupSlotWithFallback` is called
	// from `GetOrdered`; that lazy-creates and starts a ticker. Since
	// `EvalInterval: 0`, the start() bails — there's no ticker. We
	// need a TickForTest equivalent that addresses the finality slot.
	//
	// For now: drive ticks via a fresh `RegisterNetwork` with
	// EvalInterval > 0, then poll until the slot caches a result.
	// Simpler still: drop the policy-side `EvalInterval=0` and use a
	// short interval; the test's frozen-time semantics still hold.
	cancel2 := func() {}
	defer cancel2()

	cfg2 := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(10 * time.Millisecond),
		EvalTimeout:     common.Duration(100 * time.Millisecond),
		EvalFunc:        eval,
		EvalPerFinality: true,
	}
	require.NoError(t, cfg2.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg2))

	// Touch each finality bucket so the per-finality slot is created
	// and its 10ms ticker starts.
	for _, finality := range []string{"realtime", "finalized", "unknown"} {
		_ = engine.GetOrdered("evm:1", "*", finality)
	}

	// Wait for slots to tick at least once.
	require.Eventually(t, func() bool {
		return len(engine.GetOrdered("evm:1", "*", "realtime")) == 1 &&
			engine.GetOrdered("evm:1", "*", "realtime")[0].Id() == "rt-only"
	}, 2*time.Second, 20*time.Millisecond, "realtime slot should converge on rt-only")

	require.Eventually(t, func() bool {
		got := engine.GetOrdered("evm:1", "*", "finalized")
		return len(got) == 1 && got[0].Id() == "fin-only"
	}, 2*time.Second, 20*time.Millisecond, "finalized slot should converge on fin-only")

	require.Eventually(t, func() bool {
		got := engine.GetOrdered("evm:1", "*", "unknown")
		return len(got) == 3
	}, 2*time.Second, 20*time.Millisecond, "unknown slot should fall through to all upstreams")
}

// TestStdlib_EvalPerFinality_Off_FallsThroughToWildcard verifies that
// when EvalPerFinality is FALSE, GetOrdered ignores the finality arg
// and resolves to the wildcard slot. The eval still sees ctx.finality
// from its slot's `s.finality` field (or "unknown" default for the
// wildcard slot).
func TestStdlib_EvalPerFinality_Off_FallsThroughToWildcard(t *testing.T) {
	// Eval returns the finality it sees as a single-element pseudo-id
	// — but our upstreams don't have those ids, so an empty result
	// means "the eval saw a finality that doesn't match any upstream".
	// Easier: just check the cached order is the same across finality
	// arguments when EvalPerFinality is off.
	eval := `(upstreams, ctx) => upstreams`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
	defer engine.Stop()

	ups := mkUps("a", "b", "c")
	cfg := &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(10 * time.Millisecond),
		EvalTimeout:  common.Duration(100 * time.Millisecond),
		EvalFunc:     eval,
		// EvalPerFinality is FALSE (zero value) — single wildcard slot.
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", func() []common.Upstream { return ups }, cfg))

	// Wait for the initial wildcard tick to publish a cache.
	require.Eventually(t, func() bool {
		return len(engine.GetOrdered("evm:1", "*", "realtime")) == 3
	}, 2*time.Second, 20*time.Millisecond)

	// Every finality bucket resolves to the same wildcard slot →
	// same cached upstream slice (backing array identity).
	rt := engine.GetOrdered("evm:1", "*", "realtime")
	fin := engine.GetOrdered("evm:1", "*", "finalized")
	unk := engine.GetOrdered("evm:1", "*", "unknown")
	require.Equal(t, 3, len(rt))
	require.Equal(t, 3, len(fin))
	require.Equal(t, 3, len(unk))
	// Same backing array — proves all three calls hit the same wildcard slot.
	require.Equal(t, fmt.Sprintf("%p", rt), fmt.Sprintf("%p", fin),
		"EvalPerFinality=false: all finality args must resolve to the same wildcard slot")
}
