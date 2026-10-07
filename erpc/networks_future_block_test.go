package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"

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

func TestForward_FutureBlock_OneAboveTrackedHeadDispatches(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "near-tip-a", chainID: 123, latestBlock: 100},
	})
	tracker := newTestTracker(newTestSSR(t, ctx), nil, headTrackerDeps{})
	tracker.head.TryUpdate(ctx, 100)
	network.headTracker = tracker
	gock.New("http://near-tip-a.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x65","hash":"0xabc"}}`))

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x65",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.Contains(t, jrr.GetResultString(), `"number":"0x65"`, "a block already served by the upstream must not become null")
	require.GreaterOrEqual(t, req.ExecState().Snapshot().UpstreamAttempts, 1)
}

func TestForward_FutureBlock_RetryEmptyAboveTrackerHead(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "aa-lagging", chainID: 123, latestBlock: 100},
		{id: "zz-ahead", chainID: 123, latestBlock: 100},
	}, &common.EvmServedTipConfig{EnabledFor: []string{"finalized"}})
	tracker := newTestTracker(newTestSSR(t, ctx), nil, headTrackerDeps{})
	tracker.head.TryUpdate(ctx, 100)
	network.headTracker = tracker
	lagging := gock.New("http://aa-lagging.localhost").Post("").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") }).
		Times(1).Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	gock.New("http://zz-ahead.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x65","hash":"0xabc"}}`))
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x65",false]}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.Contains(t, jrr.GetResultString(), `"number":"0x65"`)
	require.True(t, lagging.Done(), "first lagging upstream must have returned null")
	require.GreaterOrEqual(t, req.ExecState().Snapshot().UpstreamAttempts, 2)
}

func TestForward_FutureBlock_UnminedNearTipReturnsNull(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, _ := setupServedTipNetworkWith(t, ctx, []servedTipFixture{
		{id: "unmined", chainID: 123, latestBlock: 100},
	}, &common.EvmServedTipConfig{EnabledFor: []string{"finalized"}})
	gock.New("http://unmined.localhost").Post("").Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") }).
		Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x65",false]}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	require.True(t, resp.IsResultEmptyish(ctx))
	require.GreaterOrEqual(t, req.ExecState().Snapshot().UpstreamAttempts, 1)
}

// A block beyond the configured safety margin may be short-circuited without
// dispatching or consuming an upstream attempt.
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
	require.Equal(t, int64(100), network.evmHeadReference(ctx, false).Available)

	// max observed head = 100; default margin = 16; block 117 is beyond it.
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x75",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.True(t, resp.IsResultEmptyish(ctx),
		"block 117 > max head 100 + margin 16 must short-circuit to null")
	assert.Zero(t, req.ExecState().Snapshot().UpstreamAttempts)
}

func TestForward_FutureBlock_NegativeMarginDisablesShortCircuit(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, _ := setupServedTipNetwork(t, ctx, []servedTipFixture{
		{id: "no-short-circuit", chainID: 123, latestBlock: 100},
	})
	margin := int64(-1)
	network.cfg.Evm.FutureBlockShortCircuitMargin = &margin
	mockGetBlockByNumberNonNull("no-short-circuit")
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x75",false]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.Contains(t, jrr.GetResultString(), "0x270f")
	require.GreaterOrEqual(t, req.ExecState().Snapshot().UpstreamAttempts, 1)
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
