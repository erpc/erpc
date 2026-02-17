package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestNetworkRetry_MissingDataError(t *testing.T) {
	t.Run("WithinSameAttempt_FallsBackToSecondUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 5,
			},
		)

		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err, "Should succeed with second upstream")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.Contains(t, jrr.GetResultString(), "0x42")
	})

	t.Run("AllUpstreamsMissingData_NetworkRetryHappens", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1CallCount := 0
		rpc2CallCount := 0

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response {
				rpc1CallCount++
				return r
			}).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response {
				rpc2CallCount++
				return r
			}).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"0xa6d381"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		_, err := network.Forward(ctx, req)

		require.Error(t, err, "Should fail when all upstreams return missing data")

		totalCalls := rpc1CallCount + rpc2CallCount
		t.Logf("Total upstream calls: %d (rpc1: %d, rpc2: %d)", totalCalls, rpc1CallCount, rpc2CallCount)

		// Both upstreams should be tried exactly once (failover from rpc1 to rpc2).
		// "missing trie node" is classified as ErrEndpointMissingData which is not retryable
		// toward the same upstream but is retryable toward the network (tries other upstreams).
		assert.Equal(t, 2, totalCalls, "Each upstream should be called exactly once")
		assert.Equal(t, 1, rpc1CallCount, "rpc1 should be called exactly once")
		assert.Equal(t, 1, rpc2CallCount, "rpc2 should be called exactly once")
	})
}

func TestNetworkRetry_ServerSideException(t *testing.T) {
	t.Run("HTTP500_ShouldRetryWithOtherUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(500).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Internal server error",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 5,
			},
		)

		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err, "Should succeed by retrying with second upstream after HTTP 500")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.Contains(t, jrr.GetResultString(), "0x42")
	})

	t.Run("JSONRPCError_InternalError_ShouldRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Internal error",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 5,
			},
		)

		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err, "Should succeed by retrying with second upstream")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.Contains(t, jrr.GetResultString(), "0x42")
	})
}

func TestNetworkRetry_ExecutionException_NoRetry(t *testing.T) {
	t.Run("ExecutionReverted_ShouldNotRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should NOT be called

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted",
					"data":    "0x08c379a0",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 5,
			},
		)

		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		_, err := network.Forward(ctx, req)

		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException),
			"Should return execution exception, got: %v", err)
	})
}

func TestNetworkRetry_RetryEmptyDirective(t *testing.T) {
	t.Run("RetryEmpty_WithMissingDataError_ShouldRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts:       5,
				EmptyResultAccept: []string{},
			},
		)

		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"0xa6d381"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		dirs := req.Directives()
		require.NotNil(t, dirs)
		assert.True(t, dirs.RetryEmpty, "RetryEmpty directive should be true")

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err, "Should succeed with RetryEmpty enabled")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.Contains(t, jrr.GetResultString(), "0x42")
	})
}

func TestNetworkRetry_ArchiveDataRequest(t *testing.T) {
	t.Run("ArchiveRequest_OneArchiveNodeAvailable_ShouldSucceed", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 5,
			},
		)

		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"0x1000000"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err, "Should succeed by using archive node after full node fails")
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.Contains(t, jrr.GetResultString(), "0x42")
	})
}

// Tests for the try-all-upstreams-per-execution behavior.
// The Forward() loop continues to the next upstream for retryable errors
// (server errors, missing data, timeouts, etc.) within the same execution.
// Only success and deterministic errors (client faults, execution reverts)
// return immediately. This means delays (emptyResultDelay, blockUnavailableDelay)
// naturally fire only after all upstreams have been tried in one execution.

func TestNetworkForward_TryAllUpstreams_FallbackWithinSameRound(t *testing.T) {
	t.Run("ServerError_FallsBackToSecondUpstream_NoRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rpc1Calls := 0
		rpc2Calls := 0

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(500).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "Internal server error"},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 5},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x42")

		// Server error on rpc1 → loop continues → rpc2 succeeds (same execution)
		assert.Equal(t, 1, rpc1Calls, "rpc1 should be called once")
		assert.Equal(t, 1, rpc2Calls, "rpc2 should be called once")
	})

	t.Run("EmptyResult_FallsBackToSecondUpstream_NoRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rpc1Calls := 0
		rpc2Calls := 0

		// rpc1 returns empty result for eth_getBlockByNumber (in MarkEmptyAsErrorMethods)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		// rpc2 returns actual block data
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number":     "0x100",
					"hash":       "0xabc",
					"parentHash": "0xdef",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 5},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)

		result, pErr := jrr.PeekStringByPath(ctx, "hash")
		require.NoError(t, pErr)
		assert.Equal(t, "0xabc", result)

		// rpc1 empty → loop continues → rpc2 returns data (same execution)
		assert.Equal(t, 1, rpc1Calls, "rpc1 should be called once")
		assert.Equal(t, 1, rpc2Calls, "rpc2 should be called once")
		assert.Equal(t, 0, resp.Retries(), "no retry needed — both tried in same execution")
	})
}

