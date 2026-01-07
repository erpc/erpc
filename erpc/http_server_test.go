package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"

	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	pprof "runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/auth"
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
				MaxTimeout: common.Duration(500 * time.Millisecond).Ptr(), // Set a very short timeout for testing
				ListenV4:   util.BoolPtr(true),                            // Explicitly enable IPv4 for test
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
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}
		erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
		require.NoError(t, err)

		erpcInstance.Bootstrap(ctx)
		require.NoError(t, err)

		httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
		require.NoError(t, err)

		// Start the server on a random port
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port

		go func() {
			err := httpServer.serverV4.Serve(listener)
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("Server error: %v", err)
			}
		}()
		defer httpServer.serverV4.Shutdown(ctx)

		// Wait for the server to start
		time.Sleep(50 * time.Millisecond)

		baseURL := fmt.Sprintf("http://localhost:%d", port)

		sendRequest := func() (int, string) {
			body := strings.NewReader(
				fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["%d", false],"id":1}`, rand.Intn(100000000)),
			)
			req, err := http.NewRequest("POST", baseURL+"/test_project/evm/123", body)
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
			assert.Equal(t, http.StatusOK, result.statusCode)
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
				MaxTimeout: common.Duration(500 * time.Millisecond).Ptr(), // Set a very short timeout for testing
				ListenV4:   util.BoolPtr(true),                            // Explicitly enable IPv4 for test
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
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}
		erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
		require.NoError(t, err)

		erpcInstance.Bootstrap(ctx)
		require.NoError(t, err)

		httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
		require.NoError(t, err)

		// Start the server on a random port
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port

		go func() {
			err := httpServer.serverV4.Serve(listener)
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("Server error: %v", err)
			}
		}()
		defer httpServer.serverV4.Shutdown(ctx)

		// Wait for the server to start
		time.Sleep(50 * time.Millisecond)

		baseURL := fmt.Sprintf("http://localhost:%d", port)

		sendRequest := func() (int, string) {
			body := strings.NewReader(
				fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["%d", false],"id":1}`, rand.Intn(100000000)),
			)
			req, err := http.NewRequest("POST", baseURL+"/test_project/evm/123", body)
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
				assert.Equal(t, http.StatusOK, statusCode)
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
				MaxTimeout: common.Duration(500 * time.Millisecond).Ptr(), // Set a very short timeout for testing
				ListenV4:   util.BoolPtr(true),                            // Explicitly enable IPv4 for test
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
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
		if err != nil {
			panic(err)
		}
		erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
		require.NoError(t, err)

		erpcInstance.Bootstrap(ctx)
		require.NoError(t, err)

		httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
		require.NoError(t, err)

		// Start the server on a random port
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port

		go func() {
			err := httpServer.serverV4.Serve(listener)
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("Server error: %v", err)
			}
		}()
		defer httpServer.serverV4.Shutdown(ctx)

		// Wait for the server to start
		time.Sleep(50 * time.Millisecond)

		baseURL := fmt.Sprintf("http://localhost:%d", port)

		sendRequest := func() (int, string) {
			body := strings.NewReader(
				fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["%d", false],"id":1}`, rand.Intn(100000000)),
			)
			req, err := http.NewRequest("POST", baseURL+"/test_project/evm/123", body)
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
			assert.Equal(t, http.StatusOK, result.statusCode)
			if strings.Contains(result.body, "timeout") {
				timeouts++
			} else {
				successes++
				assert.Contains(t, result.body, "blockNumber")
			}
		}

		t.Logf("Timeouts: %d, Successes: %d\n", timeouts, successes)
		assert.True(t, timeouts > 0, "Expected some timeouts")
		assert.True(t, successes > 0, "Expected some successes")
	})
}
func TestHttpServer_ManualTimeoutScenarios(t *testing.T) {
	t.Run("ServerHandlerTimeout", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(10 * time.Millisecond).Ptr(),
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
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(200 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(100 * time.Millisecond),
									},
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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "http request handling timeout")
	})

	t.Run("NetworkTimeoutBatchingEnabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(50 * time.Second).Ptr(),
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
									Hedge: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(30 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: nil,
									Hedge: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(300 * time.Millisecond),
									},
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  common.Duration(1 * time.Millisecond),
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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "exceeded on network-level")
	})

	t.Run("NetworkTimeoutBatchingDisabled", func(t *testing.T) {
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
									Retry: nil,
									Hedge: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(100 * time.Millisecond),
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
									Hedge: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(5 * time.Second),
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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "exceeded on network-level")
	})

	t.Run("UpstreamRequestTimeoutBatchingEnabled", func(t *testing.T) {
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
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(1 * time.Second),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(100 * time.Millisecond),
									},
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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "timeout policy exceeded on upstream-level")
	})

	t.Run("UpstreamRequestTimeoutBatchingDisabled", func(t *testing.T) {
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
									Retry: nil,
									Hedge: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(5 * time.Second),
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
									Hedge: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(100 * time.Millisecond),
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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "timeout policy exceeded on upstream-level")
	})

	t.Run("SameTimeoutLowForServerAndNetwork", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(50 * time.Millisecond).Ptr(),
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
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(50 * time.Millisecond),
									},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(5 * time.Second),
									},
								},
							},
							JsonRpc: &common.JsonRpcUpstreamConfig{
								SupportsBatch: &common.TRUE,
								BatchMaxWait:  common.Duration(1 * time.Millisecond),
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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		if !strings.Contains(body, "timeout") {
			t.Fatalf("expected timeout error, got %s", body)
		}
	})
	t.Run("ServerTimeoutNoUpstreamNoNetworkTimeout", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(500 * time.Millisecond).Ptr(),
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
									Retry:   nil,
									Hedge:   nil,
									Timeout: nil,
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
									Retry:   nil,
									Hedge:   nil,
									Timeout: nil,
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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "timeout")
	})

	t.Run("MidServerHighNetworkLowUpstreamTimeoutBatchingDisabled", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(200 * time.Millisecond).Ptr(),
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
									Hedge: nil,
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
									Retry: nil,
									Hedge: nil,
									Timeout: &common.TimeoutPolicyConfig{
										Duration: common.Duration(10 * time.Millisecond),
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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123"],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "timeout policy")
	})

	t.Run("GetLogsShouldNotRetryWhenRetryEmptyDisabled", func(t *testing.T) {
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
					Id:              "test_project",
					NetworkDefaults: &common.NetworkDefaults{
						// RetryEmpty left nil -> default false
					},
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
								Integrity: &common.EvmIntegrityConfig{
									EnforceGetLogsBlockRange: util.BoolPtr(false),
								},
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 8,
										Delay:       common.Duration(100 * time.Millisecond),
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
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 3,
										Delay:       common.Duration(100 * time.Millisecond),
									},
								},
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
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 3,
										Delay:       common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// Expect only the first upstream to be called once
		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getLogs")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		requestBody := `{
			"jsonrpc": "2.0",
			"method": "eth_getLogs",
			"params": [{
				"fromBlock": "0x1ab771",
				"toBlock": "0x1ab7d5",
				"topics": ["0xbccc00b713f54173962e7de6098f643d8ebf53d488d71f4b2a5171496d038f9e"]
			}],
			"id": 1
		}`

		headers := map[string]string{
			"X-ERPC-Skip-Interpolation": "true",
		}
		q := map[string]string{
			"skip-interpolation": "true",
		}
		statusCode, _, body := sendRequest(requestBody, headers, q)

		assert.Equal(t, http.StatusOK, statusCode, "Status code should be 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal([]byte(body), &response)
		require.NoError(t, err, "Should be able to decode response")

		assert.Contains(t, response, "result", "Response should contain 'result' field")
		assert.NotContains(t, response, "error", "Response should not contain 'error' field")

		result, ok := response["result"].([]interface{})
		assert.True(t, ok, "Result should be an array")
		assert.Empty(t, result, "Result should be an empty array")
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
		// {
		// 	name: "UpstreamSupportsBatch",
		// 	configure: func(cfg *common.Config) {
		// 		cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch = &common.TRUE
		// 		cfg.Projects[0].Upstreams[0].JsonRpc.BatchMaxSize = 1
		// 		cfg.Projects[0].Upstreams[0].JsonRpc.BatchMaxWait = common.Duration(10 * time.Millisecond)
		// 	},
		// },
		{
			name: "UpstreamDoesNotSupportBatch",
			configure: func(cfg *common.Config) {
				cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch = &common.FALSE
			},
		},
		// {
		// 	name: "CachingEnabled",
		// 	configure: func(cfg *common.Config) {
		// 		cfg.Database = &common.DatabaseConfig{
		// 			EvmJsonRpcCache: &common.CacheConfig{
		// 				Connectors: []*common.ConnectorConfig{
		// 					{
		// 						Id:     "mock",
		// 						Driver: common.DriverMemory,
		// 						Memory: &common.MemoryConnectorConfig{
		// 							MaxItems: 100_000, MaxTotalSize: "1GB",
		// 						},
		// 					},
		// 				},
		// 				Policies: []*common.CachePolicyConfig{
		// 					{
		// 						Network: "*",
		// 						Method:  "*",
		// 						TTL:     common.Duration(5 * time.Minute),
		// 					},
		// 				},
		// 			},
		// 		}
		// 	},
		// },
		// {
		// 	name: "CachingDisabled",
		// 	configure: func(cfg *common.Config) {
		// 		cfg.Database = &common.DatabaseConfig{
		// 			EvmJsonRpcCache: nil,
		// 		}
		// 	},
		// },
	}
	for _, tc := range testCases {
		// Capture the current value of tc
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()

			// t.Run("ConcurrentRequests", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
			// 		cfg.Projects[0].Upstreams[0].JsonRpc.BatchMaxSize = 10
			// 		cfg.Projects[0].Upstreams[0].JsonRpc.BatchMaxWait = common.Duration(100 * time.Millisecond)
			// 		gock.New("http://rpc1.localhost").
			// 			Post("/").
			// 			Filter(func(request *http.Request) bool {
			// 				return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
			// 			}).
			// 			Reply(200).
			// 			JSON([]interface{}{
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      0,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      1,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      2,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      3,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      4,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      5,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      6,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      7,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      8,
			// 					"result":  "0x444444",
			// 				},
			// 				map[string]interface{}{
			// 					"jsonrpc": "2.0",
			// 					"id":      9,
			// 					"result":  "0x444444",
			// 				},
			// 			})
			// 	}

			// 	const concurrentRequests = 10

			// 	var wg sync.WaitGroup
			// 	results := make([]struct {
			// 		statusCode int
			// 		body       string
			// 	}, concurrentRequests)

			// 	for i := 0; i < concurrentRequests; i++ {
			// 		if !*cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
			// 			gock.New("http://rpc1.localhost").
			// 				Post("/").
			// 				Filter(func(request *http.Request) bool {
			// 					b := util.SafeReadBody(request)
			// 					return strings.Contains(b, fmt.Sprintf(`"id":%d`, i)) && strings.Contains(b, "eth_getBalance")
			// 				}).
			// 				Reply(200).
			// 				Map(func(res *http.Response) *http.Response {
			// 					sg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"0x444444"}`, i)
			// 					res.Body = io.NopCloser(strings.NewReader(sg))
			// 					return res
			// 				})
			// 		}
			// 	}

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	for i := 0; i < concurrentRequests; i++ {
			// 		wg.Add(1)
			// 		go func(index int) {
			// 			defer wg.Done()
			// 			body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[%d],"id":%d}`, index, index)
			// 			results[index].statusCode, _, results[index].body = sendRequest(body, nil, nil)
			// 		}(i)
			// 	}

			// 	wg.Wait()

			// 	for i, result := range results {
			// 		assert.Equal(t, http.StatusOK, result.statusCode, "Status code should be 200 for request %d", i)

			// 		var response map[string]interface{}
			// 		err := sonic.Unmarshal([]byte(result.body), &response)
			// 		assert.NoError(t, err, "Should be able to decode response for request %d", i)
			// 		assert.Equal(t, "0x444444", response["result"], "Unexpected result for request %d", i)
			// 	}
			// })

			// t.Run("InvalidJSON", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	statusCode, _, body := sendRequest(`{"invalid json`, nil, nil)

			// 	assert.Equal(t, http.StatusBadRequest, statusCode)

			// 	var errorResponse map[string]interface{}
			// 	err := sonic.Unmarshal([]byte(body), &errorResponse)
			// 	require.NoError(t, err)

			// 	assert.Contains(t, errorResponse, "error")
			// 	errorObj := errorResponse["error"].(map[string]interface{})
			// 	errStr, _ := sonic.Marshal(errorObj)
			// 	assert.Contains(t, string(errStr), "failed to parse")
			// })

			// t.Run("UnsupportedMethod", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	cfg.Projects[0].Upstreams[0].IgnoreMethods = []string{}

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	gock.New("http://rpc1.localhost").
			// 		Post("/").
			// 		Reply(200).
			// 		Map(func(res *http.Response) *http.Response {
			// 			sg := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
			// 			if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
			// 				sg = "[" + sg + "]"
			// 			}
			// 			res.Body = io.NopCloser(strings.NewReader(sg))
			// 			return res
			// 		})

			// 	statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"unsupported_method","params":[],"id":1}`, nil, nil)
			// 	assert.Equal(t, http.StatusNotAcceptable, statusCode)

			// 	var errorResponse map[string]interface{}
			// 	err := sonic.Unmarshal([]byte(body), &errorResponse)
			// 	require.NoError(t, err)

			// 	assert.Contains(t, errorResponse, "error")
			// 	errorObj := errorResponse["error"].(map[string]interface{})
			// 	assert.Equal(t, float64(-32601), errorObj["code"])
			// })

			// t.Run("IgnoredMethod", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 1)

			// 	cfg.Projects[0].Upstreams[0].IgnoreMethods = []string{"ignored_method"}

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	gock.New("http://rpc1.localhost").
			// 		Post("/").
			// 		Reply(200).
			// 		Map(func(res *http.Response) *http.Response {
			// 			sg := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
			// 			if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
			// 				sg = "[" + sg + "]"
			// 			}
			// 			res.Body = io.NopCloser(strings.NewReader(sg))
			// 			return res
			// 		})

			// 	statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"ignored_method","params":[],"id":1}`, nil, nil)
			// 	assert.Equal(t, http.StatusNotAcceptable, statusCode)

			// 	var errorResponse map[string]interface{}
			// 	err := sonic.Unmarshal([]byte(body), &errorResponse)
			// 	require.NoError(t, err)

			// 	assert.Contains(t, errorResponse, "error")
			// 	errorObj := errorResponse["error"].(map[string]interface{})
			// 	assert.Equal(t, float64(-32601), errorObj["code"])
			// })

			// t.Run("InvalidProjectID", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	// Set up test fixtures
			// 	_, _, baseURL, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	req, err := http.NewRequest("POST", baseURL+"/invalid_project/evm/123", strings.NewReader(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`))
			// 	require.NoError(t, err)
			// 	req.Header.Set("Content-Type", "application/json")

			// 	client := &http.Client{
			// 		Timeout: 10 * time.Second,
			// 	}
			// 	resp, err := client.Do(req)
			// 	require.NoError(t, err)
			// 	defer resp.Body.Close()

			// 	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

			// 	body, err := io.ReadAll(resp.Body)
			// 	require.NoError(t, err)

			// 	var errorResponse map[string]interface{}
			// 	err = sonic.Unmarshal(body, &errorResponse)
			// 	require.NoError(t, err)

			// 	assert.Contains(t, errorResponse, "error")
			// 	errorObj := errorResponse["error"].(map[string]interface{})
			// 	assert.Contains(t, errorObj["message"], "not configured")
			// })
			// t.Run("UpstreamLatencyAndTimeout", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	gock.New("http://rpc1.localhost").
			// 		Post("/").
			// 		Reply(200).
			// 		Delay(6 * time.Second). // Delay longer than the server timeout
			// 		JSON(map[string]interface{}{
			// 			"jsonrpc": "2.0",
			// 			"id":      1,
			// 			"result":  "0x1111111",
			// 		})

			// 	statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

			// 	assert.Equal(t, http.StatusGatewayTimeout, statusCode)

			// 	var errorResponse map[string]interface{}
			// 	err := sonic.Unmarshal([]byte(body), &errorResponse)
			// 	require.NoError(t, err)

			// 	assert.Contains(t, errorResponse, "error")
			// 	errorObj := errorResponse["error"].(map[string]interface{})
			// 	errStr, _ := sonic.Marshal(errorObj)
			// 	assert.Contains(t, string(errStr), "timeout")
			// })

			t.Run("UnexpectedPlainErrorResponseFromUpstream", func(t *testing.T) {
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
								},
							},
							Upstreams: []*common.UpstreamConfig{
								{
									Type:     common.UpstreamTypeEvm,
									Endpoint: "http://rpc1.localhost",
									Evm: &common.EvmUpstreamConfig{
										ChainId: 123,
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

				_, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

				assert.Contains(t, body, "error code: 1015")
			})

			// t.Run("UnexpectedServerErrorResponseFromUpstream", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	gock.New("http://rpc1.localhost").
			// 		Post("/").
			// 		Times(1).
			// 		Reply(500).
			// 		BodyString(`{"error":{"code":-39999,"message":"my funky error"}}`)

			// 	statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

			// 	assert.Equal(t, http.StatusInternalServerError, statusCode)
			// 	assert.Contains(t, body, "-39999")
			// 	assert.Contains(t, body, "my funky error")
			// })

			// t.Run("MissingIDInJsonRpcRequest", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	var id interface{}
			// 	gock.New("http://rpc1.localhost").
			// 		Post("/").
			// 		Times(1).
			// 		SetMatcher(gock.NewEmptyMatcher()).
			// 		AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
			// 			if !strings.Contains(req.URL.Host, "rpc1") {
			// 				return false, nil
			// 			}
			// 			bodyBytes, err := io.ReadAll(req.Body)
			// 			if err != nil {
			// 				return false, err
			// 			}
			// 			if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
			// 				idNode, err := sonic.Get(bodyBytes, 0, "id")
			// 				if err != nil {
			// 					t.Fatalf("Error getting id node (batch): %v", err)
			// 					return false, err
			// 				}
			// 				id, err = idNode.Interface()
			// 				if err != nil {
			// 					t.Fatalf("Error getting id interface (batch): %v", err)
			// 					return false, err
			// 				}
			// 			} else {
			// 				idNode, err := sonic.Get(bodyBytes, "id")
			// 				if err != nil {
			// 					t.Fatalf("Error getting id node: %v", err)
			// 					return false, err
			// 				}
			// 				id, err = idNode.Interface()
			// 				if err != nil {
			// 					t.Fatalf("Error getting id interface: %v", err)
			// 					return false, err
			// 				}
			// 			}

			// 			if id == nil || id == 0 || id == "" {
			// 				t.Fatalf("Expected id to not be 0, got %v", id)
			// 			}

			// 			return true, nil
			// 		}).
			// 		Reply(200).
			// 		Map(func(res *http.Response) *http.Response {
			// 			var respTxt string
			// 			if *cfg.Projects[0].Upstreams[0].JsonRpc.SupportsBatch {
			// 				respTxt = `[{"jsonrpc":"2.0","id":THIS_WILL_BE_REPLACED,"result":"0x123456"}]`
			// 			} else {
			// 				respTxt = `{"jsonrpc":"2.0","id":THIS_WILL_BE_REPLACED,"result":"0x123456"}`
			// 			}
			// 			idp, err := sonic.Marshal(id)
			// 			require.NoError(t, err)
			// 			res.Body = io.NopCloser(strings.NewReader(strings.Replace(respTxt, "THIS_WILL_BE_REPLACED", string(idp), 1)))
			// 			return res
			// 		})

			// 	statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_traceDebug","params":[]}`, nil, nil)
			// 	assert.Equal(t, http.StatusOK, statusCode)
			// 	assert.Contains(t, body, "0x123456")
			// })
			// t.Run("AutoAddIDandJSONRPCFieldstoRequest", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	gock.New("http://rpc1.localhost").
			// 		Post("/").
			// 		Times(1).
			// 		SetMatcher(gock.NewEmptyMatcher()).
			// 		AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
			// 			if !strings.Contains(req.URL.Host, "rpc1") {
			// 				return false, nil
			// 			}
			// 			bodyBytes, err := io.ReadAll(req.Body)
			// 			if err != nil {
			// 				return false, err
			// 			}
			// 			bodyStr := string(bodyBytes)
			// 			if !strings.Contains(bodyStr, "\"id\"") {
			// 				fmt.Printf("ERROR: No id found in request")
			// 				return false, nil
			// 			}
			// 			if !strings.Contains(bodyStr, "\"jsonrpc\"") {
			// 				fmt.Printf("ERROR: No jsonrpc found in request")
			// 				return false, nil
			// 			}
			// 			if !strings.Contains(bodyStr, "\"method\"") {
			// 				fmt.Printf("ERROR: No method found in request")
			// 				return false, nil
			// 			}
			// 			return true, nil
			// 		}).
			// 		Reply(200).
			// 		BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x123456"}`)

			// 	sendRequest(`{"method":"eth_traceDebug","params":[]}`, nil, nil)
			// })

			// t.Run("AlwaysPropagateUpstreamErrorDataField", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	gock.New("http://rpc1.localhost").
			// 		Post("/").
			// 		Reply(400).
			// 		BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params","data":{"range":"the range 55074203 - 55124202 exceeds the range allowed for your plan (49999 > 2000)."}}}`)

			// 	_, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x0","toBlock":"0x0"}],"id":1}`, nil, nil)
			// 	assert.Contains(t, body, "the range 55074203 - 55124202")
			// })
			// t.Run("KeepIDWhen0IsProvided", func(t *testing.T) {
			// 	cfg := &common.Config{
			// 		Server: &common.ServerConfig{
			// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			// 		},
			// 		Projects: []*common.ProjectConfig{
			// 			{
			// 				Id: "test_project",
			// 				Networks: []*common.NetworkConfig{
			// 					{
			// 						Architecture: common.ArchitectureEvm,
			// 						Evm: &common.EvmNetworkConfig{
			// 							ChainId: 123,
			// 						},
			// 					},
			// 				},
			// 				Upstreams: []*common.UpstreamConfig{
			// 					{
			// 						Type:     common.UpstreamTypeEvm,
			// 						Endpoint: "http://rpc1.localhost",
			// 						Evm: &common.EvmUpstreamConfig{
			// 							ChainId: 123,
			// 						},
			// 						VendorName: "llama",
			// 						JsonRpc: &common.JsonRpcUpstreamConfig{
			// 							SupportsBatch: &common.FALSE,
			// 						},
			// 					},
			// 				},
			// 			},
			// 		},
			// 		RateLimiters: &common.RateLimiterConfig{},
			// 	}

			// 	if tc.configure != nil {
			// 		tc.configure(cfg)
			// 	}

			// 	util.ResetGock()
			// 	defer util.ResetGock()
			// 	util.SetupMocksForEvmStatePoller()
			// 	defer util.AssertNoPendingMocks(t, 0)

			// 	// Set up test fixtures
			// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
			// 	defer shutdown()

			// 	gock.New("http://rpc1.localhost").
			// 		Post("/").
			// 		SetMatcher(gock.NewEmptyMatcher()).
			// 		AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
			// 			if !strings.Contains(req.URL.Host, "rpc1") {
			// 				return false, nil
			// 			}
			// 			bodyBytes, err := io.ReadAll(req.Body)
			// 			if err != nil {
			// 				return false, err
			// 			}
			// 			bodyStr := string(bodyBytes)
			// 			if !strings.Contains(bodyStr, "\"id\"") {
			// 				fmt.Printf("ERROR: No id found in request")
			// 				return false, nil
			// 			}
			// 			if bodyStr[0] == '[' {
			// 				idNode, err := sonic.Get(bodyBytes, 0, "id")
			// 				require.NoError(t, err)
			// 				id, err := idNode.Int64()
			// 				require.NoError(t, err)
			// 				if id != 0 {
			// 					fmt.Printf("ERROR: Expected id to be 0, got %d from body: %s", id, bodyStr)
			// 					return false, nil
			// 				} else {
			// 					return true, nil
			// 				}
			// 			} else {
			// 				idNode, err := sonic.Get(bodyBytes, "id")
			// 				require.NoError(t, err)
			// 				id, err := idNode.Int64()
			// 				require.NoError(t, err)
			// 				if id != 0 {
			// 					fmt.Printf("ERROR: Expected id to be 0, got %d from body: %s", id, bodyStr)
			// 					return false, nil
			// 				} else {
			// 					return true, nil
			// 				}
			// 			}
			// 		}).
			// 		Reply(200).
			// 		BodyString(`{"jsonrpc":"2.0","id":0,"result":"0x123456"}`)

			// 	sendRequest(`{"jsonrpc":"2.0","method":"eth_traceDebug","params":[],"id":0}`, nil, nil)
			// })
		})
	}

	// t.Run("ManyRequestsMultiplexedInOneUpstream", func(t *testing.T) {
	// 	cfg := &common.Config{
	// 		Server: &common.ServerConfig{
	// 			MaxTimeout: common.Duration(5 * time.Second).Ptr(),
	// 		},
	// 		Projects: []*common.ProjectConfig{
	// 			{
	// 				Id: "test_project",
	// 				Networks: []*common.NetworkConfig{
	// 					{
	// 						Architecture: common.ArchitectureEvm,
	// 						Evm: &common.EvmNetworkConfig{
	// 							ChainId: 123,
	// 						},
	// 						Failsafe: []*common.FailsafeConfig{
	// 							{
	// 								Retry:          nil,
	// 								CircuitBreaker: nil,
	// 								Hedge:          nil,
	// 								Timeout:        nil,
	// 							},
	// 						},
	// 					},
	// 				},
	// 				Upstreams: []*common.UpstreamConfig{
	// 					{
	// 						Id:       "rpc1",
	// 						Type:     common.UpstreamTypeEvm,
	// 						Endpoint: "http://rpc1.localhost",
	// 						Evm: &common.EvmUpstreamConfig{
	// 							ChainId: 123,
	// 						},
	// 						Failsafe: []*common.FailsafeConfig{
	// 							{
	// 								Retry:          nil,
	// 								CircuitBreaker: nil,
	// 								Hedge:          nil,
	// 								Timeout:        nil,
	// 							},
	// 						},
	// 					},
	// 				},
	// 			},
	// 		},
	// 	}

	// 	util.ResetGock()
	// 	defer util.ResetGock()
	// 	util.SetupMocksForEvmStatePoller()

	// 	// Create 3 separate mocks that will handle the first 3 unique requests
	// 	// The rest should be served from the multiplexer
	// 	for i := 0; i < 3; i++ {
	// 		gock.New("http://rpc1.localhost").
	// 			Post("/").
	// 			Filter(func(request *http.Request) bool {
	// 				body := util.SafeReadBody(request)
	// 				return strings.Contains(body, "eth_getBalance")
	// 			}).
	// 			Times(1).
	// 			Reply(200).
	// 			Delay(4000 * time.Millisecond).
	// 			JSON(map[string]interface{}{
	// 				"jsonrpc": "2.0",
	// 				"id":      1, // Default ID, will be replaced by the actual request ID
	// 				"result":  "0x123456",
	// 			})
	// 	}

	// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	// 	defer shutdown()

	// 	wg := &sync.WaitGroup{}
	// 	relChan := make(chan struct{})
	// 	for i := 0; i < 10; i++ {
	// 		wg.Add(1)
	// 		go func(i int) {
	// 			defer wg.Done()
	// 			<-relChan
	// 			statusCode, _, body := sendRequest(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[{"fromBlock":"0x0","toBlock":"0x0"}],"id":%d}`, i), nil, nil)
	// 			assert.Equal(t, http.StatusOK, statusCode)
	// 			assert.Contains(t, body, "0x123456")
	// 		}(i)
	// 	}
	// 	close(relChan)
	// 	wg.Wait()
	// })

	// t.Run("CorrectResponseOnEvmRevertData", func(t *testing.T) {
	// 	cfg := &common.Config{
	// 		Server: &common.ServerConfig{
	// 			MaxTimeout: common.Duration(500 * time.Second).Ptr(),
	// 		},
	// 		Projects: []*common.ProjectConfig{
	// 			{
	// 				Id: "test_project",
	// 				Networks: []*common.NetworkConfig{
	// 					{
	// 						Architecture: common.ArchitectureEvm,
	// 						Evm: &common.EvmNetworkConfig{
	// 							ChainId: 123,
	// 						},
	// 						Failsafe: []*common.FailsafeConfig{
	// 							{
	// 								Retry: &common.RetryPolicyConfig{
	// 									MaxAttempts: 2,
	// 								},
	// 								CircuitBreaker: nil,
	// 								Hedge:          nil,
	// 								Timeout:        nil,
	// 							},
	// 						},
	// 					},
	// 				},
	// 				Upstreams: []*common.UpstreamConfig{
	// 					{
	// 						Id:       "rpc1",
	// 						Type:     common.UpstreamTypeEvm,
	// 						Endpoint: "http://rpc1.localhost",
	// 						Evm: &common.EvmUpstreamConfig{
	// 							ChainId: 123,
	// 						},
	// 						JsonRpc: &common.JsonRpcUpstreamConfig{
	// 							SupportsBatch: &common.FALSE,
	// 						},
	// 					},
	// 					{
	// 						Id:       "rpc2",
	// 						Type:     common.UpstreamTypeEvm,
	// 						Endpoint: "http://rpc2.localhost",
	// 						Evm: &common.EvmUpstreamConfig{
	// 							ChainId: 123,
	// 						},
	// 						JsonRpc: &common.JsonRpcUpstreamConfig{
	// 							SupportsBatch: &common.FALSE,
	// 						},
	// 					},
	// 				},
	// 			},
	// 		},
	// 		RateLimiters: &common.RateLimiterConfig{},
	// 	}

	// 	util.ResetGock()
	// 	defer util.ResetGock()
	// 	util.SetupMocksForEvmStatePoller()
	// 	defer util.AssertNoPendingMocks(t, 0)

	// 	// Define test-specific mocks BEFORE server/components start
	// 	gock.New("http://rpc1.localhost").
	// 		Post("").
	// 		Filter(func(request *http.Request) bool {
	// 			return strings.Contains(util.SafeReadBody(request), "\"method\":\"eth_call\"")
	// 		}).
	// 		Reply(400).
	// 		BodyString(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)

	// 	gock.New("http://rpc2.localhost").
	// 		Times(1).
	// 		Post("").
	// 		Filter(func(request *http.Request) bool {
	// 			return strings.Contains(util.SafeReadBody(request), "\"method\":\"eth_call\"")
	// 		}).
	// 		Reply(200).
	// 		JSON([]byte(`{"jsonrpc":"2.0","id":9199,"error":{"code":3,"message":"execution reverted: Dai/insufficient-balance"}}`))

	// 	// Set up test fixtures AFTER mocks
	// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	// 	defer shutdown()

	// 	statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`, nil, nil)
	// 	assert.Equal(t, http.StatusOK, statusCode)
	// 	assert.Contains(t, body, "Dai/insufficient-balance")
	// })

	// t.Run("ForwardEthCallWithoutBlockParameter", func(t *testing.T) {
	// 	// Define the configuration
	// 	cfg := &common.Config{
	// 		Server: &common.ServerConfig{
	// 			MaxTimeout: common.Duration(500 * time.Second).Ptr(),
	// 		},
	// 		Projects: []*common.ProjectConfig{
	// 			{
	// 				Id: "test_project",
	// 				Networks: []*common.NetworkConfig{
	// 					{
	// 						Architecture: common.ArchitectureEvm,
	// 						Evm: &common.EvmNetworkConfig{
	// 							ChainId: 123,
	// 						},
	// 					},
	// 				},
	// 				Upstreams: []*common.UpstreamConfig{
	// 					{
	// 						Id:       "rpc1",
	// 						Type:     common.UpstreamTypeEvm,
	// 						Endpoint: "http://rpc1.localhost",
	// 						Evm: &common.EvmUpstreamConfig{
	// 							ChainId: 123,
	// 						},
	// 						JsonRpc: &common.JsonRpcUpstreamConfig{
	// 							SupportsBatch: &common.FALSE,
	// 						},
	// 					},
	// 				},
	// 			},
	// 		},
	// 		RateLimiters: &common.RateLimiterConfig{},
	// 	}

	// 	util.ResetGock()
	// 	defer util.ResetGock()
	// 	util.SetupMocksForEvmStatePoller()
	// 	defer util.AssertNoPendingMocks(t, 0)

	// 	// Set up test fixtures
	// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	// 	defer shutdown()

	// 	// Mock the upstream response, expecting "latest" as second parameter
	// 	gock.New("http://rpc1.localhost").
	// 		Post("").
	// 		Filter(func(request *http.Request) bool {
	// 			b := util.SafeReadBody(request)
	// 			return strings.Contains(b, "eth_call") && strings.Contains(b, "latest")
	// 		}).
	// 		Reply(200).
	// 		JSON([]byte(`{
	// 			"jsonrpc": "2.0",
	// 			"id": 1,
	// 			"result": "0x0000000000000000000000000000000000000000000000056bc75e2d63100000"
	// 		}`))

	// 	// Send request and verify response
	// 	request := `{
	// 		"jsonrpc": "2.0",
	// 		"method": "eth_call",
	// 		"params": [{
	// 			"to": "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
	// 			"data": "0x70a08231000000000000000000000000b5e5d0f8c0cba267cd3d7035d6adc8eba7df7cdd"
	// 		}],
	// 		"id": 1
	// 	}`

	// 	statusCode, _, body := sendRequest(request, nil, nil)

	// 	// Verify response
	// 	assert.Equal(t, http.StatusOK, statusCode)
	// 	assert.Contains(t, body, "0x0000000000000000000000000000000000000000000000056bc75e2d63100000")

	// 	// Verify that the response is a valid JSON-RPC response
	// 	var response map[string]interface{}
	// 	err := json.Unmarshal([]byte(body), &response)
	// 	assert.NoError(t, err)
	// 	assert.Contains(t, response, "result")
	// 	assert.Equal(t, "2.0", response["jsonrpc"])
	// 	assert.Equal(t, float64(1), response["id"])
	// })

	// t.Run("ForwardEthCallWithBlockParameter", func(t *testing.T) {
	// 	cfg := &common.Config{
	// 		Server: &common.ServerConfig{
	// 			MaxTimeout: common.Duration(500 * time.Second).Ptr(),
	// 		},
	// 		Projects: []*common.ProjectConfig{
	// 			{
	// 				Id: "test_project",
	// 				Networks: []*common.NetworkConfig{
	// 					{
	// 						Architecture: common.ArchitectureEvm,
	// 						Evm: &common.EvmNetworkConfig{
	// 							ChainId: 123,
	// 						},
	// 					},
	// 				},
	// 				Upstreams: []*common.UpstreamConfig{
	// 					{
	// 						Id:       "rpc1",
	// 						Type:     common.UpstreamTypeEvm,
	// 						Endpoint: "http://rpc1.localhost",
	// 						Evm: &common.EvmUpstreamConfig{
	// 							ChainId: 123,
	// 						},
	// 						JsonRpc: &common.JsonRpcUpstreamConfig{
	// 							SupportsBatch: &common.FALSE,
	// 						},
	// 					},
	// 				},
	// 			},
	// 		},
	// 		RateLimiters: &common.RateLimiterConfig{},
	// 	}

	// 	util.ResetGock()
	// 	defer util.ResetGock()
	// 	util.SetupMocksForEvmStatePoller()
	// 	defer util.AssertNoPendingMocks(t, 0)

	// 	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	// 	defer shutdown()

	// 	// Mock the upstream response, expecting "latest" as second parameter
	// 	gock.New("http://rpc1.localhost").
	// 		Post("").
	// 		Filter(func(request *http.Request) bool {
	// 			b := util.SafeReadBody(request)
	// 			return strings.Contains(b, "eth_call") && strings.Contains(b, "0x44444")
	// 		}).
	// 		Reply(200).
	// 		JSON([]byte(`{
	// 			"jsonrpc": "2.0",
	// 			"id": 1,
	// 			"result": "0x0000000000000000000000000000000000000000000000056bc75e2d63100000"
	// 		}`))

	// 	request := `{
	// 		"jsonrpc": "2.0",
	// 		"method": "eth_call",
	// 		"params": [{
	// 			"to": "0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
	// 			"data": "0x70a08231000000000000000000000000b5e5d0f8c0cba267cd3d7035d6adc8eba7df7cdd"
	// 		},"0x44444"],
	// 		"id": 1
	// 	}`

	// 	statusCode, _, body := sendRequest(request, nil, nil)

	// 	assert.Equal(t, http.StatusOK, statusCode)
	// 	assert.Contains(t, body, "0x0000000000000000000000000000000000000000000000056bc75e2d63100000")
	// })
}
func TestHttpServer_MultipleUpstreams(t *testing.T) {
	t.Run("UpstreamNotAllowedByDirectiveViaHeaders", func(t *testing.T) {
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
									Retry:          nil,
									CircuitBreaker: nil,
									Hedge:          nil,
									Timeout:        nil,
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
									Retry:          nil,
									CircuitBreaker: nil,
									Hedge:          nil,
									Timeout:        nil,
								},
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
									Retry:          nil,
									CircuitBreaker: nil,
									Hedge:          nil,
									Timeout:        nil,
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

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode2, _, body2 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, map[string]string{
			"X-ERPC-Use-Upstream": "rpc2",
		}, nil)

		assert.Equal(t, http.StatusOK, statusCode2)
		assert.Contains(t, body2, "0x2222222")

		statusCode1, _, body1 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode1)
		assert.Contains(t, body1, "0x1111111")
	})

	t.Run("UpstreamNotAllowedByDirectiveViaQueryParams", func(t *testing.T) {
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
									Retry:          nil,
									CircuitBreaker: nil,
									Hedge:          nil,
									Timeout:        nil,
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
									Retry:          nil,
									CircuitBreaker: nil,
									Hedge:          nil,
									Timeout:        nil,
								},
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
									Retry:          nil,
									CircuitBreaker: nil,
									Hedge:          nil,
									Timeout:        nil,
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
		defer util.AssertNoPendingMocks(t, 1)

		gock.New("http://rpc1.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      111,
				"result":  "0x1111111",
			})
		gock.New("http://rpc2.localhost").
			Post("/").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      222,
				"result":  "0x2222222",
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode2, _, body2 := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1}`, nil, map[string]string{
			"use-upstream": "rpc2",
		})

		assert.Equal(t, http.StatusOK, statusCode2)
		assert.Contains(t, body2, "0x2222222")
	})

	t.Run("ShouldReturnEmptyArrayWhenHedgeAndRetryDefinedOnBothNetworkAndUpstream", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Configure multiple upstreams to test failover behavior
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					NetworkDefaults: &common.NetworkDefaults{
						DirectiveDefaults: &common.DirectiveDefaultsConfig{
							RetryEmpty: &common.TRUE,
						},
					},
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 8,
										Delay:       common.Duration(100 * time.Millisecond),
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
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 3,
										Delay:       common.Duration(100 * time.Millisecond),
									},
								},
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
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 3,
										Delay:       common.Duration(100 * time.Millisecond),
									},
								},
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}
		err := cfg.SetDefaults(nil)
		require.NoError(t, err)

		// Mock the first upstream to return an empty result array
		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{}, // Empty array result
			})

		// Mock the second upstream to also return an empty result array
		gock.New("http://rpc2.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{}, // Empty array result
			})

		// Set up test fixtures
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Prepare a getLogs request with specific parameters
		requestBody := `{
			"jsonrpc": "2.0",
			"method": "eth_getBalance",
			"params": ["0x1234567890abcdef1234567890abcdef12345678"],
			"id": 1
		}`

		// Send the request
		headers := map[string]string{
			"X-ERPC-Skip-Interpolation": "true",
		}
		q := map[string]string{
			"skip-interpolation": "true",
		}
		statusCode, _, body := sendRequest(requestBody, headers, q)

		// Verify the response
		assert.Equal(t, http.StatusOK, statusCode, "Status code should be 200 OK")

		var response map[string]interface{}
		err = json.Unmarshal([]byte(body), &response)
		require.NoError(t, err, "Should be able to decode response")

		// Verify the result is an empty array, not an error
		assert.Contains(t, response, "result", "Response should contain 'result' field")
		assert.NotContains(t, response, "error", "Response should not contain 'error' field")

		// Verify the result is an empty array
		result, ok := response["result"].([]interface{})
		assert.True(t, ok, "Result should be an array")
		assert.Empty(t, result, "Result should be an empty array")
	})

	t.Run("ShouldNotRetryWhenRetryEmptyDisabled", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					// NOTE: RetryEmpty is NOT enabled here (defaults to false)
					NetworkDefaults: &common.NetworkDefaults{
						DirectiveDefaults: &common.DirectiveDefaultsConfig{},
					},
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 123},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{MaxAttempts: 8, Delay: common.Duration(100 * time.Millisecond)},
								},
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "rpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm:      &common.EvmUpstreamConfig{ChainId: 123},
							JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(100 * time.Millisecond)},
								},
							},
						},
						{
							Id:       "rpc2",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc2.localhost",
							Evm:      &common.EvmUpstreamConfig{ChainId: 123},
							JsonRpc:  &common.JsonRpcUpstreamConfig{SupportsBatch: &common.FALSE},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(100 * time.Millisecond)},
								},
							},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		// Mock only the first upstream (rpc1) to respond with empty array.
		gock.New("http://rpc1.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		gock.New("http://rpc2.localhost").
			Post("/").
			Times(1).
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(string(body), "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})

		// Set up fixtures
		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		requestBody := `{
			"jsonrpc": "2.0",
			"method": "eth_getBalance",
			"params": ["0x1234567890abcdef1234567890abcdef12345678"],
			"id": 1
		}`

		headers := map[string]string{
			"X-ERPC-Skip-Interpolation": "true",
		}
		statusCode, _, body := sendRequest(requestBody, headers, nil)

		assert.Equal(t, http.StatusOK, statusCode, "Status code should be 200 OK")

		var response map[string]interface{}
		err := json.Unmarshal([]byte(body), &response)
		require.NoError(t, err, "Should be able to decode response")

		// Should still return empty array
		assert.Contains(t, response, "result", "Response should contain 'result' field")
		assert.NotContains(t, response, "error", "Response should not contain 'error' field")

		result, ok := response["result"].([]interface{})
		assert.True(t, ok, "Result should be an array")
		assert.Empty(t, result, "Result should be an empty array")
	})
}
func TestHttpServer_IntegrationTests(t *testing.T) {
	t.Run("SuccessRequestWithNoParams", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
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
								ChainId: 123,
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

		time.Sleep(500 * time.Millisecond)

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"trace_transaction","id":111}`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x1111111")
	})

	t.Run("DrpcUnsupportedMethod", func(t *testing.T) {
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
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "drpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "http://rpc1.localhost",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
							},
							VendorName: thirdparty.CreateDrpcVendor().Name(),
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

		statusCode, _, _ := sendRequest(`{"jsonrpc":"2.0","method":"trace_transaction","params":[],"id":111}`, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)
	})

	t.Run("ReturnCorrectCORS", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
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
								ChainId: 123,
							},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "drpc1",
							Type:     common.UpstreamTypeEvm,
							Endpoint: "https://lb.drpc.org/ogrpc?network=ethereum",
							Evm: &common.EvmUpstreamConfig{
								ChainId: 123,
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
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
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
								ChainId: 123,
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
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
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
								ChainId: 123,
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
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
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
			path:        "/myproject/evm/123",
			method:      "POST",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "123",
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
			preSelectedChain: "123",
			wantProject:      "myproject",
			wantArch:         "evm",
			wantChain:        "123",
			wantAdmin:        false,
			wantHealthcheck:  true,
			wantErr:          false,
		},
		{
			name:            "Full path healthcheck",
			path:            "/myproject/evm/123/healthcheck",
			method:          "GET",
			wantProject:     "myproject",
			wantArch:        "evm",
			wantChain:       "123",
			wantAdmin:       false,
			wantHealthcheck: true,
			wantErr:         false,
		},
		{
			name:               "Project preselected, valid path",
			path:               "/evm/123",
			method:             "POST",
			preSelectedProject: "myproject",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "123",
			wantErr:            false,
		},
		{
			name:               "Project and arch preselected, valid path",
			path:               "/123",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "123",
			wantErr:            false,
		},
		{
			name:               "All preselected, valid empty path",
			path:               "/",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			preSelectedChain:   "123",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "123",
			wantErr:            false,
		},
		{
			name:               "All preselected, healthcheck",
			path:               "/healthcheck",
			method:             "GET",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			preSelectedChain:   "123",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "123",
			wantHealthcheck:    true,
			wantErr:            false,
		},
		{
			name:        "Invalid path - too many segments",
			path:        "/myproject/evm/123/extra",
			method:      "POST",
			wantErr:     true,
			errContains: "must only provide",
		},
		{
			name:        "Invalid path - wrong architecture",
			path:        "/myproject/x/123",
			method:      "POST",
			wantErr:     true,
			errContains: "architecture is not valid",
		},
		{
			name:               "Invalid path - project preselected but missing arch",
			path:               "/123",
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
			preSelectedChain:   "123",
			wantErr:            true,
			errContains:        "it is not possible to alias for project and chain WITHOUT architecture",
		},
		{
			name:        "Invalid path - empty architecture segment",
			path:        "/myproject//123",
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
			preSelectedChain: "123",
			wantProject:      "myproject",
			wantArch:         "evm",
			wantChain:        "123",
			wantErr:          false,
		},
		{
			name:             "Architecture and chain preselected with healthcheck",
			path:             "/myproject/healthcheck",
			method:           "GET",
			preSelectedArch:  "evm",
			preSelectedChain: "123",
			wantProject:      "myproject",
			wantArch:         "evm",
			wantChain:        "123",
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
			path:        "/myproject/123",
			method:      "POST",
			wantProject: "myproject",
			wantChain:   "123",
			wantErr:     true,
			errContains: "architecture is not valid",
		},
		{
			name:               "Only architecture provided",
			path:               "/evm",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedChain:   "123",
			wantErr:            true,
			errContains:        "it is not possible",
		},
		{
			name:               "Only chainId provided",
			path:               "/123",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "123",
			wantErr:            false,
		},
		{
			name:               "Architecture and chainId provided",
			path:               "/evm/123",
			method:             "POST",
			preSelectedProject: "myproject",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "123",
			wantErr:            false,
		},
		{
			name:               "project and architecture preselected, network alias in path",
			path:               "/arbitrum",
			method:             "POST",
			preSelectedProject: "myproject",
			preSelectedArch:    "evm",
			wantProject:        "myproject",
			wantArch:           "evm",
			wantChain:          "123",
			wantErr:            false,
		},
		{
			name:            "Architecture-only preselected, global healthcheck",
			path:            "/healthcheck",
			method:          "GET",
			preSelectedArch: "evm",
			wantProject:     "",
			wantArch:        "evm",
			wantChain:       "",
			wantAdmin:       false,
			wantHealthcheck: true,
			wantErr:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &HttpServer{
				draining: &atomic.Bool{},
				erpc: &ERPC{
					projectsRegistry: &ProjectsRegistry{
						preparedProjects: map[string]*PreparedProject{
							"myproject": {
								networksRegistry: &NetworksRegistry{
									aliasMu: &sync.RWMutex{},
									aliasToNetworkId: map[string]aliasEntry{
										"arbitrum": {
											architecture: "evm",
											chainID:      "123",
										},
									},
								},
							},
						},
					},
				},
			}
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
	testCtx, testCtxCancel := context.WithCancel(context.Background())
	defer testCtxCancel()

	logger := &log.Logger
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(logger, vr, []*common.ProviderConfig{}, nil)
	require.NoError(t, err)
	ssr, err := data.NewSharedStateRegistry(testCtx, logger, &common.SharedStateConfig{
		ClusterKey: "test",
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)
	mtk := health.NewTracker(logger, "test", 1*time.Second)
	up1 := &common.UpstreamConfig{
		Id:       "test-upstream",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://rpc1.localhost",
		Evm: &common.EvmUpstreamConfig{
			ChainId: 123,
		},
	}

	tests := []struct {
		name         string
		setupServer  func(ctx context.Context) *HttpServer
		projectId    string
		architecture string
		chainId      string
		request      *http.Request
		wantStatus   int
		wantBody     string
		wantCacheHit bool
	}{
		{
			name: "Basic healthcheck",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)
				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Project not found",
			setupServer: func(ctx context.Context) *HttpServer {
				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "nonexistent",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck"}},
			wantStatus:   http.StatusNotFound,
			wantBody:     "not configured",
		},
		{
			name: "Root healthcheck",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)
				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Verbose mode healthcheck",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)
				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeVerbose,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `"status":"OK"`,
		},
		{
			name: "Eval strategy - any:initializedUpstreams - Success",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)
				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=any:initializedUpstreams", Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Eval strategy - any:initializedUpstreams - Failure",
			setupServer: func(ctx context.Context) *HttpServer {
				upNoChainId := &common.UpstreamConfig{
					Id:       "test-upstream",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc666.localhost",
					// To force it fail init:
					//
					// Evm: &common.EvmUpstreamConfig{
					// 	ChainId: 123,
					// },
				}
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{upNoChainId}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{upNoChainId}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)
				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=any:initializedUpstreams", Path: "/healthcheck"}},
			wantStatus:   http.StatusBadGateway,
			wantBody:     `no upstreams initialized`,
		},
		{
			name: "Eval strategy - all:errorRateBelow90 - Success",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				time.Sleep(200 * time.Millisecond)

				up := pp.upstreamsRegistry.GetAllUpstreams()[0]
				metrics := mtk.GetUpstreamMethodMetrics(up, "*")
				metrics.RequestsTotal.Store(100)
				metrics.ErrorsTotal.Store(5) // 5% error rate

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=all:errorRateBelow90", Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Eval strategy - all:errorRateBelow90 - Failure",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				time.Sleep(200 * time.Millisecond)

				up := pp.upstreamsRegistry.GetAllUpstreams()[0]
				metrics := mtk.GetUpstreamMethodMetrics(up, "*")
				metrics.RequestsTotal.Store(100)
				metrics.ErrorsTotal.Store(99) // 99% error rate

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=all:errorRateBelow90", Path: "/healthcheck"}},
			wantStatus:   http.StatusBadGateway,
			wantBody:     `have high error rates`,
		},
		{
			name: "Eval strategy - any:errorRateBelow90 - Success",
			setupServer: func(ctx context.Context) *HttpServer {
				upBad := &common.UpstreamConfig{
					Id:       "bad-upstream",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}

				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1, upBad}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1, upBad}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				time.Sleep(200 * time.Millisecond)

				ups1 := pp.upstreamsRegistry.GetAllUpstreams()[0]
				metrics := mtk.GetUpstreamMethodMetrics(ups1, "*")
				metrics.RequestsTotal.Store(100)
				metrics.ErrorsTotal.Store(5) // 5% error rate

				upsBad := pp.upstreamsRegistry.GetAllUpstreams()[1]
				metricsBad := mtk.GetUpstreamMethodMetrics(upsBad, "*")
				metricsBad.RequestsTotal.Store(100)
				metricsBad.ErrorsTotal.Store(99) // 99% error rate

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=any:errorRateBelow90", Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Eval strategy - any:evm:eth_chainId with incorrect chain ID",
			setupServer: func(ctx context.Context) *HttpServer {
				util.ResetGock()
				defer util.ResetGock()

				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}

				// Correct chain ID
				gock.New("http://rpc1.localhost").
					Post("/").
					Filter(func(request *http.Request) bool {
						body := util.SafeReadBody(request)
						return strings.Contains(string(body), "eth_chainId")
					}).
					Reply(200).
					BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x7b"}`)

				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				time.Sleep(200 * time.Millisecond)

				gock.New("http://rpc1.localhost").
					Post("/").
					Filter(func(request *http.Request) bool {
						body := util.SafeReadBody(request)
						return strings.Contains(string(body), "eth_chainId")
					}).
					Reply(200).
					BodyString(`{"jsonrpc":"2.0","id":1,"result":"0x666"}`)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=any:evm:eth_chainId", Path: "/healthcheck"}},
			wantStatus:   http.StatusBadGateway,
			wantBody:     `chain id verification failed`,
		},
		{
			name: "With auth enabled and valid auth provided",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				authReg, _ := auth.NewAuthRegistry(ctx, logger, "test", &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
					{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Value: "test-secret"}},
				}}, nil)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					healthCheckAuthRegistry: authReg,
					draining:                &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request: &http.Request{
				Method: "GET",
				URL: &url.URL{
					Path:     "/healthcheck",
					RawQuery: "secret=test-secret",
				},
				Header: http.Header{},
			},
			wantStatus: http.StatusOK,
			wantBody:   `OK`,
		},
		{
			name: "Project with provider only - should be healthy",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Specific architecture and chainId",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "evm",
			chainId:      "123",
			request:      &http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Default eval strategy from config",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode:        common.HealthCheckModeSimple,
						DefaultEval: common.EvalAnyInitializedUpstreams,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Eval strategy - all:activeUpstreams - Success (all upstreams active)",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				time.Sleep(200 * time.Millisecond)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=all:activeUpstreams", Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Eval strategy - all:activeUpstreams - Failure (no upstreams configured)",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{}, Providers: []*common.ProviderConfig{}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=all:activeUpstreams", Path: "/healthcheck"}},
			wantStatus:   http.StatusBadGateway,
			wantBody:     `no upstreams configured`,
		},
		{
			name: "Eval strategy - all:activeUpstreams - Failure (uninitialized upstreams)",
			setupServer: func(ctx context.Context) *HttpServer {
				upNoChainId := &common.UpstreamConfig{
					Id:       "test-upstream",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc666.localhost",
					// Missing ChainId to force initialization failure
				}
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{upNoChainId}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{upNoChainId}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=all:activeUpstreams", Path: "/healthcheck"}},
			wantStatus:   http.StatusBadGateway,
			wantBody:     `0 / 1 upstreams are initialized`,
		},
		{
			name: "Eval strategy - all:activeUpstreams - Failure (cordoned upstreams)",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				time.Sleep(200 * time.Millisecond)

				// Cordon the upstream
				up := pp.upstreamsRegistry.GetAllUpstreams()[0]
				mtk.Cordon(up, "*", "test cordoning")

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=all:activeUpstreams", Path: "/healthcheck"}},
			wantStatus:   http.StatusBadGateway,
			wantBody:     `0 / 1 upstreams are active (1 cordoned)`,
		},
		{
			name: "Eval strategy - all:activeUpstreams - Success with providers configured",
			setupServer: func(ctx context.Context) *HttpServer {
				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1}, Providers: []*common.ProviderConfig{{Id: "test-provider", Vendor: "alchemy"}}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "",
			chainId:      "",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=all:activeUpstreams", Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
		{
			name: "Eval strategy - all:activeUpstreams - Network-specific upstream counting",
			setupServer: func(ctx context.Context) *HttpServer {
				// Create upstreams for different networks
				up1 := &common.UpstreamConfig{
					Id:       "test-upstream-123",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc1.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 123,
					},
				}
				up2 := &common.UpstreamConfig{
					Id:       "test-upstream-456",
					Type:     common.UpstreamTypeEvm,
					Endpoint: "http://rpc2.localhost",
					Evm: &common.EvmUpstreamConfig{
						ChainId: 456,
					},
				}

				pp := &PreparedProject{
					Config:            &common.ProjectConfig{Id: "test", Upstreams: []*common.UpstreamConfig{up1, up2}},
					upstreamsRegistry: upstream.NewUpstreamsRegistry(ctx, logger, "", []*common.UpstreamConfig{up1, up2}, ssr, nil, vr, pr, nil, mtk, 0*time.Second, nil),
				}
				pp.upstreamsRegistry.Bootstrap(ctx)
				pp.networksRegistry = NewNetworksRegistry(pp, ctx, pp.upstreamsRegistry, mtk, nil, nil, logger)

				return &HttpServer{
					logger: logger,
					erpc: &ERPC{
						projectsRegistry: &ProjectsRegistry{
							preparedProjects: map[string]*PreparedProject{
								"test": pp,
							},
						},
					},
					healthCheckCfg: &common.HealthCheckConfig{
						Mode: common.HealthCheckModeSimple,
					},
					draining: &atomic.Bool{},
				}
			},
			projectId:    "test",
			architecture: "evm",
			chainId:      "123",
			request:      &http.Request{Method: "GET", URL: &url.URL{RawQuery: "eval=all:activeUpstreams", Path: "/healthcheck"}},
			wantStatus:   http.StatusOK,
			wantBody:     `OK`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			util.ResetGock()
			defer util.ResetGock()
			util.SetupMocksForEvmStatePoller()

			ctx, ctxCancel := context.WithCancel(testCtx)
			defer ctxCancel()

			s := tt.setupServer(ctx)
			time.Sleep(50 * time.Millisecond)
			w := httptest.NewRecorder()
			startTime := time.Now()

			encoder := common.SonicCfg.NewEncoder(w)
			s.handleHealthCheck(ctx, w, tt.request, &startTime, tt.projectId, tt.architecture, tt.chainId, encoder, func(ctx context.Context, statusCode int, body error) {
				w.WriteHeader(statusCode)
				encoder.Encode(map[string]string{"error": body.Error()})
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
		_ = util.SetupMocksForUpstream("https://eth-mainnet.g.alchemy.com", map[string]interface{}{
			"chainId": "0x1",
		})

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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1234}`, nil, map[string]string{
			"chainId": "1",
		})
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")
	})

	t.Run("SimpleCallLazyLoadedNetwork", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
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
		_ = util.SetupMocksForUpstream("https://eth-mainnet.g.alchemy.com", map[string]interface{}{
			"chainId": "0x1",
		})

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

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1234}`, nil, map[string]string{
			"chainId": "1",
		})
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")
	})

	t.Run("RespectsOnlyNetworksWhenNoMatch", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(6 * time.Second).Ptr(),
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
		_ = util.SetupMocksForUpstream("https://eth-mainnet.g.alchemy.com", map[string]interface{}{
			"chainId": "0x1",
		})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// This request calls evm:1, which is not in the OnlyNetworks list
		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1234}`, nil, map[string]string{
			"chainId": "1",
		})
		assert.Equal(t, http.StatusNotFound, statusCode)
		assert.Contains(t, body, "ErrNetworkNotFound",
			"expected network evm:1 to not be recognized because only evm:5 and evm:10 are allowed")
	})

	t.Run("RespectsOnlyNetworksWhenMatch", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
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
		_ = util.SetupMocksForUpstream("https://eth-mainnet.g.alchemy.com", map[string]interface{}{
			"chainId": "0x1",
		})

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
		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getBalance","params":[],"id":1234}`, nil, map[string]string{
			"chainId": "1",
		})
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")
	})

	t.Run("EventuallyAddsUpstreamWhenSupportsNetworkFailsInitially", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
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
			Times(2).
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
			Times(2).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456",
			})

		gock.New("https://12340001234.rpc.hypersync.xyz").
			Post("/").
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x123456",
					"hash":   "0x1234567890",
				},
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		statusCode, _, body := sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x0","toBlock":"0x0"}],"id":1234}`, nil, map[string]string{"chainId": "12340001234"})
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")

		time.Sleep((3 * time.Second) + (200 * time.Millisecond))

		statusCode, _, body = sendRequest(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x0","toBlock":"0x0"}],"id":1234}`, nil, map[string]string{"chainId": "12340001234"})
		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")
	})

	t.Run("InheritUpstreamDefaultsConfig", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					UpstreamDefaults: &common.UpstreamConfig{
						Failsafe: []*common.FailsafeConfig{
							{
								Hedge: &common.HedgePolicyConfig{
									Delay:    common.Duration(10 * time.Millisecond),
									MaxCount: 267,
								},
							},
						},
						Evm: &common.EvmUpstreamConfig{
							StatePollerInterval: common.Duration(100 * time.Millisecond),
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

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Times(2).
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

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Persist().
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x123456",
					"hash":   "0x1234567890",
				},
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		time.Sleep(200 * time.Millisecond)

		statusCode, _, body := sendRequest(
			`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x0","toBlock":"0x0"}],"id":1234}`,
			nil,
			map[string]string{"chainId": "1"},
		)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)

		upstreams := prj.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:1")
		require.NotNil(t, upstreams)
		require.Equal(t, len(upstreams), 1)
		upsCfg := upstreams[0].Config()

		assert.Equalf(t, upsCfg.Failsafe[0].Hedge.MaxCount, 267, "Hedge policy maxCount should be set")
		assert.Equalf(t, upsCfg.Failsafe[0].Hedge.Delay, common.Duration(10*time.Millisecond), "Hedge policy delay should be set")
	})

	t.Run("InheritUpstreamsOverridesAfterUpstreamDefaultsConfig", func(t *testing.T) {
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
				ListenV4:   util.BoolPtr(true),
				HttpPortV4: util.IntPtr(4000),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					UpstreamDefaults: &common.UpstreamConfig{
						Failsafe: []*common.FailsafeConfig{
							{
								Hedge: &common.HedgePolicyConfig{
									Delay:    common.Duration(10 * time.Millisecond),
									MaxCount: 267,
								},
							},
						},
						Evm: &common.EvmUpstreamConfig{
							StatePollerInterval: common.Duration(100 * time.Millisecond),
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
									Failsafe: []*common.FailsafeConfig{
										{
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
			},
		}

		util.ResetGock()
		defer util.ResetGock()

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Times(2).
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

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "latest")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number": "0x123456",
					"hash":   "0x1234567890",
				},
			})

		// Add mocks for EVM state poller bootstrap requests
		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_syncing")
			}).
			Persist(). // State poller may call this multiple times
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  false, // Not syncing
			})

		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_blockNumber")
			}).
			Persist(). // State poller may call this multiple times
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0x123456", // Current block number
			})

		// Mock finalized block requests that state poller makes
		gock.New("https://eth-mainnet.g.alchemy.com").
			Post("/v2/test-key").
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, "finalized")
			}).
			Persist(). // State poller may call this multiple times
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result": map[string]interface{}{
					"number":    "0x123456",
					"hash":      "0x1234567890",
					"timestamp": "0x123456789",
				},
			})

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		time.Sleep(200 * time.Millisecond)

		statusCode, _, body := sendRequest(
			`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x0","toBlock":"0x0"}],"id":1234}`,
			nil,
			map[string]string{"chainId": "1"},
		)

		assert.Equal(t, http.StatusOK, statusCode)
		assert.Contains(t, body, "0x123456")

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)

		upstreams := prj.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:1")
		require.NotNil(t, upstreams)
		require.Equal(t, len(upstreams), 1)
		upsCfg := upstreams[0].Config()

		assert.Equalf(t, upsCfg.Failsafe[0].Retry.MaxAttempts, 123, "Retry policy should be set")
		assert.Nilf(t, upsCfg.Failsafe[0].Hedge, "Hedge policy should not be set")
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
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
								Integrity: &common.EvmIntegrityConfig{
									EnforceGetLogsBlockRange: util.BoolPtr(false),
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
								ChainId:                            123,
								GetLogsAutoSplittingRangeThreshold: 0x100,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 2,
									},
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

		// Give state poller time to initialize
		time.Sleep(500 * time.Millisecond)

		statusCode, _, body := sendRequest(fullRangeRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)

		// Parse and verify the response contains logs from all three ranges
		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err)

		logs, ok := respObject["result"].([]interface{})
		if !ok {
			t.Fatalf("Failed to parse logs: %v", respObject)
		}
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
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
								Integrity: &common.EvmIntegrityConfig{
									EnforceGetLogsBlockRange: util.BoolPtr(false),
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
								ChainId:                            123,
								GetLogsAutoSplittingRangeThreshold: 0x100,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 2,
									},
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

		// Mock failing responses for other ranges
		gock.New("http://rpc1.localhost").
			Post("").
			Times(6).
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getLogs") &&
					!(strings.Contains(body, `"fromBlock":"0x11118000"`) &&
						strings.Contains(body, `"toBlock":"0x111180ff"`))
			}).
			Reply(503).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"error": map[string]interface{}{
					"code":    -32603,
					"message": "Server error",
				},
			})

		sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
		defer shutdown()

		// Give state poller time to initialize
		time.Sleep(500 * time.Millisecond)

		// Make the request and verify it fails
		statusCode, _, body := sendRequest(fullRangeRequest, nil, nil)

		// Verify transport remains 200 for JSON-RPC failure
		assert.Equal(t, http.StatusOK, statusCode)

		// Parse the error response
		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err)

		// Verify error structure
		assert.Contains(t, respObject, "error")
		errorObj := respObject["error"].(map[string]interface{})
		assert.Equal(t, float64(-32603), errorObj["code"].(float64))
		assert.Contains(t, errorObj["message"].(string), "Server error")
	})
	t.Run("FailSplitIfOneOfSubRequestsFailsMissingData", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 1)

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
								Integrity: &common.EvmIntegrityConfig{
									EnforceGetLogsBlockRange: util.BoolPtr(false),
								},
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 2,
									},
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
								ChainId:                            123,
								GetLogsAutoSplittingRangeThreshold: 0x100,
							},
							Failsafe: []*common.FailsafeConfig{
								{
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 2,
									},
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

		// Give state poller time to initialize
		time.Sleep(500 * time.Millisecond)

		// Make the request and verify it fails
		statusCode, _, body := sendRequest(fullRangeRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)

		// Parse the error response
		var respObject map[string]interface{}
		err := sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err)

		// Verify error structure
		assert.Contains(t, respObject, "error")
		errorObj := respObject["error"].(map[string]interface{})
		assert.Equal(t, float64(-32014), errorObj["code"].(float64))
		assert.Contains(t, errorObj["message"].(string), "missing")
	})
}

