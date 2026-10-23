package svm

import (
	"context"
	"errors"
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

// TestSvmCache_Set_EmitsOutcomeMetrics guards EVM parity on both Set outcomes:
// each must bump its counter and record one latency sample. Without these,
// Solana "Cache Set Latency" panels stay dead.
func TestSvmCache_Set_EmitsOutcomeMetrics(t *testing.T) {
	t.Parallel()

	setErr := errors.New("connector down")
	cases := []struct {
		name     string
		connErr  error
		total    func(labels []string) prometheus.Counter
		duration func(labels []string) prometheus.Observer
	}{
		{
			name: "success",
			total: func(l []string) prometheus.Counter {
				return telemetry.MetricCacheSetSuccessTotal.WithLabelValues(l...)
			},
			duration: func(l []string) prometheus.Observer {
				return telemetry.MetricCacheSetSuccessDuration.WithLabelValues(l...)
			},
		},
		{
			name:    "error",
			connErr: setErr,
			total: func(l []string) prometheus.Counter {
				return telemetry.MetricCacheSetErrorTotal.WithLabelValues(append(l, common.ErrorSummary(setErr))...)
			},
			duration: func(l []string) prometheus.Observer {
				return telemetry.MetricCacheSetErrorDuration.WithLabelValues(append(l, common.ErrorSummary(setErr))...)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			projectId := t.Name()
			conn := data.NewMockConnector("set-outcome-mem")
			conn.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(tc.connErr).Once()

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

			beforeTotal := promUtil.ToFloat64(tc.total(labels))
			beforeDuration := histogramSampleCount(t, tc.duration(labels))

			// Set swallows connector errors so the upstream request path never fails.
			require.NoError(t, c.Set(context.Background(), req, resp))

			require.Equal(t, 1.0, promUtil.ToFloat64(tc.total(labels))-beforeTotal)
			require.Equal(t, uint64(1), histogramSampleCount(t, tc.duration(labels))-beforeDuration)
			conn.AssertExpectations(t)
		})
	}
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
