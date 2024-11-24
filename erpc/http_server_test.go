package erpc

import (
	"context"
	"fmt"
	"io"

	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHttpServer_RaceTimeouts(t *testing.T) {
	t.Run("ConcurrentRequestsWithTimeouts", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(50).
			Reply(200).
			Delay(2000 * time.Millisecond). // Delay longer than the server timeout
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Setup
		logger := log.Logger
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("500ms"), // Set a very short timeout for testing
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		erpcInstance, err := NewERPC(ctx, &logger, nil, cfg)
		require.NoError(t, err)

		httpServer := NewHttpServer(ctx, &logger, cfg.Server, cfg.Admin, erpcInstance)

		// Start the server on a random port
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port

		go func() {
			err := httpServer.server.Serve(listener)
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("Server error: %v", err)
			}
		}()
		defer httpServer.server.Shutdown(ctx)

		// Wait for the server to start
		time.Sleep(100 * time.Millisecond)

		baseURL := fmt.Sprintf("http://localhost:%d", port)

		sendRequest := func() (int, string) {
			body := strings.NewReader(
				fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["%d", false],"id":1}`, rand.Intn(100000000)),
			)
			req, err := http.NewRequest("POST", baseURL+"/test_project/evm/1", body)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{
				Timeout: 1000 * time.Millisecond, // Set a client timeout longer than the server timeout
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, err.Error()
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return resp.StatusCode, err.Error()
			}
			return resp.StatusCode, string(respBody)
		}

		const concurrentRequests = 50
		var wg sync.WaitGroup
		results := make([]struct {
			statusCode int
			body       string
		}, concurrentRequests)

		for i := 0; i < concurrentRequests; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				results[index].statusCode, results[index].body = sendRequest()
			}(i)
		}

		wg.Wait()

		for _, result := range results {
			if result.statusCode != http.StatusGatewayTimeout {
				t.Errorf("unexpected status code: %d", result.statusCode)
			}
			assert.Contains(t, result.body, "timeout")
		}
	})

	t.Run("RapidSuccessiveRequests", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("/").
			Times(10).
			Reply(200).
			Delay(1000 * time.Millisecond). // Delay longer than the server timeout
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x333333",
			})

		// Setup
		logger := log.Logger
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("500ms"), // Set a very short timeout for testing
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		erpcInstance, err := NewERPC(ctx, &logger, nil, cfg)
		require.NoError(t, err)

		httpServer := NewHttpServer(ctx, &logger, cfg.Server, cfg.Admin, erpcInstance)

		// Start the server on a random port
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port

		go func() {
			err := httpServer.server.Serve(listener)
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("Server error: %v", err)
			}
		}()
		defer httpServer.server.Shutdown(ctx)

		// Wait for the server to start
		time.Sleep(100 * time.Millisecond)

		baseURL := fmt.Sprintf("http://localhost:%d", port)

		sendRequest := func() (int, string) {
			body := strings.NewReader(
				fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["%d", false],"id":1}`, rand.Intn(100000000)),
			)
			req, err := http.NewRequest("POST", baseURL+"/test_project/evm/1", body)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{
				Timeout: 1000 * time.Millisecond, // Set a client timeout longer than the server timeout
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, err.Error()
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return resp.StatusCode, err.Error()
			}
			return resp.StatusCode, string(respBody)
		}

		wg := sync.WaitGroup{}
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				statusCode, body := sendRequest()
				assert.Equal(t, http.StatusGatewayTimeout, statusCode)
				assert.Contains(t, body, "http request handling timeout")
			}()
		}
		wg.Wait()
	})

	t.Run("MixedTimeoutAndNonTimeoutRequests", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		totalReqs := 100

		for i := 0; i < totalReqs; i++ {
			var delay time.Duration
			if i%2 == 0 {
				delay = 1 * time.Millisecond // shorter than the server timeout
			} else {
				delay = 700 * time.Millisecond // longer than the server timeout
			}
			gock.New("http://rpc1.localhost").
				Post("/").
				Times(1).
				ReplyFunc(func(r *gock.Response) {
					r.Status(200)
					r.JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      rand.Intn(100_000_000),
						"result": map[string]interface{}{
							"blockNumber": rand.Intn(100000000),
						},
					})
				}).
				Delay(delay)
		}

		// Setup
		logger := log.Logger
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("500ms"), // Set a very short timeout for testing
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		erpcInstance, err := NewERPC(ctx, &logger, nil, cfg)
		require.NoError(t, err)

		httpServer := NewHttpServer(ctx, &logger, cfg.Server, cfg.Admin, erpcInstance)

		// Start the server on a random port
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port

		go func() {
			err := httpServer.server.Serve(listener)
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("Server error: %v", err)
			}
		}()
		defer httpServer.server.Shutdown(ctx)

		// Wait for the server to start
		time.Sleep(100 * time.Millisecond)

		baseURL := fmt.Sprintf("http://localhost:%d", port)

		sendRequest := func() (int, string) {
			body := strings.NewReader(
				fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["%d", false],"id":1}`, rand.Intn(100000000)),
			)
			req, err := http.NewRequest("POST", baseURL+"/test_project/evm/1", body)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			client := &http.Client{
				Timeout: 1000 * time.Millisecond, // Set a client timeout longer than the server timeout
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, err.Error()
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return resp.StatusCode, err.Error()
			}
			return resp.StatusCode, string(respBody)
		}

		var wg sync.WaitGroup
		results := make([]struct {
			statusCode int
			body       string
		}, totalReqs)

		for i := 0; i < totalReqs; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				results[index].statusCode, results[index].body = sendRequest()
			}(i)
			time.Sleep(50 * time.Millisecond)
		}
		wg.Wait()

		timeouts := 0
		successes := 0
		for _, result := range results {
			if result.statusCode == http.StatusGatewayTimeout {
				timeouts++
				assert.Contains(t, result.body, "timeout")
			} else {
				successes++
				assert.Contains(t, result.body, "blockNumber")
			}
		}

		fmt.Printf("Timeouts: %d, Successes: %d\n", timeouts, successes)
		assert.True(t, timeouts > 0, "Expected some timeouts")
		assert.True(t, successes > 0, "Expected some successes")
	})
}

func TestHttpServer_ManualTimeoutScenarios(t *testing.T) {
	t.Run("ServerHandlerTimeout", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("10ms"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "200ms",
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "100ms",
								},
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
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(1 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "http request handling timeout")
	})

	t.Run("NetworkTimeoutBatchingEnabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("50000ms"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "30ms",
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "300ms",
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  "1ms",
								BatchMaxSize:  1,
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
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(100 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		// TODO in here we should ONLY see network-level timeout but currently sometimes we see upstream timeout, we must fix this
		assert.Contains(t, body, "timeout") // policy exceeded on network-level
	})

	t.Run("NetworkTimeoutBatchingDisabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("10s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "100ms",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "5s",
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
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(30 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		// TODO in here we should ONLY see network-level timeout but currently sometimes we see upstream timeout, we must fix this
		assert.Contains(t, body, "timeout") // policy exceeded on network-level
	})

	t.Run("UpstreamRequestTimeoutBatchingEnabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("2s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "1s",
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "100ms",
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  "5ms",
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
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(30 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "1 timeout")
	})

	t.Run("UpstreamRequestTimeoutBatchingDisabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("10s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "5s",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "100ms",
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
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(300 * time.Second).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "1 timeout")
	})

	t.Run("SameTimeoutLowForServerAndNetwork", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("1ms"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "1ms",
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "5s",
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  "5ms",
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

		// Set up test fixtures
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "timeout")
	})

	t.Run("ServerTimeoutNoUpstreamNoNetworkTimeout", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("100ms"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:   nil,
								Hedge:   nil,
								Timeout: nil,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:   nil,
								Hedge:   nil,
								Timeout: nil,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  "5ms",
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
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(200 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		// TODO in here we should ONLY see http server timeout but currently sometimes we see upstream timeout, we must fix this
		assert.Contains(t, body, "timeout") // http request handling timeout
	})

	t.Run("MidServerHighNetworkLowUpstreamTimeoutBatchingDisabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("200ms"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "300ms",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "10ms",
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
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(1000 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "1 timeout")
	})
}

func TestHttpServer_HedgedRequests(t *testing.T) {
	t.Run("SimpleHedgePolicyWithoutOtherPoliciesBatchingDisabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("10s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Hedge: &common.HedgePolicyConfig{
									MaxCount: 1,
									Delay:    "10ms",
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
								ChainId: 1,
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
			Post("/").
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
			Post("/").
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
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222")
	})

	t.Run("SimpleHedgePolicyWithoutOtherPoliciesBatchingEnabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("10s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: nil,
								Retry:   nil,
								Hedge: &common.HedgePolicyConfig{
									MaxCount: 1,
									Delay:    "100ms",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: nil,
								Retry:   nil,
								Hedge:   nil,
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  "5ms",
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
			Post("/").
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
		gock.New("http://rpc1.localhost").
			Post("/").
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
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":111}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x111_FAST")
	})

	t.Run("HedgeReturnsResponseFromSecondUpstreamWhenFirstUpstreamTimesOut", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("1500ms"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: &common.HedgePolicyConfig{
									MaxCount: 1,
									Delay:    "100ms",
								},
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "2000ms",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "10ms",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "1000ms",
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
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			Delay(2000 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x111111",
			})

		gock.New("http://rpc2.localhost").
			Post("/").
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
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222")
	})

	t.Run("HedgeReturnsResponseFromSecondUpstreamWhenFirstUpstreamReturnsSlowBillingIssues", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("1000ms"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: &common.HedgePolicyConfig{
									MaxCount: 1,
									Delay:    "100ms",
								},
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "1000ms",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "1000ms",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "1000ms",
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
			Post("/").
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
			Post("/").
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
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x222222")
	})

	t.Run("ServerTimesOutBeforeHedgeReturnsResponseFromSecondUpstreamWhenFirstUpstreamHasTimeout", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("30ms"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: nil,
								Hedge: &common.HedgePolicyConfig{
									MaxCount: 1,
									Delay:    "10ms",
								},
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "300ms",
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "150ms",
								},
								Retry: nil,
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
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Timeout: &common.TimeoutPolicyConfig{
									Duration: "200ms",
								},
								Retry: nil,
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
			Post("/").
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
			Post("/").
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
		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, ErrHandlerTimeout.Error())
	})
}

func TestHttpServer_SingleUpstream(t *testing.T) {
	type testCase struct {
		name      string
		configure func(cfg *common.Config)
	}

	// Define your test cases
	testCases := []testCase{
		{
			name: "UpstreamSupportsBatch",
			configure: func(cfg *common.Config) {
				cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch = &common.TRUE
				cfg.Projects[0].Upstreams[0].JsonRpc.BatchMaxSize = 1
				cfg.Projects[0].Upstreams[0].JsonRpc.BatchMaxWait = "10ms"
			},
		},
		{
			name: "UpstreamDoesNotSupportBatch",
			configure: func(cfg *common.Config) {
				cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch = &common.FALSE
			},
		},
		{
			name: "CachingEnabled",
			configure: func(cfg *common.Config) {
				cfg.Database = &common.DatabaseConfig{
					EvmJsonRpcCache: &common.CacheConfig{
						Connectors: []*common.ConnectorConfig{
							{
								Id:     "mock",
								Driver: common.DriverMemory,
								Memory: &common.MemoryConnectorConfig{
									MaxItems: 1000,
								},
							},
						},
						Policies: []*common.CachePolicyConfig{
							{
								Network: "*",
								Method:  "*",
								TTL:     5 * time.Minute,
							},
						},
					},
				}
			},
		},
		{
			name: "CachingDisabled",
			configure: func(cfg *common.Config) {
				cfg.Database = &common.DatabaseConfig{
					EvmJsonRpcCache: nil,
				}
			},
		},
	}

	for _, tc := range testCases {
		// Capture the current value of tc
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Run("ConcurrentRequests", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
					cfg.Projects[0].Upstreams[0].JsonRpc.BatchMaxSize = 10
					cfg.Projects[0].Upstreams[0].JsonRpc.BatchMaxWait = "100ms"
					gock.New("http://rpc1.localhost").
						Post("/").
						Filter(func(request *http.Request) bool {
							return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
						}).
						Reply(200).
						JSON([]interface{}{
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      0,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      1,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      2,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      3,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      4,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      5,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      6,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      7,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      8,
								"result":  "0x444444",
							},
							map[string]interface{}{
								"jsonrpc": "2.0",
								"id":      9,
								"result":  "0x444444",
							},
						})
				}

				const concurrentRequests = 10

				var wg sync.WaitGroup
				results := make([]struct {
					statusCode int
					body       string
				}, concurrentRequests)

				for i := 0; i < concurrentRequests; i++ {
					if !*cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
						gock.New("http://rpc1.localhost").
							Post("/").
							Filter(func(request *http.Request) bool {
								b := util.SafeReadBody(request)
								return strings.Contains(b, fmt.Sprintf(`"id":%d`, i)) && strings.Contains(b, "eth_getBalance")
							}).
							Reply(200).
							Map(func(res *http.Response) *http.Response {
								sg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"0x444444"}`, i)
								res.Body = io.NopCloser(strings.NewReader(sg))
								return res
							})
					}
				}

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				for i := 0; i < concurrentRequests; i++ {
					wg.Add(1)
					go func(index int) {
						defer wg.Done()
						body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[%d],"id":%d}`, index, index)
						results[index].statusCode, results[index].body = sendRequest(body, nil, nil)
					}(i)
				}

				wg.Wait()

				for i, result := range results {
					assert.Equal(t, http.StatusOK, result.statusCode, "Status code should be 200 for request %d", i)

					var response map[string]interface{}
					err := sonic.Unmarshal([]byte(result.body), &response)
					assert.NoError(t, err, "Should be able to decode response for request %d", i)
					assert.Equal(t, "0x444444", response["result"], "Unexpected result for request %d", i)
				}
			})

			t.Run("InvalidJSON", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				statusCode, body := sendRequest(`{"invalid json`, nil, nil)

				assert.Equal(t, http.StatusBadRequest, statusCode)

				var errorResponse map[string]interface{}
				err := sonic.Unmarshal([]byte(body), &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				errStr, _ := sonic.Marshal(errorObj)
				assert.Contains(t, string(errStr), "failed to parse")
			})

			t.Run("UnsupportedMethod", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				cfg.Projects[0].Upstreams[0].IgnoreMethods = []string{}

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				gock.New("http://rpc1.localhost").
					Post("/").
					Reply(200).
					Map(func(res *http.Response) *http.Response {
						sg := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
						if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
							sg = "[" + sg + "]"
						}
						res.Body = io.NopCloser(strings.NewReader(sg))
						return res
					})

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"unsupported_method","params":[],"id":1}`, nil, nil)
				assert.Equal(t, http.StatusUnsupportedMediaType, statusCode)

				var errorResponse map[string]interface{}
				err := sonic.Unmarshal([]byte(body), &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				assert.Equal(t, float64(-32601), errorObj["code"])
			})

			t.Run("IgnoredMethod", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 1)

				cfg.Projects[0].Upstreams[0].IgnoreMethods = []string{"ignored_method"}

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				gock.New("http://rpc1.localhost").
					Post("/").
					Reply(200).
					Map(func(res *http.Response) *http.Response {
						sg := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
						if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
							sg = "[" + sg + "]"
						}
						res.Body = io.NopCloser(strings.NewReader(sg))
						return res
					})

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"ignored_method","params":[],"id":1}`, nil, nil)
				assert.Equal(t, http.StatusUnsupportedMediaType, statusCode)

				var errorResponse map[string]interface{}
				err := sonic.Unmarshal([]byte(body), &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				assert.Equal(t, float64(-32601), errorObj["code"])
			})

			t.Run("InvalidProjectID", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				_, _, baseURL, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				req, err := http.NewRequest("POST", baseURL+"/invalid_project/evm/1", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")

				client := &http.Client{
					Timeout: 10 * time.Second,
				}
				resp, err := client.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()

				assert.Equal(t, http.StatusNotFound, resp.StatusCode)

				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)

				var errorResponse map[string]interface{}
				err = sonic.Unmarshal(body, &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				assert.Contains(t, errorObj["message"], "project not configured")
			})

			t.Run("UpstreamLatencyAndTimeout", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				gock.New("http://rpc1.localhost").
					Post("/").
					Reply(200).
					Delay(6 * time.Second). // Delay longer than the server timeout
					JSON(map[string]interface{}{
						"jsonrpc": "2.0",
						"id":      1,
						"result":  "0x1111111",
					})

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

				assert.Equal(t, http.StatusGatewayTimeout, statusCode)

				var errorResponse map[string]interface{}
				err := sonic.Unmarshal([]byte(body), &errorResponse)
				require.NoError(t, err)

				assert.Contains(t, errorResponse, "error")
				errorObj := errorResponse["error"].(map[string]interface{})
				errStr, _ := sonic.Marshal(errorObj)
				assert.Contains(t, string(errStr), "timeout")
			})

			t.Run("UnexpectedPlainErrorResponseFromUpstream", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				gock.New("http://rpc1.localhost").
					Post("/").
					Times(1).
					Reply(200).
					BodyString("error code: 1015")

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

				assert.Equal(t, http.StatusTooManyRequests, statusCode)
				assert.Contains(t, body, "error code: 1015")
			})

			t.Run("UnexpectedServerErrorResponseFromUpstream", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				gock.New("http://rpc1.localhost").
					Post("/").
					Times(1).
					Reply(500).
					BodyString(`{"error":{"code":-39999,"message":"my funky error"}}`)

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

				assert.Equal(t, http.StatusInternalServerError, statusCode)
				assert.Contains(t, body, "-32603")
				assert.Contains(t, body, "my funky error")
			})

			t.Run("MissingIDInJsonRpcRequest", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				var id interface{}
				gock.New("http://rpc1.localhost").
					Post("/").
					Times(1).
					SetMatcher(gock.NewEmptyMatcher()).
					AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
						if !strings.Contains(req.URL.Host, "rpc1") {
							return false, nil
						}
						bodyBytes, err := io.ReadAll(req.Body)
						if err != nil {
							return false, err
						}
						if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
							idNode, err := sonic.Get(bodyBytes, 0, "id")
							if err != nil {
								t.Fatalf("Error getting id node (batch): %v", err)
								return false, err
							}
							id, err = idNode.Interface()
							if err != nil {
								t.Fatalf("Error getting id interface (batch): %v", err)
								return false, err
							}
						} else {
							idNode, err := sonic.Get(bodyBytes, "id")
							if err != nil {
								t.Fatalf("Error getting id node: %v", err)
								return false, err
							}
							id, err = idNode.Interface()
							if err != nil {
								t.Fatalf("Error getting id interface: %v", err)
								return false, err
							}
						}

						if id == nil || id == 0 || id == "" {
							t.Fatalf("Expected id to not be 0, got %v", id)
						}

						return true, nil
					}).
					Reply(200).
					Map(func(res *http.Response) *http.Response {
						var respTxt string
						if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
							respTxt = `[{"jsonrpc":"2.0","id":THIS_WILL_BE_REPLACED,"result":"0x123456"}]`
						} else {
							respTxt = `{"jsonrpc":"2.0","id":THIS_WILL_BE_REPLACED,"result":"0x123456"}`
						}
						idp, err := sonic.Marshal(id)
						require.NoError(t, err)
						res.Body = io.NopCloser(strings.NewReader(strings.Replace(respTxt, "THIS_WILL_BE_REPLACED", string(idp), 1)))
						return res
					})

				statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_traceDebug","params":[]}`, nil, nil)

				assert.Equal(t, http.StatusOK, statusCode)
				assert.Contains(t, body, "0x123456")
			})

			t.Run("AutoAddIDandJSONRPCFieldstoRequest", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				gock.New("http://rpc1.localhost").
					Post("/").
					Times(1).
					SetMatcher(gock.NewEmptyMatcher()).
					AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
						if !strings.Contains(req.URL.Host, "rpc1") {
							return false, nil
						}
						bodyBytes, err := io.ReadAll(req.Body)
						if err != nil {
							return false, err
						}
						bodyStr := string(bodyBytes)
						if !strings.Contains(bodyStr, "\"id\"") {
							t.Fatalf("No id found in request")
						}
						if !strings.Contains(bodyStr, "\"jsonrpc\"") {
							t.Fatalf("No jsonrpc found in request")
						}
						if !strings.Contains(bodyStr, "\"method\"") {
							t.Fatalf("No method found in request")
						}
						if !strings.Contains(bodyStr, "\"params\"") {
							t.Fatalf("No params found in request")
						}
						return true, nil
					}).
					Reply(200).
					BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x123456"}`)

				sendRequest(`{"method":"eth_traceDebug","params":[]}`, nil, nil)
			})

			t.Run("AlwaysPropagateUpstreamErrorDataField", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				gock.New("http://rpc1.localhost").
					Post("/").
					Reply(400).
					BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params","data":{"range":"the range 55074203 - 55124202 exceeds the range allowed for your plan (49999 > 2000)."}}}`)

				_, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[],"id":1}`, nil, nil)
				assert.Contains(t, body, "the range 55074203 - 55124202")
			})

			t.Run("KeepIDWhen0IsProvided", func(t *testing.T) {
				cfg := &common.Config{
					Server: &common.ServerConfig{
						MaxTimeout: util.StringPtr("5s"),
					},
					Projects: []*common.ProjectConfig{
						{
							Id: "test_project",
							Networks: []*common.NetworkConfig{
								{
									Architecture: common.ArchitectureEvm,
									Evm: &common.EvmNetworkConfig{
										ChainId: 1,
									},
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 1,
									},
									VendorName: "llama",
									JsonRpc: &common.JsonRpcUpstreamConfig{
										SupportsBatch: &common.FALSE,
									},
								},
							},
						},
					},
					RateLimiters: &common.RateLimiterConfig{},
				}

				if tc.configure != nil {
					tc.configure(cfg)
				}

				util.ResetGock()
				defer util.ResetGock()
				util.SetupMocksForEvmStatePoller()
				defer util.AssertNoPendingMocks(t, 0)

				// Set up test fixtures
				sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
				defer shutdown()

				gock.New("http://rpc1.localhost").
					Post("/").
					SetMatcher(gock.NewEmptyMatcher()).
					AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
						if !strings.Contains(req.URL.Host, "rpc1") {
							return false, nil
						}
						bodyBytes, err := io.ReadAll(req.Body)
						if err != nil {
							return false, err
						}
						bodyStr := string(bodyBytes)
						if !strings.Contains(bodyStr, "\"id\"") {
							fmt.Printf("ERROR: No id found in request")
							return false, nil
						}
						if bodyStr[0] == '[' {
							idNode, err := sonic.Get(bodyBytes, 0, "id")
							require.NoError(t, err)
							id, err := idNode.Int64()
							require.NoError(t, err)
							if id != 0 {
								fmt.Printf("ERROR: Expected id to be 0, got %d from body: %s", id, bodyStr)
								return false, nil
							} else {
								return true, nil
							}
						} else {
							idNode, err := sonic.Get(bodyBytes, "id")
							require.NoError(t, err)
							id, err := idNode.Int64()
							require.NoError(t, err)
							if id != 0 {
								fmt.Printf("ERROR: Expected id to be 0, got %d from body: %s", id, bodyStr)
								return false, nil
							} else {
								return true, nil
							}
						}
					}).
					Reply(200).
					BodyString(`{"jsonrpc":"2.0","id":0,"result":"0x123456"}`)

				sendRequest(`{"jsonrpc":"2.0","method":"eth_traceDebug","params":[],"id":0}`, nil, nil)
			})
		})
	}

	t.Run("ManyRequestsMultiplexedInOneUpstream", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:          nil,
								CircuitBreaker: nil,
								Hedge:          nil,
								Timeout:        nil,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:          nil,
								CircuitBreaker: nil,
								Hedge:          nil,
								Timeout:        nil,
							},
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(200).
			Delay(3000 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		wg := &sync.WaitGroup{}
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				statusCode, body := sendRequest(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":%d}`, i), nil, nil)
				assert.Equal(t, http.StatusOK, statusCode)
				assert.Contains(t, body, "0x123456")
			}(i)
		}
		wg.Wait()
	})
}

