package erpc

import (
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
		return gaugeValue(t, telemetry.MetricHTTPIngressInflight, "proxy", http.MethodPost) >= 1
	}, 2*time.Second, 10*time.Millisecond)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete")
	}

	assert.Equal(t, http.StatusOK, statusCode)
	assert.Contains(t, body, `"result":"0x1"`)

	require.Eventually(t, func() bool {
		return gaugeValue(t, telemetry.MetricHTTPIngressInflight, "proxy", http.MethodPost) == 0
	}, 2*time.Second, 10*time.Millisecond)

	afterPreForward := histogramCount(
		t,
		telemetry.MetricHTTPIngressPreForwardDuration,
		"proxy",
		"forwarded",
	)
	assert.GreaterOrEqual(t, afterPreForward-beforePreForward, uint64(1))
}
