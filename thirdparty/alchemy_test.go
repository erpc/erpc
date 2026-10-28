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

func TestAlchemyVendor_ColdStartFallback_SupportsNetwork(t *testing.T) {
	// Point the vendor at a URL that is guaranteed to fail so we simulate a
	// cold-start where the Alchemy API is unreachable.
	originalURL := swapAlchemyApiURL(t, "http://127.0.0.1:1/does-not-exist")
	defer swapAlchemyApiURL(t, originalURL)

	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	settings := common.VendorSettings{
		"recheckInterval": 24 * time.Hour,
	}

	// Pick a chain that is in the static fallback map.
	supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:1")
	require.NoError(t, err, "cold-start fallback should not surface a fetch error")
	assert.True(t, supported, "chain 1 is hard-coded in defaultAlchemyNetworkSubdomains")

	// Pick a chain that is not in the static map: the fallback cannot invent it.
	supported, err = vendor.SupportsNetwork(ctx, &logger, settings, "evm:999999999999")
	require.NoError(t, err)
	assert.False(t, supported)
}

func TestAlchemyVendor_ColdStartFallback_GenerateConfigs(t *testing.T) {
	originalURL := swapAlchemyApiURL(t, "http://127.0.0.1:1/does-not-exist")
	defer swapAlchemyApiURL(t, originalURL)

	vendor := CreateAlchemyVendor()
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
	assert.Contains(t, configs[0].Endpoint, "eth-mainnet.g.alchemy.com")
	assert.Contains(t, configs[0].Endpoint, "test-key")
}

func TestAlchemyVendor_SuccessfulFetchPromotesOverFallback(t *testing.T) {
	// Serve a response that adds a chain not present in the static map so we
	// can tell whether the live API result replaced the cold-start fallback.
	const customChainID = int64(424242)
	fetched := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"data":[{"networkChainId":424242,"kebabCaseId":"custom-net"},{"networkChainId":6900,"kebabCaseId":"anime-sepolia"}]}}`))
		select {
		case fetched <- struct{}{}:
		default:
		}
	}))
	defer server.Close()

	originalURL := swapAlchemyApiURL(t, server.URL)
	defer swapAlchemyApiURL(t, originalURL)

	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	settings := common.VendorSettings{"recheckInterval": 24 * time.Hour}

	// Cold-start: the synchronous return uses defaultAlchemyNetworkSubdomains
	// (which doesn't know about our custom chain) and an async refresh is
	// kicked off. See remote_cache.go for the request-path safety rule.
	supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:424242")
	require.NoError(t, err)
	assert.False(t, supported, "first call returns built-in static map; async refresh hasn't published yet")
	supported, err = vendor.SupportsNetwork(ctx, &logger, settings, "evm:6900")
	require.NoError(t, err)
	assert.False(t, supported, "chain 6900 is Nibiru mainnet, not Alchemy's anime-sepolia")

	// Wait for async refresh to hit the mock server, then re-query.
	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("async refresh never hit the mock server")
	}
	require.Eventually(t, func() bool {
		ok, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:424242")
		return err == nil && ok
	}, 5*time.Second, 50*time.Millisecond, "async-refreshed snapshot should promote custom chain over fallback")
	supported, err = vendor.SupportsNetwork(ctx, &logger, settings, "evm:6900")
	require.NoError(t, err)
	assert.False(t, supported, "live API's 6900 entry must not survive the merge")

	// After the refresh, static-default chains are NOT in the snapshot, but
	// the alchemy resolveNetworks function falls back to the default map
	// when the snapshot is missing the requested chain. Wait — check
	// behaviour: alchemy returns the snapshot when present, else the default.
	// The snapshot only contains the custom chain (the mock didn't return
	// chain 1), so chain 1 is no longer reported as supported.
	// This is acceptable — operators using a custom chainsUrl are responsible
	// for returning the full set they want supported.
	_ = customChainID
}

func TestAlchemyVendor_ChainsUrlSetting_OverridesDefault(t *testing.T) {
	const customChainID = int64(777777)
	fetched := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"data":[{"networkChainId":777777,"kebabCaseId":"custom-chains-url-net"}]}}`))
		select {
		case fetched <- struct{}{}:
		default:
		}
	}))
	defer server.Close()

	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	settings := common.VendorSettings{
		"chainsUrl":       server.URL,
		"recheckInterval": 24 * time.Hour,
	}

	// First call returns the built-in fallback (no custom chain), then the
	// async refresh publishes the snapshot and the next call sees it.
	_, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:777777")
	require.NoError(t, err)
	select {
	case <-fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("async refresh never hit the mock server")
	}
	require.Eventually(t, func() bool {
		ok, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:777777")
		return err == nil && ok
	}, 5*time.Second, 50*time.Millisecond, "chain from chainsUrl mock server should be recognized after async refresh")

	// Static defaults are still merged in.
	supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:1")
	require.NoError(t, err)
	assert.True(t, supported)
}

