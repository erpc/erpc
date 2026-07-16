package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Decision matrix for networkPostForward_eth_blockNumber across the two
// served-tip modes (common.EvmServedTipConfig):
//
//	max mode (default): floor only — below-tip responses are raised, at/above
//	pass through (a response above the last-computed MAX is fresher truth).
//	majority mode ("latest" in servedTip.enabledFor): pinned EXACTLY — above-tip
//	responses are capped too, so eth_blockNumber can never run ahead of the
//	majority tip that "latest" interpolation anchors eth_call & friends to.
//
// Both modes fail open on an unknown tip (0) and honor the per-request
// enforce-highest-block directive.
func TestNetworkPostForward_EthBlockNumber(t *testing.T) {
	netWith := func(servedTipLatest bool, tip int64) *testNetwork {
		evmCfg := &common.EvmNetworkConfig{ChainId: 123}
		if servedTipLatest {
			evmCfg.ServedTip = &common.EvmServedTipConfig{EnabledFor: []string{"latest"}}
		}
		return &testNetwork{
			cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm:          evmCfg,
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

	t.Run("MaxMode_BelowTipIsRaisedToTip", func(t *testing.T) {
		_, bn := run(t, netWith(false, 0x1000), true, "0x800", false)
		assert.Equal(t, int64(0x1000), bn)
	})

	t.Run("MaxMode_AboveTipPassesThrough", func(t *testing.T) {
		_, bn := run(t, netWith(false, 0x1000), true, "0x1200", false)
		assert.Equal(t, int64(0x1200), bn,
			"in max mode a response above the last-computed max is fresher truth and must never be capped")
	})

	t.Run("MaxMode_UnknownTipFailsOpen", func(t *testing.T) {
		_, bn := run(t, netWith(false, 0), true, "0x800", false)
		assert.Equal(t, int64(0x800), bn)
	})

	t.Run("ServedTip_BelowTipIsRaisedToTip", func(t *testing.T) {
		_, bn := run(t, netWith(true, 0x1000), true, "0x800", false)
		assert.Equal(t, int64(0x1000), bn)
	})

	t.Run("ServedTip_AtTipPassesThrough", func(t *testing.T) {
		_, bn := run(t, netWith(true, 0x1000), true, "0x1000", false)
		assert.Equal(t, int64(0x1000), bn)
	})

	t.Run("ServedTip_AboveTipIsPinnedToTip", func(t *testing.T) {
		// The inversion bug: a fresher head passing the floor-only check lets
		// eth_blockNumber run ahead of the majority tip that "latest"
		// interpolation anchors eth_call to (on-chain block.number reads then
		// come back BELOW eth_blockNumber, even via the same upstream).
		_, bn := run(t, netWith(true, 0x1000), true, "0x1002", false)
		assert.Equal(t, int64(0x1000), bn,
			"served-tip mode must cap eth_blockNumber to the majority tip")
	})

	t.Run("ServedTip_AboveTipCacheHitIsPinnedAndKeepsCacheAttribution", func(t *testing.T) {
		out, bn := run(t, netWith(true, 0x1000), true, "0x1002", true)
		assert.Equal(t, int64(0x1000), bn)
		assert.True(t, out.FromCache(),
			"the pinned response was synthesized from a cache hit and must keep cache attribution")
	})

	t.Run("ServedTip_UnknownTipFailsOpen", func(t *testing.T) {
		_, bn := run(t, netWith(true, 0), true, "0x1002", false)
		assert.Equal(t, int64(0x1002), bn,
			"an unknown tip must never pin the response down to a synthesized 0")
	})

	t.Run("ServedTip_EnforcementDisabledServesRaw", func(t *testing.T) {
		_, bn := run(t, netWith(true, 0x1000), false, "0x1002", false)
		assert.Equal(t, int64(0x1002), bn)
	})
}
