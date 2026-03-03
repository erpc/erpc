package erpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func TestForwardEthCallBatchCandidates(t *testing.T) {
	server := &HttpServer{serverCfg: &common.ServerConfig{IncludeErrorDetails: &common.TRUE}}
	startedAt := time.Now()

	makeCandidate := func(index int) ethCallBatchCandidate {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"}]}`))
		ctx := common.StartRequestSpan(context.Background(), req)
		return ethCallBatchCandidate{
			index:  index,
			ctx:    ctx,
			req:    req,
			logger: log.Logger,
		}
	}

	responses := make([]interface{}, 1)
	server.forwardEthCallBatchCandidates(&startedAt, nil, nil, []ethCallBatchCandidate{makeCandidate(0)}, responses, "test", multicallFallbackModeFull)
	require.NotNil(t, responses[0])

	origForward := forwardBatchProject
	t.Cleanup(func() {
		forwardBatchProject = origForward
	})

	t.Run("forward error", func(t *testing.T) {
		responses := make([]interface{}, 1)
		resp := common.NewNormalizedResponse()
		forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return resp, errors.New("boom")
		}

		server.forwardEthCallBatchCandidates(&startedAt, &PreparedProject{}, &Network{}, []ethCallBatchCandidate{makeCandidate(0)}, responses, "test", multicallFallbackModeFull)
		require.NotNil(t, responses[0])
	})

	t.Run("forward success", func(t *testing.T) {
		responses := make([]interface{}, 1)
		resp := common.NewNormalizedResponse()
		forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return resp, nil
		}

		server.forwardEthCallBatchCandidates(&startedAt, &PreparedProject{}, &Network{}, []ethCallBatchCandidate{makeCandidate(0)}, responses, "test", multicallFallbackModeFull)
		require.Equal(t, resp, responses[0])
	})

	t.Run("records batch fallback attempt reason", func(t *testing.T) {
		telemetry.MetricNetworkAttemptReasonTotal.Reset()

		responses := make([]interface{}, 2)
		resp := common.NewNormalizedResponse()
		forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return resp, nil
		}

		project := &PreparedProject{Config: &common.ProjectConfig{Id: "prjA"}}
		network := &Network{projectId: "prjA", networkId: "evm:1", networkLabel: "evm:1"}

		before := promUtil.ToFloat64(
			telemetry.MetricNetworkAttemptReasonTotal.WithLabelValues(
				"prjA",
				"evm:1",
				"eth_call",
				telemetry.AttemptReasonBatchFallback,
				telemetry.MetricsVariantLabel(),
				telemetry.MetricsReleaseLabel(),
			),
		)

		server.forwardEthCallBatchCandidates(
			&startedAt,
			project,
			network,
			[]ethCallBatchCandidate{makeCandidate(0), makeCandidate(1)},
			responses,
			"test_reason",
			multicallFallbackModeFull,
		)

		after := promUtil.ToFloat64(
			telemetry.MetricNetworkAttemptReasonTotal.WithLabelValues(
				"prjA",
				"evm:1",
				"eth_call",
				telemetry.AttemptReasonBatchFallback,
				telemetry.MetricsVariantLabel(),
				telemetry.MetricsReleaseLabel(),
			),
		)
		require.Equal(t, before+2, after)
	})

	t.Run("uses canonical network id for fallback metrics", func(t *testing.T) {
		telemetry.MetricMulticall3FallbackTotal.Reset()

		responses := make([]interface{}, 1)
		resp := common.NewNormalizedResponse()
		forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return resp, nil
		}

		project := &PreparedProject{Config: &common.ProjectConfig{Id: "prjB"}}
		network := &Network{projectId: "prjB", networkId: "evm:111", networkLabel: "alias-mainnet"}

		server.forwardEthCallBatchCandidates(
			&startedAt,
			project,
			network,
			[]ethCallBatchCandidate{makeCandidate(0)},
			responses,
			"reason_x",
			multicallFallbackModeFull,
		)

		idSeries := promUtil.ToFloat64(
			telemetry.MetricMulticall3FallbackTotal.WithLabelValues("prjB", "evm:111", "reason_x"),
		)
		labelSeries := promUtil.ToFloat64(
			telemetry.MetricMulticall3FallbackTotal.WithLabelValues("prjB", "alias-mainnet", "reason_x"),
		)
		require.Equal(t, float64(1), idSeries)
		require.Equal(t, float64(0), labelSeries)
	})

	t.Run("panic recovery in forward goroutine", func(t *testing.T) {
		responses := make([]interface{}, 2)
		var callCount int32
		forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			count := atomic.AddInt32(&callCount, 1)
			if count == 1 {
				panic("test panic in forward")
			}
			return common.NewNormalizedResponse(), nil
		}

		// Create 2 candidates - one will panic, one will succeed
		server.forwardEthCallBatchCandidates(&startedAt, &PreparedProject{}, &Network{}, []ethCallBatchCandidate{makeCandidate(0), makeCandidate(1)}, responses, "test", multicallFallbackModeFull)

		// Both responses should have been populated (panic recovered)
		require.NotNil(t, responses[0], "first response should not be nil after panic")
		require.NotNil(t, responses[1], "second response should not be nil")
	})

	t.Run("context cancellation is handled", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"}]}`))

		candidate := ethCallBatchCandidate{
			index:  0,
			ctx:    cancelledCtx,
			req:    req,
			logger: log.Logger,
		}

		responses := make([]interface{}, 1)
		forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, ctx.Err()
		}

		server.forwardEthCallBatchCandidates(&startedAt, &PreparedProject{}, &Network{}, []ethCallBatchCandidate{candidate}, responses, "test", multicallFallbackModeFull)
		require.NotNil(t, responses[0], "response should be populated even with cancelled context")
	})
}
