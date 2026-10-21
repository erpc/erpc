package thirdparty

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// primeRemoteCache publishes val under key through the cache's real async
// refresh path and waits until a lookup sees it as fresh.
func primeRemoteCache[T any](t *testing.T, c *RemoteDataCache[T], key string, val T) {
	t.Helper()
	logger := zerolog.Nop()
	c.TriggerAsyncRefresh(&logger, key, func(ctx context.Context) (T, error) {
		return val, nil
	})
	require.Eventually(t, func() bool {
		_, fresh := c.Lookup(key, time.Hour)
		return fresh
	}, 5*time.Second, 10*time.Millisecond)
}

// waitRefreshesIdle blocks until no async refresh is running on c, so tests
// can restore package globals that the refresh goroutines read.
func waitRefreshesIdle[T any](t *testing.T, c *RemoteDataCache[T]) {
	t.Helper()
	require.Eventually(t, func() bool {
		c.refreshMu.Lock()
		defer c.refreshMu.Unlock()
		return len(c.inflight) == 0
	}, 10*time.Second, 10*time.Millisecond)
}

// newJSONServer serves body with the given status code for every request.
func newJSONServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newChainIdServer answers every JSON-RPC call with the given eth_chainId result.
func newChainIdServer(t *testing.T, hexChainId string) *httptest.Server {
	t.Helper()
	return newJSONServer(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":"`+hexChainId+`"}`)
}

// vendorErr runs v's error normalization on a JSON-RPC error response for method.
func vendorErr(t *testing.T, v common.Vendor, method string, status int, code int, msg string, data string) (error, map[string]interface{}) {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, data))
	require.NoError(t, err)
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
	details := map[string]interface{}{}
	return v.GetVendorSpecificErrorIfAny(req, &http.Response{StatusCode: status}, jrr, details), details
}

// normalizedCode returns the JSON-RPC normalized code carried by a vendor error.
func normalizedCode(t *testing.T, err error) common.JsonRpcErrorNumber {
	t.Helper()
	se, ok := err.(common.StandardError)
	require.True(t, ok, "expected a StandardError, got %T", err)
	code, _ := se.DeepSearch("normalizedCode").(common.JsonRpcErrorNumber)
	return code
}
