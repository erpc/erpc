package thirdparty

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChainstackVendor_FetchNodes(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("follows next links, forwards filters and skips invalid nodes", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		next := "https://api.chainstack.com/v1/nodes/?page=2"
		gock.New("https://api.chainstack.com").
			Get("/v1/nodes/").
			MatchHeader("Authorization", "^Bearer cs-key$").
			MatchParam("project", "^p1$").
			MatchParam("organization", "^o1$").
			MatchParam("region", "^r1$").
			MatchParam("provider", "^aws$").
			MatchParam("type", "^dedicated$").
			Reply(200).
			JSON(map[string]interface{}{
				"next": next,
				"results": []interface{}{
					map[string]interface{}{"id": "n1", "status": "running", "details": map[string]interface{}{"https_endpoint": "https://n1.core.chainstack.com", "auth_key": "a1"}},
					map[string]interface{}{"id": "noendpoint", "status": "running", "details": map[string]interface{}{}},
					map[string]interface{}{"id": 5, "status": "running"},
				},
			})
		gock.New("https://api.chainstack.com").
			Get("/v1/nodes/").
			MatchParam("page", "^2$").
			Reply(200).
			JSON(map[string]interface{}{
				"next":    nil,
				"results": []interface{}{map[string]interface{}{"id": "n2", "status": "stopped", "details": map[string]interface{}{"https_endpoint": "https://n2.core.chainstack.com"}}},
			})

		v := CreateChainstackVendor().(*ChainstackVendor)
		got, err := v.fetchNodes(ctx, &logger, "cs-key", &ChainstackFilterParams{
			Project: "p1", Organization: "o1", Region: "r1", Provider: "aws", Type: "dedicated",
		})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "n1", got[0].ID)
		assert.Equal(t, "a1", got[0].Details.AuthKey)
		assert.Equal(t, "n2", got[1].ID)
		assert.Equal(t, "stopped", got[1].Status)
		assert.True(t, gock.IsDone())
	})

	errCases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"non-200 status", http.StatusForbidden, `denied`, "status 403: denied"},
		{"undecodable body", http.StatusOK, `{"results":`, "failed to decode Chainstack nodes response"},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			gock.New("https://api.chainstack.com").Get("/v1/nodes/").Reply(c.status).BodyString(c.body)

			v := CreateChainstackVendor().(*ChainstackVendor)
			_, err := v.fetchNodes(ctx, &logger, "k", nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}

	t.Run("transport failure is an error", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://api.chainstack.com").Get("/v1/nodes/").ReplyError(io.ErrUnexpectedEOF)

		v := CreateChainstackVendor().(*ChainstackVendor)
		_, err := v.fetchNodes(ctx, &logger, "k", &ChainstackFilterParams{})
		require.Error(t, err)
	})

	t.Run("malformed next link is an error", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://api.chainstack.com").Get("/v1/nodes/").Reply(200).
			JSON(map[string]interface{}{"next": "http://bad host/", "results": []interface{}{}})

		v := CreateChainstackVendor().(*ChainstackVendor)
		_, err := v.fetchNodes(ctx, &logger, "k", nil)
		require.Error(t, err)
	})
}

func TestChainstackVendor_FetchChainIDs(t *testing.T) {
	logger := zerolog.Nop()
	srv := newChainIdRouter(t, map[string]string{"/eth/k1": "0x1", "/arb/k2": "0xa4b1", "/bad/k": "0xqq", "/html/k": "!"})

	nodes := []*ChainstackNode{
		{ID: "eth", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL + "/eth", AuthKey: "k1"}},
		{ID: "arb", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL + "/arb", AuthKey: "k2"}},
		{ID: "rpcerr", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL + "/none", AuthKey: "k"}},
		{ID: "badhex", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL + "/bad", AuthKey: "k"}},
		{ID: "html", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL + "/html", AuthKey: "k"}},
		{ID: "down", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: "http://127.0.0.1:1"}},
		{ID: "badurl", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: "http://bad host"}},
		{ID: "stopped", Status: "stopped", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL + "/eth", AuthKey: "k1"}},
		{ID: "noendpoint", Status: "running"},
	}
	v := CreateChainstackVendor().(*ChainstackVendor)
	require.NoError(t, v.fetchChainIDs(context.Background(), &logger, nodes))

	got := map[string]int64{}
	for _, n := range nodes {
		got[n.ID] = n.ChainID
	}
	assert.Equal(t, map[string]int64{
		"eth": 1, "arb": 42161, "rpcerr": 0, "badhex": 0, "html": 0, "down": 0, "badurl": 0, "stopped": 0, "noendpoint": 0,
	}, got, "only running nodes with an endpoint are probed")

	t.Run("cancelled context fails every acquisition", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ns := []*ChainstackNode{{ID: "eth", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL + "/eth", AuthKey: "k1"}}}
		require.NoError(t, v.fetchChainIDs(ctx, &logger, ns))
		assert.Zero(t, ns[0].ChainID)
	})
}

