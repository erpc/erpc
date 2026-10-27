package thirdparty

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const conduitNetworksBody = `{"endpoints":[
	{"id":"a","name":"Zora","chainId":"7777777","httpEndpoint":"https://rpc.zora.conduit.example"},
	{"id":"b","name":"NoHttp","chainId":"42","httpEndpoint":""},
	{"id":"c","name":"Bad","chainId":"not-a-number","httpEndpoint":"https://bad.example"},
	{"id":"d","name":"Zero","chainId":"0","httpEndpoint":"https://zero.example"}
]}`

func TestConduitVendor_FetchNetworks(t *testing.T) {
	v := CreateConduitVendor().(*ConduitVendor)
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("keeps networks with valid chain id and http endpoint", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, conduitNetworksBody)
		got, err := v.fetchConduitNetworks(ctx, &logger, srv.URL)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Contains(t, got, int64(7777777))
		assert.Equal(t, "https://rpc.zora.conduit.example", got[7777777].HttpEndpoint)
	})

	t.Run("non-2xx status is an error", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusServiceUnavailable, `{}`)
		_, err := v.fetchConduitNetworks(ctx, &logger, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "503")
	})

	t.Run("invalid json is an error", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `[1,2`)
		_, err := v.fetchConduitNetworks(ctx, &logger, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse Conduit API data")
	})

	t.Run("invalid url and unreachable host are errors", func(t *testing.T) {
		_, err := v.fetchConduitNetworks(ctx, &logger, "http://bad host/")
		assert.Error(t, err)
		_, err = v.fetchConduitNetworks(ctx, &logger, "http://127.0.0.1:1/")
		assert.Error(t, err)
	})
}

func TestConduitVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("rejects non-evm and malformed ids", func(t *testing.T) {
		v := CreateConduitVendor()
		ok, err := v.SupportsNetwork(ctx, &logger, nil, "solana:mainnet")
		assert.NoError(t, err)
		assert.False(t, ok)
		_, err = v.SupportsNetwork(ctx, &logger, nil, "evm:x")
		assert.Error(t, err)
	})

	t.Run("cold cache then populated from networks url", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, conduitNetworksBody)
		v := CreateConduitVendor()
		settings := common.VendorSettings{"networksUrl": srv.URL, "recheckInterval": time.Hour}

		ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:7777777")
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
		assert.False(t, ok)

		require.Eventually(t, func() bool {
			ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:7777777")
			return err == nil && ok
		}, 5*time.Second, 10*time.Millisecond)

		ok, err = v.SupportsNetwork(ctx, &logger, settings, "evm:42")
		assert.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("default networks url is used when unset", func(t *testing.T) {
		v := CreateConduitVendor().(*ConduitVendor)
		primeRemoteCache(t, v.cache, DefaultConduitNetworksUrl, map[int64]*ConduitNetwork{
			1: {ChainID: "1", HttpEndpoint: "https://one.example"},
			2: nil,
		})
		ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
		require.NoError(t, err)
		assert.True(t, ok)
		ok, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:2")
		require.NoError(t, err)
		assert.False(t, ok, "nil network entry is unsupported")
	})
}

func TestConduitVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	const networksUrl = "https://conduit.example/networks"

	newPrimed := func(t *testing.T) *ConduitVendor {
		v := CreateConduitVendor().(*ConduitVendor)
		primeRemoteCache(t, v.cache, networksUrl, map[int64]*ConduitNetwork{
			7777777: {ChainID: "7777777", HttpEndpoint: "https://rpc.zora.conduit.example"},
		})
		return v
	}

	t.Run("appends api key to the network endpoint", func(t *testing.T) {
		v := newPrimed(t)
		ups := &common.UpstreamConfig{Id: "zora", Evm: &common.EvmUpstreamConfig{ChainId: 7777777}}
		got, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{
			"apiKey": "k1", "networksUrl": networksUrl, "recheckInterval": time.Hour,
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "https://rpc.zora.conduit.example/k1", got[0].Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, got[0].Type)
		assert.Equal(t, "conduit", got[0].VendorName)
		assert.Equal(t, "zora", got[0].Id)
		assert.Empty(t, ups.Endpoint, "input upstream is not mutated with the endpoint")
	})

	t.Run("preset endpoint passes through", func(t *testing.T) {
		v := CreateConduitVendor()
		ups := &common.UpstreamConfig{Endpoint: "https://rpc.custom.example/key"}
		got, err := v.GenerateConfigs(ctx, &logger, ups, nil)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Same(t, ups, got[0])
		assert.NotNil(t, got[0].JsonRpc)
	})

	t.Run("validation errors", func(t *testing.T) {
		v := newPrimed(t)
		ok := common.VendorSettings{"apiKey": "k", "networksUrl": networksUrl}

		_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, ok)
		assert.ErrorContains(t, err, "requires upstream.evm to be defined")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, ok)
		assert.ErrorContains(t, err, "requires upstream.evm.chainId")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 7777777}}, nil)
		assert.ErrorContains(t, err, "apiKey is required")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 5}}, ok)
		assert.ErrorContains(t, err, "chain ID 5 not found")
	})

	t.Run("cold cache is retryable", func(t *testing.T) {
		v := CreateConduitVendor()
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "k", "networksUrl": "http://127.0.0.1:1/"})
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
	})
}

func TestConduitVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateConduitVendor()

	cases := []struct {
		name string
		code int
		msg  string
		want common.ErrorCode
	}{
		{"unauthenticated", -32600, "request must be authenticated", common.ErrCodeEndpointUnauthorized},
		{"bad access key", -32600, "invalid access key", common.ErrCodeEndpointUnauthorized},
		{"bad api key", -32600, "missing api key", common.ErrCodeEndpointUnauthorized},
		{"limit exceeded", -32005, "rate limit exceeded", common.ErrCodeEndpointCapacityExceeded},
		{"capacity limit", 429, "capacity limit reached", common.ErrCodeEndpointCapacityExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err, details := vendorErr(t, v, "eth_call", 500, c.code, c.msg, "extra")
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, c.want), "got %v", err)
			assert.Equal(t, "extra", details["data"])
		})
	}

	t.Run("unrelated errors fall through", func(t *testing.T) {
		err, _ := vendorErr(t, v, "eth_call", 400, -32600, "invalid request", "")
		assert.NoError(t, err)
		err, _ = vendorErr(t, v, "eth_call", 400, 3, "execution reverted", "")
		assert.NoError(t, err)
		assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, nil, "not a jrr", map[string]interface{}{}))
	})
}

func TestConduitVendor_OwnsUpstream(t *testing.T) {
	v := CreateConduitVendor()
	assert.Equal(t, "conduit", v.Name())
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "conduit://x"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "evm+conduit://x"}))
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://a.example", VendorName: "conduit"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://a.example"}))
}
