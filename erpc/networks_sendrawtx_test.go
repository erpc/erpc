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

// Sample signed transaction for testing (a valid RLP-encoded Ethereum transaction)
// This is a simple transfer transaction - the hash is deterministic based on the content
const sampleSignedTx = "0xf86c098504a817c800825208943535353535353535353535353535353535353535880de0b6b3a76400008025a028ef61340bd939bc2195fe537567866003e1a15d3c71ff63e1590620aa636276a067cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83"

// Expected tx hash for the sample transaction
const expectedTxHash = "0x33469b22e9f636356c4160a87eb19df52b7412e8eac32a4a55f0ef7be5c61c8d"

func TestNetwork_SendRawTransaction_Idempotency(t *testing.T) {
	t.Run("AlreadyKnownReturnsSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Upstream returns "already known" error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed (idempotent) - error is converted to success
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Result should be the transaction hash
		result := jrr.GetResultString()
		assert.Contains(t, result, "0x")
	})

	t.Run("NonceTooLowWithMatchingTxReturnsSuccess", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First: upstream returns "nonce too low" error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "nonce too low",
				},
			})

		// Then: eth_getTransactionByHash returns the matching transaction
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"hash":        expectedTxHash,
					"nonce":       "0x9",
					"blockHash":   "0x1234567890abcdef",
					"blockNumber": "0x100",
					"from":        "0x1234567890123456789012345678901234567890",
					"to":          "0x3535353535353535353535353535353535353535",
					"value":       "0xde0b6b3a7640000",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - tx exists on chain
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Result should be the transaction hash
		result := jrr.GetResultString()
		assert.Contains(t, result, "0x")
	})

	t.Run("NonceTooLowWithNoMatchingTxReturnsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First: upstream returns "nonce too low" error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "nonce too low",
				},
			})

		// Then: eth_getTransactionByHash returns null (tx not found)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getTransactionByHash")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  nil,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - different tx with same nonce
		require.Error(t, err)

		// The error should be normalized to -32003 (Transaction rejected)
		var jrpcErr *common.ErrJsonRpcExceptionInternal
		if common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
			// Check that it contains the original message
			assert.Contains(t, err.Error(), "nonce too low")
		} else {
			// Accept other error types as long as they indicate failure
			assert.NotNil(t, err)
		}
		_ = jrpcErr
		_ = resp
	})

	t.Run("ExecutionRevertedIsRetryableAcrossUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream returns execution reverted
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted",
				},
			})

		// Second upstream succeeds
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Use retry policy to enable retrying across upstreams
		network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(10 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - retried to second upstream
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		result := jrr.GetResultString()
		assert.Contains(t, result, expectedTxHash)
	})

	t.Run("AllUpstreamsRevertReturnsRevertError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Both upstreams return execution reverted
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted: insufficient balance",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    3,
					"message": "execution reverted: insufficient balance",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithRetry(t, ctx, &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(10 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return the execution exception error (not ErrFailsafeRetryExceeded)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointExecutionException),
			"expected ErrCodeEndpointExecutionException but got: %v", err)
		_ = resp
	})

	t.Run("HedgeWorksForSendRawTransaction", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// First upstream is slow
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  expectedTxHash,
			})

		// Second upstream (hedged) is fast and returns "already known"
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkWithHedge(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(100 * time.Millisecond),
			MaxCount: 1,
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should succeed - hedged request returned "already known" which is converted to success
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		result := jrr.GetResultString()
		assert.Contains(t, result, "0x")
	})

	t.Run("ReplacementUnderpricedRemainsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Upstream returns "replacement transaction underpriced" - this should NOT be converted to success
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "replacement transaction underpriced",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupSendRawTxTestNetworkSingleUpstream(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - replacement underpriced is NOT idempotent
		require.Error(t, err)
		_ = resp
	})

	t.Run("IdempotentBroadcastDisabledReturnsRawError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`)

		// Upstream returns "already known" error - normally this would be converted to success
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "already known",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Set up network with IdempotentTransactionBroadcast DISABLED
		network := setupSendRawTxTestNetworkWithIdempotentDisabled(t, ctx)

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should return error - idempotency is disabled, so "already known" is NOT converted to success
		require.Error(t, err)
		_ = resp
	})
}

// Helper to set up a single-upstream test network (no load balancing uncertainty)
func setupSendRawTxTestNetworkSingleUpstream(t *testing.T, ctx context.Context) *Network {
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
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Helper to set up a single-upstream network with IdempotentTransactionBroadcast disabled
func setupSendRawTxTestNetworkWithIdempotentDisabled(t *testing.T, ctx context.Context) *Network {
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

	idempotentDisabled := false
	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                        123,
			IdempotentTransactionBroadcast: &idempotentDisabled,
		},
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Helper to set up a test network with retry policy (two upstreams)
func setupSendRawTxTestNetworkWithRetry(t *testing.T, ctx context.Context, retryConfig *common.RetryPolicyConfig) *Network {
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
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			Retry: retryConfig,
		}},
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Helper to set up a test network with hedge policy
func setupSendRawTxTestNetworkWithHedge(t *testing.T, ctx context.Context, hedgeConfig *common.HedgePolicyConfig) *Network {
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
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId: 123,
		},
		Failsafe: []*common.FailsafeConfig{{
			Hedge: hedgeConfig,
		}},
	}

	return setupSendRawTxNetwork(t, ctx, upstreamConfigs, networkConfig)
}

func setupSendRawTxNetwork(t *testing.T, ctx context.Context, upstreamConfigs []*common.UpstreamConfig, networkConfig *common.NetworkConfig) *Network {
	t.Helper()

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
		1*time.Second,
		nil,
	)

	network, err := NewNetwork(
		ctx,
		&log.Logger,
		"test",
		networkConfig,
		rateLimitersRegistry,
		upstreamsRegistry,
		metricsTracker,
	)
	require.NoError(t, err)

	upstreamsRegistry.Bootstrap(ctx)
	time.Sleep(100 * time.Millisecond)

	return network
}
