package erpc

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gaugeValue(t *testing.T, gv *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	g, err := gv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	return promUtil.ToFloat64(g)
}

func histogramCount(t *testing.T, hv *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	obs, err := hv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	m, ok := obs.(prometheus.Metric)
	require.True(t, ok)
	pbMetric := &dto.Metric{}
	require.NoError(t, m.Write(pbMetric))
	return pbMetric.GetHistogram().GetSampleCount()
}

func TestHttpServer_IngressTelemetry_InflightAndPreForward(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(string(body), "eth_getBalance")
		}).
		Times(1).
		Reply(200).
		Delay(200 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1",
		})

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
						Endpoint: "http://rpc1.localhost",
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

	beforePreForward := histogramCount(
		t,
		telemetry.MetricHTTPIngressPreForwardDuration,
		"proxy",
		"forwarded",
	)
	beforeInflight := gaugeValue(t, telemetry.MetricHTTPIngressInflight, "proxy", http.MethodPost)

	done := make(chan struct{})
	var (
		statusCode int
		body       string
	)
	go func() {
		defer close(done)
		statusCode, _, body = sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":1}`, nil, nil)
	}()

	require.Eventually(t, func() bool {
		return gaugeValue(t, telemetry.MetricHTTPIngressInflight, "proxy", http.MethodPost) >= beforeInflight+1
	}, 5*time.Second, 10*time.Millisecond)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("request did not complete")
	}

	assert.Equal(t, http.StatusOK, statusCode)
	assert.Contains(t, body, `"result":"0x1"`)

	require.Eventually(t, func() bool {
		return gaugeValue(t, telemetry.MetricHTTPIngressInflight, "proxy", http.MethodPost) <= beforeInflight
	}, 5*time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		afterPreForward := histogramCount(
			t,
			telemetry.MetricHTTPIngressPreForwardDuration,
			"proxy",
			"forwarded",
		)
		return afterPreForward-beforePreForward >= uint64(1)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestHttpServer_IngressTelemetry_PreForwardRejected(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

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
						Endpoint: "http://rpc1.localhost",
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

	beforeRejected := histogramCount(
		t,
		telemetry.MetricHTTPIngressPreForwardDuration,
		"proxy",
		"rejected",
	)

	statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","params":[],"id":1}`, nil, nil)
	assert.Equal(t, http.StatusBadRequest, statusCode)
	assert.Contains(t, body, "invalid request")

	require.Eventually(t, func() bool {
		afterRejected := histogramCount(
			t,
			telemetry.MetricHTTPIngressPreForwardDuration,
			"proxy",
			"rejected",
		)
		return afterRejected-beforeRejected >= uint64(1)
	}, 2*time.Second, 10*time.Millisecond)
}

func TestHttpServer_IngressTelemetry_PreForwardAdminHandled(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	cfg := &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
		},
		Admin: &common.AdminConfig{
			Auth: &common.AuthConfig{
				Strategies: []*common.AuthStrategyConfig{
					{
						Type: common.AuthTypeSecret,
						Secret: &common.SecretStrategyConfig{
							Id:    "admin",
							Value: "test-admin-secret",
						},
					},
				},
			},
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
						Endpoint: "http://rpc1.localhost",
						Evm: &common.EvmUpstreamConfig{
							ChainId: 123,
						},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	beforeAdminHandled := histogramCount(
		t,
		telemetry.MetricHTTPIngressPreForwardDuration,
		"admin",
		"admin_handled",
	)

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		baseURL+"/admin",
		strings.NewReader(`{"jsonrpc":"2.0","method":"erpc_config","params":[],"id":1}`),
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ERPC-Secret-Token", "test-admin-secret")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(bodyBytes), `"jsonrpc":"2.0"`)

	require.Eventually(t, func() bool {
		afterAdminHandled := histogramCount(
			t,
			telemetry.MetricHTTPIngressPreForwardDuration,
			"admin",
			"admin_handled",
		)
		return afterAdminHandled-beforeAdminHandled >= uint64(1)
	}, 2*time.Second, 10*time.Millisecond)
}