func TestHttpServer_MultipleUpstreams(t *testing.T) {
	t.Run("UpstreamNotAllowedByDirectiveViaHeaders", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("10s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:          nil,
								CircuitBreaker: nil,
								Hedge:          nil,
								Timeout:        nil,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:          nil,
								CircuitBreaker: nil,
								Hedge:          nil,
								Timeout:        nil,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:          nil,
								CircuitBreaker: nil,
								Hedge:          nil,
								Timeout:        nil,
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
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"result":  "0x1111111",
			})
		gock.New("http://rpc2.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      222,
				"result":  "0x2222222",
			})

		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode2, body2 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, map[string]string{
			"X-ERPC-Use-Upstream": "rpc2",
		}, nil)

		assert.Equal(t, http.StatusOK, statusCode2)
		assert.Contains(t, body2, "0x2222222")

		statusCode1, body1 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode1)
		assert.Contains(t, body1, "0x1111111")
	})

	t.Run("UpstreamNotAllowedByDirectiveViaQueryParams", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("10s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:          nil,
								CircuitBreaker: nil,
								Hedge:          nil,
								Timeout:        nil,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:          nil,
								CircuitBreaker: nil,
								Hedge:          nil,
								Timeout:        nil,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
							Failsafe: &common.FailsafeConfig{
								Retry:          nil,
								CircuitBreaker: nil,
								Hedge:          nil,
								Timeout:        nil,
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
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"result":  "0x1111111",
			})
		gock.New("http://rpc2.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      222,
				"result":  "0x2222222",
			})

		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode2, body2 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, map[string]string{
			"use-upstream": "rpc2",
		})

		assert.Equal(t, http.StatusOK, statusCode2)
		assert.Contains(t, body2, "0x2222222")

		statusCode1, body1 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode1)
		assert.Contains(t, body1, "0x1111111")
	})

	t.Run("RetryPendingDirectiveFromDefaults", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("10s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
							DirectiveDefaults: &common.DirectiveDefaultsConfig{
								RetryPending: util.BoolPtr(true),
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		// First RPC returns pending tx
		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"blockNumber": nil,
					"hash":        "0xabc",
				},
			})

		// Second RPC returns confirmed tx
		gock.New("http://rpc2.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"blockNumber": "0x1234",
					"hash":        "0xabc",
				},
			})

		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getTransactionByHash","params":["0xabc"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, `"blockNumber":null`)
	})
}

