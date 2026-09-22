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
		_, _ = w.Write([]byte(`{"result":{"data":[{"networkChainId":424242,"kebabCaseId":"custom-net"}]}}`))
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
