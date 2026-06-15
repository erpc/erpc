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
