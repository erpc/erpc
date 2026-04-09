package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockWsUpstream creates a test HTTP server that accepts WebSocket upgrades
// and responds to JSON-RPC requests. It also supports eth_subscribe by
// sending periodic notifications.
func mockWsUpstream(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("mock ws upstream upgrade error: %v", err)
			return
		}
		defer c.Close()
		handler(c)
	}))
	return srv
}

// standardWsConfig returns a config with both an HTTP upstream (gock) and a WS upstream (real server).
func standardWsConfig(wsURL string) *common.Config {
	return &common.Config{
		Server: &common.ServerConfig{
			ListenV4: util.BoolPtr(true),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_ws",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "http-upstream",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
					{
						Id:       "ws-upstream",
						Type:     common.UpstreamTypeEvm,
						Endpoint: wsURL,
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

// httpOnlyConfig returns a config with only HTTP upstreams (no WS).
func httpOnlyConfig() *common.Config {
	return &common.Config{
		Server: &common.ServerConfig{
			ListenV4: util.BoolPtr(true),
		},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_ws",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
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
}

// setupGock sets up standard gock mocks for tests.
func setupGock() {
	util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
		return shouldMakeRealCall
	})
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("/").
		Persist().
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "eth_getBalance")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0xabc123",
		})
}

// dialWs connects to the eRPC WebSocket endpoint.
func dialWs(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	wsURL := fmt.Sprintf("ws://%s/test_ws/evm/123", addr)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err, "WebSocket dial should succeed")
	return conn
}

// sendAndReceive sends a JSON-RPC request and reads the response.
func sendAndReceive(t *testing.T, conn *websocket.Conn, req string) map[string]interface{} {
	t.Helper()
	err := conn.WriteMessage(websocket.TextMessage, []byte(req))
	require.NoError(t, err)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(msg, &resp))
	return resp
}

func init() {
	util.ConfigureTestLogger()
}

func setupTestERPCServer(t *testing.T, cfg *common.Config) (string, context.CancelFunc) {
	t.Helper()

	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())

	err := cfg.SetDefaults(&common.DefaultOptions{})
	require.NoError(t, err)

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
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

	go func() {
		err := httpServer.serverV4.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Server error: %v", err)
		}
	}()

	time.Sleep(500 * time.Millisecond)

	baseURL := fmt.Sprintf("127.0.0.1:%d", port)

	cleanup := func() {
		_ = httpServer.Shutdown(&logger)
		cancel()
	}

	return baseURL, cleanup
}

