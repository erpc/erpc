package thirdparty

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func init() {
	util.ConfigureTestLogger()
}

func TestSqdVendor_Name(t *testing.T) {
	v := CreateSqdVendor()
	assert.Equal(t, "sqd", v.Name())
}

func TestSqdVendor_OwnsUpstream(t *testing.T) {
	v := CreateSqdVendor()

	tests := []struct {
		name     string
		upstream *common.UpstreamConfig
		expected bool
	}{
		{
			name:     "vendorName sqd",
			upstream: &common.UpstreamConfig{Type: common.UpstreamTypeEvm, VendorName: "sqd"},
			expected: true,
		},
		{
			name:     "endpoint with portal.sqd.dev but no vendorName",
			upstream: &common.UpstreamConfig{Endpoint: "https://portal.sqd.dev/datasets/ethereum-mainnet"},
			expected: false,
		},
		{
			name:     "other vendor",
			upstream: &common.UpstreamConfig{Type: common.UpstreamTypeEvm, VendorName: "alchemy"},
			expected: false,
		},
		{
			name:     "empty config",
			upstream: &common.UpstreamConfig{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, v.OwnsUpstream(tt.upstream))
		})
	}
}

func TestSqdVendor_SupportsNetwork(t *testing.T) {
	v := CreateSqdVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	tests := []struct {
		name      string
		networkId string
		settings  common.VendorSettings
		expected  bool
		wantErr   bool
	}{
		{
			name:      "ethereum mainnet supported",
			networkId: "evm:1",
			settings:  nil,
			expected:  true,
		},
		{
			name:      "polygon mainnet supported",
			networkId: "evm:137",
			settings:  nil,
			expected:  true,
		},
		{
			name:      "default datasets disabled rejects known chain",
			networkId: "evm:1",
			settings:  common.VendorSettings{"useDefaultDatasets": false},
			expected:  false,
		},
		{
			name:      "unsupported chainId",
			networkId: "evm:999999",
			settings:  nil,
			expected:  false,
		},
		{
			name:      "non-evm network not supported",
			networkId: "solana:mainnet",
			settings:  nil,
			expected:  false,
		},
		{
			name:      "invalid network format",
			networkId: "invalid",
			settings:  nil,
			expected:  false,
		},
		{
			name:      "unsupported chainId with dataset override",
			networkId: "evm:999999",
			settings:  common.VendorSettings{"dataset": "custom-dataset"},
			expected:  true,
		},
		{
			name:      "unsupported chainId with datasetByChainId override",
			networkId: "evm:999999",
			settings:  common.VendorSettings{"datasetByChainId": map[string]interface{}{"999999": "custom-dataset"}},
			expected:  true,
		},
		{
			name:      "default datasets disabled with datasetByChainId override",
			networkId: "evm:1",
			settings: common.VendorSettings{
				"useDefaultDatasets": false,
				"datasetByChainId":   map[string]interface{}{"1": "ethereum-mainnet"},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supported, err := v.SupportsNetwork(ctx, &logger, tt.settings, tt.networkId)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expected, supported)
		})
	}
}

func TestSqdVendor_GenerateConfigs(t *testing.T) {
	v := CreateSqdVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("fails when endpoint missing", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:   "test-sqd",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: 1},
		}

		_, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "upstream.endpoint")
	})

	t.Run("preserves pre-defined endpoint", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal-wrapper.internal/v1/evm/1",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, "https://portal-wrapper.internal/v1/evm/1", configs[0].Endpoint)
		assert.Equal(t, "sqd", configs[0].VendorName)
		assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
	})

	t.Run("sets ignoreMethods and allowMethods", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal-wrapper.internal/v1/evm/1",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, []string{"*"}, configs[0].IgnoreMethods)
		assert.Contains(t, configs[0].AllowMethods, "eth_getBlockByNumber")
		assert.Contains(t, configs[0].AllowMethods, "eth_getLogs")
		assert.Contains(t, configs[0].AllowMethods, "trace_block")
	})

	t.Run("sets wrapper api key header", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal-wrapper.internal/v1/evm/1",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, common.VendorSettings{
			"wrapperApiKey": "secret",
		})
		assert.NoError(t, err)
		assert.Equal(t, "secret", configs[0].JsonRpc.Headers["X-API-Key"])
	})

	t.Run("preserves existing wrapper header", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal-wrapper.internal/v1/evm/1",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				Headers: map[string]string{"X-API-Key": "existing"},
			},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, common.VendorSettings{
			"wrapperApiKey": "secret",
		})
		assert.NoError(t, err)
		assert.Equal(t, "existing", configs[0].JsonRpc.Headers["X-API-Key"])
	})

	t.Run("preserves user-defined ignoreMethods", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:            "test-sqd",
			Type:          common.UpstreamTypeEvm,
			Endpoint:      "https://portal-wrapper.internal/v1/evm/1",
			Evm:           &common.EvmUpstreamConfig{ChainId: 1},
			IgnoreMethods: []string{"eth_call"},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, []string{"eth_call"}, configs[0].IgnoreMethods)
	})

	t.Run("preserves user-defined allowMethods", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:           "test-sqd",
			Type:         common.UpstreamTypeEvm,
			Endpoint:     "https://portal-wrapper.internal/v1/evm/1",
			Evm:          &common.EvmUpstreamConfig{ChainId: 1},
			AllowMethods: []string{"eth_getBlockByNumber"},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, []string{"eth_getBlockByNumber"}, configs[0].AllowMethods)
	})

	t.Run("fails when evm config is nil", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:   "test-sqd",
			Type: common.UpstreamTypeEvm,
		}

		_, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "upstream.evm")
	})

	t.Run("fails when chainId is zero", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:   "test-sqd",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: 0},
		}

		_, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "chainId")
	})
}
