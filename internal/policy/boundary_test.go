package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func ids(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

// TestEngine_PerBoundary_LaneScopesPool — with the boundary axis on, a
// request scoped to a lane (a proper subset of upstream IDs) is evaluated
// against ONLY that subset, while nil lane IDs use the full-pool wildcard.
// The lane key is membership-based, so passing the same set in a different
// order resolves to the same lane.
func TestEngine_PerBoundary_LaneScopesPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	enabled := true
	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(50 * time.Millisecond),
		EvalTimeout:     common.Duration(10 * time.Millisecond),
		EvalScope:       common.EvalScopeNetwork,
		EvalPerBoundary: &enabled,
		EvalFunc:        "(ups, _ctx) => ups", // identity → pool passes through in order
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, cfg.Validate())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	defer engine.Stop()

	ups := []common.Upstream{
		&fakeUpstream{id: "rpc1"},
		&fakeUpstream{id: "rpc2"},
		&fakeUpstream{id: "rpc3"}, // e.g. the body-pruned node, out of range for old blocks
	}
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	require.True(t, engine.PerBoundaryEnabled("evm:1"))

	// Lane {rpc1, rpc2}: once the lazily-created lane slot has ticked, the
	// pool excludes rpc3 entirely (capability, not health).
	require.Eventually(t, func() bool {
		got := engine.GetOrderedInLane("evm:1", "*", "*", []string{"rpc1", "rpc2"})
		return len(got) == 2 && got[0].Id() == "rpc1" && got[1].Id() == "rpc2"
	}, 2*time.Second, 10*time.Millisecond, "lane {rpc1,rpc2} should scope the pool to exactly those two")

	// Same membership, different order → same lane, same scoped pool.
	require.Eventually(t, func() bool {
		got := engine.GetOrderedInLane("evm:1", "*", "*", []string{"rpc2", "rpc1"})
		return len(got) == 2 && got[0].Id() == "rpc1" && got[1].Id() == "rpc2"
	}, 2*time.Second, 10*time.Millisecond, "lane key must be order-independent")

	// nil lane IDs → full-pool wildcard slot (populated synchronously at
	// RegisterNetwork), so all three are returned.
	full := engine.GetOrderedInLane("evm:1", "*", "*", nil)
	require.Equal(t, []string{"rpc1", "rpc2", "rpc3"}, ids(full))
}

// TestEngine_PerBoundary_Off_IgnoresLane — with the axis off, lane IDs are
// ignored and every request resolves to the full-pool wildcard slot
// (identical behavior to GetOrdered).
func TestEngine_PerBoundary_Off_IgnoresLane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	cfg := &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(50 * time.Millisecond),
		EvalTimeout:  common.Duration(10 * time.Millisecond),
		EvalScope:    common.EvalScopeNetwork,
		EvalFunc:     "(ups, _ctx) => ups",
		// EvalPerBoundary omitted → off
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, cfg.Validate())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	defer engine.Stop()

	ups := []common.Upstream{
		&fakeUpstream{id: "rpc1"},
		&fakeUpstream{id: "rpc2"},
		&fakeUpstream{id: "rpc3"},
	}
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	require.False(t, engine.PerBoundaryEnabled("evm:1"))

	// Even with a proper-subset lane passed, the axis is off → wildcard pool.
	// The wildcard cache was populated by RegisterNetwork's synchronous tick,
	// so this is deterministic without waiting.
	got := engine.GetOrderedInLane("evm:1", "*", "*", []string{"rpc1"})
	require.Equal(t, []string{"rpc1", "rpc2", "rpc3"}, ids(got))
}

// TestEngine_PerBoundary_LaneSlotEvicted — lane slots are narrow (non
// all-wildcard) and must be reclaimed by the idle sweep, while the network
// wildcard slot survives. Guards against lane-key cardinality growing the
// slot map without bound.
func TestEngine_PerBoundary_LaneSlotEvicted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	enabled := true
	cfg := &common.SelectionPolicyConfig{
		EvalInterval:         common.Duration(0),
		EvalTimeout:          common.Duration(500 * time.Millisecond),
		EvalScope:            common.EvalScopeNetwork,
		EvalPerBoundary:      &enabled,
		EvalFunc:             "(ups, _ctx) => ups",
		DisableTickerForTest: true,
	}
	require.NoError(t, cfg.SetDefaults())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	defer engine.Stop()
	engine.SetIdleEvictionAfter(50 * time.Millisecond)

	ups := []common.Upstream{
		&fakeUpstream{id: "rpc1"},
		&fakeUpstream{id: "rpc2"},
		&fakeUpstream{id: "rpc3"},
	}
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Lazy-create two distinct lane slots in addition to the wildcard.
	_ = engine.GetOrderedInLane("evm:1", "*", "*", []string{"rpc1", "rpc2"})
	_ = engine.GetOrderedInLane("evm:1", "*", "*", []string{"rpc1"})
	require.Equal(t, 3, policy.SlotCountForTest(engine),
		"wildcard + two lane slots")

	time.Sleep(100 * time.Millisecond)
	policy.SweepIdleSlotsForTest(engine)

	require.Equal(t, 1, policy.SlotCountForTest(engine),
		"both idle lane slots evicted; only the wildcard remains")
}
