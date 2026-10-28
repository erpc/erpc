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

// newPrimedTenderlyVendor returns a vendor whose supported-networks cache is
// already populated, so no call reaches the (non-configurable) Tenderly API.
func newPrimedTenderlyVendor(t *testing.T, networks map[int64]string) *TenderlyVendor {
	t.Helper()
	v := CreateTenderlyVendor().(*TenderlyVendor)
	primeRemoteCache(t, v.cache, tenderlyApiUrl, networks)
	return v
}

// newColdTenderlyVendor returns a vendor whose cache stays cold: a refresh is
// marked in flight so resolveNetworks' TriggerAsyncRefresh is a no-op and never
// dials the real Tenderly API.
func newColdTenderlyVendor() *TenderlyVendor {
	v := CreateTenderlyVendor().(*TenderlyVendor)
	v.cache.inflight[tenderlyApiUrl] = struct{}{}
	return v
}

func TestTenderlyVendor_SupportsNetwork(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	v := newPrimedTenderlyVendor(t, map[int64]string{1: "mainnet", 8453: "base"})

	ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:8453")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{"recheckInterval": time.Hour}, "evm:999")
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "solana:mainnet")
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:abc")
	assert.Error(t, err)
	assert.False(t, ok)
}

func TestTenderlyVendor_SupportsNetwork_ColdStart(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	ok, err := newColdTenderlyVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
	assert.False(t, ok)
}

func TestTenderlyVendor_SupportsNetwork_StaleUsesCachedData(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	v := newPrimedTenderlyVendor(t, map[int64]string{1: "mainnet"})
	// Block the stale-triggered refresh so it cannot reach the real API. Wait for
	// the priming refresh to clear its in-flight marker first, or its deferred
	// cleanup would delete ours.
	require.Eventually(t, func() bool {
		v.cache.refreshMu.Lock()
		defer v.cache.refreshMu.Unlock()
		if _, busy := v.cache.inflight[tenderlyApiUrl]; busy {
			return false
		}
		v.cache.inflight[tenderlyApiUrl] = struct{}{}
		return true
	}, 5*time.Second, 10*time.Millisecond)

	// A 1ns recheck interval makes the entry stale: the cached data still answers.
	ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{"recheckInterval": time.Nanosecond}, "evm:1")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestTenderlyVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	v := newPrimedTenderlyVendor(t, map[int64]string{1: "mainnet", 8453: "base"})

	t.Run("generated endpoint", func(t *testing.T) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}}
		cfgs, err := v.GenerateConfigs(ctx, &logger, ups, common.VendorSettings{"apiKey": "key1"})
		require.NoError(t, err)
		require.Len(t, cfgs, 1)
		assert.Equal(t, "https://base.gateway.tenderly.co/key1", cfgs[0].Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, cfgs[0].Type)
		assert.Equal(t, "tenderly", cfgs[0].VendorName)
		assert.NotNil(t, cfgs[0].JsonRpc)
	})

	t.Run("preset endpoint passes through and gets vendor name", func(t *testing.T) {
		ups := &common.UpstreamConfig{Endpoint: "https://mainnet.gateway.tenderly.co/mine"}
		cfgs, err := v.GenerateConfigs(ctx, &logger, ups, nil)
		require.NoError(t, err)
		assert.Equal(t, "https://mainnet.gateway.tenderly.co/mine", cfgs[0].Endpoint)
		assert.Equal(t, "tenderly", cfgs[0].VendorName)
	})

	errCases := []struct {
		name     string
		vendor   *TenderlyVendor
		ups      *common.UpstreamConfig
		settings common.VendorSettings
		contains string
		is       error
	}{
		{"missing apiKey", v, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{}, "apiKey is required in tenderly settings", nil},
		{"missing evm", v, &common.UpstreamConfig{}, common.VendorSettings{"apiKey": "k"}, "requires upstream.evm to be defined", nil},
		{"missing chain id", v, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"apiKey": "k"}, "requires upstream.evm.chainId", nil},
		{"unsupported chain", v, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 999}}, common.VendorSettings{"apiKey": "k", "recheckInterval": time.Hour}, "unsupported network chain ID for Tenderly: 999", nil},
		{"malformed api key", v, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{"apiKey": "%zz"}, "invalid URL escape", nil},
		{"cold cache", newColdTenderlyVendor(), &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{"apiKey": "k"}, "", ErrRemoteCacheCold},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.vendor.GenerateConfigs(ctx, &logger, c.ups, c.settings)
			if c.is != nil {
				assert.ErrorIs(t, err, c.is)
			} else {
				assert.ErrorContains(t, err, c.contains)
			}
		})
	}
}

func TestTenderlyVendor_ErrorsAndOwnership(t *testing.T) {
	v := CreateTenderlyVendor()
	assert.Equal(t, "tenderly", v.Name())

	err, details := vendorErr(t, v, "eth_call", 500, -32000, "boom", "revert-data")
	assert.NoError(t, err)
	assert.Equal(t, "revert-data", details["data"])

	err, details = vendorErr(t, v, "eth_call", 500, -32000, "boom", "")
	assert.NoError(t, err)
	assert.NotContains(t, details, "data")

	assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500}, nil, map[string]interface{}{}))

	cases := []struct {
		ups   *common.UpstreamConfig
		owned bool
	}{
		{&common.UpstreamConfig{Endpoint: "tenderly://k"}, true},
		{&common.UpstreamConfig{Endpoint: "evm+tenderly://k"}, true},
		{&common.UpstreamConfig{Endpoint: "https://x.example.com", VendorName: "tenderly"}, true},
		{&common.UpstreamConfig{Endpoint: "https://mainnet.gateway.tenderly.co/k"}, true},
		{&common.UpstreamConfig{Endpoint: "https://rpc.example.com"}, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.owned, v.OwnsUpstream(c.ups), c.ups.Endpoint)
	}
}
