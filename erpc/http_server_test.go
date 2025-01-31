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
				MaxTimeout: util.StringPtr("1s"),
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

		// Mock #1 (faster) – "original request"
		gock.New("http://rpc1.localhost").
			Post("/").
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
		gock.New("http://rpc1.localhost").
			Post("/").
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

	t.Run("SetCORSHeaders_AllowedOrigin", func(t *testing.T) {
		// When the origin is allowed, we send CORS headers
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					CORS: &common.CORSConfig{
						AllowedOrigins: []string{"https://allowed.origin"},
						AllowedMethods: []string{"POST", "GET", "OPTIONS"},
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

		_, sendOptionsRequest, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Send OPTIONS request from allowed origin => not expect CORS headers
		statusCode, headers, _ := sendOptionsRequest("https://allowed.origin")
		assert.Equal(t, http.StatusNoContent, statusCode, "OPTIONS for allowed origin should be 204")
		assert.Equal(t, "https://allowed.origin", headers["Access-Control-Allow-Origin"])
		assert.Equal(t, "POST, GET, OPTIONS", headers["Access-Control-Allow-Methods"])
	})

	t.Run("NotSetCORSHeaders_DisallowedOrigin", func(t *testing.T) {
		// When the origin is disallowed, we don't send CORS headers
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					CORS: &common.CORSConfig{
						AllowedOrigins: []string{"https://allowed.origin"},
						AllowedMethods: []string{"POST", "GET", "OPTIONS"},
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

		_, sendOptionsRequest, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Send OPTIONS request from disallowed origin => expect CORS headers
		statusCode, headers, _ := sendOptionsRequest("https://disallowed.origin")
		assert.Equal(t, http.StatusNoContent, statusCode, "OPTIONS for disallowed origin should be 204")
		assert.Empty(t, headers["Access-Control-Allow-Origin"])
		assert.Empty(t, headers["Access-Control-Allow-Methods"])
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
					upstreamsRegistry: upstream.NewUpstreamsRegistry(context.TODO(), &zerolog.Logger{}, "", nil, nil, nil, nil, nil, nil, 0*time.Second),
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
					upstreamsRegistry: upstream.NewUpstreamsRegistry(context.TODO(), &zerolog.Logger{}, "", nil, nil, nil, nil, nil, nil, 0*time.Second),
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
}

func TestHttpServer_EvmGetLogs(t *testing.T) {
	t.Run("SuccessfulSplitIfOneOfSubRequestsNeedsRetries", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceGetLogsBlockRange: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:              1,
								GetLogsMaxBlockRange: 0x100,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: &common.RetryPolicyConfig{
									MaxAttempts: 2,
								},
							},
						},
					},
				},
			},
		}

		// Create the full request that will be split into three parts
		fullRangeRequest := `{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118000",
				"toBlock": "0x11118300",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`

		// Mock successful response for first range
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118000"`) &&
					strings.Contains(body, `"toBlock":"0x111180ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []map[string]interface{}{
					{"logIndex": "0x1", "blockNumber": "0x11118050", "data": "0x1"},
				},
			})

		// Mock failing response for second range (first attempt)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118100"`) &&
					strings.Contains(body, `"toBlock":"0x111181ff"`)
			}).
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Server error",
				},
			})

		// Mock successful response for second range (second attempt)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118100"`) &&
					strings.Contains(body, `"toBlock":"0x111181ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result": []map[string]interface{}{
					{"logIndex": "0x2", "blockNumber": "0x11118150", "data": "0x2"},
				},
			})

		// Mock successful response for third range
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118200"`) &&
					strings.Contains(body, `"toBlock":"0x111182ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      3,
				"result": []map[string]interface{}{
					{"logIndex": "0x3", "blockNumber": "0x11118250", "data": "0x3"},
				},
			})

		// Mock successful response for last range
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118300"`) &&
					strings.Contains(body, `"toBlock":"0x11118300"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      3,
				"result": []map[string]interface{}{
					{"logIndex": "0x4", "blockNumber": "0x11118300", "data": "0x4"},
				},
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(fullRangeRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)

		// Parse and verify the response contains logs from all three ranges
		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err)

		logs := respObject["result"].([]interface{})
		assert.Equal(t, 4, len(logs), "Expected exactly 4 logs (one from each range)")

		// Verify log block numbers and data
		blockNumbers := make([]string, len(logs))
		data := make([]string, len(logs))
		for i, l := range logs {
			log := l.(map[string]interface{})
			blockNumbers[i] = log["blockNumber"].(string)
			data[i] = log["data"].(string)
		}

		// Verify we got logs from all three ranges
		assert.Contains(t, blockNumbers, "0x11118050", "Missing log from first range")
		assert.Contains(t, blockNumbers, "0x11118150", "Missing log from retried middle range")
		assert.Contains(t, blockNumbers, "0x11118250", "Missing log from third range")

		// Verify data values
		assert.Contains(t, data, "0x1", "Missing data from first range")
		assert.Contains(t, data, "0x2", "Missing data from retried middle range")
		assert.Contains(t, data, "0x3", "Missing data from third range")
	})

	t.Run("FailSplitIfOneOfSubRequestsFailsServerSide", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceGetLogsBlockRange: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:              1,
								GetLogsMaxBlockRange: 0x100,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: &common.RetryPolicyConfig{
									MaxAttempts: 2,
								},
							},
						},
					},
				},
			},
		}

		// Create the request that will be split into multiple parts
		fullRangeRequest := `{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118000",
				"toBlock": "0x11118300",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`

		// Mock successful response for first range
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118000"`) &&
					strings.Contains(body, `"toBlock":"0x111180ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []map[string]interface{}{
					{"logIndex": "0x1", "blockNumber": "0x11118050", "data": "0x1"},
				},
			})

		// Mock failing responses for second range (both attempts)
		gock.New("http://rpc1.localhost").
			Post("").
			Times(2). // Will be called twice due to retry
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118100"`) &&
					strings.Contains(body, `"toBlock":"0x111181ff"`)
			}).
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Server error",
				},
			})

		// We don't need to mock the third range because the request should fail after
		// the second range fails all retry attempts

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Make the request and verify it fails
		statusCode, body := sendRequest(fullRangeRequest, nil, nil)

		// Verify response indicates failure
		assert.Equal(t, http.StatusServiceUnavailable, statusCode)

		// Parse the error response
		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err)

		// Verify error structure
		assert.Contains(t, respObject, "error")
		errorObj := respObject["error"].(map[string]interface{})
		assert.Equal(t, float64(-32603), errorObj["code"].(float64))
		assert.Contains(t, errorObj["message"].(string), "server error")
	})

	t.Run("FailSplitIfOneOfSubRequestsFailsMissingData", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceGetLogsBlockRange: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:              1,
								GetLogsMaxBlockRange: 0x100,
							},
							Failsafe: &common.FailsafeConfig{
								Retry: &common.RetryPolicyConfig{
									MaxAttempts: 2,
								},
							},
						},
					},
				},
			},
		}

		// Create the request that will be split into multiple parts
		fullRangeRequest := `{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x11118000",
				"toBlock": "0x11118300",
				"address": "0x0000000000000000000000000000000000000000",
				"topics": ["0x1234567890123456789012345678901234567890123456789012345678901234"]
			}]
		}`

		// Mock successful response for first range
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118000"`) &&
					strings.Contains(body, `"toBlock":"0x111180ff"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": []map[string]interface{}{
					{"logIndex": "0x1", "blockNumber": "0x11118050", "data": "0x1"},
				},
			})

		// Mock failing responses for second range (both attempts)
		gock.New("http://rpc1.localhost").
			Post("").
			Times(2). // Will be called twice due to retry
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					strings.Contains(body, `"fromBlock":"0x11118100"`) &&
					strings.Contains(body, `"toBlock":"0x111181ff"`)
			}).
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"error": map[string]interface{}{
					"code":    -32602,
					"message": "missing trie node",
				},
			})

		// We don't need to mock the third range because the request should fail after
		// the second range fails all retry attempts

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Make the request and verify it fails
		statusCode, body := sendRequest(fullRangeRequest, nil, nil)

		// Verify response indicates failure
		assert.Equal(t, http.StatusServiceUnavailable, statusCode)

		// Parse the error response
		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err)

		// Verify error structure
		assert.Contains(t, respObject, "error")
		errorObj := respObject["error"].(map[string]interface{})
		assert.Equal(t, float64(-32603), errorObj["code"].(float64))
		assert.Contains(t, errorObj["message"].(string), "missing data")
	})
}

func TestHttpServer_EvmGetBlockByNumber(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	t.Run("FallbackToHighestBlockIfUpstreamReturnsOlderBlock", func(t *testing.T) {
		// 1. Reset gock, set up poller mocks, etc.
		util.ResetGock()
		defer util.ResetGock()

		// 2. Create a config with 2 upstreams and enforce highest block
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		// 3. Request: eth_getBlockByNumber("latest", true)
		requestBody := `{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "eth_getBlockByNumber",
			"params": ["latest", true]
		}`

		// 4. Mock latest block and eth_syncing for state poller
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_syncing")
			}).
			Reply(200).
			JSON([]byte(`{"result":false}`))
		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_syncing")
			}).
			Reply(200).
			JSON([]byte(`{"result":false}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x111"}}`))

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x222"}}`))

		// 5. Mock "rpc1" response for user's call
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x113",
				},
			})

		// 6. Mock “rpc2” returning the newer block
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(req *http.Request) bool {
				// The code will exclude the failing upstream using the directive "!rpc1"
				// and request 0x222 specifically
				body := util.SafeReadBody(req)
				return strings.Contains(body, "eth_getBlockByNumber") &&
					strings.Contains(body, `"0x222"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x222",
					"hash":   "0x123",
				},
			})

		// 7. Spin up the test server, make the request, and check the final result
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(requestBody, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body")

		// 8. Confirm "result.hash" is 0x123, the newer block
		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x123", result["hash"], "should fallback to the highest known block")

		assert.Equal(t, 2, len(gock.Pending()), "unexpected pending requests")
	})

	t.Run("ReturnBlockIfUpstreamReturnsNewerBlock", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Create a config with 1 upstream and enforce highest block.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		requestBody := `{
			"jsonrpc": "2.0",
			"id":      999,
			"method":  "eth_getBlockByNumber",
			"params": ["latest", false]
		}`

		// Mock poller calls for "rpc1" => we say "eth_syncing" = false, and eth_getBlockByNumber("latest") = 0x100
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_syncing")
			}).
			Reply(200).
			JSON([]byte(`{"result":false}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x100"}}`))

		// Mock "rpc1" returning the user's newer block 0x123 in response to the request
		// for the method "eth_getBlockByNumber('latest')".
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      999,
				"result": map[string]interface{}{
					"number": "0x123",
					"hash":   "0xabcdef12345",
				},
			})

		// Spin up the test server, make the request, and check final result.
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(requestBody, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body successfully")

		// Confirm result.number is "0x123" — the upstream's newer block
		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x123", result["number"], "upstream's newer block should be returned as-is")

		assert.Equal(t, 1, len(gock.Pending()), "unexpected pending requests")
	})

	t.Run("ShouldSkipHookIfHighestBlockCheckDisabled", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Provide a config where Evm.Integrity.EnforceHighestBlock = false
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(false),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		// We'll request the latest block from upstream, but the poller will internally store a higher block.
		// Because enforceHighestBlock = false, we expect no override logic (the server should just return the direct result).
		requestBody := `{
			"jsonrpc": "2.0",
			"id":      999,
			"method":  "eth_getBlockByNumber",
			"params": ["latest", false]
		}`

		// Mock poller calls: "eth_syncing" => false, then a poller sees "latest" = 0x300
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_syncing")
			}).
			Reply(200).
			JSON([]byte(`{"result":false}`))

		// Poller viewpoint for "latest" => block 0x300
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x300"}}`))

		// Mock the user call: it will also request "eth_getBlockByNumber" "latest" => upstream actually returns 0x200
		// Because HighestBlockCheck is disabled, the server should NOT override it with 0x300.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				// This filter picks up the user's actual request (ID=999).
				body := util.SafeReadBody(request)
				return strings.Contains(body, `"id":999`) && strings.Contains(body, `"eth_getBlockByNumber"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      999,
				"result": map[string]interface{}{
					"number": "0x200",
					"hash":   "0xabc_no_override",
				},
			})

		// Spin up the test server and send the request
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, body := sendRequest(requestBody, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body")

		// We expect the server to return block number "0x200" exactly as the upstream did,
		// ignoring the poller's knowledge of 0x300, since enforceHighestBlock is false.
		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x200", result["number"], "server should skip highest-block override when disabled")

		assert.Equal(t, 1, len(gock.Pending()), "unexpected pending requests")
	})

	t.Run("ShouldReturnForwardErrorIfUpstreamFails", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Provide a config with Evm.Integrity.EnforceHighestBlock = true
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		// We'll request the latest block from upstream to trigger the forward call
		requestBody := `{
			"jsonrpc": "2.0",
			"id":      777,
			"method":  "eth_getBlockByNumber",
			"params": ["latest", true]
		}`

		// Mock poller calls: "eth_syncing" => false, then "eth_getBlockByNumber(latest)" => 0x100 so that
		// the poller thinks the chain is at block 0x100
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_syncing")
			}).
			Reply(200).
			JSON([]byte(`{"result":false}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x100",
				},
			})

		// When the user actually calls "eth_getBlockByNumber('latest')", the forward call fails with a 503 error.
		// The post-forward hook should just return the error as-is, without overriding or fallback logic.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, `"id":777`) && strings.Contains(body, `"eth_getBlockByNumber"`)
			}).
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      777,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "Upstream error",
				},
			})

		// Spin up the test server and make the request
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, respBody := sendRequest(requestBody, nil, nil)
		assert.Equal(t, http.StatusInternalServerError, statusCode, "should reflect upstream's 500 error")

		// Verify the server forwards the original error info
		var respObject map[string]interface{}
		err := sonic.UnmarshalString(respBody, &respObject)
		assert.NoError(t, err, "should parse error response body")
		assert.Contains(t, respObject, "error", "should contain error field")

		errorObj := respObject["error"].(map[string]interface{})
		assert.Equal(t, float64(-32603), errorObj["code"].(float64), "error code should match upstream error")
		assert.Contains(t, errorObj["message"].(string), "Upstream error", "error message should match upstream message")

		assert.Equal(t, 1, len(gock.Pending()), "unexpected pending requests")
	})

	t.Run("ShouldNoopIfParamIsDirectHex", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Provide a config with EnforceHighestBlock = true, so we can confirm
		// that the hook is skipped if the param is a direct hex number.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		// Mock poller calls: "eth_syncing" => false, "latest" => 0x300
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_syncing")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": false})

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(r), `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x300",
				},
			})

		// Mock the user request for eth_getBlockByNumber("0x12345", false)
		// Because it's a direct hex, we expect the hook to NOT override this.
		userRequestBody := `{
			"jsonrpc": "2.0",
			"id":      9999,
			"method":  "eth_getBlockByNumber",
			"params": ["0x12345", false]
		}`

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"id":9999`) &&
					strings.Contains(body, `"eth_getBlockByNumber"`) &&
					strings.Contains(body, `"0x12345"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      9999,
				"result": map[string]interface{}{
					"number": "0x12345",
					"hash":   "0xabc_hex",
				},
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, respBody := sendRequest(userRequestBody, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)

		var respObj map[string]interface{}
		err := sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		result := respObj["result"].(map[string]interface{})
		assert.Equal(t, "0xabc_hex", result["hash"], "direct hex param should not be overridden")

		assert.Equal(t, 1, len(gock.Pending()), "unexpected pending requests")
	})

	t.Run("ShouldNoopIfParamIsEarliestOrPending", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Provide a config with EnforceHighestBlock = true. We'll confirm the hook
		// is skipped for 'earliest' and 'pending'.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		// Mock poller calls: "eth_syncing" => false, "latest" => 0x300
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_syncing")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": false})

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(r), `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x300",
				},
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// We'll test "earliest" -> returns 0x0
		userRequestEarliest := `{
			"jsonrpc":"2.0",
			"id":1,
			"method":"eth_getBlockByNumber",
			"params":["earliest",true]
		}`
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"id":1`) &&
					strings.Contains(body, `"eth_getBlockByNumber"`) &&
					strings.Contains(body, `"earliest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x0",
					"hash":   "0xearliest",
				},
			})

		statusCodeEarliest, respBodyEarliest := sendRequest(userRequestEarliest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCodeEarliest)

		var respObjEarliest map[string]interface{}
		errEarliest := sonic.UnmarshalString(respBodyEarliest, &respObjEarliest)
		require.NoError(t, errEarliest, "should parse earliest response")

		resultEarliest := respObjEarliest["result"].(map[string]interface{})
		assert.Equal(t, "0xearliest", resultEarliest["hash"], "earliest block should be returned unchanged")

		// We'll test "pending" -> returns e.g. 0x301 (typical pending block num)
		userRequestPending := `{
			"jsonrpc":"2.0",
			"id":2,
			"method":"eth_getBlockByNumber",
			"params":["pending",false]
		}`
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"id":2`) &&
					strings.Contains(body, `"eth_getBlockByNumber"`) &&
					strings.Contains(body, `"pending"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result": map[string]interface{}{
					"number": "0x301",
					"hash":   "0xpending",
				},
			})

		statusCodePending, respBodyPending := sendRequest(userRequestPending, nil, nil)
		assert.Equal(t, http.StatusOK, statusCodePending)

		var respObjPending map[string]interface{}
		errPending := sonic.UnmarshalString(respBodyPending, &respObjPending)
		require.NoError(t, errPending, "should parse pending response")

		resultPending := respObjPending["result"].(map[string]interface{})
		assert.Equal(t, "0xpending", resultPending["hash"], "pending block should be returned unchanged")

		assert.Equal(t, 1, len(gock.Pending()), "unexpected pending requests")
	})

	t.Run("ShouldOverrideIfUpstreamReturnsOlderBlockOnFinalized", func(t *testing.T) {
		// This test covers the scenario:
		// - User calls eth_getBlockByNumber("finalized", true)
		// - The chosen upstream returns a finalized block number X
		// - The poller, however, knows a newer finalized block number X+5
		// - The code triggers a second request to a different upstream ("!firstUpstream") to fetch block X+5
		// - We confirm that the final response to the user is the newer block X+5.

		util.ResetGock()
		defer util.ResetGock()

		// Create a config with 2 upstreams, and EnforceHighestBlock = true.
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
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		// Mock poller calls so that upstreams are recognized and we learn what the "finalized" block is on each:
		// The poller loop typically calls:
		//   eth_syncing
		//   eth_getBlockByNumber("latest")
		//   eth_getBlockByNumber("finalized") if available
		// We'll fake that upstream1 sees finalized = 0x100, upstream2 sees finalized = 0x105
		// Thus the poller aggregates and says the highest finalized is 0x105.

		// Mark both upstreams as not syncing
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_syncing") }).
			Reply(200).
			JSON(map[string]interface{}{"result": false})
		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_syncing") }).
			Reply(200).
			JSON(map[string]interface{}{"result": false})
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(r), `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x444",
				},
			})
		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(r), `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x444",
				},
			})

		// Poller sees upstream1 for finalized => 0x100
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(r), `"finalized"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x100",
				},
			})

		// Poller sees upstream2 for finalized => 0x105
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(r), `"finalized"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x105",
				},
			})

		// Now the user calls eth_getBlockByNumber("finalized", true).
		// The server picks upstream1 first. Let's say upstream1 returns block 0x100:
		userRequest := `{
			"jsonrpc": "2.0",
			"id":      42,
			"method":  "eth_getBlockByNumber",
			"params": ["finalized", true]
		}`

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"id":42`) &&
					strings.Contains(body, `"eth_getBlockByNumber"`) &&
					strings.Contains(body, `"finalized"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      42,
				"result": map[string]interface{}{
					"number": "0x100",
					"hash":   "0x111111bb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624",
				},
			})

		// The server sees that the poller says the highest finalized is 0x105, so it triggers a second request
		// to a different upstream (i.e. "!rpc1") for block 0x105.
		// We mock that request:
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				// We expect a request for exactly block 0x105.
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") &&
					strings.Contains(body, "0x105")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      42,
				"result": map[string]interface{}{
					"number": "0x105",
					"hash":   "0x222222bb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624",
				},
			})

		// Spin up the test server, send the request, check final result is 0x105 from the second upstream.
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, respBody := sendRequest(userRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "overall request should be 200")

		var respObj map[string]interface{}
		err := sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		result, hasResult := respObj["result"].(map[string]interface{})
		require.True(t, hasResult, "the response must contain a 'result' object")

		assert.Equal(t, "0x105", result["number"], "the server should override the older 0x100 with finalized 0x105")
		assert.Equal(t, "0x222222bb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624", result["hash"], "the final data should come from the second upstream")

		assert.Equal(t, 4, len(gock.Pending()), "unexpected pending requests")
	})

	t.Run("ShouldStickWithUpstreamBlockIfItMatchesHighestOnFinalized", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Use a config with 2 upstreams, EnforceHighestBlock = true,
		// so we can confirm that even with multiple upstreams, no fallback request is triggered if
		// the first upstream's "finalized" block is already the highest known block.
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
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		// ----------------------------
		// Poller Mock Setup
		// ----------------------------
		// Mark both upstreams as "not syncing."
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_syncing")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": false})

		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_syncing")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": false})

		// Poller calls eth_getBlockByNumber("latest") to establish each upstream's tip
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(r), `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x300",
				},
			})

		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(r), `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x300",
				},
			})

		// Poller calls eth_getBlockByNumber("finalized") if it's supported. We'll say each upstream's finalized is 0x100.
		// So the poller sees that the highest finalized block across both upstreams is also 0x100.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), `"finalized"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x100",
				},
			})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), `"finalized"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x100",
				},
			})

		// ----------------------------
		// User Request: eth_getBlockByNumber("finalized", true)
		// ----------------------------
		// The chosen upstream returns block 0x100, which matches poller's highest finalized block.
		userRequest := `{
			"jsonrpc": "2.0",
			"id":      999,
			"method":  "eth_getBlockByNumber",
			"params": ["finalized", true]
		}`

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"id":999`) &&
					strings.Contains(body, `"eth_getBlockByNumber"`) &&
					strings.Contains(body, `"finalized"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      999,
				"result": map[string]interface{}{
					"number": "0x100",
					"hash":   "0x222222bb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624",
				},
			})

		// We expect no second request to the other upstream for block 0x100,
		// because the block number from the first upstream matches the highest known finalized block.

		// ----------------------------
		// Spin up the test server, send request
		// ----------------------------
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, respBody := sendRequest(userRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "overall request should be successful")

		// Parse the response
		var respObj map[string]interface{}
		err := sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		// Ensure that result is still 0x100 and the same hash, meaning we used the original upstream response
		result, hasResult := respObj["result"].(map[string]interface{})
		require.True(t, hasResult, "must have a 'result' object in the JSON")

		assert.Equal(t, "0x100", result["number"], "should match poller's highest finalized block with no override")
		assert.Equal(t, "0x222222bb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624", result["hash"], "should remain the same as upstream's response")

		assert.True(t, len(gock.Pending()) == 2, "unexpected pending requests")
	})

	t.Run("ShouldHandleInvalidBlockTag", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Provide a config with EnforceHighestBlock = true so we can confirm that
		// the logic still skips override if the block tag is not one of the recognized keywords.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: util.StringPtr("5s"),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 1,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Endpoint: "http://rpc1.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             1,
								StatePollerInterval: "10s",
							},
						},
					},
				},
			},
		}

		// Mock poller calls: "eth_syncing" => false, then "latest" => 0x300, just so the poller
		// has some known state. We won't use it because the block tag is invalid, so the hook shouldn't apply.
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_syncing")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": false})

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBlockByNumber") &&
					strings.Contains(util.SafeReadBody(request), `"latest"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"result": map[string]interface{}{
					"number": "0x300",
				},
			})

		// The user calls eth_getBlockByNumber("unknownTag", false). Since "unknownTag" is neither
		// 'latest', 'finalized', 'pending', 'earliest', nor a direct hex, we expect NO override logic to happen.
		// The server should forward the call as-is, and return whatever the upstream responds.
		userRequestBody := `{
			"jsonrpc": "2.0",
			"id":      5050,
			"method":  "eth_getBlockByNumber",
			"params": ["unknownTag", false]
		}`

		// Mock the upstream returning a normal block result, e.g. block "0xABCD" for "unknownTag".
		// In reality, many upstreams might just return an error for an unrecognized block tag,
		// but for testing, let's assume it returns a valid response so we can confirm the override is skipped.
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"id":5050`) &&
					strings.Contains(body, `"eth_getBlockByNumber"`) &&
					strings.Contains(body, `"unknownTag"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      5050,
				"result": map[string]interface{}{
					"number": "0xabcd",
					"hash":   "0xhash_for_unknownTag",
				},
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, respBody := sendRequest(userRequestBody, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "the server should pass the response as-is")

		var respObj map[string]interface{}
		err := sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		// Confirm the server didn't do any override logic; it just returned whatever the upstream said.
		result := respObj["result"].(map[string]interface{})
		assert.Equal(t, "0xabcd", result["number"], "invalid block tag should not trigger any override")
		assert.Equal(t, "0xhash_for_unknownTag", result["hash"], "the block hash should match upstream exactly")

		// Only the poller calls plus the single user request are used; no fallback or second request is triggered.
		assert.Equal(t, 1, len(gock.Pending()), "unexpected pending requests")
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