func TestHttpServer_IntegrationTests(t *testing.T) {
	t.Run("DrpcUnsupportedMethod", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					CORS: &common.CORSConfig{
						AllowedOrigins: []string{"https://erpc.cloud"},
						AllowedMethods: []string{"POST"},
					},
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "drpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "https://lb.drpc.org/ogrpc?network=ethereum",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
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

		gock.New("https://lb.drpc.org").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "trace_transaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"error": map[string]interface{}{
					"code":    -32601,
					"message": "the method trace_transaction does not exist/is not available",
				},
			})

		sendRequest, _, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, _ := sendRequest(`{"jsonrpc":"2.0","method":"trace_transaction","params":[],"id":111}`, nil, nil)
		assert.Equal(t, http.StatusBadRequest, statusCode)
	})

	t.Run("ReturnCorrectCORS", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					CORS: &common.CORSConfig{
						AllowedOrigins: []string{"https://erpc.cloud"},
						AllowedMethods: []string{"POST"},
					},
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "drpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "https://lb.drpc.org/ogrpc?network=ethereum",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 1,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		util.ResetGock()
		defer util.ResetGock()

		_, sendOptionsRequest, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, headers, _ := sendOptionsRequest("https://erpc.cloud")

		assert.Equal(t, http.StatusNoContent, statusCode)
		assert.Equal(t, "https://erpc.cloud", headers["Access-Control-Allow-Origin"])
		assert.Equal(t, "POST", headers["Access-Control-Allow-Methods"])
	})
}

