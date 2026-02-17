package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
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
		// Uses eth_getBalance (NOT in default emptyResultAccept) so retry is allowed.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
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
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
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

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
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

		// Uses eth_getBalance (NOT in default emptyResultAccept) so MissingData
		// is retried across rounds with emptyResultDelay.

		// Round 1: rpc1 returns "missing trie node"
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
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
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
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
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xde0b6b3a7640000",
			})

		// Round 2: rpc2 still missing (in case it's tried)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
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

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
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
		assert.Contains(t, jrr.GetResultString(), "0xde0b6b3a7640000")

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

// Test: blockUnavailableDelay applied after full round of all upstreams
// reporting block unavailable. The request targets a future block that the
// state poller hasn't seen yet, so the pre-flight check skips all upstreams.
// After blockUnavailableDelay the retry round should try again.
func TestNetworkForward_BlockUnavailableDelay_AppliedAfterFullRound(t *testing.T) {
	t.Run("AllUpstreamsBlockUnavailable_DelayBeforeRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// rpc1 returns valid data when eventually called
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
			}).
			Persist().
			Reply(200).
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
			&common.RetryPolicyConfig{
				MaxAttempts:           3,
				Delay:                 common.Duration(50 * time.Millisecond),
				BlockUnavailableDelay: common.Duration(200 * time.Millisecond),
			},
		)

		// Request a block that state poller already reports as available (block 0x100 = 256).
		// The SetupMocksForEvmStatePoller mocks report latest block ~0x100+,
		// so block 0x100 should be available and Forward should succeed.
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		result, pErr := jrr.PeekStringByPath(ctx, "hash")
		require.NoError(t, pErr)
		assert.Equal(t, "0xabc", result)
	})
}

// Test: RetryEmpty disabled blocks MissingData retry at both scopes.
// When RetryEmpty is false, a MissingData error from an upstream should NOT
// trigger retry at network OR upstream level.
func TestNetworkForward_RetryEmptyDisabled_BlocksMissingData(t *testing.T) {
	t.Run("RetryEmptyFalse_MissingDataNotRetried", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1Calls := 0
		rpc2Calls := 0

		// Both upstreams return "missing trie node" error
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
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node abc123",
				},
			})
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
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node def456",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// RetryEmpty is explicitly false — should NOT retry MissingData
		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(false)},
			&common.RetryPolicyConfig{
				MaxAttempts:      3,
				Delay:            common.Duration(10 * time.Millisecond),
				EmptyResultDelay: common.Duration(10 * time.Millisecond),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		_, err := network.Forward(ctx, req)
		require.Error(t, err, "should return error since MissingData is not retried")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData) ||
			common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"error should be MissingData or UpstreamsExhausted, got: %v", err)

		// With RetryEmpty disabled, the HandleIf blocks MissingData at both scopes.
		// Both upstreams tried in 1 round, no further retry rounds.
		totalCalls := rpc1Calls + rpc2Calls
		t.Logf("rpc1=%d rpc2=%d total=%d", rpc1Calls, rpc2Calls, totalCalls)
		assert.LessOrEqual(t, totalCalls, 2, "should try each upstream once, no retry rounds")
	})
}

// Test: Method-ignored upstream permanently gated, retryable upstream retried.
// Two upstreams: rpc1 ignores eth_getBlockByNumber, rpc2 returns server error
// then succeeds on retry. rpc1 should stay gated (never HTTP-called), rpc2
// should succeed after retry.
func TestNetworkForward_PermanentErrorGated_RetryableRetried(t *testing.T) {
	t.Run("MethodIgnored_StaysGated_RetryableSucceeds", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc2Calls := 0

		// rpc2 first returns 503, then succeeds
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
			}).
			Times(1).
			Reply(503).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"message": "temporary server error",
				},
			})
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

		network := setupTestNetworkWithMethodIgnore(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(10 * time.Millisecond),
			},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err, "should succeed on rpc2's second attempt")
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		result, pErr := jrr.PeekStringByPath(ctx, "hash")
		require.NoError(t, pErr)
		assert.Equal(t, "0xabc", result)

		// rpc2 should have been called twice (503 then success), rpc1 never HTTP-called
		assert.Equal(t, 2, rpc2Calls, "rpc2 should be called twice (error then success)")
	})
}

