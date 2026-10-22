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

const pimlicoUnknownChain = "evm:999888" // 0xf41d0, not in pimlicoSupportedChains

func TestPimlicoVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("known chain short-circuits without probe or apiKey", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://api.pimlico.io").Persist().ReplyError(assert.AnError)
		ok, err := CreatePimlicoVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:8453")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	probeCases := []struct {
		name    string
		apiKey  string
		host    string
		path    string
		body    string
		want    bool
		wantErr bool
	}{
		{"api key probe match", "k1", "https://api.pimlico.io", "/v2/999888/rpc", `{"jsonrpc":"2.0","id":1,"result":"0xf41d0"}`, true, false},
		{"public probe match", "public", "https://public.pimlico.io", "/v2/999888/rpc", `{"jsonrpc":"2.0","id":1,"result":"0xf41d0"}`, true, false},
		{"mismatching chain id", "k1", "https://api.pimlico.io", "/v2/999888/rpc", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, false, false},
		{"json-rpc error", "k1", "https://api.pimlico.io", "/v2/999888/rpc", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`, false, true},
		{"non-hex result", "k1", "https://api.pimlico.io", "/v2/999888/rpc", `{"jsonrpc":"2.0","id":1,"result":"xyz"}`, false, true},
		{"non-string result", "k1", "https://api.pimlico.io", "/v2/999888/rpc", `{"jsonrpc":"2.0","id":1,"result":{}}`, false, true},
	}
	for _, c := range probeCases {
		t.Run(c.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			gockProbeReply(c.host, c.path, c.body)

			ok, err := CreatePimlicoVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": c.apiKey}, pimlicoUnknownChain)
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
		gockProbeReply("https://api.pimlico.io", "/v2/999888/rpc", `{"jsonrpc":"2.0","id":1,"result":"0xf41d0"}`)
		v := CreatePimlicoVendor()
		for range 2 {
			ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, pimlicoUnknownChain)
			require.NoError(t, err)
			assert.True(t, ok)
		}
	})

	t.Run("unreachable endpoint", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://api.pimlico.io").Post("/v2/999888/rpc").Persist().ReplyError(assert.AnError)
		ok, err := CreatePimlicoVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, pimlicoUnknownChain)
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-evm network", func(t *testing.T) {
		ok, err := CreatePimlicoVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, "solana:mainnet")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("unparsable chain id", func(t *testing.T) {
		ok, err := CreatePimlicoVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, "evm:x")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("unknown chain without apiKey", func(t *testing.T) {
		ok, err := CreatePimlicoVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, pimlicoUnknownChain)
		assert.ErrorContains(t, err, "apiKey is required in pimlico settings")
		assert.False(t, ok)
	})
}

func TestPimlicoVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("api key endpoint and default method filters", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}}
		cfgs, err := CreatePimlicoVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "k1"})
		require.NoError(t, err)
		require.Len(t, cfgs, 1)
		assert.Equal(t, "https://api.pimlico.io/v2/137/rpc?apikey=k1", cfgs[0].Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, cfgs[0].Type)
		assert.Equal(t, []string{"*"}, cfgs[0].IgnoreMethods)
		assert.Contains(t, cfgs[0].AllowMethods, "eth_sendUserOperation")
		assert.Contains(t, cfgs[0].AllowMethods, "pm_sponsorUserOperation")
		assert.NotContains(t, cfgs[0].AllowMethods, "eth_getLogs")
	})

	t.Run("public endpoint", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}
		cfgs, err := CreatePimlicoVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "public"})
		require.NoError(t, err)
		assert.Equal(t, "https://public.pimlico.io/v2/10/rpc", cfgs[0].Endpoint)
	})

	t.Run("preset endpoint and user method filters are kept", func(t *testing.T) {
		ups := &common.UpstreamConfig{
			Endpoint:      "https://api.pimlico.io/v2/1/rpc?apikey=mine",
			Evm:           &common.EvmUpstreamConfig{ChainId: 1},
			IgnoreMethods: []string{"pm_*"},
			AllowMethods:  []string{"eth_chainId"},
		}
		cfgs, err := CreatePimlicoVendor().GenerateConfigs(ctx, &logger, ups, nil)
		require.NoError(t, err)
		assert.Equal(t, "https://api.pimlico.io/v2/1/rpc?apikey=mine", cfgs[0].Endpoint)
		assert.Equal(t, []string{"pm_*"}, cfgs[0].IgnoreMethods)
		assert.Equal(t, []string{"eth_chainId"}, cfgs[0].AllowMethods)
		assert.NotNil(t, cfgs[0].JsonRpc)
	})

	t.Run("missing apiKey", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := CreatePimlicoVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		assert.ErrorContains(t, err, "apiKey is required in pimlico settings")
	})

	t.Run("missing chain id", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}
		_, err := CreatePimlicoVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "k1"})
		assert.ErrorContains(t, err, "requires upstream.evm.chainId")
	})
}

func TestPimlicoVendor_ErrorsAndOwnership(t *testing.T) {
	v := CreatePimlicoVendor()
	assert.Equal(t, "pimlico", v.Name())

	err, details := vendorErr(t, v, "eth_sendUserOperation", 500, -32000, "boom", "d")
	assert.NoError(t, err)
	assert.Empty(t, details)

	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "pimlico://k"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+pimlico://k"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://api.pimlico.io/v2/1/rpc"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://rpc.example.com"}))
}
