package thirdparty

import (
	"context"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
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

func TestProvider_GenerateUpstreamConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("builds base config, applies vendor output, credit units and env vars", func(t *testing.T) {
		t.Setenv("PROVIDER_TEST_KEY", "sekret")
		mv := new(MockVendor)
		settings := common.VendorSettings{"creditUnits": map[string]interface{}{"eth_call": 3}}
		cfg := &common.ProviderConfig{
			Id:                 "prov",
			Vendor:             "mockvendor",
			Settings:           settings,
			UpstreamIdTemplate: "<VENDOR>-<PROVIDER>-<NETWORK>-<EVM_CHAIN_ID>",
		}
		defaults := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{NodeType: common.EvmNodeTypeArchive}}
		p := NewProvider(&logger, cfg, mv, defaults)
		assert.Equal(t, "prov", p.Id())

		preset := map[string]int64{"eth_getLogs": 9}
		mv.On("GenerateConfigs", mock.Anything, mock.Anything, mock.Anything, settings).
			Return([]*common.UpstreamConfig{
				{Endpoint: "https://rpc.example.com/${PROVIDER_TEST_KEY}"},
				{Endpoint: "https://rpc2.example.com", CreditUnits: preset},
			}, nil).
			Run(func(args mock.Arguments) {
				base := args.Get(2).(*common.UpstreamConfig)
				assert.Equal(t, "mockvendor-prov-evm:10-10", base.Id)
				assert.Equal(t, "mockvendor", base.VendorName)
				assert.Equal(t, common.UpstreamTypeEvm, base.Type)
				require.NotNil(t, base.Evm)
				assert.Equal(t, int64(10), base.Evm.ChainId)
				assert.Equal(t, common.EvmNodeTypeArchive, base.Evm.NodeType, "upstream defaults' evm settings apply")
			})

		got, err := p.GenerateUpstreamConfigs(ctx, &logger, "evm:10")
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "https://rpc.example.com/sekret", got[0].Endpoint)
		assert.Equal(t, map[string]int64{"eth_call": 3}, got[0].CreditUnits)
		assert.Equal(t, preset, got[1].CreditUnits, "an upstream's own credit units are not overwritten")
		mv.AssertExpectations(t)
	})

	t.Run("vendor error is returned", func(t *testing.T) {
		mv := new(MockVendor)
		p := NewProvider(&logger, &common.ProviderConfig{Id: "p", Vendor: "v"}, mv, nil)
		mv.On("GenerateConfigs", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return([]*common.UpstreamConfig(nil), assert.AnError)
		_, err := p.GenerateUpstreamConfigs(ctx, &logger, "evm:1")
		assert.ErrorIs(t, err, assert.AnError)
	})

	t.Run("bad evm chain id fails before the vendor is called", func(t *testing.T) {
		mv := new(MockVendor)
		p := NewProvider(&logger, &common.ProviderConfig{Id: "p", Vendor: "v"}, mv, nil)
		_, err := p.GenerateUpstreamConfigs(ctx, &logger, "evm:notanumber")
		assert.Error(t, err)
		mv.AssertNotCalled(t, "GenerateConfigs", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}

func TestProvider_buildBaseUpstreamConfig(t *testing.T) {
	t.Run("non-evm network gets no evm block and N/A chain id", func(t *testing.T) {
		p := &Provider{config: &common.ProviderConfig{Id: "p", Vendor: "v", UpstreamIdTemplate: "<PROVIDER>-<EVM_CHAIN_ID>-<NETWORK>"}}
		got, err := p.buildBaseUpstreamConfig("solana:mainnet")
		require.NoError(t, err)
		assert.Equal(t, "p-N/A-solana:mainnet", got.Id)
		assert.Equal(t, "v", got.VendorName)
	})

	t.Run("override is copied, not shared", func(t *testing.T) {
		override := &common.UpstreamConfig{RateLimitBudget: "b"}
		p := &Provider{config: &common.ProviderConfig{Id: "p", Vendor: "v", Overrides: map[string]*common.UpstreamConfig{"evm:1": override}}}
		got, err := p.buildBaseUpstreamConfig("evm:1")
		require.NoError(t, err)
		assert.Equal(t, "b", got.RateLimitBudget)
		assert.Equal(t, int64(1), got.Evm.ChainId)
		assert.Nil(t, override.Evm, "the provider's override must not be mutated")
	})

	t.Run("invalid override pattern is skipped", func(t *testing.T) {
		p := &Provider{config: &common.ProviderConfig{Id: "p", Vendor: "v", Overrides: map[string]*common.UpstreamConfig{
			"(evm:1": {RateLimitBudget: "broken"},
		}}}
		got, err := p.buildBaseUpstreamConfig("evm:1")
		require.NoError(t, err)
		assert.Empty(t, got.RateLimitBudget)
	})

	t.Run("equally specific patterns resolve lexicographically", func(t *testing.T) {
		p := &Provider{config: &common.ProviderConfig{Id: "p", Vendor: "v", Overrides: map[string]*common.UpstreamConfig{
			"evm:1*": {RateLimitBudget: "one-star"},
			"evm:*1": {RateLimitBudget: "star-one"},
		}}}
		for range 50 {
			got, err := p.buildBaseUpstreamConfig("evm:1")
			require.NoError(t, err)
			assert.Equal(t, "star-one", got.RateLimitBudget)
		}
	})
}
