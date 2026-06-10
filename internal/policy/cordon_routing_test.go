package policy_test

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

// TestEngine_RemoveCordoned_HonorsWildcardCordon proves the routing-level
// half of the forward-jump guard: the guard (and the consensus misbehavior
// path) cordon an upstream at method "*", and the default policy's first
// step — removeCordoned() — must drop that upstream from selection at EVERY
// EvalScope grain, not just the wildcard slot.
//
// Before the fix, removeCordoned() read only the per-method bucket
// (u.metrics.cordonedReason). Under EvalScope=network-method the per-method
// slot's u.metrics is the {ups, eth_call, All} bucket, which never sees a
// "*"-scoped cordon — so the cordoned upstream stayed routable for real user
// methods. The fix also consults the cross-method aggregate
// (u.metricsAcrossMethods), where the wildcard cordon actually lives. The
// network-method subtest below fails on the pre-fix stdlib and passes after.
func TestEngine_RemoveCordoned_HonorsWildcardCordon(t *testing.T) {
	// setup builds a frozen-ticker engine running the rich default policy
	// (EvalFunc == DefaultSelectionPolicySource triggers the upgrade at
	// RegisterNetwork) with one healthy and one offending upstream, at the
	// given EvalScope. Returns the shared tracker + the exact upstream
	// instances (cordon must target the SAME instance the engine snapshots).
	setup := func(t *testing.T, scope common.EvalScope) (*policy.Engine, *health.Tracker, common.Upstream, common.Upstream) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		logger := zerolog.Nop()
		tracker := health.NewTracker(&logger, "test", time.Minute)

		cfg := &common.SelectionPolicyConfig{
			// Frozen: only our explicit TickForTest fires, so background
			// re-eval can't race the assertions.
			EvalInterval:         common.Duration(0),
			EvalTimeout:          common.Duration(500 * time.Millisecond),
			EvalScope:            scope,
			EvalFunc:             common.DefaultSelectionPolicySource, // upgraded to default_policy.js
			DisableTickerForTest: true,
		}
		require.NoError(t, cfg.SetDefaults())

		engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)
		t.Cleanup(engine.Stop)

		healthy := &fakeUpstream{id: "healthy"}
		offender := &fakeUpstream{id: "offender"}
		ups := []common.Upstream{healthy, offender}
		require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

		return engine, tracker, healthy, offender
	}

	const cordonReason = "implausible forward block-head jump (1000000 -> 60000000)"

	t.Run("network-method scope drops a wildcard-cordoned upstream", func(t *testing.T) {
		engine, tracker, _, offender := setup(t, common.EvalScopeNetworkMethod)

		// Lazy-create the narrow eth_call slot FIRST. Without this,
		// TickForTest/LatestDecisionOutputForTest fall back to the wildcard
		// ("*","*") slot — which evaluates at method "*" and would mask the
		// per-method gap (its u.metrics already reads the cordoned bucket).
		engine.GetOrdered("evm:1", "eth_call", "*")

		// Cordon at "*" exactly as the poller's forward-jump handler does.
		tracker.Cordon(offender, "*", cordonReason)

		// Re-eval the narrow slot post-cordon and read its decision.
		policy.TickForTest(engine, "evm:1", "eth_call")
		order, excluded := policy.LatestDecisionOutputForTest(engine, "evm:1", "eth_call")

		require.NotContains(t, order, "offender",
			"wildcard-cordoned upstream must not be routable for eth_call under network-method scope")
		require.Contains(t, order, "healthy", "healthy upstream must remain routable")
		require.Contains(t, excluded, "offender", "offender must be reported in the excluded set")
	})

	t.Run("default network scope drops a wildcard-cordoned upstream", func(t *testing.T) {
		engine, tracker, _, offender := setup(t, common.EvalScopeNetwork)

		tracker.Cordon(offender, "*", cordonReason)

		// Default scope has a single ("*","*") slot created at register.
		policy.TickForTest(engine, "evm:1", "*")
		order, excluded := policy.LatestDecisionOutputForTest(engine, "evm:1", "*")

		require.NotContains(t, order, "offender", "wildcard-cordoned upstream must not be routable")
		require.Contains(t, order, "healthy", "healthy upstream must remain routable")
		require.Contains(t, excluded, "offender", "offender must be reported in the excluded set")
	})
}
