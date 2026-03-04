package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterUpstreamsByRequiredCapabilities(t *testing.T) {
	upArchive := common.NewFakeUpstream("archive")
	upArchive.Config().Capabilities = []string{"archive"}
	upTrace := common.NewFakeUpstream("trace")
	upTrace.Config().Capabilities = []string{"trace", "archive"}
	upBasic := common.NewFakeUpstream("basic")

	in := []common.Upstream{upArchive, upTrace, upBasic}

	filtered := filterUpstreamsByRequiredCapabilities(in, []string{"trace"})
	require.Len(t, filtered, 1)
	assert.Equal(t, "trace", filtered[0].Id())

	filtered = filterUpstreamsByRequiredCapabilities(in, []string{"archive"})
	require.Len(t, filtered, 2)
	assert.Equal(t, "archive", filtered[0].Id())
	assert.Equal(t, "trace", filtered[1].Id())

	filtered = filterUpstreamsByRequiredCapabilities(in, nil)
	require.Len(t, filtered, 3)
}

func TestNetworkForward_MethodCapabilitiesMismatchPrevented(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	methodCfg := *common.DefaultStaticCacheMethods["eth_chainId"]
	methodCfg.Requires = []string{"trace"}

	network := setupTestNetworkSimple(t, ctx, &common.UpstreamConfig{
		Id:           "rpc-no-trace",
		Type:         common.UpstreamTypeEvm,
		Endpoint:     "http://rpc1.localhost",
		Capabilities: []string{"archive"},
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Methods: &common.MethodsConfig{
			PreserveDefaultMethods: true,
			Definitions: map[string]*common.CacheMethodConfig{
				"eth_chainId": &methodCfg,
			},
		},
	})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	resp, err := network.Forward(ctx, req)

	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, "ErrFailsafeConfiguration"), "unexpected error: %v", err)
	assert.Contains(t, err.Error(), "required capabilities")
	assert.Nil(t, resp)
}
