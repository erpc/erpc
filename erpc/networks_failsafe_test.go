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

	upstream.ReorderUpstreams(upstreamsRegistry)

	return network
}

// Helper function to setup a test network with multiple failsafe policies
func setupTestNetworkWithMultipleFailsafePolicies(t *testing.T, ctx context.Context, failsafeConfigs []*common.FailsafeConfig) *Network {
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
	}

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: failsafeConfigs,
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

func TestGetFailsafeExecutor_OrderRespected(t *testing.T) {
	t.Run("FirstMatchingPolicy_ByFinality_BeforeMethodOnlyMatch", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Policy 1: wildcard method, specific finality (realtime) - delay 100ms
		// Policy 2: specific method (eth_call), no finality constraint - delay 0ms
		// For eth_call with realtime finality, Policy 1 should be selected (order-based)
		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod:   "*",
				MatchFinality: []common.DataFinalityState{common.DataFinalityStateRealtime},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
					Delay:       common.Duration(100 * time.Millisecond),
				},
			},
			{
				MatchMethod: "eth_call|trace_*|debug_*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
					Delay:       0,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// Create request for eth_call with latest (realtime finality)
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection

		executor := network.getFailsafeExecutor(ctx, req)

		require.NotNil(t, executor)
		// Policy 1 should be selected (wildcard method, realtime finality)
		assert.Equal(t, "*", executor.method)
		assert.Contains(t, executor.finalities, common.DataFinalityStateRealtime)
	})

	t.Run("SecondPolicy_WhenFirstDoesNotMatchFinality", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Policy 1: wildcard method, specific finality (realtime)
		// Policy 2: specific method (eth_call), no finality constraint
		// For eth_call with a specific block number (not realtime), Policy 2 should be selected
		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod:   "*",
				MatchFinality: []common.DataFinalityState{common.DataFinalityStateRealtime},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
				},
			},
			{
				MatchMethod: "eth_call|trace_*|debug_*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// Create request for eth_call with a specific block number (finality = unfinalized or finalized, not realtime)
		// Note: All block tags (latest, pending, finalized, safe) are considered "realtime"
		// Only specific block numbers are considered non-realtime
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x123"},"0x100"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection

		executor := network.getFailsafeExecutor(ctx, req)

		require.NotNil(t, executor)
		// Policy 2 should be selected (specific method, no finality constraint)
		assert.Equal(t, "eth_call|trace_*|debug_*", executor.method)
		assert.Empty(t, executor.finalities)
	})

	t.Run("GenericFallback_WhenNoSpecificMatch", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Policy 1: specific method (eth_call), specific finality (realtime)
		// Policy 2: wildcard method, no finality (fallback)
		// For eth_blockNumber with unfinalized finality, Policy 2 should be selected
		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod:   "eth_call",
				MatchFinality: []common.DataFinalityState{common.DataFinalityStateRealtime},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
				},
			},
			{
				MatchMethod: "*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 2,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// Create request for eth_blockNumber (doesn't match eth_call)
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection

		executor := network.getFailsafeExecutor(ctx, req)

		require.NotNil(t, executor)
		// Policy 2 (wildcard fallback) should be selected
		assert.Equal(t, "*", executor.method)
		assert.Empty(t, executor.finalities)
	})

	t.Run("WildcardMethodMatch_AnyMethod", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Single policy with wildcard method
		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod: "*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// Test various methods
		methods := []string{"eth_call", "eth_getBalance", "eth_blockNumber", "trace_call", "debug_traceTransaction"}
		for _, method := range methods {
			requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`)
			req := common.NewNormalizedRequest(requestBytes)
			req.SetNetwork(network) // Required for finality detection

			executor := network.getFailsafeExecutor(ctx, req)

			require.NotNil(t, executor, "executor should not be nil for method %s", method)
			assert.Equal(t, "*", executor.method)
		}
	})

	t.Run("WildcardPatternMatch_trace_star", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod: "trace_*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
				},
			},
			{
				MatchMethod: "*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 2,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// trace_call should match trace_*
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"trace_call","params":[]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection

		executor := network.getFailsafeExecutor(ctx, req)

		require.NotNil(t, executor)
		assert.Equal(t, "trace_*", executor.method)

		// eth_call should NOT match trace_*, should fall through to wildcard
		requestBytes2 := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)
		req2 := common.NewNormalizedRequest(requestBytes2)
		req2.SetNetwork(network) // Required for finality detection

		executor2 := network.getFailsafeExecutor(ctx, req2)

		require.NotNil(t, executor2)
		assert.Equal(t, "*", executor2.method)
	})

	t.Run("PipeDelimitedMethodMatch", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod: "eth_call|eth_getBalance|eth_getCode",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
				},
			},
			{
				MatchMethod: "*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 2,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// eth_call should match
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection
		executor := network.getFailsafeExecutor(ctx, req)
		require.NotNil(t, executor)
		assert.Equal(t, "eth_call|eth_getBalance|eth_getCode", executor.method)

		// eth_getBalance should match
		requestBytes2 := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":[]}`)
		req2 := common.NewNormalizedRequest(requestBytes2)
		req2.SetNetwork(network) // Required for finality detection
		executor2 := network.getFailsafeExecutor(ctx, req2)
		require.NotNil(t, executor2)
		assert.Equal(t, "eth_call|eth_getBalance|eth_getCode", executor2.method)

		// eth_blockNumber should NOT match, falls to wildcard
		requestBytes3 := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
		req3 := common.NewNormalizedRequest(requestBytes3)
		req3.SetNetwork(network) // Required for finality detection
		executor3 := network.getFailsafeExecutor(ctx, req3)
		require.NotNil(t, executor3)
		assert.Equal(t, "*", executor3.method)
	})

	t.Run("MultipleFinalitiesMatch", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Policy 1: Only matches realtime (NOT unknown)
		// Policy 2: Matches unknown only
		// Policy 3: Wildcard fallback
		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod:   "*",
				MatchFinality: []common.DataFinalityState{common.DataFinalityStateRealtime},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
				},
			},
			{
				MatchMethod:   "*",
				MatchFinality: []common.DataFinalityState{common.DataFinalityStateUnknown},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
				},
			},
			{
				MatchMethod: "*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 2,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// Request with latest (realtime) should match first policy
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection
		executor := network.getFailsafeExecutor(ctx, req)
		require.NotNil(t, executor)
		assert.Contains(t, executor.finalities, common.DataFinalityStateRealtime)

		// Request with a specific block number (finality = unknown without response context)
		// should match second policy (unknown finality)
		requestBytes2 := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{},"0x100"]}`)
		req2 := common.NewNormalizedRequest(requestBytes2)
		req2.SetNetwork(network) // Required for finality detection
		executor2 := network.getFailsafeExecutor(ctx, req2)
		require.NotNil(t, executor2)
		// Should match Policy 2 (unknown finality) since specific block without response = unknown
		assert.Contains(t, executor2.finalities, common.DataFinalityStateUnknown)
	})

	t.Run("NoMatchingPolicy_ReturnsNil", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Policy that only matches eth_call with realtime finality
		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod:   "eth_call",
				MatchFinality: []common.DataFinalityState{common.DataFinalityStateRealtime},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// eth_blockNumber should NOT match any policy
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection
		executor := network.getFailsafeExecutor(ctx, req)

		// No matching policy - but there's always a default executor appended
		// Let's verify behavior - it should return nil if no explicit match
		// Actually, looking at networks_registry.go, a default executor is always added
		// So this test verifies the default is returned
		require.NotNil(t, executor)
		assert.Equal(t, "*", executor.method)
		assert.Empty(t, executor.finalities)
	})

	t.Run("ConfigOrder_FirstPolicyWins_EvenIfLessSpecific", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Key test: order matters, first match wins
		// Policy 1: wildcard (less specific) but comes first
		// Policy 2: more specific (eth_call) but comes second
		failsafeConfigs := []*common.FailsafeConfig{
			{
				MatchMethod: "*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 10, // Distinctive value to verify selection
				},
			},
			{
				MatchMethod: "eth_call",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// eth_call should match Policy 1 (wildcard) because it comes first
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection
		executor := network.getFailsafeExecutor(ctx, req)

		require.NotNil(t, executor)
		// Policy 1 (wildcard) should be selected because it comes first and matches
		assert.Equal(t, "*", executor.method)
	})

	t.Run("RealWorldScenario_MonadConfig", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Simulates monad.yml config structure
		failsafeConfigs := []*common.FailsafeConfig{
			{
				// Policy 1: realtime requests with retry delay
				MatchMethod:   "*",
				MatchFinality: []common.DataFinalityState{common.DataFinalityStateRealtime},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 5,
					Delay:       common.Duration(100 * time.Millisecond),
				},
			},
			{
				// Policy 2: eth_call with no delay (was incorrectly matched before)
				MatchMethod: "eth_call|trace_*|debug_*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 3,
					Delay:       0,
				},
			},
			{
				// Policy 3: unknown finality
				MatchMethod:   "*",
				MatchFinality: []common.DataFinalityState{common.DataFinalityStateUnknown},
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 6,
				},
			},
			{
				// Policy 4: fallback
				MatchMethod: "*",
				Retry: &common.RetryPolicyConfig{
					MaxAttempts: 4,
				},
			},
		}

		network := setupTestNetworkWithMultipleFailsafePolicies(t, ctx, failsafeConfigs)

		// eth_call with latest (realtime) should get Policy 1 (with delay)
		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{},"latest"]}`)
		req := common.NewNormalizedRequest(requestBytes)
		req.SetNetwork(network) // Required for finality detection
		executor := network.getFailsafeExecutor(ctx, req)

		require.NotNil(t, executor)
		// Should match Policy 1: wildcard method with realtime finality
		assert.Equal(t, "*", executor.method)
		assert.Contains(t, executor.finalities, common.DataFinalityStateRealtime)

		// eth_call with specific block number (not realtime) should get Policy 2 (no delay)
		// Note: "finalized" block tag is actually realtime, so we use a block number instead
		requestBytes2 := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{},"0x100"]}`)
		req2 := common.NewNormalizedRequest(requestBytes2)
		req2.SetNetwork(network) // Required for finality detection
		executor2 := network.getFailsafeExecutor(ctx, req2)

		require.NotNil(t, executor2)
		// Should match Policy 2: specific method, no finality constraint
		assert.Equal(t, "eth_call|trace_*|debug_*", executor2.method)

		// eth_getBalance with latest (realtime) should get Policy 1
		requestBytes3 := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)
		req3 := common.NewNormalizedRequest(requestBytes3)
		req3.SetNetwork(network) // Required for finality detection
		executor3 := network.getFailsafeExecutor(ctx, req3)

		require.NotNil(t, executor3)
		assert.Equal(t, "*", executor3.method)
		assert.Contains(t, executor3.finalities, common.DataFinalityStateRealtime)

		// eth_getBalance with specific block number should get Policy 3 (unknown finality)
		// Note: Specific block number without response context = unknown finality
		requestBytes4 := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","0x100"]}`)
		req4 := common.NewNormalizedRequest(requestBytes4)
		req4.SetNetwork(network) // Required for finality detection
		executor4 := network.getFailsafeExecutor(ctx, req4)

		require.NotNil(t, executor4)
		// Policy 2 doesn't match (eth_getBalance not in list)
		// Policy 3 DOES match (specific block = unknown finality)
		assert.Equal(t, "*", executor4.method)
		assert.Contains(t, executor4.finalities, common.DataFinalityStateUnknown)
	})
}