func TestHttpServer_EvmGetBlockByNumber(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	t.Run("FallbackToHighestBlockIfUpstreamReturnsOlderBlock", func(t *testing.T) {
		// 1. Reset gock, set up poller mocks, etc.
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		// 2. Create a config with 2 upstreams and enforce highest block
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
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

		// 4. Spin up the test server, make the request, and check the final result
		// The test will use the state poller's mocks which return the latest block as 0x11118888
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize and update shared state
		time.Sleep(500 * time.Millisecond)

		headers := map[string]string{
			"X-ERPC-Skip-Interpolation": "true",
		}
		statusCode, _, body := sendRequest(requestBody, headers, nil)

		assert.Equal(t, http.StatusOK, statusCode, "should return 200 OK body: %s", body)

		t.Logf("Response body: %s", body)

		var respObject map[string]interface{}
		err = sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body")

		// 8. Confirm we got the highest known block from state poller
		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x22228888", result["number"], "should have the state poller's latest block number")
		// The test confirms that when an upstream returns an older block than what the state poller knows,
		// it falls back to requesting the highest known block
	})

	t.Run("ReturnBlockIfUpstreamReturnsNewerBlock", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Create a config with 1 upstream and enforce highest block.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
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

		// Spin up the test server, make the request, and check final result.
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize and update shared state
		time.Sleep(500 * time.Millisecond)

		statusCode, _, body := sendRequest(requestBody, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err = sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body successfully")

		// Confirm we got the state poller's latest block
		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x11118888", result["number"], "should return the state poller's latest block")
	})

	t.Run("ReturnsHigherAvailableBlockFromLaggingUpstreamWhenItSyncs", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Create a config with 2 upstreams and enforce highest block.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(100 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
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

		// eth_getBlockByNumber does NOT interpolate "latest" (TranslateLatestTag=false by default),
		// so the request goes to upstream with "latest" directly.
		// The state poller mock for rpc1 returns 0x11118888 for "latest".
		// EnforceHighestBlock will detect that 0x11118888 < 0x22228888 (highest known from rpc2)
		// and re-fetch using 0x22228888.
		// The state poller mock for rpc2 matches "0x22228888", so it will handle the re-fetch.

		// Spin up the test server, make the request, and check final result.
		// The test will use the state poller's mocks which return the latest block as 0x11118888
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize and update shared state
		time.Sleep(500 * time.Millisecond)

		statusCode, _, body := sendRequest(requestBody, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err = sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body successfully")

		// The test will return the highest state poller's latest block (0x22228888)
		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x22228888", result["number"], "should return the state poller's latest block")
	})

	t.Run("ReturnsHigherAvailableBlockFromUpToDateUpstreamWhenOneIsLagging", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Create a config with 2 upstreams and enforce highest block.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(100 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
							Failsafe: []*common.FailsafeConfig{
								{
									// This is necessary to ensure that request will be retried towards rpc2 when rpc1 still returns a block not found error.
									Retry: &common.RetryPolicyConfig{
										MaxAttempts: 2,
									},
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
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

		// eth_getBlockByNumber does NOT interpolate "latest" (TranslateLatestTag=false by default),
		// so the request goes to upstream with "latest" directly.
		// The state poller mock for rpc1 returns 0x11118888 for "latest".
		// EnforceHighestBlock will detect that 0x11118888 < 0x22228888 (highest known from rpc2)
		// and re-fetch using 0x22228888.
		// The state poller mock for rpc2 matches "0x22228888", so it will handle the re-fetch and return the block.

		// Spin up the test server, make the request, and check final result.
		// The test will use the state poller's mocks which return the latest block as 0x11118888
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize and update shared state
		time.Sleep(500 * time.Millisecond)

		statusCode, _, body := sendRequest(requestBody, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err = sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body successfully")

		// The test will return the highest state poller's latest block (0x22228888)
		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x22228888", result["number"], "should return the state poller's latest block")
	})

	t.Run("ReturnsMissingDataIfAllUpstreamsReturnNull", func(t *testing.T) {
		util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.ResetGock()
		defer util.AssertNoPendingMocks(t, 0)

		// Create a config with 1 upstream and enforce highest block.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(100 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
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
			"params": ["0x777", false]
		}`

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"0x777"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      999,
				"result":  nil,
			})

		// Spin up the test server, make the request, and check final result.
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		time.Sleep(50 * time.Millisecond)

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		statusCode, _, body := sendRequest(requestBody, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err = sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body successfully")

		assert.Contains(t, body, "block not found")
		assert.Contains(t, body, "-32014")
	})

	t.Run("FailFastIfSharedStateIsTooSlow", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Create a config with 1 upstream and enforce highest block.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(100 * time.Second).Ptr(),
			},
			Database: &common.DatabaseConfig{
				SharedState: &common.SharedStateConfig{
					Connector: &common.ConnectorConfig{
						Driver: "mock",
						Mock: &common.MockConnectorConfig{
							MemoryConnectorConfig: common.MemoryConnectorConfig{
								MaxItems: 100_000, MaxTotalSize: "1GB",
							},
							GetDelay: 100 * time.Minute,
							SetDelay: 100 * time.Minute,
						},
					},
					FallbackTimeout: common.Duration(100 * time.Millisecond),
				},
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(1000 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(1000 * time.Second),
							},
						},
					},
				},
			},
		}

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": "0x7b"})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": "0x7b"})

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
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x777"}}`))
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x777"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"finalized"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x775"}}`))
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"finalized"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x775"}}`))

		// Mock "rpc1" returning the user's newer block 0x123 in response to the request
		// for the method "eth_getBlockByNumber('latest')".
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				// Accept any eth_getBlockByNumber user call for rpc1; integrity logic will verify contents
				return strings.Contains(body, "eth_getBlockByNumber")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      999,
				"result": map[string]interface{}{
					"number": "0x123", // Lower than highest latest block (0x777)
					"hash":   "0x6a251dd26d1f5b4c84bb434fea376a91c8d37c11dc8b496ca1e79a9101906df7",
				},
			})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"0x777"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      999,
				"result":  nil,
			})

		// Spin up the test server, make the request, and check final result.
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize and update shared state
		time.Sleep(500 * time.Millisecond)

		statusCode, _, body := sendRequest(requestBody, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err = sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body successfully")

		assert.Contains(t, body, "0x123")

	})

	t.Run("FailFastIfSharedStateIsDown", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Create a config with 1 upstream and enforce highest block.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(100 * time.Second).Ptr(),
			},
			Database: &common.DatabaseConfig{
				SharedState: &common.SharedStateConfig{
					Connector: &common.ConnectorConfig{
						Driver: "mock",
						Mock: &common.MockConnectorConfig{
							MemoryConnectorConfig: common.MemoryConnectorConfig{
								MaxItems: 100_000, MaxTotalSize: "1GB",
							},
							GetErrorRate: 1,
							SetErrorRate: 1,
						},
					},
					FallbackTimeout: common.Duration(100 * time.Millisecond),
				},
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(1000 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(1000 * time.Second),
							},
						},
					},
				},
			},
		}

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": "0x7b"})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": "0x7b"})

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
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x777"}}`))
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"latest"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x777"}}`))

		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"finalized"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x775"}}`))
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"finalized"`)
			}).
			Reply(200).
			JSON([]byte(`{"result":{"number":"0x775"}}`))

		// Mock "rpc1" returning the user's newer block 0x123 in response to the request
		// for the method "eth_getBlockByNumber('latest')".
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				// Accept any eth_getBlockByNumber user call for rpc1; integrity logic will verify contents
				return strings.Contains(body, "eth_getBlockByNumber")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      999,
				"result": map[string]interface{}{
					"number": "0x123", // Lower than highest latest block (0x777)
					"hash":   "0x6a251dd26d1f5b4c84bb434fea376a91c8d37c11dc8b496ca1e79a9101906df7",
				},
			})
		gock.New("http://rpc2.localhost").
			Post("").
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, `"0x777"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      999,
				"result":  nil,
			})

		// Spin up the test server, make the request, and check final result.
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize and update shared state
		time.Sleep(500 * time.Millisecond)

		statusCode, _, body := sendRequest(requestBody, nil, nil)

		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err = sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body successfully")

		assert.Contains(t, body, "0x123")

	})

	t.Run("ShouldSkipHookIfHighestBlockCheckDisabled", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0) // We use Times(2) not Persist() for the user call

		// Provide a config where Evm.Integrity.EnforceHighestBlock = false
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		// We'll request the latest block from upstream
		// Because enforceHighestBlock = false, we expect no override logic (the server should just return the direct result).
		requestBody := `{
			"jsonrpc": "2.0",
			"id":      999,
			"method":  "eth_getBlockByNumber",
			"params": ["latest", false]
		}`

		// Spin up the test server and send the request
		// The test will use the state poller's mocks
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize
		time.Sleep(500 * time.Millisecond)

		statusCode, _, body := sendRequest(requestBody, nil, nil)
		t.Logf("Response body: %s", body)
		assert.Equal(t, http.StatusOK, statusCode)

		var respObject map[string]interface{}
		err = sonic.UnmarshalString(body, &respObject)
		assert.NoError(t, err, "should parse response body")

		// With enforceHighestBlock = false, the server forwards the request and gets the response
		// from the state poller's mock (0x11118888). This is correct behavior - no override is happening.
		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x11118888", result["number"], "server should forward request normally when enforceHighestBlock is disabled")
	})

	t.Run("ShouldReturnSuccessWithRealStatePoller", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Provide a config with Evm.Integrity.EnforceHighestBlock = true
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		// We'll request the latest block from upstream
		requestBody := `{
			"jsonrpc": "2.0",
			"id":      777,
			"method":  "eth_getBlockByNumber",
			"params": ["latest", true]
		}`

		// Spin up the test server and make the request
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize
		time.Sleep(500 * time.Millisecond)

		statusCode, _, respBody := sendRequest(requestBody, nil, nil)
		assert.Equal(t, 200, statusCode, "should return success")

		// Verify the server returns the state poller's mock data
		var respObject map[string]interface{}
		err = sonic.UnmarshalString(respBody, &respObject)
		assert.NoError(t, err, "should parse response body")

		result, hasResult := respObject["result"].(map[string]interface{})
		assert.True(t, hasResult, "response should have a 'result' field")
		assert.Equal(t, "0x11118888", result["number"], "should return the state poller's latest block")
	})

	t.Run("ShouldNoopIfParamIsDirectHex", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Provide a config with EnforceHighestBlock = true, so we can confirm
		// that the hook is skipped if the param is a direct hex number.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

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

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize
		time.Sleep(500 * time.Millisecond)

		statusCode, _, respBody := sendRequest(userRequestBody, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)

		var respObj map[string]interface{}
		err = sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		result := respObj["result"].(map[string]interface{})
		assert.Equal(t, "0xabc_hex", result["hash"], "direct hex param should not be overridden")
	})

	t.Run("ShouldNoopIfParamIsEarliestOrPending", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.AssertNoPendingMocks(t, 0)

		// Provide a config with EnforceHighestBlock = true. We'll confirm the hook
		// is skipped for 'earliest' and 'pending'.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize
		time.Sleep(500 * time.Millisecond)

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

		statusCodeEarliest, _, respBodyEarliest := sendRequest(userRequestEarliest, nil, nil)
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

		statusCodePending, _, respBodyPending := sendRequest(userRequestPending, nil, nil)
		assert.Equal(t, http.StatusOK, statusCodePending)

		var respObjPending map[string]interface{}
		errPending := sonic.UnmarshalString(respBodyPending, &respObjPending)
		require.NoError(t, errPending, "should parse pending response")

		resultPending := respObjPending["result"].(map[string]interface{})
		assert.Equal(t, "0xpending", resultPending["hash"], "pending block should be returned unchanged")
	})

	// NOTE: This test is rewritten to rely on poller state and interpolation
	// to return the highest finalized block across upstreams.
	// rpc1 finalized=0x11117777, rpc2 finalized=0x22227777 via util.SetupMocksForEvmStatePoller
	// (duplicate removed; covered earlier)
	// Verifies behavior when interpolation is explicitly disabled via directive
	// The proxy should keep the "finalized" tag in the first request, then enforce
	// the highest finalized block by issuing a follow-up request to the newer block number.
	// Highest finalized from poller mocks: rpc1=0x11117777, rpc2=0x22227777
	// Expect final result to be 0x22227777.

	t.Run("ShouldHonorSkipInterpolationOnFinalized", func(t *testing.T) {
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
								ChainId:             123,
								StatePollerInterval: common.Duration(30 * time.Second),
								StatePollerDebounce: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(30 * time.Second),
								StatePollerDebounce: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Allow poller to initialize
		time.Sleep(500 * time.Millisecond)

		requestBody := `{
			"jsonrpc": "2.0",
			"id":      42,
			"method":  "eth_getBlockByNumber",
			"params": ["finalized", false]
		}`

		headers := map[string]string{
			"X-ERPC-Skip-Interpolation": "true",
		}

		statusCode, _, body := sendRequest(requestBody, headers, nil)
		assert.Equal(t, http.StatusOK, statusCode)

		var resp map[string]interface{}
		err = sonic.UnmarshalString(body, &resp)
		require.NoError(t, err)
		result, ok := resp["result"].(map[string]interface{})
		require.True(t, ok, "the response must contain a 'result' object")
		assert.Equal(t, "0x22227777", result["number"], "should return highest finalized when skip-interpolation is true")
	})

	t.Run("ShouldOverrideIfUpstreamReturnsOlderBlockOnFinalized", func(t *testing.T) {
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
								ChainId:             123,
								StatePollerInterval: common.Duration(30 * time.Second),
								StatePollerDebounce: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(30 * time.Second),
								StatePollerDebounce: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize and populate highest finalized
		time.Sleep(500 * time.Millisecond)

		// eth_getBlockByNumber does NOT interpolate "finalized" (TranslateFinalizedTag=false by default),
		// so the request goes to upstream with "finalized" directly.
		// The state poller mock for rpc1 returns 0x11117777 for finalized, and rpc2 returns 0x22227777.
		// EnforceHighestBlock will detect that rpc1's response (0x11117777) is older than the highest
		// known finalized (0x22227777 from rpc2), and will re-fetch using that block number.
		// The state poller mocks already handle both "finalized" and the concrete hex block numbers.

		userRequest := `{
			"jsonrpc": "2.0",
			"id":      42,
			"method":  "eth_getBlockByNumber",
			"params": ["finalized", false]
		}`

		statusCode, _, respBody := sendRequest(userRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode)

		var respObj map[string]interface{}
		err = sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		result, hasResult := respObj["result"].(map[string]interface{})
		require.True(t, hasResult, "the response must contain a 'result' object")

		// Highest finalized across poller mocks is 0x22227777 (rpc2)
		assert.Equal(t, "0x22227777", result["number"], "returns the highest finalized block from poller")
	})

	t.Run("ShouldStickWithUpstreamBlockIfItMatchesHighestOnFinalized", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Use a config with 2 upstreams, EnforceHighestBlock = true,
		// so we can confirm that even with multiple upstreams, no fallback request is triggered if
		// the first upstream's "finalized" block is already the highest known block.
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		// ----------------------------
		// Poller Mock Setup
		// ----------------------------
		gock.New("http://rpc1.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": "0x7b"})
		gock.New("http://rpc2.localhost").
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{"result": "0x7b"})

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
		// Make these mocks specific to NOT match the user request (id:999)
		gock.New("http://rpc1.localhost").
			Post("").
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"finalized"`) && !strings.Contains(body, `"id":999`)
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
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"finalized"`) && !strings.Contains(body, `"id":999`)
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

		// The user request mock should be more specific and placed after the general mocks
		// eth_getBlockByNumber does NOT interpolate "finalized" (TranslateFinalizedTag=false by default),
		// so the request goes to upstream with "finalized" directly.
		gock.New("http://rpc1.localhost").
			Post("").
			Times(1). // Only match once for the specific user request
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				// Match the exact user request with id:999 and "finalized" tag (NOT interpolated)
				return strings.Contains(body, `"id":999`) &&
					strings.Contains(body, `"eth_getBlockByNumber"`) &&
					strings.Contains(body, `"finalized"`) &&
					strings.Contains(body, `true`) // Full block data requested
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
		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		// Give state poller time to initialize and learn about finalized blocks
		time.Sleep(1 * time.Second)

		statusCode, _, respBody := sendRequest(userRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "overall request should be successful")

		// Parse the response
		var respObj map[string]interface{}
		err = sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		// Log the actual response for debugging
		t.Logf("Response body: %s", respBody)

		// Ensure that result is still 0x100 and the same hash, meaning we used the original upstream response
		result, hasResult := respObj["result"].(map[string]interface{})
		require.True(t, hasResult, "must have a 'result' object in the JSON")

		assert.Equal(t, "0x100", result["number"], "should match poller's highest finalized block with no override")
		assert.Equal(t, "0x222222bb83862798c81820bbaae8bfbc542b8abf73c130583f2b36521cf10624", result["hash"], "should remain the same as upstream's response")
	})

	t.Run("ShouldHandleInvalidBlockTag", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		// Provide a config with EnforceHighestBlock = true so we can confirm that
		// the logic still skips override if the block tag is not one of the recognized keywords.
		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		// Mock poller calls: "eth_syncing" => false, then "latest" => 0x300, just so the poller
		// has some known state. We won't use it because the block tag is invalid, so the hook shouldn't apply.
		for _, rpc := range cfg.Projects[0].Upstreams {
			gock.New(rpc.Endpoint).
				Post("").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_chainId")
				}).
				Reply(200).
				JSON(map[string]interface{}{"result": "0x7b"})

			gock.New(rpc.Endpoint).
				Post("").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_syncing")
				}).
				Reply(200).
				JSON(map[string]interface{}{"result": false})

			gock.New(rpc.Endpoint).
				Post("").
				Persist().
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
		}

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

		statusCode, _, respBody := sendRequest(userRequestBody, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "the server should pass the response as-is")

		var respObj map[string]interface{}
		err := sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		// Confirm the server didn't do any override logic; it just returned whatever the upstream said.
		result := respObj["result"].(map[string]interface{})
		assert.Equal(t, "0xabcd", result["number"], "invalid block tag should not trigger any override")
		assert.Equal(t, "0xhash_for_unknownTag", result["hash"], "the block hash should match upstream exactly")

	})

	t.Run("ShouldHandleSuccessfulConsensus", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
							Failsafe: []*common.FailsafeConfig{
								{
									MatchMethod: "*",
									Consensus: &common.ConsensusPolicyConfig{
										MaxParticipants:         3,
										AgreementThreshold:      2,
										DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
										LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
									},
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc3",
							Endpoint: "http://rpc3.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		// Mock poller calls: "eth_syncing" => false, then "latest" => 0x300, just so the poller
		// has some known state. We won't use it because the block tag is invalid, so the hook shouldn't apply.
		for _, rpc := range cfg.Projects[0].Upstreams {
			gock.New(rpc.Endpoint).
				Post("").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_chainId")
				}).
				Reply(200).
				JSON(map[string]interface{}{"result": "0x7b"})

			gock.New(rpc.Endpoint).
				Post("").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_syncing")
				}).
				Reply(200).
				JSON(map[string]interface{}{"result": false})

			gock.New(rpc.Endpoint).
				Post("").
				Persist().
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
		}

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()
		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		userRequest := `{
			"jsonrpc": "2.0",
			"id": 5010,
			"method": "eth_getBlockByNumber",
			"params": ["0x300", true]
		}`

		for _, rpc := range cfg.Projects[0].Upstreams {
			gock.New(rpc.Endpoint).
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, `"eth_getBlockByNumber"`) &&
						strings.Contains(body, `"0x300"`)
				}).
				Reply(200).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1, // ID doesn't matter for the response
					"result": map[string]interface{}{
						"number": "0x300",
						"hash":   "0xhash_for_consensus",
					},
				})
		}

		statusCode, _, respBody := sendRequest(userRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "status code should indicate success")

		var respObj map[string]interface{}
		err = sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		result, ok := respObj["result"].(map[string]interface{})
		assert.True(t, ok, "result should be present")
		assert.Equal(t, "0x300", result["number"], "the block number should match the upstream's response")
		assert.Equal(t, "0xhash_for_consensus", result["hash"], "the block hash should match upstream exactly")
	})

	t.Run("ShouldHandleConsensusFailureDueToLowParticipants", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
							Failsafe: []*common.FailsafeConfig{
								{
									MatchMethod: "*",
									Consensus: &common.ConsensusPolicyConfig{
										MaxParticipants:         3,
										AgreementThreshold:      2,
										DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
										LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
									},
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		// Mock poller calls: "eth_syncing" => false, then "latest" => 0x300, just so the poller
		// has some known state. We won't use it because the block tag is invalid, so the hook shouldn't apply.
		for _, rpc := range cfg.Projects[0].Upstreams {
			gock.New(rpc.Endpoint).
				Post("").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_chainId")
				}).
				Reply(200).
				JSON(map[string]interface{}{"result": "0x7b"})

			gock.New(rpc.Endpoint).
				Post("").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_syncing")
				}).
				Reply(200).
				JSON(map[string]interface{}{"result": false})

			gock.New(rpc.Endpoint).
				Post("").
				Persist().
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
		}

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		defer shutdown()
		userRequest := `{
			"jsonrpc": "2.0",
			"id": 5010,
			"method": "eth_getBlockByNumber",
			"params": ["0x300", true]
		}`

		gock.New("http://rpc1.localhost").
			Post("").
			// Due to consensus policy implementation currently, we will send 3 requests
			// to the same upstream but then fail with low participants as opposed to short circuiting.
			Times(3).
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, `"eth_getBlockByNumber"`) &&
					strings.Contains(body, `"0x300"`)
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1, // ID doesn't matter for the response
				"result": map[string]interface{}{
					"number": "0x300",
					"hash":   "0xhash_for_consensus",
				},
			})

		statusCode, _, respBody := sendRequest(userRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "transport should be 200 for JSON-RPC failures")

		var respObj map[string]interface{}
		err = sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		errResp, ok := respObj["error"].(map[string]interface{})
		assert.True(t, ok, "error should be present")
		assert.Equal(t, float64(-32603), errResp["code"], "error code should be -32603")
		assert.Contains(t, errResp["message"], "not enough participants", "error message should be 'not enough participants'")
	})

	t.Run("ShouldHandleConsensusFailureDueToDispute", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				MaxTimeout: common.Duration(5 * time.Second).Ptr(),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_project",
					Networks: []*common.NetworkConfig{
						{
							Architecture: "evm",
							Evm: &common.EvmNetworkConfig{
								ChainId: 123,
								Integrity: &common.EvmIntegrityConfig{
									EnforceHighestBlock: util.BoolPtr(true),
								},
							},
							Failsafe: []*common.FailsafeConfig{
								{
									MatchMethod: "*",
									Consensus: &common.ConsensusPolicyConfig{
										MaxParticipants:         3,
										AgreementThreshold:      2,
										DisputeBehavior:         common.ConsensusDisputeBehaviorReturnError,
										LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
									},
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
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc2",
							Endpoint: "http://rpc2.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
						{
							Id:       "rpc3",
							Endpoint: "http://rpc3.localhost",
							Type:     common.UpstreamTypeEvm,
							Evm: &common.EvmUpstreamConfig{
								ChainId:             123,
								StatePollerInterval: common.Duration(10 * time.Second),
							},
						},
					},
				},
			},
		}

		// Mock poller calls: "eth_syncing" => false, then "latest" => 0x300, just so the poller
		// has some known state. We won't use it because the block tag is invalid, so the hook shouldn't apply.
		for _, rpc := range cfg.Projects[0].Upstreams {
			gock.New(rpc.Endpoint).
				Post("").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_chainId")
				}).
				Reply(200).
				JSON(map[string]interface{}{"result": "0x7b"})

			gock.New(rpc.Endpoint).
				Post("").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_syncing")
				}).
				Reply(200).
				JSON(map[string]interface{}{"result": false})

			gock.New(rpc.Endpoint).
				Post("").
				Persist().
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
		}

		sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
		defer shutdown()

		prj, err := erpcInstance.GetProject("test_project")
		require.NoError(t, err)
		upstream.ReorderUpstreams(prj.upstreamsRegistry)

		userRequest := `{
			"jsonrpc": "2.0",
			"id": 5010,
			"method": "eth_getBlockByNumber",
			"params": ["0x300", true]
		}`

		for idx, rpc := range cfg.Projects[0].Upstreams {
			gock.New(rpc.Endpoint).
				Post("").
				Filter(func(r *http.Request) bool {
					body := util.SafeReadBody(r)
					return strings.Contains(body, `"eth_getBlockByNumber"`) &&
						strings.Contains(body, `"0x300"`)
				}).
				Reply(200).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1, // ID doesn't matter for the response
					"result": map[string]interface{}{
						"number": "0x300",
						"hash":   fmt.Sprintf("0xhash_for_consensus_%d", idx),
					},
				})
		}

		statusCode, _, respBody := sendRequest(userRequest, nil, nil)
		assert.Equal(t, http.StatusOK, statusCode, "transport should be 200 for JSON-RPC failures")

		var respObj map[string]interface{}
		err = sonic.UnmarshalString(respBody, &respObj)
		require.NoError(t, err, "should parse response body")

		errResp, ok := respObj["error"].(map[string]interface{})
		assert.True(t, ok, "error should be present")
		assert.Equal(t, float64(-32603), errResp["code"], "error code should be -32603")
		assert.Equal(t, "not enough agreement among responses", errResp["message"], "error message should be 'not enough agreement among responses'")
	})
}

