package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// headReportingConnector is a MockConnector that also implements data.CacheHeadReporter, exposing a
// configurable latest-block timestamp so the connector-fallback path of the realtime age guard can
// be exercised.
type headReportingConnector struct {
	*data.MockConnector
	latestTs int64
	known    bool
}

func (h *headReportingConnector) CacheLatestBlockTimestamp(networkId string) (int64, bool) {
	return h.latestTs, h.known
}

func newHeadReportingConnector(id string, latestTs int64, known bool) *headReportingConnector {
	mc := &data.MockConnector{}
	mc.On("Id").Return(id)
	return &headReportingConnector{MockConnector: mc, latestTs: latestTs, known: known}
}

func plainConnector(id string) *data.MockConnector {
	mc := &data.MockConnector{}
	mc.On("Id").Return(id)
	return mc
}

func freshGuardCache() *EvmJsonRpcCache {
	logger := log.Logger
	return &EvmJsonRpcCache{projectId: "test-project", logger: &logger}
}

func realtimeReq(method, params string) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s,"id":1}`, method, params),
	))
	req.SetNetwork(&testNetwork{finalityState: common.DataFinalityStateRealtime})
	return req
}

func tsPolicy(t *testing.T, conn data.Connector, ttl time.Duration) *data.CachePolicy {
	t.Helper()
	cfg := &common.CachePolicyConfig{
		Connector: "conn",
		Network:   "*",
		Method:    "*",
		Finality:  common.DataFinalityStateRealtime,
	}
	if ttl > 0 {
		cfg.TTL = common.Duration(ttl)
	}
	policy, err := data.NewCachePolicy(cfg, conn)
	require.NoError(t, err)
	return policy
}

func blockJrr(tsUnix int64) *common.JsonRpcResponse {
	return common.MustNewJsonRpcResponse(1, map[string]interface{}{
		"number":    "0x1234",
		"hash":      "0xabc",
		"timestamp": fmt.Sprintf("0x%x", tsUnix),
	}, nil)
}

func scalarJrr() *common.JsonRpcResponse {
	return common.MustNewJsonRpcResponse(1, "0x3b9aca00", nil) // a gas price, no block reference
}

func logsJrr() *common.JsonRpcResponse {
	return common.MustNewJsonRpcResponse(1, []interface{}{
		map[string]interface{}{"address": "0xabc", "blockNumber": "0x1234", "data": "0x"},
	}, nil)
}

// TestEvmJsonRpcCache_ConnectorFreshnessGuard exercises the realtime age guard: the block timestamp
// is taken from the response when present, else from the serving connector's reported latest-block
// timestamp, and the result is rejected when older than the policy TTL.
func TestEvmJsonRpcCache_ConnectorFreshnessGuard(t *testing.T) {
	ctx := context.Background()
	cache := freshGuardCache()
	now := time.Now().Unix()

	// --- response-timestamp path (block-bearing methods; pre-existing behavior) ---

	t.Run("ResponseTimestampFreshAccepted", func(t *testing.T) {
		policy := tsPolicy(t, plainConnector("conn"), time.Minute)
		req := realtimeReq("eth_getBlockByNumber", `["latest",false]`)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-5), policy))
	})

	t.Run("ResponseTimestampStaleRejected", func(t *testing.T) {
		policy := tsPolicy(t, plainConnector("conn"), time.Minute)
		req := realtimeReq("eth_getBlockByNumber", `["latest",false]`)
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-120), policy))
	})

	// --- connector-fallback path (methods whose response carries no block timestamp) ---

	t.Run("NoResponseTsConnectorFreshAccepted", func(t *testing.T) {
		policy := tsPolicy(t, newHeadReportingConnector("conn", now-5, true), time.Minute)
		req := realtimeReq("eth_gasPrice", `[]`)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})

	t.Run("NoResponseTsConnectorStaleRejected", func(t *testing.T) {
		policy := tsPolicy(t, newHeadReportingConnector("conn", now-120, true), time.Minute)
		req := realtimeReq("eth_gasPrice", `[]`)
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})

	// The motivating win: getLogs carries no top-level block timestamp, so only the connector's
	// reported head can detect a lagging source.
	t.Run("GetLogsConnectorStaleRejected", func(t *testing.T) {
		policy := tsPolicy(t, newHeadReportingConnector("conn", now-120, true), time.Minute)
		req := realtimeReq("eth_getLogs", `[{"fromBlock":"latest","toBlock":"latest"}]`)
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, logsJrr(), policy))
	})

	t.Run("EthBlockNumberConnectorStaleRejected", func(t *testing.T) {
		policy := tsPolicy(t, newHeadReportingConnector("conn", now-120, true), time.Minute)
		req := realtimeReq("eth_blockNumber", `[]`)
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, common.MustNewJsonRpcResponse(1, "0x1234", nil), policy))
	})

	// --- fail-open cases ---

	t.Run("NoResponseTsConnectorNotHeadAwareAccepted", func(t *testing.T) {
		policy := tsPolicy(t, plainConnector("conn"), time.Minute)
		req := realtimeReq("eth_gasPrice", `[]`)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})

	t.Run("NoResponseTsConnectorHeadUnknownAccepted", func(t *testing.T) {
		policy := tsPolicy(t, newHeadReportingConnector("conn", 0, false), time.Minute)
		req := realtimeReq("eth_gasPrice", `[]`)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})

	// --- precedence + bypass ---

	t.Run("ResponseTimestampPreferredOverConnector", func(t *testing.T) {
		// Response is fresh but the connector's polled head looks stale: the per-response timestamp
		// wins (fresher, no poll lag), so the result is accepted.
		policy := tsPolicy(t, newHeadReportingConnector("conn", now-120, true), time.Minute)
		req := realtimeReq("eth_getBlockByNumber", `["latest",false]`)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-5), policy))
	})

	t.Run("NonRealtimeFinalityAccepted", func(t *testing.T) {
		policy := tsPolicy(t, newHeadReportingConnector("conn", now-100000, true), time.Minute)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_gasPrice","params":[],"id":1}`))
		req.SetNetwork(&testNetwork{finalityState: common.DataFinalityStateFinalized})
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})

	t.Run("NoTTLAccepted", func(t *testing.T) {
		policy := tsPolicy(t, newHeadReportingConnector("conn", now-100000, true), 0)
		req := realtimeReq("eth_gasPrice", `[]`)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})
}

