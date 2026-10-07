package erpc

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// This exercises physical provider calls through Forward, not a copy of the
// selection algorithm. Frozen heads and explicit policy ticks remove timing
// dependence. Existing moving-chain tests put the leader first in priority,
// which cannot expose the cost of admitting a higher-priority stale upstream.
func TestHeadTracker_TipCheckOrdering(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		masked, pinned, historical, ignoreLeader, retick bool
		served                                           int
	}{
		{name: "rc9-style-lag", served: 1},
		{name: "masked-lag", masked: true, served: 1},
		{name: "request-path-retick", masked: true, retick: true, served: 1},
		{name: "method-ineligible-leader", masked: true, ignoreLeader: true, served: 2},
		{name: "pinned-lazy-debounce", masked: true, pinned: true, served: 0},
		{name: "historical-keeps-priority", masked: true, historical: true, served: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			masked := tc.masked
			mr := miniredis.RunT(t)
			const base int64 = 1000
			nodes := []*timedChain{newTimedChain(100*time.Millisecond, base), newTimedChain(100*time.Millisecond, base), newTimedChain(100*time.Millisecond, base)}
			for i, node := range nodes {
				node.pinned.Store(base)
				t.Cleanup(node.Close)
				if i > 0 {
					node.pinned.Store(base + 40)
				}
			}
			cfg := headTrackerTestConfig(mr.Addr(), t.Name(), nodes[0].URL(), nil, false)
			prj := cfg.Projects[0]
			// No probes: compare ordering independently of the already-fixed lag fanout.
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
				if tc.ignoreLeader && i == 1 {
					c.IgnoreMethods = []string{"eth_getLogs"}
				}
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
				require.NotNil(t, u)
				require.Eventually(t, func() bool { return u.EvmStatePoller().LatestBlock() == nodes[i].head() }, 5*time.Second, time.Millisecond)
			}
			// Install an unstarted tracker: publication and leader polls are driven
			// explicitly, with no timer/election racing the concurrent client burst.
			ht := newTestTracker(newTestSSR(t, t.Context()), nil, nw.headTrackerDeps())
			nw.headTracker = ht
			if masked {
				nw.policyEngine.SetNetworkHooks(nw.networkId, &policy.NetworkHooks{HeadLagOnDemand: func() bool { return nw.EvmTrackedHead() > 0 }})
			}
			const requests = 24
			step := int64(1)
			if tc.pinned {
				// Each publication moves past the previously checked head by
				// the legacy policy's exclusion threshold, keeping u1 the poller.
				step = 20
			}
			for epoch := int64(0); epoch < 3; epoch++ {
				block := base + 40 + epoch*step
				nodes[1].pinned.Store(block)
				nodes[2].pinned.Store(block)
				if tc.pinned {
					nodes[0].pinned.Store(block)
				}
				// Two independently observed heads make the health tracker's lag view
				// corroborated, as in the fleet. u0 stays genuinely behind.
				for i := 1; i < len(ups); i++ {
					ups[i].EvmStatePoller().SuggestLatestBlock(block)
					nw.metricsTracker.SetLatestBlockNumber(ups[i], block, 0)
				}
				nw.metricsTracker.SetLatestBlockNumber(ups[0], base, 0)
				// The leader has its own eligible routing view. Perform a real latest
				// poll before publishing the head to the follower's masked policy.
				nw.policyEngine.SetNetworkHooks(nw.networkId, nil)
				policy.TickForTest(nw.policyEngine, nw.networkId, "*")
				obs, err := nw.headTrackerPoll(t.Context(), false)
				require.NoError(t, err)
				require.Equal(t, "u1", obs.Upstream.Id())
				require.Equal(t, block, obs.Number)
				ht.head.TryUpdate(t.Context(), block-1)
				ht.head.TryUpdate(t.Context(), block)
				ht.advancedAtNs.Store(time.Now().UnixNano())
				if masked {
					nw.policyEngine.SetNetworkHooks(nw.networkId, &policy.NetworkHooks{HeadLagOnDemand: func() bool { return nw.EvmTrackedHead() > 0 }})
				}
				if !tc.retick {
					policy.TickForTest(nw.policyEngine, nw.networkId, "*")
					ordered := nw.policyEngine.GetOrdered(nw.networkId, "eth_getLogs", "*")
					if masked {
						require.Equal(t, "u0", ordered[0].Id(), "masking admits the higher-priority behind upstream")
					} else {
						require.Equal(t, "u1", ordered[0].Id())
					}
				}
				before := make([]map[string]int, len(nodes))
				for i, node := range nodes {
					before[i] = node.snapshot()
				}
				target := block
				if tc.historical {
					target = base - 1
				}
				start := make(chan struct{})
				errs := make(chan error, requests)
				var wg sync.WaitGroup
				for j := 0; j < requests; j++ {
					wg.Add(1)
					go func(j int) {
						defer wg.Done()
						<-start
						// Distinct near-head ranges avoid request multiplexing hiding calls.
						req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getLogs","params":[{"fromBlock":"0x%x","toBlock":"0x%x"}]}`, j+1, target-int64(j), target)))
						if tc.pinned {
							req.SetDirectives(&common.RequestDirectives{UseUpstream: "u0"})
						}
						resp, err := nw.Forward(t.Context(), req)
						if err == nil && (resp == nil || resp.Upstream().Id() != fmt.Sprintf("u%d", tc.served)) {
							err = fmt.Errorf("request %d not served by expected upstream u%d", j, tc.served)
						}
						if err == nil {
							jrr, decodeErr := resp.JsonRpcResponse(t.Context())
							if decodeErr != nil {
								err = decodeErr
							} else {
								var logs []json.RawMessage
								err = json.Unmarshal(jrr.GetResultBytes(), &logs)
								if err == nil && len(logs) != j+1 {
									err = fmt.Errorf("request %d got %d logs, want %d", j, len(logs), j+1)
								}
							}
						}
						if resp != nil {
							resp.Release()
						}
						errs <- err
					}(j)
				}
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					require.NoError(t, err)
				}
				if tc.retick {
					require.Eventually(t, func() bool {
						return nw.policyEngine.GetOrdered(nw.networkId, "eth_getLogs", "*")[0].Id() == "u0"
					}, time.Second, time.Millisecond, "request path resynchronizes the masked policy without provider calls")
				}
				for i, node := range nodes {
					delta, _ := callsDelta(before[i], node.snapshot())
					t.Logf("block=%d u%d physical calls=%v", block, i, delta)
					if tc.pinned && i == 0 {
						require.Equal(t, 1, delta["eth_blockNumber"], "24 concurrent requests share one lazy check per upstream and tracker head")
					} else {
						require.Zero(t, delta["eth_blockNumber"], "no verification of known-ready or unused upstreams")
					}
					if i == tc.served {
						require.Equal(t, requests, delta["eth_getLogs"])
					} else {
						require.Zero(t, delta["eth_getLogs"])
					}
				}
			}
		})
	}
}
