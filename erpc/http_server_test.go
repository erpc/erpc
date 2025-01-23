package erpc

import (
	"context"
	"fmt"
	"io"

	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "exceeded on network-level")
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "exceeded on network-level")
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "1 upstream timeout")
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "1 upstream timeout")
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "http request handling timeout")
	})

	t.Run("ServerTimeoutNoUpstreamNoNetworkTimeout", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("500ms"),
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
			Delay(1000 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x222222",
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "http request handling timeout")
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, "1 upstream timeout")
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":111}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x111_FAST")
	})

	t.Run("HedgeReturnsResponseFromSecondUpstreamWhenFirstUpstreamTimesOut", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("2000ms"),
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
									Delay:    "50ms",
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
									Duration: "100ms",
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
									Duration: "500ms",
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
				"result":  "0x111111",
			})

		gock.New("http://rpc2.localhost").
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusGatewayTimeout, statusCode)
		assert.Contains(t, body, ErrHandlerTimeout.Error())
	})

	t.Run("HedgeDiscardsSlowerCall_FirstRequestCancelled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				// Enough total server time to let hedged calls finish.
				MaxTimeout: util.StringPtr("2s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 1},
							Failsafe: &common.FailsafeConfig{
								// We allow a 2-attempt hedge: the "original" plus 1 "hedge".
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

		// Mock #1 (slower) – "original request"
		gock.New("http://rpc1.localhost").
			Post("/").
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
		gock.New("http://rpc1.localhost").
			Post("/").
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

		statusCode, body := sendRequest(jsonBody, nil, nil)

		// From the user's perspective, we must see a 200 + "0xFAST" rather than "context canceled"
		assert.Equal(t, http.StatusOK, statusCode, "Expected 200 OK from hedged request")
		assert.Contains(t, body, "0xFAST", "Expected final result from faster hedge call")
		assert.NotContains(t, body, "context canceled", "Must never return 'context canceled' to the user")
	})

	t.Run("HedgeDiscardsSlowerCall_SecondRequestCancelled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				// Enough total server time to let hedged calls finish.
				MaxTimeout: util.StringPtr("2s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 1},
							Failsafe: &common.FailsafeConfig{
								// We allow a 2-attempt hedge: the "original" plus 1 "hedge".
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

		// Mock #1 (faster) – "hedge request"
		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance") && strings.Contains(string(body), "FAST")
			}).
			Reply(200).
			Delay(300 * time.Millisecond). // This will be canceled if the hedge wins
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      99,
				"result":  "0xFAST",
			})

		// Mock #2 (slower) – "original request"
		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance") && strings.Contains(string(body), "SLOW")
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

		// We'll ask for a single "eth_getBalance" call but pass distinct markers SLOW/FAST to
		// ensure that Gock can differentiate which is the "original" vs. "hedge" request body.
		// In real usage, hedged calls have identical payloads. Here we just need to disambiguate
		// the two mocks so we can see which is canceled, which returns successfully, etc.
		jsonBody := `{"jsonrpc":"2.0","id":99,"method":"eth_getBalance","params":["0xSLOW","true","0xFAST","true"]}`

		statusCode, body := sendRequest(jsonBody, nil, nil)

		// From the user's perspective, we must see a 200 + "0xFAST" rather than "context canceled"
		assert.Equal(t, http.StatusOK, statusCode, "Expected 200 OK from hedged request")
		assert.Contains(t, body, "0xFAST", "Expected final result from faster hedge call")
		assert.NotContains(t, body, "context canceled", "Must never return 'context canceled' to the user")
	})
}

func TestHttpServer_SingleUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

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
			util.ResetGock()
			defer util.ResetGock()

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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
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
				assert.Contains(t, errorObj["message"], "not configured")
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
				sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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

		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Times(2).
			Reply(200).
			Delay(3000 * time.Millisecond).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
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
}

