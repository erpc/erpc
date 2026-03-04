package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type adaptiveHedgeTestQuantiles struct {
	sampleCount uint64
	p50         time.Duration
	p95         time.Duration
}

func (q *adaptiveHedgeTestQuantiles) Add(value float64) {}

func (q *adaptiveHedgeTestQuantiles) GetQuantile(qtile float64) time.Duration {
	switch qtile {
	case 0.50:
		return q.p50
	case 0.95:
		return q.p95
	default:
		return 0
	}
}

func (q *adaptiveHedgeTestQuantiles) GetSampleCount() uint64 {
	return q.sampleCount
}

func (q *adaptiveHedgeTestQuantiles) Reset() {}

type adaptiveHedgeTestMetrics struct {
	qt common.QuantileTracker
}

func (m *adaptiveHedgeTestMetrics) ErrorRate() float64 {
	return 0
}

func (m *adaptiveHedgeTestMetrics) GetResponseQuantiles() common.QuantileTracker {
	return m.qt
}

type adaptiveHedgeTestNetwork struct {
	methodMetrics common.TrackedMetrics
	finality      common.DataFinalityState
}

func (n *adaptiveHedgeTestNetwork) Id() string {
	return "evm:123"
}

func (n *adaptiveHedgeTestNetwork) Label() string {
	return "evm:123"
}

func (n *adaptiveHedgeTestNetwork) ProjectId() string {
	return "test"
}

func (n *adaptiveHedgeTestNetwork) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}

func (n *adaptiveHedgeTestNetwork) Config() *common.NetworkConfig {
	return &common.NetworkConfig{Architecture: common.ArchitectureEvm}
}

func (n *adaptiveHedgeTestNetwork) Logger() *zerolog.Logger {
	l := zerolog.Nop()
	return &l
}

func (n *adaptiveHedgeTestNetwork) GetMethodMetrics(method string) common.TrackedMetrics {
	return n.methodMetrics
}

func (n *adaptiveHedgeTestNetwork) Forward(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}

func (n *adaptiveHedgeTestNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return n.finality
}

func (n *adaptiveHedgeTestNetwork) Cache() common.CacheDAL {
	return nil
}

func (n *adaptiveHedgeTestNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	return 0
}

func (n *adaptiveHedgeTestNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	return 0
}

func (n *adaptiveHedgeTestNetwork) EvmLeaderUpstream(ctx context.Context) common.Upstream {
	return nil
}

func TestShouldSuppressAdaptiveHedge_RequiresWarmupSamples(t *testing.T) {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1","latest"]}`))
	req.SetNetwork(&adaptiveHedgeTestNetwork{
		methodMetrics: &adaptiveHedgeTestMetrics{
			qt: &adaptiveHedgeTestQuantiles{
				sampleCount: adaptiveHedgeMinSampleCount - 1,
				p50:         40 * time.Millisecond,
				p95:         50 * time.Millisecond,
			},
		},
		finality: common.DataFinalityStateFinalized,
	})

	suppress, reason := shouldSuppressAdaptiveHedge(context.Background(), req, "eth_getBalance")
	require.False(t, suppress)
	require.Equal(t, "insufficient_samples", reason)
}

func TestShouldSuppressAdaptiveHedge_StableAfterWarmup(t *testing.T) {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1","latest"]}`))
	req.SetNetwork(&adaptiveHedgeTestNetwork{
		methodMetrics: &adaptiveHedgeTestMetrics{
			qt: &adaptiveHedgeTestQuantiles{
				sampleCount: adaptiveHedgeMinSampleCount,
				p50:         40 * time.Millisecond,
				p95:         50 * time.Millisecond,
			},
		},
		finality: common.DataFinalityStateFinalized,
	})

	suppress, reason := shouldSuppressAdaptiveHedge(context.Background(), req, "eth_getBalance")
	require.True(t, suppress)
	require.Equal(t, "stable_latency", reason)
}
