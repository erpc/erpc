package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// A block-agnostic method (eth_chainId, net_version) is marked finalized, and
// block-reference extraction gives it block number 1. That 1 is a cache
// sentinel ("final since the first block"), not a block the caller asked for.
// The availability gates must not compare it with an upstream's lower bound:
// a pruned node with maxAvailableRecentBlocks has a lower bound of latest-N,
// and 1 is below it.
func TestNetworkAvailability_StaticMethodIsNotGatedAsABlockRead(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupDirectAvailabilityNetwork(t, ctx,
		&common.EvmUpstreamConfig{MaxAvailableRecentBlocks: 128}, nil,
	)

	for _, method := range []string{"eth_chainId", "net_version"} {
		t.Run(method, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
			req.SetNetwork(network)

			skipErr, retryable := network.checkUpstreamBlockAvailability(ctx, ups, req, method)
			require.NoError(t, skipErr, "a static method needs no block, so a pruned upstream cannot be missing it")
			require.False(t, retryable)

			require.Nil(t, network.eligibleUpstreamIDsForBoundary(ctx, method, req),
				"no boundary lane applies to a static method")
		})
	}

	// The gate still refuses a real historical read below the bound.
	old := balanceReqAt(network, "0x5", 5)
	skipErr, retryable := network.checkUpstreamBlockAvailability(ctx, ups, old, "eth_getBalance")
	require.Error(t, skipErr, "block 5 is below latest-128 and must still be refused")
	require.True(t, common.HasErrorCode(skipErr, common.ErrCodeUpstreamBlockUnavailable))
	require.False(t, retryable)
}

// The same defect through Network.Forward. eRPC answers eth_chainId itself by
// default, so the request reaches the upstreams only when the network turns
// that local answer off. Then a pruned upstream must still serve it.
func TestNetworkAvailability_StaticMethodForwardsToAPrunedUpstream(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupDirectAvailabilityNetwork(t, ctx,
		&common.EvmUpstreamConfig{MaxAvailableRecentBlocks: 128},
		&common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{
			ChainId:      123,
			LocalAnswers: map[string]bool{"eth_chainId": false},
		}},
	)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "the pruned upstream must serve eth_chainId")
	require.NotNil(t, resp)
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.Equal(t, `"0x7b"`, string(jrr.GetResultBytes()))
}
