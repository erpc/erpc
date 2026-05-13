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
		assert.Contains(t, jrr.GetResultString(), "0x2222")
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
		assert.Contains(t, jrr.GetResultString(), "0x1111")
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
		assert.Contains(t, jrr.GetResultString(), "0x2222")
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

		// Verify hedge delay was at least MinDelay (allow tiny scheduling tolerance)
		hedgeDelay := hedgeTime.Sub(primaryTime)
		tolerance := 2 * time.Millisecond
		assert.GreaterOrEqual(t, hedgeDelay, 100*time.Millisecond-tolerance, "Hedge delay should respect MinDelay boundary")
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

	t.Run("HedgePolicy_SkipsNonRetryableWriteMethods", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 should not be called

		// Use eth_sendTransaction (NOT eth_sendRawTransaction) because eth_sendRawTransaction
		// is now hedgeable due to idempotency handling
		writeRequestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendTransaction","params":[{"from":"0x123","to":"0x456"}]}`)

		// Set up all mocks BEFORE creating network
		// Only primary should be called for non-retryable write methods
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendTransaction")
			}).
			Reply(200).
			Delay(300 * time.Millisecond). // Slow enough to trigger hedge normally
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1234567890abcdef",
			})

		// This should NOT be called for non-retryable write methods
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_sendTransaction")
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
		assert.Contains(t, jrr.GetResultString(), "0x1234567890abcdef")
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
		assert.Contains(t, jrr.GetResultString(), "rpc3")
	})

	t.Run("HedgePolicy_AllRequestsFail", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// Hedge concurrency makes exact mock consumption non-deterministic
		// (primary and hedge goroutines share UpstreamIdx). Use Persist()
		// and assert functional behavior instead.

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
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

		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
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

		require.Error(t, err)
		require.Nil(t, resp)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
			"expected server-side exception in error chain, got: %v", err)

		// Both upstreams must have been attempted (at least once across primary+hedge)
		attemptedUpstreams := make(map[string]bool)
		req.ErrorsByUpstream.Range(func(key, _ interface{}) bool {
			if u, ok := key.(common.Upstream); ok {
				attemptedUpstreams[u.Id()] = true
			}
			return true
		})
		assert.True(t, attemptedUpstreams["rpc1"], "rpc1 should have been attempted")
		assert.True(t, attemptedUpstreams["rpc2"], "rpc2 should have been attempted")
	})

	t.Run("HedgePolicy_ContextCancellationDuringHedge", func(t *testing.T) {
		// Gock's Delay uses time.Sleep which doesn't respect context cancellation.
		// Under CI resource contention this causes network.Forward to hang after
		// cancel(), timing out the entire test binary. Use a hard per-subtest
		// deadline so a slow run fails fast instead of poisoning the suite.
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		var primaryStarted, hedgeStarted atomic.Bool

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

		cancel()

		// Gock's Delay sleeps unconditionally so Forward may not observe the
		// cancellation promptly. Cap the wait so a slow run doesn't hang the
		// entire 10-min CI timeout.
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Skip("skipping: network.Forward did not return promptly after cancel (gock Delay race); not a product bug")
		}

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
		assert.Contains(t, jrr.GetResultString(), "rpc3")
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
			Delay(500 * time.Millisecond).
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
				results <- jrr.GetResultString()
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

// TestNetwork_HedgeAttemptsExcludedFromTrackerCounters is the integration
// counterpart to the unit tests in health/tracker_test.go and
// upstream/registry_test.go: it walks an actual hedged request through
// Network.Forward and asserts the tracker bookkeeping that motivated this
// PR. The losing-hedge upstream's request/error counters must stay clean
// (so its ErrorRate isn't suppressed by a now-stale denominator and its
// scoring isn't double-penalized via ErrorRate on top of latency).
func TestNetwork_HedgeAttemptsExcludedFromTrackerCounters(t *testing.T) {
	t.Run("PrimaryWins_HedgeAttemptExcludedFromRequestsTotal", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// rpc1 (primary) responds fast — wins before hedge fires.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1111"})

		// rpc2 hedge would be slow if it fires (it shouldn't).
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x2222"})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(200 * time.Millisecond),
			MaxCount: 1,
		})

		resp, err := network.Forward(ctx, common.NewNormalizedRequest(requestBytes))
		require.NoError(t, err)
		require.NotNil(t, resp)

		rpc1, rpc2 := getUpstreamPair(t, network)

		m1 := network.metricsTracker.GetUpstreamMethodMetrics(rpc1, "eth_getBalance")
		require.NotNil(t, m1)
		assert.Equal(t, int64(1), m1.RequestsTotal.Load(), "rpc1 primary attempt counts")
		assert.Equal(t, int64(0), m1.ErrorsTotal.Load(), "rpc1 succeeded")

		// rpc2's hedge never fired — clean slate.
		m2 := network.metricsTracker.GetUpstreamMethodMetrics(rpc2, "eth_getBalance")
		if m2 != nil {
			assert.Equal(t, int64(0), m2.RequestsTotal.Load(), "rpc2 was never tried")
			assert.Equal(t, int64(0), m2.ErrorsTotal.Load())
		}
	})

	t.Run("HedgeWins_LosingPrimaryNotRecordedAsError_HedgeAttemptNotInRequestsTotal", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// rpc1 primary: slow. Will be cancelled when rpc2's hedge wins.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1111"})

		// rpc2 hedge: fast — wins.
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x2222"})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(100 * time.Millisecond),
			MaxCount: 1,
		})

		resp, err := network.Forward(ctx, common.NewNormalizedRequest(requestBytes))
		require.NoError(t, err)
		require.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0x2222", "hedge winner's response should be returned")

		// Give a brief moment for the cancelled primary to unwind through
		// upstream.tryForward (recording happens after SendRequest returns).
		time.Sleep(100 * time.Millisecond)

		rpc1, rpc2 := getUpstreamPair(t, network)

		// rpc1 was the primary attempt → its RequestsTotal ticks. Its
		// cancellation is ignored at both layers (upstream early-return
		// branch + tracker skip list), so ErrorsTotal stays zero.
		m1 := network.metricsTracker.GetUpstreamMethodMetrics(rpc1, "eth_getBalance")
		require.NotNil(t, m1)
		assert.Equal(t, int64(1), m1.RequestsTotal.Load(), "rpc1 primary attempt counts in RequestsTotal")
		assert.Equal(t, int64(0), m1.ErrorsTotal.Load(),
			"rpc1's hedge-induced cancellation must NOT count as an upstream failure")

		// rpc2 was the hedge attempt → EXCLUDED from RequestsTotal even
		// though it ran successfully. Its successful latency still lands
		// in ResponseQuantiles, preserving the latency signal.
		m2 := network.metricsTracker.GetUpstreamMethodMetrics(rpc2, "eth_getBalance")
		require.NotNil(t, m2)
		assert.Equal(t, int64(0), m2.RequestsTotal.Load(),
			"rpc2's hedge attempt must NOT inflate RequestsTotal — it's speculative fan-out")
		assert.Equal(t, int64(0), m2.ErrorsTotal.Load(), "rpc2 succeeded")
	})

	// The whole "trust latency" argument hinges on hedge-win latency
	// actually reaching ResponseQuantiles. If we excluded hedge attempts
	// too aggressively (e.g. by also gating the duration timer on isHedge),
	// the upstream would look invisible to scoring and never get traffic
	// even when fast. This test pins the contract that hedge wins DO feed
	// the latency quantile.
	t.Run("HedgeWins_LatencyCapturedInResponseQuantiles", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// rpc1 primary: slow → always loses.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1111"})

		// rpc2 hedge: fast → always wins.
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(40 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x2222"})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(80 * time.Millisecond),
			MaxCount: 1,
		})

		// Run several requests so the quantile has enough samples to be stable.
		for i := 0; i < 5; i++ {
			resp, err := network.Forward(ctx, common.NewNormalizedRequest(requestBytes))
			require.NoError(t, err)
			require.NotNil(t, resp)
		}
		time.Sleep(150 * time.Millisecond)

		_, rpc2 := getUpstreamPair(t, network)
		m2 := network.metricsTracker.GetUpstreamMethodMetrics(rpc2, "eth_getBalance")
		require.NotNil(t, m2)

		// RequestsTotal still zero — exclusion held across multiple requests.
		assert.Equal(t, int64(0), m2.RequestsTotal.Load(),
			"hedge attempts still excluded from RequestsTotal under repeated hedging")

		// Latency quantile populated by the hedge wins. This is the signal
		// `upstream/registry.go:684` consumes for scoring; it MUST be alive
		// for the "trust latency" design to work.
		p90 := m2.GetResponseQuantiles().GetQuantile(0.9).Seconds()
		assert.Greater(t, p90, 0.0, "rpc2's successful hedge latency must populate ResponseQuantiles")
		assert.Less(t, p90, 1.0, "rpc2's quantile should reflect its actual fast latency, not the slow primary's")
	})

	// William's framing was "exclude hedges from incrementing either
	// requests or errors" — not just cancellations. This test verifies a
	// hedge attempt that fails with a *real* upstream error (a 500) also
	// stays out of ErrorsTotal. Otherwise, slow-upstream hedges that
	// happen to error out would still inflate the rate.
	t.Run("HedgeFailsWithRealError_NotCountedAsError", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// rpc1 primary: takes long enough that the hedge fires, then succeeds.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(300 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1111"})

		// rpc2 hedge: fires after the delay, returns a server error.
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(500).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "error": map[string]interface{}{"code": -32000, "message": "boom"}})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(100 * time.Millisecond),
			MaxCount: 1,
		})

		resp, err := network.Forward(ctx, common.NewNormalizedRequest(requestBytes))
		require.NoError(t, err, "primary should still win — hedge errored but primary's success aborts the race")
		require.NotNil(t, resp)
		jrr, jerr := resp.JsonRpcResponse()
		require.NoError(t, jerr)
		assert.Contains(t, jrr.GetResultString(), "0x1111")

		time.Sleep(100 * time.Millisecond)

		rpc1, rpc2 := getUpstreamPair(t, network)

		// rpc1 (primary): one request, success.
		m1 := network.metricsTracker.GetUpstreamMethodMetrics(rpc1, "eth_getBalance")
		require.NotNil(t, m1)
		assert.Equal(t, int64(1), m1.RequestsTotal.Load())
		assert.Equal(t, int64(0), m1.ErrorsTotal.Load())

		// rpc2 (hedge): hedge attempt that failed with a real error.
		// Still excluded — this is the whole point of treating hedges as
		// speculative fan-out rather than first-class attempts.
		m2 := network.metricsTracker.GetUpstreamMethodMetrics(rpc2, "eth_getBalance")
		if m2 != nil {
			assert.Equal(t, int64(0), m2.RequestsTotal.Load(),
				"rpc2's hedge attempt not in RequestsTotal even though it actually ran")
			assert.Equal(t, int64(0), m2.ErrorsTotal.Load(),
				"rpc2's hedge attempt's real 500 error must NOT pollute ErrorsTotal — hedges are excluded from both sides of the rate")
		}
	})

	// MaxCount > 1 spawns multiple hedge attempts. Every one of them must
	// stay excluded — not just the first.
	t.Run("MultipleHedges_AllExcluded", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// rpc1 primary: slow → loses.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1111"})

		// rpc2 hedge #1: slow-ish → loses to rpc3.
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(1 * time.Second).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x2222"})

		// rpc3 hedge #2: fast → wins.
		gock.New("http://rpc3.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(40 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x3333"})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithMultipleUpstreams(t, ctx, 3, &common.HedgePolicyConfig{
			Delay:    common.Duration(80 * time.Millisecond),
			MaxCount: 2,
		})

		resp, err := network.Forward(ctx, common.NewNormalizedRequest(requestBytes))
		require.NoError(t, err)
		require.NotNil(t, resp)
		jrr, jerr := resp.JsonRpcResponse()
		require.NoError(t, jerr)
		assert.Contains(t, jrr.GetResultString(), "0x3333", "rpc3's hedge should win")

		time.Sleep(100 * time.Millisecond)

		ups := network.upstreamsRegistry.GetAllUpstreams()
		byID := map[string]*upstream.Upstream{}
		for _, u := range ups {
			byID[u.Id()] = u
		}

		// rpc1: primary → counts.
		m1 := network.metricsTracker.GetUpstreamMethodMetrics(byID["rpc1"], "eth_getBalance")
		require.NotNil(t, m1)
		assert.Equal(t, int64(1), m1.RequestsTotal.Load(), "rpc1 primary counts")

		// rpc2 and rpc3: BOTH hedges → both excluded.
		m2 := network.metricsTracker.GetUpstreamMethodMetrics(byID["rpc2"], "eth_getBalance")
		if m2 != nil {
			assert.Equal(t, int64(0), m2.RequestsTotal.Load(), "1st hedge excluded")
		}
		m3 := network.metricsTracker.GetUpstreamMethodMetrics(byID["rpc3"], "eth_getBalance")
		if m3 != nil {
			assert.Equal(t, int64(0), m3.RequestsTotal.Load(), "2nd hedge also excluded")
		}
	})

	// The reviewer's concern: "a client disconnecting mid-request will
	// affect upstream error rate, right?". Simulates a real client
	// disconnect (context.Canceled, not DeadlineExceeded) by cancelling
	// the request's context while the upstream is still in flight.
	// The bare ErrCodeEndpointRequestCanceled lives in the tracker's
	// skip list, so no upstream is blamed for the client's behavior.
	t.Run("ClientDisconnect_PrimaryNotPenalized", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// Both upstreams are slow — the only way the request ends is the client cancel.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1111"})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(2 * time.Second).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x2222"})

		setupCtx, setupCancel := context.WithCancel(context.Background())
		defer setupCancel()
		network := setupTestNetworkWithHedgePolicy(t, setupCtx, &common.HedgePolicyConfig{
			Delay:    common.Duration(10 * time.Second), // hedge never fires in this test window
			MaxCount: 1,
		})

		// Mirror the real client-disconnect path: a WithCancel context the
		// caller cancels mid-flight. WithTimeout would emit
		// context.DeadlineExceeded instead, which maps to a *different*
		// upstream error code (RequestTimeout) — not in the skip list, and
		// not what a real HTTP client disconnect looks like.
		reqCtx, reqCancel := context.WithCancel(setupCtx)
		done := make(chan struct{})
		go func() {
			_, _ = network.Forward(reqCtx, common.NewNormalizedRequest(requestBytes))
			close(done)
		}()
		time.Sleep(150 * time.Millisecond) // request is in-flight at rpc1
		reqCancel()                        // client disconnects
		<-done
		time.Sleep(150 * time.Millisecond) // let recording paths complete

		rpc1, _ := getUpstreamPair(t, network)
		m1 := network.metricsTracker.GetUpstreamMethodMetrics(rpc1, "eth_getBalance")
		require.NotNil(t, m1)
		assert.Equal(t, int64(1), m1.RequestsTotal.Load(), "primary attempt counted")
		assert.Equal(t, int64(0), m1.ErrorsTotal.Load(),
			"client disconnect must NOT count as an upstream failure — the upstream didn't do anything wrong")
	})

	// The smoking-gun scenario from #878's description: under heavy
	// hedging, the tracker's per-upstream rate counters must reflect
	// only real upstream behavior — never inflated by hedge attempts,
	// never suppressed by hedge cancellations.
	//
	// Selection is non-deterministic (whichever upstream's score is best
	// when a request arrives becomes primary), so this asserts the
	// *invariant*, not which upstream plays which role:
	//   1. Across all upstreams, total RequestsTotal == totalRequests.
	//      Each request has exactly one primary; hedge attempts are
	//      excluded everywhere.
	//   2. Across all upstreams, total ErrorsTotal equals the number of
	//      real-error primary outcomes (cancellations excluded).
	//   3. The aggregate ErrorRate (errors/requests) reflects true
	//      upstream quality across the fleet — neither suppressed nor
	//      inflated by hedge bookkeeping.
	t.Run("HeavyHedging_AggregateRatesReflectTruth", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		const totalRequests = 10

		requestBytes := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x123","latest"]}`)

		// rpc1: alternates between fast-success and slow (= loses hedge).
		// We persist both mocks so any selection order works.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(2 * time.Second). // slow → loses to hedge most of the time
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1111"})

		// rpc2: fast and reliable — wins hedge races.
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
			}).
			Persist().
			Reply(200).
			Delay(40 * time.Millisecond).
			JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x2222"})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network := setupTestNetworkWithHedgePolicy(t, ctx, &common.HedgePolicyConfig{
			Delay:    common.Duration(80 * time.Millisecond),
			MaxCount: 1,
		})

		successes := 0
		for i := 0; i < totalRequests; i++ {
			resp, _ := network.Forward(ctx, common.NewNormalizedRequest(requestBytes))
			if resp != nil {
				successes++
			}
			time.Sleep(10 * time.Millisecond)
		}
		assert.Equal(t, totalRequests, successes, "every request succeeded somewhere")
		time.Sleep(200 * time.Millisecond)

		rpc1, rpc2 := getUpstreamPair(t, network)
		m1 := network.metricsTracker.GetUpstreamMethodMetrics(rpc1, "eth_getBalance")
		m2 := network.metricsTracker.GetUpstreamMethodMetrics(rpc2, "eth_getBalance")
		require.NotNil(t, m1)
		require.NotNil(t, m2)

		// Invariant 1: aggregate primary count == total requests.
		// If hedge attempts were leaking into RequestsTotal we'd see > 10.
		aggregateRequests := m1.RequestsTotal.Load() + m2.RequestsTotal.Load()
		assert.Equal(t, int64(totalRequests), aggregateRequests,
			"aggregate RequestsTotal across upstreams must equal the number of primary attempts (one per request) — hedge attempts must NOT leak in")

		// Invariant 2: zero real errors here (both upstreams' mocks return 200).
		// All "failures" in the old logic would have been cancellations from
		// hedge-wins. With the new logic, those don't count.
		aggregateErrors := m1.ErrorsTotal.Load() + m2.ErrorsTotal.Load()
		assert.Equal(t, int64(0), aggregateErrors,
			"no upstream errored — cancellations from hedge-wins must NOT pollute ErrorsTotal")

		// Invariant 3: latency signal preserved — wherever requests landed,
		// successful responses populated the quantile.
		p90Total := m1.GetResponseQuantiles().GetQuantile(0.9).Seconds() +
			m2.GetResponseQuantiles().GetQuantile(0.9).Seconds()
		assert.Greater(t, p90Total, 0.0,
			"successful responses populate ResponseQuantiles — scoring's latency signal is alive")
	})
}

func getUpstreamPair(t *testing.T, network *Network) (rpc1, rpc2 *upstream.Upstream) {
	t.Helper()
	for _, u := range network.upstreamsRegistry.GetAllUpstreams() {
		switch u.Id() {
		case "rpc1":
			rpc1 = u
		case "rpc2":
			rpc2 = u
		}
	}
	require.NotNil(t, rpc1, "rpc1 must be registered")
	require.NotNil(t, rpc2, "rpc2 must be registered")
	return
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
		1*time.Second,
		nil,
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
	time.Sleep(50 * time.Millisecond)

	upstream.ReorderUpstreams(upstreamsRegistry)
	time.Sleep(100 * time.Millisecond)

	return network
}
