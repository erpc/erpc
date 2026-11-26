package thirdparty

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newChainIdRouter answers eth_chainId per request path ("/<name>" → result).
// Paths mapped to "" get a JSON-RPC error; "!" gets an undecodable body.
func newChainIdRouter(t *testing.T, results map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "eth_chainId") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		res, ok := results[r.URL.Path]
		w.Header().Set("Content-Type", "application/json")
		switch {
		case !ok || res == "":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`))
		case res == "!":
			_, _ = w.Write([]byte(`<html>`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + res + `"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestQuicknodeVendor_FetchEndpoints(t *testing.T) {
	ctx := context.Background()

	t.Run("paginates, forwards filters and drops endpoints without http url", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		page1 := make([]map[string]interface{}, 0, 100)
		for i := range 99 {
			page1 = append(page1, map[string]interface{}{"id": "e" + strconv.Itoa(i), "http_url": "https://e" + strconv.Itoa(i) + ".quiknode.pro/k/", "chain": "eth"})
		}
		page1 = append(page1, map[string]interface{}{"id": "nohttp", "http_url": ""})

		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			MatchHeader("x-api-key", "qn-key").
			MatchParam("offset", "^0$").
			MatchParam("limit", "^100$").
			MatchParam("tag_ids", "^1,2$").
			MatchParam("tag_labels", "^prod,eu$").
			Reply(200).
			JSON(map[string]interface{}{"data": page1})
		gock.New("https://api.quicknode.com").
			Get("/v0/endpoints").
			MatchParam("offset", "^100$").
			Reply(200).
			JSON(map[string]interface{}{"data": []map[string]interface{}{{"id": "last", "http_url": "https://last.quiknode.pro/k/", "chain": "matic"}}})

		v := CreateQuicknodeVendor().(*QuicknodeVendor)
		got, err := v.fetchEndpoints(ctx, "qn-key", &QuicknodeFilterParams{TagIDs: []int{1, 2}, TagLabels: []string{"prod", "eu"}})
		require.NoError(t, err)
		require.Len(t, got, 100)
		assert.Equal(t, "e0", got[0].ID)
		assert.Equal(t, "eth", got[0].Chain)
		assert.Equal(t, "last", got[99].ID)
		assert.Equal(t, "matic", got[99].Chain)
		assert.True(t, gock.IsDone(), "both pages requested")
	})

	errCases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"non-200 status", http.StatusUnauthorized, `{"error":"bad key"}`, "status 401"},
		{"body error field", http.StatusOK, `{"data":[],"error":"forbidden"}`, "quicknode API error: forbidden"},
		{"undecodable body", http.StatusOK, `not-json`, "failed to decode QuickNode endpoints response"},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			gock.New("https://api.quicknode.com").Get("/v0/endpoints").Reply(c.status).BodyString(c.body)

			v := CreateQuicknodeVendor().(*QuicknodeVendor)
			_, err := v.fetchEndpoints(ctx, "k", nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}

	t.Run("transport failure is an error", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.New("https://api.quicknode.com").Get("/v0/endpoints").ReplyError(io.ErrUnexpectedEOF)

		v := CreateQuicknodeVendor().(*QuicknodeVendor)
		_, err := v.fetchEndpoints(ctx, "k", &QuicknodeFilterParams{})
		require.Error(t, err)
	})
}

func TestQuicknodeVendor_FetchChainIDs(t *testing.T) {
	logger := zerolog.Nop()
	srv := newChainIdRouter(t, map[string]string{"/eth": "0x1", "/base": "0x2105", "/bad": "0xzz", "/html": "!"})

	endpoints := []*QuicknodeEndpoint{
		{ID: "eth", HttpUrl: srv.URL + "/eth"},
		{ID: "base", HttpUrl: srv.URL + "/base"},
		{ID: "rpcerr", HttpUrl: srv.URL + "/missing"},
		{ID: "badhex", HttpUrl: srv.URL + "/bad"},
		{ID: "html", HttpUrl: srv.URL + "/html"},
		{ID: "down", HttpUrl: "http://127.0.0.1:1/"},
		{ID: "badurl", HttpUrl: "http://bad host/"},
		{ID: "skip"},
	}
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	require.NoError(t, v.fetchChainIDs(context.Background(), &logger, endpoints), "per-endpoint failures are tolerated")

	got := map[string]int64{}
	for _, e := range endpoints {
		got[e.ID] = e.ChainID
	}
	assert.Equal(t, map[string]int64{
		"eth": 1, "base": 8453, "rpcerr": 0, "badhex": 0, "html": 0, "down": 0, "badurl": 0, "skip": 0,
	}, got)

	t.Run("cancelled context fails every acquisition", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		eps := []*QuicknodeEndpoint{{ID: "eth", HttpUrl: srv.URL + "/eth"}}
		require.NoError(t, v.fetchChainIDs(ctx, &logger, eps))
		assert.Zero(t, eps[0].ChainID)
	})
}