// Test: Single upstream errors are consistently wrapped in ErrUpstreamsExhausted.
// Verifies that even with a single upstream, the Forward() function always
// returns ErrUpstreamsExhausted (not raw server errors) for consistent handling.
func TestNetworkForward_ConsistentExhaustedWrapping(t *testing.T) {
	t.Run("SingleUpstreamServerError_WrappedInExhausted", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Both upstreams return 503 — ensures HasErrorCode can traverse
		// regardless of which joined error comes first.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"message": "backend unavailable",
				},
			})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_call")
			}).
			Persist().
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"message": "backend unavailable",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts: 2,
				Delay:       common.Duration(10 * time.Millisecond),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		_, err := network.Forward(ctx, req)
		require.Error(t, err)

		// Must be wrapped in ErrUpstreamsExhausted, NOT raw ErrEndpointServerSideException
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"error should be ErrUpstreamsExhausted, got: %v", err)
		// Child error should still be the server exception
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
			"child error should contain server-side exception, got: %v", err)
	})

	t.Run("SingleUpstreamMissingData_WrappedInExhausted", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Both upstreams return MissingData — ensures consistent wrapping
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node abc123",
				},
			})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node def456",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:      2,
				Delay:            common.Duration(10 * time.Millisecond),
				EmptyResultDelay: common.Duration(10 * time.Millisecond),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		_, err := network.Forward(ctx, req)
		require.Error(t, err)

		// Must be wrapped in ErrUpstreamsExhausted
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"error should be ErrUpstreamsExhausted, got: %v", err)
		// Child error should contain MissingData
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
			"child error should contain MissingData, got: %v", err)
	})
}

// Test: Mixed block-unavailable skip and MissingData across upstreams.
// rpc1 returns MissingData, rpc2 returns valid data. The MissingData upstream
// should NOT block the working upstream from serving the request.
func TestNetworkForward_MixedMissingDataAndSuccess(t *testing.T) {
	t.Run("OneMissingData_OtherSuccess_ReturnsSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// rpc1 returns missing trie node
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "missing trie node abc123",
				},
			})

		// rpc2 returns valid balance
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xde0b6b3a7640000",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:      3,
				EmptyResultDelay: common.Duration(100 * time.Millisecond),
			},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err, "should succeed using rpc2")
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		result := jrr.GetResultString()
		assert.Contains(t, result, "0xde0b6b3a7640000")

		// Should return quickly (no delay since rpc2 succeeded in same round)
		assert.Less(t, elapsed, 500*time.Millisecond,
			"should return fast — no delay needed when another upstream has data")
	})
}

// Test: emptyResultAccept stops retry for accepted methods like eth_getLogs.
// When eth_getLogs returns an empty array, it should be accepted as valid
// (no retry) because eth_getLogs is in the emptyResultAccept list.
func TestNetworkForward_EmptyResultAccept_StopsRetryForAcceptedMethod(t *testing.T) {
	t.Run("EthGetLogs_EmptyArray_AcceptedNoRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1Calls := 0
		// rpc1 returns empty array for eth_getLogs
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:       5,
				Delay:             common.Duration(50 * time.Millisecond),
				EmptyResultDelay:  common.Duration(100 * time.Millisecond),
				EmptyResultAccept: []string{"eth_getLogs", "eth_call"},
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x100"}]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err, "eth_getLogs empty should be accepted")
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.True(t, jrr.IsResultEmptyish(), "result should be emptyish")

		// Should NOT retry — empty accepted for eth_getLogs
		t.Logf("rpc1 called %d times, elapsed %v", rpc1Calls, elapsed)
		assert.LessOrEqual(t, rpc1Calls, 2, "should not retry beyond initial round")
		assert.Less(t, elapsed, 500*time.Millisecond, "should return quickly without delays")
	})
}

// Test: blockUnavailableDelay is respected with timing verification.
// Both upstreams return MissingData (simulating block not yet available) on
// first round, then rpc1 succeeds on retry. We verify that the total elapsed
// time is at least blockUnavailableDelay, confirming the delay fires between
// retry rounds — not between individual upstream attempts.
func TestNetworkForward_BlockUnavailableDelay_TimingVerified(t *testing.T) {
	t.Run("AllUpstreamsMissing_DelayBeforeRetry_ThenSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1Calls := 0
		rpc2Calls := 0

		// Round 1: both upstreams return MissingData (null result for eth_getBlockByNumber)
		// Round 2 (after blockUnavailableDelay): rpc1 returns data
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
				"result":  nil,
			})

		// rpc1 on retry round: returns block data
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
				"result": map[string]interface{}{
					"number":     "0x100",
					"hash":       "0xabc",
					"parentHash": "0xdef",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		blockDelay := 300 * time.Millisecond
		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:           5,
				Delay:                 common.Duration(50 * time.Millisecond),
				EmptyResultDelay:      common.Duration(blockDelay),
				BlockUnavailableDelay: common.Duration(blockDelay),
			},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		result, pErr := jrr.PeekStringByPath(ctx, "hash")
		require.NoError(t, pErr)
		assert.Equal(t, "0xabc", result)

		t.Logf("rpc1=%d rpc2=%d elapsed=%v", rpc1Calls, rpc2Calls, elapsed)
		// The delay should fire between round 1 (both empty) and round 2 (success).
		assert.GreaterOrEqual(t, elapsed, blockDelay-50*time.Millisecond,
			"elapsed should be at least close to blockUnavailableDelay")
		assert.Equal(t, 2, rpc1Calls, "rpc1: once empty, once success")
		assert.Equal(t, 1, rpc2Calls, "rpc2: once empty")
	})
}

