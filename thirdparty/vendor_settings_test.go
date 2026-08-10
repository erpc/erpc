package thirdparty

import (
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shorthandCase is one `<vendor>://…` upstream endpoint and the provider
// settings it must translate into. These expectations are the contract of the
// endpoint shorthand: they were lifted verbatim from the vendor-name switch
// that used to live in common/defaults.go (buildProviderSettings) before each
// vendor took ownership of its own parser.
type shorthandCase struct {
	name          string
	vendor        string
	endpoint      string
	expected      common.VendorSettings
	errorContains string
}

var shorthandCases = []shorthandCase{
	{
		name:     "goldsky secret in authority",
		vendor:   "goldsky",
		endpoint: "goldsky://my-edge-secret",
		expected: common.VendorSettings{"secret": "my-edge-secret"},
	},
	{
		name:     "goldsky tier query param",
		vendor:   "goldsky",
		endpoint: "goldsky://my-edge-secret?tier=custom",
		expected: common.VendorSettings{"secret": "my-edge-secret", "tier": "custom"},
	},
	{
		name:     "goldsky secret query param fallback",
		vendor:   "goldsky",
		endpoint: "goldsky://?secret=query-secret",
		expected: common.VendorSettings{"secret": "query-secret"},
	},
	{
		name:     "alchemy",
		vendor:   "alchemy",
		endpoint: "alchemy://some_test_api",
		expected: common.VendorSettings{"apiKey": "some_test_api"},
	},
	{
		name:     "alchemy via evm+ alias",
		vendor:   "alchemy",
		endpoint: "evm+alchemy://some_test_api",
		expected: common.VendorSettings{"apiKey": "some_test_api"},
	},
	{
		name:     "ankr",
		vendor:   "ankr",
		endpoint: "ankr://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "blastapi",
		vendor:   "blastapi",
		endpoint: "blastapi://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "blockdaemon",
		vendor:   "blockdaemon",
		endpoint: "blockdaemon://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "blockpi",
		vendor:   "blockpi",
		endpoint: "blockpi://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "chainstack with filters",
		vendor:   "chainstack",
		endpoint: "chainstack://test-api-key?project=proj-123&organization=org-456&region=us-east-1&provider=aws&type=dedicated",
		expected: common.VendorSettings{
			"apiKey":       "test-api-key",
			"project":      "proj-123",
			"organization": "org-456",
			"region":       "us-east-1",
			"provider":     "aws",
			"type":         "dedicated",
		},
	},
	{
		name:     "chainstack with partial filters",
		vendor:   "chainstack",
		endpoint: "chainstack://test-api-key?project=proj-123&type=shared",
		expected: common.VendorSettings{
			"apiKey":  "test-api-key",
			"project": "proj-123",
			"type":    "shared",
		},
	},
	{
		name:     "chainstack without filters",
		vendor:   "chainstack",
		endpoint: "chainstack://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "chainstack repeated query param keeps every value",
		vendor:   "chainstack",
		endpoint: "chainstack://test-api-key?region=us-east-1&region=eu-west-1",
		expected: common.VendorSettings{
			"apiKey": "test-api-key",
			"region": []string{"us-east-1", "eu-west-1"},
		},
	},
	{
		name:     "conduit",
		vendor:   "conduit",
		endpoint: "conduit://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "drpc",
		vendor:   "drpc",
		endpoint: "drpc://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "dwellir",
		vendor:   "dwellir",
		endpoint: "dwellir://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "envio",
		vendor:   "envio",
		endpoint: "envio://rpc.hypersync.xyz",
		expected: common.VendorSettings{"rootDomain": "rpc.hypersync.xyz"},
	},
	{
		name:     "erpc with path and secret",
		vendor:   "erpc",
		endpoint: "erpc://rpc.example.com/main?secret=my-secret",
		expected: common.VendorSettings{
			"endpoint": "https://rpc.example.com/main",
			"secret":   "my-secret",
		},
	},
	{
		name:     "erpc without path or secret",
		vendor:   "erpc",
		endpoint: "erpc://rpc.example.com",
		expected: common.VendorSettings{"endpoint": "https://rpc.example.com/"},
	},
	{
		name:     "etherspot",
		vendor:   "etherspot",
		endpoint: "etherspot://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "infura",
		vendor:   "infura",
		endpoint: "infura://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "llama",
		vendor:   "llama",
		endpoint: "llama://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "onfinality",
		vendor:   "onfinality",
		endpoint: "onfinality://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "pimlico",
		vendor:   "pimlico",
		endpoint: "pimlico://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "quicknode with filters",
		vendor:   "quicknode",
		endpoint: "quicknode://test-api-key?tagIds=123,456&tagLabels=production,staging",
		expected: common.VendorSettings{
			"apiKey":    "test-api-key",
			"tagIds":    []int{123, 456},
			"tagLabels": []string{"production", "staging"},
		},
	},
	{
		name:     "quicknode with single filters",
		vendor:   "quicknode",
		endpoint: "quicknode://test-api-key?tagIds=123&tagLabels=production",
		expected: common.VendorSettings{
			"apiKey":    "test-api-key",
			"tagIds":    123,
			"tagLabels": "production",
		},
	},
	{
		name:     "quicknode drops non-numeric tagIds",
		vendor:   "quicknode",
		endpoint: "quicknode://test-api-key?tagIds=abc",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "quicknode without filters",
		vendor:   "quicknode",
		endpoint: "quicknode://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "repository with path and query",
		vendor:   "repository",
		endpoint: "repository://evm-public-endpoints.erpc.cloud/list?tier=free",
		expected: common.VendorSettings{
			"repositoryUrl": "https://evm-public-endpoints.erpc.cloud/list?tier=free",
		},
	},
	{
		name:     "repository without path or query",
		vendor:   "repository",
		endpoint: "repository://evm-public-endpoints.erpc.cloud",
		expected: common.VendorSettings{
			// The trailing "?" is what the shorthand has always produced.
			"repositoryUrl": "https://evm-public-endpoints.erpc.cloud/?",
		},
	},
	{
		name:     "routemesh",
		vendor:   "routemesh",
		endpoint: "routemesh://lb.routemes.sh/rpc/1/test-api-key",
		expected: common.VendorSettings{
			"baseURL": "lb.routemes.sh",
			"apiKey":  "test-api-key",
		},
	},
	{
		name:          "routemesh with malformed path",
		vendor:        "routemesh",
		endpoint:      "routemesh://lb.routemes.sh/1/test-api-key",
		errorContains: "routemesh endpoint path must be in format /rpc/<chainId>/<apiKey>",
	},
	{
		name:     "satelink",
		vendor:   "satelink",
		endpoint: "satelink://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "superchain with registry path",
		vendor:   "superchain",
		endpoint: "superchain://example.com/chainList.json",
		expected: common.VendorSettings{"registryUrl": "example.com/chainList.json"},
	},
	{
		name:     "superchain host only",
		vendor:   "superchain",
		endpoint: "superchain://example.com/",
		expected: common.VendorSettings{"registryUrl": "example.com"},
	},
	{
		name:     "tenderly",
		vendor:   "tenderly",
		endpoint: "tenderly://test-api-key",
		expected: common.VendorSettings{"apiKey": "test-api-key"},
	},
	{
		name:     "thirdweb",
		vendor:   "thirdweb",
		endpoint: "thirdweb://test-client-id",
		expected: common.VendorSettings{"clientId": "test-client-id"},
	},
}

// TestVendorSettingsShorthand drives every shorthand through the real config
// path (ProjectConfig.SetDefaults → convertUpstreamToProvider), which also
// proves each vendor registered its parser under the name common looks up.
func TestVendorSettingsShorthand(t *testing.T) {
	for _, tc := range shorthandCases {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := shorthandToProvider(tc.endpoint)
			if tc.errorContains != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.errorContains)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.vendor, provider.Vendor)
			assert.Equal(t, tc.expected, provider.Settings)
		})
	}
}

