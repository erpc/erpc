package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

func TestNetwork_HedgePolicy(t *testing.T) {
	t.Run("FixedDelayHedge_FirstRequestSlowHedgeWins", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// Set up all mocks BEFORE creating network
		// First request to rpc1 - slow
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc":  "2.0",
				"id":       1,
				"result":   "0x1111",
				"fromHost": "rpc1",
			})

		// Hedged request to rpc2 - fast
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc":  "2.0",
				"id":       1,
				"result":   "0x2222",
				"fromHost": "rpc2",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(100 * time.Millisecond),
			MaxCount: 1,
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Should get response from rpc2 (hedged request)
		assert.Contains(t, string(jrr.Result), "0x2222")
	})

	t.Run("FixedDelayHedge_FirstRequestFastNoHedge", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 mock should not be called

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// Set up all mocks BEFORE creating network
		// First request to rpc1 - fast
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond). // Faster than hedge delay
			JSON(map[string]interface{}{
				"jsonrpc":  "2.0",
				"id":       1,
				"result":   "0x1111",
				"fromHost": "rpc1",
			})

		// This should not be called
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc":  "2.0",
				"id":       1,
				"result":   "0x2222",
				"fromHost": "rpc2",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(200 * time.Millisecond),
			MaxCount: 1,
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Should get response from rpc1 (no hedge triggered)
		assert.Contains(t, string(jrr.Result), "0x1111")
	})

	t.Run("QuantileBasedHedge_DynamicDelay", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// Don't assert pending mocks due to unpredictable concurrent requests

		// Set up all mocks BEFORE creating network
		// First, set up mocks for metric building phase
		for i := 0; i < 10; i++ {
			gock.New("http://rpc1.localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Times(1).
				Reply(200).
				Delay(time.Duration(50+i*10) * time.Millisecond). // Varying delays 50-140ms
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}

		// Then set up mocks for the actual hedge test
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Persist(). // Allow multiple matches in case of retries
			Reply(200).
			Delay(500 * time.Millisecond). // Slow primary
			JSON(map[string]interface{}{
				"jsonrpc":  "2.0",
				"id":       1,
				"result":   "0x1111",
				"fromHost": "rpc1",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Persist(). // Allow multiple matches in case of retries
			Reply(200).
			Delay(50 * time.Millisecond). // Fast hedge
			JSON(map[string]interface{}{
				"jsonrpc":  "2.0",
				"id":       1,
				"result":   "0x2222",
				"fromHost": "rpc2",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Set up network with quantile-based hedge
		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(50 * time.Millisecond), // Base delay
			MaxCount: 1,
			Quantile: 0.9,                                     // 90th percentile
			MinDelay: common.Duration(20 * time.Millisecond),  // Min boundary
			MaxDelay: common.Duration(200 * time.Millisecond), // Max boundary
		})

		// First, make several requests to build up metrics
		for i := 0; i < 10; i++ {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			resp.Release()
		}

		// Now test hedge with quantile-based delay
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Should get response from hedged request
		assert.Contains(t, string(jrr.Result), "0x2222")
	})

	t.Run("QuantileBasedHedge_MinDelayBoundary", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Set up all mocks BEFORE creating network
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

		// Track hedge timing
		var hedgeTime time.Time
		var primaryTime time.Time

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getBalance") {
					primaryTime = time.Now()
					return true
				}
				return false
			}).
			Reply(200).
			Delay(300 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getBalance") {
					hedgeTime = time.Now()
					return true
				}
				return false
			}).
			Reply(200).
			Delay(10 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Set up network with quantile that would result in very low delay
		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(0), // Zero base delay
			MaxCount: 1,
			Quantile: 0.1,                                     // 10th percentile (will be low)
			MinDelay: common.Duration(100 * time.Millisecond), // Min boundary
			MaxDelay: common.Duration(500 * time.Millisecond),
		})

		// Build metrics
		for i := 0; i < 5; i++ {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			resp.Release()
		}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		// Verify hedge delay was at least MinDelay
		hedgeDelay := hedgeTime.Sub(primaryTime)
		assert.GreaterOrEqual(t, hedgeDelay, 100*time.Millisecond, "Hedge delay should respect MinDelay boundary")
	})

	t.Run("QuantileBasedHedge_MaxDelayBoundary", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// Don't assert pending mocks for this test

		// Set up all mocks BEFORE creating network
		// Build metrics with slow responses - set up mocks for both upstreams
		for i := 0; i < 5; i++ {
			// Set up mocks for both upstreams
			gock.New("http://rpc1.localhost").
				Post("").
				Times(1).
				Reply(200).
				Delay(200 * time.Millisecond). // Slow responses
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})

			// Also set up rpc2 in case it's used
			gock.New("http://rpc2.localhost").
				Post("").
				Times(1).
				Reply(200).
				Delay(200 * time.Millisecond).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"result":  "0x1111",
				})
		}

		// Track hedge timing
		var hedgeTime time.Time
		var primaryTime time.Time

		// Set up test mocks - make them catch-all
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				if primaryTime.IsZero() {
					primaryTime = time.Now()
				}
				return true
			}).
			Reply(200).
			Delay(400 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				if hedgeTime.IsZero() {
					hedgeTime = time.Now()
				}
				return true
			}).
			Reply(200).
			Delay(10 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Set up network with quantile that would result in very high delay
		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(300 * time.Millisecond), // High base delay
			MaxCount: 1,
			Quantile: 0.99, // 99th percentile
			MinDelay: common.Duration(10 * time.Millisecond),
			MaxDelay: common.Duration(150 * time.Millisecond), // Max boundary
		})

		// Build metrics with slow responses
		for i := 0; i < 5; i++ {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err)
			resp.Release()
		}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)

		// Verify hedge delay was at most MaxDelay
		hedgeDelay := hedgeTime.Sub(primaryTime)
		assert.LessOrEqual(t, hedgeDelay, 160*time.Millisecond, "Hedge delay should respect MaxDelay boundary")
	})

	t.Run("HedgePolicy_SkipsWriteMethods", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should not be called

		writeRequestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0xabcdef"]}`)

		// Set up all mocks BEFORE creating network
		// Only primary should be called for write methods
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendRawTransaction")
			}).
			Reply(200).
			Delay(300 * time.Millisecond). // Slow enough to trigger hedge normally
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1234567890abcdef",
			})

		// This should NOT be called for write methods
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
				"result":  "0xfedcba0987654321",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(50 * time.Millisecond),
			MaxCount: 5,
		})

		req := common.NewNormalizedRequest(writeRequestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Should only get response from primary
		assert.Contains(t, string(jrr.Result), "0x1234567890abcdef")
	})

	t.Run("HedgePolicy_MaxCountLimit", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc4 should not be called (rpc3 completes fastest)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// Set up all mocks BEFORE creating network
		// Set up delays so rpc3 (second hedge) completes first
		delays := map[string]time.Duration{
			"rpc1": 500 * time.Millisecond, // Primary - slow
			"rpc2": 400 * time.Millisecond, // First hedge - slow
			"rpc3": 100 * time.Millisecond, // Second hedge - fastest
			"rpc4": 300 * time.Millisecond, // Would be third hedge (not allowed)
		}

		for host, delay := range delays {
			gock.New("http://" + host + ".localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Reply(200).
				Delay(delay).
				JSON(map[string]interface{}{
					"jsonrpc":  "2.0",
					"id":       1,
					"result":   "0x" + host,
					"fromHost": host,
				})
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create network with 4 upstreams but MaxCount=2
		network := setupTestNetworkWithMultipleUpstreams(t, ctx, 4, &common.HedgePolicyConfig{
			Delay:    common.Duration(50 * time.Millisecond),
			MaxCount: 2, // Only 2 hedges allowed (total 3 requests including primary)
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Should get response from rpc3 (second hedge, fastest to complete)
		assert.Contains(t, string(jrr.Result), "rpc3")
	})

	t.Run("HedgePolicy_AllRequestsFail", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// Set up all mocks BEFORE creating network
		// Primary fails
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(500).
			Delay(100 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Server error",
				},
			})

		// Hedge also fails
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(503).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Service unavailable",
				},
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(50 * time.Millisecond),
			MaxCount: 1,
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		// Should get error since all requests failed
		require.Error(t, err)
		require.Nil(t, resp)
		assert.Contains(t, err.Error(), "ErrEndpointServerSideException")
	})

	t.Run("HedgePolicy_ContextCancellationDuringHedge", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// Don't assert pending mocks since we're canceling requests

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		var primaryStarted, hedgeStarted atomic.Bool

		// Set up all mocks BEFORE creating network
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getBalance") {
					primaryStarted.Store(true)
					return true
				}
				return false
			}).
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getBalance") {
					hedgeStarted.Store(true)
					return true
				}
				return false
			}).
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(50 * time.Millisecond),
			MaxCount: 1,
		})

		var wg sync.WaitGroup
		wg.Add(1)
		var respErr error

		go func() {
			defer wg.Done()
			req := common.NewNormalizedRequest(requestBytes)
			_, respErr = network.Forward(ctx, req)
		}()

		// Wait for hedge to start
		time.Sleep(100 * time.Millisecond)
		assert.True(t, primaryStarted.Load(), "Primary request should have started")
		assert.True(t, hedgeStarted.Load(), "Hedge request should have started")

		// Cancel context
		cancel()

		wg.Wait()
		require.Error(t, respErr)
		assert.True(t, strings.Contains(respErr.Error(), "context canceled"))
	})

	t.Run("HedgePolicy_MultipleHedgesWithVaryingResponseTimes", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// Set up all mocks BEFORE creating network
		// Set up response times: rpc3 will be fastest
		delays := map[string]time.Duration{
			"rpc1": 400 * time.Millisecond, // Primary - slowest
			"rpc2": 300 * time.Millisecond, // First hedge
			"rpc3": 100 * time.Millisecond, // Second hedge - fastest
			"rpc4": 200 * time.Millisecond, // Third hedge
		}

		for host, delay := range delays {
			gock.New("http://" + host + ".localhost").
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, "eth_getBalance")
				}).
				Reply(200).
				Delay(delay).
				JSON(map[string]interface{}{
					"jsonrpc":  "2.0",
					"id":       1,
					"result":   "0x" + host,
					"fromHost": host,
				})
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithMultipleUpstreams(t, ctx, 4, &common.HedgePolicyConfig{
			Delay:    common.Duration(50 * time.Millisecond),
			MaxCount: 3, // Allow 3 hedges
		})

		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)

		// Should get response from rpc3 (fastest)
		assert.Contains(t, string(jrr.Result), "rpc3")
	})

	t.Run("QuantileBasedHedge_NoMetricsAvailable", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// Set up all mocks BEFORE creating network
		// Track timing
		var primaryTime, hedgeTime time.Time

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getBalance") {
					primaryTime = time.Now()
					return true
				}
				return false
			}).
			Reply(200).
			Delay(300 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getBalance") {
					hedgeTime = time.Now()
					return true
				}
				return false
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Set up network with quantile-based hedge but no metrics
		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(100 * time.Millisecond), // Base delay (fallback)
			MaxCount: 1,
			Quantile: 0.9,
			MinDelay: common.Duration(50 * time.Millisecond),
			MaxDelay: common.Duration(200 * time.Millisecond),
		})

		// First request without any metrics history
		req := common.NewNormalizedRequest(requestBytes)
		resp, err := network.Forward(ctx, req)

		require.NoError(t, err)
		require.NotNil(t, resp)

		// Verify hedge was triggered with base delay (no metrics available)
		hedgeDelay := hedgeTime.Sub(primaryTime)
		assert.GreaterOrEqual(t, hedgeDelay, 90*time.Millisecond, "Should use base delay when no metrics available")
		assert.LessOrEqual(t, hedgeDelay, 110*time.Millisecond, "Should use base delay when no metrics available")
	})

	t.Run("HedgePolicy_ConcurrentRequests", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// Don't assert pending mocks for concurrent test

		// Set up all mocks BEFORE creating network
		// Set up persistent mocks for concurrent requests
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Reply(200).
			Delay(200 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1111",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2222",
			})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(100 * time.Millisecond),
			MaxCount: 1,
		})

		// Launch multiple concurrent requests
		const numRequests = 10
		var wg sync.WaitGroup
		results := make(chan string, numRequests)

		for i := 0; i < numRequests; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`))
				resp, err := network.Forward(ctx, req)
				if err != nil {
					results <- "error"
					return
				}
				jrr, _ := resp.JsonRpcResponse()
				results <- string(jrr.Result)
			}()
		}

		wg.Wait()
		close(results)

		// Verify all requests succeeded with hedged responses
		successCount := 0
		for result := range results {
			if strings.Contains(result, "0x2222") {
				successCount++
			}
		}

		assert.Equal(t, numRequests, successCount, "All requests should succeed with hedged responses")
	})
}

// Helper function to set up network with hedge policy
func setupTestNetworkWithHedgePolicy(t *testing.T, ctx context.Context, hedgeConfig *common.HedgePolicyConfig) *Network {
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

	return setupTestNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Helper function to set up network with multiple upstreams
func setupTestNetworkWithMultipleUpstreams(t *testing.T, ctx context.Context, numUpstreams int, hedgeConfig *common.HedgePolicyConfig) *Network {
	t.Helper()

	upstreamConfigs := make([]*common.UpstreamConfig, numUpstreams)
	for i := 0; i < numUpstreams; i++ {
		upstreamConfigs[i] = &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       fmt.Sprintf("rpc%d", i+1),
			Endpoint: fmt.Sprintf("http://rpc%d.localhost", i+1),
			Evm: &common.EvmUpstreamConfig{
				ChainId: 123,
			},
		}
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

	return setupTestNetwork(t, ctx, upstreamConfigs, networkConfig)
}

// Common network setup function
func setupTestNetwork(t *testing.T, ctx context.Context, upstreamConfigs []*common.UpstreamConfig, networkConfig *common.NetworkConfig) *Network {
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

	err = upstreamsRegistry.Bootstrap(ctx)
	require.NoError(t, err)

	err = upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(123))
	require.NoError(t, err)

	err = network.Bootstrap(ctx)
	require.NoError(t, err)

	// Set up state pollers
	upsList := upstreamsRegistry.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))
	for _, ups := range upsList {
		err = ups.Bootstrap(ctx)
		require.NoError(t, err)
		ups.EvmStatePoller().SuggestLatestBlock(1000)
		ups.EvmStatePoller().SuggestFinalizedBlock(900)
	}

	upstream.ReorderUpstreams(upstreamsRegistry)
	time.Sleep(100 * time.Millisecond)

	return network
}
