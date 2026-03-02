package erpc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHttpIngressRouteLabel(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{name: "admin path", method: http.MethodPost, path: "/admin", want: "admin"},
		{name: "root get is healthcheck", method: http.MethodGet, path: "/", want: "healthcheck"},
		{name: "root options is proxy", method: http.MethodOptions, path: "/", want: "proxy"},
		{name: "root post is proxy", method: http.MethodPost, path: "/", want: "proxy"},
		{name: "suffix healthcheck", method: http.MethodPost, path: "/project/evm/1/healthcheck", want: "healthcheck"},
		{name: "proxy request", method: http.MethodPost, path: "/project/evm/1", want: "proxy"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://localhost"+tc.path, nil)
			assert.Equal(t, tc.want, httpIngressRouteLabel(req))
		})
	}
}

func TestHttpServer_PreForwardMetrics_SingleVsMultiRequestPaths(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result := "0x1"
		if method, ok := req["method"].(string); ok && method == "eth_chainId" {
			// Keep upstream bootstrap consistent with configured chainId 123.
			result = "0x7b"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result":  result,
		})
	}))
	defer upstream.Close()

	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm: &common.EvmNetworkConfig{
							ChainId: 123,
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "rpc1",
						Type:     common.UpstreamTypeEvm,
						Endpoint: upstream.URL,
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	before := histogramCount(t, telemetry.MetricHTTPIngressPreForwardDuration, "proxy", "forwarded")

	statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`, nil, nil)
	require.Equal(t, http.StatusOK, statusCode)
	require.Contains(t, body, `"result":"0x7b"`)

	afterSingle := histogramCount(t, telemetry.MetricHTTPIngressPreForwardDuration, "proxy", "forwarded")
	require.GreaterOrEqual(t, afterSingle-before, uint64(1))

	statusCode, _, batchBody := sendRequest(`[
		{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":2},
		{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":3}
	]`, nil, nil)
	require.Equal(t, http.StatusOK, statusCode)
	var batchResp []map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(batchBody), &batchResp))
	require.Len(t, batchResp, 2)

	afterBatch := histogramCount(t, telemetry.MetricHTTPIngressPreForwardDuration, "proxy", "forwarded")
	require.GreaterOrEqual(t, afterBatch-afterSingle, uint64(2))
}