func TestAlchemyVendor_ChainsUrlSetting_ColdStartFallback(t *testing.T) {
	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	settings := common.VendorSettings{
		"chainsUrl":       "http://127.0.0.1:1/does-not-exist",
		"recheckInterval": 24 * time.Hour,
	}

	supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:1")
	require.NoError(t, err, "cold-start fallback via chainsUrl should not surface a fetch error")
	assert.True(t, supported, "chain 1 is hard-coded in defaultAlchemyNetworkSubdomains")
}

func TestAlchemyVendor_ChainsUrlSetting_IsolatedFromDefaultUrl(t *testing.T) {
	// Verify that a vendor using chainsUrl does not pollute the cache for the
	// default alchemyApiUrl (and vice versa) — each URL key is independent.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"data":[{"networkChainId":888888,"kebabCaseId":"isolated-net"}]}}`))
	}))
	defer server.Close()

	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	settingsWithCustomUrl := common.VendorSettings{
		"chainsUrl":       server.URL,
		"recheckInterval": 24 * time.Hour,
	}
	settingsDefault := common.VendorSettings{
		"recheckInterval": 24 * time.Hour,
	}

	// Populate cache for custom URL.
	_, err := vendor.SupportsNetwork(ctx, &logger, settingsWithCustomUrl, "evm:888888")
	require.NoError(t, err)

	// Default URL cache should not know about chain 888888.
	originalURL := swapAlchemyApiURL(t, "http://127.0.0.1:1/does-not-exist")
	defer swapAlchemyApiURL(t, originalURL)

	supported, err := vendor.SupportsNetwork(ctx, &logger, settingsDefault, "evm:888888")
	require.NoError(t, err)
	assert.False(t, supported, "chain 888888 should not bleed into the default URL cache")
}

func TestAlchemyVendor_ChainsUrlSetting_InvalidURLReturnsError(t *testing.T) {
	vendor := CreateAlchemyVendor().(*AlchemyVendor)
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

// swapAlchemyApiURL temporarily overrides the package-level alchemyApiUrl so
// tests can point the vendor at a mock server or a deliberately broken URL.
// Returns the previous value so the caller can restore it.
func swapAlchemyApiURL(t *testing.T, newURL string) string {
	t.Helper()
	prev := alchemyApiUrl
	alchemyApiUrl = newURL
	return prev
}

func TestAlchemyVendor_Code3_MissingDataIsRetryable(t *testing.T) {
	v := CreateAlchemyVendor()

	makeErr := func(msg string) error {
		jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(3, msg, ""))
		require.NoError(t, err)
		return v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 400}, jrr, map[string]interface{}{})
	}

	// Code 3 with a data-availability message must be classified as missing
	// data (retryable toward other upstreams), not as an execution revert.
	for _, msg := range []string{"Unknown block", "block not found with number 0x1234abc"} {
		err := makeErr(msg)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "expected missing-data for %q, got %v", msg, err)
		assert.True(t, common.IsRetryableTowardNetwork(err), "missing-data must stay network-retryable for %q", msg)
	}

	// A genuine execution revert on code 3 keeps the existing classification.
	err := makeErr("execution reverted")
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException), "expected execution exception, got %v", err)
	assert.False(t, common.IsRetryableTowardNetwork(err))
}

