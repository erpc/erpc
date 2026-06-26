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

func init() { util.ConfigureTestLogger() }

// quicknodeGenerateConfigsAfterRefresh kicks off the async cache refresh and
// polls until the snapshot is populated. See remote_cache.go's request-path
// safety rule.
func quicknodeGenerateConfigsAfterRefresh(
	t *testing.T,
	vendor *QuicknodeVendor,
	logger *zerolog.Logger,
	ups *common.UpstreamConfig,
	settings common.VendorSettings,
) ([]*common.UpstreamConfig, error) {
	t.Helper()
	ctx := context.Background()

	// Cold cache: synchronous return is retryable; async refresh starts.
	_, _ = vendor.GenerateConfigs(ctx, logger, ups, settings)

	var (
		out []*common.UpstreamConfig
		err error
	)
	require.Eventually(t, func() bool {
		out, err = vendor.GenerateConfigs(ctx, logger, ups, settings)
		return err == nil
	}, 30*time.Second, 50*time.Millisecond, "async refresh should populate quicknode cache")
	return out, err
}

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
	logger := zerolog.Nop()

	// Mock response payloads reused across subtests.
	endpointsSingle := map[string]interface{}{
		"data": []map[string]interface{}{
			{
				"id":       "111",
				"http_url": "https://single-chain-endpoint.arbitrum-mainnet.quiknode.pro/sometoken/",
			},
		},
	}
	endpointsMulti := map[string]interface{}{
		"data": []map[string]interface{}{
			{
				"id":            "612624",
				"http_url":      "https://newest-orbital-research.quiknode.pro/sometoken/",
				"is_multichain": true,
			},
		},
	}
	// Mock response for GET /v0/endpoints/612624/urls listing two Chain Prism
	// URLs: arbitrum-mainnet (probe will succeed) and base-mainnet (probe will
	// return 401 to simulate a chain not activated on the endpoint).
	endpointUrls612624 := map[string]interface{}{
		"data": map[string]interface{}{
			"http_url": "https://newest-orbital-research.quiknode.pro/sometoken/",
			"wss_url":  "wss://newest-orbital-research.quiknode.pro/sometoken/",
			"multichain_urls": map[string]interface{}{
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
		out, err := quicknodeGenerateConfigsAfterRefresh(t, vendor, &logger, ups, settings)
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

		// Ethereum mainnet: one upstream (the root endpoint).
		ethUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		ethOut, err := quicknodeGenerateConfigsAfterRefresh(t, vendor, &logger, ethUps, settings)
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
		out, err := quicknodeGenerateConfigsAfterRefresh(t, vendor, &logger, ups, settings)
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

		for i, chainID := range []int64{1, 42161, 8453} {
			ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: chainID}}
			var out []*common.UpstreamConfig
			var err error
			if i == 0 {
				out, err = quicknodeGenerateConfigsAfterRefresh(t, vendor, &logger, ups, settings)
			} else {
				out, err = vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
			}
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

		// Ethereum mainnet (the root chain) still resolves — we should not
		// break the happy path if the urls API is unavailable.
		ethUps := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}
		ethOut, err := quicknodeGenerateConfigsAfterRefresh(t, vendor, &logger, ethUps, settings)
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
		settings := common.VendorSettings{
			"apiKey":           "k",
			"enableMultiChain": true,
		}
		out, err := quicknodeGenerateConfigsAfterRefresh(t, vendor, &logger, ups, settings)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, "https://solo.arbitrum-mainnet.quiknode.pro/tok/", out[0].Endpoint)
	})

	t.Run("cached results are reused within recheck interval", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

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

		out1, err := quicknodeGenerateConfigsAfterRefresh(t, vendor, &logger, ups, settings)
		require.NoError(t, err)
		require.Len(t, out1, 1)

		// Second call must not hit the API again — no new mocks are registered,
		// so any unexpected request would cause gock to fail.
		out2, err := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		require.NoError(t, err)
		require.Len(t, out2, 1)
	})
}
