package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockGetBlockByNumberNonNull makes every listed upstream answer
// eth_getBlockByNumber with a NON-null sentinel block (number 0x270f). The
// future-block short-circuit, when it fires, never dispatches — so the caller
// sees null. Asserting null vs. the sentinel cleanly distinguishes
// "short-circuited" from "dispatched". (eth_chainId is mocked by the setup
// helper already.)
func mockGetBlockByNumberNonNull(ids ...string) {
	for _, id := range ids {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x270f","hash":"0xabc"}}`))
	}
}

// A concrete block number beyond every eligible upstream's head can be served by
// no upstream yet, so erpc must return the truthful null immediately instead of
// dispatching + hedging across upstreams that all return empty.
func TestForward_FutureBlock_ShortCircuitsToNull(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		{id: "fb3", chainID: 123, latestBlock: 98},
	})
	network.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)
	mockGetBlockByNumberNonNull("fb1", "fb2", "fb3")

	// max observed head = 100; block 105 (0x69) is beyond every upstream.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x69",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.True(t, resp.IsResultEmptyish(ctx),
		"block 105 > max head 100 must short-circuit to null without dispatch")
}

// A request at (or below) the max observed head must dispatch normally — the
// short-circuit must not over-fire and swallow blocks an upstream actually has.
func TestForward_FutureBlock_AtMaxHead_Dispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		{id: "fb3", chainID: 123, latestBlock: 98},
	})
	mockGetBlockByNumberNonNull("fb1", "fb2", "fb3")

	// block 100 (0x64) == max head → not future → dispatched → sentinel block.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x64",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f",
		"block at the head must be dispatched (served from upstream), not short-circuited")
}

func TestForward_FutureBlock_MixedStaticCapDispatchesWithinAvailableRange(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "live", chainID: 123, latestBlock: 900},
		{id: "archive", chainID: 123, latestBlock: 1_200, upperExactBlock: 1_000},
	})
	mockGetBlockByNumberNonNull("live", "archive")

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x3b6",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f")
}

// The short-circuit is opt-in via evm.shortCircuitFutureBlocks; the default
// config (here: nil servedTip, no flag) must dispatch.
func TestForward_FutureBlock_ServedTipDisabled_Dispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		{id: "fb3", chainID: 123, latestBlock: 98},
	}, nil) // nil served-tip config => feature disabled
	mockGetBlockByNumberNonNull("fb1", "fb2", "fb3")

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x69",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f",
		"with served-tip disabled the short-circuit is off; request must dispatch")
}

// A "latest" tag carries no concrete future number (it resolves to a real block
// the upstream has), so it must never be short-circuited.
func TestForward_FutureBlock_LatestTag_Dispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		{id: "fb3", chainID: 123, latestBlock: 98},
	})
	mockGetBlockByNumberNonNull("fb1", "fb2", "fb3")

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f",
		"latest tag has no concrete future number; must dispatch normally")
}

