package erpc

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

// The directive gate in front of tryServeLogsFill.
func TestBlockStoreDirected_SkipCacheReadSemantics(t *testing.T) {
	require.False(t, blockStoreDirected(nil))
	for skip, want := range map[string]bool{
		"": false, "false": false, "FALSE": false, "False": false,
		"true": true, "*": true, "redis-*": true, "pg-cache": true,
	} {
		require.Equal(t, want, blockStoreDirected(&common.RequestDirectives{SkipCacheRead: skip}), "skipCacheRead=%q", skip)
	}
	require.True(t, blockStoreDirected(&common.RequestDirectives{SkipCacheRead: "false", UseUpstream: "a"}))
	require.True(t, blockStoreDirected(&common.RequestDirectives{IsInternal: true}))
	require.True(t, blockStoreDirected(&common.RequestDirectives{IntegritySelector: "strict"}))
}

func TestBlockStore_FingerprintIncludesProviderAndDefaultTrust(t *testing.T) {
	network := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 1}}
	project := &common.ProjectConfig{
		Upstreams:        []*common.UpstreamConfig{{Id: "one", Endpoint: "https://rpc.invalid/key-a"}, {Id: "two", Endpoint: "https://rpc.invalid/key-b"}},
		Providers:        []*common.ProviderConfig{{Id: "provider", Vendor: "arbitrary-future-vendor", Settings: common.VendorSettings{"apiKey": "secret-a"}, Overrides: map[string]*common.UpstreamConfig{"evm:*": {Endpoint: "https://override.invalid/key-a"}}}},
		NetworkDefaults:  &common.NetworkDefaults{SelectionPolicy: &common.SelectionPolicyConfig{EvalFunc: "upstreams => upstreams"}},
		UpstreamDefaults: &common.UpstreamConfig{Id: "defaults", Endpoint: "https://default.invalid/key-a"},
	}
	fingerprint := func() string {
		hash, err := blockStoreFingerprint(project, network)
		require.NoError(t, err)
		require.Len(t, hash, 64)
		require.NotContains(t, hash, "secret")
		return hash
	}
	original := fingerprint()
	project.Upstreams[0], project.Upstreams[1] = project.Upstreams[1], project.Upstreams[0]
	require.Equal(t, original, fingerprint(), "set ordering is irrelevant")
	project.Providers[0].Settings["apiKey"] = "secret-b"
	require.NotEqual(t, original, fingerprint(), "provider settings must not be display-redacted before hashing")
	previous := fingerprint()
	project.Providers[0].Overrides["evm:*"].Endpoint = "https://override.invalid/key-b"
	require.NotEqual(t, previous, fingerprint())
	previous = fingerprint()
	project.Providers[0].IgnoreNetworks = []string{"evm:1"}
	require.NotEqual(t, previous, fingerprint())
	previous = fingerprint()
	project.NetworkDefaults.SelectionPolicy.EvalFunc = "upstreams => []"
	require.NotEqual(t, previous, fingerprint())
	previous = fingerprint()
	project.UpstreamDefaults.Endpoint = "https://default.invalid/key-b"
	require.NotEqual(t, previous, fingerprint())
	previous = fingerprint()
	network.SelectionPolicy = &common.SelectionPolicyConfig{EvalFunc: "upstreams => upstreams.slice(1)"}
	require.NotEqual(t, previous, fingerprint())
}
