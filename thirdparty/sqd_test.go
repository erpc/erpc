package thirdparty

import (
	"context"
	"net/http"
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
			name:     "endpoint with portal.sqd.dev detected",
			upstream: &common.UpstreamConfig{Endpoint: "https://portal.sqd.dev/datasets/ethereum-mainnet"},
			expected: true,
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
			name:      "evm network with non-numeric chainId",
			networkId: "evm:notanumber",
			settings:  nil,
			expected:  false,
			wantErr:   true,
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
		{
			name:      "chainId placeholder supports any EVM chain",
			networkId: "evm:999999",
			settings:  common.VendorSettings{"endpoint": "https://wrapper.example.com/v1/evm/{chainId}"},
			expected:  true,
		},
		{
			name:      "chainId placeholder supports known chain",
			networkId: "evm:1",
			settings:  common.VendorSettings{"endpoint": "https://wrapper.example.com/v1/evm/{chainId}"},
			expected:  true,
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

	t.Run("sets wrapper api key custom header", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal-wrapper.internal/v1/evm/1",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, common.VendorSettings{
			"wrapperApiKey":       "secret",
			"wrapperApiKeyHeader": "X-SQD-Key",
		})
		assert.NoError(t, err)
		assert.Equal(t, "secret", configs[0].JsonRpc.Headers["X-SQD-Key"])
		_, hasDefault := configs[0].JsonRpc.Headers["X-API-Key"]
		assert.False(t, hasDefault)
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

	t.Run("applies dataset placeholder to endpoint", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal.sqd.dev/datasets/{dataset}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, "https://portal.sqd.dev/datasets/ethereum-mainnet", configs[0].Endpoint)
	})

	t.Run("applies dataset to base endpoint", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal.sqd.dev/datasets",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, "https://portal.sqd.dev/datasets/ethereum-mainnet", configs[0].Endpoint)
	})

	t.Run("applies dataset to base endpoint with trailing slash", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal.sqd.dev/datasets/",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, "https://portal.sqd.dev/datasets/ethereum-mainnet", configs[0].Endpoint)
	})

	t.Run("fails when dataset placeholder unresolved", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://portal.sqd.dev/datasets/{dataset}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 999999},
		}

		_, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "{dataset}")
		assert.Contains(t, err.Error(), "999999")
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

	t.Run("applies chainId placeholder to endpoint", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/{chainId}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 42161},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, "https://wrapper.example.com/v1/evm/42161", configs[0].Endpoint)
	})

	t.Run("chainId placeholder works for any chain without dataset mapping", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/{chainId}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 999999},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, "https://wrapper.example.com/v1/evm/999999", configs[0].Endpoint)
	})

	t.Run("chainId placeholder takes precedence over dataset placeholder", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/{chainId}/datasets/{dataset}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		// chainId is replaced, dataset placeholder remains (no dataset resolution when chainId is used)
		assert.Equal(t, "https://wrapper.example.com/1/datasets/{dataset}", configs[0].Endpoint)
	})
}

func TestSqdVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateSqdVendor().(*SqdVendor)

	tests := []struct {
		name       string
		resp       *http.Response
		expectNil  bool
		expectType string
	}{
		{
			name:      "nil response returns nil",
			resp:      nil,
			expectNil: true,
		},
		{
			name:      "200 returns nil",
			resp:      &http.Response{StatusCode: http.StatusOK},
			expectNil: true,
		},
		{
			name:       "401 returns unauthorized",
			resp:       &http.Response{StatusCode: http.StatusUnauthorized},
			expectType: "ErrEndpointUnauthorized",
		},
		{
			name:       "403 returns unauthorized",
			resp:       &http.Response{StatusCode: http.StatusForbidden},
			expectType: "ErrEndpointUnauthorized",
		},
		{
			name:       "429 returns capacity exceeded",
			resp:       &http.Response{StatusCode: http.StatusTooManyRequests},
			expectType: "ErrEndpointCapacityExceeded",
		},
		{
			name:       "402 returns billing issue",
			resp:       &http.Response{StatusCode: http.StatusPaymentRequired},
			expectType: "ErrEndpointBillingIssue",
		},
		{
			name:      "500 returns nil (handled by generic normalization)",
			resp:      &http.Response{StatusCode: http.StatusInternalServerError},
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.GetVendorSpecificErrorIfAny(nil, tt.resp, nil, nil)
			if tt.expectNil {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			switch tt.expectType {
			case "ErrEndpointUnauthorized":
				assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized), "expected ErrEndpointUnauthorized, got %v", err)
			case "ErrEndpointCapacityExceeded":
				assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "expected ErrEndpointCapacityExceeded, got %v", err)
			case "ErrEndpointBillingIssue":
				assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointBillingIssue), "expected ErrEndpointBillingIssue, got %v", err)
			}
		})
	}
}
