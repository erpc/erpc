package erpc

import (
	"encoding/json"
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

func TestHttpServer_HedgedRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with background goroutines in short mode")
	}
	t.Run("SimpleHedgePolicyDifferentUpstreams", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(10 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(10 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(100 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222")
	})

	t.Run("PrimaryFailsBeforeHedgeStarts", func(t *testing.T) {
		// When primary fails BEFORE hedge delay, should return immediately
		// This enables quick retry without waiting for hedge delay
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 2) // rpc2 and rpc3 never called

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 2,
										Delay:    common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc3",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc3.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary request - fails with 500 error
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(50 * time.Millisecond). // Fails before hedge delay (100ms)
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2 and rpc3: Never called because primary fails before hedge starts
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Persist(). // Keep it pending
			Reply(503).
			JSON(map[string]interface{}{
				"error": "service unavailable",
			})

		gock.New("http://rpc3.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Persist(). // Keep it pending
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x333333",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		// Should fail immediately but transport stays 200 per JSON-RPC over HTTP
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "internal server error")
	})

	t.Run("HedgeDoesNotCancelOnFailuresWhenRunning", func(t *testing.T) {
		// When hedge is already running, failures don't cancel other attempts
		// This ensures resilience - other hedges might succeed even if one fails
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 2,
										Delay:    common.Duration(30 * time.Millisecond), // Short delay
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc3",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc3.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary request - fails after hedge starts
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(50 * time.Millisecond). // Fails after hedge starts (30ms)
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: First hedge - also fails
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(503).
			Delay(40 * time.Millisecond).
			JSON(map[string]interface{}{
				"error": "service unavailable",
			})

		// rpc3: Second hedge - succeeds
		gock.New("http://rpc3.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x333333",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		// Should succeed with result from rpc3, even though rpc1 and rpc2 failed
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x333333")
	})

	t.Run("RetryImmediateWhenPrimaryFailsBeforeHedge", func(t *testing.T) {
		// When primary fails before hedge delay, retry kicks in immediately
		// No waiting for hedge since it never starts
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 2,
										Delay:       common.Duration(0), // No retry delay
									},
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(200 * time.Millisecond), // Hedge delay
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: First attempt - fails immediately
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(10 * time.Millisecond). // Fails very quickly
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: Retry attempt - succeeds
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)
		start := time.Now()

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)
		elapsed := time.Since(start)

		// Should retry immediately without waiting for hedge delay
		// Total time: ~10ms (primary fail) + 50ms (retry execution) = ~60ms
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222")
		assert.Less(t, elapsed, 150*time.Millisecond, "Should not wait for hedge delay")
	})

	t.Run("RetryWaitsForHedgeWhenHedgeIsRunning", func(t *testing.T) {
		// When hedge is already running and primary fails, retry waits for hedge to complete
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 2,
										Delay:       common.Duration(0), // No retry delay
									},
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(30 * time.Millisecond), // Short hedge delay
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// First attempt: both fail but hedge is running
		// rpc1: Primary - fails after hedge starts
		gock.New("http://rpc1.localhost").
			Post("").
			Times(1).
			Reply(500).
			Delay(50 * time.Millisecond). // Fails after hedge starts (30ms)
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: Hedge - also fails
		gock.New("http://rpc2.localhost").
			Post("").
			Times(1).
			Reply(503).
			Delay(100 * time.Millisecond). // Takes longer
			JSON(map[string]interface{}{
				"error": "service unavailable",
			})

		// Second attempt (retry): succeeds
		gock.New("http://rpc1.localhost").
			Post("").
			Times(1).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xRetrySuccess",
			})

		start := time.Now()
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)
		elapsed := time.Since(start)

		// Should wait for hedge to complete before retry
		// Total time: ~30ms (hedge start) + 100ms (hedge complete) + 20ms (retry) = ~150ms
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0xRetrySuccess")
		assert.Greater(t, elapsed, 120*time.Millisecond, "Should wait for hedge to complete")
	})

	t.Run("HedgeCancelsOnSuccess", func(t *testing.T) {
		// Test that successful response cancels other hedges
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 2) // 2 hedges will be cancelled

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 2,
										Delay:    common.Duration(50 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc3",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc3.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - succeeds quickly
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			Delay(75 * time.Millisecond). // Succeeds before second hedge
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})

		// rpc2: First hedge - would succeed but gets cancelled
		gock.New("http://rpc2.localhost").
			Post("").
			Persist(). // Might be cancelled
			Reply(200).
			Delay(200 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// rpc3: Second hedge - would succeed but gets cancelled
		gock.New("http://rpc3.localhost").
			Post("").
			Persist(). // Might be cancelled
			Reply(200).
			Delay(200 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x333333",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		// Should get result from primary (rpc1) which succeeds first
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x111111")
	})

	t.Run("PrimaryFastSuccessHedgeNotTriggered", func(t *testing.T) {
		// Primary succeeds before hedge delay - hedge never starts
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // Second upstream never called

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(200 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - succeeds quickly
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond). // Much faster than hedge delay
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})

		// rpc2: Would be hedge but never called
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x111111")
	})

	t.Run("PrimarySlowSuccessHedgeFastSuccess", func(t *testing.T) {
		// Primary is slow but succeeds, hedge is fast and succeeds - hedge wins
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // Primary mock cancelled when hedge succeeds

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - slow but succeeds
		gock.New("http://rpc1.localhost").
			Post("").
			Persist(). // Might be cancelled
			Reply(200).
			Delay(500 * time.Millisecond). // Very slow
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})

		// rpc2: Hedge - fast and succeeds
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			Delay(50 * time.Millisecond). // Fast
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222") // Hedge wins
	})

	t.Run("PrimarySlowSuccessHedgeFastFail", func(t *testing.T) {
		// Primary is slow but succeeds, hedge is fast but fails - primary wins
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - slow but succeeds
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(300 * time.Millisecond). // Slow
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})

		// rpc2: Hedge - fast but fails
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(50 * time.Millisecond). // Fast fail
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x111111") // Primary wins despite hedge failing fast
	})

	t.Run("PrimaryFastFailBeforeHedge", func(t *testing.T) {
		// Primary fails before hedge starts - should return immediately
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 never called

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - fails quickly
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(50 * time.Millisecond). // Fast fail
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: Never called because primary fails before hedge starts
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Persist(). // Keep it pending
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		// Should fail immediately with primary error
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "internal server error")
	})

	t.Run("PrimaryFailsAfterHedgeStartsAndSucceeds", func(t *testing.T) {
		// Primary fails after hedge starts, hedge succeeds - request succeeds
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(50 * time.Millisecond), // Hedge starts quickly
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - fails after hedge has started
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(100 * time.Millisecond). // Fails after hedge starts (50ms)
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: Hedge - succeeds quickly
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(30 * time.Millisecond). // Quick success
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xHedgeSuccess",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		// Should succeed with hedge result
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0xHedgeSuccess")
	})

	t.Run("PrimarySlowFailHedgeFastSuccess", func(t *testing.T) {
		// Primary is slow and fails, hedge is fast and succeeds - hedge wins
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - slow and fails
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(300 * time.Millisecond). // Slow fail
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: Hedge - fast and succeeds
		gock.New("http://rpc2.localhost").
			Post("").
			Reply(200).
			Delay(50 * time.Millisecond). // Fast success
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222") // Hedge wins quickly
	})

	t.Run("BothFailDifferentErrorsBeforeHedge", func(t *testing.T) {
		// Primary fails before hedge starts - should return immediately with primary error
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1) // rpc2 never called

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - fails before hedge starts
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(50 * time.Millisecond). // Fails before hedge delay (100ms)
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: Never called because primary fails before hedge starts
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Persist(). // Keep it pending
			Reply(503).
			JSON(map[string]interface{}{
				"error": "service unavailable",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		// Should fail immediately with primary error (500)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "internal server error")
	})

	t.Run("BothFailDifferentErrorsWithHedgeRunning", func(t *testing.T) {
		// When hedge is already running and both fail, should wait for both
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(500 * time.Millisecond), // Short delay
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary - fails after hedge starts
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(500).
			Delay(800 * time.Millisecond). // Fails after hedge starts (500ms)
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: Hedge - also fails
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(503).
			Delay(900 * time.Millisecond). // Takes slightly longer
			JSON(map[string]interface{}{
				"error": "service unavailable",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		// Should wait for both to complete and return error
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "error")
	})

	t.Run("SingleUpstreamHedgeContinuesPrimary", func(t *testing.T) {
		// Test that with a single upstream, when hedge can't find an available upstream
		// (because primary is using it), the primary continues and succeeds
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(50 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// rpc1: Primary request - succeeds after hedge would trigger
		gock.New("http://rpc1.localhost").
			Post("").
			Reply(200).
			Delay(200 * time.Millisecond). // Slower than hedge delay
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		// Should succeed with primary result even though hedge couldn't find an upstream
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x111111")
	})

	t.Run("SingleUpstreamHedgeContinuesPrimary", func(t *testing.T) {
		// Test that with a single upstream, when hedge can't find an available upstream
		// (because primary is using it), the primary continues and succeeds
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										Delay:    common.Duration(10 * time.Millisecond),
										MaxCount: 2,
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
		}

		// rpc1: Primary request - slow but succeeds
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(200 * time.Millisecond). // Slow response
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		body := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`
		statusCode, _, respBody := sendRequest(body, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		var resp map[string]interface{}
		err := json.Unmarshal([]byte(respBody), &resp)
		require.NoError(t, err)
		assert.Equal(t, "0x111111", resp["result"])
	})

	t.Run("BothSuccessPrimaryWins", func(t *testing.T) {
		// Both primary and hedge succeed, but primary is faster
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										Delay:    common.Duration(300 * time.Millisecond),
										MaxCount: 2,
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
		}

		// rpc1: Primary - fast success
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(350 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xPrimary",
			})

		// rpc2: Hedge - slower success
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(400 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xHedge",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		body := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`
		statusCode, _, respBody := sendRequest(body, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		var resp map[string]interface{}
		err = json.Unmarshal([]byte(respBody), &resp)
		require.NoError(t, err)
		assert.Equal(t, "0xPrimary", resp["result"]) // Primary wins
	})

	t.Run("BothSuccessHedgeWins", func(t *testing.T) {
		// Both primary and hedge succeed, but hedge is faster
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										Delay:    common.Duration(50 * time.Millisecond),
										MaxCount: 2,
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
		}

		// rpc1: Primary - slow success
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(200 * time.Millisecond). // Would finish at 200ms
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xPrimary",
			})

		// rpc2: Hedge - fast success
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(20 * time.Millisecond). // Finishes at 70ms (50ms hedge delay + 20ms)
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xHedge",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		body := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`
		statusCode, _, respBody := sendRequest(body, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		var resp map[string]interface{}
		err = json.Unmarshal([]byte(respBody), &resp)
		require.NoError(t, err)
		assert.Equal(t, "0xHedge", resp["result"]) // Hedge wins
	})

	t.Run("HedgeWithRetryOnPrimaryFailure", func(t *testing.T) {
		// Primary fails, hedge fails, then retry kicks in and succeeds
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 3,
										Delay:       common.Duration(10 * time.Millisecond),
									},
									Hedge: &common.HedgePolicyConfig{
										Delay:    common.Duration(50 * time.Millisecond),
										MaxCount: 2,
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
		}

		// First attempt: both fail
		// rpc1: Primary - fails fast
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Times(1).
			Reply(500).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{
				"error": "internal server error",
			})

		// rpc2: Hedge - also fails
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Times(1).
			Reply(503).
			Delay(30 * time.Millisecond).
			JSON(map[string]interface{}{
				"error": "service unavailable",
			})

		// Second attempt (retry): succeeds
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Times(1).
			Reply(200).
			Delay(10 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xRetrySuccess",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		time.Sleep(500 * time.Millisecond)

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		body := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`
		statusCode, _, respBody := sendRequest(body, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		var resp map[string]interface{}
		err = json.Unmarshal([]byte(respBody), &resp)
		require.NoError(t, err)
		assert.Equal(t, "0xRetrySuccess", resp["result"]) // Retry succeeds
	})

	t.Run("SimpleHedgePolicyAvoidSameUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(10 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(10 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(100 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x111111")
	})

	t.Run("SimpleHedgePolicyWithoutOtherPoliciesBatchingEnabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(10 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Timeout: nil,
									Retry:   nil,
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Timeout: nil,
									Retry:   nil,
									Hedge:   nil,
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  common.Duration(5 * time.Millisecond),
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Timeout: nil,
									Retry:   nil,
									Hedge:   nil,
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  common.Duration(5 * time.Millisecond),
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := string(util.SafeReadBody(request))
				return strings.Contains(body, "eth_getBalance") && strings.Contains(body, "111")
			}).
			Reply(200).
			Delay(1000 * time.Millisecond).
			JSON([]interface{}{
				map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      111,
					"result":  "0x111_SLOW",
				}})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance") && strings.Contains(body, "111")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON([]interface{}{map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"result":  "0x111_FAST",
			}})

		// Set up test fixtures
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":111}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x111_FAST")
	})

	t.Run("HedgeReturnsResponseFromSecondUpstreamWhenFirstUpstreamTimesOut", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(2 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: nil,
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(50 * time.Millisecond),
									},
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(1000 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(100 * time.Millisecond),
									},
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(500 * time.Millisecond),
									},
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(200 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(20 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222")
	})

	t.Run("HedgeReturnsResponseFromSecondUpstreamWhenFirstUpstreamReturnsSlowBillingIssues", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(2 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: nil,
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(100 * time.Millisecond),
									},
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(1000 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(1000 * time.Millisecond),
									},
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(1000 * time.Millisecond),
									},
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(402).
			Delay(500 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32005,
					"message": "Monthly capacity limit exceeded. Visit https://dashboard.alchemyapi.io/settings/billing to upgrade your scaling policy for continued service.",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222")
	})

	t.Run("ServerTimesOutBeforeHedgeReturnsResponseFromSecondUpstreamWhenFirstUpstreamHasTimeout", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(30 * time.Millisecond).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: nil,
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(10 * time.Millisecond),
									},
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(300 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(150 * time.Millisecond),
									},
									Retry: nil,
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(200 * time.Millisecond),
									},
									Retry: nil,
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(402).
			Delay(5000 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32005,
					"message": "Monthly capacity limit exceeded. Visit https://dashboard.alchemyapi.io/settings/billing to upgrade your scaling policy for continued service.",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(150 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, ErrHandlerTimeout.Error())
	})

	t.Run("HedgeDiscardsSlowerCallFirstRequestCancelled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				// Enough total server time to let hedged calls finish.
				MaxTimeout: common.Duration(2 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 123},
							Failsafe: []*common.FailsafeConfig{
								// We allow a 2-attempt hedge: the "original" plus 1 "hedge".
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(10 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Mock #1 (slower) – "original request"
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance") && strings.Contains(string(body), "SLOW")
			}).
			Reply(200).
			Delay(300 * time.Millisecond). // This will be canceled if the hedge wins
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      99,
				"result":  "0xSLOW",
			})

		// Mock #2 (faster) – "hedge request"
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance") && strings.Contains(string(body), "FAST")
			}).
			Reply(200).
			Delay(50 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      99,
				"result":  "0xFAST",
			})

		// Launch the test server with above config
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// We'll ask for a single "eth_getBalance" call but pass distinct markers SLOW/FAST to
		// ensure that Gock can differentiate which is the "original" vs. "hedge" request body.
		// In real usage, hedged calls have identical payloads. Here we just need to disambiguate
		// the two mocks so we can see which is canceled, which returns successfully, etc.
		jsonBody := `{"jsonrpc":"2.0","id":99,"method":"eth_getBalance","params":["0xSLOW","true","0xFAST","true"]}`

		statusCode, _, body := sendRequest(jsonBody, nil, nil)

		// From the user's perspective, we must see a 200 + "0xFAST" rather than "context canceled"
		assert.Equal(t, http.StatusOK, statusCode, "Expected 200 OK from hedged request")
		assert.Contains(t, body, "0xFAST", "Expected final result from faster hedge call")
		assert.NotContains(t, body, "context canceled", "Must never return 'context canceled' to the user")
	})
	t.Run("HedgeDiscardsSlowerCallSecondRequestCancelled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				// Enough total server time to let hedged calls finish.
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 123},
							Failsafe: []*common.FailsafeConfig{
								// We allow a 2-attempt hedge: the "original" plus 1 "hedge".
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(10 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Mock #1 (faster) – "original request"
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(300 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      99,
				"result":  "0xFAST",
			})

		// Mock #2 (slower) – "hedge request"
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(5000 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      99,
				"result":  "0xSLOW",
			})

		// Launch the test server with above config
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		jsonBody := `{"jsonrpc":"2.0","id":99,"method":"eth_getBalance","params":["0x1234"]}`

		statusCode, _, body := sendRequest(jsonBody, nil, nil)

		// From the user's perspective, we must see a 200 + "0xFAST" rather than "context canceled"
		assert.Equal(t, http.StatusOK, statusCode, "Expected 200 OK from hedged request")
		assert.Contains(t, body, "0xFAST", "Expected final result from faster hedge call")
		assert.NotContains(t, body, "context canceled", "Must never return 'context canceled' to the user")
	})

	t.Run("HedgeReturnsFirstResponseWhenInitiallySlowRequestCompletesFaster", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				// Enough total server time to let hedged calls finish
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 123},
							Failsafe: []*common.FailsafeConfig{
								// Network-level hedge configuration
								{
									Hedge: &common.HedgePolicyConfig{
										MaxCount: 1,
										Delay:    common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.FALSE,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Mock first upstream - initially slow but eventually completes faster
		// The key feature is that this request looks like it will be slow (it waits 300ms before responding)
		// but actually finishes before the second request
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(300 * time.Millisecond). // Enough delay to trigger hedge but not too long
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1234567", // Valid hex string for primary upstream
			})

		// Mock second upstream - hedge request goes to this upstream
		// This request is much slower overall (1000ms)
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(1000 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x7890123", // Valid hex string for backup upstream
			})

		// Launch test server with config
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Time the request to verify we don't wait for the slower hedge
		start := time.Now()
		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1}`, nil, nil)
		elapsed := time.Since(start)

		// Verify correct response was returned
		assert.Equal(t, http.StatusOK, statusCode, "Expected 200 OK response")
		assert.Contains(t, body, "0x1234567", "Expected result from primary upstream")

		// Check that response time is approximately equal to the first request latency
		// and NOT waiting for the second request to finish (which would be ~1000ms)
		// We add a small buffer for processing time
		assert.Less(t, elapsed, 600*time.Millisecond,
			"Response time should be close to first request latency (~300ms) and not wait for hedge request to complete (~1000ms)")
		assert.GreaterOrEqual(t, elapsed, 300*time.Millisecond,
			"Response time should be at least equal to the simulated primary upstream delay")

		// Make sure we don't return context canceled errors to the user
		assert.NotContains(t, body, "context canceled", "Must never return 'context canceled' to the user")
	})
}
