package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"

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

		assert.Equal(t, 2, totalCalls, "Both upstreams should be tried")
		assert.Equal(t, 1, rpc1CallCount, "rpc1 should be called once")
		assert.Equal(t, 1, rpc2CallCount, "rpc2 should be called once")
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
				EmptyResultIgnore: []string{},
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

func setupTestNetworkForMissingDataRetry(
	t *testing.T,
	ctx context.Context,
	directiveDefaults *common.DirectiveDefaultsConfig,
	retryConfig *common.RetryPolicyConfig,
) *Network {
	t.Helper()
	return setupTestNetworkWithRetryConfig(t, ctx, directiveDefaults, retryConfig)
}
