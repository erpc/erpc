package thirdparty

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const envioUnknownChain = "evm:999888" // 0xf41d0, not in envioKnownSupportedChains

func TestEnvioVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("known chain short-circuits without probe", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://1.rpc.hypersync.xyz").Persist().ReplyError(assert.AnError)
		ok, err := CreateEnvioVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	probeCases := []struct {
		name     string
		settings common.VendorSettings
		host     string
		path     string
		body     string
		want     bool
		wantErr  bool
	}{
		{"default root domain with api key", common.VendorSettings{"apiKey": "k1"}, "https://999888.rpc.hypersync.xyz", "/k1", `{"jsonrpc":"2.0","id":1,"result":"0xf41d0"}`, true, false},
		{"custom root domain without api key", common.VendorSettings{"rootDomain": "rpc.example.dev"}, "https://999888.rpc.example.dev", "", `{"jsonrpc":"2.0","id":1,"result":"0xf41d0"}`, true, false},
		{"mismatching chain id", common.VendorSettings{"apiKey": "k1"}, "https://999888.rpc.hypersync.xyz", "/k1", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, false, false},
		{"json-rpc error", common.VendorSettings{"apiKey": "k1"}, "https://999888.rpc.hypersync.xyz", "/k1", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`, false, true},
		{"non-hex result", common.VendorSettings{"apiKey": "k1"}, "https://999888.rpc.hypersync.xyz", "/k1", `{"jsonrpc":"2.0","id":1,"result":"qq"}`, false, true},
		{"non-string result", common.VendorSettings{"apiKey": "k1"}, "https://999888.rpc.hypersync.xyz", "/k1", `{"jsonrpc":"2.0","id":1,"result":[]}`, false, true},
	}
	for _, c := range probeCases {
		t.Run(c.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			gockProbeReply(c.host, c.path, c.body)

			ok, err := CreateEnvioVendor().SupportsNetwork(ctx, &logger, c.settings, envioUnknownChain)
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
		gockProbeReply("https://999888.rpc.hypersync.xyz", "/k1", `{"jsonrpc":"2.0","id":1,"result":"0xf41d0"}`)
		v := CreateEnvioVendor()
		for range 2 {
			ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, envioUnknownChain)
			require.NoError(t, err)
			assert.True(t, ok)
		}
	})

	t.Run("certificate failure means unsupported", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://999888.rpc.hypersync.xyz").Post("/k1").Persist().
			ReplyError(errors.New("tls: failed to verify certificate: x509: certificate is valid for other.host"))
		ok, err := CreateEnvioVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, envioUnknownChain)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("other transport failure is an error", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://999888.rpc.hypersync.xyz").Post("/k1").Persist().ReplyError(errors.New("connection refused"))
		ok, err := CreateEnvioVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, envioUnknownChain)
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-evm network", func(t *testing.T) {
		ok, err := CreateEnvioVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, "solana:mainnet")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("unparsable chain id", func(t *testing.T) {
		ok, err := CreateEnvioVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:z")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("malformed api key", func(t *testing.T) {
		ok, err := CreateEnvioVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "%zz"}, envioUnknownChain)
		assert.Error(t, err)
		assert.False(t, ok)
	})
}

func TestEnvioVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		name     string
		settings common.VendorSettings
		expected string
	}{
		{"default root domain with api key", common.VendorSettings{"apiKey": "k1"}, "https://42161.rpc.hypersync.xyz/k1"},
		{"default root domain without api key", common.VendorSettings{}, "https://42161.rpc.hypersync.xyz"},
		{"custom root domain", common.VendorSettings{"rootDomain": "rpc.example.dev", "apiKey": "k1"}, "https://42161.rpc.example.dev/k1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
			cfgs, err := CreateEnvioVendor().GenerateConfigs(ctx, &logger, ups, c.settings)
			require.NoError(t, err)
			require.Len(t, cfgs, 1)
			assert.Equal(t, c.expected, cfgs[0].Endpoint)
			assert.Equal(t, common.UpstreamTypeEvm, cfgs[0].Type)
			assert.Equal(t, []string{"*"}, cfgs[0].IgnoreMethods)
			assert.Contains(t, cfgs[0].AllowMethods, "eth_getLogs")
			assert.NotContains(t, cfgs[0].AllowMethods, "eth_call")
			assert.NotNil(t, cfgs[0].JsonRpc)
		})
	}

	t.Run("preset endpoint and user method filters are kept", func(t *testing.T) {
		ups := &common.UpstreamConfig{
			Endpoint:      "https://1.rpc.hypersync.xyz/mine",
			Evm:           &common.EvmUpstreamConfig{ChainId: 1},
			IgnoreMethods: []string{"eth_getLogs"},
			AllowMethods:  []string{"eth_blockNumber"},
		}
		cfgs, err := CreateEnvioVendor().GenerateConfigs(ctx, &logger, ups, nil)
		require.NoError(t, err)
		assert.Equal(t, "https://1.rpc.hypersync.xyz/mine", cfgs[0].Endpoint)
		assert.Equal(t, []string{"eth_getLogs"}, cfgs[0].IgnoreMethods)
		assert.Equal(t, []string{"eth_blockNumber"}, cfgs[0].AllowMethods)
	})

	t.Run("missing chain id", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}
		_, err := CreateEnvioVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{})
		assert.ErrorContains(t, err, "requires upstream.evm.chainId")
	})

	t.Run("malformed api key", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := CreateEnvioVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "%zz"})
		assert.ErrorContains(t, err, "invalid URL escape")
	})
}

func TestEnvioVendor_ErrorsAndOwnership(t *testing.T) {
	v := CreateEnvioVendor()
	assert.Equal(t, "envio", v.Name())

	err, details := vendorErr(t, v, "eth_getLogs", 500, -32000, "boom", "d")
	assert.NoError(t, err)
	assert.Empty(t, details)

	for _, ep := range []string{"envio://x", "evm+envio://x", "https://indexer.envio.dev/x", "https://1.rpc.hypersync.xyz"} {
		assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: ep}), ep)
	}
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://rpc.example.com"}))
}