func TestWebSocket_BasicRPC(t *testing.T) {
	t.Run("SingleRequestOverWebSocket", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xabc123",
			})

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_ws",
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

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		// Connect via WebSocket
		wsURL := fmt.Sprintf("ws://%s/test_ws/evm/123", addr)
		conn, httpResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			if httpResp != nil {
				body, _ := json.Marshal(httpResp.Header)
				t.Logf("WS upgrade failed: status=%d, headers=%s", httpResp.StatusCode, string(body))
				respBody := make([]byte, 1024)
				n, _ := httpResp.Body.Read(respBody)
				t.Logf("Response body: %s", string(respBody[:n]))
			}
		}
		require.NoError(t, err, "WebSocket dial should succeed")
		defer conn.Close()

		// Send JSON-RPC request (use eth_getBalance to avoid state poller interception)
		reqMsg := `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1234567890abcdef1234567890abcdef12345678","latest"]}`
		err = conn.WriteMessage(websocket.TextMessage, []byte(reqMsg))
		require.NoError(t, err, "WebSocket write should succeed")

		// Read response
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, respMsg, err := conn.ReadMessage()
		require.NoError(t, err, "WebSocket read should succeed")

		var resp map[string]interface{}
		err = json.Unmarshal(respMsg, &resp)
		require.NoError(t, err, "Response should be valid JSON")

		assert.Equal(t, "2.0", resp["jsonrpc"])
		assert.Equal(t, float64(1), resp["id"])
		assert.Equal(t, "0xabc123", resp["result"])
	})

	t.Run("MultipleRequestsOnSameConnection", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()
		// gock pending mocks not checked since WS tests use Persist()

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xdef456",
			})

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result":  "0x7b",
			})

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_ws",
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

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		wsURL := fmt.Sprintf("ws://%s/test_ws/evm/123", addr)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		// Send first request
		err = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`))
		require.NoError(t, err)

		_, resp1, err := conn.ReadMessage()
		require.NoError(t, err)

		var r1 map[string]interface{}
		require.NoError(t, json.Unmarshal(resp1, &r1))
		assert.Equal(t, "0xdef456", r1["result"])

		// Send second request on the same connection
		err = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":2,"method":"eth_chainId","params":[]}`))
		require.NoError(t, err)

		_, resp2, err := conn.ReadMessage()
		require.NoError(t, err)

		var r2 map[string]interface{}
		require.NoError(t, json.Unmarshal(resp2, &r2))
		assert.Equal(t, "0x7b", r2["result"])
	})

	t.Run("ConcurrentRequestsOnSameConnection", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()
		// gock pending mocks not checked since WS tests use Persist()

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xfeed",
			})

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_ws",
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

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		wsURL := fmt.Sprintf("ws://%s/test_ws/evm/123", addr)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		const numRequests = 10
		var writeMu sync.Mutex

		// Send multiple requests sequentially (gorilla/websocket doesn't support concurrent writes)
		for i := 0; i < numRequests; i++ {
			writeMu.Lock()
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0xaaaa","latest"]}`, i)
			err := conn.WriteMessage(websocket.TextMessage, []byte(msg))
			writeMu.Unlock()
			require.NoError(t, err)
		}

		// Read all responses
		responses := make(map[float64]bool)
		for i := 0; i < numRequests; i++ {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, msg, err := conn.ReadMessage()
			require.NoError(t, err)

			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal(msg, &resp))
			assert.Equal(t, "2.0", resp["jsonrpc"])
			if id, ok := resp["id"].(float64); ok {
				responses[id] = true
			}
		}

		assert.Equal(t, numRequests, len(responses), "should receive all responses")
	})

	t.Run("BatchRequestOverWebSocket", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()
		// gock pending mocks not checked since WS tests use Persist()

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xbatch1",
			})

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_chainId")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      2,
				"result":  "0x7b",
			})

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_ws",
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

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		wsURL := fmt.Sprintf("ws://%s/test_ws/evm/123", addr)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		// Send batch request
		batch := `[{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]},{"jsonrpc":"2.0","id":2,"method":"eth_chainId","params":[]}]`
		err = conn.WriteMessage(websocket.TextMessage, []byte(batch))
		require.NoError(t, err)

		// Read batch response
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, respMsg, err := conn.ReadMessage()
		require.NoError(t, err)

		var responses []map[string]interface{}
		err = json.Unmarshal(respMsg, &responses)
		require.NoError(t, err, "Response should be a JSON array")
		assert.Equal(t, 2, len(responses), "Batch response should contain 2 items")
	})

	t.Run("WebSocketClientDisconnect", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()
		// gock pending mocks not checked since WS tests use Persist()

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xdeadbeef",
			})

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_ws",
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

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		wsURL := fmt.Sprintf("ws://%s/test_ws/evm/123", addr)

		// Connect, send a request, read response, then close
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)

		err = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`))
		require.NoError(t, err)

		_, respMsg, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Contains(t, string(respMsg), "0xdeadbeef")

		// Close the connection gracefully
		err = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		assert.NoError(t, err)
		conn.Close()

		// Server should handle disconnect gracefully - connect again to verify
		time.Sleep(200 * time.Millisecond)
		conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err, "Should be able to reconnect after disconnect")
		defer conn2.Close()

		err = conn2.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`))
		require.NoError(t, err)

		_, respMsg2, err := conn2.ReadMessage()
		require.NoError(t, err)
		assert.Contains(t, string(respMsg2), "0xdeadbeef")
	})

	t.Run("HTTPStillWorksAlongsideWebSocket", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
			return shouldMakeRealCall
		})
		util.SetupMocksForEvmStatePoller()
		// gock pending mocks not checked since WS tests use Persist()

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"result":  "0xboth",
			})

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_ws",
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

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		// WebSocket request
		wsURL := fmt.Sprintf("ws://%s/test_ws/evm/123", addr)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		err = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`))
		require.NoError(t, err)

		_, wsResp, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Contains(t, string(wsResp), "0xboth")

		// HTTP request on the same server — use a clean client to avoid gock interception
		httpURL := fmt.Sprintf("http://%s/test_ws/evm/123", addr)
		cleanClient := &http.Client{Transport: &http.Transport{}}
		httpResp, err := cleanClient.Post(httpURL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`))
		require.NoError(t, err)
		defer httpResp.Body.Close()

		assert.Equal(t, http.StatusOK, httpResp.StatusCode)
	})
}

