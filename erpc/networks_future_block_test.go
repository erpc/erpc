package erpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/consensus"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
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
	sampleBlockTime(t, network)
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

// A block far above the ceiling whose reports are older than 1 bt is
// decided again after the stale ones are Asked (spec §6.4). A report that
// was fresh when the Ask began, and so was not Asked, ages past 1 bt while
// the Ask runs; the block is still future, null with no dispatch (row K7):
// the Ask refreshed what was stale at its start.
func TestForward_FutureBlock_ShortCircuitSurvivesSlowStaleAsk(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "fs1", chainID: 123, latestBlock: 100},
		{id: "fs2", chainID: 123, latestBlock: 100},
		{id: "fs3", chainID: 123, latestBlock: 100},
	})
	network.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)
	sampleBlockTime(t, network) // bt 1s
	// fs2 and fs3 answer an Ask half a block time later.
	for _, id := range []string{"fs2", "fs3"} {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return r.URL.Host == id+".localhost" && strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x65","timestamp":"0x6702a8f0"}}`))
	}
	var dispatched atomic.Int64
	mockGetBlockByNumberAt("0x44d", futureBlockSentinel, &dispatched, "fs1", "fs2", "fs3")

	// fs2 and fs3 last reported at setup, more than 1 bt ago; fs1 reports
	// now and is 0.7 bt old when the request arrives.
	time.Sleep(1200 * time.Millisecond)
	ups[0].EvmStatePoller().SuggestLatestBlock(101, time.Now())
	time.Sleep(700 * time.Millisecond)

	resp := forwardGetBlockByNumber(t, ctx, network, "0x44d") // 1101
	assert.True(t, resp.IsResultEmptyish(ctx), "a block 1000 above the ceiling is null")
	assert.Zero(t, dispatched.Load(), "and never dispatched")
}

