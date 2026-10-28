package thirdparty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	archEvm "github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWillowVendor_Name(t *testing.T) {
	assert.Equal(t, "willow", CreateWillowVendor().Name())
}

// willowOfflineURL refuses connections instantly so the async refresh kicked
// off by SupportsNetwork never leaves the machine.
const willowOfflineURL = "http://127.0.0.1:1/willow-offline"

// swapWillowChainsURL temporarily overrides willowChainsURL so tests can point
// the vendor at a mock server or a deliberately broken URL.
// Returns the previous value so the caller can restore it.
func swapWillowChainsURL(t *testing.T, newURL string) string {
	t.Helper()
	prev := willowChainsURL
	willowChainsURL = newURL
	return prev
}

func TestWillowVendor_SupportsNetwork(t *testing.T) {
	offline := common.VendorSettings{"chainsUrl": willowOfflineURL}
	svtCheckSupports(t, CreateWillowVendor(), offline, []svtSupportCase{
		{"evm:1", true, false},
		{"evm:4663", true, false},
		{"evm:57073", true, false},
		{"evm:137", false, false},
		{"solana:mainnet", false, false},
		{"evm:abc", false, true},
	})

	assert.Equal(t, "https://api.willow.tech/v1/rpc/chains", willowChainsURL)
	assert.Equal(t, 24*time.Hour, DefaultWillowRecheckInterval)
	prev := swapWillowChainsURL(t, willowOfflineURL)
	defer swapWillowChainsURL(t, prev)
	ok, err := svtSupports(t, CreateWillowVendor(), common.VendorSettings{}, "evm:8453")
	require.NoError(t, err)
	assert.True(t, ok, "empty settings use willowChainsURL with the built-in fallback")
}

func TestWillowVendor_ColdStartFallback(t *testing.T) {
	v := CreateWillowVendor()
	settings := common.VendorSettings{"chainsUrl": willowOfflineURL, "recheckInterval": 24 * time.Hour}

	ok, err := svtSupports(t, v, settings, "evm:1")
	require.NoError(t, err, "cold-start fallback should not surface a fetch error")
	assert.True(t, ok, "chain 1 is in defaultWillowChains")

	ok, err = svtSupports(t, v, settings, "evm:999999999999")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestWillowVendor_FetchedListReplacesFallback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chains":[{"id":1,"name":"Ethereum"},{"id":424242,"name":"New Chain"}]}`))
	}))
	defer srv.Close()

	v := CreateWillowVendor().(*WillowVendor)
	settings := common.VendorSettings{"chainsUrl": srv.URL, "recheckInterval": 24 * time.Hour}

	// The first call answers from the built-in fallback, which lacks 424242, and starts the fetch.
	ok, err := svtSupports(t, v, settings, "evm:424242")
	require.NoError(t, err)
	assert.False(t, ok, "first call returns the built-in fallback")

	require.Eventually(t, func() bool {
		ok, err := svtSupports(t, v, settings, "evm:424242")
		return err == nil && ok
	}, 5*time.Second, 50*time.Millisecond, "a chain the fallback lacks appears after a fetch")

	// The fetched list replaces the fallback: a fallback chain it omits is no longer claimed.
	ok, err = svtSupports(t, v, settings, "evm:4663")
	require.NoError(t, err)
	assert.False(t, ok)
	waitRefreshesIdle(t, v.cache)
	assert.Equal(t, int32(1), hits.Load(), "a fresh list is not refetched")

	// recheckInterval makes a stale list refetch.
	stale := common.VendorSettings{"chainsUrl": srv.URL, "recheckInterval": time.Nanosecond}
	_, err = svtSupports(t, v, stale, "evm:1")
	require.NoError(t, err)
	waitRefreshesIdle(t, v.cache)
	assert.Equal(t, int32(2), hits.Load())
}