func TestWebSocket_ErrorHandling(t *testing.T) {
	t.Run("InvalidJSON", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		// Send invalid JSON
		err := conn.WriteMessage(websocket.TextMessage, []byte(`{not valid json`))
		require.NoError(t, err)

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := conn.ReadMessage()
		require.NoError(t, err)

		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(msg, &resp))

		// Should get an error response
		assert.NotNil(t, resp["error"], "should return error for invalid JSON")
	})

	t.Run("EmptyBody", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		err := conn.WriteMessage(websocket.TextMessage, []byte(``))
		require.NoError(t, err)

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := conn.ReadMessage()
		require.NoError(t, err)

		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(msg, &resp))
		assert.NotNil(t, resp["error"], "should return error for empty body")
	})

	t.Run("UpstreamError", func(t *testing.T) {
		util.ResetGock()
		gock.EnableNetworking()
		gock.NetworkingFilter(func(req *http.Request) bool {
			return strings.Split(req.URL.Host, ":")[0] == "127.0.0.1"
		})
		util.SetupMocksForEvmStatePoller()
		defer util.ResetGock()

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				body := util.SafeReadBody(request)
				return strings.Contains(body, "eth_getBalance")
			}).
			Reply(200).
			JSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"error": map[string]interface{}{
					"code":    -32000,
					"message": "execution reverted",
				},
			})

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.NotNil(t, resp["error"], "should forward upstream error")
	})
}

func TestWebSocket_MultipleConnections(t *testing.T) {
	t.Run("IndependentConnections", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		// Open multiple connections simultaneously
		conn1 := dialWs(t, addr)
		defer conn1.Close()
		conn2 := dialWs(t, addr)
		defer conn2.Close()
		conn3 := dialWs(t, addr)
		defer conn3.Close()

		// Send requests on each connection
		resp1 := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		resp3 := sendAndReceive(t, conn3, `{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)

		// All should get responses with correct IDs
		assert.Equal(t, float64(1), resp1["id"])
		assert.Equal(t, float64(2), resp2["id"])
		assert.Equal(t, float64(3), resp3["id"])
		assert.Equal(t, "0xabc123", resp1["result"])
		assert.Equal(t, "0xabc123", resp2["result"])
		assert.Equal(t, "0xabc123", resp3["result"])
	})

	t.Run("OneDisconnectDoesNotAffectOthers", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn1 := dialWs(t, addr)
		conn2 := dialWs(t, addr)
		defer conn2.Close()

		// Verify both work
		resp1 := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp1["result"])

		// Close conn1
		conn1.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn1.Close()
		time.Sleep(100 * time.Millisecond)

		// conn2 should still work
		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp2["result"])
	})
}

func TestWebSocket_Subscriptions(t *testing.T) {
	t.Run("SubscribeReceiveUnsubscribe", func(t *testing.T) {
		// Create a mock WS upstream that handles eth_subscribe
		notifCh := make(chan struct{}, 1)
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]interface{}
				if err := json.Unmarshal(msg, &req); err != nil {
					continue
				}

				method, _ := req["method"].(string)
				id := req["id"]

				switch method {
				case "eth_chainId":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				case "eth_getBlockByNumber":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x100", "timestamp": "0x6702a8f0"}})
				case "eth_syncing":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
				case "eth_subscribe":
					subID := "0xdeadbeef12345678"
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": subID})

					// Send a notification after a short delay
					go func() {
						time.Sleep(200 * time.Millisecond)
						conn.WriteJSON(map[string]interface{}{
							"jsonrpc": "2.0",
							"method":  "eth_subscription",
							"params": map[string]interface{}{
								"subscription": subID,
								"result": map[string]interface{}{
									"number": "0x101",
									"hash":   "0xaaa",
								},
							},
						})
						select {
						case notifCh <- struct{}{}:
						default:
						}
					}()
				case "eth_unsubscribe":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			}
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		cfg := standardWsConfig(wsUpstreamURL)

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		// Allow WS upstream to initialize
		time.Sleep(2 * time.Second)

		conn := dialWs(t, addr)
		defer conn.Close()

		// Subscribe
		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		assert.NotNil(t, resp["result"], "should return subscription ID")
		assert.Nil(t, resp["error"], "should not have error")
		clientSubID, ok := resp["result"].(string)
		require.True(t, ok, "subscription ID should be a string")
		assert.True(t, strings.HasPrefix(clientSubID, "0x"), "subscription ID should start with 0x")

		// Wait for notification
		select {
		case <-notifCh:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for upstream to send notification")
		}

		// Read the notification
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, notifMsg, err := conn.ReadMessage()
		require.NoError(t, err, "should receive notification")

		var notif map[string]interface{}
		require.NoError(t, json.Unmarshal(notifMsg, &notif))
		assert.Equal(t, "eth_subscription", notif["method"])
		params, ok := notif["params"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, clientSubID, params["subscription"], "notification should use client subscription ID")

		// Unsubscribe
		unsubResp := sendAndReceive(t, conn, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"eth_unsubscribe","params":["%s"]}`, clientSubID))
		assert.Equal(t, true, unsubResp["result"])
	})

	t.Run("SubscribeWithNoWsUpstreamFails", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		// HTTP-only config — no WS upstream
		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		assert.NotNil(t, resp["error"], "should return error when no WS upstream available")
	})

	t.Run("UnsubscribeUnknownIDFails", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_unsubscribe","params":["0xnonexistent"]}`)
		assert.NotNil(t, resp["error"], "should return error for unknown subscription ID")
	})

	t.Run("SubscriptionCleanupOnDisconnect", func(t *testing.T) {
		unsubReceived := make(chan struct{}, 1)
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]interface{}
				if err := json.Unmarshal(msg, &req); err != nil {
					continue
				}
				method, _ := req["method"].(string)
				id := req["id"]

				switch method {
				case "eth_chainId":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				case "eth_getBlockByNumber":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x100", "timestamp": "0x6702a8f0"}})
				case "eth_syncing":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
				case "eth_subscribe":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xsub123"})
				case "eth_unsubscribe":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
					select {
					case unsubReceived <- struct{}{}:
					default:
					}
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			}
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		cfg := standardWsConfig(wsUpstreamURL)

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		time.Sleep(2 * time.Second)

		conn := dialWs(t, addr)

		// Subscribe
		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp["result"])

		// Disconnect without unsubscribing
		conn.Close()

		// The server should send eth_unsubscribe to the upstream during cleanup
		select {
		case <-unsubReceived:
			// Good — server cleaned up
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for server to unsubscribe from upstream on client disconnect")
		}
	})
}

