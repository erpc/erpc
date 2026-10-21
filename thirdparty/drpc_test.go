package thirdparty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrpcVendor_ColdStartFallback_SupportsNetwork(t *testing.T) {
	prev := swapDrpcNetworksURL(t, "http://127.0.0.1:1/does-not-exist")
	defer swapDrpcNetworksURL(t, prev)

	vendor := CreateDrpcVendor().(*DrpcVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	settings := common.VendorSettings{
		"recheckInterval": 24 * time.Hour,
	}

	supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:1")
	require.NoError(t, err, "cold-start fallback should not surface a fetch error")
	assert.True(t, supported, "chain 1 (ethereum) is in defaultDrpcNetworkNames")

	supported, err = vendor.SupportsNetwork(ctx, &logger, settings, "evm:999999999999")
	require.NoError(t, err)
	assert.False(t, supported)
}

func TestDrpcVendor_ColdStartFallback_GenerateConfigs(t *testing.T) {
	prev := swapDrpcNetworksURL(t, "http://127.0.0.1:1/does-not-exist")
	defer swapDrpcNetworksURL(t, prev)

	vendor := CreateDrpcVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	settings := common.VendorSettings{
		"apiKey":          "test-key",
		"recheckInterval": 24 * time.Hour,
	}

	upstream := &common.UpstreamConfig{
		Evm: &common.EvmUpstreamConfig{ChainId: 1},
	}

	configs, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Contains(t, configs[0].Endpoint, "lb.drpc.org")
	assert.Contains(t, configs[0].Endpoint, "ethereum")
	assert.Contains(t, configs[0].Endpoint, "test-key")
}

func TestDrpcVendor_SuccessfulFetchPromotesOverFallback(t *testing.T) {
	fetched := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"custom","label":"Custom","chains":[{"name":"custom-net","chain_id":"0x67932","priority":100,"api_type":"jsonrpc","blockchain_type":"eth","has_premium":true}]}]`))
		select {
		case fetched <- struct{}{}:
		default:
		}
	}))
	defer server.Close()

	prev := swapDrpcNetworksURL(t, server.URL)
	defer swapDrpcNetworksURL(t, prev)

	vendor := CreateDrpcVendor().(*DrpcVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	settings := common.VendorSettings{"recheckInterval": 24 * time.Hour}

	// First call kicks off the async refresh; the synchronous return uses
	// the built-in fallback, which doesn't know about our custom chain.
	// This is by design (see remote_cache.go's request-path safety rule).
	supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:424242")
	require.NoError(t, err)
	assert.False(t, supported, "first call returns built-in fallback, which does not contain the custom chain")

	// Wait for the async refresh to complete, then re-query.
	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("async refresh never hit the mock server")
	}
	// Allow the snapshot to publish after the response body is parsed.
	require.Eventually(t, func() bool {
		ok, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:424242")
		return err == nil && ok
	}, 5*time.Second, 50*time.Millisecond, "async-refreshed snapshot should promote custom chain over fallback")
}

func TestDrpcVendor_ChainsUrlSetting_InvalidURLReturnsError(t *testing.T) {
	vendor := CreateDrpcVendor().(*DrpcVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	for _, badURL := range []string{"not-a-url", "ftp://host", "://missing-scheme"} {
		settings := common.VendorSettings{
			"chainsUrl":       badURL,
			"recheckInterval": 24 * time.Hour,
		}
		_, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:1")
		require.Errorf(t, err, "malformed chainsUrl %q should return an error", badURL)
		assert.Contains(t, err.Error(), "invalid chainsUrl")
	}
}

// swapDrpcNetworksURL temporarily overrides drpcNetworksURL so tests can point
// the vendor at a mock server or a deliberately broken URL.
// Returns the previous value so the caller can restore it.
func swapDrpcNetworksURL(t *testing.T, newURL string) string {
	t.Helper()
	prev := drpcNetworksURL
	drpcNetworksURL = newURL
	return prev
}

// drpcOfflineURL refuses connections instantly so the async refresh kicked
// off by SupportsNetwork / GenerateConfigs never leaves the machine.
const drpcOfflineURL = "http://127.0.0.1:1/drpc-offline"

func TestDrpcVendor_Name(t *testing.T) {
	assert.Equal(t, "drpc", CreateDrpcVendor().Name())
}

func TestDrpcVendor_CreditUnits(t *testing.T) {
	v := CreateDrpcVendor().(*DrpcVendor)
	assert.Equal(t, int64(0), v.CreditUnits(cuReq(t, "eth_chainId"), nil), "informational method is free")
	assert.Equal(t, int64(20), v.CreditUnits(cuReq(t, "eth_getLogs"), nil), "unlisted method uses '*'")
	assert.Equal(t, int64(20), v.CreditUnits(cuReq(t, "eth_getLogs"), &common.UpstreamConfig{}), "no override uses '*'")

	ups := &common.UpstreamConfig{CreditUnits: map[string]int64{"eth_getLogs": 7, "eth_chainId": 3}}
	assert.Equal(t, int64(7), v.CreditUnits(cuReq(t, "eth_getLogs"), ups), "override wins over '*'")
	assert.Equal(t, int64(3), v.CreditUnits(cuReq(t, "eth_chainId"), ups), "override wins over listed method")
	assert.Equal(t, int64(20), v.CreditUnits(cuReq(t, "eth_call"), ups), "non-overridden method keeps '*'")
}

func TestDrpcVendor_SupportsNetworkBranches(t *testing.T) {
	offline := common.VendorSettings{"chainsUrl": drpcOfflineURL}
	svtCheckSupports(t, CreateDrpcVendor(), offline, []svtSupportCase{
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
		{"evm:1", true, false}, // default recheck interval + built-in fallback
	})

	prev := swapDrpcNetworksURL(t, drpcOfflineURL)
	defer swapDrpcNetworksURL(t, prev)
	ok, err := svtSupports(t, CreateDrpcVendor(), common.VendorSettings{}, "evm:8453")
	require.NoError(t, err)
	assert.True(t, ok, "empty settings use drpcNetworksURL with built-in fallback")
}

func TestDrpcVendor_GenerateConfigsBranches(t *testing.T) {
	key := common.VendorSettings{"apiKey": "KEY", "chainsUrl": drpcOfflineURL}

	svtCheckGenErrors(t, CreateDrpcVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{"chainsUrl": drpcOfflineURL}, "apiKey is required in drpc settings"},
		{"nil evm", &common.UpstreamConfig{}, key, "drpc vendor requires upstream.evm to be defined"},
		{"zero chain", svtEvm(0), key, "drpc vendor requires upstream.evm.chainId to be defined"},
		{"invalid chainsUrl", svtEvm(1), common.VendorSettings{"apiKey": "KEY", "chainsUrl": "ftp://host"}, "invalid chainsUrl"},
		{"unsupported chain", svtEvm(999999999999), key, "unsupported network chain ID for DRPC: 999999999999"},
		{"unparsable url", svtEvm(1), common.VendorSettings{"apiKey": "a\nb", "chainsUrl": drpcOfflineURL}, "invalid control character"},
	})

	t.Run("built-in fallback url", func(t *testing.T) {
		cfg, err := svtGenerate(t, CreateDrpcVendor(), svtEvm(42161), key)
		require.NoError(t, err)
		assert.Equal(t, "https://lb.drpc.org/ogrpc?network=arbitrum&dkey=KEY", cfg.Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, cfg.Type)
		require.NotNil(t, cfg.AutoIgnoreUnsupportedMethods)
		assert.False(t, *cfg.AutoIgnoreUnsupportedMethods)
	})

	t.Run("default chainsUrl", func(t *testing.T) {
		prev := swapDrpcNetworksURL(t, drpcOfflineURL)
		defer swapDrpcNetworksURL(t, prev)
		cfg, err := svtGenerate(t, CreateDrpcVendor(), svtEvm(1), common.VendorSettings{"apiKey": "KEY"})
		require.NoError(t, err)
		assert.Equal(t, "https://lb.drpc.org/ogrpc?network=ethereum&dkey=KEY", cfg.Endpoint)
	})

	t.Run("fetched snapshot wins over built-in", func(t *testing.T) {
		v := CreateDrpcVendor().(*DrpcVendor)
		primeRemoteCache(t, v.cache, drpcOfflineURL, map[int64]string{1: "eth-custom"})
		cfg, err := svtGenerate(t, v, svtEvm(1), common.VendorSettings{"apiKey": "KEY", "chainsUrl": drpcOfflineURL, "recheckInterval": time.Hour})
		require.NoError(t, err)
		assert.Equal(t, "https://lb.drpc.org/ogrpc?network=eth-custom&dkey=KEY", cfg.Endpoint)
	})

	t.Run("preset owned endpoint gets evm type", func(t *testing.T) {
		cfg, err := svtGenerate(t, CreateDrpcVendor(), &common.UpstreamConfig{Endpoint: "https://lb.drpc.org/ogrpc?network=base&dkey=x"}, common.VendorSettings{})
		require.NoError(t, err)
		assert.Equal(t, "https://lb.drpc.org/ogrpc?network=base&dkey=x", cfg.Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, cfg.Type)
	})

	t.Run("preset owned endpoint keeps explicit type", func(t *testing.T) {
		ups := &common.UpstreamConfig{Endpoint: "drpc://KEY", Type: "custom"}
		cfg, err := svtGenerate(t, CreateDrpcVendor(), ups, common.VendorSettings{})
		require.NoError(t, err)
		assert.Equal(t, common.UpstreamType("custom"), cfg.Type)
	})

	t.Run("preset foreign endpoint stays untyped", func(t *testing.T) {
		cfg, err := svtGenerate(t, CreateDrpcVendor(), &common.UpstreamConfig{Endpoint: "https://rpc.ankr.com/eth/x"}, common.VendorSettings{})
		require.NoError(t, err)
		assert.Empty(t, cfg.Type)
	})
}

func TestDrpcVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateDrpcVendor()

	err, details := vendorErr(t, v, "eth_getLogs", 400, -32000, "token is invalid", "d")
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "got %v", err)
	assert.Equal(t, common.JsonRpcErrorUnauthorized, normalizedCode(t, err))
	assert.Equal(t, "d", details["data"])

	for _, msg := range []string{"ChainException: Unexpected error (code=40000)", "invalid block range"} {
		err, _ := vendorErr(t, v, "eth_getLogs", 400, -32000, msg, "")
		require.Error(t, err, msg)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "%q -> %v", msg, err)
		assert.Equal(t, common.JsonRpcErrorMissingData, normalizedCode(t, err))
		assert.True(t, common.IsRetryableTowardNetwork(err), "missing data stays network-retryable")
	}

	err, details = vendorErr(t, v, "eth_getLogs", 400, -32000, "other", "d")
	assert.NoError(t, err)
	assert.Equal(t, "d", details["data"])

	err, details = vendorErr(t, v, "eth_getLogs", 400, 0, "token is invalid", "d")
	assert.NoError(t, err, "code 0 is not inspected")
	assert.NotContains(t, details, "data")

	assert.NoError(t, v.GetVendorSpecificErrorIfAny(svtReq(), nil, "x", map[string]interface{}{}))
}

func TestDrpcVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateDrpcVendor(), map[string]bool{
		"drpc://key":     true,
		"evm+drpc://key": true,
		"https://lb.drpc.org/ogrpc?network=ethereum&dkey=key": true,
		"https://rpc.ankr.com/eth/key":                        false,
	})
}

func TestDrpcVendor_FetchDrpcNetworks(t *testing.T) {
	v := CreateDrpcVendor().(*DrpcVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	t.Run("filters and picks highest priority", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `[
			{"id":"a","chains":[
				{"name":"eth-low","chain_id":"0x1","priority":1,"api_type":"jsonrpc","blockchain_type":"eth","has_premium":true},
				{"name":"eth-high","chain_id":"0x1","priority":9,"api_type":"jsonrpc","blockchain_type":"eth","has_premium":true},
				{"name":"eth-mid","chain_id":"0x1","priority":5,"api_type":"jsonrpc","blockchain_type":"eth","has_premium":true},
				{"name":"base","chain_id":"2105","priority":1,"api_type":"jsonrpc","blockchain_type":"eth","has_premium":true}
			]},
			{"id":"b","chains":[
				{"name":"bad-id","chain_id":"0xzz","priority":1,"api_type":"jsonrpc","blockchain_type":"eth","has_premium":true},
				{"name":"no-id","chain_id":"","priority":1,"api_type":"jsonrpc","blockchain_type":"eth","has_premium":true},
				{"name":"free","chain_id":"0xa","priority":1,"api_type":"jsonrpc","blockchain_type":"eth","has_premium":false},
				{"name":"rest","chain_id":"0x89","priority":1,"api_type":"rest","blockchain_type":"eth","has_premium":true},
				{"name":"sol","chain_id":"0x65","priority":1,"api_type":"jsonrpc","blockchain_type":"solana","has_premium":true}
			]}
		]`)
		got, err := v.fetchDrpcNetworks(ctx, &logger, srv.URL)
		require.NoError(t, err)
		assert.Equal(t, map[int64]string{1: "eth-high", 0x2105: "base"}, got)
	})

	t.Run("non-2xx status", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusServiceUnavailable, `[]`)
		_, err := v.fetchDrpcNetworks(ctx, &logger, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-200 code: 503")
	})

	t.Run("invalid json", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{not json`)
		_, err := v.fetchDrpcNetworks(ctx, &logger, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse dRPC networks response")
	})

	t.Run("connection refused", func(t *testing.T) {
		_, err := v.fetchDrpcNetworks(ctx, &logger, drpcOfflineURL)
		assert.Error(t, err)
	})

	t.Run("malformed url", func(t *testing.T) {
		_, err := v.fetchDrpcNetworks(ctx, &logger, "http://bad\nhost")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid control character")
	})
}

func TestDrpcVendor_FetchErrorKeepsFallbackViaChainsUrl(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		select {
		case hit <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	v := CreateDrpcVendor().(*DrpcVendor)
	settings := common.VendorSettings{"chainsUrl": srv.URL, "recheckInterval": time.Hour}
	ok, err := svtSupports(t, v, settings, "evm:1")
	require.NoError(t, err)
	assert.True(t, ok)
	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		t.Fatal("async refresh never hit the chainsUrl server")
	}
	ok, err = svtSupports(t, v, settings, "evm:1")
	require.NoError(t, err)
	assert.True(t, ok, "failed refresh keeps serving the built-in fallback")
}
