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
		assert.Equal(t, "https://spectrum-03.simplystaking.xyz/KEY/base/tn_sepolia/84532/shared/archive/rpc/", ep)
	})

	t.Run("node type falls back to the one the chain offers", func(t *testing.T) {
		ep, err := generate(14, common.VendorSettings{"apiKey": "KEY"}) // pruned only
		require.NoError(t, err)
		assert.Equal(t, "https://spectrum-03.simplystaking.xyz/KEY/flare/mn/14/shared/pruned/rpc/", ep)

		ep, err = generate(84532, common.VendorSettings{"apiKey": "KEY", "nodeType": "pruned"}) // archive only
		require.NoError(t, err)
		assert.Equal(t, "https://spectrum-03.simplystaking.xyz/KEY/base/tn_sepolia/84532/shared/archive/rpc/", ep)
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