// syncLog collects log lines written from the async refresh goroutine.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A failed or unreadable fetch keeps the built-in fallback and is logged as a
// failed refresh, the same path drpc's failures take.
func TestWillowVendor_FetchErrorKeepsFallback(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"server error", http.StatusInternalServerError, `{"chains":[{"id":424242}]}`},
		{"malformed json", http.StatusOK, `{"chains":[`},
		{"html", http.StatusOK, `<html>maintenance</html>`},
		{"not an object", http.StatusOK, `[{"id":424242}]`},
		{"unknown shape", http.StatusOK, `{"data":[{"id":424242}]}`},
		{"empty list", http.StatusOK, `{"chains":[]}`},
		{"no readable entry", http.StatusOK, `{"chains":[{"id":"424242"},{"id":0}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := newJSONServer(t, c.status, c.body)
			v := CreateWillowVendor().(*WillowVendor)
			logs := &syncLog{}
			logger := zerolog.New(logs)
			settings := common.VendorSettings{"chainsUrl": srv.URL, "recheckInterval": time.Hour}
			supports := func(networkId string) bool {
				ok, err := v.SupportsNetwork(context.Background(), &logger, settings, networkId)
				require.NoError(t, err)
				waitRefreshesIdle(t, v.cache)
				return ok
			}
			assert.True(t, supports("evm:1"), "cold start answers from the built-in fallback")
			assert.True(t, supports("evm:1"), "failed refresh keeps serving the built-in fallback")
			assert.False(t, supports("evm:424242"))
			assert.Contains(t, logs.String(), `"level":"warn"`)
			assert.Contains(t, logs.String(), `"vendor":"willow"`)
			assert.Contains(t, logs.String(), "vendor remote-data refresh failed; keeping previous snapshot")
		})
	}
}

func TestWillowVendor_ChainsUrlSetting_InvalidURLReturnsError(t *testing.T) {
	for _, badURL := range []string{"not-a-url", "ftp://host", "://missing-scheme"} {
		_, err := svtSupports(t, CreateWillowVendor(), common.VendorSettings{"chainsUrl": badURL}, "evm:1")
		require.Errorf(t, err, "malformed chainsUrl %q should return an error", badURL)
		assert.Contains(t, err.Error(), "invalid chainsUrl")
	}
}

func TestWillowVendor_FetchWillowChains(t *testing.T) {
	v := CreateWillowVendor().(*WillowVendor)
	logger := zerolog.Nop()
	ctx := context.Background()

	t.Run("skips entries it cannot read", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{"chains":[
			{"id":1,"name":"Ethereum"},
			{"id":"8453","name":"string id"},
			{"id":0},
			{"id":-5},
			{"name":"no id"},
			{"id":1.5},
			"not an object",
			{"id":57073,"name":"Ink","extra":{"x":1}}
		],"other":true}`)
		got, err := v.fetchWillowChains(ctx, &logger, srv.URL)
		require.NoError(t, err)
		assert.Equal(t, map[int64]struct{}{1: {}, 57073: {}}, got)
	})

	t.Run("non-2xx status", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusServiceUnavailable, `{}`)
		_, err := v.fetchWillowChains(ctx, &logger, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-2xx code: 503")
	})

	t.Run("invalid json", func(t *testing.T) {
		srv := newJSONServer(t, http.StatusOK, `{not json`)
		_, err := v.fetchWillowChains(ctx, &logger, srv.URL)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse willow chains response")
	})

	t.Run("no readable entry", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"chains":null}`, `{"chains":[]}`, `{"chains":[{"id":"x"}]}`} {
			srv := newJSONServer(t, http.StatusOK, body)
			_, err := v.fetchWillowChains(ctx, &logger, srv.URL)
			require.Error(t, err, body)
			assert.Contains(t, err.Error(), "willow chains response lists no chains", body)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		_, err := v.fetchWillowChains(ctx, &logger, willowOfflineURL)
		assert.Error(t, err)
	})

	t.Run("malformed url", func(t *testing.T) {
		_, err := v.fetchWillowChains(ctx, &logger, "http://bad\nhost")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid control character")
	})
}

func TestWillowVendor_GenerateConfigs(t *testing.T) {
	key := common.VendorSettings{"apiKey": "KEY"}
	svtCheckGenErrors(t, CreateWillowVendor, []svtErrCase{
		{"missing apiKey", svtEvm(1), common.VendorSettings{}, "apiKey is required in willow settings"},
		{"nil evm", &common.UpstreamConfig{}, key, "willow vendor requires upstream.evm.chainId to be defined"},
		{"zero chain", svtEvm(0), key, "willow vendor requires upstream.evm.chainId to be defined"},
	})
	svtCheckURLs(t, CreateWillowVendor, key, map[int64]string{
		1:    "https://api.willow.tech/v1/rpc/1/KEY",
		8453: "https://api.willow.tech/v1/rpc/8453/KEY",
		// Not in defaultWillowChains: reachable through onlyNetworks, same URL shape.
		56: "https://api.willow.tech/v1/rpc/56/KEY",
	})
	svtCheckURLs(t, CreateWillowVendor, common.VendorSettings{"apiKey": "a/b"}, map[int64]string{
		1: "https://api.willow.tech/v1/rpc/1/a%2Fb",
	})
	svtCheckPreset(t, CreateWillowVendor(), "https://api.willow.tech/v1/rpc/1/own")
}

// Willow's error bodies (live responses where one could be produced, otherwise
// the server's messages for those cases, plus a stand-in -32603 for an
// unrecognized error) map to the right normalized errors with no
// vendor-specific handling.
func TestWillowVendor_ErrorsUseGenericNormalization(t *testing.T) {
	err, _ := vendorErr(t, CreateWillowVendor(), "eth_getLogs", 429, -32005, "rate limit exceeded", "")
	assert.NoError(t, err, "willow defers every error to generic handling")

	cases := []struct {
		name   string
		status int
		body   string
		want   common.ErrorCode
	}{
		{"unrecognized error falls through", 200,
			`{"error":{"code":-32603,"message":"internal error"},"id":1,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointServerSideException},
		{"too many logs", 200,
			`{"error":{"code":-32002,"message":"query returned more than 10000 results. Try with this block range [0x32bb644, 0x32bb64a]."},"id":1,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointRequestTooLarge},
		{"request time budget", 200,
			`{"error":{"code":-32002,"message":"range too large: answering this filter ran past this request's 2000 ms at block 26119000 (from 26100000); send the rest in another request. Try with this block range [0x18e4120, 0x18e8b58]."},"id":1,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointRequestTooLarge},
		{"block not held", 200,
			`{"error":{"code":-32000,"message":"not found in this store: block 4294967295 is beyond the indexed range 26133199-26290857"},"id":1,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointMissingData},
		{"transaction not held", 200,
			`{"error":{"code":-32000,"message":"not found in this store: transaction 0x1111111111111111111111111111111111111111111111111111111111111111 is not in the full-block tier"},"id":1,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointMissingData},
		{"history being fetched", 200,
			`{"error":{"code":-32000,"message":"not found in this store: blocks 30000000-30000099 are older than the blocks held here and are being fetched from history, verified against the chain and kept; try again in about 30 s (willow_historyStatus reports progress, and willow_historyCancel ends a fetch); this account's fetch is 128 blocks, about 10313 units","data":{"history":{"state":"fetching","chain":8453,"blocks":[[30000000,30000099]],"ranges":1,"fetches":[7],"retryAfterSeconds":30,"quote":{"blocks":128,"units":10313}}}},"id":1,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointMissingData},
		{"method not served", 200,
			`{"error":{"code":-32601,"message":"the method eth_newFilter does not exist/is not available"},"id":1,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointUnsupported},
		{"eth_call halted", 200,
			`{"error":{"code":-32000,"message":"out of gas"},"id":1,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointExecutionException},
		{"rate limited", 429,
			`{"error":{"code":-32005,"message":"rate limit exceeded"},"id":null,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointCapacityExceeded},
		{"balance exhausted (x402 body, error is a string)", 402,
			`{"jsonrpc":"2.0","id":1,"x402Version":1,"error":"payment required: this API key's balance is used up; top it up with an x402 payment for one of accepts (v1 in X-PAYMENT, v2 in PAYMENT-SIGNATURE) or in the dashboard","accepts":[]}`,
			common.ErrCodeEndpointBillingIssue},
		{"invalid key", 401,
			`{"error":{"code":-32000,"message":"invalid API key"},"id":null,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointUnauthorized},
		{"key not enabled for chain", 403,
			`{"error":{"code":-32000,"message":"this API key is not enabled for chain 8453"},"id":null,"jsonrpc":"2.0"}`,
			common.ErrCodeEndpointUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			jr := &common.JsonRpcResponse{}
			require.NoError(t, jr.ParseFromStream(nil, strings.NewReader(c.body), len(c.body)))
			err := archEvm.ExtractJsonRpcError(&http.Response{StatusCode: c.status, Header: http.Header{}}, nil, jr, nil)
			require.Error(t, err)
			assert.True(t, common.HasErrorCode(err, c.want), "want %s, got %v", c.want, err)
		})
	}
}

func TestWillowVendor_OwnsUpstream(t *testing.T) {
	svtAssertOwns(t, CreateWillowVendor(), map[string]bool{
		"willow://key":                         true,
		"evm+willow://key":                     true,
		"https://api.willow.tech/v1/rpc/1/key": true,
		"https://api.willow.technology/rpc":    false,
		"https://rpc.ankr.com/eth/key":         false,
	})

	v := NewVendorsRegistry().LookupByUpstream(&common.UpstreamConfig{Endpoint: "https://api.willow.tech/v1/rpc/1/key"})
	require.NotNil(t, v)
	assert.Equal(t, "willow", v.Name())
}