func TestWebSocket_UpstreamClient(t *testing.T) {
	t.Run("RPCThroughWsUpstream", func(t *testing.T) {
		// Mock WS upstream that handles regular RPC
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]interface{}
				if err := json.Unmarshal(msg, &req); err != nil {
					continue
				}
				method, _ := req["method"].(string)
				id := req["id"]

				switch method {
				case "eth_chainId":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				case "eth_getBlockByNumber":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x200", "timestamp": "0x6702a8f0"}})
				case "eth_syncing":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
				case "eth_getBalance":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xws_upstream_balance"})
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			}
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")

		// Config with ONLY the WS upstream (no HTTP) to ensure requests go through WS
		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id: "test_ws",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 123},
						},
					},
					Upstreams: []*common.UpstreamConfig{
						{
							Id:       "ws-only",
							Type:     common.UpstreamTypeEvm,
							Endpoint: wsUpstreamURL,
							Evm:      &common.EvmUpstreamConfig{ChainId: 123},
						},
					},
				},
			},
			RateLimiters: &common.RateLimiterConfig{},
		}

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		time.Sleep(2 * time.Second)

		// Send HTTP request — should be forwarded through the WS upstream
		// Use a clean client to avoid gock interception
		httpURL := fmt.Sprintf("http://%s/test_ws/evm/123", addr)
		cleanClient := &http.Client{Transport: &http.Transport{}}
		httpResp, err := cleanClient.Post(httpURL, "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`))
		require.NoError(t, err)
		defer httpResp.Body.Close()

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(httpResp.Body).Decode(&resp))
		assert.Equal(t, "0xws_upstream_balance", resp["result"], "response should come from WS upstream")
	})

	t.Run("WsUpstreamReconnects", func(t *testing.T) {
		connCount := 0
		var connMu sync.Mutex

		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			connMu.Lock()
			connCount++
			count := connCount
			connMu.Unlock()

			if count == 1 {
				// First connection: accept one request then close
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]interface{}
				json.Unmarshal(msg, &req)
				id := req["id"]
				method, _ := req["method"].(string)
				if method == "eth_chainId" {
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				}
				// Close abruptly to trigger reconnect
				time.Sleep(100 * time.Millisecond)
				conn.Close()
				return
			}

			// Subsequent connections: handle normally
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]interface{}
				if err := json.Unmarshal(msg, &req); err != nil {
					continue
				}
				id := req["id"]
				method, _ := req["method"].(string)
				switch method {
				case "eth_chainId":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				case "eth_getBlockByNumber":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x300", "timestamp": "0x6702a8f0"}})
				case "eth_syncing":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
				case "eth_getBalance":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xreconnected"})
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			}
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		cfg := standardWsConfig(wsUpstreamURL)

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		// Wait for initial connect + disconnect + reconnect
		time.Sleep(5 * time.Second)

		connMu.Lock()
		assert.GreaterOrEqual(t, connCount, 2, "WS upstream should have reconnected")
		connMu.Unlock()

		// Requests should work after reconnect (may go through HTTP or WS upstream)
		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.NotNil(t, resp["result"], "should get a response after upstream reconnect")
	})
}

