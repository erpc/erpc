package thirdparty

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuicknodeFilterParams(t *testing.T) {
	vendor := CreateQuicknodeVendor().(*QuicknodeVendor)

	t.Run("extracts both tag IDs and labels from interface arrays", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey":    "test-key",
			"tagIds":    []interface{}{123, 456},
			"tagLabels": []interface{}{"production", "staging"},
		}
		result := vendor.extractFilterParams(settings)
		assert.Equal(t, []int{123, 456}, result.TagIDs)
		assert.Equal(t, []string{"production", "staging"}, result.TagLabels)
	})

	t.Run("extracts both tag IDs and labels from concrete types", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey":    "test-key",
			"tagIds":    []int{123, 456},
			"tagLabels": []string{"production", "staging"},
		}
		result := vendor.extractFilterParams(settings)
		assert.Equal(t, []int{123, 456}, result.TagIDs)
		assert.Equal(t, []string{"production", "staging"}, result.TagLabels)
	})

	t.Run("handles empty settings", func(t *testing.T) {
		settings := common.VendorSettings{"apiKey": "test-key"}
		result := vendor.extractFilterParams(settings)
		assert.Empty(t, result.TagIDs)
		assert.Empty(t, result.TagLabels)
		assert.False(t, result.EnableMultiChain)
	})

	t.Run("extracts enableMultiChain flag", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey":           "test-key",
			"enableMultiChain": true,
		}
		result := vendor.extractFilterParams(settings)
		assert.True(t, result.EnableMultiChain)
	})
}