func createServerTestFixtures(cfg *common.Config, t *testing.T) (
	func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string),
	func(host string) (int, map[string]string, string),
	string,
	func(),
	*ERPC,
) {
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())

	var ssr data.SharedStateRegistry
	var err error
	if cfg != nil && cfg.Database != nil && cfg.Database.SharedState != nil {
		ssr, err = data.NewSharedStateRegistry(ctx, &logger, cfg.Database.SharedState)
	} else {
		ssr, err = data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
			Connector: &common.ConnectorConfig{
				Driver: "memory",
				Memory: &common.MemoryConnectorConfig{
					MaxItems: 100_000, MaxTotalSize: "1GB",
				},
			},
		})
	}
	if err != nil {
		panic(err)
	}

	// Apply defaults to set up IPv4 server configuration if cfg is provided
	if cfg != nil {
		err = cfg.SetDefaults(nil)
		require.NoError(t, err)

		// Force IPv4 server to be enabled for all tests (override test mode defaults)
		// In test mode, SetDefaults() doesn't enable IPv4 unless FORCE_TEST_LISTEN_V4=true
		if cfg.Server != nil {
			cfg.Server.ListenV4 = util.BoolPtr(true)
		}
	}

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)

	// Callback now set at construction; do not mutate in tests

	erpcInstance.Bootstrap(ctx)
	require.NoError(t, err)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		err := httpServer.serverV4.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Server error: %v", err)
		}
	}()

	time.Sleep(500 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)

	sendRequest := func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string) {
		rctx, cancel := context.WithTimeout(ctx, 100*time.Second)
		defer cancel()
		chainId := "123"
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
			return 0, nil, err.Error()
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, nil, err.Error()
		}
		respHeaders := make(map[string]string)
		for k, v := range resp.Header {
			respHeaders[k] = v[0]
		}
		return resp.StatusCode, respHeaders, string(respBody)
	}

	sendOptionsRequest := func(host string) (int, map[string]string, string) {
		req, err := http.NewRequestWithContext(ctx, "OPTIONS", baseURL+"/test_project/evm/123", nil)
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
		httpServer.serverV4.Shutdown(ctx)
		listener.Close()
		cancel()

		// TODO Can we do it more strictly maybe via a Shutdown() sequence?
		// This to wait for batch processor to stop + initializers to finish
		time.Sleep(500 * time.Millisecond)
	}, erpcInstance
}