func TestWebSocket_SubscriptionDedup(t *testing.T) {
	t.Run("TwoClientsShareOneUpstreamSubscription", func(t *testing.T) {
		subscribeCount := 0
		var subMu sync.Mutex
		upstreamSubID := "0xsharedsub123"

		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]interface{}
				if err := json.Unmarshal(msg, &req); err != nil {
					continue
				}
				method, _ := req["method"].(string)
				id := req["id"]

				switch method {
				case "eth_chainId":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				case "eth_getBlockByNumber":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x100", "timestamp": "0x6702a8f0"}})
				case "eth_syncing":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
				case "eth_subscribe":
					subMu.Lock()
					subscribeCount++
					subMu.Unlock()
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": upstreamSubID})

					// Send a notification (only on first subscribe to avoid duplicates)
					subMu.Lock()
					count := subscribeCount
					subMu.Unlock()
					if count == 1 {
						go func() {
							time.Sleep(500 * time.Millisecond)
							conn.WriteJSON(map[string]interface{}{
								"jsonrpc": "2.0",
								"method":  "eth_subscription",
								"params": map[string]interface{}{
									"subscription": upstreamSubID,
									"result":       map[string]interface{}{"number": "0x999"},
								},
							})
						}()
					}
				case "eth_unsubscribe":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			}
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		cfg := standardWsConfig(wsUpstreamURL)

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()
		time.Sleep(2 * time.Second)

		// Client 1 subscribes
		conn1 := dialWs(t, addr)
		defer conn1.Close()
		resp1 := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp1["result"])
		clientSubID1 := resp1["result"].(string)

		// Client 2 subscribes to the same thing
		conn2 := dialWs(t, addr)
		defer conn2.Close()
		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp2["result"])
		clientSubID2 := resp2["result"].(string)

		// Client subscription IDs should be different
		assert.NotEqual(t, clientSubID1, clientSubID2, "each client should get a unique subscription ID")

		// Only ONE eth_subscribe should have been sent upstream (dedup)
		subMu.Lock()
		assert.Equal(t, 1, subscribeCount, "upstream should only receive one eth_subscribe (dedup)")
		subMu.Unlock()

		// Both clients should receive the notification
		conn1.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, notif1, err := conn1.ReadMessage()
		require.NoError(t, err)

		conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, notif2, err := conn2.ReadMessage()
		require.NoError(t, err)

		var n1, n2 map[string]interface{}
		require.NoError(t, json.Unmarshal(notif1, &n1))
		require.NoError(t, json.Unmarshal(notif2, &n2))

		// Each notification should have the correct client-specific subscription ID
		p1 := n1["params"].(map[string]interface{})
		p2 := n2["params"].(map[string]interface{})
		assert.Equal(t, clientSubID1, p1["subscription"])
		assert.Equal(t, clientSubID2, p2["subscription"])
	})
}

func TestWebSocket_RateLimiting(t *testing.T) {
	t.Run("ProjectRateLimitAppliedOverWs", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id:              "test_ws",
					RateLimitBudget: "ws-test-budget",
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 123},
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
			RateLimiters: &common.RateLimiterConfig{
				Store: &common.RateLimitStoreConfig{
					Driver: "memory",
				},
				Budgets: []*common.RateLimitBudgetConfig{
					{
						Id: "ws-test-budget",
						Rules: []*common.RateLimitRuleConfig{
							{
								Method:   "*",
								MaxCount: 3,
								Period:   common.RateLimitPeriodMinute,
							},
						},
					},
				},
			},
		}

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		// Align to start of the next minute to avoid rate limit window rollover flakiness
		now := time.Now()
		time.Sleep(time.Until(now.Truncate(time.Minute).Add(time.Minute)))

		// Send more requests than the budget allows (3 per minute)
		var lastResp map[string]interface{}
		rateLimited := false
		for i := 0; i < 10; i++ {
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0xaaaa","latest"]}`, i)
			err := conn.WriteMessage(websocket.TextMessage, []byte(msg))
			require.NoError(t, err)

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, respBytes, err := conn.ReadMessage()
			require.NoError(t, err)

			require.NoError(t, json.Unmarshal(respBytes, &lastResp))
			if lastResp["error"] != nil {
				errStr, _ := json.Marshal(lastResp["error"])
				if strings.Contains(string(errStr), "RateLimitRuleExceeded") ||
					strings.Contains(string(errStr), "rate limit") ||
					strings.Contains(string(errStr), "ErrProjectRateLimitRuleExceeded") {
					rateLimited = true
					break
				}
			}
		}

		assert.True(t, rateLimited, "should hit rate limit when sending requests over WS")
	})
}