func TestNetworkForward_TryAllUpstreams_AllEmpty_DelayBetweenRounds(t *testing.T) {
	t.Run("AllUpstreamsEmpty_ReturnsEmptyForRetryPolicyEvaluation", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1Calls := 0

		// rpc1 returns null for eth_call. Empty results are success (nil error)
		// so the loop returns immediately. HandleIf checks EmptyResultAccept —
		// since eth_call is in the list, the empty result is accepted.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// eth_call with emptyResultAccept means empty results are accepted
		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:       3,
				EmptyResultAccept: []string{"eth_call"},
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		// eth_call is in emptyResultAccept, so empty result is accepted without error
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.True(t, jrr.IsResultEmptyish())

		t.Logf("Total calls: rpc1=%d", rpc1Calls)
		assert.GreaterOrEqual(t, rpc1Calls, 1, "rpc1 should be called at least once")
	})
}

func TestNetworkForward_TryAllUpstreams_ExecutionReverted_StopsImmediately(t *testing.T) {
	t.Run("ExecutionReverted_DoesNotTryNextUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should NOT be called

		rpc1Calls := 0

		// rpc1 returns execution reverted
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": 3, "message": "execution reverted", "data": "0x08c379a0"},
			})

		// rpc2 mock — should NOT be consumed
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 5},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		_, err := network.Forward(ctx, req)

		// Execution reverted is a deterministic error — returned immediately
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException))
		assert.Equal(t, 1, rpc1Calls, "only rpc1 should be called")
	})
}

func TestNetworkForward_TryAllUpstreams_ValidationError_ContinuesToNextUpstream(t *testing.T) {
	t.Run("ValidationError_TriesAllUpstreams_ReturnsValid", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rpc1Calls := 0
		rpc2Calls := 0

		// rpc1 returns missing trie node (gets normalized to MissingData)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "missing trie node"},
			})

		// rpc2 returns valid data
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 5},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x42")

		// MissingData on rpc1 → loop continues → rpc2 succeeds (same execution)
		assert.Equal(t, 1, rpc1Calls)
		assert.Equal(t, 1, rpc2Calls)
		assert.Equal(t, 0, resp.Retries(), "no retry needed — both tried in same execution")
		assert.Equal(t, 1, resp.Attempts(), "single execution attempt")
	})
}

func TestNetworkForward_TryAllUpstreams_AllServerErrors(t *testing.T) {
	t.Run("AllUpstreamsError_ReturnsErrorForFailsafeRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1Calls := 0
		rpc2Calls := 0

		// Both upstreams always return 500
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(500).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "Internal server error"},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(500).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "Internal server error"},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 2, Delay: common.Duration(10 * time.Millisecond)},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		_, err := network.Forward(ctx, req)

		// Both upstreams tried per round, error returned after max attempts
		require.Error(t, err)
		assert.GreaterOrEqual(t, rpc1Calls, 2, "rpc1 should be called at least twice (once per round)")
		assert.GreaterOrEqual(t, rpc2Calls, 2, "rpc2 should be called at least twice (once per round)")
	})
}

func TestNetworkForward_TryAllUpstreams_MixedErrorAndEmpty(t *testing.T) {
	t.Run("ServerErrorThenEmpty_PrefersEmptyOverError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		rpc1Calls := 0
		rpc2Calls := 0

		// rpc1 returns 500 error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(500).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "Internal server error"},
			})

		// rpc2 returns empty/null (valid response, but emptyish)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			// eth_call is in default EmptyResultAccept, so empty is accepted
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 3},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		// bestResp (empty from rpc2) is preferred over lastErr (500 from rpc1)
		require.NoError(t, err, "should return empty response, not 500 error")
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.True(t, jrr.IsResultEmptyish(), "should return emptyish result from rpc2")

		assert.Equal(t, 1, rpc1Calls, "rpc1 should be called once")
		assert.Equal(t, 1, rpc2Calls, "rpc2 should be called once")
		assert.Equal(t, 1, resp.Attempts(), "single execution — both tried in same round")
	})
}

func TestNetworkForward_TryAllUpstreams_HappyPathFirstUpstream(t *testing.T) {
	t.Run("FirstUpstreamSuccess_SecondNotCalled", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 mock should NOT be consumed

		rpc1Calls := 0

		// rpc1 returns valid non-empty success
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		// rpc2 mock should never be consumed
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x99",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 5},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.Contains(t, jrr.GetResultString(), "0x42")

		// Happy path: first upstream succeeds → loop returns immediately
		assert.Equal(t, 1, rpc1Calls, "only rpc1 should be called")
		assert.Equal(t, 0, resp.Retries())
		assert.Equal(t, 1, resp.Attempts())
	})
}

