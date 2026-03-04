package thirdparty

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestAlchemyVendor_DefaultNetworkMappingIncludesKatana(t *testing.T) {
	subdomain, ok := defaultAlchemyNetworkSubdomains[747474]
	require.True(t, ok)
	require.Equal(t, "katana-mainnet", subdomain)
}

func TestAlchemyVendor_SupportsNetwork_KatanaFromDefaults(t *testing.T) {
	logger := zerolog.New(io.Discard)
	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	vendor.remoteData[alchemyApiUrl] = defaultAlchemyNetworkSubdomains
	vendor.remoteDataLastFetchedAt[alchemyApiUrl] = time.Now()

	supported, err := vendor.SupportsNetwork(
		context.Background(),
		&logger,
		common.VendorSettings{
			"recheckInterval": 24 * time.Hour,
		},
		"evm:747474",
	)
	require.NoError(t, err)
	require.True(t, supported)
}

func TestAlchemyVendor_GenerateConfigs_UsesKatanaDefaultSubdomain(t *testing.T) {
	logger := zerolog.New(io.Discard)
	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	vendor.remoteData[alchemyApiUrl] = defaultAlchemyNetworkSubdomains
	vendor.remoteDataLastFetchedAt[alchemyApiUrl] = time.Now()

	cfgs, err := vendor.GenerateConfigs(
		context.Background(),
		&logger,
		&common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{
				ChainId: 747474,
			},
		},
		common.VendorSettings{
			"apiKey":          "demo",
			"recheckInterval": 24 * time.Hour,
		},
	)
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	require.Equal(t, "https://katana-mainnet.g.alchemy.com/v2/demo", cfgs[0].Endpoint)
}

func TestAlchemyVendor_SupportsNetwork_UnsupportedChainReturnsFalse(t *testing.T) {
	logger := zerolog.New(io.Discard)
	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	vendor.remoteData[alchemyApiUrl] = defaultAlchemyNetworkSubdomains
	vendor.remoteDataLastFetchedAt[alchemyApiUrl] = time.Now()

	supported, err := vendor.SupportsNetwork(
		context.Background(),
		&logger,
		common.VendorSettings{
			"recheckInterval": 24 * time.Hour,
		},
		"evm:999999998",
	)
	require.NoError(t, err)
	require.False(t, supported)
}

func TestAlchemyVendor_GenerateConfigs_UnsupportedChainReturnsError(t *testing.T) {
	logger := zerolog.New(io.Discard)
	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	vendor.remoteData[alchemyApiUrl] = defaultAlchemyNetworkSubdomains
	vendor.remoteDataLastFetchedAt[alchemyApiUrl] = time.Now()

	_, err := vendor.GenerateConfigs(
		context.Background(),
		&logger,
		&common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{
				ChainId: 999999998,
			},
		},
		common.VendorSettings{
			"apiKey":          "demo",
			"recheckInterval": 24 * time.Hour,
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported network chain ID")
}

func TestAlchemyVendor_MergeAlchemyNetworkSubdomains_PreservesDefaultsAndOverridesWithApi(t *testing.T) {
	merged := mergeAlchemyNetworkSubdomains(map[int64]string{
		1:      "eth-mainnet-override",
		84531:  "",
		84532:  "base-sepolia",
		999001: "new-custom-chain",
	})

	require.Equal(t, "eth-mainnet-override", merged[1])
	require.Equal(t, "katana-mainnet", merged[747474])
	require.Equal(t, "new-custom-chain", merged[999001])
}
