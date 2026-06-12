package evm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockTimeNetwork wraps the shared testNetwork fake and reports a fixed EMA
// block time, exercising the realtime age guard's block-time-derived TTL path
// deterministically (no real health.Tracker warm-up needed).
type blockTimeNetwork struct {
	*testNetwork
	blockTime time.Duration
}

func (n *blockTimeNetwork) EvmBlockTime() time.Duration { return n.blockTime }

// realtimeReqBT builds a realtime request whose network reports blockTime as its
// estimated (EMA) block time. blockTime == 0 mimics a cold/unknown estimate.
func realtimeReqBT(method, params string, blockTime time.Duration) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s,"id":1}`, method, params),
	))
	req.SetNetwork(&blockTimeNetwork{
		testNetwork: &testNetwork{finalityState: common.DataFinalityStateRealtime},
		blockTime:   blockTime,
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

// TestEvmJsonRpcCache_BlockTimeDerivedTTL exercises the realtime age guard when
// the effective TTL is derived from the network's estimated block time (the
// ttl's object form, blockTimeMultiplier) instead of a fixed value. Scenarios
// mirror real chains.
func TestEvmJsonRpcCache_BlockTimeDerivedTTL(t *testing.T) {
	ctx := context.Background()
	cache := freshGuardCache()
	now := time.Now().Unix()

	const (
		mainnetBT = 12 * time.Second // ~ethereum mainnet
		fastBT    = 2 * time.Second  // ~base/polygon/optimism
	)

	// --- mainnet (~12s blocks), multiplier 1.0 => ~1 block of head staleness ---

	t.Run("MainnetHeadWithinOneBlockAccepted", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 0, 1.0)
		req := realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, mainnetBT)
		// 5s old < 12s effective TTL -> served from cache
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-5), policy))
	})

	t.Run("MainnetHeadOlderThanOneBlockRejected", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 0, 1.0)
		req := realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, mainnetBT)
		// 20s old > 12s effective TTL -> refetched
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-20), policy))
	})

	// --- fast chain (~2s blocks): same multiplier yields a much tighter window ---

	t.Run("FastChainHeadWithinOneBlockAccepted", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 0, 1.0)
		req := realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, fastBT)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-1), policy))
	})

	t.Run("FastChainHeadOlderThanOneBlockRejected", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 0, 1.0)
		req := realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, fastBT)
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-5), policy))
	})

	// The core win: an identical 5s-old head is accepted on mainnet but rejected
	// on a fast chain with the SAME policy — TTL tracks each chain's cadence.
	t.Run("SamePolicyDiffersByChainCadence", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 0, 1.0)
		head5sOld := blockJrr(now - 5)
		assert.True(t, cache.shouldAcceptCachedResult(ctx,
			realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, mainnetBT), head5sOld, policy))
		assert.False(t, cache.shouldAcceptCachedResult(ctx,
			realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, fastBT), head5sOld, policy))
	})

	// --- multiplier > 1 widens the window proportionally ---

	t.Run("MultiplierAboveOneToleratesMoreBlocks", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 0, 1.5) // 12s * 1.5 = 18s
		req := realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, mainnetBT)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-15), policy))  // 15s < 18s
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-25), policy)) // 25s > 18s
	})

	// --- block time wins over the static TTL when known ---

	t.Run("BlockTimeOverridesStaticTTLWhenKnown", func(t *testing.T) {
		// Static 2s would reject a 5s-old mainnet head; block-time (12s) keeps it.
		policy := btPolicy(t, plainConnector("conn"), 2*time.Second, 1.0)
		req := realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, mainnetBT)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-5), policy))
	})

	// --- cold EMA: block time unknown -> fall back to the static TTL ---

	t.Run("UnknownBlockTimeFallsBackToStaticTTL", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 2*time.Second, 1.0)
		req := realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, 0)                // EMA not warmed up
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-1), policy))  // 1s < 2s static
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-5), policy)) // 5s > 2s static
	})

	// --- cold EMA and no static TTL -> bounded by the built-in safe default (2s) ---

	t.Run("UnknownBlockTimeNoStaticTTLUsesSafeDefault", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 0, 1.0)
		req := realtimeReqBT("eth_getBlockByNumber", `["latest",false]`, 0)
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-1), policy))  // 1s < 2s default
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-5), policy)) // 5s > 2s default
	})

	// --- network not block-time-aware -> static TTL still applies ---

	t.Run("NonBlockTimeNetworkUsesStaticTTL", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 2*time.Second, 1.0)
		req := realtimeReq("eth_getBlockByNumber", `["latest",false]`) // plain testNetwork, no EvmBlockTime
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-1), policy))
		assert.False(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-5), policy))
	})

	// --- getLogs: no response timestamp, block-time TTL applied to connector head ---

	t.Run("GetLogsBlockTimeTTLViaConnectorHead", func(t *testing.T) {
		// Connector head is 5s behind. Fast chain (2s) -> rejected; mainnet (12s) -> accepted.
		fastPolicy := btPolicy(t, newHeadReportingConnector("conn", now-5, true), 0, 1.0)
		assert.False(t, cache.shouldAcceptCachedResult(ctx,
			realtimeReqBT("eth_getLogs", `[{"fromBlock":"latest","toBlock":"latest"}]`, fastBT), logsJrr(), fastPolicy))

		mainnetPolicy := btPolicy(t, newHeadReportingConnector("conn", now-5, true), 0, 1.0)
		assert.True(t, cache.shouldAcceptCachedResult(ctx,
			realtimeReqBT("eth_getLogs", `[{"fromBlock":"latest","toBlock":"latest"}]`, mainnetBT), logsJrr(), mainnetPolicy))
	})

	// --- non-realtime finality ignores the block-time TTL entirely ---

	t.Run("NonRealtimeFinalityUnaffected", func(t *testing.T) {
		policy := btPolicy(t, plainConnector("conn"), 0, 1.0)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1234",false],"id":1}`))
		req.SetNetwork(&blockTimeNetwork{
			testNetwork: &testNetwork{finalityState: common.DataFinalityStateFinalized},
			blockTime:   fastBT,
		})
		// Finalized data is immutable -> accepted regardless of age.
		assert.True(t, cache.shouldAcceptCachedResult(ctx, req, blockJrr(now-100000), policy))
	})
}
