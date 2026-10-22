package svm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestSvmCache_Set_EmitsSuccessMetrics guards the EVM-parity path: a successful
// connector.Set must increment erpc_cache_set_success_total and record
// erpc_cache_set_success_duration_seconds. Without these, Solana "Cache Set
// Latency (Success)" panels stay dead even when writes are healthy.
func TestSvmCache_Set_EmitsSuccessMetrics(t *testing.T) {
	t.Parallel()

	projectId := t.Name()
	conn := data.NewMockConnector("set-success-mem")
	conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()

	c, policies := newMissReasonCache(t, projectId, conn)
	require.Len(t, policies, 1)
	policy := policies[0]

	req := newMissReasonRequest(t)
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"blockhash": "abc"}, nil)
	require.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	labels := []string{
		projectId,
		missReasonNetworkLabel,
		missReasonMethod,
		conn.Id(),
		policy.String(),
		ttlString(policy.GetTTL()),
	}

	beforeTotal := promUtil.ToFloat64(telemetry.MetricCacheSetSuccessTotal.WithLabelValues(labels...))
	beforeDuration := histogramSampleCount(t, telemetry.MetricCacheSetSuccessDuration.WithLabelValues(labels...))

	require.NoError(t, c.Set(context.Background(), req, resp))

	afterTotal := promUtil.ToFloat64(telemetry.MetricCacheSetSuccessTotal.WithLabelValues(labels...))
	afterDuration := histogramSampleCount(t, telemetry.MetricCacheSetSuccessDuration.WithLabelValues(labels...))

	require.Equal(t, 1.0, afterTotal-beforeTotal,
		"erpc_cache_set_success_total must rise by 1 on a successful Set")
	require.Equal(t, uint64(1), afterDuration-beforeDuration,
		"erpc_cache_set_success_duration_seconds must record one observation")
	conn.AssertExpectations(t)
}

func histogramSampleCount(t *testing.T, obs prometheus.Observer) uint64 {
	t.Helper()
	m, ok := obs.(prometheus.Metric)
	require.True(t, ok, "observer must also be a prometheus.Metric")
	var pb dto.Metric
	require.NoError(t, m.Write(&pb))
	h := pb.GetHistogram()
	require.NotNil(t, h, "metric must be a histogram")
	return h.GetSampleCount()
}
