package thirdparty

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpectrumVendor_GenerateConfigs(t *testing.T) {
	vendor := CreateSpectrumVendor()
	logger := zerolog.Nop()

	generate := func(chainID int64, settings common.VendorSettings) (string, error) {
		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: chainID}}
		cfgs, err := vendor.GenerateConfigs(context.Background(), &logger, ups, settings)
		if err != nil {
			return "", err
		}
		require.Len(t, cfgs, 1)
		return cfgs[0].Endpoint, nil
	}

	t.Run("defaults", func(t *testing.T) {
		ep, err := generate(84532, common.VendorSettings{"apiKey": "KEY"})
		require.NoError(t, err)
		assert.Equal(t, "https://spectrum-03.simplystaking.xyz/KEY/base/tn/84532/shared/archive/rpc/", ep)
	})

	t.Run("node type falls back to the one the chain offers", func(t *testing.T) {
		ep, err := generate(14, common.VendorSettings{"apiKey": "KEY"}) // pruned only
		require.NoError(t, err)
		assert.Equal(t, "https://spectrum-03.simplystaking.xyz/KEY/flare/mn/14/shared/pruned/rpc/", ep)

		ep, err = generate(84532, common.VendorSettings{"apiKey": "KEY", "nodeType": "pruned"}) // archive only
		require.NoError(t, err)
		assert.Equal(t, "https://spectrum-03.simplystaking.xyz/KEY/base/tn/84532/shared/archive/rpc/", ep)
	})

	t.Run("chain-specific routes", func(t *testing.T) {
		ep, err := generate(43114, common.VendorSettings{"apiKey": "KEY"})
		require.NoError(t, err)
		assert.Equal(t, "https://spectrum-03.simplystaking.xyz/KEY/avalanche/mn/43114/shared/archive/rpc/ext/bc/C/rpc", ep)

		ep, err = generate(25, common.VendorSettings{"apiKey": "KEY"})
		require.NoError(t, err)
		assert.Equal(t, "https://spectrum-03.simplystaking.xyz/KEY/cronos/mn/cronosmainnet_25-1/shared/archive/rpc/", ep)
	})

	t.Run("host, plan and node type override the defaults", func(t *testing.T) {
		ep, err := generate(1, common.VendorSettings{
			"apiKey":   "KEY",
			"host":     "spectrum-02.simplystaking.xyz",
			"plan":     "dedicated",
			"nodeType": "pruned",
		})
		require.NoError(t, err)
		assert.Equal(t, "https://spectrum-02.simplystaking.xyz/KEY/ethereum/mn/1/dedicated/pruned/rpc/", ep)
	})

	t.Run("unsupported chain", func(t *testing.T) {
		_, err := generate(999999999, common.VendorSettings{"apiKey": "KEY"})
		assert.Error(t, err)
	})

	t.Run("missing apiKey", func(t *testing.T) {
		_, err := generate(1, common.VendorSettings{})
		assert.Error(t, err)
	})
}

func TestSpectrumVendor_Name(t *testing.T) {
	assert.Equal(t, "spectrum", CreateSpectrumVendor().Name())
}

func TestSpectrumVendor_SupportsNetwork(t *testing.T) {
	svtCheckSupports(t, CreateSpectrumVendor(), common.VendorSettings{}, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:686868", true, false},
		{"evm:999999999", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, false}, // spectrum swallows parse errors
	})
}

func TestSpectrumVendor_GenerateConfigsBranches(t *testing.T) {
	key := common.VendorSettings{"apiKey": "KEY"}
	svtCheckGenErrors(t, CreateSpectrumVendor, []svtErrCase{
		{"nil evm", &common.UpstreamConfig{}, key, "spectrum vendor requires upstream.evm.chainId to be defined"},
		{"zero chain", svtEvm(0), key, "spectrum vendor requires upstream.evm.chainId to be defined"},
	})
	svtCheckURLs(t, CreateSpectrumVendor, key, map[int64]string{
		1: "https://spectrum-03.simplystaking.xyz/KEY/ethereum/mn/1/shared/archive/rpc/",
	})
	svtCheckPreset(t, CreateSpectrumVendor(), "https://spectrum-03.simplystaking.xyz/own/ethereum/mn/1/shared/archive/rpc/")
}

func TestSpectrumVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	err, _ := vendorErr(t, CreateSpectrumVendor(), "eth_getLogs", 401, -32000, "unauthorized", "d")
	assert.NoError(t, err, "spectrum defers every error to generic handling")
}

func TestSpectrumVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateSpectrumVendor(), map[string]bool{
		"spectrum://key":     true,
		"evm+spectrum://key": true,
		"https://spectrum-03.simplystaking.xyz/KEY/ethereum/mn/1/shared/archive/rpc/": true,
		"https://simplystaking.xyz/":   false,
		"https://rpc.ankr.com/eth/key": false,
	})
}
