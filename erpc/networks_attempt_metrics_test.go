package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func histogramCountAndSum(t *testing.T, labels ...string) (uint64, float64) {
	t.Helper()
	obs, err := telemetry.MetricNetworkUpstreamCallsPerRequest.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	metric, ok := obs.(interface{ Write(*dto.Metric) error })
	require.True(t, ok)
	pbMetric := &dto.Metric{}
	require.NoError(t, metric.Write(pbMetric))
	h := pbMetric.GetHistogram()
	return h.GetSampleCount(), h.GetSampleSum()
}

func TestNetworkForward_AttemptReasonRetryAndUpstreamCallsMetric(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	telemetry.MetricNetworkAttemptReasonTotal.Reset()
	telemetry.MetricNetworkUpstreamCallsPerRequest.Reset()
	telemetry.ResetHandleCache()

	// First call fails, second succeeds. With one upstream this forces a retry round.
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Times(1).
		Reply(503).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    -32603,
				"message": "temporary upstream failure",
			},
		})

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x7b",
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkSimple(t, ctx,
		&common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
			Failsafe: []*common.FailsafeConfig{{
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 2,
					Delay:       common.Duration(1 * time.Millisecond),
				},
			}},
		},
	)

	method := "eth_getBalance"
	labels := []string{
		"test",
		network.Label(),
		method,
		telemetry.AttemptReasonRetry,
		telemetry.MetricsVariantLabel(),
		telemetry.MetricsReleaseLabel(),
	}
	beforeRetry := promUtil.ToFloat64(telemetry.MetricNetworkAttemptReasonTotal.WithLabelValues(labels...))

	hLabels := []string{
		"test",
		network.Label(),
		method,
		telemetry.MetricsVariantLabel(),
		telemetry.MetricsReleaseLabel(),
	}
	beforeCount, beforeSum := histogramCountAndSum(t, hLabels...)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	afterRetry := promUtil.ToFloat64(telemetry.MetricNetworkAttemptReasonTotal.WithLabelValues(labels...))
	require.Equal(t, beforeRetry+1, afterRetry)

	afterCount, afterSum := histogramCountAndSum(t, hLabels...)
	require.Equal(t, beforeCount+1, afterCount)
	require.Equal(t, beforeSum+2, afterSum)
}

func TestNetworkForward_UpstreamCallsMetric_SelectionPolicySkipDoesNotCountAsCall(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	telemetry.MetricNetworkUpstreamCallsPerRequest.Reset()
	telemetry.ResetHandleCache()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkSimple(t, ctx,
		&common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				ChainId: 123,
			},
			SelectionPolicy: &common.SelectionPolicyConfig{
				EvalInterval: common.Duration(20 * time.Millisecond),
				Rules: []*common.SelectionPolicyRuleConfig{
					{
						MatchUpstreamID: "*",
						Action:          common.SelectionPolicyRuleActionExclude,
					},
				},
			},
		},
	)

	method := "eth_getBalance"
	require.Eventually(t, func() bool {
		if network.selectionPolicyEvaluator == nil {
			return false
		}
		upstreams := network.upstreamsRegistry.GetNetworkUpstreams(ctx, network.networkId)
		if len(upstreams) == 0 {
			return false
		}
		return network.selectionPolicyEvaluator.AcquirePermit(network.logger, upstreams[0], method) != nil
	}, time.Second, 20*time.Millisecond, "selection policy exclusion should become active")

	hLabels := []string{
		"test",
		network.Label(),
		method,
		telemetry.MetricsVariantLabel(),
		telemetry.MetricsReleaseLabel(),
	}
	beforeCount, beforeSum := histogramCountAndSum(t, hLabels...)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","latest"]}`))
	resp, err := network.Forward(ctx, req)
	require.Error(t, err)
	require.Nil(t, resp)

	afterCount, afterSum := histogramCountAndSum(t, hLabels...)
	require.Equal(t, beforeCount+1, afterCount)
	require.Equal(t, beforeSum, afterSum)
}
