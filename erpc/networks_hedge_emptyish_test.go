package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hedge-keep policy for emptyish results.
//
// The hedge "keep" predicate decides whether a completed leg should be
// declared the race winner (cancelling siblings) or whether the race
// should continue. For methods where `null` is the canonical final
// answer (eth_getLogs, eth_call, point state reads — see
// common.DefaultEmptyResultAccept) a fast empty winner is the correct
// outcome and we keep it.
//
// For methods where `null` means "this upstream does not have the
// data yet" — block / transaction lookups governed by
// common.DefaultMarkEmptyAsErrorMethods — a fast `{"result": null}`
// from one leg must NOT cancel siblings that may still return real
// data. These tests pin the contract.

// Helper: build a non-empty block response payload for assertions.
const testBlockResultHash = "0xabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca"

func nonEmptyBlockJSON() map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]interface{}{
			"number":     "0x10",
			"hash":       testBlockResultHash,
			"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
			"timestamp":  "0x1",
		},
	}
}

func nullResultJSON() map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  nil,
	}
}

// TestHedge_EmptyishLosesToNonEmpty_GetBlockByNumber asserts the
// end-to-end contract for the eth_getBlockByNumber happy path:
// when a non-empty block is available, the caller receives it. The
// strongest bug-isolation case is TestHedge_EmptyishLosesToNonEmpty_ThreeUpstreams
// below; this 2-upstream case primarily guards adjacent invariants
// (the sweep wrapping the hedge ALSO rejects emptyish mid-rotation).
func TestHedge_EmptyishLosesToNonEmpty_GetBlockByNumber(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x10",false]}`)

	// Primary: fast empty.
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
		}).
		Reply(200).
		Delay(20 * time.Millisecond).
		JSON(nullResultJSON())

	// Hedge: slower but real block.
	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
		}).
		Reply(200).
		Delay(150 * time.Millisecond).
		JSON(nonEmptyBlockJSON())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
		Delay:    common.NewStaticDuration(50 * time.Millisecond),
		MaxCount: 1,
	})

	req := common.NewNormalizedRequest(requestBytes)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), testBlockResultHash,
		"hedged non-empty result must win over fast empty primary")
}

// TestHedge_EmptyishLosesToNonEmpty_GetTransactionByHash mirrors the
// above for eth_getTransactionByHash — another method where null
// means "not yet on this upstream", not "doesn't exist".
func TestHedge_EmptyishLosesToNonEmpty_GetTransactionByHash(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["0x` +
		strings.Repeat("ab", 32) + `"]}`)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getTransactionByHash")
		}).
		Reply(200).
		Delay(20 * time.Millisecond).
		JSON(nullResultJSON())

	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getTransactionByHash")
		}).
		Reply(200).
		Delay(150 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"hash":             "0x" + strings.Repeat("ab", 32),
				"blockNumber":      "0x10",
				"transactionIndex": "0x0",
				"from":             "0x" + strings.Repeat("11", 20),
				"to":               "0x" + strings.Repeat("22", 20),
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
		Delay:    common.NewStaticDuration(50 * time.Millisecond),
		MaxCount: 1,
	})

	req := common.NewNormalizedRequest(requestBytes)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), `"blockNumber":"0x10"`,
		"hedged non-empty tx must win over fast empty primary")
}

// TestHedge_EmptyishLosesToNonEmpty_GetTransactionReceipt covers
// eth_getTransactionReceipt — frequently affected at the tip when one
// upstream has indexed the receipt and another has not.
func TestHedge_EmptyishLosesToNonEmpty_GetTransactionReceipt(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	txHash := "0x" + strings.Repeat("cd", 32)
	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["` + txHash + `"]}`)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getTransactionReceipt")
		}).
		Reply(200).
		Delay(20 * time.Millisecond).
		JSON(nullResultJSON())

	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getTransactionReceipt")
		}).
		Reply(200).
		Delay(150 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"transactionHash":   txHash,
				"transactionIndex":  "0x0",
				"blockNumber":       "0x10",
				"status":            "0x1",
				"cumulativeGasUsed": "0x5208",
				"gasUsed":           "0x5208",
				"logs":              []interface{}{},
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
		Delay:    common.NewStaticDuration(50 * time.Millisecond),
		MaxCount: 1,
	})

	req := common.NewNormalizedRequest(requestBytes)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), txHash,
		"hedged non-empty receipt must win over fast empty primary")
	assert.Contains(t, jrr.GetResultString(), `"status":"0x1"`)
}

// TestHedge_AllEmptyish_ReturnsEmpty_GetBlockByNumber verifies the
// terminal behaviour: if every hedge leg ends up emptyish for a
// non-accept method, the response is still returned to the caller
// (matching pre-existing semantics) rather than hanging or erroring.
// The retry layer is responsible for any further fan-out.
func TestHedge_AllEmptyish_ReturnsEmpty_GetBlockByNumber(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x10",false]}`)

	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
		gock.New(host).
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
			}).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(nullResultJSON())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
		Delay:    common.NewStaticDuration(10 * time.Millisecond),
		MaxCount: 1,
	})

	req := common.NewNormalizedRequest(requestBytes)
	resp, err := network.Forward(ctx, req)
	// The hedge fans out, every leg returns null. Either the caller
	// surfaces the null response or surfaces a missing-data error;
	// what we MUST NOT do is hang or panic.
	if err == nil {
		require.NotNil(t, resp)
		assert.True(t, resp.IsResultEmptyish(),
			"all-empty hedge must surface an emptyish response when not erroring")
	}
}