func TestQuicknodeGenerateConfigs(t *testing.T) {
	util.ConfigureTestLogger()
	logger := zerolog.Nop()

	// Mock response payloads reused across subtests.
	endpointsMulti := map[string]interface{}{
		"data": []map[string]interface{}{
			{
				"id":            "612624",
				"http_url":      "https://newest-orbital-research.quiknode.pro/sometoken/",
				"is_multichain": true,
			},
		},
	}
	// Mock response for GET /v0/endpoints/612624/urls listing three Chain Prism
	// slugs: ethereum-mainnet (same URL as root — must be deduped), arbitrum-mainnet
	// (probe will succeed), and base-mainnet (probe will return 401 to simulate a
	// chain not activated on the endpoint).
	endpointUrls612624 := map[string]interface{}{
		"data": map[string]interface{}{
			"http_url": "https://newest-orbital-research.quiknode.pro/sometoken/",
			"wss_url":  "wss://newest-orbital-research.quiknode.pro/sometoken/",
			"multichain_urls": map[string]interface{}{
				"ethereum-mainnet": map[string]interface{}{
					"http_url": "https://newest-orbital-research.quiknode.pro/sometoken/",
					"wss_url":  "wss://newest-orbital-research.quiknode.pro/sometoken/",
				},
				"arbitrum-mainnet": map[string]interface{}{
					"http_url": "https://newest-orbital-research.arbitrum-mainnet.quiknode.pro/sometoken/",
					"wss_url":  "wss://newest-orbital-research.arbitrum-mainnet.quiknode.pro/sometoken/",
				},
				"base-mainnet": map[string]interface{}{
					"http_url": "https://newest-orbital-research.base-mainnet.quiknode.pro/sometoken/",
					"wss_url":  "wss://newest-orbital-research.base-mainnet.quiknode.pro/sometoken/",
				},
			},
		},
	}

	t.Run("single-chain endpoint registers one upstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(map[string]interface{}{
				"data": []map[string]interface{}{
					{"id": "111", "http_url": "https://solo.arbitrum-mainnet.quiknode.pro/tok/"},
				},
			})
		gock.New("https://solo.arbitrum-mainnet.quiknode.pro").
			Post("/tok/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xa4b1"}) // 42161

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		settings := common.VendorSettings{"apiKey": "k"}

		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("k", 5*time.Second), "cache not populated in time")

		out, err := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "https://solo.arbitrum-mainnet.quiknode.pro/tok/", out[0].Endpoint)
		assert.Equal(t, "quicknode-42161-111", out[0].Id)
	})

	t.Run("multi-chain endpoint registers derived upstream per enabled chain", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(endpointsMulti)
		// Root probe resolves Ethereum mainnet.
		gock.New("https://newest-orbital-research.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"}) // 1

		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints/612624/urls").
			Reply(200).
			JSON(endpointUrls612624)

		// Arbitrum probe succeeds.
		gock.New("https://newest-orbital-research.arbitrum-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xa4b1"}) // 42161
		// Base probe fails (endpoint not enabled for this chain).
		gock.New("https://newest-orbital-research.base-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(401).
			BodyString("unauthorized")

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		settings := common.VendorSettings{
			"apiKey":           "k",
			"enableMultiChain": true,
		}

		// Warm the cache: first call is cold, triggers async refresh.
		warmUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, warmUps, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("k|mc=1", 10*time.Second), "cache not populated in time")

		// Ethereum mainnet: one upstream (the root endpoint).
		ethUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		ethOut, err := vendor.GenerateConfigs(context.Background(), &logger, ethUps, settings)
		require.NoError(t, err)
		require.Len(t, ethOut, 1)
		assert.Equal(t, "https://newest-orbital-research.quiknode.pro/sometoken/", ethOut[0].Endpoint)
		assert.Equal(t, "quicknode-1-612624", ethOut[0].Id)

		// Arbitrum: one upstream (via Chain Prism expansion).
		arbUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		arbOut, err := vendor.GenerateConfigs(context.Background(), &logger, arbUps, settings)
		require.NoError(t, err)
		require.Len(t, arbOut, 1)
		assert.Equal(t, "https://newest-orbital-research.arbitrum-mainnet.quiknode.pro/sometoken/", arbOut[0].Endpoint)
		// Derived upstream IDs carry the slug suffix to stay unique across
		// chains off the same endpoint.
		assert.Equal(t, "quicknode-42161-612624-arbitrum-mainnet", arbOut[0].Id)

		// Base: no upstream (probe failed).
		baseUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}}
		baseOut, err := vendor.GenerateConfigs(context.Background(), &logger, baseUps, settings)
		require.NoError(t, err)
		assert.Empty(t, baseOut)
	})

	t.Run("multi-chain disabled by default", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(endpointsMulti)
		gock.New("https://newest-orbital-research.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
		// Intentionally no /v0/endpoints/:id/urls mock: if the code reaches
		// that call, gock rejects the unmocked request and the subtest fails
		// — that's the point, we're asserting Chain Prism discovery is skipped.

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		settings := common.VendorSettings{"apiKey": "k"}

		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("k", 5*time.Second), "cache not populated in time")

		out, err := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.NoError(t, err)
		assert.Empty(t, out)
	})

	t.Run("tag filter excludes multi-chain endpoint", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// The tag filter is forwarded to QuickNode as a query parameter; when
		// the labels do not match, QuickNode returns an empty data set and
		// neither the endpoint nor any Chain Prism derivatives are registered.
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			MatchParam("tag_labels", "nonexistent-label").
			Reply(200).
			JSON(map[string]interface{}{"data": []map[string]interface{}{}})

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		settings := common.VendorSettings{
			"apiKey":           "k",
			"enableMultiChain": true,
			"tagLabels":        []interface{}{"nonexistent-label"},
		}

		// Warm cache: cold start triggers async fetch (returns empty data set).
		warmUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, warmUps, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("k|mc=1|tagLabels=nonexistent-label", 5*time.Second), "cache not populated in time")

		for _, chainID := range []int64{1, 42161, 8453} {
			ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: chainID}}
			out, err := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
			require.NoError(t, err)
			assert.Empty(t, out, "chain %d should not produce upstreams when tag filter matches nothing", chainID)
		}
	})

	t.Run("endpoint urls failure degrades gracefully to single-chain behavior", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(endpointsMulti)
		gock.New("https://newest-orbital-research.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints/612624/urls").
			Reply(500).
			BodyString("server error")

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		settings := common.VendorSettings{
			"apiKey":           "k",
			"enableMultiChain": true,
		}

		// Warm cache: async refresh fetches endpoints + probes root chain.
		// /urls returns 500, so only the root endpoint is stored.
		warmUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, warmUps, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("k|mc=1", 5*time.Second), "cache not populated in time")

		// Ethereum mainnet (the root chain) still resolves — we should not
		// break the happy path if the urls API is unavailable.
		ethUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		ethOut, err := vendor.GenerateConfigs(context.Background(), &logger, ethUps, settings)
		require.NoError(t, err)
		require.Len(t, ethOut, 1)

		// Non-root chains cannot be discovered without the urls response.
		arbUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		arbOut, err := vendor.GenerateConfigs(context.Background(), &logger, arbUps, settings)
		require.NoError(t, err)
		assert.Empty(t, arbOut)
	})

	t.Run("single-chain endpoint never triggers urls fetch", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Endpoint flagged is_multichain=false: /urls must NOT be called, only
		// the root probe runs. No urls mock registered — any call would fail.
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(map[string]interface{}{
				"data": []map[string]interface{}{
					{
						"id":            "413738",
						"http_url":      "https://solo.arbitrum-mainnet.quiknode.pro/tok/",
						"is_multichain": false,
					},
				},
			})
		gock.New("https://solo.arbitrum-mainnet.quiknode.pro").
			Post("/tok/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xa4b1"})

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		settings := common.VendorSettings{"apiKey": "k", "enableMultiChain": true}

		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("k|mc=1", 5*time.Second), "cache not populated in time")

		out, err := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "https://solo.arbitrum-mainnet.quiknode.pro/tok/", out[0].Endpoint)
	})

	t.Run("cached results are reused within recheck interval", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		endpointsSingle := map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"id":       "111",
					"http_url": "https://single-chain-endpoint.arbitrum-mainnet.quiknode.pro/sometoken/",
				},
			},
		}
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(endpointsSingle)
		gock.New("https://single-chain-endpoint.arbitrum-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xa4b1"})

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		settings := common.VendorSettings{
			"apiKey":          "k",
			"recheckInterval": 1 * time.Hour,
		}
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}

		// Warm cache: cold start triggers async refresh (one /endpoints + one probe).
		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("k", 5*time.Second), "cache not populated in time")

		out1, err := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.NoError(t, err)
		require.Len(t, out1, 1)

		// Second call must not hit the API again — no new mocks are registered,
		// so any unexpected request would cause gock to fail.
		out2, err := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.NoError(t, err)
		require.Len(t, out2, 1)
	})

	t.Run("cache key isolation prevents cross-contamination between multichain and non-multichain configs", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Mock registration order matters: gock matches mocks FIFO per host+path.
		// First /v0/endpoints mock is consumed by the multichain warm-up;
		// second is consumed by the non-multichain warm-up.

		// multichain config: endpoint returns is_multichain=true, /urls lists
		// arbitrum-mainnet, probe succeeds → Arbitrum upstream registered.
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(endpointsMulti)
		gock.New("https://newest-orbital-research.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints/612624/urls").
			Reply(200).
			JSON(endpointUrls612624)
		gock.New("https://newest-orbital-research.arbitrum-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xa4b1"})
		gock.New("https://newest-orbital-research.base-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(401).BodyString("unauthorized")

		// non-multichain config: same API key, same endpoint list returned,
		// but /urls must NOT be called and Arbitrum must NOT appear.
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(endpointsMulti)
		gock.New("https://newest-orbital-research.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)

		mcSettings := common.VendorSettings{"apiKey": "shared-key", "enableMultiChain": true}
		noMcSettings := common.VendorSettings{"apiKey": "shared-key"}

		// Warm multichain cache entry.
		warmUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, warmUps, mcSettings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("shared-key|mc=1", 10*time.Second), "multichain cache not populated")

		// Arbitrum should be reachable via the multichain config.
		arbUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		mcOut, err := vendor.GenerateConfigs(context.Background(), &logger, arbUps, mcSettings)
		require.NoError(t, err)
		require.Len(t, mcOut, 1, "multichain config should register Arbitrum upstream")

		// Warm non-multichain cache entry (separate key).
		_, coldErr2 := vendor.GenerateConfigs(context.Background(), &logger, warmUps, noMcSettings)
		require.ErrorIs(t, coldErr2, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("shared-key", 10*time.Second), "non-multichain cache not populated")

		// Arbitrum must NOT appear via the non-multichain config.
		noMcOut, err := vendor.GenerateConfigs(context.Background(), &logger, arbUps, noMcSettings)
		require.NoError(t, err)
		assert.Empty(t, noMcOut, "non-multichain config must not see Chain Prism-derived upstreams")
	})

	t.Run("probes cancelled by context deadline are dropped, partial results stored", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(endpointsMulti)
		// Root probe: instant.
		gock.New("https://newest-orbital-research.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints/612624/urls").
			Reply(200).
			JSON(endpointUrls612624)
		// Arbitrum probe delayed well past the short refresh budget.
		gock.New("https://newest-orbital-research.arbitrum-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xa4b1"})
		gock.New("https://newest-orbital-research.base-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(401).BodyString("unauthorized")

		// Vendor with a very short refresh budget so the delayed probe is
		// cancelled before it completes.
		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		vendor.cache = NewRemoteDataCache[[]*QuicknodeEndpoint]("quicknode").
			WithRefreshTimeout(100 * time.Millisecond)

		settings := common.VendorSettings{"apiKey": "k", "enableMultiChain": true}
		warmUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}

		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, warmUps, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		// Wait long enough for the refresh to finish (timeout fires at 100ms,
		// goroutine cleanup runs shortly after).
		require.True(t, vendor.cache.WaitForKey("k|mc=1", 2*time.Second), "cache not populated after deadline")

		// Root chain (instant probe) should be present.
		ethOut, err := vendor.GenerateConfigs(context.Background(), &logger, warmUps, settings)
		require.NoError(t, err)
		require.Len(t, ethOut, 1, "root chain upstream must be registered despite probe deadline")

		// Arbitrum probe was delayed past deadline — must not appear.
		arbUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		arbOut, err := vendor.GenerateConfigs(context.Background(), &logger, arbUps, settings)
		require.NoError(t, err)
		assert.Empty(t, arbOut, "delayed probe must be dropped when refresh budget expires")
	})

	t.Run("probeChainID drops candidate on JSON-RPC error body with 200 status", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			Reply(200).
			JSON(endpointsMulti)
		// Root probe succeeds.
		gock.New("https://newest-orbital-research.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints/612624/urls").
			Reply(200).
			JSON(endpointUrls612624)
		// Arbitrum probe returns 200 but with a JSON-RPC error envelope —
		// exercises the result.Error != nil branch in probeChainID.
		gock.New("https://newest-orbital-research.arbitrum-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": 1,
				"error": map[string]interface{}{"code": -32601, "message": "method not found"},
			})
		gock.New("https://newest-orbital-research.base-mainnet.quiknode.pro").
			Post("/sometoken/").
			Reply(401).BodyString("unauthorized")

		vendor := CreateQuicknodeVendor().(*QuicknodeVendor)
		settings := common.VendorSettings{"apiKey": "k", "enableMultiChain": true}

		warmUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		_, coldErr := vendor.GenerateConfigs(context.Background(), &logger, warmUps, settings)
		require.ErrorIs(t, coldErr, ErrRemoteCacheCold)
		require.True(t, vendor.cache.WaitForKey("k|mc=1", 10*time.Second), "cache not populated in time")

		// Arbitrum must not be registered — the JSON-RPC error probe must be dropped.
		arbUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 42161}}
		out, err := vendor.GenerateConfigs(context.Background(), &logger, arbUps, settings)
		require.NoError(t, err)
		assert.Empty(t, out, "Arbitrum upstream must be dropped when probe returns JSON-RPC error")
	})
}

func TestQuicknodeOwnsUpstream(t *testing.T) {
	vendor := CreateQuicknodeVendor().(*QuicknodeVendor)

	cases := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{"provisioned host", "https://newest-orbital-research.quiknode.pro/sometoken/", true},
		{"multichain slug host", "https://solo.arbitrum-mainnet.quiknode.pro/tok/", true},
		{"apex host", "https://quiknode.pro/tok/", true},
		{"quicknode alias scheme", "quicknode://example", true},
		{"evm+quicknode alias scheme", "evm+quicknode://example", true},
		{"spoofed host suffix", "https://quiknode.pro.evil.com/tok/", false},
		{"unrelated host", "https://eth.llamarpc.com", false},
		{"empty endpoint", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ups := &common.UpstreamConfig{Endpoint: tc.endpoint}
			assert.Equal(t, tc.want, vendor.OwnsUpstream(ups))
		})
	}
}
