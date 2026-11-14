package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Network.Forward publishes a request to the probe bus before its own
// stateful-method check, so a filter poll reaches the prober. These tests walk
// that path through a real Network: a client pins its filter polls to the
// node that holds the filter, and the policy has excluded one upstream.
func TestNetworkProbe_NeverMirrorsAFilterPoll(t *testing.T) {
	const (
		poll = "eth_getFilterChanges"
		read = "eth_getBlockByNumber"
	)

	// setup builds a two-upstream network in which `slow` is excluded for
	// both methods and so is a probe candidate.
	setup := func(t *testing.T, slow string) (context.Context, context.CancelFunc, *Network, string, string) {
		util.ResetGock()
		t.Cleanup(util.ResetGock)
		util.SetupMocksForEvmStatePoller()
		setupGockObserver(t)
		ctx, cancel := context.WithCancel(context.Background())

		holderHost, otherHost := upstreamHostFromID("holder"), upstreamHostFromID("other")
		network := setupSelectionPolicyNetworkWithEval(t, ctx, []*common.UpstreamConfig{
			{Type: common.UpstreamTypeEvm, Id: "holder", Endpoint: holderHost, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
			{Type: common.UpstreamTypeEvm, Id: "other", Endpoint: otherHost, Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		}, `(upstreams, ctx) => upstreams
			.blockMethods({ match: ['*'], when: all(samplesAbove(20), latencyAbove(5000, 99)) })
			.probeExcluded({ sampleRate: 1, minSamples: 0 })`)

		for _, id := range []string{"holder", "other"} {
			avg := 10
			if id == slow {
				avg = 9000
			}
			for _, m := range []string{poll, read} {
				seedDegraded(network.metricsTracker, upstreamByID(t, network, id), seedSpec{method: m, successful: 100, successAvgMs: avg})
			}
		}
		policy.TickForTest(network.policyEngine, network.networkId, "*")
		for _, m := range []string{poll, read} {
			require.Equal(t, []string{slow}, idsFromUpstreams(network.policyEngine.GetExcluded(network.networkId, m, "*")),
				"%s must be the probe candidate for %s, or this test proves nothing", slow, m)
		}
		for _, host := range []string{holderHost, otherHost} {
			mockClean(t, host, poll, "0x")
			mockClean(t, host, read, "0x1")
		}
		return ctx, cancel, network, holderHost, otherHost
	}

	forward := func(ctx context.Context, network *Network, method, params, pin string) error {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`))
		if pin != "" {
			req.SetDirectives(&common.RequestDirectives{UseUpstream: pin})
		}
		_, err := network.Forward(ctx, req)
		return err
	}
	waitForHits := func(host string, want int) int {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && gockHits(host) < want {
			time.Sleep(10 * time.Millisecond)
		}
		return gockHits(host)
	}

	// The node that holds the filter is itself excluded. The client's poll,
	// pinned there, is not routed to an excluded upstream, so any poll that
	// reaches the node is a mirror. It moves the filter's cursor, and the
	// client loses those events for good once the node is back.
	t.Run("ExcludedFilterHolderGetsNoMirroredPoll", func(t *testing.T) {
		ctx, cancel, network, holderHost, _ := setup(t, "holder")
		defer cancel()

		// Control: an unpinned read is mirrored to the excluded upstream.
		resetGockHits()
		require.NoError(t, forward(ctx, network, read, `["0x10",false]`, ""))
		require.GreaterOrEqual(t, waitForHits(holderHost, 1), 1, "a read must still be mirrored to the excluded upstream")

		resetGockHits()
		_ = forward(ctx, network, poll, `["0xabc"]`, "holder")
		time.Sleep(300 * time.Millisecond)
		assert.Equal(t, 0, gockHits(holderHost), "a mirrored filter poll must never reach the filter's node")
	})

	// Another upstream is excluded. A filter poll mirrored there is a request
	// for a filter it does not hold, and its error counts against it.
	t.Run("ExcludedOtherUpstreamGetsNoPoll", func(t *testing.T) {
		ctx, cancel, network, holderHost, otherHost := setup(t, "other")
		defer cancel()

		resetGockHits()
		require.NoError(t, forward(ctx, network, read, `["0x10",false]`, ""))
		require.GreaterOrEqual(t, waitForHits(otherHost, 1), 1, "a read must still be mirrored to the excluded upstream")

		resetGockHits()
		require.NoError(t, forward(ctx, network, poll, `["0xabc"]`, "holder"))
		time.Sleep(300 * time.Millisecond)
		assert.Equal(t, 0, gockHits(otherHost), "a filter poll must never be mirrored to another upstream")
		assert.Equal(t, 1, gockHits(holderHost), "the filter's node must see the client's poll exactly once")
	})
}
