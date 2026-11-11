package thirdparty

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVendorsRegistry_SupportedVendors(t *testing.T) {
	names := NewVendorsRegistry().SupportedVendors()
	for _, n := range []string{"goldsky", "erpc", "envio", "pimlico", "thirdweb", "tenderly", "routemesh"} {
		assert.Contains(t, names, n)
	}
	seen := map[string]bool{}
	for _, n := range names {
		assert.False(t, seen[n], "vendor %q registered twice", n)
		seen[n] = true
	}
}

func TestVendorsRegistry_LookupByName(t *testing.T) {
	r := NewVendorsRegistry()
	for _, n := range []string{"goldsky", "erpc", "tenderly", "routemesh"} {
		v := r.LookupByName(n)
		require.NotNil(t, v, n)
		assert.Equal(t, n, v.Name())
	}
	assert.Nil(t, r.LookupByName("no-such-vendor"))
}

func TestVendorsRegistry_LookupByUpstream(t *testing.T) {
	r := NewVendorsRegistry()

	byEndpoint := []struct {
		endpoint string
		vendor   string
	}{
		{"goldsky://sec", "goldsky"},
		{"erpc://edge.example.com", "erpc"},
		{"https://mainnet.gateway.tenderly.co/k", "tenderly"},
		{"https://lb.routemes.sh/rpc/1/k", "routemesh"},
		{"https://1.rpc.thirdweb.com/cid", "thirdweb"},
		{"https://api.pimlico.io/v2/1/rpc?apikey=k", "pimlico"},
		{"https://1.rpc.hypersync.xyz/k", "envio"},
	}
	for _, c := range byEndpoint {
		t.Run(c.endpoint, func(t *testing.T) {
			v := r.LookupByUpstream(&common.UpstreamConfig{Endpoint: c.endpoint})
			require.NotNil(t, v)
			assert.Equal(t, c.vendor, v.Name())
		})
	}

	t.Run("vendor name wins over endpoint ownership", func(t *testing.T) {
		v := r.LookupByUpstream(&common.UpstreamConfig{Endpoint: "goldsky://sec", VendorName: "tenderly"})
		require.NotNil(t, v)
		assert.Equal(t, "tenderly", v.Name())
	})

	t.Run("unknown vendor name", func(t *testing.T) {
		assert.Nil(t, r.LookupByUpstream(&common.UpstreamConfig{Endpoint: "goldsky://sec", VendorName: "nope"}))
	})

	t.Run("endpoint owned by nobody", func(t *testing.T) {
		assert.Nil(t, r.LookupByUpstream(&common.UpstreamConfig{Endpoint: "https://node.unowned-example.net/rpc"}))
	})
}

func TestVendorsRegistry_Register(t *testing.T) {
	r := &VendorsRegistry{}
	assert.Empty(t, r.SupportedVendors())
	r.Register(CreateRoutemeshVendor())
	assert.Equal(t, []string{"routemesh"}, r.SupportedVendors())
	assert.NotNil(t, r.LookupByUpstream(&common.UpstreamConfig{Endpoint: "https://lb.routemes.sh/rpc/1/k"}))
	assert.Nil(t, r.LookupByUpstream(&common.UpstreamConfig{Endpoint: "goldsky://sec"}))
}
