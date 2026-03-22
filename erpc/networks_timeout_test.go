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

func TestNetwork_TimeoutPolicy(t *testing.T) {
	t.Run("FixedTimeout_BackwardCompatible", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Fixed timeout with no quantile — backward compatible behavior
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration: common.Duration(5 * time.Second),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x1111")
	})

	t.Run("FixedTimeout_RequestExceedsTimeout", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Short timeout that the request should exceed
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration: common.Duration(200 * time.Millisecond),
		})

		req := common.NewNormalizedRequest(requestBytes)
		_, err := network.Forward(ctx, req)

		require.Error(t, err)
	})

	t.Run("QuantileTimeout_DynamicComputation", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Set up mocks for metric building phase (10 requests with varying latencies)
		for i := 0; i < 10; i++ {
			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Times(1).
				Reply(200).
				Delay(time.Duration(20+i*5) * time.Millisecond). // 20-65ms
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}

		// Then a fast request that should succeed within the dynamic timeout
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Quantile-based timeout: p90 of latencies (~60ms), clamped to [200ms, 5s]
		// minDuration ensures timeout is at least 200ms even though p90 is ~60ms
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration:    common.Duration(1 * time.Second), // fallback
			Quantile:    0.9,
			MinDuration: common.Duration(200 * time.Millisecond),
			MaxDuration: common.Duration(5 * time.Second),
		})

		// Build up metrics
		for i := 0; i < 10; i++ {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			resp.Release()
		}

		// Now test with built-up metrics — should succeed since 20ms < dynamic timeout (min 200ms)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x2222")
	})

	t.Run("QuantileTimeout_MinDurationBoundary", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// Build metrics with very fast responses
		for i := 0; i < 5; i++ {
			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Reply(200).
				Delay(5 * time.Millisecond). // Very fast
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}

		// Request that takes longer than the raw quantile but less than minDuration
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(80 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// p10 of very fast responses would be ~5ms, but minDuration is 200ms
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Quantile:    0.1,
			MinDuration: common.Duration(200 * time.Millisecond),
			MaxDuration: common.Duration(5 * time.Second),
		})

		// Build metrics
		for i := 0; i < 5; i++ {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			resp.Release()
		}

		// Should succeed because minDuration (200ms) > actual latency (80ms)
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x2222")
	})

	t.Run("QuantileTimeout_ColdStartFallback", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Quantile-based timeout with Duration as cold start fallback
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Duration:    common.Duration(5 * time.Second), // fallback during cold start
			Quantile:    0.9,
			MinDuration: common.Duration(100 * time.Millisecond),
			MaxDuration: common.Duration(10 * time.Second),
		})

		// First request — no metrics yet, should use Duration fallback (5s)
		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x1111")
	})

	t.Run("QuantileTimeout_ColdStartFallbackToMaxDuration", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// No Duration set — should fall back to MaxDuration during cold start
		network := setupTestNetworkWithTimeoutPolicy(t, ctx, &common.TimeoutPolicyConfig{
			Quantile:    0.9,
			MinDuration: common.Duration(100 * time.Millisecond),
			MaxDuration: common.Duration(10 * time.Second),
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x1111")
	})
}

// Helper to set up network with timeout policy
func setupTestNetworkWithTimeoutPolicy(t *testing.T, ctx context.Context, timeoutConfig *common.TimeoutPolicyConfig) *Network {
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
		Failsafe: []*common.FailsafeConfig{{
			Timeout: timeoutConfig,
		}},
	}

	return setupTestNetwork(t, ctx, upstreamConfigs, networkConfig)
}