// On a halted chain every Ask answers the head each upstream already
// voted for. That answer is still evidence taken at its send time (spec
// §6.4 ceiling): after one Ask, a block far above the ceiling is future,
// null with no dispatch, on every request, not kept in the band by a
// report time only a new head would move.
func TestForward_FutureBlock_HaltedChainAskDatesReport(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ids := []string{"hc1", "hc2", "hc3"}
	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: ids[0], chainID: 123, latestBlock: 100},
		{id: ids[1], chainID: 123, latestBlock: 100},
		{id: ids[2], chainID: 123, latestBlock: 100},
	})
	network.cfg.Evm.ShortCircuitFutureBlocks = util.BoolPtr(true)
	sampleBlockTime(t, network) // bt 1s
	// As a registry's network does: every poll's header goes through intake.
	network.sources.start(ctx)
	var polls atomic.Int64
	for _, id := range ids {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if r.URL.Host != id+".localhost" || !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"latest"`) {
					return false
				}
				polls.Add(1)
				return true
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(tipHead("a", 100)) + `}`))
	}
	var dispatched atomic.Int64
	mockGetBlockByNumberAt("0x44d", futureBlockSentinel, &dispatched, ids...)

	for round := 1; round <= 3; round++ {
		// Every report is older than 1 bt and the last Ask's fetch is
		// older than its pace: the request Asks each upstream once.
		time.Sleep(1200 * time.Millisecond)
		before := polls.Load()
		resp := forwardGetBlockByNumber(t, ctx, network, "0x44d") // 1101
		require.GreaterOrEqual(t, polls.Load()-before, int64(len(ids)), "round %d: the request Asks each stale upstream", round)
		assert.True(t, resp.IsResultEmptyish(ctx), "round %d: a block 1001 above a halted chain is null", round)
		assert.Zero(t, dispatched.Load(), "round %d: and never dispatched", round)
	}
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
	sampleBlockTime(t, network)

	// max head = 100; block 105 (0x69) is beyond every upstream.
	resp := forwardGetBlockByNumber(t, ctx, network, "0x69")
	assert.True(t, resp.IsResultEmptyish(ctx),
		"block 105 > max head 100 must short-circuit to null")
	assert.Zero(t, dispatched.Load(),
		"a block beyond every upstream's head must not be dispatched to any upstream")
}

// Without served-tip "latest" is the corroborated (second-highest) head, but a
// block only the most-ahead upstream has is still servable: with the opt-in it
// must be dispatched, not nulled. One failed poll of the most-ahead upstream,
// a 429, is no failing poll (spec §6.1, §6.4 ceiling): its head stays in the
// ceiling, so the block is still dispatched.
func TestForward_FutureBlock_OptInWithoutServedTip_OnlyMostAheadHasBlock_Dispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	var dispatched atomic.Int64
	mockGetBlockByNumberAt("0x64", futureBlockSentinel, &dispatched, "fb1", "fb2", "fb3")
	// Registered before the network starts, since building a gock mock
	// while pollers match requests is a data race; armed only around the Ask.
	var rateLimited atomic.Bool
	gock.New("http://fb1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if !rateLimited.Load() {
				return false
			}
			body := util.SafeReadBody(r)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
		}).
		Reply(429).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limited"}}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, ups := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
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

	sampleBlockTime(t, network)
	rateLimited.Store(true)
	a := ups[0].EvmStatePoller().Ask(ctx, common.HeadNeedFork, 0, common.AskRefresh)
	rateLimited.Store(false)
	require.Error(t, a.Err, "fb1's latest poll answered 429")
	network.refreshHeads()

	resp = forwardGetBlockByNumber(t, ctx, network, "0x64")
	jrr, err = resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f",
		"one 429 keeps fb1 in the ceiling: a block at its head is dispatched, not nulled")
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
		u.EvmStatePoller().SuggestFinalizedBlock(90, time.Now())
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
	sampleBlockTime(t, network)

	for i := 0; i < 5; i++ {
		resp := forwardGetBlockByNumber(t, ctx, network, "0x69")
		assert.True(t, resp.IsResultEmptyish(ctx))
	}
	// Mirrors are asynchronous; give the prober time to dispatch any.
	time.Sleep(500 * time.Millisecond)
	assert.Zero(t, dispatched.Load(),
		"a short-circuited request must reach no upstream, mirrored probes included")
}

// Every leg of a band request gets the band's one retry (spec §6.4), and a
// miss in one leg never stops another. Hedges: the first leg routes to the
// upstream whose head reaches the block (slow); a hedge gets a null below
// it and retries; a later hedge, beside that leg, dispatches too, and no
// hedge's null wins over the block. Consensus slots, each pinned to one
// upstream: all three slots' upstreams miss, one reporting the block it
// lacks; the band's Ask finds the fourth upstream's node at the block, and
// a slot's retry, head first, serves it. A slot's retry picks only the
// upstreams the request may still select: one whose answer was final
// (#1348) is not asked again.
func TestForward_BandBlock_EveryLegRetries(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Consensus (1 of 3) over four upstreams: cs1 reports 101 but lacks it,
	// cs2 and cs3 are at 100, cs4's node reaches 101 once Asked.
	var csHead atomic.Int64
	csHead.Store(100)
	heads := map[string]func() int64{
		"cs1": func() int64 { return 101 },
		"cs2": func() int64 { return 100 },
		"cs3": func() int64 { return 100 },
		"cs4": csHead.Load,
	}
	var slotMisses, csHolder atomic.Int64
	for _, id := range []string{"cs1", "cs2", "cs3", "cs4"} {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return r.URL.Host == id+".localhost" && strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x%x","timestamp":"0x6702a8f0"}}`, heads[id]())
				res.Body = io.NopCloser(strings.NewReader(body))
				res.ContentLength = int64(len(body))
				return res
			})
		result, calls := "null", &slotMisses
		if id == "cs4" {
			result, calls = futureBlockSentinel, &csHolder
		}
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if r.URL.Host != id+".localhost" || !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"0x65"`) {
					return false
				}
				calls.Add(1)
				return true
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
	}
	// Consensus (1 of 3) over three upstreams: cf1 reports 101 but lacks
	// it, cf2 and cf3 do not support the method, a final answer.
	var finalAnswers atomic.Int64
	for _, id := range []string{"cf1", "cf2", "cf3"} {
		head := int64(100)
		if id == "cf1" {
			head = 101
		}
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return r.URL.Host == id+".localhost" && strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x%x","timestamp":"0x6702a8f0"}}`, head)))
		answer := `"result":null`
		if id != "cf1" {
			answer = `"error":{"code":-32601,"message":"the method eth_getBlockByNumber does not exist/is not available"}`
		}
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if r.URL.Host != id+".localhost" || !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"0x65"`) {
					return false
				}
				if id != "cf1" {
					finalAnswers.Add(1)
				}
				return true
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,` + answer + `}`))
	}
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
	// lands on bh2 or bh3 and runs past the second hedge's start, and bh4
	// answers long after both.
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
		Delay(300 * time.Millisecond).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + futureBlockSentinel + `}`))

	resp := forwardGetBlockByNumber(t, ctx, network, "0x65")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f", "the band block from the upstream whose head reaches it, not a hedge's null")
	assert.Equal(t, int64(1), holder.Load(), "the upstream at the block, asked once")
	assert.GreaterOrEqual(t, below.Load(), int64(3), "the miss, its retry, and the hedge beside them")

	slots, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "cs1", chainID: 123, latestBlock: 101},
		{id: "cs2", chainID: 123, latestBlock: 100},
		{id: "cs3", chainID: 123, latestBlock: 100},
		{id: "cs4", chainID: 123, latestBlock: 100},
	})
	// A tip log policy's shape: 1 of 3, non-empty preferred, the most
	// common valid result on a dispute or too few participants.
	consCfg := &common.ConsensusPolicyConfig{
		MaxParticipants: 3, AgreementThreshold: 1, PreferNonEmpty: util.BoolPtr(true),
		DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
	}
	require.NoError(t, consCfg.SetDefaults())
	cons, err := consensus.NewConsensus(consCfg, slots.logger, "test")
	require.NoError(t, err)
	ex, err = NewNetworkExecutor(&common.FailsafeConfig{MatchMethod: "*", Consensus: consCfg}, slots.logger, cons, nil)
	require.NoError(t, err)
	slots.failsafeExecutors = []*networkExecutor{ex}
	require.Equal(t, int64(100), servedHead(slots, ctx).Latest)

	csHead.Store(101)
	resp = forwardGetBlockByNumber(t, ctx, slots, "0x65")
	jrr, err = resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f", "a slot's retry reaches the upstream the band's Ask found at the block")
	assert.Equal(t, int64(1), csHolder.Load(), "the upstream at the block, asked once")
	assert.GreaterOrEqual(t, slotMisses.Load(), int64(3), "every slot dispatched its own upstream")

	final, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "cf1", chainID: 123, latestBlock: 101},
		{id: "cf2", chainID: 123, latestBlock: 100},
		{id: "cf3", chainID: 123, latestBlock: 100},
	})
	cons, err = consensus.NewConsensus(consCfg, final.logger, "test")
	require.NoError(t, err)
	ex, err = NewNetworkExecutor(&common.FailsafeConfig{MatchMethod: "*", Consensus: consCfg}, final.logger, cons, nil)
	require.NoError(t, err)
	final.failsafeExecutors = []*networkExecutor{ex}
	require.Equal(t, int64(100), servedHead(final, ctx).Latest)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x65",false]}`))
	_, _ = final.Forward(ctx, req)
	assert.Equal(t, int64(2), finalAnswers.Load(), "each upstream that gave a final answer, asked once")
}