// TestEvmJsonRpcCache_ConnectorFreshnessThroughFailsafeWrapper verifies the connector-fallback path
// still works when the head-aware connector is wrapped by a FailsafeConnector — the production setup,
// since NewConnector wraps any connector that configures failsafeForGets/Sets (the prism cache does).
func TestEvmJsonRpcCache_ConnectorFreshnessThroughFailsafeWrapper(t *testing.T) {
	ctx := context.Background()
	cache := freshGuardCache()
	logger := log.Logger
	now := time.Now().Unix()

	wrap := func(t *testing.T, base data.Connector) data.Connector {
		t.Helper()
		w, err := data.NewFailsafeConnector(&logger, base, nil, nil)
		require.NoError(t, err)
		return w
	}

	t.Run("RejectsWhenWrappedConnectorStale", func(t *testing.T) {
		policy := tsPolicy(t, wrap(t, newHeadReportingConnector("conn", now-120, true)), time.Minute)
		req := realtimeReq("eth_gasPrice", `[]`)
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})

	t.Run("AcceptsWhenWrappedConnectorFresh", func(t *testing.T) {
		policy := tsPolicy(t, wrap(t, newHeadReportingConnector("conn", now-5, true)), time.Minute)
		req := realtimeReq("eth_gasPrice", `[]`)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})

	t.Run("FailsOpenWhenWrappedBaseNotHeadAware", func(t *testing.T) {
		policy := tsPolicy(t, wrap(t, plainConnector("conn")), time.Minute)
		req := realtimeReq("eth_gasPrice", `[]`)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, scalarJrr(), policy))
	})
}

// TestEvmJsonRpcCache_ConnectorFreshnessThroughGet proves the guard is wired into the Get fan-out
// for realtime finality: a stale realtime hit becomes a miss (nil → caller falls through), a fresh
// one is served from cache.
func TestEvmJsonRpcCache_ConnectorFreshnessThroughGet(t *testing.T) {
	ctx := context.Background()
	logger := log.Logger

	// Connectors store the bare JSON-RPC result (not the full envelope); see doGet.
	blockResult := func(tsUnix int64) []byte {
		b, _ := json.Marshal(map[string]interface{}{
			"number":    "0x1234",
			"hash":      "0xabc",
			"timestamp": fmt.Sprintf("0x%x", tsUnix),
		})
		return b
	}

	setup := func(blockTs int64, ttl time.Duration) (*EvmJsonRpcCache, *common.NormalizedRequest) {
		mc := &data.MockConnector{}
		mc.On("Id").Return("conn")
		b := blockResult(blockTs)
		mc.On("Get", mock.Anything, data.ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).Return(b, nil)
		mc.On("Get", mock.Anything, data.ConnectorReverseIndex, mock.Anything, mock.Anything, mock.Anything).Return(b, nil).Maybe()

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "conn",
			Network:   "*",
			Method:    "eth_getBlockByNumber",
			Finality:  common.DataFinalityStateRealtime,
			TTL:       common.Duration(ttl),
		}, mc)
		require.NoError(t, err)

		cache := &EvmJsonRpcCache{projectId: "test-project", logger: &logger, policies: []*data.CachePolicy{policy}}
		req := realtimeReq("eth_getBlockByNumber", `["latest",false]`)
		return cache, req
	}

	t.Run("ServesFresh", func(t *testing.T) {
		cache, req := setup(time.Now().Unix()-5, time.Minute)
		resp, err := cache.Get(ctx, req)
		assert.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.FromCache())
	})

	t.Run("MissesStale", func(t *testing.T) {
		cache, req := setup(time.Now().Unix()-120, time.Minute)
		resp, err := cache.Get(ctx, req)
		assert.NoError(t, err)
		assert.Nil(t, resp, "stale realtime hit should be treated as a miss")
	})
}
