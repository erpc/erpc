package erpc

import (
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// Lag masking removes lag from the selection policy for every method while
// the tracker is fresh, so a head-relative read of ANY method (literal
// "latest" that is not interpolated, an omitted block param, a realtime
// method, or "latest" interpolated to the tracked head) must not be served
// by an upstream that is behind. The behind upstream u0 is the policy's
// first choice (masked lag, priority order); the reads land on u1, which
// the leader's poll proved current, with no verification call, and u0 is
// never asked. A historical read keeps policy priority (u0).
func TestHeadTracker_LagMaskCoversHeadRelativeReads(t *testing.T) {
	mr := miniredis.RunT(t)
	const base int64 = 1000
	const block = base + 40
	nodes := []*timedChain{newTimedChain(100*time.Millisecond, base), newTimedChain(100*time.Millisecond, base), newTimedChain(100*time.Millisecond, base)}
	for i, node := range nodes {
		t.Cleanup(node.Close)
		node.pinned.Store(base)
		if i > 0 {
			node.pinned.Store(block)
		}
	}
	cfg := headTrackerTestConfig(mr.Addr(), t.Name(), nodes[0].URL(), nil, false)
	prj := cfg.Projects[0]
	prj.Networks[0].SelectionPolicy = &common.SelectionPolicyConfig{
		EvalFunc:     `(upstreams) => upstreams.excludeIf(blockNumberLagAbove(16)).whenEmpty(() => upstreams).sortBy(u => Number(u.tags[0]))`,
		EvalInterval: common.Duration(time.Hour), EvalScope: common.EvalScopeNetwork,
	}
	template := prj.Upstreams[0]
	prj.Upstreams = nil
	for i, node := range nodes {
		c := *template
		evm := *template.Evm
		evm.StatePollerInterval = common.Duration(time.Hour)
		c.Evm = &evm
		c.Id, c.Endpoint, c.Tags = fmt.Sprintf("u%d", i), node.URL(), []string{fmt.Sprint(i)}
		prj.Upstreams = append(prj.Upstreams, &c)
	}
	rep := startHTReplicas(t, 1, func() *common.Config { return cfg })[0]
	nw := rep.network(t)
	nw.policyEngine.SetPaused(true)
	ups := make([]common.EvmUpstream, len(nodes))
	for _, u := range nw.AllUpstreams() {
		for i := range ups {
			if u.Id() == fmt.Sprintf("u%d", i) {
				ups[i] = u
			}
		}
	}
	for i, u := range ups {
		require.Eventually(t, func() bool { return u.EvmStatePoller().LatestBlock() == nodes[i].head() }, 5*time.Second, time.Millisecond)
	}
	ht := newTestTracker(newTestSSR(t, t.Context()), nil, nw.headTrackerDeps())
	nw.headTracker = ht
	for i := 1; i < len(ups); i++ {
		nw.metricsTracker.SetLatestBlockNumber(ups[i], block, 0)
	}
	nw.metricsTracker.SetLatestBlockNumber(ups[0], base, 0)
	// The leader's real poll: u1 serves it (u0 is lag-excluded unmasked).
	policy.TickForTest(nw.policyEngine, nw.networkId, "*")
	obs, err := nw.headTrackerPoll(t.Context(), false)
	require.NoError(t, err)
	require.Equal(t, "u1", obs.Upstream.Id())
	ht.head.TryUpdate(t.Context(), block-1)
	ht.head.TryUpdate(t.Context(), block)
	ht.advancedAtNs.Store(time.Now().UnixNano())
	nw.policyEngine.SetNetworkHooks(nw.networkId, &policy.NetworkHooks{HeadLagOnDemand: func() bool { return nw.EvmTrackedHead() > 0 }})
	policy.TickForTest(nw.policyEngine, nw.networkId, "*")
	require.Equal(t, "u0", nw.policyEngine.GetOrdered(nw.networkId, "eth_getBalance", "*")[0].Id(), "masking makes the behind upstream the first choice")

	addr := `"0x0000000000000000000000000000000000000001"`
	for _, tc := range []struct {
		name, body, served string
		skipInterpolation  bool
	}{
		{name: "getBalance latest (interpolated)", body: `{"method":"eth_getBalance","params":[` + addr + `,"latest"]}`, served: "u1"},
		{name: "getBalance latest (not interpolated)", body: `{"method":"eth_getBalance","params":[` + addr + `,"latest"]}`, served: "u1", skipInterpolation: true},
		{name: "getBalance pending", body: `{"method":"eth_getBalance","params":[` + addr + `,"pending"]}`, served: "u1"},
		{name: "getBalance no block param", body: `{"method":"eth_getBalance","params":[` + addr + `]}`, served: "u1"},
		{name: "gasPrice (realtime, no block param)", body: `{"method":"eth_gasPrice","params":[]}`, served: "u1"},
		{name: "getBalance historical", body: fmt.Sprintf(`{"method":"eth_getBalance","params":[%s,"0x%x"]}`, addr, base-5), served: "u0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := make([]map[string]int, len(nodes))
			for i, node := range nodes {
				before[i] = node.snapshot()
			}
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,` + tc.body[1:]))
			dirs := &common.RequestDirectives{SkipCacheRead: "true"}
			dirs.SkipInterpolation = tc.skipInterpolation
			req.SetDirectives(dirs)
			resp, err := nw.Forward(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, tc.served, resp.Upstream().Id())
			resp.Release()
			for i, node := range nodes {
				delta, _ := callsDelta(before[i], node.snapshot())
				require.Zero(t, delta["eth_blockNumber"], "u%d: no verification call: u1 is known current, u0 is never chosen", i)
				if fmt.Sprintf("u%d", i) != tc.served {
					require.Zero(t, delta["eth_getBalance"]+delta["eth_gasPrice"], "u%d must not serve", i)
				}
			}
		})
	}

	// An upstream behind the tracked head that is still chosen (here pinned,
	// with the literal tag reaching it) is checked lazily (one
	// eth_blockNumber) and skipped: no stale answer, no fan-out. (A pinned
	// request that IS interpolated resolves "latest" to the pinned subset's
	// own head, a consistent read the gate leaves alone.)
	t.Run("only behind upstream left", func(t *testing.T) {
		before := nodes[0].snapshot()
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":[` + addr + `,"latest"]}`))
		req.SetDirectives(&common.RequestDirectives{SkipCacheRead: "true", UseUpstream: "u0", SkipInterpolation: true})
		resp, err := nw.Forward(t.Context(), req)
		if resp != nil {
			resp.Release()
		}
		require.Error(t, err, "a behind upstream does not serve a head-relative read")
		delta, _ := callsDelta(before, nodes[0].snapshot())
		require.Equal(t, 1, delta["eth_blockNumber"], "one lazy check")
		require.Zero(t, delta["eth_getBalance"])
	})
}
