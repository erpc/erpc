package stdlib_test

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// blockNumberLagAbove must exclude a lagging upstream at EVERY eval-scope grain,
// not just the network (wildcard) grain. With per-finality tracking on, requests
// recorded at a concrete (method, finality) register that specific bucket in the
// (ups,method) dedup index; lag is a per-upstream value, but the optimized
// lag-write only touched indexed keys — leaving the {method, All} rollup that
// network/network-method slots read starved, so the predicate silently no-oped.
func TestSelectionPolicy_HeadLagExclusion_AllScopes(t *testing.T) {
	const eval = `(upstreams, ctx) => upstreams.excludeIf(blockNumberLagAbove(16))`

	scopes := []struct {
		name     string
		scope    common.EvalScope
		tickMeth string // method to tick/read the slot at
	}{
		{"network", common.EvalScopeNetwork, "*"},
		{"network-method", common.EvalScopeNetworkMethod, "eth_getLogs"},
		{"network-method-finality", common.EvalScopeNetworkMethodFinality, "eth_getLogs"},
	}

	for _, sc := range scopes {
		t.Run(sc.name, func(t *testing.T) {
			engine, _, tracker, cancel := newTestEngine(t, eval)
			defer cancel()
			defer engine.Stop()
			tracker.EnableFinalityTracking()

			ups := mkUps("rpc1", "rpc2")
			cfg := &common.SelectionPolicyConfig{
				EvalInterval: 0,
				EvalTimeout:  common.Duration(50 * time.Millisecond),
				EvalFunc:     eval,
				EvalScope:    sc.scope,
			}
			require.NoError(t, cfg.SetDefaults())
			require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

			// Specific-finality records create {method,fin} first → it wins the
			// (ups,method) dedup slot; {method,All} stays out of the index.
			fin := common.DataFinalityStateUnfinalized
			for _, u := range ups {
				for i := 0; i < 20; i++ {
					tracker.RecordUpstreamRequest(u, "eth_getLogs", fin)
					tracker.RecordUpstreamDuration(u, "eth_getLogs", 10*time.Millisecond, true, "none", fin, "n/a")
				}
			}

			// Real lag path: rpc2 at tip (1000), rpc1 frozen far behind (lag 900).
			tracker.SetLatestBlockNumber(ups[1], 1000, 0)
			tracker.SetLatestBlockNumber(ups[0], 100, 0)

			_ = engine.GetOrdered("evm:1", sc.tickMeth, "*") // lazy-create the per-grain slot
			policy.TickForTest(engine, "evm:1", sc.tickMeth)
			ordered := ids(engine.GetOrdered("evm:1", sc.tickMeth, "*"))

			require.NotContains(t, ordered, "rpc1",
				"rpc1 (lag 900) must be excluded by blockNumberLagAbove(16) at %s grain (got %v)", sc.name, ordered)
			require.Contains(t, ordered, "rpc2", "rpc2 (at tip) must survive")
		})
	}
}

// Tracker-level invariant: after SetLatestBlockNumber with per-finality tracking
// on, the per-upstream lag must reach the {method, All} rollup (the bucket
// non-finality-grain readers normalize to), not only the specific-finality
// bucket that happened to win the (ups,method) dedup index.
func TestTracker_BlockHeadLag_ReachesMethodAllRollup(t *testing.T) {
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	tracker.EnableFinalityTracking()

	ups := mkUps("rpc1", "rpc2")
	fin := common.DataFinalityStateUnfinalized
	for _, u := range ups {
		tracker.RecordUpstreamRequest(u, "eth_getLogs", fin)
		tracker.RecordUpstreamDuration(u, "eth_getLogs", 5*time.Millisecond, true, "none", fin, "n/a")
	}

	tracker.SetLatestBlockNumber(ups[1], 1000, 0) // network tip
	tracker.SetLatestBlockNumber(ups[0], 100, 0)  // lag 900

	got := tracker.GetUpstreamMethodMetrics(ups[0], "eth_getLogs", common.DataFinalityStateAll).BlockHeadLag.Load()
	require.EqualValues(t, 900, got,
		"{eth_getLogs, All} rollup must carry the upstream lag (was starved before the fix)")

	// The full-wildcard and specific-finality buckets must agree too.
	require.EqualValues(t, 900, tracker.GetUpstreamMethodMetrics(ups[0], "*", common.DataFinalityStateAll).BlockHeadLag.Load())
	require.EqualValues(t, 900, tracker.GetUpstreamMethodMetrics(ups[0], "eth_getLogs", fin).BlockHeadLag.Load())
}