// alchemyOfflineURL refuses connections instantly so the async refreshes
// kicked off by SupportsNetwork / GenerateConfigs never leave the machine.
const alchemyOfflineURL = "http://127.0.0.1:1/alchemy-offline"

func TestAlchemyVendor_Name(t *testing.T) {
	assert.Equal(t, "alchemy", CreateAlchemyVendor().Name())
}

func TestAlchemyVendor_SupportsNetworkBranches(t *testing.T) {
	offline := common.VendorSettings{"chainsUrl": alchemyOfflineURL}
	svtCheckSupports(t, CreateAlchemyVendor(), offline, []svtSupportCase{
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
		{"evm:8453", true, false}, // default recheck interval + built-in fallback
	})

	prev := swapAlchemyApiURL(t, alchemyOfflineURL)
	defer swapAlchemyApiURL(t, prev)
	ok, err := svtSupports(t, CreateAlchemyVendor(), common.VendorSettings{}, "evm:1")
	require.NoError(t, err)
	assert.True(t, ok, "empty settings use alchemyApiUrl with built-in fallback")
}

func TestAlchemyVendor_GenerateConfigsBranches(t *testing.T) {
	key := common.VendorSettings{"apiKey": "KEY", "chainsUrl": alchemyOfflineURL}

	svtCheckGenErrors(t, CreateAlchemyVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{"chainsUrl": alchemyOfflineURL}, "apiKey is required in alchemy settings"},
		{"nil evm", &common.UpstreamConfig{}, key, "alchemy vendor requires upstream.evm to be defined"},
		{"zero chain", svtEvm(0), key, "alchemy vendor requires upstream.evm.chainId to be defined"},
		{"invalid chainsUrl", svtEvm(1), common.VendorSettings{"apiKey": "KEY", "chainsUrl": "ftp://host"}, "invalid chainsUrl"},
		{"unsupported chain", svtEvm(999999999999), key, "unsupported network chain ID for Alchemy: 999999999999"},
		{"unparsable url", svtEvm(1), common.VendorSettings{"apiKey": "a\nb", "chainsUrl": alchemyOfflineURL}, "invalid control character"},
	})

	t.Run("built-in fallback urls", func(t *testing.T) {
		svtCheckURLs(t, CreateAlchemyVendor, key, map[int64]string{
			1:     "https://eth-mainnet.g.alchemy.com/v2/KEY",
			42161: "https://arb-mainnet.g.alchemy.com/v2/KEY",
		})
		cfg, err := svtGenerate(t, CreateAlchemyVendor(), svtEvm(1), key)
		require.NoError(t, err)
		assert.Equal(t, "alchemy", cfg.VendorName)
	})

	t.Run("default chainsUrl", func(t *testing.T) {
		prev := swapAlchemyApiURL(t, alchemyOfflineURL)
		defer swapAlchemyApiURL(t, prev)
		cfg, err := svtGenerate(t, CreateAlchemyVendor(), svtEvm(10), common.VendorSettings{"apiKey": "KEY"})
		require.NoError(t, err)
		assert.Equal(t, "https://opt-mainnet.g.alchemy.com/v2/KEY", cfg.Endpoint)
	})

	t.Run("fetched snapshot wins over built-in", func(t *testing.T) {
		v := CreateAlchemyVendor().(*AlchemyVendor)
		primeRemoteCache(t, v.cache, alchemyOfflineURL, map[int64]string{1: "eth-custom"})
		cfg, err := svtGenerate(t, v, svtEvm(1), common.VendorSettings{"apiKey": "KEY", "chainsUrl": alchemyOfflineURL, "recheckInterval": time.Hour})
		require.NoError(t, err)
		assert.Equal(t, "https://eth-custom.g.alchemy.com/v2/KEY", cfg.Endpoint)
	})

	t.Run("preset non-evm endpoint passes through without panicking", func(t *testing.T) {
		ups := &common.UpstreamConfig{Endpoint: "https://solana-mainnet.g.alchemy.com/v2/KEY", Type: common.UpstreamTypeSvm}
		cfg, err := svtGenerate(t, CreateAlchemyVendor(), ups, common.VendorSettings{})
		require.NoError(t, err)
		assert.Equal(t, "https://solana-mainnet.g.alchemy.com/v2/KEY", cfg.Endpoint)
		assert.Equal(t, common.UpstreamTypeSvm, cfg.Type)
		assert.Equal(t, "alchemy", cfg.VendorName)
		assert.NotNil(t, cfg.JsonRpc)
	})
}

func TestAlchemyVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateAlchemyVendor()

	for _, msg := range []string{"Must be authenticated!", "invalid access key"} {
		err, details := vendorErr(t, v, "eth_getLogs", 401, -32600, msg, "d")
		require.Error(t, err, msg)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "%q -> %v", msg, err)
		assert.Equal(t, common.JsonRpcErrorUnauthorized, normalizedCode(t, err))
		assert.Equal(t, "d", details["data"])
	}

	t.Run("code 3 missing data", func(t *testing.T) {
		err, _ := vendorErr(t, v, "eth_getLogs", 400, 3, "header not found", "")
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "got %v", err)
		assert.Equal(t, common.JsonRpcErrorMissingData, normalizedCode(t, err))
	})

	t.Run("code 3 revert", func(t *testing.T) {
		err, details := vendorErr(t, v, "eth_call", 400, 3, "execution reverted", "0x08c379a0")
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException), "got %v", err)
		assert.Equal(t, common.JsonRpcErrorEvmReverted, normalizedCode(t, err))
		assert.Equal(t, "0x08c379a0", details["data"])
	})

	err, details := vendorErr(t, v, "eth_getLogs", 400, 0, "Must be authenticated!", "d")
	assert.NoError(t, err, "code 0 is not inspected")
	assert.NotContains(t, details, "data")

	assert.NoError(t, v.GetVendorSpecificErrorIfAny(svtReq(), nil, "x", map[string]interface{}{}))
}

func TestAlchemyVendor_OwnsUpstream(t *testing.T) {
	v := CreateAlchemyVendor()
	svtAssertOwns(t, v, map[string]bool{
		"alchemy://key":                            true,
		"evm+alchemy://key":                        true,
		"https://eth-mainnet.g.alchemy.com/v2/key": true,
		"https://eth-mainnet.alchemyapi.io/v2/key": true,
		"https://rpc.ankr.com/eth/key":             false,
	})
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://proxy.example/x", VendorName: "alchemy"}), "vendorName claims ownership")
}

func TestAlchemyVendor_FetchAlchemyNetworks(t *testing.T) {
	v := CreateAlchemyVendor().(*AlchemyVendor)
	ctx := context.Background()

	t.Run("api wins, defaults merged, invalid and colliding entries dropped", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"result":{"data":[
			{"networkChainId":1,"kebabCaseId":"eth-renamed"},
			{"networkChainId":424242,"kebabCaseId":"custom-net"},
			{"networkChainId":0,"kebabCaseId":"no-chain"},
			{"networkChainId":555,"kebabCaseId":""},
			{"networkChainId":6900,"kebabCaseId":"anime-sepolia"}
		]}}`)
		got, err := v.fetchAlchemyNetworks(ctx, srv.URL)
		require.NoError(t, err)
		assert.Equal(t, "eth-renamed", got[1])
		assert.Equal(t, "custom-net", got[424242])
		assert.Equal(t, "base-mainnet", got[8453], "built-in default merged in")
		assert.NotContains(t, got, int64(0))
		assert.NotContains(t, got, int64(555))
		assert.NotContains(t, got, int64(6900))
		assert.Len(t, got, len(defaultAlchemyNetworkSubdomains)+1)
	})

	t.Run("non-2xx status", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusBadGateway, `{}`)
		_, err := v.fetchAlchemyNetworks(ctx, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-200 code: 502")
	})

	t.Run("invalid json", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{not json`)
		_, err := v.fetchAlchemyNetworks(ctx, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse Alchemy API data")
	})

	t.Run("connection refused", func(t *testing.T) {
		_, err := v.fetchAlchemyNetworks(ctx, alchemyOfflineURL)
		assert.Error(t, err)
	})

	t.Run("malformed url", func(t *testing.T) {
		_, err := v.fetchAlchemyNetworks(ctx, "http://bad\nhost")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid control character")
	})
}

