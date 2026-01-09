package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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

// This test validates that split-on-error happens end-to-end at project-level (post-forward)
// and returns a merged response when the upstream responds with a 413-like large-range error.
func TestHttp_EvmGetLogs_SplitOnError_MergedResponse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		// Allow localhost connections for the test HTTP server; mock only upstream
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// 1) Initial upstream call returns request-too-large (range) error
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getLogs") }).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    int(common.JsonRpcErrorCallException),
				"message": "Request exceeds the range",
			},
		})

	// 2) After split-on-error, sub-requests should be issued; respond to two ranges
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x18100\"") && strings.Contains(b, "\"toBlock\":\"0x181ff\"")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      2,
			"result": []map[string]interface{}{
				{"blockNumber": "0x18101", "logIndex": "0x2"},
			},
		})
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x18200\"") && strings.Contains(b, "\"toBlock\":\"0x182ff\"")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      3,
			"result": []map[string]interface{}{
				{"blockNumber": "0x18202", "logIndex": "0x3"},
			},
		})

	// --- Real server setup (mirrors http_server_test.go style) ---
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitOnError: util.BoolPtr(true)},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}},
	})
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)
	require.NoError(t, err)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		_ = httpServer.serverV4.Serve(listener)
	}()
	defer httpServer.serverV4.Shutdown(ctx)
	// Wait a bit for server
	time.Sleep(50 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	send := func(body string, headers map[string]string) (int, string) {
		req, err := http.NewRequest("POST", baseURL+"/test_project/evm/123", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	payload := `{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x18100","toBlock":"0x182ff","topics":["0x1234"]}],"id":1}`
	status, body := 0, ""
	status, body = send(payload, nil)
	if status != 200 {
		t.Fatalf("unexpected status: %d body=%s", status, body)
	}
	if !strings.Contains(body, "\"result\":") {
		t.Fatalf("expected merged result, got: %s", body)
	}
	if !(strings.Contains(body, "\"0x18101\"") && strings.Contains(body, "\"0x18202\"")) {
		t.Fatalf("expected logs from both sub-requests, got: %s", body)
	}
}

// This test validates proactive range splitting (pre-forward) at network-level, ensuring
// eth_getLogs is broken into contiguous block sub-requests and the merged response is returned.
func TestHttp_EvmGetLogs_ProactiveRangeSplit_MergedResponse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		// Allow localhost connections for the test HTTP server; mock only upstream
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Sub-requests expected (threshold=2, range=0x100..0x104): [0x100-0x101], [0x102-0x103], [0x104-0x104]
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x100\"") && strings.Contains(b, "\"toBlock\":\"0x101\"")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      2,
			"result": []map[string]interface{}{
				{"blockNumber": "0x100", "logIndex": "0x1"},
			},
		})

	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x102\"") && strings.Contains(b, "\"toBlock\":\"0x103\"")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      3,
			"result": []map[string]interface{}{
				{"blockNumber": "0x103", "logIndex": "0x2"},
			},
		})

	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x104\"") && strings.Contains(b, "\"toBlock\":\"0x104\"")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      4,
			"result": []map[string]interface{}{
				{"blockNumber": "0x104", "logIndex": "0x3"},
			},
		})

	// --- Real server setup ---
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitOnError: util.BoolPtr(true), GetLogsSplitConcurrency: 8},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123, GetLogsAutoSplittingRangeThreshold: 2},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}},
	})
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)
	require.NoError(t, err)

	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(50 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	send := func(body string, headers map[string]string) (int, string) {
		req, err := http.NewRequest("POST", baseURL+"/test_project/evm/123", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	payload := `{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x100","toBlock":"0x104"}],"id":1}`
	status, body := send(payload, nil)
	if status != 200 {
		t.Fatalf("unexpected status: %d body=%s", status, body)
	}
	if !strings.Contains(body, "\"result\":") {
		t.Fatalf("expected merged result, got: %s", body)
	}
	// Validate parsing and presence of 3 logs
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	arr, _ := out["result"].([]interface{})
	require.Equal(t, 3, len(arr))
}

// On-error split by addresses for single-block range; ensure merged output contains both halves
func TestHttp_EvmGetLogs_SplitOnError_ByAddresses_MergedResponse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		return strings.Split(req.URL.Host, ":")[0] == "localhost"
	})
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// 1) Initial upstream call returns large-range style JSON-RPC error to trigger split-on-error
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getLogs") }).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    int(common.JsonRpcErrorEvmLargeRange),
				"message": "Request exceeds the limit",
			},
		})

	// 2) Sub-requests after split by addresses [0xA],[0xB]
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x1\"") && strings.Contains(b, "\"toBlock\":\"0x1\"") && strings.Contains(b, "\"address\":[\"0xA\"]")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      2,
			"result": []map[string]interface{}{
				{"blockNumber": "0x1", "logIndex": "0x1", "address": "0xA"},
			},
		})

	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x1\"") && strings.Contains(b, "\"toBlock\":\"0x1\"") && strings.Contains(b, "\"address\":[\"0xB\"]")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      3,
			"result": []map[string]interface{}{
				{"blockNumber": "0x1", "logIndex": "0x2", "address": "0xB"},
			},
		})

	// --- Real server setup ---
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{
			{
				Id:        "test_project",
				Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitOnError: util.BoolPtr(true)}}},
				Upstreams: []*common.UpstreamConfig{{Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)
	require.NoError(t, err)
	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(50 * time.Millisecond)
	baseURL := fmt.Sprintf("http://localhost:%d", port)

	payload := `{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x1","address":["0xA","0xB"]}],"id":1}`
	req, err := http.NewRequest("POST", baseURL+"/test_project/evm/123", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, 200, resp.StatusCode, string(b))
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &out))
	arr, _ := out["result"].([]interface{})
	require.Equal(t, 2, len(arr))
}

