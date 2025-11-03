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

func TestNetworkFailsafe_RetryEmpty(t *testing.T) {
	t.Run("RetryEmptyFalse_TransactionReceipt_NoRetries", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should not be called

		// First upstream returns empty (null) receipt
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil, // Empty receipt
			})

		// Second upstream should NOT be called due to retryEmpty=false
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"transactionHash": "0x123",
					"blockNumber":     "0x100",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Network with retryEmpty=false directive default
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(false),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 3, // Would allow retries if not blocked by retryEmpty
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xed9b8902d8c588112481f5b4d0011b2ff30a98587862a527984fae417649cbed"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
		if dirs := req.Directives(); dirs != nil {
			dirs.UseUpstream = "rpc1"
		}

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Should get the empty result from rpc1, no retries
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.True(t, jrr.IsResultEmptyish())

		// Verify no retries in response headers
		assert.Equal(t, 0, resp.Retries())
		assert.Equal(t, 1, resp.Attempts())
	})

	t.Run("RetryEmptyTrue_TransactionReceipt_Retries", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0) // Both mocks should be consumed

		// First upstream returns empty (null) receipt
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil, // Empty receipt
			})

			// Second upstream returns actual receipt (will be used on retry)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"transactionHash": "0xed9b8902d8c588112481f5b4d0011b2ff30a98587862a527984fae417649cbed",
					"blockNumber":     "0x100",
					"status":          "0x1",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Network with retryEmpty=true directive default
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xed9b8902d8c588112481f5b4d0011b2ff30a98587862a527984fae417649cbed"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
		if dirs := req.Directives(); dirs != nil {
			// Allow both upstreams (no UseUpstream) so retry can hit rpc2
			dirs.UseUpstream = ""
		}

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Should get the non-empty result from rpc2 after retry
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.False(t, jrr.IsResultEmptyish())

		result, err := jrr.PeekStringByPath(ctx, "transactionHash")
		require.NoError(t, err)
		assert.Equal(t, "0xed9b8902d8c588112481f5b4d0011b2ff30a98587862a527984fae417649cbed", result)
	})

	t.Run("RetryEmptyTrue_GetTransactionByHash_Retries", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// First upstream returns null (no tx found)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil, // Transaction not found
			})

			// Second upstream returns actual transaction
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"hash":        "0x123",
					"blockNumber": "0x100",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Network with retryEmpty=true
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts:       3,
				EmptyResultIgnore: []string{"eth_getLogs", "eth_call"}, // eth_getTransactionByHash not in list
			},
		)

		// Ensure deterministic upstream ordering: rpc1 tried first, then rpc2 on retry
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["0x123"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Should get the transaction from rpc2 after retry
		// eth_getTransactionByHash goes through markUnexpectedEmpty hook, so empty becomes error and triggers retry
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.False(t, jrr.IsResultEmptyish())

		result, err := jrr.PeekStringByPath(ctx, "hash")
		require.NoError(t, err)
		assert.Equal(t, "0x123", result)

		// Verify a retry occurred
		assert.Equal(t, 1, resp.Retries())
		assert.Equal(t, 2, resp.Attempts())
	})

	t.Run("RetryEmptyTrue_IgnoreIncludesReceipt_NoRetry", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should not be called because method is ignored

		// rpc1 returns empty receipt
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		// rpc2 would return non-empty if retried (but should not be hit)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"transactionHash": "0xabc",
					"blockNumber":     "0x100",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// retryEmpty=true but ignore list includes receipt
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
			&common.RetryPolicyConfig{
				MaxAttempts:       3,
				EmptyResultIgnore: []string{"eth_getTransactionReceipt"},
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0x123"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
		// Force upstream order determinism for this request only
		if dirs := req.Directives(); dirs != nil {
			dirs.UseUpstream = "rpc1"
		}

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.True(t, jrr.IsResultEmptyish())
	})

	t.Run("RetryEmptyFalse_EthLogs_NoRetries", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should not be called

		// First upstream returns empty logs array
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getLogs")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{}, // Empty logs
			})

		// Second upstream should NOT be called
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getLogs")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []interface{}{
					map[string]interface{}{
						"address": "0x123",
						"topics":  []string{"0xabc"},
					},
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Network with retryEmpty=false
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(false),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
		if dirs := req.Directives(); dirs != nil {
			dirs.UseUpstream = "rpc1"
		}

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Should get the empty result from rpc1, no retries
		// Note: eth_getLogs doesn't go through markUnexpectedEmpty hook
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.Equal(t, `[]`, jrr.GetResultString())

		// Verify no retries
		assert.Equal(t, 0, resp.Retries())
		assert.Equal(t, 1, resp.Attempts())
	})

	t.Run("HeaderOverridesDefaults_RetryEmptyFalse", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should not be called

		// First upstream returns empty transaction receipt
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil, // Empty receipt
			})

		// Second upstream should NOT be called
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"transactionHash": "0x123",
					"blockNumber":     "0x100",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Network with retryEmpty=true as default
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true), // Default is true
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0x123"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
		if dirs := req.Directives(); dirs != nil {
			dirs.UseUpstream = "rpc1"
		}

		// Override with header
		headers := http.Header{
			"X-ERPC-Retry-Empty": []string{"false"},
		}
		req.EnrichFromHttp(headers, nil, common.UserAgentTrackingModeSimplified)

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Should get the empty result from rpc1, no retries due to header override
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.True(t, jrr.IsResultEmptyish())
	})

	t.Run("MaxAttemptsReached_StopsRetrying", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 mock may remain pending due to MaxAttempts

		// Both upstreams return empty (but we may hit max attempts before trying rpc2)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Network with retryEmpty=true and emptyResultMaxAttempts=2
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts:            3,
				EmptyResultMaxAttempts: 2, // Cap empty retries at 2 attempts total
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0x123"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
		if dirs := req.Directives(); dirs != nil {
			dirs.UseUpstream = "rpc1"
		}

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Should get empty result after hitting emptyResultMaxAttempts
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.True(t, jrr.IsResultEmptyish())
	})

	t.Run("NonEmptyResponse_NoRetries", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should not be called

		// First upstream returns non-empty receipt
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"transactionHash": "0x123",
					"blockNumber":     "0x100",
					"status":          "0x1",
				},
			})

		// Second upstream should NOT be called
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionReceipt")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"transactionHash": "0x456",
					"blockNumber":     "0x200",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Network with retryEmpty=true (shouldn't matter for non-empty)
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(true),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0x123"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
		if dirs := req.Directives(); dirs != nil {
			dirs.UseUpstream = "rpc1"
		}

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Should get the non-empty result from rpc1, no retries needed
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.False(t, jrr.IsResultEmptyish())

		result, err := jrr.PeekStringByPath(ctx, "transactionHash")
		require.NoError(t, err)
		assert.Equal(t, "0x123", result)

		// Verify no retries
		assert.Equal(t, 0, resp.Retries())
		assert.Equal(t, 1, resp.Attempts())
	})

	t.Run("RetryEmptyFalse_GetBlockByNumber_NoRetries", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// First upstream returns null block
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber")
			}).
			Times(1).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil, // Block doesn't exist yet
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Network with retryEmpty=false
		network := setupTestNetworkWithRetryConfig(t, ctx,
			&common.DirectiveDefaultsConfig{
				RetryEmpty: util.BoolPtr(false),
			},
			&common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		)

		// Ensure deterministic upstream ordering: rpc1 first
		upstream.ReorderUpstreams(network.upstreamsRegistry, "rpc1", "rpc2")

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",false]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)

		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Should get null result, no retries
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Nil(t, jrr.Error)
		assert.True(t, jrr.IsResultEmptyish())

		// Verify no retries
		assert.Equal(t, 0, resp.Retries())
		assert.Equal(t, 1, resp.Attempts())
	})
}

// Helper function to setup a test network with retry configuration
func setupTestNetworkWithRetryConfig(t *testing.T, ctx context.Context, directiveDefaults *common.DirectiveDefaultsConfig, retryConfig *common.RetryPolicyConfig) *Network {
	t.Helper()

	upstreamConfigs := []*common.UpstreamConfig{
		{
			Type:     common.UpstreamTypeEvm,
			Id:       "rpc1",
			Endpoint: "http://rpc1.localhost",
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
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

	rateLimitersRegistry, err := upstream.NewRateLimitersRegistry(&common.RateLimiterConfig{}, &log.Logger)
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

	upstream.ReorderUpstreams(upstreamsRegistry)

	return network
}