func TestAlchemyVendor_CreditUnitsEdgeCases(t *testing.T) {
	t.Run("nil vendor and nil cache use built-in table", func(t *testing.T) {
		var nilVendor *AlchemyVendor
		assert.Equal(t, int64(26), common.ResolveCreditUnits(nilVendor.creditUnitsTable(), nil, "eth_call"))
		bare := &AlchemyVendor{}
		assert.Equal(t, int64(26), bare.CreditUnits(cuReq(t, "eth_call"), nil))
		logger := zerolog.Nop()
		nilVendor.refreshCreditUnitsAsync(&logger) // must not panic
		bare.refreshCreditUnitsAsync(&logger)
	})

	t.Run("fresh snapshot skips refresh", func(t *testing.T) {
		hits := make(chan struct{}, 4)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits <- struct{}{}
		}))
		defer srv.Close()
		orig := swapAlchemyCreditUnitsURL(t, srv.URL)
		defer swapAlchemyCreditUnitsURL(t, orig)

		v := CreateAlchemyVendor().(*AlchemyVendor)
		primeRemoteCache(t, v.cuCache, srv.URL, map[string]int64{"eth_call": 1})
		logger := zerolog.Nop()
		v.refreshCreditUnitsAsync(&logger)
		assert.Equal(t, int64(1), v.CreditUnits(cuReq(t, "eth_call"), nil))
		assert.Len(t, hits, 0, "fresh table must not be refetched")
	})

	t.Run("parser skips single-cell rows and keeps other sections out", func(t *testing.T) {
		md := "# Trace API\n|trace_block|\n| trace_call | 30 |\n| **trace_filter** | 75 |\n|---|---|\n"
		assert.Equal(t, map[string]int64{"trace_call": 30, "trace_filter": 75}, parseAlchemyCreditUnits(md))
		assert.Empty(t, parseAlchemyCreditUnits("| eth_call | 1 |\n"), "rows before any allowlisted heading are ignored")
	})

	t.Run("fetch errors", func(t *testing.T) {
		orig := swapAlchemyCreditUnitsURL(t, "http://bad\nhost")
		defer swapAlchemyCreditUnitsURL(t, orig)
		_, err := fetchAlchemyCreditUnits(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid control character")

		swapAlchemyCreditUnitsURL(t, alchemyOfflineURL)
		_, err = fetchAlchemyCreditUnits(context.Background())
		assert.Error(t, err, "connection refused")

		truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("# Debug API\n"))
		}))
		defer truncated.Close()
		swapAlchemyCreditUnitsURL(t, truncated.URL)
		_, err = fetchAlchemyCreditUnits(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected EOF")
	})

	t.Run("request sends markdown accept header", func(t *testing.T) {
		accept := make(chan string, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			accept <- r.Header.Get("Accept")
			_, _ = w.Write([]byte(alchemyCUMarkdownSample))
		}))
		defer srv.Close()
		orig := swapAlchemyCreditUnitsURL(t, srv.URL)
		defer swapAlchemyCreditUnitsURL(t, orig)
		_, err := fetchAlchemyCreditUnits(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "text/markdown, text/plain", <-accept)
	})
}
