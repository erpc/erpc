package thirdparty

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func seedAlchemyVendorWithDefaultNetworks() *AlchemyVendor {
	vendor := CreateAlchemyVendor().(*AlchemyVendor)
	vendor.remoteData[alchemyApiUrl] = defaultAlchemyNetworkSubdomains
	vendor.remoteDataLastFetchedAt[alchemyApiUrl] = time.Now()
	return vendor
}

func TestAlchemyVendor_DefaultNetworkMappingIncludesRecentEvmChains(t *testing.T) {
	testCases := map[int64]string{
		25:      "cronos-mainnet",
		338:     "cronos-testnet",
		196:     "xlayer-mainnet",
		1952:    "xlayer-testnet",
		1672:    "pharos-mainnet",
		4153:    "rise-mainnet",
		4217:    "tempo-mainnet",
		4326:    "megaeth-mainnet",
		42018:   "mythos-mainnet",
		46630:   "robinhood-testnet",
		51014:   "risa-testnet",
		737373:  "katana-bokuto",
		202601:  "ronin-saigon",
		685689:  "gensyn-mainnet",
		99999:   "adi-testnet",
		36900:   "adi-mainnet",
		747474:  "katana-mainnet",
		688689:  "pharos-atlantic",
		5734951: "jovay-mainnet",
		2019775: "jovay-testnet",
	}

	for chainID, expectedSubdomain := range testCases {
		subdomain, ok := defaultAlchemyNetworkSubdomains[chainID]
		require.Truef(t, ok, "expected chain %d to exist in fallback map", chainID)
		require.Equalf(t, expectedSubdomain, subdomain, "unexpected subdomain for chain %d", chainID)
	}
}

func TestAlchemyVendor_SupportsNetwork_UsesFallbackMappings(t *testing.T) {
	logger := zerolog.New(io.Discard)
	vendor := seedAlchemyVendorWithDefaultNetworks()

	for _, chainID := range []int64{4153, 99999, 202601, 737373} {
		t.Run(fmt.Sprintf("evm:%d", chainID), func(t *testing.T) {
			supported, err := vendor.SupportsNetwork(
				context.Background(),
				&logger,
				common.VendorSettings{
					"recheckInterval": 24 * time.Hour,
				},
				fmt.Sprintf("evm:%d", chainID),
			)
			require.NoError(t, err)
			require.True(t, supported)
		})
	}
}

func TestAlchemyVendor_GenerateConfigs_UsesFallbackSubdomains(t *testing.T) {
	logger := zerolog.New(io.Discard)
	vendor := seedAlchemyVendorWithDefaultNetworks()

	testCases := []struct {
		chainID          int64
		expectedEndpoint string
	}{
		{
			chainID:          4153,
			expectedEndpoint: "https://rise-mainnet.g.alchemy.com/v2/demo",
		},
		{
			chainID:          36900,
			expectedEndpoint: "https://adi-mainnet.g.alchemy.com/v2/demo",
		},
		{
			chainID:          202601,
			expectedEndpoint: "https://ronin-saigon.g.alchemy.com/v2/demo",
		},
		{
			chainID:          737373,
			expectedEndpoint: "https://katana-bokuto.g.alchemy.com/v2/demo",
		},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("evm:%d", tc.chainID), func(t *testing.T) {
			cfgs, err := vendor.GenerateConfigs(
				context.Background(),
				&logger,
				&common.UpstreamConfig{
					Evm: &common.EvmUpstreamConfig{
						ChainId: tc.chainID,
					},
				},
				common.VendorSettings{
					"apiKey":          "demo",
					"recheckInterval": 24 * time.Hour,
				},
			)
			require.NoError(t, err)
			require.Len(t, cfgs, 1)
			require.Equal(t, tc.expectedEndpoint, cfgs[0].Endpoint)
		})
	}
}

func TestAlchemyVendor_SupportsNetwork_UnsupportedChainReturnsFalse(t *testing.T) {
	logger := zerolog.New(io.Discard)
	vendor := seedAlchemyVendorWithDefaultNetworks()

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
	vendor := seedAlchemyVendorWithDefaultNetworks()

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