// On-error split by topics[0] OR-list for single-block range
func TestHttp_EvmGetLogs_SplitOnError_ByTopic0ORList_MergedResponse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool { return strings.Split(req.URL.Host, ":")[0] == "localhost" })
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Initial error to trigger split-on-error
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getLogs") }).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    int(common.JsonRpcErrorEvmLargeRange),
				"message": "too large",
			},
		})

	// Subrequest for topic0 = 0xT1
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"topics\":[[\"0xT1\"]]")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      2,
			"result":  []map[string]interface{}{{"blockNumber": "0x2", "logIndex": "0x1", "topics": []string{"0xT1"}}},
		})

	// Subrequest for topic0 = 0xT2
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"topics\":[[\"0xT2\"]]")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      3,
			"result":  []map[string]interface{}{{"blockNumber": "0x2", "logIndex": "0x2", "topics": []string{"0xT2"}}},
		})

	// Server
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)
	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{{
			Id:        "test_project",
			Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitOnError: util.BoolPtr(true)}}},
			Upstreams: []*common.UpstreamConfig{{Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}},
		}},
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)
	require.NoError(t, err)
	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(50 * time.Millisecond)

	payload := `{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x2","toBlock":"0x2","topics":[["0xT1","0xT2"]]}],"id":1}`
	req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d/test_project/evm/123", port), strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, 200, resp.StatusCode, string(b))
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &out))
	arr, _ := out["result"].([]interface{})
	require.Equal(t, 2, len(arr))
}

// On-error split where one sub-request returns an empty array and the other returns logs.
// Merged response should include only non-empty results.
func TestHttp_EvmGetLogs_SplitOnError_EmptyAndNonEmptyMergedSkipsEmpty(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool { return strings.Split(req.URL.Host, ":")[0] == "localhost" })
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Initial large-range error
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getLogs") }).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error":   map[string]interface{}{"code": int(common.JsonRpcErrorEvmLargeRange), "message": "too large"},
		})

	// Subrequest 1: empty []
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x10\"") && strings.Contains(b, "\"toBlock\":\"0x10\"")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 2, "result": []interface{}{}})

	// Subrequest 2: non-empty
	gock.New("http://rpc1.localhost").
		Post("/").
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getLogs") && strings.Contains(b, "\"fromBlock\":\"0x11\"") && strings.Contains(b, "\"toBlock\":\"0x11\"")
		}).
		Times(1).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 3, "result": []map[string]interface{}{{"blockNumber": "0x11", "logIndex": "0x1"}}})

	// Server
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)
	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{{
			Id:        "test_project",
			Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitOnError: util.BoolPtr(true)}}},
			Upstreams: []*common.UpstreamConfig{{Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}},
		}},
	}
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)
	require.NoError(t, err)
	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(50 * time.Millisecond)

	payload := `{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x10","toBlock":"0x11"}],"id":1}`
	req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d/test_project/evm/123", port), strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, 200, resp.StatusCode, string(b))
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &out))
	arr, _ := out["result"].([]interface{})
	require.Equal(t, 1, len(arr))
}

// Reproduces concurrent identical requests over HTTP to exercise multiplexer and response lifecycle.
// Ensures no client ever receives a parse error like "no body available to parse JsonRpcResponse".
func TestHttp_ConcurrentIdenticalRequests_NoEmptyBodyParse(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool { return strings.Split(req.URL.Host, ":")[0] == "localhost" })
	util.SetupMocksForEvmStatePoller()
	// 1 user mock kept Persist(), so expect 1 pending besides poller mocks
	defer util.AssertNoPendingMocks(t, 1)

	// Upstream mock for eth_getBalance (persist to allow many rounds)
	gock.New("http://rpc1.localhost").
		Post("/").
		Persist().
		Filter(func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), "eth_getBalance") }).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      0,
			"result":  "0xabcdef",
		})

	// --- Server setup ---
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer util.CancelAndWait(cancel)

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{{
			Id:        "test_project",
			Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
			Upstreams: []*common.UpstreamConfig{{Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}}},
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{Connector: &common.ConnectorConfig{Driver: "memory", Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"}}})
	require.NoError(t, err)
	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)
	require.NoError(t, err)
	httpServer, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, erpcInstance)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	go func() { _ = httpServer.serverV4.Serve(listener) }()
	defer httpServer.serverV4.Shutdown(ctx)
	time.Sleep(50 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	mkReq := func(id int) *http.Request {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0x123","latest"],"id":%d}`, id)
		req, e := http.NewRequest("POST", baseURL+"/test_project/evm/123", strings.NewReader(body))
		require.NoError(t, e)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	rounds := 50
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		wg.Add(2)

		// First request
		go func(iter int) {
			defer wg.Done()
			resp, e := client.Do(mkReq(100000 + iter*2))
			require.NoError(t, e)
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			require.Equal(t, 200, resp.StatusCode, string(b))
			// must not contain parse error
			require.NotContains(t, string(b), "no body available to parse JsonRpcResponse")
		}(i)

		// Second, identical request (different id → multiplex eligible)
		go func(iter int) {
			defer wg.Done()
			resp, e := client.Do(mkReq(100001 + iter*2))
			require.NoError(t, e)
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			require.Equal(t, 200, resp.StatusCode, string(b))
			require.NotContains(t, string(b), "no body available to parse JsonRpcResponse")
		}(i)

		wg.Wait()
	}
}
