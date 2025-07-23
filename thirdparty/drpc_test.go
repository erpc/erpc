package thirdparty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestDrpcVendor_GenerateConfigs(t *testing.T) {
	vendor := CreateDrpcVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("without API key", func(t *testing.T) {
		settings := common.VendorSettings{}

		upstream := &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{
				ChainId: 1,
			},
		}

		_, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "apiKey is required")
	})

	t.Run("without evm config", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey": "test-api-key",
		}

		upstream := &common.UpstreamConfig{}

		_, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "drpc vendor requires upstream.evm to be defined")
	})

	t.Run("without chain ID", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey": "test-api-key",
		}

		upstream := &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{},
		}

		_, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "drpc vendor requires upstream.evm.chainId to be defined")
	})

	t.Run("with pre-defined endpoint", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey": "test-api-key",
		}

		upstream := &common.UpstreamConfig{
			Endpoint: "https://lb.drpc.org/ogrpc?network=ethereum&dkey=test-key",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 1,
			},
		}

		configs, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, upstream.Endpoint, configs[0].Endpoint)
		assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
		assert.Equal(t, &common.FALSE, configs[0].AutoIgnoreUnsupportedMethods)
	})

	t.Run("with valid chain ID and mock server", func(t *testing.T) {
		mockYAML := `
version: v1
chain-settings:
  protocols:
    - chains:
        - chain-id: 0x1
          short-names: [ethereum, eth]
        - chain-id: 0x89
          short-names: [polygon, matic]
`

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mockYAML))
		}))
		defer server.Close()

		settings := common.VendorSettings{
			"apiKey":    "test-api-key",
			"chainsUrl": server.URL,
		}

		upstream := &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{
				ChainId: 1, // Ethereum mainnet
			},
		}

		configs, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Contains(t, configs[0].Endpoint, "https://lb.drpc.org/ogrpc?network=ethereum&dkey=test-api-key")
		assert.Equal(t, common.UpstreamTypeEvm, configs[0].Type)
		assert.Equal(t, &common.FALSE, configs[0].AutoIgnoreUnsupportedMethods)
	})

	t.Run("with unsupported chain ID", func(t *testing.T) {
		mockYAML := `
version: v1
chain-settings:
  protocols:
    - chains:
        - chain-id: 0x1
          short-names: [ethereum, eth]
`

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mockYAML))
		}))
		defer server.Close()

		settings := common.VendorSettings{
			"apiKey":    "test-api-key",
			"chainsUrl": server.URL,
		}

		upstream := &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{
				ChainId: 999, // Unsupported chain
			},
		}

		_, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported network chain ID for DRPC: 999")
	})
}

func TestDrpcVendor_SupportsNetwork(t *testing.T) {
	vendor := CreateDrpcVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("non-EVM network", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey": "test-api-key",
		}

		supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "solana:mainnet")
		assert.NoError(t, err)
		assert.False(t, supported)
	})

	t.Run("invalid EVM network ID", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey": "test-api-key",
		}

		supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:invalid")
		assert.Error(t, err)
		assert.False(t, supported)
	})

	t.Run("supported EVM network with mock server", func(t *testing.T) {
		mockYAML := `
version: v1
chain-settings:
  protocols:
    - chains:
        - chain-id: 0x1
          short-names: [ethereum, eth]
        - chain-id: 0x89
          short-names: [polygon, matic]
`

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mockYAML))
		}))
		defer server.Close()

		settings := common.VendorSettings{
			"apiKey":    "test-api-key",
			"chainsUrl": server.URL,
		}

		supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:1")
		assert.NoError(t, err)
		assert.True(t, supported)

		supported, err = vendor.SupportsNetwork(ctx, &logger, settings, "evm:137")
		assert.NoError(t, err)
		assert.True(t, supported)
	})

	t.Run("unsupported EVM network", func(t *testing.T) {
		mockYAML := `
version: v1
chain-settings:
  protocols:
    - chains:
        - chain-id: 0x1
          short-names: [ethereum, eth]
`

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mockYAML))
		}))
		defer server.Close()

		settings := common.VendorSettings{
			"apiKey":    "test-api-key",
			"chainsUrl": server.URL,
		}

		supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:999")
		assert.NoError(t, err)
		assert.False(t, supported)
	})
}

func TestDrpcVendor_FetchDrpcNetworks(t *testing.T) {
	vendor := CreateDrpcVendor().(*DrpcVendor)
	ctx := context.Background()

	t.Run("successful fetch and parse", func(t *testing.T) {
		mockYAML := `
version: v1
chain-settings:
  protocols:
    - chains:
        - chain-id: 0x1
          short-names: [ethereum, eth]
        - chain-id: 0x89
          short-names: [polygon, matic]
        - chain-id: 0xa
          short-names: [optimism, opt]
`

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mockYAML))
		}))
		defer server.Close()

		networks, err := vendor.fetchDrpcNetworks(ctx, server.URL)
		assert.NoError(t, err)
		assert.Len(t, networks, 3)
		assert.Equal(t, "ethereum", networks[1])
		assert.Equal(t, "polygon", networks[137])
		assert.Equal(t, "optimism", networks[10])
	})

	t.Run("invalid YAML", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("invalid: yaml: content: ["))
		}))
		defer server.Close()

		_, err := vendor.fetchDrpcNetworks(ctx, server.URL)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse DRPC chains YAML")
	})

	t.Run("HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		_, err := vendor.fetchDrpcNetworks(ctx, server.URL)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "DRPC chains API returned non-200 code: 500")
	})

	t.Run("invalid chain ID format", func(t *testing.T) {
		mockYAML := `
version: v1
chain-settings:
  protocols:
    - chains:
        - chain-id: invalid
          short-names: [invalid]
        - chain-id: 0x1
          short-names: [ethereum, eth]
`

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mockYAML))
		}))
		defer server.Close()

		networks, err := vendor.fetchDrpcNetworks(ctx, server.URL)
		assert.NoError(t, err)
		assert.Len(t, networks, 1) // Only valid chain should be included
		assert.Equal(t, "ethereum", networks[1])
	})

	t.Run("empty short names", func(t *testing.T) {
		mockYAML := `
version: v1
chain-settings:
  protocols:
    - chains:
        - chain-id: 0x1
          short-names: []
        - chain-id: 0x89
          short-names: [polygon, matic]
`

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/yaml")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mockYAML))
		}))
		defer server.Close()

		networks, err := vendor.fetchDrpcNetworks(ctx, server.URL)
		assert.NoError(t, err)
		assert.Len(t, networks, 1) // Only chain with short names should be included
		assert.Equal(t, "polygon", networks[137])
	})
}

func TestDrpcVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	vendor := CreateDrpcVendor()

	t.Run("unauthorized error", func(t *testing.T) {
		req := &common.NormalizedRequest{}
		resp := &http.Response{}
		jrr := &common.JsonRpcResponse{
			Error: &common.ErrJsonRpcExceptionExternal{
				Code:    -32001,
				Message: "token is invalid",
				Data:    "additional error data",
			},
		}
		details := make(map[string]interface{})

		err := vendor.GetVendorSpecificErrorIfAny(req, resp, jrr, details)
		assert.Error(t, err)
		assert.IsType(t, &common.ErrEndpointUnauthorized{}, err)
		assert.Equal(t, "additional error data", details["data"])
	})

	t.Run("other error", func(t *testing.T) {
		req := &common.NormalizedRequest{}
		resp := &http.Response{}
		jrr := &common.JsonRpcResponse{
			Error: &common.ErrJsonRpcExceptionExternal{
				Code:    -32000,
				Message: "some other error",
			},
		}
		details := make(map[string]interface{})

		err := vendor.GetVendorSpecificErrorIfAny(req, resp, jrr, details)
		assert.NoError(t, err) // Should return nil for non-auth errors
	})

	t.Run("non-JsonRpcResponse", func(t *testing.T) {
		req := &common.NormalizedRequest{}
		resp := &http.Response{}
		jrr := "not a JsonRpcResponse"
		details := make(map[string]interface{})

		err := vendor.GetVendorSpecificErrorIfAny(req, resp, jrr, details)
		assert.NoError(t, err)
	})

	t.Run("ChainException error", func(t *testing.T) {
		req := &common.NormalizedRequest{}
		resp := &http.Response{}
		jrr := &common.JsonRpcResponse{
			Error: &common.ErrJsonRpcExceptionExternal{
				Code:    -32005,
				Message: "ChainException: Unexpected error (code=40000)",
			},
		}
		details := make(map[string]interface{})

		err := vendor.GetVendorSpecificErrorIfAny(req, resp, jrr, details)
		assert.Error(t, err)
		assert.IsType(t, &common.ErrEndpointMissingData{}, err)
	})

	t.Run("invalid block range error", func(t *testing.T) {
		req := &common.NormalizedRequest{}
		resp := &http.Response{}
		jrr := &common.JsonRpcResponse{
			Error: &common.ErrJsonRpcExceptionExternal{
				Code:    -32602,
				Message: "invalid block range",
			},
		}
		details := make(map[string]interface{})

		err := vendor.GetVendorSpecificErrorIfAny(req, resp, jrr, details)
		assert.Error(t, err)
		assert.IsType(t, &common.ErrEndpointMissingData{}, err)
	})
}

func TestDrpcVendor_OwnsUpstream(t *testing.T) {
	vendor := CreateDrpcVendor()

	tests := []struct {
		name     string
		endpoint string
		expected bool
	}{
		{
			name:     "drpc protocol",
			endpoint: "drpc://some-endpoint",
			expected: true,
		},
		{
			name:     "evm+drpc protocol",
			endpoint: "evm+drpc://some-endpoint",
			expected: true,
		},
		{
			name:     "drpc.org domain",
			endpoint: "https://lb.drpc.org/ogrpc?network=ethereum&dkey=test",
			expected: true,
		},
		{
			name:     "other endpoint",
			endpoint: "https://mainnet.infura.io/v3/test",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &common.UpstreamConfig{
				Endpoint: tt.endpoint,
			}
			result := vendor.OwnsUpstream(upstream)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDrpcVendor_Name(t *testing.T) {
	vendor := CreateDrpcVendor()
	assert.Equal(t, "drpc", vendor.Name())
}

func TestDrpcVendor_CacheAndRecheck(t *testing.T) {
	vendor := CreateDrpcVendor().(*DrpcVendor)
	ctx := context.Background()

	mockYAML := `
version: v1
chain-settings:
  protocols:
    - chains:
        - chain-id: 0x1
          short-names: [ethereum, eth]
`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(mockYAML))
	}))
	defer server.Close()

	// First call should fetch data
	err := vendor.ensureRemoteData(ctx, time.Hour, server.URL)
	assert.NoError(t, err)
	assert.Contains(t, vendor.remoteData, server.URL)
	assert.Equal(t, "ethereum", vendor.remoteData[server.URL][1])

	// Second call within recheck interval should use cached data
	err = vendor.ensureRemoteData(ctx, time.Hour, server.URL)
	assert.NoError(t, err)

	// Call with short recheck interval should attempt refresh
	err = vendor.ensureRemoteData(ctx, time.Nanosecond, server.URL)
	assert.NoError(t, err)
}