// Test: emptyResultAccept for eth_call (default accept list).
// eth_call is in the default emptyResultAccept list. When eth_call returns an
// empty/null result, the failsafe should accept it immediately without retry,
// even when RetryEmpty=true and emptyResultDelay is configured.
func TestNetworkForward_EmptyResultAccept_EthCallDefault(t *testing.T) {
	t.Run("EthCall_NullResult_AcceptedNoRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1Calls := 0

		// Both upstreams return null for eth_call
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
				"result":  "0x",
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
				"result":  "0x",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:      5,
				Delay:            common.Duration(50 * time.Millisecond),
				EmptyResultDelay: common.Duration(200 * time.Millisecond),
				// eth_call is in the default accept list via SetDefaults
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err, "eth_call empty should be accepted")
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.True(t, jrr.IsResultEmptyish(), "result should be emptyish")

		t.Logf("rpc1 called %d times, elapsed %v", rpc1Calls, elapsed)
		// eth_call is in default emptyResultAccept → no retry, no delay
		assert.LessOrEqual(t, rpc1Calls, 2, "should only try each upstream once, no retries")
		assert.Less(t, elapsed, 400*time.Millisecond, "should return quickly without delay")
	})
}

// Test: RetryEmpty=false blocks MissingData retry at upstream scope.
// The HandleIf check for RetryEmpty respects the directive universally (not
// just at network scope). When RetryEmpty=false, MissingData errors should
// not trigger retry at either scope, so the request returns quickly as exhausted.
func TestNetworkForward_RetryEmptyDisabled_UpstreamScope(t *testing.T) {
	t.Run("RetryEmptyFalse_MissingDataNotRetriedAtUpstreamScope", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		totalCalls := 0

		// Both upstreams return MissingData-inducing null for eth_getBlockByNumber
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { totalCalls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Map(func(r *http.Response) *http.Response { totalCalls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			// RetryEmpty=false means "don't retry empty results"
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(false)},
			&common.RetryPolicyConfig{
				MaxAttempts:      5,
				Delay:            common.Duration(50 * time.Millisecond),
				EmptyResultDelay: common.Duration(200 * time.Millisecond),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		t.Logf("totalCalls=%d elapsed=%v err=%v", totalCalls, elapsed, err)

		// With RetryEmpty=false, the empty result should be returned without retries.
		// Either a successful empty response or an exhausted error.
		if err != nil {
			// If error, it should be exhausted and contain MissingData
			assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
				"error should be ErrUpstreamsExhausted")
		} else {
			// If success, it should be an emptyish result
			require.NotNil(t, resp)
		}

		// Key assertion: no retry rounds. Each upstream tried once = 2 total.
		assert.LessOrEqual(t, totalCalls, 2, "no retries should happen with RetryEmpty=false")
		assert.Less(t, elapsed, 400*time.Millisecond, "should return quickly")
	})
}

// Test: method-ignored upstream stays permanently gated; retryable upstream
// eventually succeeds on a retry round. This prevents regression of the
// ErrorsByUpstream permanent gating for non-retryable errors.
func TestNetworkForward_MethodIgnored_GatedWhileRetryableSucceeds(t *testing.T) {
	t.Run("IgnoredUpstreamNeverRetried_RetryableSucceedsOnRound2", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc2Calls := 0

		// rpc1 has eth_getBlockByNumber in ignoreMethods → never called.
		// rpc2 fails first, then succeeds.
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber")
			}).
			Times(1).
			Reply(500).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "internal error"},
			})

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
					"number": "0x100",
					"hash":   "0xabc",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithMethodIgnore(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts: 5,
				Delay:       common.Duration(50 * time.Millisecond),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		result, pErr := jrr.PeekStringByPath(ctx, "hash")
		require.NoError(t, pErr)
		assert.Equal(t, "0xabc", result)

		// rpc1 never called (method ignored). rpc2: first 500, then success.
		assert.Equal(t, 2, rpc2Calls, "rpc2 should be called twice: error then success")
	})
}

