package thirdparty

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThirdwebVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	settings := common.VendorSettings{"clientId": "cid"}

	probeCases := []struct {
		name    string
		body    string
		want    bool
		wantErr bool
	}{
		{"matching chain id", `{"jsonrpc":"2.0","id":1,"result":"0x2105"}`, true, false},
		{"mismatching chain id", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, false, false},
		{"json-rpc error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`, false, true},
		{"non-hex result", `{"jsonrpc":"2.0","id":1,"result":"base"}`, false, true},
		{"non-string result", `{"jsonrpc":"2.0","id":1,"result":true}`, false, true},
	}
	for _, c := range probeCases {
		t.Run(c.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			gockProbeReply("https://8453.rpc.thirdweb.com", "/cid", c.body)

			ok, err := CreateThirdwebVendor().SupportsNetwork(ctx, &logger, settings, "evm:8453")
			if c.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, c.want, ok)
		})
	}

	t.Run("cached client reused", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gockProbeReply("https://8453.rpc.thirdweb.com", "/cid", `{"jsonrpc":"2.0","id":1,"result":"0x2105"}`)
		v := CreateThirdwebVendor()
		for range 2 {
			ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:8453")
			require.NoError(t, err)
			assert.True(t, ok)
		}
	})

	t.Run("unreachable endpoint", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://8453.rpc.thirdweb.com").Post("/cid").Persist().ReplyError(assert.AnError)
		ok, err := CreateThirdwebVendor().SupportsNetwork(ctx, &logger, settings, "evm:8453")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-evm network", func(t *testing.T) {
		ok, err := CreateThirdwebVendor().SupportsNetwork(ctx, &logger, settings, "solana:mainnet")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("unparsable chain id", func(t *testing.T) {
		ok, err := CreateThirdwebVendor().SupportsNetwork(ctx, &logger, settings, "evm:base")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("missing clientId", func(t *testing.T) {
		ok, err := CreateThirdwebVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:8453")
		assert.ErrorContains(t, err, "clientId is required in thirdweb settings")
		assert.False(t, ok)
	})

	t.Run("malformed clientId", func(t *testing.T) {
		ok, err := CreateThirdwebVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"clientId": "%zz"}, "evm:8453")
		assert.Error(t, err)
		assert.False(t, ok)
	})
}

func TestThirdwebVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("generated endpoint", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		cfgs, err := CreateThirdwebVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"clientId": "cid"})
		require.NoError(t, err)
		require.Len(t, cfgs, 1)
		assert.Equal(t, "https://42161.rpc.thirdweb.com/cid", cfgs[0].Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, cfgs[0].Type)
		assert.NotNil(t, cfgs[0].JsonRpc)
	})

	t.Run("preset endpoint passes through", func(t *testing.T) {
		ups := &common.UpstreamConfig{Endpoint: "https://1.rpc.thirdweb.com/other", Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		cfgs, err := CreateThirdwebVendor().GenerateConfigs(ctx, &logger, ups, nil)
		require.NoError(t, err)
		assert.Equal(t, "https://1.rpc.thirdweb.com/other", cfgs[0].Endpoint)
	})

	t.Run("missing clientId", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := CreateThirdwebVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		assert.ErrorContains(t, err, "clientId is required in thirdweb settings")
	})

	t.Run("malformed clientId", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := CreateThirdwebVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"clientId": "%zz"})
		assert.ErrorContains(t, err, "invalid URL escape")
	})
}

func TestThirdwebVendor_ErrorsAndOwnership(t *testing.T) {
	v := CreateThirdwebVendor()
	assert.Equal(t, "thirdweb", v.Name())

	err, details := vendorErr(t, v, "eth_getLogs", 500, -32000, "boom", "d")
	assert.NoError(t, err)
	assert.Empty(t, details)

	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "thirdweb://cid"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+thirdweb://cid"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://1.rpc.thirdweb.com/cid"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://rpc.example.com"}))
}