// End-to-end discovery: the Admin API lists endpoints, each is probed for its
// chain id, and the snapshot then drives SupportsNetwork/GenerateConfigs.
func TestQuicknodeVendor_DiscoveryLifecycle(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	ctx := context.Background()
	logger := zerolog.Nop()
	probe := newChainIdRouter(t, map[string]string{"/eth-a": "0x1", "/eth-b": "0x1", "/base": "0x2105", "/broken": ""})

	gock.New("https://api.quicknode.com").
		Get("/v0/endpoints").
		MatchHeader("x-api-key", "life-key").
		Persist().
		Reply(200).
		JSON(map[string]interface{}{"data": []map[string]interface{}{
			{"id": "a", "http_url": probe.URL + "/eth-a", "chain": "eth"},
			{"id": "b", "http_url": probe.URL + "/eth-b", "chain": "eth"},
			{"id": "c", "http_url": probe.URL + "/base", "chain": "base"},
			{"id": "d", "http_url": probe.URL + "/broken", "chain": "broken"},
		}})

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	settings := common.VendorSettings{"apiKey": "life-key", "recheckInterval": time.Hour}

	ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
	assert.ErrorIs(t, err, ErrRemoteCacheCold, "first call is a cold start")
	assert.False(t, ok)

	require.Eventually(t, func() bool {
		ok, err := v.SupportsNetwork(ctx, &logger, settings, "evm:1")
		return err == nil && ok
	}, 5*time.Second, 10*time.Millisecond)

	ok, err = v.SupportsNetwork(ctx, &logger, settings, "evm:10")
	require.NoError(t, err)
	assert.False(t, ok, "no endpoint probed as chain 10")

	assert.Equal(t, "eth", v.chainSlug("life-key", 1))
	assert.Equal(t, "base", v.chainSlug("life-key", 8453))
	assert.Equal(t, "", v.chainSlug("life-key", 10))
	assert.Equal(t, "", v.chainSlug("other-key", 1))

	t.Run("generates one upstream per matching endpoint", func(t *testing.T) {
		got, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Id: "qn", Evm: &common.EvmUpstreamConfig{ChainId: 1}}, settings)
		require.NoError(t, err)
		require.Len(t, got, 2)
		ids := []string{got[0].Id, got[1].Id}
		assert.ElementsMatch(t, []string{"qn-a", "qn-b"}, ids)
		endpoints := []string{got[0].Endpoint, got[1].Endpoint}
		assert.ElementsMatch(t, []string{probe.URL + "/eth-a", probe.URL + "/eth-b"}, endpoints)
		for _, c := range got {
			assert.Equal(t, common.UpstreamTypeEvm, c.Type)
		}

		got, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}}, settings)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "quicknode-8453-c", got[0].Id)

		got, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 10}}, settings)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("credit-unit table refresh uses the discovered chain slug", func(t *testing.T) {
		gotPath := make(chan string, 1)
		cuSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case gotPath <- r.URL.Path:
			default:
			}
			_, _ = w.Write([]byte(`{"data":[{"method":"eth_call","credits":55}],"error":null}`))
		}))
		defer cuSrv.Close()
		// Earlier SupportsNetwork calls may still be refreshing other chains'
		// tables against the disabled URL; let them finish before swapping.
		waitRefreshesIdle(t, v.cuCache)
		orig := swapQuicknodeApiCreditsBaseURL(t, cuSrv.URL+"/v0/api-credits/")
		defer func() {
			waitRefreshesIdle(t, v.cuCache)
			swapQuicknodeApiCreditsBaseURL(t, orig)
		}()

		ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 8453}}
		_, err := v.SupportsNetwork(ctx, &logger, settings, "evm:8453")
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return v.CreditUnits(qnCuReq(t, "eth_call"), ups) == 55
		}, 5*time.Second, 10*time.Millisecond)
		assert.Equal(t, "/v0/api-credits/base", <-gotPath)
		assert.Equal(t, int64(20), v.CreditUnits(qnCuReq(t, "eth_call"), &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}), "other chains keep the built-in table")
	})
}

