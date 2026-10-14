package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
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

// The short-circuit is gated on served-tip being enabled for the latest axis
// (so the head is trustworthy). With served-tip disabled it must stay off.
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
