package thirdparty

import (
	"context"
	"net/url"
	"strconv"
	"strings"
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

	t.Run("escapes API keys exactly once as a single path segment", func(t *testing.T) {
		for _, key := range []string{"key/part", "key%2Fpart", "key?x=1#fragment", "key with space", "key+plus", "key@host", "clé"} {
			t.Run(key, func(t *testing.T) {
				configs, err := vendor.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{
					Evm: &common.EvmUpstreamConfig{ChainId: 130},
				}, common.VendorSettings{"apiKey": key})
				require.NoError(t, err)
				u, err := url.Parse(configs[0].Endpoint)
				require.NoError(t, err)
				segments := strings.Split(u.EscapedPath(), "/")
				require.Len(t, segments, 4)
				decoded, err := url.PathUnescape(segments[1])
				require.NoError(t, err)
				assert.Equal(t, key, decoded)
				assert.Equal(t, "rpc.solidrpc.io", u.Host)
				assert.Empty(t, u.RawQuery)
				assert.Empty(t, u.Fragment)
			})
		}
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
		assert.False(t, vendor.OwnsUpstream(nil))
		for _, endpoint := range []string{
			"solidrpc://key",
			"evm+solidrpc://key",
			"https://rpc.solidrpc.io/key/evm/1",
			"https://RPC.SOLIDRPC.IO:443/key/evm/1",
		} {
			assert.True(t, vendor.OwnsUpstream(&common.UpstreamConfig{Endpoint: endpoint}), endpoint)
		}
		for _, endpoint := range []string{
			"https://example.com",
			"https://rpc.solidrpc.io.evil.example/key/evm/1",
			"https://notrpc.solidrpc.io/key/evm/1",
			"https://example.com/rpc.solidrpc.io",
			"https://example.com/?host=rpc.solidrpc.io",
			"https://example.com/#rpc.solidrpc.io",
			"https://rpc.solidrpc.io@evil.example/key/evm/1",
			"https://rpc.solidrpc.io/%zz",
			"ftp://rpc.solidrpc.io/key/evm/1",
			"",
		} {
			assert.False(t, vendor.OwnsUpstream(&common.UpstreamConfig{Endpoint: endpoint}), endpoint)
		}
	})
}