func TestQuicknodeVendor_RefreshCreditUnitsAsync_Guards(t *testing.T) {
	logger := zerolog.Nop()

	// Nil receiver and missing inputs are no-ops.
	var nilVendor *QuicknodeVendor
	nilVendor.refreshCreditUnitsAsync(&logger, "k", 1)

	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	v.refreshCreditUnitsAsync(&logger, "", 1)
	v.refreshCreditUnitsAsync(&logger, "k", 0)
	// No endpoints discovered yet: slug unknown, no refresh is started.
	v.refreshCreditUnitsAsync(&logger, "k", 1)
	assert.False(t, v.cuCache.Has("1"))

	// A fresh table short-circuits before any slug lookup.
	primeRemoteCache(t, v.cuCache, "1", map[string]int64{"*": 99})
	v.refreshCreditUnitsAsync(&logger, "k", 1)
	assert.Equal(t, int64(99), v.CreditUnits(qnCuReq(t, "eth_getBalance"), &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}))
	assert.Equal(t, int64(20), v.CreditUnits(qnCuReq(t, "eth_getBalance"), nil), "nil upstream uses built-in table")
}

func TestQuicknodeVendor_SupportsNetworkAndGenerateConfigs_Inputs(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("SupportsNetwork input handling", func(t *testing.T) {
		v := CreateQuicknodeVendor()
		ok, err := v.SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k"}, "solana:mainnet")
		assert.NoError(t, err)
		assert.False(t, ok)

		_, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{"apiKey": "k"}, "evm:one")
		assert.Error(t, err)

		ok, err = v.SupportsNetwork(ctx, &logger, common.VendorSettings{}, "evm:1")
		assert.NoError(t, err)
		assert.False(t, ok, "no apiKey means not supported")
	})

	t.Run("GenerateConfigs validation", func(t *testing.T) {
		v := CreateQuicknodeVendor()
		_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, common.VendorSettings{})
		assert.ErrorContains(t, err, "apiKey is required")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{}, common.VendorSettings{"apiKey": "k"})
		assert.ErrorContains(t, err, "requires upstream.evm to be defined")

		_, err = v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}, common.VendorSettings{"apiKey": "k"})
		assert.ErrorContains(t, err, "requires upstream.evm.chainId")
	})

	t.Run("GenerateConfigs cold cache is retryable", func(t *testing.T) {
		v := CreateQuicknodeVendor().(*QuicknodeVendor)
		// Mark discovery as in flight so the test never dials the Admin API.
		v.cache.inflight["cold-key"] = struct{}{}
		_, err := v.GenerateConfigs(ctx, &logger, &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}, common.VendorSettings{"apiKey": "cold-key", "recheckInterval": time.Minute})
		assert.ErrorIs(t, err, ErrRemoteCacheCold)
	})

	t.Run("GenerateConfigs keeps a preset endpoint", func(t *testing.T) {
		v := CreateQuicknodeVendor()
		ups := &common.UpstreamConfig{Endpoint: "https://x.quiknode.pro/abc/"}
		got, err := v.GenerateConfigs(ctx, &logger, ups, nil)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Same(t, ups, got[0])
		assert.NotNil(t, got[0].JsonRpc)
	})
}

func TestQuicknodeFilterParams_SingleValues(t *testing.T) {
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	got := v.extractFilterParams(common.VendorSettings{"tagIds": 7, "tagLabels": "prod"})
	assert.Equal(t, []int{7}, got.TagIDs)
	assert.Equal(t, []string{"prod"}, got.TagLabels)

	got = v.extractFilterParams(common.VendorSettings{"tagIds": []interface{}{1, "x"}, "tagLabels": []interface{}{"a", 2}})
	assert.Equal(t, []int{1}, got.TagIDs, "non-int entries are dropped")
	assert.Equal(t, []string{"a"}, got.TagLabels, "non-string entries are dropped")
}

