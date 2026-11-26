package thirdparty

import (
	"context"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestGoldskyVendor_SupportsNetwork_Probe(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	probe := func(t *testing.T, endpoint string, networkId string) (bool, error) {
		return CreateGoldskyVendor().SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": endpoint, "secret": "sec"}, networkId)
	}

	t.Run("matching chain id", func(t *testing.T) {
		ok, err := probe(t, newChainIdServer(t, "0x1").URL, "evm:1")
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("cached client reused", func(t *testing.T) {
		srv := newChainIdServer(t, "0x1")
		v := CreateGoldskyVendor()
		for range 2 {
			ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{"endpoint": srv.URL, "secret": "sec"}, "evm:1")
			require.NoError(t, err)
			assert.True(t, ok)
		}
	})

	t.Run("secret from goldsky:// shorthand is enough to probe", func(t *testing.T) {
		// The shorthand always targets the real Edge host, so assert only that the
		// derived secret makes it past the no-secret skip (the URL is built correctly).
		u, err := (&GoldskyVendor{}).buildEndpointURL("goldsky://tok", "", "", 1)
		require.NoError(t, err)
		assert.Equal(t, "https://edge.goldsky.com/standard/evm/1?secret=tok", u.String())
	})

	t.Run("mismatching chain id", func(t *testing.T) {
		ok, err := probe(t, newChainIdServer(t, "0x89").URL, "evm:1")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("json-rpc error response", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`)
		ok, err := probe(t, srv.URL, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-hex result", func(t *testing.T) {
		ok, err := probe(t, newChainIdServer(t, "nope").URL, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("non-string result", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":[1]}`)
		ok, err := probe(t, srv.URL, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("server down", func(t *testing.T) {
		srv := newChainIdServer(t, "0x1")
		url := srv.URL
		srv.Close()
		ok, err := probe(t, url, "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("unparsable chain id", func(t *testing.T) {
		ok, err := probe(t, "http://127.0.0.1:1", "evm:xyz")
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("malformed endpoint", func(t *testing.T) {
		ok, err := probe(t, "http://[::1", "evm:1")
		assert.Error(t, err)
		assert.False(t, ok)
	})
}

func TestGoldskyVendor_GenerateConfigs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	cases := []struct {
		name     string
		endpoint string
		settings common.VendorSettings
		expected string
	}{
		{"settings secret default host", "", common.VendorSettings{"secret": "s1"}, "https://edge.goldsky.com/standard/evm/8453?secret=s1"},
		{"settings tier and custom host", "", common.VendorSettings{"secret": "s1", "tier": "premium", "endpoint": "gw.example.com"}, "https://gw.example.com/premium/evm/8453?secret=s1"},
		{"preset goldsky:// shorthand", "goldsky://tok?tier=custom", nil, "https://edge.goldsky.com/custom/evm/8453?secret=tok"},
		{"preset evm+goldsky:// shorthand", "evm+goldsky://tok", nil, "https://edge.goldsky.com/standard/evm/8453?secret=tok"},
		{"preset https endpoint passes through", "https://edge.goldsky.com/standard/evm/8453?secret=x", nil, "https://edge.goldsky.com/standard/evm/8453?secret=x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ups := &common.UpstreamConfig{Endpoint: c.endpoint, Evm: &common.EvmUpstreamConfig{ChainId: 8453}}
			cfgs, err := CreateGoldskyVendor().GenerateConfigs(ctx, &logger, ups, c.settings)
			require.NoError(t, err)
			require.Len(t, cfgs, 1)
			assert.Equal(t, c.expected, cfgs[0].Endpoint)
			assert.Equal(t, common.UpstreamTypeEvm, cfgs[0].Type)
			assert.NotNil(t, cfgs[0].JsonRpc)
		})
	}

	errCases := []struct {
		name     string
		ups      *common.UpstreamConfig
		settings common.VendorSettings
		contains string
	}{
		{"shorthand without evm", &common.UpstreamConfig{Endpoint: "goldsky://tok"}, nil, "requires upstream.evm to be defined"},
		{"settings without evm", &common.UpstreamConfig{}, common.VendorSettings{"secret": "s"}, "requires upstream.evm to be defined"},
		{"settings without chain id", &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"secret": "s"}, "requires upstream.evm.chainId"},
		{"malformed settings endpoint", &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{"endpoint": "https://[::1"}, "missing ']'"},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			_, err := CreateGoldskyVendor().GenerateConfigs(ctx, &logger, c.ups, c.settings)
			assert.ErrorContains(t, err, c.contains)
		})
	}
}

func TestGoldskyVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateGoldskyVendor()
	assert.Equal(t, "goldsky", v.Name())

	err, details := vendorErr(t, v, "eth_getLogs", 500, -32000, "boom", "why")
	assert.NoError(t, err)
	assert.Equal(t, "why", details["data"])

	err, details = vendorErr(t, v, "eth_getLogs", 500, -32000, "boom", "")
	assert.NoError(t, err)
	assert.NotContains(t, details, "data")

	assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 500}, "not-a-response", map[string]interface{}{}))
}

func TestGoldskyVendor_buildEndpointURL_UnparsableShorthand(t *testing.T) {
	// An unparsable goldsky:// authority yields no derived secret, but still
	// targets the default Edge host instead of dialing the shorthand.
	u, err := (&GoldskyVendor{}).buildEndpointURL("goldsky://[bad", "", "fromsettings", 1)
	require.NoError(t, err)
	assert.Equal(t, "https://edge.goldsky.com/standard/evm/1?secret=fromsettings", u.String())
}

func TestGoldskyVendor_OwnsUpstream_ByVendorName(t *testing.T) {
	v := CreateGoldskyVendor()
	assert.True(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://rpc.example.com", VendorName: "goldsky"}))
	assert.False(t, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: "https://rpc.example.com", VendorName: "alchemy"}))
}
