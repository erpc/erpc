package thirdparty

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestBlockdaemonVendor_SupportsNetwork(t *testing.T) {
	vendor := CreateBlockdaemonVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		networkId string
		expected  bool
	}{
		{"evm:1", true},          // Ethereum mainnet
		{"evm:8453", true},       // Base
		{"evm:42161", true},      // Arbitrum
		{"evm:10", true},         // Optimism
		{"evm:43114", true},      // Avalanche C-Chain
		{"evm:137", true},        // Polygon
		{"evm:80002", true},      // Polygon Amoy
		{"evm:11155111", true},   // Sepolia
		{"evm:999999999", false}, // unknown
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

func TestBlockdaemonVendor_GenerateConfigs(t *testing.T) {
	vendor := CreateBlockdaemonVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("requires apiKey", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "apiKey is required")
	})

	t.Run("rejects unsupported chain", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 999999999}}
		_, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "key"})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported network chain ID")
	})

	endpointCases := []struct {
		name     string
		chainId  int64
		endpoint string
	}{
		{"ethereum", 1, "https://svc.blockdaemon.com/ethereum/mainnet/native"},
		{"base", 8453, "https://svc.blockdaemon.com/base/mainnet/native/http-rpc"},
		{"optimism", 10, "https://svc.blockdaemon.com/optimism/mainnet/native/http-rpc"},
		{"arbitrum", 42161, "https://svc.blockdaemon.com/arbitrum/mainnet-one/native/http-rpc"},
		{"avalanche", 43114, "https://svc.blockdaemon.com/avalanche/mainnet/native/ext/bc/c/eth"},
		{"polygon", 137, "https://svc.blockdaemon.com/polygon/mainnet/native/http-rpc"},
		{"polygon-amoy", 80002, "https://svc.blockdaemon.com/polygon/amoy/native/http-rpc"},
		{"tron", 728126428, "https://svc.blockdaemon.com/tron/mainnet/native/jsonrpc"},
		{"tron-nile", 3448148188, "https://svc.blockdaemon.com/tron/nile/native/jsonrpc"},
	}
	for _, c := range endpointCases {
		t.Run("builds "+c.name, func(t *testing.T) {
			ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: c.chainId}}
			cfgs, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "secret-key"})
			assert.NoError(t, err)
			assert.Len(t, cfgs, 1)
			assert.Equal(t, c.endpoint, cfgs[0].Endpoint)
			assert.Equal(t, common.UpstreamTypeEvm, cfgs[0].Type)
			assert.Equal(t, "Bearer secret-key", cfgs[0].JsonRpc.Headers["Authorization"])
		})
	}

	t.Run("passes through preset endpoint", func(t *testing.T) {
		ups := &common.UpstreamConfig{
			Endpoint: "https://svc.blockdaemon.com/base/mainnet/native/http-rpc",
			Evm:      &common.EvmUpstreamConfig{ChainId: 8453},
		}
		cfgs, err := vendor.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "k"})
		assert.NoError(t, err)
		assert.Len(t, cfgs, 1)
		assert.Equal(t, ups.Endpoint, cfgs[0].Endpoint)
	})
}

func TestBlockdaemonVendor_OwnsUpstream(t *testing.T) {
	vendor := CreateBlockdaemonVendor()
	cases := []struct {
		endpoint string
		owned    bool
	}{
		{"blockdaemon://abc", true},
		{"evm+blockdaemon://abc", true},
		{"https://svc.blockdaemon.com/ethereum/mainnet/native", true},
		{"https://rpc.ankr.com/eth/abc", false},
	}
	for _, c := range cases {
		t.Run(c.endpoint, func(t *testing.T) {
			got := vendor.OwnsUpstream(&common.UpstreamConfig{Endpoint: c.endpoint})
			assert.Equal(t, c.owned, got)
		})
	}
}
