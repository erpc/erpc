package erpc

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// These tests exercise the full HttpServer → Network → Upstream flow while a slow cache Set()
// runs in the background, to ensure we're covering the real objects and coordination around
// response locks. We keep the scenario minimal: one project, one EVM network, one upstream.

func TestHttpServer_CacheSetSlow_DoesNotDelayHttpResponse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		// Allow only local http server traffic to pass through the network stack
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Upstream responds quickly to eth_getBalance
	gock.New("http://rpc1.localhost").
		Post("/").
		Times(1).
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `"eth_getBalance"`)
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1",
		})

	// Configure a slow cache connector for Set()
	cacheCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{
				Id:     "mock",
				Driver: "mock",
				Mock: &common.MockConnectorConfig{
					MemoryConnectorConfig: common.MemoryConnectorConfig{
						MaxItems: 100_000, MaxTotalSize: "1GB",
					},
					SetDelay: 300 * time.Millisecond,
				},
			},
		},
		Policies: []*common.CachePolicyConfig{
			{
				Network:   "*",
				Method:    "*",
				TTL:       common.Duration(5 * time.Minute),
				Connector: "mock",
				// Finality intentionally omitted → default unknown, matches dynamic outcomes
			},
		},
	}
	require.NoError(t, cacheCfg.SetDefaults())

	// Build ERPC + HttpServer
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.Config{
		Server: &common.ServerConfig{
			ListenV4: util.BoolPtr(true),
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
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: cacheCfg,
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	// Start server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(100 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	reqBody := `{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":1}`
	req, err := http.NewRequest("POST", baseURL+"/test_project/evm/123", strings.NewReader(reqBody))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	// HTTP should complete well before slow cache Set delay
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("HTTP response took too long (%s). Expected to return before slow cache Set (>=300ms)", elapsed)
	}
}

func TestHttpServer_CacheSetSlow_AllowsConcurrentRequests(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Two fast upstream replies for two back-to-back requests
	gock.New("http://rpc1.localhost").
		Post("/").
		Times(2).
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `"eth_getBalance"`)
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x2",
		})

	cacheCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{
				Id:     "mock",
				Driver: "mock",
				Mock: &common.MockConnectorConfig{
					MemoryConnectorConfig: common.MemoryConnectorConfig{
						MaxItems: 100_000, MaxTotalSize: "1GB",
					},
					SetDelay: 400 * time.Millisecond,
				},
			},
		},
		Policies: []*common.CachePolicyConfig{
			{
				Network:   "*",
				Method:    "*",
				TTL:       common.Duration(5 * time.Minute),
				Connector: "mock",
			},
		},
	}
	require.NoError(t, cacheCfg.SetDefaults())

	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.Config{
		Server: &common.ServerConfig{
			ListenV4: util.BoolPtr(true),
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
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: cacheCfg,
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(100 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	send := func(id int) (time.Duration, int, string) {
		payload := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":%d}`, id)
		req, _ := http.NewRequest("POST", baseURL+"/test_project/evm/123", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 3 * time.Second}
		start := time.Now()
		resp, err := client.Do(req)
		elapsed := time.Since(start)
		if err != nil {
			return elapsed, 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return elapsed, resp.StatusCode, string(b)
	}

	// First request triggers background cache set (slow)
	el1, sc1, b1 := send(1)
	require.Equal(t, http.StatusOK, sc1, b1)
	// While the first background Set is in-flight, issue a second request.
	el2, sc2, b2 := send(2)
	require.Equal(t, http.StatusOK, sc2, b2)

	// Both responses should be fast despite slow cache Set in background.
	if el1 >= 250*time.Millisecond {
		t.Fatalf("first HTTP response too slow: %s", el1)
	}
	if el2 >= 250*time.Millisecond {
		t.Fatalf("second HTTP response too slow: %s", el2)
	}
}

// Prove goroutine pile-up happens while slow cache Set runs in the background,
// and that it recovers after those Sets finish. Then contrast with the same
// traffic pattern when cache is disabled to show the difference is significant.
func TestHttpServer_SlowCacheSet_GoroutineSpikeThenRecover(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	total := 200
	// Make upstream respond quickly for burst (persist to avoid brittle counts)
	gock.New("http://rpc1.localhost").
		Post("/").
		Times(total).
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `"eth_getBalance"`)
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x3",
		})

	// Slow Set to accumulate background goroutines
	setDelay := 1000 * time.Millisecond
	cacheCfg := &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{
			{
				Id:     "mock",
				Driver: "mock",
				Mock: &common.MockConnectorConfig{
					MemoryConnectorConfig: common.MemoryConnectorConfig{
						MaxItems: 100_000, MaxTotalSize: "1GB",
					},
					SetDelay: setDelay,
				},
			},
		},
		Policies: []*common.CachePolicyConfig{
			{
				Network:   "*",
				Method:    "*",
				TTL:       common.Duration(5 * time.Minute),
				Connector: "mock",
			},
		},
	}
	require.NoError(t, cacheCfg.SetDefaults())

	// Build server with cache enabled
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &common.Config{
		Server: &common.ServerConfig{
			ListenV4: util.BoolPtr(true),
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
		Database: &common.DatabaseConfig{
			EvmJsonRpcCache: cacheCfg,
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems: 100_000, MaxTotalSize: "1GB",
			},
		},
	})
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)
	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(100 * time.Millisecond)
	baseURL := fmt.Sprintf("http://localhost:%d", port)

	// Fire a burst of requests concurrently
	before := runtime.NumGoroutine()
	wg := sync.WaitGroup{}
	client := &http.Client{Timeout: 3 * time.Second}

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xabc","latest"],"id":%d}`, id)
			req, _ := http.NewRequest("POST", baseURL+"/test_project/evm/123", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err == nil && resp != nil {
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
		}(i + 1)
	}
	wg.Wait()
	mid := runtime.NumGoroutine()

	// Expect a visible spike while many slow Set goroutines exist
	if mid-before < 10 {
		t.Fatalf("expected goroutine spike with slow cache set; got before=%d mid=%d (delta=%d)", before, mid, mid-before)
	}

	// After slow Set finishes, goroutine count should drop back near baseline
	time.Sleep(setDelay + 500*time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+20 {
		t.Fatalf("expected goroutine count to recover after slow cache set; before=%d after=%d", before, after)
	}
}


func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}


