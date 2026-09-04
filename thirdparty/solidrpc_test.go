package thirdparty

import (
	"context"
	"strconv"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSolidrpcVendor(t *testing.T) {
	vendor := CreateSolidrpcVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("supports catalog networks", func(t *testing.T) {
		require.Len(t, solidrpcChainIDs, 55)
		for chainID := range solidrpcChainIDs {
			supported, err := vendor.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:"+strconv.FormatInt(chainID, 10))
			require.NoError(t, err)
			assert.True(t, supported, chainID)
		}
	})

	t.Run("rejects unsupported networks", func(t *testing.T) {
		for _, networkID := range []string{"solana:mainnet", "evm:999999", "evm:not-a-number"} {
			supported, _ := vendor.SupportsNetwork(ctx, &logger, common.VendorSettings{}, networkID)
			assert.False(t, supported, networkID)
		}
	})

	t.Run("generates authenticated endpoint", func(t *testing.T) {
		configs, err := vendor.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{ChainId: 8453},
		}, common.VendorSettings{"apiKey": "ak_test_key"})
		require.NoError(t, err)
		require.Len(t, configs, 1)
		assert.Equal(t, "https://rpc.solidrpc.io/ak_test_key/evm/8453", configs[0].Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
		assert.NotNil(t, configs[0].JsonRpc)
	})

	t.Run("validates settings and chain", func(t *testing.T) {
		_, err := vendor.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{ChainId: 1},
		}, common.VendorSettings{})
		assert.ErrorContains(t, err, "apiKey is required")

		_, err = vendor.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, common.VendorSettings{"apiKey": "key"})
		assert.ErrorContains(t, err, "upstream.evm")

		_, err = vendor.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{ChainId: 999999},
		}, common.VendorSettings{"apiKey": "key"})
		assert.ErrorContains(t, err, "unsupported network")
	})

	t.Run("identifies shorthand and generated endpoints", func(t *testing.T) {
		for _, endpoint := range []string{
			"solidrpc://key",
			"evm+solidrpc://key",
			"https://rpc.solidrpc.io/key/evm/1",
		} {
			assert.True(t, vendor.OwnsUpstream(&common.UpstreamConfig{Endpoint: endpoint}), endpoint)
		}
		assert.False(t, vendor.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://example.com"}))
	})
}
