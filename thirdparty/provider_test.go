package thirdparty

import (
	"context"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type MockVendor struct {
	mock.Mock
}

func (m *MockVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error {
	panic("unimplemented")
}

func (m *MockVendor) Name() string {
	panic("unimplemented")
}

func (m *MockVendor) OwnsUpstream(upstream *common.UpstreamConfig) bool {
	panic("unimplemented")
}

func (m *MockVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	args := m.Called(ctx, logger, settings, networkId)
	return args.Bool(0), args.Error(1)
}

func (m *MockVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	args := m.Called(ctx, logger, baseConfig, settings)
	return args.Get(0).([]*common.UpstreamConfig), args.Error(1)
}

func TestProvider_SupportsNetwork_WithIgnoreNetworks(t *testing.T) {
	logger := zerolog.New(nil)
	mockVendor := new(MockVendor)

	tests := []struct {
		name             string
		ignoreNetworks   []string
		onlyNetworks     []string
		networkId        string
		expected         bool
		shouldCallVendor bool
	}{
		{
			name:             "network in ignore list should return false",
			ignoreNetworks:   []string{"evm:123", "evm:137"},
			networkId:        "evm:123",
			expected:         false,
			shouldCallVendor: false,
		},
		{
			name:             "network not in ignore list should check vendor",
			ignoreNetworks:   []string{"evm:123", "evm:137"},
			networkId:        "evm:56",
			expected:         true,
			shouldCallVendor: true,
		},
		{
			name:             "no ignore networks should check vendor",
			networkId:        "evm:123",
			expected:         true,
			shouldCallVendor: true,
		},
		{
			name:             "only networks takes precedence over vendor",
			onlyNetworks:     []string{"evm:123", "evm:137"},
			networkId:        "evm:123",
			expected:         true,
			shouldCallVendor: false,
		},
		{
			name:             "network not in only networks should return false",
			onlyNetworks:     []string{"evm:123", "evm:137"},
			networkId:        "evm:56",
			expected:         false,
			shouldCallVendor: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockVendor.ExpectedCalls = nil

			config := &common.ProviderConfig{
				Id:             "test-provider",
				Vendor:         "test",
				IgnoreNetworks: tt.ignoreNetworks,
				OnlyNetworks:   tt.onlyNetworks,
			}

			provider := NewProvider(&logger, config, mockVendor, nil)

			if tt.shouldCallVendor {
				mockVendor.On("SupportsNetwork", mock.Anything, mock.Anything, mock.Anything, tt.networkId).Return(tt.expected, nil)
			}

			result, err := provider.SupportsNetwork(context.Background(), tt.networkId)

			assert.NoError(t, err)
			assert.Equal(t, tt.expected, result)

			if tt.shouldCallVendor {
				mockVendor.AssertExpectations(t)
			}
		})
	}
}

// Overrides do not merge — the winning pattern is copied whole — so when
// several match, WHICH one wins decides the entire upstream config. Selecting
// the first match out of a Go map made that depend on randomized iteration
// order, which is how a live deployment ended up with one chain's cost tier and
// rate-limit budget differing between pods: the provider declared both "evm:*"
// (carrying tags + rateLimitBudget) and "evm:42161" (carrying neither).
func TestProviderOverrideMostSpecificWins(t *testing.T) {
	cfg := &common.ProviderConfig{
		Id:     "p",
		Vendor: "alchemy",
		Overrides: map[string]*common.UpstreamConfig{
			"evm:*":     {Tags: []string{"tier:expensive"}, RateLimitBudget: "shared"},
			"evm:42161": {RateLimitBudget: "arbitrum-only"},
			"evm:1*":    {RateLimitBudget: "prefix"},
		},
	}
	p := &Provider{config: cfg}

	// Run it many times: a map-order-dependent implementation fails this
	// probabilistically, which is exactly how the bug hid in production.
	for i := 0; i < 200; i++ {
		got, err := p.buildBaseUpstreamConfig("evm:42161")
		assert.NoError(t, err)
		assert.Equal(t, "arbitrum-only", got.RateLimitBudget,
			"exact pattern must beat evm:* and evm:1* every time")
	}
	for i := 0; i < 200; i++ {
		got, err := p.buildBaseUpstreamConfig("evm:1")
		assert.NoError(t, err)
		assert.Equal(t, "prefix", got.RateLimitBudget,
			"the longer literal prefix must beat the bare wildcard every time")
	}
	got, err := p.buildBaseUpstreamConfig("evm:999")
	assert.NoError(t, err)
	assert.Equal(t, "shared", got.RateLimitBudget, "the wildcard still applies when nothing more specific matches")
	assert.Equal(t, []string{"tier:expensive"}, got.Tags)
}