func TestQuicknodeVendor_GetVendorSpecificErrorIfAny(t *testing.T) {
	v := CreateQuicknodeVendor()

	cases := []struct {
		name           string
		method         string
		code           int
		msg            string
		wantCode       common.ErrorCode
		wantNormalized common.JsonRpcErrorNumber
		networkRetry   bool
	}{
		{"-32614 range too large", "eth_call", -32614, "too large", common.ErrCodeEndpointRequestTooLarge, common.JsonRpcErrorEvmLargeRange, true},
		{"getLogs limited to", "eth_getLogs", -32000, "eth_getLogs is limited to a 10000 range", common.ErrCodeEndpointRequestTooLarge, common.JsonRpcErrorEvmLargeRange, true},
		{"-32009 capacity", "eth_call", -32009, "slow down", common.ErrCodeEndpointCapacityExceeded, common.JsonRpcErrorCapacityExceeded, true},
		{"-32007 capacity", "eth_call", -32007, "rate limited", common.ErrCodeEndpointCapacityExceeded, common.JsonRpcErrorCapacityExceeded, true},
		{"-32612 unsupported", "eth_call", -32612, "method not allowed", common.ErrCodeEndpointUnsupported, common.JsonRpcErrorCapacityExceeded, true},
		{"-32613 unsupported", "eth_call", -32613, "not on plan", common.ErrCodeEndpointUnsupported, common.JsonRpcErrorCapacityExceeded, true},
		{"parse failure", "eth_call", -32000, "failed to parse request", common.ErrCodeEndpointClientSideException, common.JsonRpcErrorParseException, false},
		{"-32010 gas limit", "eth_estimateGas", -32010, "transaction cost exceeds gas limit", common.ErrCodeEndpointClientSideException, common.JsonRpcErrorClientSideException, true},
		{"-32602 bad hex", "eth_getBalance", -32602, "cannot unmarshal hex string without 0x", common.ErrCodeEndpointClientSideException, common.JsonRpcErrorInvalidArgument, false},
		{"unauthorized", "eth_call", -32001, "UNAUTHORIZED: invalid token", common.ErrCodeEndpointUnauthorized, common.JsonRpcErrorUnauthorized, true},
		{"code 3 revert", "eth_call", 3, "execution reverted", common.ErrCodeEndpointExecutionException, common.JsonRpcErrorEvmReverted, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err, _ := vendorErr(t, v, c.method, 200, c.code, c.msg, "0xdead")
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, c.wantCode), "got %v", err)
			assert.Equal(t, c.wantNormalized, normalizedCode(t, err))
			assert.Equal(t, c.networkRetry, common.IsRetryableTowardNetwork(err))
			se := err.(common.StandardError)
			assert.Equal(t, "0xdead", se.DeepSearch("data"), "data travels with the error")
		})
	}

	t.Run("unmatched errors and non json-rpc bodies fall through", func(t *testing.T) {
		err, _ := vendorErr(t, v, "eth_call", 200, -32602, "invalid params", "")
		assert.NoError(t, err)
		err, _ = vendorErr(t, v, "eth_call", 200, -32000, "header not found", "")
		assert.NoError(t, err)
		err, _ = vendorErr(t, v, "eth_call", 200, -32000, "limited to 100", "")
		assert.NoError(t, err, "'limited to' only maps for eth_getLogs")
		assert.NoError(t, v.GetVendorSpecificErrorIfAny(nil, nil, map[string]string{}, map[string]interface{}{}))
	})
}

func TestQuicknodeVendor_OwnsUpstream(t *testing.T) {
	v := CreateQuicknodeVendor()
	assert.Equal(t, "quicknode", v.Name())
	for ep, want := range map[string]bool{
		"quicknode://key":                      true,
		"evm+quicknode://key":                  true,
		"https://foo-bar.quiknode.pro/abc123/": true,
		"https://rpc.ankr.com/eth":             false,
	} {
		assert.Equal(t, want, v.OwnsUpstream(&common.UpstreamConfig{Endpoint: ep}), ep)
	}
}