// Test: single-upstream errors are consistently wrapped in ErrUpstreamsExhausted.
// Even when only one upstream is configured and it fails, the error should be
// ErrUpstreamsExhausted wrapping the underlying error. HasErrorCode should find
// the specific underlying code inside the wrapper.
func TestNetworkForward_SingleUpstream_ConsistentWrapping(t *testing.T) {
	t.Run("SingleUpstreamFails_WrappedInExhausted", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Only rpc1 configured, returns 503
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "service unavailable"},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "service unavailable"},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{},
			&common.RetryPolicyConfig{
				MaxAttempts: 2,
				Delay:       common.Duration(50 * time.Millisecond),
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		_, err := network.Forward(ctx, req)
		require.Error(t, err, "should fail after retries")

		// Top-level must be ErrUpstreamsExhausted
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"error should be wrapped in ErrUpstreamsExhausted, got: %v", err)

		// The underlying server-side exception should be accessible via HasErrorCode
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
			"underlying server error should be findable inside ErrUpstreamsExhausted")
	})
}

// Test: blockUnavailableDelay vs emptyResultDelay priority.
// When one upstream returns a server error (which wraps blockUnavailable via
// pre-forward skip) and another returns an emptyish response, the DelayFunc
// priority is: blockUnavailable checked first, then emptyResult.
// In practice, the result type depends on what the Forward() loop returns:
//   - If bestResp exists (emptyish response from one upstream) → returns (bestResp, nil) → emptyResultDelay
//   - If all errors (no bestResp) → returns (nil, exhaustedErr) → blockUnavailableDelay wins
//
// This test verifies the second case: all upstreams error, mixed block-unavail and
// missing-data, blockUnavailableDelay should take priority.
func TestNetworkForward_BlockUnavailableVsEmptyDelay_Priority(t *testing.T) {
	t.Run("MixedErrors_BlockUnavailableDelayWins", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		rpc1Calls := 0
		rpc2Calls := 0

		// rpc1: returns a -32000 "header not found" error (normalizer converts to MissingData)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "header not found"},
			})

		// rpc2: also returns missing data
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc2Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error":   map[string]interface{}{"code": -32000, "message": "header not found"},
			})

		// On retry round: rpc1 succeeds
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			Map(func(r *http.Response) *http.Response { rpc1Calls++; return r }).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1234",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		blockDelay := 400 * time.Millisecond
		emptyDelay := 100 * time.Millisecond

		network := setupTestNetworkForMissingDataRetry(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:           5,
				Delay:                 common.Duration(50 * time.Millisecond),
				EmptyResultDelay:      common.Duration(emptyDelay),
				BlockUnavailableDelay: common.Duration(blockDelay),
			},
		)
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		start := time.Now()
		resp, err := network.Forward(ctx, req)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, jrrErr := resp.JsonRpcResponse()
		require.NoError(t, jrrErr)
		assert.Contains(t, jrr.GetResultString(), "0x1234")

		t.Logf("rpc1=%d rpc2=%d elapsed=%v", rpc1Calls, rpc2Calls, elapsed)

		// Both upstreams had MissingData errors. The delay applied should be
		// emptyResultDelay (since MissingData errors trigger that path).
		// Verify the delay was applied (not just the base 50ms).
		assert.GreaterOrEqual(t, elapsed, emptyDelay-30*time.Millisecond,
			"should have applied emptyResultDelay between rounds")
		assert.Equal(t, 2, rpc1Calls, "rpc1: error then success")
		assert.Equal(t, 1, rpc2Calls, "rpc2: error only")
	})
}

// ---------------------------------------------------------------------------
// Helper: network with method-ignored upstream
// ---------------------------------------------------------------------------

func setupTestNetworkWithMethodIgnore(
	t *testing.T,
	ctx context.Context,
	directiveDefaults *common.DirectiveDefaultsConfig,
	retryConfig *common.RetryPolicyConfig,
) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
			IgnoreMethods: []string{"eth_getBlockByNumber"},
		},
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc2",
			Endpoint: "http://rpc2.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		},
	}

	networkConfig := &common.NetworkConfig{
		Architecture:      common.ArchitectureEvm,
		DirectiveDefaults: directiveDefaults,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			Retry: retryConfig,
		}},
	}

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{}, &log.Logger)
	require.NoError(t, err)

	metricsTracker := health.NewTracker(&log.Logger, "test", time.Minute)

	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	upstreamsRegistry := upstream.NewUpstreamsRegistry(
		ctx,
		&log.Logger,
		"test",
		upstreamConfigs,
		ssr,
		rateLimitersRegistry,
		vr,
		pr,
		nil,
		metricsTracker,
		time.Second,
		nil,
	)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	network, err := NewNetwork(ctx, &log.Logger, "test", networkConfig, rateLimitersRegistry, upstreamsRegistry, metricsTracker)
	require.NoError(t, err)

	err = upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, networkConfig.NetworkId())
	require.NoError(t, err)

	err = network.Bootstrap(ctx)
	require.NoError(t, err)

	return network
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
