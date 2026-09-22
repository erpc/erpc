package thirdparty

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestGoldskyVendor_buildEndpointURL(t *testing.T) {
	v := &GoldskyVendor{}
	chainId := int64(1)

	testCases := []struct {
		name           string
		endpoint       string
		tier           string
		secret         string
		expectedScheme string
		expectedHost   string
		expectedPath   string
		expectedQuery  string
	}{
		{
			name:           "defaults to edge.goldsky.com standard tier",
			endpoint:       "",
			tier:           "",
			secret:         "sec123",
			expectedScheme: "https",
			expectedHost:   "edge.goldsky.com",
			expectedPath:   "/standard/evm/1",
			expectedQuery:  "secret=sec123",
		},
		{
			name:           "no secret omits the secret query param",
			endpoint:       "",
			tier:           "",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "edge.goldsky.com",
			expectedPath:   "/standard/evm/1",
			expectedQuery:  "",
		},
		{
			name:           "custom tier is used in the path",
			endpoint:       "",
			tier:           "custom",
			secret:         "sec123",
			expectedScheme: "https",
			expectedHost:   "edge.goldsky.com",
			expectedPath:   "/custom/evm/1",
			expectedQuery:  "secret=sec123",
		},
		{
			name:           "bare host override defaults to https and standard tier",
			endpoint:       "gateway.example.com",
			tier:           "",
			secret:         "sec123",
			expectedScheme: "https",
			expectedHost:   "gateway.example.com",
			expectedPath:   "/standard/evm/1",
			expectedQuery:  "secret=sec123",
		},
		{
			name:           "full https endpoint with explicit path is respected",
			endpoint:       "https://edge.goldsky.com/custom/route",
			tier:           "",
			secret:         "sec123",
			expectedScheme: "https",
			expectedHost:   "edge.goldsky.com",
			expectedPath:   "/custom/route/1",
			expectedQuery:  "secret=sec123",
		},
		{
			name:           "goldsky:// authority is the secret, host defaults to edge",
			endpoint:       "goldsky://shorthand-secret",
			tier:           "",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "edge.goldsky.com",
			expectedPath:   "/standard/evm/1",
			expectedQuery:  "secret=shorthand-secret",
		},
		{
			name:           "goldsky:// authority secret with tier query",
			endpoint:       "goldsky://shorthand-secret?tier=custom",
			tier:           "",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "edge.goldsky.com",
			expectedPath:   "/custom/evm/1",
			expectedQuery:  "secret=shorthand-secret",
		},
		{
			name:           "evm+goldsky:// authority is the secret",
			endpoint:       "evm+goldsky://shorthand-secret",
			tier:           "",
			secret:         "",
			expectedScheme: "https",
			expectedHost:   "edge.goldsky.com",
			expectedPath:   "/standard/evm/1",
			expectedQuery:  "secret=shorthand-secret",
		},
		{
			name:           "existing secret in base URL is not overwritten",
			endpoint:       "https://edge.goldsky.com/standard/evm?secret=existing",
			tier:           "",
			secret:         "override",
			expectedScheme: "https",
			expectedHost:   "edge.goldsky.com",
			expectedPath:   "/standard/evm/1",
			expectedQuery:  "secret=existing",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			parsedURL, err := v.buildEndpointURL(tc.endpoint, tc.tier, tc.secret, chainId)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedScheme, parsedURL.Scheme)
			assert.Equal(t, tc.expectedHost, parsedURL.Host)
			assert.Equal(t, tc.expectedPath, parsedURL.Path)
			assert.Equal(t, tc.expectedQuery, parsedURL.RawQuery)
		})
	}
}

func TestGoldskyVendor_SupportsNetwork_SkipsWithoutProbe(t *testing.T) {
	v := CreateGoldskyVendor()
	logger := zerolog.Nop()
	ctx := context.Background()

	// Non-evm networks short-circuit before any secret/probe logic.
	ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{"secret": "x"}, "solana:mainnet")
	assert.NoError(t, err)
	assert.False(t, ok)

	// No secret anywhere (settings or endpoint) → silent skip, no network probe.
	ok, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
	assert.NoError(t, err)
	assert.False(t, ok)

	// A custom endpoint with no derivable secret also skips rather than probing.
	ok, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": "https://gateway.example.com"}, "evm:1")
	assert.NoError(t, err)
	assert.False(t, ok)
}

func TestGoldskyVendor_OwnsUpstream(t *testing.T) {
	vendor := CreateGoldskyVendor()
	cases := []struct {
		endpoint string
		owned    bool
	}{
		{"goldsky://sec123", true},
		{"evm+goldsky://sec123", true},
		{"https://edge.goldsky.com/standard/evm/1", true},
		{"https://example.com/rpc", false},
	}
	for _, c := range cases {
		t.Run(c.endpoint, func(t *testing.T) {
			got := vendor.OwnsUpstream(&common.UpstreamConfig{Endpoint: c.endpoint})
			assert.Equal(t, c.owned, got)
		})
	}
}
