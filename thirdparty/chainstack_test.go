package thirdparty

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestChainstackVendor_GenerateConfigs(t *testing.T) {
	t.Parallel()
	vendor := CreateChainstackVendor()
	ctx := context.Background()
	logger := zerolog.Nop()

	// Test with API key (will use dynamic node discovery)
	t.Run("with API key - authentication failure", func(t *testing.T) {
		t.Parallel()
		settings := common.VendorSettings{
			"apiKey":          "test-api-key",
			"recheckInterval": 2 * time.Hour,
		}

		upstream := &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123, // Ethereum mainnet
			},
		}

		// This will attempt to fetch nodes from the API
		// With a test API key, it should fail with authentication error or network error
		configs, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)

		// Should return an error (authentication failure or network error in test environments)
		if assert.Error(t, err) {
			// Accept either authentication_failed or network errors (DNS, connection, etc.)
			errorMsg := err.Error()
			hasExpectedError := strings.Contains(errorMsg, "authentication_failed") ||
				strings.Contains(errorMsg, "dial tcp") ||
				strings.Contains(errorMsg, "lookup") ||
				strings.Contains(errorMsg, "connection refused") ||
				strings.Contains(errorMsg, "server misbehaving")
			assert.True(t, hasExpectedError, "Expected authentication or network error, got: %s", errorMsg)
		}
		assert.Nil(t, configs)
	})

	// Test without API key (should fail)
	t.Run("without API key", func(t *testing.T) {
		settings := common.VendorSettings{}

		upstream := &common.UpstreamConfig{
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		_, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "apiKey is required")
	})

	// Test with pre-defined endpoint (should not make API calls)
	t.Run("with pre-defined endpoint", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey": "test-api-key",
		}

		upstream := &common.UpstreamConfig{
			Endpoint: "https://ethereum-mainnet.core.chainstack.com/test-key",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}

		// Should return the upstream as-is without making API calls
		configs, err := vendor.GenerateConfigs(ctx, &logger, upstream, settings)
		assert.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, upstream.Endpoint, configs[0].Endpoint)
	})

	// Test SupportsNetwork with API key
	t.Run("supports network with API key - authentication failure", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey": "test-api-key",
		}

		// Should return false due to authentication failure
		supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:123")
		assert.Error(t, err)
		assert.False(t, supported)
	})

	// Test SupportsNetwork without API key
	t.Run("supports network without API key", func(t *testing.T) {
		settings := common.VendorSettings{}

		// Should return false when no API key is provided
		supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "evm:123")
		assert.NoError(t, err)
		assert.False(t, supported)
	})

	// Test SupportsNetwork with non-EVM network
	t.Run("supports network - non-EVM", func(t *testing.T) {
		settings := common.VendorSettings{
			"apiKey": "test-api-key",
		}

		// Should return false for non-EVM networks
		supported, err := vendor.SupportsNetwork(ctx, &logger, settings, "solana:mainnet")
		assert.NoError(t, err)
		assert.False(t, supported)
	})
}

// TestChainstackNode_ResilientDecoding demonstrates that the vendor can handle
// API responses with unexpected field types
func TestChainstackNode_ResilientDecoding(t *testing.T) {
	t.Parallel()
	// Example of a response with mixed types that would cause issues
	// Note: version is a number instead of string in the original API response
	rawJSON := `{
		"id": "ND-123-456",
		"status": "running",
		"name": "Test Node",
		"version": 25.3,
		"configuration": {
			"archive": true,
			"someUnknownField": {"nested": "data"}
		},
		"details": {
			"https_endpoint": "https://test.endpoint.com",
			"auth_key": "test-key",
			"version": 25.3,
			"api_namespaces": ["eth", "web3"]
		},
		"unknownField": "ignored"
	}`

	var node ChainstackNode
	err := json.Unmarshal([]byte(rawJSON), &node)

	// Should decode successfully, ignoring unknown fields
	assert.NoError(t, err)
	assert.Equal(t, "ND-123-456", node.ID)
	assert.Equal(t, "running", node.Status)
	assert.Equal(t, "https://test.endpoint.com", node.Details.HTTPSEndpoint)
	assert.Equal(t, "test-key", node.Details.AuthKey)
	assert.True(t, node.Configuration.Archive)
}

func TestChainstackFilterParams(t *testing.T) {
	t.Parallel()
	vendor := CreateChainstackVendor().(*ChainstackVendor)

	tests := []struct {
		name     string
		settings common.VendorSettings
		expected *ChainstackFilterParams
	}{
		{
			name: "no filters",
			settings: common.VendorSettings{
				"apiKey": "test-key",
			},
			expected: &ChainstackFilterParams{},
		},
		{
			name: "all filters",
			settings: common.VendorSettings{
				"apiKey":       "test-key",
				"project":      "proj-123",
				"organization": "org-456",
				"region":       "us-east-1",
				"provider":     "aws",
				"type":         "dedicated",
			},
			expected: &ChainstackFilterParams{
				Project:      "proj-123",
				Organization: "org-456",
				Region:       "us-east-1",
				Provider:     "aws",
				Type:         "dedicated",
			},
		},
		{
			name: "partial filters",
			settings: common.VendorSettings{
				"apiKey":  "test-key",
				"project": "proj-123",
				"type":    "shared",
			},
			expected: &ChainstackFilterParams{
				Project: "proj-123",
				Type:    "shared",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := vendor.extractFilterParams(tt.settings)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestChainstackCacheKey(t *testing.T) {
	t.Parallel()
	vendor := CreateChainstackVendor().(*ChainstackVendor)

	tests := []struct {
		name     string
		apiKey   string
		params   *ChainstackFilterParams
		expected string
	}{
		{
			name:     "no filters",
			apiKey:   "test-key",
			params:   &ChainstackFilterParams{},
			expected: "test-key",
		},
		{
			name:   "all filters",
			apiKey: "test-key",
			params: &ChainstackFilterParams{
				Project:      "proj-123",
				Organization: "org-456",
				Region:       "us-east-1",
				Provider:     "aws",
				Type:         "dedicated",
			},
			expected: "test-key_p:proj-123_o:org-456_r:us-east-1_pr:aws_t:dedicated",
		},
		{
			name:   "partial filters",
			apiKey: "test-key",
			params: &ChainstackFilterParams{
				Project: "proj-123",
				Type:    "shared",
			},
			expected: "test-key_p:proj-123_t:shared",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := vendor.getCacheKey(tt.apiKey, tt.params)
			assert.Equal(t, tt.expected, result)
		})
	}
}