// mockGetBlockByNumberAt makes every listed upstream answer
// eth_getBlockByNumber for blockHex only, counting each dispatch. Matching the
// concrete number keeps the state poller's latest/finalized polls off it, so it
// can be registered before the network is set up.
func mockGetBlockByNumberAt(blockHex, result string, dispatched *atomic.Int64, ids ...string) {
	for _, id := range ids {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				// gock runs every mock's filter on each request.
				if r.URL.Host != id+".localhost" {
					return false
				}
				body := util.SafeReadBody(r)
				if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"`+blockHex+`"`) {
					return false
				}
				dispatched.Add(1)
				return true
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
	}
}

const futureBlockSentinel = `{"number":"0x270f","hash":"0xabc"}`

func forwardGetBlockByNumber(t *testing.T, ctx context.Context, network *Network, blockHex string) *common.NormalizedResponse {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["` + blockHex + `",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

// evm.shortCircuitFutureBlocks enables the short-circuit without served-tip: a
// block above every head returns null and reaches no upstream.
func TestForward_FutureBlock_OptInWithoutServedTip_ShortCircuitsToNull(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	var dispatched atomic.Int64
	mockGetBlockByNumberAt("0x69", "null", &dispatched, "fb1", "fb2", "fb3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		{id: "fb3", chainID: 123, latestBlock: 98},
	}, nil)
	network.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)

	// max head = 100; block 105 (0x69) is beyond every upstream.
	resp := forwardGetBlockByNumber(t, ctx, network, "0x69")
	assert.True(t, resp.IsResultEmptyish(ctx),
		"block 105 > max head 100 must short-circuit to null")
	assert.Zero(t, dispatched.Load(),
		"a block beyond every upstream's head must not be dispatched to any upstream")
}

// Without served-tip "latest" is the corroborated (second-highest) head, but a
// block only the most-ahead upstream has is still servable: with the opt-in it
// must be dispatched, not nulled.
func TestForward_FutureBlock_OptInWithoutServedTip_OnlyMostAheadHasBlock_Dispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	var dispatched atomic.Int64
	mockGetBlockByNumberAt("0x64", futureBlockSentinel, &dispatched, "fb1", "fb2", "fb3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		{id: "fb3", chainID: 123, latestBlock: 98},
	}, nil)
	network.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)

	// block 100 (0x64) is above the corroborated head (99) but fb1 has it.
	resp := forwardGetBlockByNumber(t, ctx, network, "0x64")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f",
		"a block the most-ahead upstream has must be dispatched, not short-circuited")
}

// Every upstream of the network counts toward the ceiling, syncing ones
// included: a block at or below a syncing upstream's head must be dispatched.
func TestForward_FutureBlock_OptIn_SyncingUpstreamAhead_Dispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	var dispatched atomic.Int64
	mockGetBlockByNumberAt("0x69", futureBlockSentinel, &dispatched, "fb1", "fb2", "sync1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		{id: "sync1", chainID: 123, latestBlock: 110, syncing: true},
	}, nil)
	network.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)

	// block 105 (0x69) is above every non-syncing head (100) but below sync1's.
	resp := forwardGetBlockByNumber(t, ctx, network, "0x69")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f",
		"a syncing upstream may have the block; it must be dispatched")
}

// An upstream with no known head may already have the block, so the
// short-circuit fails open and the request is dispatched.
func TestForward_FutureBlock_OptIn_UpstreamWithUnknownHead_Dispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	var dispatched atomic.Int64
	mockGetBlockByNumberAt("0x65", futureBlockSentinel, &dispatched, "fb1", "fb2", "nohead")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		// no latestBlock: the poller never learns a head for this upstream
		{id: "nohead", chainID: 123},
	}, nil)
	network.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)

	// block 101 (0x65) is above every known head (100).
	resp := forwardGetBlockByNumber(t, ctx, network, "0x65")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f",
		"an upstream with an unknown head may have the block; it must be dispatched")
	assert.Positive(t, dispatched.Load())
}

// emptyResultConfidence: finalizedBlock decides how an empty upstream answer is
// treated, not whether an upstream can have the block. A block between the
// finalized and latest heads must still be dispatched.
func TestForward_FutureBlock_OptIn_FinalizedConfidence_UnfinalizedBlock_Dispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	var dispatched atomic.Int64
	mockGetBlockByNumberAt("0x5f", futureBlockSentinel, &dispatched, "fb1", "fb2", "fb3")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "fb1", chainID: 123, latestBlock: 100},
		{id: "fb2", chainID: 123, latestBlock: 99},
		{id: "fb3", chainID: 123, latestBlock: 98},
	}, nil)
	network.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)
	network.cfg.Evm.EmptyResultConfidence = common.AvailbilityConfidenceFinalized
	for _, u := range ups {
		u.EvmStatePoller().SuggestFinalizedBlock(90)
	}

	// block 95 (0x5f) is above every finalized head (90) but below the latest ones.
	resp := forwardGetBlockByNumber(t, ctx, network, "0x5f")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f",
		"an unfinalized block the upstreams already have must be dispatched")
}

// A fallback the selection policy cordons off can still serve through the
// per-request escape, so its head counts toward the ceiling: a block between
// the primaries' head and the fallbacks' head is dispatched, not nulled.
func TestForward_FutureBlock_OptIn_CordonedFallbackAhead_Dispatches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer util.ResetGock()

	var dispatched atomic.Int64
	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
		primaryLatest:  "0x64", // 100
		fallbackLatest: "0x6e", // 110
		enableFailover: true,
		mocks: func() {
			mockGetBlockByNumberAt("0x69", "null", &dispatched, "rpc1", "rpc2", "rpc3", "rpc4")
		},
		network: func(cfg *common.NetworkConfig) {
			cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)
		},
	})

	// block 105 (0x69) is above the primaries' head but below the fallbacks'.
	forwardGetBlockByNumber(t, ctx, network, "0x69")
	assert.Positive(t, dispatched.Load(),
		"a block a cordoned fallback may have must be dispatched, not short-circuited")
}

// The default selection policy mirrors sampled requests to the upstreams it
// excludes (probeExcluded). A short-circuited request is never dispatched, so it
// must not be mirrored either.
func TestForward_FutureBlock_OptIn_ShortCircuitNotMirroredToExcluded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer util.ResetGock()

	var dispatched atomic.Int64
	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
		primaryLatest:  "0x64", // 100
		fallbackLatest: "0x64", // 100, cordoned by the default policy
		enableFailover: true,
		mocks: func() {
			mockGetBlockByNumberAt("0x69", "null", &dispatched, "rpc1", "rpc2", "rpc3", "rpc4")
		},
		network: func(cfg *common.NetworkConfig) {
			cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)
		},
	})

	for i := 0; i < 5; i++ {
		resp := forwardGetBlockByNumber(t, ctx, network, "0x69")
		assert.True(t, resp.IsResultEmptyish(ctx))
	}
	// Mirrors are asynchronous; give the prober time to dispatch any.
	time.Sleep(500 * time.Millisecond)
	assert.Zero(t, dispatched.Load(),
		"a short-circuited request must reach no upstream, mirrored probes included")
}

// A band block with hedging on (spec §6.4): the request takes one retryable
// miss across its hedges. The first leg routes to the upstream whose head
// reaches the block (slow); a hedge gets a null below it, takes the miss
// and retries; a later hedge, beside that leg, stops without dispatching,
// and no hedge's null wins over the block.
func TestForward_BandBlock_HedgesShareOneMiss(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "bh1", chainID: 123, latestBlock: 100},
		{id: "bh2", chainID: 123, latestBlock: 100},
		{id: "bh3", chainID: 123, latestBlock: 100},
		{id: "bh4", chainID: 123, latestBlock: 101},
	})
	ex, err := NewNetworkExecutor(&common.FailsafeConfig{
		MatchMethod: "*",
		Hedge:       &common.HedgePolicyConfig{Delay: common.NewStaticDuration(20 * time.Millisecond), MaxCount: 2},
	}, network.logger, nil, nil)
	require.NoError(t, err)
	network.failsafeExecutors = []*networkExecutor{ex}

	var below, holder atomic.Int64
	// bh1 answers at once, so the first hedge takes the miss; its retry
	// lands on bh2 or bh3 and runs past the second hedge's start.
	for i, id := range []string{"bh1", "bh2", "bh3"} {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				if r.URL.Host != id+".localhost" {
					return false
				}
				body := util.SafeReadBody(r)
				if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"0x65"`) {
					return false
				}
				below.Add(1)
				return true
			}).
			Reply(200).
			Delay(time.Duration(min(i, 1)) * 60 * time.Millisecond).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	}
	gock.New("http://bh4.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != "bh4.localhost" {
				return false
			}
			body := util.SafeReadBody(r)
			if !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"0x65"`) {
				return false
			}
			holder.Add(1)
			return true
		}).
		Reply(200).
		Delay(80 * time.Millisecond).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + futureBlockSentinel + `}`))

	resp := forwardGetBlockByNumber(t, ctx, network, "0x65")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f", "the band block from the upstream whose head reaches it, not a hedge's null")
	assert.Equal(t, int64(1), holder.Load(), "the upstream at the block, asked once")
	assert.Equal(t, int64(2), below.Load(), "the miss and its retry; the hedge beside them dispatched no more")
}

// A future block dispatched (shortCircuitFutureBlocks off) takes one
// retryable miss too (spec §6.4): the first null is never final, and the
// answer after it goes to the client as the upstream gave it.
func TestForward_FutureBlock_FirstNullIsAMiss(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "fm1", chainID: 123, latestBlock: 100},
		{id: "fm2", chainID: 123, latestBlock: 100},
		{id: "fm3", chainID: 123, latestBlock: 100},
	})
	var first, second atomic.Int64
	mockGetBlockByNumberAt("0x69", "null", &first, "fm1")
	mockGetBlockByNumberAt("0x69", futureBlockSentinel, &second, "fm2", "fm3")

	resp := forwardGetBlockByNumber(t, ctx, network, "0x69")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f", "the answer after the future block's one miss")
	assert.Positive(t, first.Load(), "the first upstream answered null")
	assert.Equal(t, int64(1), second.Load(), "one retry")
}

// mockMethodAnswer makes every listed upstream answer method with result,
// counting each dispatch.
func mockMethodAnswer(method, result string, dispatched *atomic.Int64, ids ...string) {
	for _, id := range ids {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				if r.URL.Host != id+".localhost" {
					return false
				}
				if !strings.Contains(util.SafeReadBody(r), `"`+method+`"`) {
					return false
				}
				dispatched.Add(1)
				return true
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
	}
}

// A by-hash answer from a block above Latest waits up to 1 bt, then is
// null, from the cache as from an upstream (spec §6.4, §7 family 5): two
// instances share a cache, and instance 1, one block ahead, cached a
// receipt of its Latest. eth_getLogs by blockHash is of the family, and a
// request that ends during the wait gets null too.
func TestForward_ByHash_AboveLatestHeldFromCacheAndUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cacheCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{{
			Id:     "mem",
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		}},
		Policies: []*common.CachePolicyConfig{{
			Network:   "*",
			Method:    "*",
			Finality:  common.DataFinalityStateUnfinalized,
			TTL:       common.FixedDuration(time.Hour),
			Connector: "mem",
		}},
	}
	require.NoError(t, cacheCfg.SetDefaults())
	cache, err := evm.NewEvmJsonRpcCache(ctx, &log.Logger, cacheCfg)
	require.NoError(t, err)

	ahead, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "hc1", chainID: 123, latestBlock: 101},
		{id: "hc2", chainID: 123, latestBlock: 101},
	})
	behind, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "hd1", chainID: 123, latestBlock: 100},
		{id: "hd2", chainID: 123, latestBlock: 100},
	})
	ahead.cacheDal = cache.WithProjectId("test")
	behind.cacheDal = cache.WithProjectId("test")
	require.Equal(t, int64(101), servedHead(ahead, ctx).Latest)
	require.Equal(t, int64(100), servedHead(behind, ctx).Latest)

	const receipt = `{"blockNumber":"0x65","blockHash":"0x00000000000000000000000000000000000000000000000000000000000000b1","transactionHash":"0x00000000000000000000000000000000000000000000000000000000000000a1","status":"0x1"}`
	var aheadCalls, behindCalls atomic.Int64
	mockMethodAnswer("eth_getTransactionReceipt", receipt, &aheadCalls, "hc1", "hc2")
	mockMethodAnswer("eth_getTransactionReceipt", receipt, &behindCalls, "hd1", "hd2")
	const receiptReq = `{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0x00000000000000000000000000000000000000000000000000000000000000a1"]}`

	req := common.NewNormalizedRequest([]byte(receiptReq))
	resp, err := ahead.Forward(ctx, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.Contains(t, jrr.GetResultString(), `"0x65"`, "instance 1's Latest includes the block")
	require.Eventually(t, func() bool {
		probe := common.NewNormalizedRequest([]byte(receiptReq))
		probe.SetNetwork(behind)
		got, err := cache.WithProjectId("test").Get(ctx, probe)
		return err == nil && got != nil && !got.IsResultEmptyish(ctx)
	}, 2*time.Second, 20*time.Millisecond, "instance 1 cached the receipt")

	start := time.Now()
	req = common.NewNormalizedRequest([]byte(receiptReq))
	resp, err = behind.Forward(ctx, req)
	require.NoError(t, err)
	jrr, err = resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Equal(t, "null", jrr.GetResultString(), "a cached receipt above instance 2's Latest is null after the hold")
	assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond, "held up to 1 bt first")
	assert.Zero(t, behindCalls.Load(), "served from the cache")

	var logsCalls atomic.Int64
	mockMethodAnswer("eth_getLogs", `[{"blockNumber":"0x65","blockHash":"0x00000000000000000000000000000000000000000000000000000000000000b1","logIndex":"0x0"}]`, &logsCalls, "hd1", "hd2")
	reqCtx, reqCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer reqCancel()
	req = common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"blockHash":"0x00000000000000000000000000000000000000000000000000000000000000b1"}]}`))
	resp, err = behind.Forward(reqCtx, req)
	require.NoError(t, err)
	jrr, err = resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Equal(t, "null", jrr.GetResultString(), "logs by hash above Latest, request ended during the hold")
	assert.Positive(t, logsCalls.Load(), "dispatched to an upstream")
}