func TestHttpServer_IntegrationTests(t *testing.T) {
	t.Run("SuccessRequestWithNoParams", func(t *testing.T) {
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
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "https://rpc1.localhost",
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

		gock.New("https://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "trace_transaction")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"result":  "0x1111111",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_transaction","id":111}`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x1111111")
	})

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

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, _ := sendRequest(`{"jsonrpc":"2.0","method":"trace_transaction","params":[],"id":111}`, nil, nil)
		assert.Equal(t, http.StatusUnsupportedMediaType, statusCode)
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

		_, sendOptionsRequest, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, headers, _ := sendOptionsRequest("https://erpc.cloud")

		assert.Equal(t, http.StatusNoContent, statusCode)
		assert.Equal(t, "https://erpc.cloud", headers["Access-Control-Allow-Origin"])
		assert.Equal(t, "POST", headers["Access-Control-Allow-Methods"])
	})

	t.Run("KnownOrigin_Headers_Forwarded", func(t *testing.T) {
		// When the origin is known, we send CORS headers and forward the request to the upstream
		// no matter what the NoHeadersForUnknownOrigins setting is.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					CORS: &common.CORSConfig{
						AllowedOrigins:             []string{"https://known.origin"},
						NoHeadersForUnknownOrigins: util.BoolPtr(true),
						AllowedMethods:             []string{"POST", "GET", "OPTIONS"},
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
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
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
		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xABCD",
			})

		sendRequest, sendOptionsRequest, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		// (A) Send OPTIONS request from known origin => expect 204, CORS headers
		statusCode, headers, _ := sendOptionsRequest("https://known.origin")
		assert.Equal(t, http.StatusNoContent, statusCode, "OPTIONS for known origin should be 204")
		assert.Equal(t, "https://known.origin", headers["Access-Control-Allow-Origin"])
		assert.Equal(t, "POST, GET, OPTIONS", headers["Access-Control-Allow-Methods"])

		// (B) Send a POST JSON-RPC request from known origin => expect 200
		reqBody := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
		statusCode, respBody := sendRequest(reqBody, map[string]string{"Origin": "https://known.origin"}, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, respBody, `"result":"0xABCD"`)
	})

	t.Run("UnknownOrigin_NoHeaders_Forwarded", func(t *testing.T) {
		// Setup with NoHeadersForUnknownOrigins = false means we do *not* block unknown origin,
		// but we do *not* send any CORS response headers either.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					CORS: &common.CORSConfig{
						AllowedOrigins:             []string{"https://known.origin"},
						NoHeadersForUnknownOrigins: util.BoolPtr(false),
						AllowedMethods:             []string{"POST", "GET", "OPTIONS"},
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
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
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
		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xABCD",
			})

		sendRequest, sendOptionsRequest, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		// (A) Send OPTIONS request from unknown origin => expect 204, no CORS headers
		statusCode, headers, _ := sendOptionsRequest("https://unknown.origin")
		assert.Equal(t, http.StatusNoContent, statusCode, "OPTIONS for unknown origin should be 204")
		assert.Empty(t, headers["Access-Control-Allow-Origin"])
		assert.Empty(t, headers["Access-Control-Allow-Methods"])

		// (B) Send a POST JSON-RPC request from unknown origin => expect 200
		reqBody := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
		statusCode, respBody := sendRequest(reqBody, map[string]string{"Origin": "https://unknown.origin"}, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, respBody, `"result":"0xABCD"`)
	})

	t.Run("UnknownOrigin_NoHeaders_NotForwarded", func(t *testing.T) {
		// Setup with NoHeadersForUnknownOrigins = true means we *do* block unknown origin,
		// and we do *not* send any CORS response headers either.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					CORS: &common.CORSConfig{
						AllowedOrigins:             []string{"https://known.origin"},
						NoHeadersForUnknownOrigins: util.BoolPtr(true),
						AllowedMethods:             []string{"POST", "GET", "OPTIONS"},
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
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
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
		gock.New("http://rpc1.localhost").
			Post("/").
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xABCD",
			})

		sendRequest, sendOptionsRequest, _, shutdown := createServerTestFixtures(cfg, t)
		defer shutdown()

		// (A) Send OPTIONS request from unknown origin => expect 204, no CORS headers
		statusCode, headers, _ := sendOptionsRequest("https://unknown.origin")
		assert.Equal(t, http.StatusNoContent, statusCode, "OPTIONS for unknown origin should be 204")
		assert.Empty(t, headers["Access-Control-Allow-Origin"])
		assert.Empty(t, headers["Access-Control-Allow-Methods"])
		assert.Empty(t, headers["Access-Control-Allow-Headers"])

		// (B) Send a POST JSON-RPC request from unknown origin => expect 200
		reqBody := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
		statusCode, _ = sendRequest(reqBody, map[string]string{"Origin": "https://unknown.origin"}, nil)
		assert.Equal(t, http.StatusForbidden, statusCode)
	})

}

func TestHttpServer_ParseUrlPath(t *testing.T) {
	tests := []struct {
		name               string
		path               string
		method             string
		preSelectedProject string
		preSelectedArch    string
		preSelectedChain   string
		wantProject        string
		wantArch           string
		wantChain          string
		wantAdmin          bool
		wantHealthcheck    bool
		wantErr            bool
		errContains        string
	}{
		{
			name:        "Basic path with all segments",
			path:        "/myproject/evm/1",
			method:      "POST",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "1",
			wantAdmin:   false,
			wantErr:     false,
		},
		{
			name:        "Admin endpoint",
			path:        "/admin",
			method:      "POST",
			wantProject: "",
			wantArch:    "",
			wantChain:   "",
			wantAdmin:   true,
			wantErr:     false,
		},
		{
			name:        "Admin endpoint with OPTIONS",
			path:        "/admin",
			method:      "OPTIONS",
			wantProject: "",
			wantArch:    "",
			wantChain:   "",
			wantAdmin:   true,
			wantErr:     false,
		},
		{
			name:            "Root healthcheck GET",
			path:            "/",
			method:          "GET",
			wantProject:     "",
			wantArch:        "",
			wantChain:       "",
			wantAdmin:       false,
			wantHealthcheck: true,
			wantErr:         false,
		},
		{
			name:            "Root healthcheck with empty path GET",
			path:            "",
			method:          "GET",
			wantProject:     "",
			wantArch:        "",
			wantChain:       "",
			wantAdmin:       false,
			wantHealthcheck: true,
			wantErr:         false,
		},
		{
			name:             "Project healthcheck",
			path:             "/myproject/healthcheck",
			method:           "GET",
			preSelectedArch:  "evm",
			preSelectedChain: "1",
			wantProject:      "myproject",
			wantArch:         "evm",
			wantChain:        "1",
			wantAdmin:        false,
			wantHealthcheck:  true,
			wantErr:          false,
		},
		{
			name:            "Full path healthcheck",
			path:            "/myproject/evm/1/healthcheck",
			method:          "GET",
			wantProject:     "myproject",
			wantArch:        "evm",
			wantChain:       "1",
			wantAdmin:       false,
			wantHealthcheck: true,
			wantErr:         false,
		},
		{
			name:               "Project preselected, valid path",
			path:               "/evm/1",
			method:             "POST",
			preSelectedProject: "myproject",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "1",
			wantErr:            false,
		},
		{
			name:               "Project and arch preselected, valid path",
			path:               "/1",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "1",
			wantErr:            false,
		},
		{
			name:               "All preselected, valid empty path",
			path:               "/",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			preSelectedChain:   "1",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "1",
			wantErr:            false,
		},
		{
			name:               "All preselected, healthcheck",
			path:               "/healthcheck",
			method:             "GET",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			preSelectedChain:   "1",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "1",
			wantHealthcheck:    true,
			wantErr:            false,
		},
		{
			name:        "Invalid path - too many segments",
			path:        "/myproject/evm/1/extra",
			method:      "POST",
			wantErr:     true,
			errContains: "must only provide",
		},
		{
			name:        "Invalid path - wrong architecture",
			path:        "/myproject/x/1",
			method:      "POST",
			wantErr:     true,
			errContains: "architecture is not valid",
		},
		{
			name:               "Invalid path - project preselected but missing arch",
			path:               "/1",
			method:             "POST",
			preSelectedProject: "myproject",
			wantErr:            true,
			errContains:        "architecture is not valid",
		},
		{
			name:               "Invalid path - project and chain preselected",
			path:               "/evm",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedChain:   "1",
			wantErr:            true,
			errContains:        "it is not possible to alias for project and chain WITHOUT architecture",
		},
		{
			name:        "Invalid path - empty architecture segment",
			path:        "/myproject//1",
			method:      "POST",
			wantErr:     true,
			errContains: "architecture is not valid",
		},
		{
			name:        "Root path POST without preselection",
			path:        "/",
			method:      "POST",
			wantErr:     true,
			errContains: "project is required",
		},
		{
			name:             "Architecture and chain preselected",
			path:             "/myproject",
			method:           "POST",
			preSelectedArch:  "evm",
			preSelectedChain: "1",
			wantProject:      "myproject",
			wantArch:         "evm",
			wantChain:        "1",
			wantErr:          false,
		},
		{
			name:             "Architecture and chain preselected with healthcheck",
			path:             "/myproject/healthcheck",
			method:           "GET",
			preSelectedArch:  "evm",
			preSelectedChain: "1",
			wantProject:      "myproject",
			wantArch:         "evm",
			wantChain:        "1",
			wantHealthcheck:  true,
			wantErr:          false,
		},
		{
			name:        "Only project provided",
			path:        "/myproject",
			method:      "POST",
			wantProject: "myproject",
			wantErr:     false,
		},
		{
			name:        "Project and architecture provided",
			path:        "/myproject/evm",
			method:      "POST",
			wantProject: "myproject",
			wantArch:    "evm",
			wantErr:     false,
		},
		{
			name:        "Project and chainId provided but not architecture",
			path:        "/myproject/1",
			method:      "POST",
			wantProject: "myproject",
			wantChain:   "1",
			wantErr:     true,
			errContains: "architecture is not valid",
		},
		{
			name:            "Project and chainId provided with preselected architecture",
			path:            "/myproject/1",
			method:          "POST",
			preSelectedArch: "evm",
			wantProject:     "myproject",
			wantArch:        "evm",
			wantChain:       "1",
			wantErr:         false,
		},
		{
			name:               "Only architecture provided",
			path:               "/evm",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedChain:   "1",
			wantErr:            true,
			errContains:        "it is not possible",
		},
		{
			name:               "Only chainId provided",
			path:               "/1",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "1",
			wantErr:            false,
		},
		{
			name:               "Architecture and chainId provided",
			path:               "/evm/1",
			method:             "POST",
			preSelectedProject: "myproject",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "1",
			wantErr:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &HttpServer{}
			r := &http.Request{
				Method: tt.method,
				URL: &url.URL{
					Path: tt.path,
				},
			}

			project, arch, chain, isAdmin, isHealth, err := s.parseUrlPath(r, tt.preSelectedProject, tt.preSelectedArch, tt.preSelectedChain)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantProject, project)
			assert.Equal(t, tt.wantArch, arch)
			assert.Equal(t, tt.wantChain, chain)
			assert.Equal(t, tt.wantAdmin, isAdmin)
			assert.Equal(t, tt.wantHealthcheck, isHealth)
		})
	}
}

func TestHttpServer_HandleHealthCheck(t *testing.T) {
	tests := []struct {
		name         string
		setupServer  func() *HttpServer
		projectId    string
		wantStatus   int
		wantBody     string
		wantCacheHit bool
	}{
		{
			name: "Basic healthcheck",
			setupServer: func() *HttpServer {
				pp := &PreparedProject{
					upstreamsRegistry: upstream.NewUpstreamsRegistry(context.TODO(), &zerolog.Logger{}, "", nil, nil, nil, nil, nil, 0*time.Second),
				}
				pp.networksRegistry = NewNetworksRegistry(pp, context.TODO(), pp.upstreamsRegistry, nil, nil, nil, &zerolog.Logger{})
				return &HttpServer{
					logger: &zerolog.Logger{},
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
				}
			},
			projectId:  "test",
			wantStatus: http.StatusOK,
			wantBody:   `OK`,
		},
		{
			name: "Project not found",
			setupServer: func() *HttpServer {
				return &HttpServer{
					logger: &zerolog.Logger{},
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{},
						},
					},
				}
			},
			projectId:  "nonexistent",
			wantStatus: http.StatusNotFound,
			wantBody:   "not configured",
		},
		{
			name: "Root healthcheck",
			setupServer: func() *HttpServer {
				pp := &PreparedProject{
					upstreamsRegistry: upstream.NewUpstreamsRegistry(context.TODO(), &zerolog.Logger{}, "", nil, nil, nil, nil, nil, 0*time.Second),
				}
				pp.networksRegistry = NewNetworksRegistry(pp, context.TODO(), pp.upstreamsRegistry, nil, nil, nil, &zerolog.Logger{})
				return &HttpServer{
					logger: &zerolog.Logger{},
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
				}
			},
			projectId:  "",
			wantStatus: http.StatusOK,
			wantBody:   `OK`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.setupServer()
			w := httptest.NewRecorder()
			startTime := time.Now()

			encoder := common.SonicCfg.NewEncoder(w)
			s.handleHealthCheck(w, &startTime, tt.projectId, encoder, func(code int, err error) {
				w.WriteHeader(code)
				encoder.Encode(map[string]string{"error": err.Error()})
			})

			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Contains(t, string(body), tt.wantBody)

			if tt.wantCacheHit {
				assert.Equal(t, "HIT", resp.Header.Get("X-ERPC-Cache"))
			}
		})
	}
}

func TestHttpServer_ProviderBasedUpstreams(t *testing.T) {
	t.Run("SimpleCallExistingNetwork", func(t *testing.T) {
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
					Providers: []*common.ProviderConfig{
						{
							Id:     "alchemy-prod",
							Vendor: "alchemy",
							Settings: map[string]interface{}{
								"apiKey": "test-key",
							},
							UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1234}`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")
	})

	t.Run("SimpleCallLazyLoadedNetwork", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Providers: []*common.ProviderConfig{
						{
							Id:     "alchemy-prod",
							Vendor: "alchemy",
							Settings: map[string]interface{}{
								"apiKey": "test-key",
							},
							UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1234}`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")
	})

	t.Run("RespectsOnlyNetworksWhenNoMatch", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Providers: []*common.ProviderConfig{
						{
							Id:     "alchemy-prod",
							Vendor: "alchemy",
							Settings: map[string]interface{}{
								"apiKey": "test-key",
							},
							OnlyNetworks:       []string{"evm:5", "evm:10"},
							UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// This request calls evm:1, which is not in the OnlyNetworks list
		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1234}`, nil, nil)
		assert.Equal(t, http.StatusNotFound, statusCode)
		assert.Contains(t, body, "no upstreams found",
			"expected network evm:1 to not be recognized because only evm:5 and evm:10 are allowed")
	})

	t.Run("RespectsOnlyNetworksWhenMatch", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Providers: []*common.ProviderConfig{
						{
							Id:     "alchemy-prod",
							Vendor: "alchemy",
							Settings: map[string]interface{}{
								"apiKey": "test-key",
							},
							OnlyNetworks:       []string{"evm:1", "evm:10"},
							UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		// This request calls evm:1, which is not in the OnlyNetworks list
		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1234}`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")
	})

	t.Run("EventuallyAddsUpstreamWhenSupportsNetworkFailsInitially", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Providers: []*common.ProviderConfig{
						{
							Id:                 "envio-prod",
							Vendor:             "envio",
							UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("https://12340001234.rpc.hypersync.xyz").
			Post("/").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(500).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Internal bummer error",
				},
			})

		gock.New("https://12340001234.rpc.hypersync.xyz").
			Post("/").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x2DF8579D2",
			})

		gock.New("https://12340001234.rpc.hypersync.xyz").
			Post("/").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[],"id":1234}`, nil, map[string]string{"chainId": "12340001234"})
		assert.Equal(t, http.StatusInternalServerError, statusCode)
		assert.Contains(t, body, "Internal bummer error")

		time.Sleep((3 * time.Second) + (100 * time.Millisecond))

		statusCode, body = sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[],"id":1234}`, nil, map[string]string{"chainId": "12340001234"})
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")
	})

	t.Run("InheritUpstreamDefaultsConfig", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					UpstreamDefaults: &common.UpstreamConfig{
						Failsafe: &common.FailsafeConfig{
							Hedge: &common.HedgePolicyConfig{
								Delay:    "10ms",
								MaxCount: 267,
							},
						},
					},
					Providers: []*common.ProviderConfig{
						{
							Id:                 "alchemy-prod",
							Vendor:             "alchemy",
							UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
							Settings: map[string]interface{}{
								"apiKey": "test-key",
							},
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1",
			})

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[],"id":1234}`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)

		upstreams := prj.upstreamsRegistry.GetNetworkUpstreams("evm:1")
		require.NotNil(t, upstreams)
		require.Equal(t, len(upstreams), 1)
		upsCfg := upstreams[0].Config()

		assert.Equalf(t, upsCfg.Failsafe.Hedge.MaxCount, 267, "Hedge policy maxCount should be set")
		assert.Equalf(t, upsCfg.Failsafe.Hedge.Delay, "10ms", "Hedge policy delay should be set")
	})

	t.Run("InheritUpstreamsOverridesAfterUpstreamDefaultsConfig", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					UpstreamDefaults: &common.UpstreamConfig{
						Failsafe: &common.FailsafeConfig{
							Hedge: &common.HedgePolicyConfig{
								Delay:    "10ms",
								MaxCount: 267,
							},
						},
					},
					Providers: []*common.ProviderConfig{
						{
							Id:                 "alchemy-prod",
							Vendor:             "alchemy",
							UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
							Settings: map[string]interface{}{
								"apiKey": "test-key",
							},
							Overrides: map[string]*common.UpstreamConfig{
								"evm:1": {
									Failsafe: &common.FailsafeConfig{
										Retry: &common.RetryPolicyConfig{
											MaxAttempts: 123,
										},
									},
								},
							},
						},
					},
				},
			},
		}

		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x1",
			})

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getLogs")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[],"id":1234}`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)

		upstreams := prj.upstreamsRegistry.GetNetworkUpstreams("evm:1")
		require.NotNil(t, upstreams)
		require.Equal(t, len(upstreams), 1)
		upsCfg := upstreams[0].Config()

		assert.Equalf(t, upsCfg.Failsafe.Retry.MaxAttempts, 123, "Retry policy should be set")
		assert.Nilf(t, upsCfg.Failsafe.Hedge, "Hedge policy should not be set")
	})

	t.Run("HappyPath - Override match with wildcard pattern", func(t *testing.T) {
		// Scenario:
		//  - ProviderConfig.Overrides has key = "evm:*" that matches "evm:1",
		//  - ensures the override is applied,
		//  - expecting the returned config to be a shallow copy of the override,
		//    with the correct ID from UpstreamIdTemplate, etc.
	})

	t.Run("UnhappyPath - Vendor PrepareConfig fails", func(t *testing.T) {
		// Scenario:
		//  - The vendor's PrepareConfig method returns an error (e.g. missing apiKey),
		//  - expecting that error to propagate out of GenerateUpstreamConfig.
	})

	t.Run("UnhappyPath - 'evm:' prefix but chain ID parse fails in buildBaseUpstreamConfig", func(t *testing.T) {
		// Scenario:
		//  - networkId = "evm:not-a-number",
		//  - expecting an error from strconv.ParseInt,
		//  - ensuring GenerateUpstreamConfig returns the parse error.
	})

	t.Run("HappyPath - No override found, creates new base config with defaults", func(t *testing.T) {
		// Scenario:
		//  - ProviderConfig.Overrides is empty or has no wildcard match,
		//  - upsteamDefaults is provided,
		//  - ensures new UpstreamConfig is created and defaults are set.
	})

	t.Run("Override Found - wildcards partially match the networkId", func(t *testing.T) {
		// Scenario:
		//  - ProviderConfig.Overrides has key = "evm:1*",
		//  - networkId = "evm:1234",
		//  - ensures the override is used if that wildcard logic returns a match.
	})

	t.Run("UnhappyPath - Wildcard matching function returns internal error", func(t *testing.T) {
		// Scenario:
		//  - Suppose the wildcard matching function (common.WildcardMatch) can return an error,
		//  - expecting the error to be logged or handled gracefully.
	})

	t.Run("Check UpstreamIdTemplate placeholders are replaced properly", func(t *testing.T) {
		// Scenario:
		//  - UpstreamIdTemplate = "<VENDOR>-<PROVIDER>-<NETWORK>-<EVM_CHAIN_ID>",
		//  - networkId = "evm:56",
		//  - expects the final UpstreamConfig.Id to be "alchemy-alchemy-prod-evm:56-56" (example).
	})

	t.Run("HappyPath - All placeholders replaced in EVM scenario", func(t *testing.T) {
		// Scenario:
		//  - template = "<VENDOR>_<PROVIDER>_<NETWORK>_<EVM_CHAIN_ID>",
		//  - vendorName = "alchemy", providerId = "alchemy-prod", networkId = "evm:1",
		//  - expecting "alchemy_alchemy-prod_evm:1_1".
	})

	t.Run("HappyPath - Non-EVM network, <EVM_CHAIN_ID> replaced with 'N/A'", func(t *testing.T) {
		// Scenario:
		//  - template = "<VENDOR>_<PROVIDER>_<NETWORK>_<EVM_CHAIN_ID>",
		//  - networkId does not start with "evm:",
		//  - expecting the <EVM_CHAIN_ID> placeholder to become "N/A".
	})

	t.Run("Non-Existent Placeholders - template has some random placeholders", func(t *testing.T) {
		// Scenario:
		//  - template = "<VENDOR>_<PROVIDER>_<NETWORK>_<SOME_RANDOM>",
		//  - <SOME_RANDOM> won't be replaced. Implementation might leave it as is or skip.
	})

	t.Run("EmptyTemplate - returns empty string", func(t *testing.T) {
		// Scenario:
		//  - template = "",
		//  - expecting final result is an empty string regardless of placeholders.
	})
}

func createServerTestFixtures(cfg *common.Config, t *testing.T) (
	func(body string, headers map[string]string, queryParams map[string]string) (int, string),
	func(host string) (int, map[string]string, string),
	string,
	func(),
	*ERPC,
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

	upstream.ReorderUpstreams(erpcInstance.projectsRegistry.preparedProjects["test_project"].upstreamsRegistry)

	baseURL := fmt.Sprintf("http://localhost:%d", port)

	sendRequest := func(body string, headers map[string]string, queryParams map[string]string) (int, string) {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		chainId := "1"
		if queryParams["chainId"] != "" {
			chainId = queryParams["chainId"]
		}
		req, err := http.NewRequestWithContext(rctx, "POST", baseURL+"/test_project/evm/"+chainId, strings.NewReader(body))
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

		// TODO Can we do it more strictly maybe via a Shutdown() sequence?
		// This to wait for batch processor to stop + initializers to finish
		time.Sleep(500 * time.Millisecond)
	}, erpcInstance
}
