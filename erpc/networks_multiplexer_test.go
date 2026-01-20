package erpc

import (
	"context"
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

// TestNetwork_Multiplexer_FollowersReceiveResponse tests that followers of a multiplexed request
// correctly receive the leader's response before cleanup runs.
//
// This test reproduces the race condition described in https://github.com/erpc/erpc/pull/615
// where cleanupMultiplexer would acquire the lock before followers could copy the response,
// causing followers to see nil response and make their own upstream requests.
func TestNetwork_Multiplexer_FollowersReceiveResponse(t *testing.T) {
	t.Run("ConcurrentFollowers_AllReceiveLeaderResponse", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		// Don't use AssertNoPendingMocks because with race condition,
		// followers might make their own requests

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Track how many upstream requests are actually made
		var upstreamRequestCount atomic.Int32

		// Set up mock that counts requests and has a delay
		// The delay ensures followers have time to register before leader finishes
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_blockNumber") {
					upstreamRequestCount.Add(1)
					return true
				}
				return false
			}).
			Persist(). // Allow multiple matches if race occurs
			Reply(200).
			Delay(100 * time.Millisecond). // Delay to ensure followers register
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1234567",
			})

		// Set up network without caching to test pure multiplexing
		network := setupTestNetworkForMultiplexer(t, ctx)

		// Number of concurrent requests (1 leader + followers)
		numRequests := 10
		var wg sync.WaitGroup
		wg.Add(numRequests)

		results := make([]*common.NormalizedResponse, numRequests)
		errors := make([]error, numRequests)
		nilResponseCount := atomic.Int32{}

		// Send all requests concurrently with the same request body
		// They should all have the same cache hash and be multiplexed
		requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)

		for i := 0; i < numRequests; i++ {
			go func(idx int) {
				defer wg.Done()

				req := common.NewNormalizedRequest(requestBody)
				resp, err := network.Forward(ctx, req)
				results[idx] = resp
				errors[idx] = err

				if resp == nil && err == nil {
					nilResponseCount.Add(1)
				}
			}(i)
		}

		wg.Wait()

		// Verify results
		successCount := 0
		for i := 0; i < numRequests; i++ {
			if errors[i] == nil && results[i] != nil {
				successCount++
				jrr, err := results[i].JsonRpcResponse()
				require.NoError(t, err, "Request %d: failed to parse JSON-RPC response", i)
				// GetResultString includes quotes, so check contains
				assert.Contains(t, jrr.GetResultString(), "0x1234567", "Request %d: unexpected result", i)
				results[i].Release()
			}
		}

		// Key assertion: ALL requests should succeed with response
		assert.Equal(t, numRequests, successCount,
			"All %d requests should receive a response, but only %d succeeded. "+
				"This indicates the multiplexer race condition where followers see nil response.",
			numRequests, successCount)

		// Ideally, only 1 upstream request should be made (leader)
		// If the race condition occurs, followers will make their own requests
		actualUpstreamRequests := int(upstreamRequestCount.Load())
		assert.Equal(t, 1, actualUpstreamRequests,
			"With proper multiplexing, only 1 upstream request should be made, but %d were made. "+
				"This indicates followers couldn't copy the leader's response and made their own requests.",
			actualUpstreamRequests)
	})

	t.Run("HerdOfFollowers_StressTest", func(t *testing.T) {
		// This test creates a large "herd" of concurrent requests to stress-test
		// the multiplexer synchronization
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var upstreamRequestCount atomic.Int32

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getBalance") {
					upstreamRequestCount.Add(1)
					return true
				}
				return false
			}).
			Persist().
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xdeadbeef",
			})

		network := setupTestNetworkForMultiplexer(t, ctx)

		// Large herd of requests
		numRequests := 50
		var wg sync.WaitGroup
		wg.Add(numRequests)

		successCount := atomic.Int32{}
		requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1234567890123456789012345678901234567890","latest"]}`)

		for i := 0; i < numRequests; i++ {
			go func() {
				defer wg.Done()

				req := common.NewNormalizedRequest(requestBody)
				resp, err := network.Forward(ctx, req)

				if err == nil && resp != nil {
					successCount.Add(1)
					resp.Release()
				}
			}()
		}

		wg.Wait()

		// All requests should succeed
		assert.Equal(t, int32(numRequests), successCount.Load(),
			"All %d requests should succeed, but only %d did",
			numRequests, successCount.Load())

		// Only 1 upstream request should be made
		assert.Equal(t, int32(1), upstreamRequestCount.Load(),
			"With proper multiplexing, only 1 upstream request should be made, got %d",
			upstreamRequestCount.Load())
	})

	t.Run("MultipleWaves_ConsecutiveMultiplexedRequests", func(t *testing.T) {
		// Test multiple waves of multiplexed requests to ensure cleanup doesn't
		// affect subsequent multiplexing
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var upstreamRequestCount atomic.Int32

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_getTransactionCount") {
					upstreamRequestCount.Add(1)
					return true
				}
				return false
			}).
			Persist().
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1",
			})

		network := setupTestNetworkForMultiplexer(t, ctx)

		numWaves := 3
		requestsPerWave := 10
		requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0x1234567890123456789012345678901234567890","latest"]}`)

		totalSuccess := 0

		for wave := 0; wave < numWaves; wave++ {
			var wg sync.WaitGroup
			wg.Add(requestsPerWave)

			waveSuccess := atomic.Int32{}

			for i := 0; i < requestsPerWave; i++ {
				go func() {
					defer wg.Done()

					req := common.NewNormalizedRequest(requestBody)
					resp, err := network.Forward(ctx, req)

					if err == nil && resp != nil {
						waveSuccess.Add(1)
						resp.Release()
					}
				}()
			}

			wg.Wait()
			totalSuccess += int(waveSuccess.Load())

			// Gap between waves to ensure cleanup completes
			// The 10ms cleanup delay + some buffer
			time.Sleep(50 * time.Millisecond)
		}

		// All requests in all waves should succeed
		expectedTotal := numWaves * requestsPerWave
		assert.Equal(t, expectedTotal, totalSuccess,
			"All %d requests across %d waves should succeed, but only %d did",
			expectedTotal, numWaves, totalSuccess)

		// Should have exactly numWaves upstream requests (one per wave)
		assert.Equal(t, int32(numWaves), upstreamRequestCount.Load(),
			"Should have %d upstream requests (one per wave), got %d",
			numWaves, upstreamRequestCount.Load())
	})

	t.Run("SlowUpstream_FollowersWaitAndReceive", func(t *testing.T) {
		// Test with a slower upstream to ensure followers wait properly
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var upstreamRequestCount atomic.Int32

		// Slow upstream response
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				if strings.Contains(body, "eth_gasPrice") {
					upstreamRequestCount.Add(1)
					return true
				}
				return false
			}).
			Persist().
			Reply(200).
			Delay(300 * time.Millisecond). // Slow response
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x77359400",
			})

		network := setupTestNetworkForMultiplexer(t, ctx)

		numRequests := 20
		var wg sync.WaitGroup
		wg.Add(numRequests)

		responseTimes := make([]time.Duration, numRequests)
		successCount := atomic.Int32{}
		requestBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_gasPrice","params":[]}`)

		startTime := time.Now()

		for i := 0; i < numRequests; i++ {
			go func(idx int) {
				defer wg.Done()

				req := common.NewNormalizedRequest(requestBody)
				resp, err := network.Forward(ctx, req)

				responseTimes[idx] = time.Since(startTime)

				if err == nil && resp != nil {
					successCount.Add(1)
					resp.Release()
				}
			}(i)
		}

		wg.Wait()

		// All requests should succeed
		assert.Equal(t, int32(numRequests), successCount.Load(),
			"All requests should succeed")

		// All response times should be roughly the same (within tolerance)
		// since they should all be multiplexed
		var minTime, maxTime time.Duration = responseTimes[0], responseTimes[0]
		for _, t := range responseTimes {
			if t < minTime {
				minTime = t
			}
			if t > maxTime {
				maxTime = t
			}
		}

		// The spread should be small - all requests finish around the same time
		// Allow 100ms tolerance for scheduling variance
		assert.Less(t, maxTime-minTime, 200*time.Millisecond,
			"All multiplexed requests should finish around the same time. Min: %v, Max: %v",
			minTime, maxTime)

		// Only 1 upstream request
		assert.Equal(t, int32(1), upstreamRequestCount.Load(),
			"Should have exactly 1 upstream request")
	})
}

// setupTestNetworkForMultiplexer creates a test network configured for multiplexer testing
// without caching to ensure we're testing pure multiplexing behavior.
func setupTestNetworkForMultiplexer(t *testing.T, ctx context.Context) *Network {
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
		// No caching to test pure multiplexing
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