func createServerTestFixtures(cfg *common.Config, t *testing.T) (
	func(body string, headers map[string]string, queryParams map[string]string) (int, string),
	func(host string) (int, map[string]string, string),
	string,
	func(),
) {
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())

	erpcInstance, err := NewERPC(ctx, &logger, nil, cfg)
	require.NoError(t, err)

	httpServer := NewHttpServer(ctx, &logger, cfg.Server, cfg.Admin, erpcInstance)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		err := httpServer.server.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Server error: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)

	sendRequest := func(body string, headers map[string]string, queryParams map[string]string) (int, string) {
		req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/test_project/evm/1", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		q := req.URL.Query()
		for k, v := range queryParams {
			q.Add(k, v)
		}
		req.URL.RawQuery = q.Encode()

		client := &http.Client{
			Timeout: 60 * time.Second,
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, err.Error()
		}
		return resp.StatusCode, string(respBody)
	}

	sendOptionsRequest := func(host string) (int, map[string]string, string) {
		req, err := http.NewRequestWithContext(ctx, "OPTIONS", baseURL+"/test_project/evm/1", nil)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if host != "" {
			req.Header.Set("Host", host)
			req.Header.Set("Origin", host)
		}
		client := &http.Client{
			Timeout: 10 * time.Second,
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err.Error()
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, nil, err.Error()
		}

		headers := make(map[string]string)
		for k, v := range resp.Header {
			headers[k] = v[0]
		}
		return resp.StatusCode, headers, string(respBody)
	}

	return sendRequest, sendOptionsRequest, baseURL, func() {
		httpServer.server.Shutdown(ctx)
		listener.Close()
		cancel()
		time.Sleep(30 * time.Millisecond) // So that batch processor stops
	}
}
