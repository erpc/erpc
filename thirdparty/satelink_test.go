package thirdparty

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestSatelinkVendor_SupportsNetwork(t *testing.T) {
	vendor := CreateSatelinkVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		networkId string
		expected  bool
	}{
		{"evm:137", true},        // Polygon mainnet
		{"evm:1", false},         // Ethereum — not supported
		{"evm:999999", false},    // unknown
		{"solana:mainnet", false},
	}

	for _, c := range cases {
		t.Run(c.networkId, func(t *testing.T) {
			ok, err := vendor.SupportsNetwork(ctx, &logger, common.VendorSettings{}, c.networkId)
			assert.NoError(t, err)
			assert.Equal(t, c.expected, ok)
		})
	}
}

func TestSatelinkVendor_GenerateConfigs(t *testing.T) {
	vendor := CreateSatelinkVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("paid tier with api key", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}}
		cfgs, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "sk_test_123"})
		assert.NoError(t, err)
		assert.Len(t, cfgs, 1)
		assert.Equal(t, "https://rpc.satelink.network/rpc/polygon", cfgs[0].Endpoint)
		assert.Equal(t, "sk_test_123", cfgs[0].JsonRpc.Headers["X-API-Key"])
	})

	t.Run("free tier without api key", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}}
		cfgs, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		assert.NoError(t, err)
		assert.Len(t, cfgs, 1)
		assert.Equal(t, "https://rpc.satelink.network/rpc/polygon", cfgs[0].Endpoint)
		_, hasKey := cfgs[0].JsonRpc.Headers["X-API-Key"]
		assert.False(t, hasKey)
	})

	t.Run("unsupported chain returns empty", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		cfgs, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "sk_test_123"})
		assert.NoError(t, err)
		assert.Empty(t, cfgs)
	})
}
