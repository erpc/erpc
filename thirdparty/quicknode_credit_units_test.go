package thirdparty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	// Keep unit tests hermetic: the lifecycle refresh (SupportsNetwork /
	// GenerateConfigs) must never reach the real Admin API. Tests that
	// exercise fetching swap this per-test to a mock server.
	quicknodeApiCreditsBaseURL = "http://127.0.0.1:1/disabled-in-tests/"
}

func qnCuReq(t *testing.T, method string) *common.NormalizedRequest {
	t.Helper()
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`,
	))
}

func swapQuicknodeApiCreditsBaseURL(t *testing.T, newURL string) string {
	t.Helper()
	prev := quicknodeApiCreditsBaseURL
	quicknodeApiCreditsBaseURL = newURL
	return prev
}

// The Admin API fetch sends x-api-key + the slug path, parses the documented
// {data:[{method,credits}]} shape, and merges over the built-in table.
func TestFetchQuicknodeCreditUnits_MergesOverBuiltin(t *testing.T) {
	var gotPath, gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"method":"eth_call","credits":30},{"method":"eth_getLogs","credits":75}],"error":""}`))
	}))
	defer server.Close()
	orig := swapQuicknodeApiCreditsBaseURL(t, server.URL+"/v0/api-credits/")
	defer swapQuicknodeApiCreditsBaseURL(t, orig)

	table, err := fetchQuicknodeCreditUnits(context.Background(), "test-key", "ethereum")
	require.NoError(t, err)

	assert.Equal(t, "/v0/api-credits/ethereum", gotPath, "slug is the path param")
	assert.Equal(t, "test-key", gotKey, "account key sent as x-api-key")
	assert.Equal(t, int64(30), table["eth_call"], "fetched value wins over built-in 20")
	assert.Equal(t, int64(75), table["eth_getLogs"])
	assert.Equal(t, int64(20), table["*"], "built-in '*' fallback survives the merge")
	assert.Equal(t, int64(40), table["debug_traceTransaction"], "built-in-only method survives")
}

func TestFetchQuicknodeCreditUnits_Errors(t *testing.T) {
	orig := quicknodeApiCreditsBaseURL
	defer func() { quicknodeApiCreditsBaseURL = orig }()

	// Non-2xx → error.
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv500.Close()
	quicknodeApiCreditsBaseURL = srv500.URL + "/"
	_, err := fetchQuicknodeCreditUnits(context.Background(), "k", "ethereum")
	assert.Error(t, err, "non-2xx surfaces an error")

	// Body-level error field → error.
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"error":"invalid chain"}`))
	}))
	defer srvErr.Close()
	quicknodeApiCreditsBaseURL = srvErr.URL + "/"
	_, err = fetchQuicknodeCreditUnits(context.Background(), "k", "ethereum")
	assert.Error(t, err, "body error field surfaces an error")

	// Empty data → error (don't publish an empty table).
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"error":""}`))
	}))
	defer srvEmpty.Close()
	quicknodeApiCreditsBaseURL = srvEmpty.URL + "/"
	_, err = fetchQuicknodeCreditUnits(context.Background(), "k", "ethereum")
	assert.Error(t, err, "no methods surfaces an error")
}

// Before any fetch, CreditUnits uses the built-in table; operator overrides
// still win.
func TestQuicknodeCreditUnits_ColdStartFallback(t *testing.T) {
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	ups := &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{ChainId: 1}}

	assert.Equal(t, int64(20), v.CreditUnits(qnCuReq(t, "eth_call"), ups), "built-in base cost")
	assert.Equal(t, int64(40), v.CreditUnits(qnCuReq(t, "debug_traceTransaction"), ups), "built-in advanced-API cost")

	over := &common.UpstreamConfig{
		Evm:         &common.EvmUpstreamConfig{ChainId: 1},
		CreditUnits: map[string]int64{"eth_call": 7},
	}
	assert.Equal(t, int64(7), v.CreditUnits(qnCuReq(t, "eth_call"), over), "operator override wins")
}