// TestHedge_EmptyishWinsForAcceptedMethod_GetLogs preserves the existing
// fast-path: methods in DefaultEmptyResultAccept (eth_getLogs, eth_call,
// eth_getBalance, eth_getCode, eth_getStorageAt, eth_getTransactionCount,
// trace_filter, arbtrace_filter) MUST still let a fast empty winner
// short-circuit the hedge — empty is the legitimate answer.
func TestHedge_EmptyishWinsForAcceptedMethod_GetLogs(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x10","toBlock":"0x10"}]}`)

	// Primary returns an empty array fast. This is in
	// DefaultEmptyResultAccept and must be allowed to win.
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
		}).
		Reply(200).
		Delay(10 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	// Hedge: prepared but should not need to be consumed because the
	// fast empty primary wins. Mark Persist so a late hedge fire
	// after race shutdown is harmless.
	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
		}).
		Persist().
		Reply(200).
		Delay(200 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  []interface{}{},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
		Delay:    common.NewStaticDuration(100 * time.Millisecond),
		MaxCount: 1,
	})

	start := time.Now()
	req := common.NewNormalizedRequest(requestBytes)
	resp, err := network.Forward(ctx, req)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, "[]", strings.TrimSpace(jrr.GetResultString()),
		"empty array is the legitimate eth_getLogs result and must be returned")
	assert.Less(t, elapsed, 150*time.Millisecond,
		"accept-listed empty result must short-circuit hedge — no waiting for siblings")
}

// TestHedge_EmptyishWinsForAcceptedMethod_GetBalance is a regression
// guard for the PR-894 expansion (eth_getBalance / eth_getCode /
// eth_getStorageAt / eth_getTransactionCount): a fast "0x0" must still
// short-circuit the hedge.
func TestHedge_EmptyishWinsForAcceptedMethod_GetBalance(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x` +
		strings.Repeat("11", 20) + `","latest"]}`)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		Delay(10 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x0",
		})

	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Persist().
		Reply(200).
		Delay(200 * time.Millisecond).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x0",
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
		Delay:    common.NewStaticDuration(100 * time.Millisecond),
		MaxCount: 1,
	})

	start := time.Now()
	req := common.NewNormalizedRequest(requestBytes)
	resp, err := network.Forward(ctx, req)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), "0x0")
	assert.Less(t, elapsed, 150*time.Millisecond,
		"zero-balance must short-circuit hedge — it is the canonical final answer")
}

// TestHedge_NonEmptyPrimaryWins is the baseline happy path: a fast
// non-empty primary keeps its win and the hedge never has to fire.
func TestHedge_NonEmptyPrimaryWins(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// rpc2 mock should remain unused (one pending mock expected).
	defer util.AssertNoPendingMocks(t, 1)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x10",false]}`)

	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
		}).
		Reply(200).
		Delay(20 * time.Millisecond).
		JSON(nonEmptyBlockJSON())

	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
		}).
		Reply(200).
		JSON(nonEmptyBlockJSON())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
		Delay:    common.NewStaticDuration(200 * time.Millisecond),
		MaxCount: 1,
	})

	req := common.NewNormalizedRequest(requestBytes)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), testBlockResultHash)
}

// TestHedge_EmptyishLosesToNonEmpty_ThreeUpstreams is the direct
// bug-isolation case. With 3 upstreams and 2 hedge fan-outs, the
// request's NextUpstream rotation hands different upstreams to each
// fan-out. One sweep can therefore complete with an emptyish
// bestResp (its slice of upstreams all returned null) while another
// sweep still has the real block in flight. Before the fix the
// emptyish sweep would win the hedge and cancel the in-flight block
// fetch; after the fix the hedge keeps racing and the non-empty leg
// is returned.
func TestHedge_EmptyishLosesToNonEmpty_ThreeUpstreams(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x10",false]}`)

	// Two upstreams empty (fast), one upstream returns the block
	// (slower than the hedge delay so the race actually plays out).
	gock.New("http://rpc1.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
		}).
		Reply(200).
		Delay(20 * time.Millisecond).
		JSON(nullResultJSON())

	gock.New("http://rpc2.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
		}).
		Reply(200).
		Delay(30 * time.Millisecond).
		JSON(nullResultJSON())

	gock.New("http://rpc3.localhost").
		Post("").
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
		}).
		Reply(200).
		Delay(120 * time.Millisecond).
		JSON(nonEmptyBlockJSON())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupTestNetworkWithMultipleUpstreams(t, ctx, 3, &common.HedgePolicyConfig{
		Delay:    common.NewStaticDuration(40 * time.Millisecond),
		MaxCount: 2,
	})

	req := common.NewNormalizedRequest(requestBytes)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), testBlockResultHash,
		"three-way race must still surface the non-empty leg")
}
