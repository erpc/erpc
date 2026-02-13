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
			name:     "endpoint with .sqd.dev detected",
			upstream: &common.UpstreamConfig{Endpoint: "https://portal.sqd.dev/v1/evm/1"},
			expected: true,
		},
		{
			name:     "endpoint with ://sqd.dev detected",
			upstream: &common.UpstreamConfig{Endpoint: "https://sqd.dev/v1/evm/1"},
			expected: true,
		},
		{
			name:     "false positive avoided - notsqd.dev",
			upstream: &common.UpstreamConfig{Endpoint: "https://notsqd.dev/api"},
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
		expected  bool
		wantErr   bool
	}{
		{
			name:      "any EVM chain is supported",
			networkId: "evm:1",
			expected:  true,
		},
		{
			name:      "any EVM chain is supported (arbitrary)",
			networkId: "evm:999999",
			expected:  true,
		},
		{
			name:      "non-evm network not supported",
			networkId: "solana:mainnet",
			expected:  false,
		},
		{
			name:      "invalid network format",
			networkId: "invalid",
			expected:  false,
		},
		{
			name:      "evm network with non-numeric chainId",
			networkId: "evm:notanumber",
			expected:  false,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			supported, err := v.SupportsNetwork(ctx, &logger, nil, tt.networkId)
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
		assert.Contains(t, err.Error(), "endpoint")
	})

	t.Run("accepts already-substituted endpoint when settings are nil", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/1",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, "https://wrapper.example.com/v1/evm/1", configs[0].Endpoint)
	})

	t.Run("fails when endpoint missing chainId placeholder and settings provided", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/1",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		_, err := v.GenerateConfigs(ctx, &logger, upstream, common.VendorSettings{})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "{chainId}")
	})

	t.Run("substitutes chainId placeholder", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/{chainId}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 42161},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, "https://wrapper.example.com/v1/evm/42161", configs[0].Endpoint)
		assert.Equal(t, "sqd", configs[0].VendorName)
		assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
	})

	t.Run("sets ignoreMethods and allowMethods", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/{chainId}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.Equal(t, []string{"*"}, configs[0].IgnoreMethods)
		assert.Contains(t, configs[0].AllowMethods, "eth_getBlockByNumber")
		assert.Contains(t, configs[0].AllowMethods, "eth_getLogs")
		assert.Contains(t, configs[0].AllowMethods, "trace_block")
	})

	t.Run("sets batch defaults", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/{chainId}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.NoError(t, err)
		assert.NotNil(t, configs[0].JsonRpc.SupportsBatch)
		assert.True(t, *configs[0].JsonRpc.SupportsBatch)
		assert.Equal(t, common.Duration(1), configs[0].JsonRpc.BatchMaxWait) // 1ns
		assert.Equal(t, 1000, configs[0].JsonRpc.BatchMaxSize)
	})

	t.Run("sets X-Api-Key header from settings", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/{chainId}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, common.VendorSettings{
			"apiKey": "secret-key",
		})
		assert.NoError(t, err)
		assert.Equal(t, "secret-key", configs[0].JsonRpc.Headers["X-Api-Key"])
	})

	t.Run("preserves existing X-Api-Key header", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:       "test-sqd",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "https://wrapper.example.com/v1/evm/{chainId}",
			Evm:      &common.EvmUpstreamConfig{ChainId: 1},
			JsonRpc: &common.JsonRpcUpstreamConfig{
				Headers: map[string]string{"X-Api-Key": "existing"},
			},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, common.VendorSettings{
			"apiKey": "secret-key",
		})
		assert.NoError(t, err)
		assert.Equal(t, "existing", configs[0].JsonRpc.Headers["X-Api-Key"])
	})

	t.Run("preserves user-defined ignoreMethods", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:            "test-sqd",
			Type:          common.UpstreamTypeEvm,
			Endpoint:      "https://wrapper.example.com/v1/evm/{chainId}",
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
			Endpoint:     "https://wrapper.example.com/v1/evm/{chainId}",
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

	t.Run("reads endpoint and apiKey from settings when not on upstream", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:   "test-sqd",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: 1},
		}

		configs, err := v.GenerateConfigs(ctx, &logger, upstream, common.VendorSettings{
			"endpoint": "https://wrapper.example.com/v1/evm/{chainId}",
			"apiKey":   "settings-api-key",
		})
		assert.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, "https://wrapper.example.com/v1/evm/1", configs[0].Endpoint)
		assert.Equal(t, "settings-api-key", configs[0].JsonRpc.Headers["X-Api-Key"])
	})

	t.Run("fails when endpoint missing from both upstream and settings", func(t *testing.T) {
		upstream := &common.UpstreamConfig{
			Id:   "test-sqd",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: 1},
		}

		_, err := v.GenerateConfigs(ctx, &logger, upstream, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "endpoint")
	})

	t.Run("idempotent: second call with nil settings after provider substitution", func(t *testing.T) {
		// Simulates the provider double-call: first call from provider with settings,
		// second call from NewUpstream with nil settings and already-substituted endpoint.
		upstream := &common.UpstreamConfig{
			Id:   "sqd-portal-evm-1",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: 1},
		}

		// First call: provider flow with settings (has {chainId} template)
		settings := common.VendorSettings{
			"endpoint": "https://morpho.portal.sqd.dev/rpc/v1/evm/{chainId}",
			"apiKey":   "test-key",
		}
		configs, err := v.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, "https://morpho.portal.sqd.dev/rpc/v1/evm/1", configs[0].Endpoint)
		assert.Equal(t, "test-key", configs[0].JsonRpc.Headers["X-Api-Key"])

		// Second call: NewUpstream flow with nil settings and already-substituted endpoint
		configs2, err := v.GenerateConfigs(ctx, &logger, configs[0], nil)
		assert.NoError(t, err)
		assert.Len(t, configs2, 1)
		assert.Equal(t, "https://morpho.portal.sqd.dev/rpc/v1/evm/1", configs2[0].Endpoint)
		assert.Equal(t, "test-key", configs2[0].JsonRpc.Headers["X-Api-Key"])
	})

	t.Run("idempotent: multiple networks don't interfere", func(t *testing.T) {
		settings := common.VendorSettings{
			"endpoint": "https://morpho.portal.sqd.dev/rpc/v1/evm/{chainId}",
			"apiKey":   "test-key",
		}

		// Generate for chain 1
		ups1 := &common.UpstreamConfig{
			Id:  "sqd-portal-evm-1",
			Evm: &common.EvmUpstreamConfig{ChainId: 1},
		}
		configs1, err := v.GenerateConfigs(ctx, &logger, ups1, settings)
		assert.NoError(t, err)
		assert.Equal(t, "https://morpho.portal.sqd.dev/rpc/v1/evm/1", configs1[0].Endpoint)

		// Generate for chain 8453
		ups2 := &common.UpstreamConfig{
			Id:  "sqd-portal-evm-8453",
			Evm: &common.EvmUpstreamConfig{ChainId: 8453},
		}
		configs2, err := v.GenerateConfigs(ctx, &logger, ups2, settings)
		assert.NoError(t, err)
		assert.Equal(t, "https://morpho.portal.sqd.dev/rpc/v1/evm/8453", configs2[0].Endpoint)

		// Second calls with nil settings (NewUpstream flow)
		configs1b, err := v.GenerateConfigs(ctx, &logger, configs1[0], nil)
		assert.NoError(t, err)
		assert.Equal(t, "https://morpho.portal.sqd.dev/rpc/v1/evm/1", configs1b[0].Endpoint)

		configs2b, err := v.GenerateConfigs(ctx, &logger, configs2[0], nil)
		assert.NoError(t, err)
		assert.Equal(t, "https://morpho.portal.sqd.dev/rpc/v1/evm/8453", configs2b[0].Endpoint)
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
