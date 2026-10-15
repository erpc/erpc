package svm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// blockTimeNetwork reports realtime finality and a fixed EMA slot time for
// ResolveTTL tests (no real tracker warm-up).
type blockTimeNetwork struct {
	finality  common.DataFinalityState
	blockTime time.Duration
}

func (n *blockTimeNetwork) Id() string                               { return "svm:test" }
func (n *blockTimeNetwork) Label() string                            { return "" }
func (n *blockTimeNetwork) ProjectId() string                        { return "test" }
func (n *blockTimeNetwork) Architecture() common.NetworkArchitecture { return common.ArchitectureSvm }
func (n *blockTimeNetwork) Config() *common.NetworkConfig {
	return &common.NetworkConfig{Architecture: common.ArchitectureSvm}
}
func (n *blockTimeNetwork) Logger() *zerolog.Logger {
	l := log.Logger
	return &l
}
func (n *blockTimeNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (n *blockTimeNetwork) SvmHighestLatestSlot(context.Context) int64    { return 0 }
func (n *blockTimeNetwork) SvmHighestFinalizedSlot(context.Context) int64 { return 0 }
func (n *blockTimeNetwork) SvmHighestFinalizedSlotMax(context.Context) int64 {
	return 0
}
func (n *blockTimeNetwork) SvmHighestIndexedSlot(context.Context) int64 { return 0 }
func (n *blockTimeNetwork) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (n *blockTimeNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return n.finality
}
func (n *blockTimeNetwork) EvmBlockTime() time.Duration { return n.blockTime }

// realtimeReqBT builds a realtime getAccountInfo request whose network reports
// blockTime as its estimated (EMA) slot time. getBalance is hard never-cache;
// getAccountInfo is the canonical moving-head cacheable read. blockTime == 0
// mimics a cold EMA.
func realtimeReqBT(blockTime time.Duration) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["11111111111111111111111111111111"]}`,
	))
	req.SetNetwork(&blockTimeNetwork{
		finality:  common.DataFinalityStateRealtime,
		blockTime: blockTime,
	})
	return req
}

func btPolicy(t *testing.T, conn data.Connector, staticTTL time.Duration, mult float64) *data.CachePolicy {
	t.Helper()
	cfg := &common.CachePolicyConfig{
		Connector: "conn",
		Network:   "*",
		Method:    "*",
		Finality:  common.DataFinalityStateRealtime,
	}
	if staticTTL > 0 || mult > 0 {
		cfg.TTL = &common.BlockTimeAdaptiveDuration{Fallback: common.Duration(staticTTL), BlockTimeMultiplier: mult}
	}
	policy, err := data.NewCachePolicy(cfg, conn)
	require.NoError(t, err)
	return policy
}

type ttlCapturingConnector struct {
	*data.MockConnector
	ttl *time.Duration
}

func plainConnector(id string) *data.MockConnector {
	return data.NewMockConnector(id)
}

func setCapturingConnector(t *testing.T, id string) *ttlCapturingConnector {
	t.Helper()
	c := &ttlCapturingConnector{MockConnector: plainConnector(id)}
	c.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			if ttl, ok := args.Get(4).(*time.Duration); ok {
				c.ttl = ttl
			}
		}).
		Return(nil)
	return c
}

func accountInfoResp(t *testing.T, req *common.NormalizedRequest) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{
		"context": map[string]interface{}{"slot": float64(100)},
		"value": map[string]interface{}{
			"lamports":   float64(1),
			"owner":      "11111111111111111111111111111111",
			"data":       []interface{}{"", "base64"},
			"executable": false,
		},
	}, nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

// TestSvmJsonRpcCache_BlockTimeDerivedTTL mirrors the EVM Set-path coverage for
// blockTimeMultiplier: storage TTL must track the slot-time EMA when known and
// fall back when cold. SVM has no age guard, so only the write path is exercised.
func TestSvmJsonRpcCache_BlockTimeDerivedTTL(t *testing.T) {
	ctx := context.Background()
	logger := log.Logger

	const (
		slot400ms = 400 * time.Millisecond
		slot200ms = 200 * time.Millisecond
	)

	t.Run("SetStoresWithSlotTimeDerivedTTL", func(t *testing.T) {
		captured := setCapturingConnector(t, "conn")
		policy := btPolicy(t, captured.MockConnector, 2*time.Second, 1.0)
		cache := &SvmJsonRpcCache{projectId: "test-project", logger: &logger, policies: []*data.CachePolicy{policy}}

		req := realtimeReqBT(slot400ms)
		require.NoError(t, cache.Set(ctx, req, accountInfoResp(t, req)))
		require.NotNil(t, captured.ttl)
		assert.Equal(t, slot400ms, *captured.ttl)
	})

	t.Run("SetFallsBackToFixedTTLWhenSlotTimeUnknown", func(t *testing.T) {
		captured := setCapturingConnector(t, "conn")
		policy := btPolicy(t, captured.MockConnector, 2*time.Second, 1.0)
		cache := &SvmJsonRpcCache{projectId: "test-project", logger: &logger, policies: []*data.CachePolicy{policy}}

		req := realtimeReqBT(0)
		require.NoError(t, cache.Set(ctx, req, accountInfoResp(t, req)))
		require.NotNil(t, captured.ttl)
		assert.Equal(t, 2*time.Second, *captured.ttl)
	})

	t.Run("ChangingEMAChangesEffectiveTTL", func(t *testing.T) {
		capturedSlow := setCapturingConnector(t, "conn-slow")
		capturedFast := setCapturingConnector(t, "conn-fast")
		policySlow := btPolicy(t, capturedSlow.MockConnector, 2*time.Second, 1.0)
		policyFast := btPolicy(t, capturedFast.MockConnector, 2*time.Second, 1.0)

		cacheSlow := &SvmJsonRpcCache{projectId: "test-project", logger: &logger, policies: []*data.CachePolicy{policySlow}}
		cacheFast := &SvmJsonRpcCache{projectId: "test-project", logger: &logger, policies: []*data.CachePolicy{policyFast}}

		reqSlow := realtimeReqBT(slot400ms)
		reqFast := realtimeReqBT(slot200ms)
		require.NoError(t, cacheSlow.Set(ctx, reqSlow, accountInfoResp(t, reqSlow)))
		require.NoError(t, cacheFast.Set(ctx, reqFast, accountInfoResp(t, reqFast)))
		require.NotNil(t, capturedSlow.ttl)
		require.NotNil(t, capturedFast.ttl)
		assert.Equal(t, slot400ms, *capturedSlow.ttl)
		assert.Equal(t, slot200ms, *capturedFast.ttl)
		assert.NotEqual(t, *capturedSlow.ttl, *capturedFast.ttl)
	})

	t.Run("MultiplierAboveOneWidensTTL", func(t *testing.T) {
		captured := setCapturingConnector(t, "conn")
		policy := btPolicy(t, captured.MockConnector, 2*time.Second, 1.5)
		cache := &SvmJsonRpcCache{projectId: "test-project", logger: &logger, policies: []*data.CachePolicy{policy}}

		req := realtimeReqBT(slot400ms)
		require.NoError(t, cache.Set(ctx, req, accountInfoResp(t, req)))
		require.NotNil(t, captured.ttl)
		assert.Equal(t, time.Duration(float64(slot400ms)*1.5), *captured.ttl)
	})

	t.Run("UnknownSlotTimeNoStaticTTLUsesSafeDefault", func(t *testing.T) {
		captured := setCapturingConnector(t, "conn")
		policy := btPolicy(t, captured.MockConnector, 0, 1.0)
		cache := &SvmJsonRpcCache{projectId: "test-project", logger: &logger, policies: []*data.CachePolicy{policy}}

		req := realtimeReqBT(0)
		require.NoError(t, cache.Set(ctx, req, accountInfoResp(t, req)))
		require.NotNil(t, captured.ttl)
		assert.Equal(t, defaultRealtimeColdStartTTL, *captured.ttl)
	})

	t.Run("NonBlockTimeNetworkUsesStaticTTL", func(t *testing.T) {
		captured := setCapturingConnector(t, "conn")
		policy := btPolicy(t, captured.MockConnector, 2*time.Second, 1.0)
		cache := &SvmJsonRpcCache{projectId: "test-project", logger: &logger, policies: []*data.CachePolicy{policy}}

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["11111111111111111111111111111111"]}`,
		))
		req.SetNetwork(realtimeNetworkNoBlockTime{})

		require.NoError(t, cache.Set(ctx, req, accountInfoResp(t, req)))
		require.NotNil(t, captured.ttl)
		assert.Equal(t, 2*time.Second, *captured.ttl)
	})

	t.Run("MetricLabelsUseFixedTTLComponent", func(t *testing.T) {
		// ResolveTTL may yield 400ms storage expiry while GetTTL()/labels stay on
		// the configured fallback — cardinality stays stable.
		policy := btPolicy(t, plainConnector("conn"), 2*time.Second, 1.0)
		assert.Equal(t, 2*time.Second, *policy.GetTTL())
		assert.Equal(t, slot400ms, policy.ResolveTTL(slot400ms, defaultRealtimeColdStartTTL))
		assert.Equal(t, "2s", policy.GetTTL().String())
	})
}