// A future block dispatched (shortCircuitFutureBlocks off) takes one
// retryable miss too (spec §6.4): the first null is never final, and the
// answer after it goes to the client as the upstream gave it. Once the
// block time is known, a block at most W / bt + 1 above the ceiling waits for
// it, at most W, while the voters are Asked every bt (near future): a
// numbered read and a log range are served once the nodes reach them,
// and a range still above the ceiling after W is refused, counted
// future_after_wait.
func TestForward_FutureBlock_FirstNullIsAMiss(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The nodes' head: their latest answer, and the blocks they hold.
	var head atomic.Int64
	head.Store(100)
	held := func(blockHex, result string) string {
		var n int64
		fmt.Sscanf(blockHex, "0x%x", &n)
		if head.Load() < n {
			return "null"
		}
		return result
	}
	ids := []string{"fm1", "fm2", "fm3"}
	var first, second, nearCalls atomic.Int64
	latestCalls := map[string]*atomic.Int64{}
	for _, id := range ids {
		latestCalls[id] = &atomic.Int64{}
	}
	mockGetBlockByNumberAt("0x69", "null", &first, "fm1")
	mockGetBlockByNumberAt("0x69", futureBlockSentinel, &second, "fm2", "fm3")
	for _, id := range ids {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if r.URL.Host != id+".localhost" || !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"latest"`) {
					return false
				}
				latestCalls[id].Add(1)
				return true
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x%x","timestamp":"0x6702a8f0"}}`, head.Load())
				res.Body = io.NopCloser(strings.NewReader(body))
				res.ContentLength = int64(len(body))
				return res
			})
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if r.URL.Host != id+".localhost" || !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"0x66"`) {
					return false
				}
				nearCalls.Add(1)
				return true
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				body := `{"jsonrpc":"2.0","id":1,"result":` + held("0x66", futureBlockSentinel) + `}`
				res.Body = io.NopCloser(strings.NewReader(body))
				res.ContentLength = int64(len(body))
				return res
			})
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return r.URL.Host == id+".localhost" && strings.Contains(util.SafeReadBody(r), `"eth_getLogs"`)
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				body := `{"jsonrpc":"2.0","id":1,"result":` + held("0x67", `[{"blockNumber":"0x67","blockHash":"0x00000000000000000000000000000000000000000000000000000000000000c7","logIndex":"0x0"}]`) + `}`
				res.Body = io.NopCloser(strings.NewReader(body))
				res.ContentLength = int64(len(body))
				return res
			})
	}

	network, ups := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "fm1", chainID: 123, latestBlock: 100},
		{id: "fm2", chainID: 123, latestBlock: 100},
		{id: "fm3", chainID: 123, latestBlock: 100},
	})

	resp := forwardGetBlockByNumber(t, ctx, network, "0x69")
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f", "the answer after the future block's one miss")
	assert.Positive(t, first.Load(), "the first upstream answered null")
	assert.Equal(t, int64(1), second.Load(), "one retry")

	// bt 1s: W 3s, so the near future is 4 blocks above the ceiling: W of
	// chain, and the block the chain may have made since the report.
	sampleBlockTime(t, network)
	afterWait := func() float64 {
		return promUtil.ToFloat64(telemetry.MetricHeadRefusalTotal.WithLabelValues("test", network.Label(), "future_after_wait"))
	}
	refused := afterWait()
	reach := func(n int64, after time.Duration) {
		time.AfterFunc(after, func() { head.Store(n) })
	}

	reach(0x66, 300*time.Millisecond)
	start := time.Now()
	resp = forwardGetBlockByNumber(t, ctx, network, "0x66")
	jrr, err = resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f", "a block 2 above the ceiling, served once the nodes reach it")
	assert.Less(t, time.Since(start), 3*time.Second, "within W")
	assert.Positive(t, nearCalls.Load())

	logs := func(toHex string) (*common.NormalizedResponse, error) {
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"` + toHex + `","toBlock":"` + toHex + `"}]}`))
		return network.Forward(ctx, req)
	}
	reach(0x67, 300*time.Millisecond)
	start = time.Now()
	resp, err = logs("0x67")
	require.NoError(t, err, "a log range 1 above the ceiling, served once the nodes reach it")
	jrr, err = resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), `"0x67"`)
	assert.Less(t, time.Since(start), 3*time.Second, "within W")
	assert.Equal(t, refused, afterWait(), "no near-future request refused yet")

	start = time.Now()
	_, err = logs("0x6a")
	require.Error(t, err, "a range 3 above a ceiling that stays")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable), "got %v", err)
	assert.GreaterOrEqual(t, time.Since(start), 2500*time.Millisecond, "refused once W ended")
	assert.Equal(t, refused+1, afterWait(), "counted future_after_wait")

	// A report taken just now can still trail the chain by a block: the
	// nodes made 0x68 right after it. 0x6b is 4 above the ceiling, yet
	// only 3 blocks, W of chain, above the nodes' head (§19 P12): it waits,
	// and is served once they reach it, never answered null before.
	var farCalls atomic.Int64
	for _, id := range ids {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if r.URL.Host != id+".localhost" || !strings.Contains(body, "eth_getBlockByNumber") || !strings.Contains(body, `"0x6b"`) {
					return false
				}
				farCalls.Add(1)
				return true
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				body := `{"jsonrpc":"2.0","id":1,"result":` + held("0x6b", futureBlockSentinel) + `}`
				res.Body = io.NopCloser(strings.NewReader(body))
				res.ContentLength = int64(len(body))
				return res
			})
	}
	head.Store(0x68)
	for _, u := range ups {
		u.EvmStatePoller().SuggestLatestBlock(0x67, time.Now())
	}
	// Faster than the 1s block time sampled: the wait's Asks, one per bt,
	// see 0x6b at the latest at 2s.
	reach(0x69, 300*time.Millisecond)
	reach(0x6a, 600*time.Millisecond)
	reach(0x6b, 900*time.Millisecond)
	start = time.Now()
	resp = forwardGetBlockByNumber(t, ctx, network, "0x6b")
	jrr, err = resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x270f", "a block W of chain above the nodes, served once they reach it")
	assert.GreaterOrEqual(t, time.Since(start), 850*time.Millisecond, "it waited for the chain")
	assert.Less(t, time.Since(start), 3*time.Second, "within W")
	assert.Equal(t, refused+1, afterWait(), "not refused")

	// On a 100ms chain, W 2s, a block that never comes waits all of W
	// while every tick Asks each voter below it for a head fetched after
	// the previous tick: about 20 fetches each. A tick that took the
	// previous tick's fetch, sent a few µs less than 1 bt before, would
	// see a new head only every 2 bt.
	now := time.Now().Unix()
	for i := int64(0); i <= 1000; i++ {
		network.metricsTracker.SampleConfirmedHeaderTime(network.networkId, network.Label(), 5000+i, now+1000+i/10)
	}
	require.InDelta(t, float64(100*time.Millisecond), float64(network.EvmBlockTime()), float64(5*time.Millisecond))
	before := map[string]int64{}
	for _, id := range ids {
		before[id] = latestCalls[id].Load()
	}
	start = time.Now()
	_, err = logs("0x70")
	require.Error(t, err, "a range 5 above a ceiling that stays")
	assert.GreaterOrEqual(t, time.Since(start), 1900*time.Millisecond, "refused once W ended")
	assert.Equal(t, refused+2, afterWait(), "counted future_after_wait")
	for _, id := range ids {
		assert.GreaterOrEqual(t, latestCalls[id].Load()-before[id], int64(16), "%s fetched once per tick", id)
	}
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
// request that ends during the wait gets null too. The wait Asks the
// voters Latest lacks for a head fetched after it began: on instance 3,
// whose voters' counters are fresh but whose nodes already hold the block,
// the answer is served. Each wait counts its outcome.
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

	ahead, aheadUps := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "hc1", chainID: 123, latestBlock: 101},
		{id: "hc2", chainID: 123, latestBlock: 101},
	})
	behind, behindUps := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "hd1", chainID: 123, latestBlock: 100},
		{id: "hd2", chainID: 123, latestBlock: 100},
	})
	// The voters give Head.Finalized: the receipt's block is above it, so
	// finality is known and the unfinalized policy caches it.
	for _, u := range append(aheadUps, behindUps...) {
		setServedTipFinalized(t, u, 90)
	}
	require.Eventually(t, func() bool {
		return servedHead(ahead, ctx).Finalized == 90 && servedHead(behind, ctx).Finalized == 90
	}, 2*time.Second, 10*time.Millisecond, "both instances' Head.Finalized is their voters'")
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

	// The instances share a label, so the waits' counts are read as deltas.
	held := func(rule string) float64 {
		return promUtil.ToFloat64(telemetry.MetricHeadRefusalTotal.WithLabelValues("test", behind.Label(), rule))
	}
	nulls := held("held_null")
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
	assert.Equal(t, nulls+2, held("held_null"), "both waits on instance 2 ended null")

	// Instance 3: its voters reported 100 just now, with a cadence of an
	// hour, so their counters meet any number need; their nodes hold 101.
	var head atomic.Int64
	head.Store(100)
	for _, id := range []string{"hf1", "hf2"} {
		gock.New("http://" + id + ".localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return r.URL.Host == id+".localhost" && strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x%x","timestamp":"0x6702a8f0"}}`, head.Load())
				res.Body = io.NopCloser(strings.NewReader(body))
				res.ContentLength = int64(len(body))
				return res
			})
	}
	nulls, served := held("held_null"), held("held_served")
	fresh, freshUps := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "hf1", chainID: 123, latestBlock: 100, debounce: time.Hour},
		{id: "hf2", chainID: 123, latestBlock: 100, debounce: time.Hour},
	})
	for _, u := range freshUps {
		setServedTipFinalized(t, u, 90)
	}
	require.Equal(t, int64(100), servedHead(fresh, ctx).Latest)
	var freshCalls atomic.Int64
	mockMethodAnswer("eth_getTransactionReceipt", receipt, &freshCalls, "hf1", "hf2")
	head.Store(101)
	resp, err = fresh.Forward(ctx, common.NewNormalizedRequest([]byte(receiptReq)))
	require.NoError(t, err)
	jrr, err = resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), `"0x65"`, "the wait's fetch finds 101 on both voters: the receipt is served")
	assert.Equal(t, int64(101), servedHead(fresh, ctx).Latest)
	assert.Equal(t, served+1, held("held_served"), "the wait on instance 3 ended served")
	assert.Equal(t, nulls, held("held_null"))
}

// sampleBlockTime gives network its first block time sample: before it,
// every report counts as older than 1 bt, so no block is future (spec §6.4).
func sampleBlockTime(t *testing.T, network *Network) {
	t.Helper()
	for i := int64(0); i < 3; i++ {
		network.metricsTracker.SampleConfirmedHeaderTime(network.networkId, network.Label(), 1000+i, time.Now().Unix()+100+i)
	}
	require.Positive(t, network.EvmBlockTime())
}
