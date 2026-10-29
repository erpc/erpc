package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// networkPostForward_eth_blockNumber pins every response EXACTLY to the served
// head in every config: below-head responses are raised, above-head ones are
// capped, so eth_blockNumber can never run ahead of the head "latest"
// interpolation anchors eth_call & friends to. It fails open on an unknown
// head (0) and honors the per-request enforce-highest-block directive.
func TestNetworkPostForward_EthBlockNumber(t *testing.T) {
	netWith := func(tip int64) *testNetwork {
		return &testNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
			},
			highestLatest: tip,
		}
	}

	run := func(t *testing.T, network common.Network, enforce bool, respHex string, fromCache bool) (*common.NormalizedResponse, int64) {
		t.Helper()
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		req.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: enforce})
		jrr, err := common.NewJsonRpcResponse(1, respHex, nil)
		require.NoError(t, err)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		if fromCache {
			resp.WithFromCache(true)
		}
		out, err := networkPostForward_eth_blockNumber(context.Background(), network, req, resp, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		_, bn, err := ExtractBlockReferenceFromResponse(context.Background(), out)
		require.NoError(t, err)
		return out, bn
	}

	t.Run("BelowTipIsRaisedToTip", func(t *testing.T) {
		_, bn := run(t, netWith(0x1000), true, "0x800", false)
		assert.Equal(t, int64(0x1000), bn)
	})

	t.Run("AtTipPassesThrough", func(t *testing.T) {
		_, bn := run(t, netWith(0x1000), true, "0x1000", false)
		assert.Equal(t, int64(0x1000), bn)
	})

	t.Run("EnforcedCacheHitRefreshesLatest", func(t *testing.T) {
		// Cache hits return from Network.Forward before pre-forward, so the
		// post-forward path must still ask pollers for a debounced refresh.
		net := netWith(0x1000)
		_, bn := run(t, net, true, "0x1000", true)
		assert.Equal(t, int64(0x1000), bn)
		assert.Equal(t, 1, net.refreshLatestCalls)
	})

	t.Run("EnforcementDisabledDoesNotRefresh", func(t *testing.T) {
		net := netWith(0x1000)
		_, bn := run(t, net, false, "0x1000", true)
		assert.Equal(t, int64(0x1000), bn)
		assert.Equal(t, 0, net.refreshLatestCalls)
	})

	t.Run("AboveTipIsPinnedToTip", func(t *testing.T) {
		// The inversion bug: a fresher head passing a floor-only check lets
		// eth_blockNumber run ahead of the served head that "latest"
		// interpolation anchors eth_call to (on-chain block.number reads then
		// come back BELOW eth_blockNumber, even via the same upstream).
		_, bn := run(t, netWith(0x1000), true, "0x1002", false)
		assert.Equal(t, int64(0x1000), bn,
			"eth_blockNumber must be capped to the served head")
	})

	t.Run("AboveTipCacheHitIsPinnedAndKeepsCacheAttribution", func(t *testing.T) {
		out, bn := run(t, netWith(0x1000), true, "0x1002", true)
		assert.Equal(t, int64(0x1000), bn)
		assert.True(t, out.FromCache(),
			"the pinned response was synthesized from a cache hit and must keep cache attribution")
	})

	t.Run("UnknownTipFailsOpen", func(t *testing.T) {
		_, bn := run(t, netWith(0), true, "0x1002", false)
		assert.Equal(t, int64(0x1002), bn,
			"an unknown tip must never pin the response down to a synthesized 0")
	})

	t.Run("EnforcementDisabledServesRaw", func(t *testing.T) {
		_, bn := run(t, netWith(0x1000), false, "0x1002", false)
		assert.Equal(t, int64(0x1002), bn)
	})
}
