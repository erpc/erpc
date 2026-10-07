package stdlib_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// On a head-tracked network with a fresh head, lag is enforced per request
// (tip check). The 60s poller lag view must then neither exclude nor probe
// an upstream; the moment the tracker goes stale, lag exclusion applies
// again without waiting for the next tick. Error exclusions are untouched.
func TestSelectionPolicy_HeadLagOnDemand(t *testing.T) {
	const eval = `(upstreams, ctx) => upstreams
		.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
		.excludeIf(any(blockNumberLagAbove(16), blockSecondsLagAbove(30)))
		.whenEmpty(() => upstreams)
		.probeExcluded({ sampleRate: 1, minSamples: 0 })`

	for _, sc := range []struct {
		name  string
		scope common.EvalScope
		meth  string
	}{
		{"network", common.EvalScopeNetwork, "*"},
		{"network-method", common.EvalScopeNetworkMethod, "eth_getBlockByNumber"},
	} {
		t.Run(sc.name, func(t *testing.T) {
			engine, _, tracker, cancel := newTestEngine(t, eval)
			defer cancel()
			defer engine.Stop()

			ups := mkUps("leader", "lagging", "broken")
			cfg := &common.SelectionPolicyConfig{
				EvalTimeout: common.Duration(50 * time.Millisecond),
				EvalFunc:    eval,
				EvalScope:   sc.scope,
			}
			require.NoError(t, cfg.SetDefaults())
			cfg.EvalInterval = 0 // frozen: only manual / resync ticks
			var fresh atomic.Bool
			fresh.Store(true)
			engine.SetNetworkHooks("evm:1", &policy.NetworkHooks{HeadLagOnDemand: fresh.Load})
			require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))

			m := "eth_getBlockByNumber"
			for _, u := range ups {
				for i := 0; i < 20; i++ {
					tracker.RecordUpstreamRequest(u, m, common.DataFinalityStateUnfinalized)
					tracker.RecordUpstreamDuration(u, m, 10*time.Millisecond, true, "none", common.DataFinalityStateUnfinalized, "n/a")
				}
			}
			for i := 0; i < 20; i++ {
				tracker.RecordUpstreamFailure(ups[2], m, common.DataFinalityStateUnfinalized, common.NewErrEndpointServerSideException(nil, nil, 500))
			}
			// "lagging" looks 900 blocks behind from its slow poller.
			tracker.SetLatestBlockNumber(ups[0], 1000, 0)
			tracker.SetLatestBlockNumber(ups[2], 1000, 0)
			tracker.SetLatestBlockNumber(ups[1], 100, 0)

			_ = engine.GetOrdered("evm:1", sc.meth, "*")
			policy.TickForTest(engine, "evm:1", sc.meth)
			ordered := ids(engine.GetOrdered("evm:1", sc.meth, "*"))
			require.Equal(t, []string{"leader", "lagging"}, ordered, "fresh tracker: lag does not exclude, errors still do")
			require.Equal(t, []string{"broken"}, ids(engine.GetExcluded("evm:1", sc.meth, "*")),
				"fresh tracker: only the error-excluded upstream is a probe target")

			// Tracker goes stale: the next request-path read triggers an
			// immediate re-eval with the real lag.
			fresh.Store(false)
			_ = engine.GetOrdered("evm:1", sc.meth, "*")
			require.Eventually(t, func() bool {
				o := ids(engine.GetOrdered("evm:1", sc.meth, "*"))
				return len(o) == 1 && o[0] == "leader"
			}, 2*time.Second, 5*time.Millisecond, "stale tracker: lag exclusion applies again at once")
			require.ElementsMatch(t, []string{"lagging", "broken"}, ids(engine.GetExcluded("evm:1", sc.meth, "*")),
				"stale tracker: lag exclusions are probe targets again (untracked behavior)")

			// Fresh again, and the last tick (unmasked) is still cached: the
			// lag-only exclusion is not probed even before the resync lands.
			fresh.Store(true)
			require.Equal(t, []string{"broken"}, ids(engine.GetExcluded("evm:1", sc.meth, "*")))
			_ = engine.GetOrdered("evm:1", sc.meth, "*")
			require.Eventually(t, func() bool {
				return len(engine.GetOrdered("evm:1", sc.meth, "*")) == 2
			}, 2*time.Second, 5*time.Millisecond, "fresh again: resynced to the masked view")
		})
	}
}

// Without hooks (untracked network) lag exclusion and its probes are
// unchanged.
func TestSelectionPolicy_HeadLagWithoutHooksUnchanged(t *testing.T) {
	const eval = `(upstreams, ctx) => upstreams
		.excludeIf(blockNumberLagAbove(16))
		.probeExcluded({ sampleRate: 1, minSamples: 0 })`
	engine, _, tracker, cancel := newTestEngine(t, eval)
	defer cancel()
	defer engine.Stop()
	ups := mkUps("a", "b", "c")
	cfg := &common.SelectionPolicyConfig{EvalTimeout: common.Duration(50 * time.Millisecond), EvalFunc: eval}
	require.NoError(t, cfg.SetDefaults())
	cfg.EvalInterval = 0
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))
	tracker.SetLatestBlockNumber(ups[0], 1000, 0)
	tracker.SetLatestBlockNumber(ups[2], 1000, 0)
	tracker.SetLatestBlockNumber(ups[1], 100, 0)
	policy.TickForTest(engine, "evm:1", "*")
	require.Equal(t, []string{"a", "c"}, ids(engine.GetOrdered("evm:1", "*", "*")))
	require.Equal(t, []string{"b"}, ids(engine.GetExcluded("evm:1", "*", "*")))
}