func TestHttpServer_Evm_GetLogs_MemoryProfile(t *testing.T) {
	t.Skip("Skipping memory profile test in CI - run locally for profiling")
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})
	util.SetupMocksForEvmStatePoller()
	// Allow four persistent eth_getLogs upstream mocks to remain pending
	defer util.AssertNoPendingMocks(t, 4)

	// Upstream mocks: 4 providers returning small-but-nontrivial logs arrays
	buildLogs := func(n int) []map[string]interface{} {
		logs := make([]map[string]interface{}, 0, n)
		for i := 0; i < n; i++ {
			logs = append(logs, map[string]interface{}{
				"address":         fmt.Sprintf("0x%040x", i+1),
				"topics":          []string{"0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"},
				"data":            "0x",
				"blockNumber":     "0x123456",
				"blockHash":       "0xabcdef",
				"transactionHash": fmt.Sprintf("0x%064x", i+1000),
				"logIndex":        fmt.Sprintf("0x%x", i),
				"blockTimestamp":  "0x65f9a2b0",
			})
		}
		return logs
	}

	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  buildLogs(60),
	}

	for i := 1; i <= 4; i++ {
		gock.New(fmt.Sprintf("http://rpc%d.localhost", i)).
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				b := util.SafeReadBody(request)
				return strings.Contains(b, "eth_getLogs")
			}).
			Reply(200).
			JSON(resp)
	}

	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture:      common.ArchitectureEvm,
						Evm:               &common.EvmNetworkConfig{ChainId: 123, Integrity: &common.EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(true)}},
						DirectiveDefaults: &common.DirectiveDefaultsConfig{RetryPending: util.BoolPtr(false), RetryEmpty: util.BoolPtr(true)},
						Failsafe: []*common.FailsafeConfig{
							{
								MatchMethod:   "eth_getLogs|eth_getTransactionReceipt|eth_getBlockReceipts",
								MatchFinality: []common.DataFinalityState{common.DataFinalityStateUnfinalized},
								Timeout:       &common.TimeoutPolicyConfig{Duration: common.Duration(10 * time.Second)},
								Hedge:         &common.HedgePolicyConfig{Quantile: 0.95, MaxCount: 1, MinDelay: common.Duration(100 * time.Millisecond), MaxDelay: common.Duration(10 * time.Second)},
								Retry:         &common.RetryPolicyConfig{MaxAttempts: 4, Delay: 0, EmptyResultConfidence: common.AvailbilityConfidenceBlockHead, EmptyResultIgnore: []string{"eth_getLogs"}, EmptyResultMaxAttempts: 1},
								Consensus: &common.ConsensusPolicyConfig{
									AgreementThreshold:      2,
									MaxParticipants:         4,
									LowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
									DisputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
									IgnoreFields:            map[string][]string{"eth_getLogs": {"*.blockTimestamp"}},
								},
							},
						},
					},
				},
				Upstreams: []*common.UpstreamConfig{{Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}, {Type: common.UpstreamTypeEvm, Endpoint: "http://rpc2.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}, {Type: common.UpstreamTypeEvm, Endpoint: "http://rpc3.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}, {Type: common.UpstreamTypeEvm, Endpoint: "http://rpc4.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	// Setup mocks for state poller initialization
	util.ResetGock()
	defer util.ResetGock()

	// We need to set up mocks for each upstream for the state poller
	for _, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost", "http://rpc3.localhost", "http://rpc4.localhost"} {
		// Mock eth_chainId for feature detection
		gock.New(host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"result":  "0x7b", // ChainId 123
			})

		// Mock eth_syncing
		gock.New(host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_syncing")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"result":  false,
			})

		// Mock eth_getBlockByNumber for latest
		gock.New(host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"result": map[string]interface{}{
					"number": "0xfb01f8", // Block 16445432 in hex
				},
			})

		// Mock eth_getBlockByNumber for finalized
		gock.New(host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "finalized")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"result": map[string]interface{}{
					"number": "0xfb01f0", // Slightly lower than latest
				},
			})

		// Add mocks for eth_getLogs to return a valid response
		gock.New(host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_getLogs")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  []interface{}{},
			})
	}

	ssCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
	}
	// Apply defaults to ensure proper timeout values
	ssCfg.SetDefaults("test")

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, ssCfg)
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	// Give state poller more time to initialize and update shared state
	time.Sleep(1 * time.Second)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(50 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	send := func() (*http.Response, string, error) {
		payload := `{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0xfaeff2","toBlock":"0xfaeff8","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}],"id":12345}`
		req, err := http.NewRequest("POST", baseURL+"/test_project/evm/123", strings.NewReader(payload))
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", err
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(b), nil
	}

	// Warmup
	for i := 0; i < 5; i++ {
		_, _, _ = send()
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < 20; i++ {
		resp, body, err := send()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, body, "result")
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	t.Logf("Alloc delta: %.2f MB, HeapInuse delta: %.2f MB, TotalAlloc: %.2f MB", float64(int64(after.Alloc)-int64(before.Alloc))/1024.0/1024.0, float64(int64(after.HeapInuse)-int64(before.HeapInuse))/1024.0/1024.0, float64(int64(after.TotalAlloc)-int64(before.TotalAlloc))/1024.0/1024.0)

	dir := t.TempDir()
	heapPath := filepath.Join(dir, "heap_getlogs.pb")
	f, err := os.Create(heapPath)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, pprof.Lookup("heap").WriteTo(f, 0))
	t.Logf("heap profile saved to: %s", heapPath)
}
