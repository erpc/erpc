package thirdparty

import (
	"context"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gockProbeReply mocks one JSON-RPC endpoint (host+path) with a raw body for
// the eth_chainId probes issued by vendor SupportsNetwork implementations.
func gockProbeReply(host, path, body string) {
	gock.New(host).Post(path).Persist().Reply(http.StatusOK).
		SetHeader("Content-Type", "application/json").BodyString(body)
}

func TestRoutemeshVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	probeCases := []struct {
		name      string
		settings  common.VendorSettings
		host      string
		path      string
		body      string
		networkId string
		want      bool
		wantErr   bool
	}{
		{"default base url match", common.VendorSettings{"apiKey": "k1"}, "https://lb.routemes.sh", "/rpc/1/k1", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, "evm:1", true, false},
		{"custom base url match", common.VendorSettings{"apiKey": "k1", "baseURL": "eu.routemes.sh"}, "https://eu.routemes.sh", "/rpc/10/k1", `{"jsonrpc":"2.0","id":1,"result":"0xa"}`, "evm:10", true, false},
		{"mismatching chain id", common.VendorSettings{"apiKey": "k1"}, "https://lb.routemes.sh", "/rpc/1/k1", `{"jsonrpc":"2.0","id":1,"result":"0x89"}`, "evm:1", false, false},
		{"json-rpc error", common.VendorSettings{"apiKey": "k1"}, "https://lb.routemes.sh", "/rpc/1/k1", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`, "evm:1", false, true},
		{"non-hex result", common.VendorSettings{"apiKey": "k1"}, "https://lb.routemes.sh", "/rpc/1/k1", `{"jsonrpc":"2.0","id":1,"result":"zz"}`, "evm:1", false, true},
		{"non-string result", common.VendorSettings{"apiKey": "k1"}, "https://lb.routemes.sh", "/rpc/1/k1", `{"jsonrpc":"2.0","id":1,"result":{}}`, "evm:1", false, true},
	}
	for _, c := range probeCases {
		t.Run(c.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			gockProbeReply(c.host, c.path, c.body)

			ok, err := CreateRoutemeshVendor().SupportsNetwork(ctx, &logger, c.settings, c.networkId)
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
		gockProbeReply("https://lb.routemes.sh", "/rpc/1/k1", `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
		v := CreateRoutemeshVendor()
		for range 2 {
			ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, "evm:1")
			require.NoError(t, err)
			assert.True(t, ok)
		}
	})

	t.Run("unreachable endpoint", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://lb.routemes.sh").Post("/rpc/1/k1").Persist().ReplyError(assert.AnError)
		ok, err := CreateRoutemeshVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-evm network", func(t *testing.T) {
		ok, err := CreateRoutemeshVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, "solana:mainnet")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("unparsable chain id", func(t *testing.T) {
		ok, err := CreateRoutemeshVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1"}, "evm:one")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("missing apiKey", func(t *testing.T) {
		ok, err := CreateRoutemeshVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
		assert.ErrorContains(t, err, "apiKey is required in routemesh settings")
		assert.False(t, ok)
	})

	t.Run("malformed base url", func(t *testing.T) {
		ok, err := CreateRoutemeshVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k1", "baseURL": "bad%zzhost"}, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})
}

func TestRoutemeshVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		name     string
		endpoint string
		settings common.VendorSettings
		expected string
	}{
		{"default base url", "", common.VendorSettings{"apiKey": "k1"}, "https://lb.routemes.sh/rpc/137/k1"},
		{"custom base url", "", common.VendorSettings{"apiKey": "k1", "baseURL": "eu.routemes.sh"}, "https://eu.routemes.sh/rpc/137/k1"},
		{"preset endpoint passes through", "https://lb.routemes.sh/rpc/137/other", nil, "https://lb.routemes.sh/rpc/137/other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ups := &common.UpstreamConfig{Endpoint: c.endpoint, Evm: &common.EvmUpstreamConfig{ChainId: 137}}
			cfgs, err := CreateRoutemeshVendor().GenerateConfigs(ctx, &logger, ups, c.settings)
			require.NoError(t, err)
			require.Len(t, cfgs, 1)
			assert.Equal(t, c.expected, cfgs[0].Endpoint)
			assert.NotNil(t, cfgs[0].JsonRpc)
		})
	}

	t.Run("generated endpoint is typed evm", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}}
		cfgs, err := CreateRoutemeshVendor().GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "k1"})
		require.NoError(t, err)
		assert.Equal(t, common.UpstreamTypeEvm, cfgs[0].Type)
	})

	errCases := []struct {
		name     string
		chainId  int64
		settings common.VendorSettings
		contains string
	}{
		{"missing apiKey", 137, common.VendorSettings{}, "apiKey is required in routemesh settings"},
		{"missing chain id", 0, common.VendorSettings{"apiKey": "k1"}, "requires upstream.evm.chainId"},
		{"malformed base url", 137, common.VendorSettings{"apiKey": "k1", "baseURL": "bad%zzhost"}, "invalid URL escape"},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: c.chainId}}
			_, err := CreateRoutemeshVendor().GenerateConfigs(ctx, &logger, ups, c.settings)
			assert.ErrorContains(t, err, c.contains)
		})
	}
}

func TestRoutemeshVendor_ErrorsAndOwnership(t *testing.T) {
	v := CreateRoutemeshVendor()
	assert.Equal(t, "routemesh", v.Name())

	err, details := vendorErr(t, v, "eth_getLogs", 500, -32000, "boom", "d")
	assert.NoError(t, err)
	assert.Empty(t, details)

	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{VendorName: "routemesh"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://lb.routemes.sh/rpc/1/k"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://rpc.example.com"}))
}