// TestVendorSettingsShorthand_CoversEveryVendor keeps the table above honest:
// every vendor the registry serves must have at least one expectation here, so
// a vendor whose parser is missing (or registered under a typo'd name) cannot
// slip through.
func TestVendorSettingsShorthand_CoversEveryVendor(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range shorthandCases {
		covered[tc.vendor] = true
	}
	for _, name := range NewVendorsRegistry().SupportedVendors() {
		assert.Truef(t, covered[name],
			"vendor %q has no `%s://` shorthand expectation in shorthandCases", name, name)
	}
}

func TestVendorSettingsShorthand_UnknownVendor(t *testing.T) {
	_, err := shorthandToProvider("notavendor://some-key")
	require.Error(t, err)
	assert.ErrorContains(t, err, "unsupported vendor name in vendor.settings: notavendor")
}

// TestVendorSettingsShorthand_MixedWithPlainUpstreams checks the surrounding
// conversion: only the shorthand upstream becomes a provider, the plain http
// upstreams stay put, in order.
func TestVendorSettingsShorthand_MixedWithPlainUpstreams(t *testing.T) {
	cfg := &common.Config{
		Projects: []*common.ProjectConfig{
			{
				Id: "test1",
				Upstreams: []*common.UpstreamConfig{
					{Endpoint: "http://rpc1.localhost"},
					{Endpoint: "alchemy://some_test_api"},
					{Endpoint: "http://rpc3.localhost"},
				},
			},
		},
	}

	require.NoError(t, cfg.SetDefaults(&common.DefaultOptions{}))
	assert.Len(t, cfg.Projects[0].Upstreams, 2)
	require.Len(t, cfg.Projects[0].Providers, 1)
	assert.EqualValues(t, "alchemy", cfg.Projects[0].Providers[0].Vendor)
	assert.Equal(t, common.VendorSettings{"apiKey": "some_test_api"}, cfg.Projects[0].Providers[0].Settings)
	assert.EqualValues(t, "http://rpc1.localhost", cfg.Projects[0].Upstreams[0].Endpoint)
	assert.EqualValues(t, "http://rpc3.localhost", cfg.Projects[0].Upstreams[1].Endpoint)
}