// realtimeNetworkNoBlockTime is a Network stub without EvmBlockTime.
type realtimeNetworkNoBlockTime struct{}

func (realtimeNetworkNoBlockTime) Id() string        { return "svm:test" }
func (realtimeNetworkNoBlockTime) Label() string     { return "" }
func (realtimeNetworkNoBlockTime) ProjectId() string { return "test" }
func (realtimeNetworkNoBlockTime) Architecture() common.NetworkArchitecture {
	return common.ArchitectureSvm
}
func (realtimeNetworkNoBlockTime) Config() *common.NetworkConfig {
	return &common.NetworkConfig{Architecture: common.ArchitectureSvm}
}
func (realtimeNetworkNoBlockTime) Logger() *zerolog.Logger {
	l := log.Logger
	return &l
}
func (realtimeNetworkNoBlockTime) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (realtimeNetworkNoBlockTime) SvmHighestLatestSlot(context.Context) int64    { return 0 }
func (realtimeNetworkNoBlockTime) SvmHighestFinalizedSlot(context.Context) int64 {
	return 0
}
func (realtimeNetworkNoBlockTime) SvmHighestFinalizedSlotMax(context.Context) int64 { return 0 }
func (realtimeNetworkNoBlockTime) SvmHighestIndexedSlot(context.Context) int64      { return 0 }
func (realtimeNetworkNoBlockTime) Forward(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (realtimeNetworkNoBlockTime) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateRealtime
}
