package policy_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// fakeUpstream is a minimal `common.Upstream` for engine smoke tests.
type fakeUpstream struct {
	id   string
	tier string
}

func (f *fakeUpstream) Id() string           { return f.id }
func (f *fakeUpstream) VendorName() string   { return "test" }
func (f *fakeUpstream) NetworkId() string    { return "evm:1" }
func (f *fakeUpstream) NetworkLabel() string { return "evm:1" }
func (f *fakeUpstream) Config() *common.UpstreamConfig {
	cfg := &common.UpstreamConfig{Id: f.id}
	if f.tier != "" {
		cfg.Tags = []string{"tier:" + f.tier}
	}
	return cfg
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

// TestEngine_IdentityPolicy_PassesThrough runs the simplest possible eval —
// `(upstreams, ctx) => upstreams` — and verifies the engine returns the
// upstreams in registration order.
func TestEngine_IdentityPolicy_PassesThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(50 * time.Millisecond),
		EvalTimeout:     common.Duration(10 * time.Millisecond),
		// Distinct from `common.DefaultSelectionPolicySource` so the engine
		// does NOT auto-upgrade to the rich default policy.
		EvalFunc: "(ups, _ctx) => ups",
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, cfg.Validate())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	defer engine.Stop()

	ups := []common.Upstream{
		&fakeUpstream{id: "rpc1", tier: "main"},
		&fakeUpstream{id: "rpc2", tier: "main"},
		&fakeUpstream{id: "rpc3", tier: "fallback"},
	}
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	ordered := engine.GetOrdered("evm:1", "*", "*")
	require.Len(t, ordered, 3)
	require.Equal(t, "rpc1", ordered[0].Id())
	require.Equal(t, "rpc2", ordered[1].Id())
	require.Equal(t, "rpc3", ordered[2].Id())
}

// TestEngine_OverrideOrderForTest exercises the test-only helper that the
// retry/hedge/failsafe migration depends on (Phase 2.3).
func TestEngine_OverrideOrderForTest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	cfg := &common.SelectionPolicyConfig{
		EvalInterval:    common.Duration(0), // frozen: only manual ticks
		EvalTimeout:     common.Duration(10 * time.Millisecond),
	}
	require.NoError(t, cfg.SetDefaults())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	defer engine.Stop()

	ups := []common.Upstream{
		&fakeUpstream{id: "rpc1"},
		&fakeUpstream{id: "rpc2"},
		&fakeUpstream{id: "rpc3"},
	}
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	policy.OverrideOrderForTest(engine, "evm:1", "rpc3", "rpc1")
	ordered := engine.GetOrdered("evm:1", "*", "*")
	require.Len(t, ordered, 2)
	require.Equal(t, "rpc3", ordered[0].Id())
	require.Equal(t, "rpc1", ordered[1].Id())
}

// TestEngine_SweepIdleSlots verifies the engine evicts narrow slots
// that have gone silent past `idleEvictionAfter`. Defends against
// method-flood: a malicious client hitting random JSON-RPC method
// names would otherwise grow the slot map unbounded (one slot per
// distinct method × finality combo when evalScope opts in).
//
// Properties asserted:
//
//  1. Narrow slots (specific method, with evalScope=network-method)
//     lazy-create on first GetOrdered and DO get evicted after
//     idleEvictionAfter elapses.
//  2. The network wildcard slot (`("*", "*")`) is NEVER evicted, no
//     matter how long it sits idle — it's the cold-start fallback.
//  3. A hot narrow slot (re-touched on every sweep cycle) survives.
func TestEngine_SweepIdleSlots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", time.Minute)

	cfg := &common.SelectionPolicyConfig{
		// EvalInterval=0 freezes auto-ticking — combined with
		// DisableTickerForTest=true, slot timestamps don't refresh
		// behind our back while we wait for the idle threshold to
		// elapse. The initial sync tick at RegisterNetwork still runs
		// (it's separate from the ticker), so the wildcard slot's
		// cache is populated for the GetOrdered fallback to hit.
		EvalInterval:         common.Duration(0),
		EvalTimeout:          common.Duration(500 * time.Millisecond),
		EvalScope:            common.EvalScopeNetworkMethod, // lazy per-method slots
		EvalFunc:             `(ups, _ctx) => ups`,          // trivial pass-through
		DisableTickerForTest: true,
	}
	require.NoError(t, cfg.SetDefaults())

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, nil, nil)
	defer engine.Stop()
	// Aggressive threshold so the sweep test doesn't have to wait an hour.
	engine.SetIdleEvictionAfter(50 * time.Millisecond)

	ups := []common.Upstream{
		&fakeUpstream{id: "rpc1"},
		&fakeUpstream{id: "rpc2"},
	}
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

	// Simulate the method-flood: 50 unique JSON-RPC method names, each
	// touched once via GetOrdered (which lazy-creates the slot).
	for i := 0; i < 50; i++ {
		_ = engine.GetOrdered("evm:1", fmt.Sprintf("eth_random%d", i), "*")
	}
	// One method that we'll keep alive across the wait.
	const hotMethod = "eth_blockNumber"
	_ = engine.GetOrdered("evm:1", hotMethod, "*")

	// Wildcard slot (created at RegisterNetwork) + 50 random + 1 hot = 52.
	require.Equal(t, 52, policy.SlotCountForTest(engine),
		"flood should have lazy-created 50 narrow slots plus the hot one + wildcard")

	// Wait past the idle threshold, then sweep.
	time.Sleep(100 * time.Millisecond)
	// Re-touch the hot slot RIGHT BEFORE the sweep so it survives.
	_ = engine.GetOrdered("evm:1", hotMethod, "*")
	policy.SweepIdleSlotsForTest(engine)

	// Now: wildcard + hot = 2.
	got := policy.SlotCountForTest(engine)
	require.Equal(t, 2, got,
		"after sweep, only the wildcard slot + the hot method's slot should remain (got %d)", got)

	// Sanity: the wildcard slot is still alive AND its slot key
	// remains in the engine's map.
	wildcardOK := false
	wildcardCacheLen := -1
	policy.WalkSlotsForTest(engine, func(network, method, finality string, cacheLen int) {
		if network == "evm:1" && method == "*" && finality == "*" {
			wildcardOK = true
			wildcardCacheLen = cacheLen
		}
	})
	require.True(t, wildcardOK, "wildcard slot ('*', '*') must NOT be evicted")
	// Initial RegisterNetwork tick populated the cache, sweep didn't
	// touch it — the wildcard cache should still hold both upstreams.
	require.Equal(t, 2, wildcardCacheLen,
		"wildcard cache should still hold both upstreams after the sweep (got %d)", wildcardCacheLen)
}