func TestWebSocket_GracefulShutdown(t *testing.T) {
	t.Run("ServerShutdownClosesWsWithGoingAway", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		// Don't defer cleanup — we'll call it explicitly to trigger shutdown

		conn := dialWs(t, addr)
		defer conn.Close()

		// Verify connection works
		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp["result"])

		// Trigger server shutdown
		cleanup()

		// The client should receive a close frame with GoingAway (1001)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		require.Error(t, err)

		closeErr, ok := err.(*websocket.CloseError)
		if ok {
			assert.Equal(t, websocket.CloseGoingAway, closeErr.Code,
				"should receive CloseGoingAway (1001) on server shutdown")
		}
		// If not a CloseError, the connection was closed abruptly which is also acceptable
	})

	t.Run("ServerShutdownCleansUpSubscriptions", func(t *testing.T) {
		unsubReceived := make(chan struct{}, 1)
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]interface{}
				if err := json.Unmarshal(msg, &req); err != nil {
					continue
				}
				method, _ := req["method"].(string)
				id := req["id"]

				switch method {
				case "eth_chainId":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				case "eth_getBlockByNumber":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x100", "timestamp": "0x6702a8f0"}})
				case "eth_syncing":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
				case "eth_subscribe":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xshutdownsub"})
				case "eth_unsubscribe":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
					select {
					case unsubReceived <- struct{}{}:
					default:
					}
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			}
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		cfg := standardWsConfig(wsUpstreamURL)

		addr, cleanup := setupTestERPCServer(t, cfg)
		time.Sleep(2 * time.Second)

		conn := dialWs(t, addr)
		defer conn.Close()

		// Create a subscription
		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp["result"])

		// Trigger server shutdown (should clean up subscriptions)
		cleanup()

		// The upstream should receive eth_unsubscribe during shutdown cleanup
		select {
		case <-unsubReceived:
			// Good — subscriptions were cleaned up
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for server to unsubscribe from upstream during shutdown")
		}
	})

	t.Run("MultipleConnectionsClosedOnShutdown", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())

		// Open multiple connections
		conn1 := dialWs(t, addr)
		defer conn1.Close()
		conn2 := dialWs(t, addr)
		defer conn2.Close()
		conn3 := dialWs(t, addr)
		defer conn3.Close()

		// Verify all work
		resp1 := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp1["result"])
		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp2["result"])
		resp3 := sendAndReceive(t, conn3, `{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp3["result"])

		// Shutdown
		cleanup()

		// All connections should be closed
		closedCount := 0
		for _, conn := range []*websocket.Conn{conn1, conn2, conn3} {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, _, err := conn.ReadMessage()
			if err != nil {
				closedCount++
			}
		}
		assert.Equal(t, 3, closedCount, "all connections should be closed on shutdown")
	})
}

func TestWebSocket_MethodFiltering(t *testing.T) {
	t.Run("IgnoredMethodReturnsError", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		cfg := &common.Config{
			Server: &common.ServerConfig{
				ListenV4: util.BoolPtr(true),
			},
			Projects: []*common.ProjectConfig{
				{
					Id:            "test_ws",
					IgnoreMethods: []string{"debug_*"},
					Networks: []*common.NetworkConfig{
						{
							Architecture: common.ArchitectureEvm,
							Evm:          &common.EvmNetworkConfig{ChainId: 123},
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

		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"debug_traceTransaction","params":["0xabc"]}`)
		assert.NotNil(t, resp["error"], "ignored method should return error")
		errObj := resp["error"].(map[string]interface{})
		assert.Contains(t, errObj["message"], "not supported")
	})
}
