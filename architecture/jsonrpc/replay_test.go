package jsonrpc

import (
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type replayNetwork struct {
	genericNetwork
	cfg *common.NetworkConfig
}

func (n replayNetwork) Config() *common.NetworkConfig { return n.cfg }

func reqOn(cfg *common.NetworkConfig, method string) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
	if cfg != nil {
		req.SetNetwork(replayNetwork{cfg: cfg})
	}
	return req
}

func TestIsReplaySafe(t *testing.T) {
	t.Parallel()
	generic := &common.NetworkConfig{
		Architecture: common.ArchitectureJsonRpc,
		Generic:      &common.GenericNetworkConfig{Slug: "starknet", ReplaySafeMethods: []string{"starknet_blockNumber", "starknet_get*"}},
	}
	assert.True(t, IsReplaySafe(reqOn(generic, "starknet_blockNumber")), "exact declaration")
	assert.True(t, IsReplaySafe(reqOn(generic, "starknet_getBlockWithTxHashes")), "wildcard declaration")
	assert.False(t, IsReplaySafe(reqOn(generic, "starknet_addInvokeTransaction")), "undeclared method")
	assert.False(t, IsReplaySafe(reqOn(generic, "createItem")), "unknown method")

	undeclared := &common.NetworkConfig{Architecture: common.ArchitectureJsonRpc, Generic: &common.GenericNetworkConfig{Slug: "near"}}
	assert.False(t, IsReplaySafe(reqOn(undeclared, "status")), "no declarations: nothing is replay-safe")

	evm := &common.NetworkConfig{Architecture: common.ArchitectureEvm}
	assert.True(t, IsReplaySafe(reqOn(evm, "eth_sendRawTransaction")), "non-jsonrpc networks: their own write sets govern")

	assert.True(t, IsReplaySafe(reqOn(nil, "starknet_blockNumber")), "no network bound: generic policy does not apply")
	assert.True(t, IsReplaySafe(nil))
}

func TestReplayGuard_WrapsUndeclaredErrors(t *testing.T) {
	t.Parallel()
	generic := &common.NetworkConfig{
		Architecture: common.ArchitectureJsonRpc,
		Generic:      &common.GenericNetworkConfig{Slug: "starknet", ReplaySafeMethods: []string{"starknet_get*"}},
	}
	upstreamErr := common.NewErrEndpointServerSideException(errors.New("boom"), nil, 500)

	_, err := upstreamPostForward_replayGuard(reqOn(generic, "starknet_addInvokeTransaction"), nil, upstreamErr)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException), "undeclared: sweep must stop, got %v", err)
	assert.False(t, common.IsRetryableTowardNetwork(err))
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException), "the upstream failure stays visible as the cause")

	_, err = upstreamPostForward_replayGuard(reqOn(generic, "starknet_getBlockWithTxHashes"), nil, upstreamErr)
	assert.Same(t, upstreamErr, err, "declared: the error is passed through untouched")

	_, err = upstreamPostForward_replayGuard(reqOn(generic, "starknet_addInvokeTransaction"), nil, nil)
	assert.NoError(t, err)

	preDispatch := common.NewErrUpstreamMethodIgnored("starknet_addInvokeTransaction", "rpc1")
	_, err = upstreamPostForward_replayGuard(reqOn(generic, "starknet_addInvokeTransaction"), nil, preDispatch)
	assert.Same(t, preDispatch, err, "a call that never reached the wire cannot have taken effect")
}