func TestChainstackVendor_DiscoveryLifecycle(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx := context.Background()
	logger := zerolog.Nop()
	probe := newChainIdRouter(t, map[string]string{"/a/ka": "0x1", "/b/kb": "0x1", "/c/kc": "0x89"})

	gock.New("https://api.chainstack.com").
		Get("/v1/nodes/").
		MatchParam("project", "^proj$").
		Persist().
		Reply(200).
		JSON(map[string]interface{}{"next": nil, "results": []interface{}{
			map[string]interface{}{"id": "a", "status": "running", "details": map[string]interface{}{"https_endpoint": probe.URL + "/a", "auth_key": "ka"}},
			map[string]interface{}{"id": "b", "status": "running", "details": map[string]interface{}{"https_endpoint": probe.URL + "/b", "auth_key": "kb"}},
			map[string]interface{}{"id": "c", "status": "running", "details": map[string]interface{}{"https_endpoint": probe.URL + "/c", "auth_key": "kc"}},
		}})

	v := CreateChainstackVendor()
	settings := common.VendorSettings{"apiKey": "cs", "project": "proj", "recheckInterval": time.Hour}

	ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
	assert.False(t, ok)

	require.Eventually(t, func() bool {
		ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
		return err == nil && ok
	}, 5*time.Second, 10*time.Millisecond)

	ok, err = v.SupportsNetwork(ctx, &logger, settings, "evm:10")
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = v.SupportsNetwork(ctx, &logger, settings, "evm:zz")
	assert.Error(t, err)

	got, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Id: "cs", Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.ElementsMatch(t, []string{"cs-a", "cs-b"}, []string{got[0].Id, got[1].Id})
	assert.ElementsMatch(t, []string{probe.URL + "/a/ka", probe.URL + "/b/kb"}, []string{got[0].Endpoint, got[1].Endpoint})
	for _, c := range got {
		assert.Equal(t, common.UpstreamTypeEvm, c.Type)
		require.NotNil(t, c.JsonRpc)
		assert.NotNil(t, c.JsonRpc.Headers)
	}

	got, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 137}}, settings)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "chainstack-137-c", got[0].Id)
}

func TestChainstackVendor_GenerateConfigs_Validation(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()
	v := CreateChainstackVendor().(*ChainstackVendor)

	_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, common.VendorSettings{"apiKey": "k"})
	assert.ErrorContains(t, err, "requires upstream.evm to be defined")

	_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"apiKey": "k"})
	assert.ErrorContains(t, err, "requires upstream.evm.chainId")

	// Mark discovery as in flight so the cold path never dials out.
	v.cache.inflight["cold"] = struct{}{}
	_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{"apiKey": "cold", "recheckInterval": time.Minute})
	assert.ErrorIs(t, err, ErrRemoteCacheCold)

	_, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "cold", "recheckInterval": time.Minute}, "evm:1")
	assert.ErrorIs(t, err, ErrRemoteCacheCold)
}

func TestChainstackVendor_OwnershipAndErrors(t *testing.T) {
	v := CreateChainstackVendor()
	assert.Equal(t, "chainstack", v.Name())
	for ep, want := range map[string]bool{
		"chainstack://k":     true,
		"evm+chainstack://k": true,
		"https://nd-1.ethereum-mainnet.core.chainstack.com/abc": true,
		"https://rpc.ankr.com/eth":                              false,
	} {
		assert.Equal(t, want, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: ep}), ep)
	}

	err, details := vendorErr(t, v, "eth_call", 500, -32000, "boom", "0xbeef")
	assert.NoError(t, err, "chainstack defers to generic handling")
	assert.Equal(t, "0xbeef", details["data"])
	assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, nil, "raw", map[string]interface{}{}))
}