// TestVendorSettingsShorthand_OnlyProviderValidates covers a project whose only
// upstream is a shorthand: after conversion the project has zero upstreams and
// must still validate.
func TestVendorSettingsShorthand_OnlyProviderValidates(t *testing.T) {
	cfg := &common.Config{
		Projects: []*common.ProjectConfig{
			{
				Id: "test-alchemy-only",
				Upstreams: []*common.UpstreamConfig{
					{Endpoint: "alchemy://some_test_api_key"},
				},
			},
		},
	}

	require.NoError(t, cfg.SetDefaults(&common.DefaultOptions{}))

	project := cfg.Projects[0]
	assert.Len(t, project.Upstreams, 0, "shorthand upstream should be replaced by a provider")
	require.Len(t, project.Providers, 1)
	assert.Equal(t, "alchemy", project.Providers[0].Vendor)
	assert.Equal(t, common.VendorSettings{"apiKey": "some_test_api_key"}, project.Providers[0].Settings)

	assert.NoError(t, project.Validate(cfg), "a project with only a provider should validate")
}

// shorthandToProvider runs one shorthand endpoint through config defaults and
// returns the provider it was converted into.
func shorthandToProvider(endpoint string) (*common.ProviderConfig, error) {
	cfg := &common.Config{
		Projects: []*common.ProjectConfig{
			{
				Id:        "test-shorthand",
				Upstreams: []*common.UpstreamConfig{{Endpoint: endpoint}},
			},
		},
	}
	if err := cfg.SetDefaults(&common.DefaultOptions{}); err != nil {
		return nil, err
	}
	providers := cfg.Projects[0].Providers
	if len(providers) != 1 {
		return nil, fmt.Errorf("expected exactly 1 provider from %q, got %d", endpoint, len(providers))
	}
	return providers[0], nil
}