func TestNetworkForward_TryAllUpstreams_SingleUpstreamBackwardCompat(t *testing.T) {
	t.Run("SingleUpstream_ErrorThenSuccess_FailsafeRetries", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		callCount := 0

		// First call: 500 error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(500).
			Map(func(r *http.Response) *http.Response { callCount++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "server error"},
			})

		// Second call: success
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { callCount++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		// Only mock rpc2 for state poller (not for eth_call) so it doesn't interfere
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(500).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "server error"},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(10 * time.Millisecond)},
		)
		// rpc1 first — in round 1 it errors, rpc2 also errors → lastErr → failsafe retries
		// In round 2 rpc1 succeeds → happy path
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.Contains(t, jrr.GetResultString(), "0x42")

		// Round 1: rpc1 error + rpc2 error → retry. Round 2: rpc1 success.
		assert.Equal(t, 2, resp.Attempts(), "should take 2 attempts (round 1 all-error, round 2 success)")
		assert.GreaterOrEqual(t, callCount, 2, "rpc1 should be called at least twice")
	})
}

func TestNetworkForward_EmptyResultDelayForMissingDataError(t *testing.T) {
	t.Run("MissingDataError_UsesEmptyResultDelay", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Both upstreams persistently return "missing trie node"
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "missing trie node"},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "missing trie node"},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:      3,
				Delay:            common.Duration(10 * time.Millisecond),
				EmptyResultDelay: common.Duration(300 * time.Millisecond),
			},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		_, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		// All attempts exhaust with MissingData errors — expected.
		// With the fix, upstreams are re-tried each round (not gated), so the final
		// error is either ErrUpstreamsExhausted (wrapping) or ErrEndpointMissingData (raw).
		require.Error(t, err)
		assert.True(t,
			common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted) || common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
			"should fail after retries with exhausted or missing-data error, got: %v", err)

		// The key assertion: emptyResultDelay (300ms) applies between rounds for
		// MissingData errors (not the normal 10ms delay). With 3 maxAttempts we
		// get 2 retries × 300ms ≈ 600ms total wait, well above 2 × 10ms = 20ms.
		assert.GreaterOrEqual(t, elapsed.Milliseconds(), int64(500),
			"emptyResultDelay (300ms) should apply between rounds for MissingData errors, "+
				"expected ≥500ms but got %dms", elapsed.Milliseconds())
	})
}

// =====================================================================
// Tests for upstream re-selection across retry rounds
// These verify that upstreams returning empty/MissingData in round 1
// are available for re-selection in subsequent retry rounds.
// =====================================================================

func TestNetworkForward_UpstreamReselection_MissingDataSucceedsOnRetry(t *testing.T) {
	t.Run("BothUpstreamsMissingData_Round2_Rpc1Succeeds", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1Calls := 0
		rpc2Calls := 0

		// Round 1: rpc1 returns "missing trie node"
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "missing trie node"},
			})

		// Round 1: rpc2 returns "missing trie node"
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "missing trie node"},
			})

		// Round 2: rpc1 now has the data
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		// Round 2: rpc2 still missing (in case it's tried)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "missing trie node"},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:      3,
				Delay:            common.Duration(10 * time.Millisecond),
				EmptyResultDelay: common.Duration(10 * time.Millisecond),
			},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		// BUG (before fix): ErrorsByUpstream gate permanently blocks rpc1 & rpc2
		// after round 1, so round 2 can't select any upstream → ErrUpstreamsExhausted.
		// EXPECTED (after fix): rpc1 is re-selected in round 2 and returns "0x42".
		require.NoError(t, err, "should succeed on round 2 — rpc1 has data now")
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.Contains(t, jrr.GetResultString(), "0x42")

		assert.GreaterOrEqual(t, rpc1Calls, 2, "rpc1 should be called in both rounds")
		assert.Equal(t, 2, resp.Attempts(), "should take 2 attempts (round 1 all-error, round 2 success)")
	})
}

// Empty-result re-selection unit tests are in common/request_test.go:
// - TestUpstreamSelection_EmptyResponses_DontBlockReselection
// - TestUpstreamSelection_MissingDataError_DontBlockReselection

func TestNetworkForward_UpstreamReselection_WrongEmptyStillTracked(t *testing.T) {
	t.Run("Rpc1Empty_Rpc2HasData_ErrorsByUpstreamTracksEmpty", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// rpc1 returns empty/null
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		// rpc2 returns real data
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x42",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{MaxAttempts: 3},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.Contains(t, jrr.GetResultString(), "0x42")

		// Verify that ErrorsByUpstream still tracks that rpc1 returned empty.
		// This is important for the "wrong empty response" metric and error reporting.
		emptyCount := 0
		req.ErrorsByUpstream.Range(func(key, value interface{}) bool {
			if err, ok := value.(error); ok {
				if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
					emptyCount++
				}
			}
			return true
		})
		assert.Equal(t, 1, emptyCount,
			"ErrorsByUpstream should track exactly 1 upstream that returned empty (for wrong-empty metric)")
	})
}

func setupTestNetworkForMissingDataRetry(
	t *testing.T,
	ctx context.Context,
	directiveDefaults *common.DirectiveDefaultsConfig,
	retryConfig *common.RetryPolicyConfig,
) *Network {
	t.Helper()
	return setupTestNetworkWithRetryConfig(t, ctx, directiveDefaults, retryConfig)
}
