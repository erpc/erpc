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

//
// --- Test helpers ---
//

func init() {
	util.ConfigureTestLogger()
}

// mockWsUpstream creates a test HTTP server that upgrades to WebSocket
// and delegates all message handling to the provided callback.
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

// standardWsConfig returns a config with both an HTTP upstream (gock) and a WS upstream.
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

// setupGock sets up standard gock mocks (eth_getBalance) and EVM state poller stubs.
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

// dialWs connects to the eRPC WebSocket endpoint for the test project.
func dialWs(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	wsURL := fmt.Sprintf("ws://%s/test_ws/evm/123", addr)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err, "WebSocket dial should succeed")
	return conn
}

// sendAndReceive sends a JSON-RPC request string and reads the JSON response.
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

// setupTestERPCServer boots an eRPC instance with the given config, returns
// the listen address and a cleanup function that shuts everything down.
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

// standardMockWsHandler handles the common set of state poller methods
// (eth_chainId, eth_getBlockByNumber, eth_syncing) that the upstream
// must respond to before the WS client is considered ready.
func standardMockWsHandler(conn *websocket.Conn, customHandler func(method string, id interface{}, req map[string]interface{})) {
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
		default:
			if customHandler != nil {
				customHandler(method, id, req)
			} else {
				conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
			}
		}
	}
}

//
// --- Tests: basic JSON-RPC over WebSocket ---
//

func TestWebSocket_BasicRPC(t *testing.T) {
	// Verifies a single JSON-RPC request/response over a WebSocket connection
	t.Run("SingleRequestOverWebSocket", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x1234567890abcdef1234567890abcdef12345678","latest"]}`)
		assert.Equal(t, "2.0", resp["jsonrpc"])
		assert.Equal(t, float64(1), resp["id"])
		assert.Equal(t, "0xabc123", resp["result"])
	})

	// Verifies sequential requests on the same connection work correctly
	t.Run("MultipleRequestsOnSameConnection", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

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

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp1 := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp1["result"])

		resp2 := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":2,"method":"eth_chainId","params":[]}`)
		assert.Equal(t, "0x7b", resp2["result"])
	})

	// Verifies many concurrent writes/reads on a single connection
	t.Run("ConcurrentRequestsOnSameConnection", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		const numRequests = 10
		var writeMu sync.Mutex

		for i := 0; i < numRequests; i++ {
			writeMu.Lock()
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0xaaaa","latest"]}`, i)
			err := conn.WriteMessage(websocket.TextMessage, []byte(msg))
			writeMu.Unlock()
			require.NoError(t, err)
		}

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

	// Verifies JSON array batch requests work over WebSocket
	t.Run("BatchRequestOverWebSocket", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

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

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		batch := `[{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]},{"jsonrpc":"2.0","id":2,"method":"eth_chainId","params":[]}]`
		err := conn.WriteMessage(websocket.TextMessage, []byte(batch))
		require.NoError(t, err)

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, respMsg, err := conn.ReadMessage()
		require.NoError(t, err)

		var responses []map[string]interface{}
		err = json.Unmarshal(respMsg, &responses)
		require.NoError(t, err, "Response should be a JSON array")
		assert.Equal(t, 2, len(responses), "Batch response should contain 2 items")
	})

	// Verifies the server handles client disconnect and allows reconnection
	t.Run("WebSocketClientDisconnect", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Contains(t, resp["result"], "0xabc123")

		err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		assert.NoError(t, err)
		conn.Close()

		time.Sleep(200 * time.Millisecond)
		conn2 := dialWs(t, addr)
		defer conn2.Close()

		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Contains(t, resp2["result"], "0xabc123")
	})

	// Verifies HTTP and WebSocket work simultaneously on the same server
	t.Run("HTTPStillWorksAlongsideWebSocket", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		wsResp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", wsResp["result"])

		httpURL := fmt.Sprintf("http://%s/test_ws/evm/123", addr)
		cleanClient := &http.Client{Transport: &http.Transport{}}
		httpResp, err := cleanClient.Post(httpURL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`))
		require.NoError(t, err)
		defer httpResp.Body.Close()

		assert.Equal(t, http.StatusOK, httpResp.StatusCode)
	})
}

//
// --- Tests: error handling ---
//

func TestWebSocket_ErrorHandling(t *testing.T) {
	// Verifies invalid JSON returns a parse error response
	t.Run("InvalidJSON", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		err := conn.WriteMessage(websocket.TextMessage, []byte(`{not valid json`))
		require.NoError(t, err)

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := conn.ReadMessage()
		require.NoError(t, err)

		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(msg, &resp))
		assert.NotNil(t, resp["error"], "should return error for invalid JSON")
	})

	// Verifies an empty message body returns an error response
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

	// Verifies upstream JSON-RPC errors are forwarded to the client
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

//
// --- Tests: multiple independent connections ---
//

func TestWebSocket_MultipleConnections(t *testing.T) {
	// Verifies multiple simultaneous connections each get correct responses
	t.Run("IndependentConnections", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn1 := dialWs(t, addr)
		defer conn1.Close()
		conn2 := dialWs(t, addr)
		defer conn2.Close()
		conn3 := dialWs(t, addr)
		defer conn3.Close()

		resp1 := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		resp3 := sendAndReceive(t, conn3, `{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)

		assert.Equal(t, float64(1), resp1["id"])
		assert.Equal(t, float64(2), resp2["id"])
		assert.Equal(t, float64(3), resp3["id"])
		assert.Equal(t, "0xabc123", resp1["result"])
		assert.Equal(t, "0xabc123", resp2["result"])
		assert.Equal(t, "0xabc123", resp3["result"])
	})

	// Verifies closing one connection does not affect others
	t.Run("OneDisconnectDoesNotAffectOthers", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn1 := dialWs(t, addr)
		conn2 := dialWs(t, addr)
		defer conn2.Close()

		resp1 := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp1["result"])

		conn1.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn1.Close()
		time.Sleep(100 * time.Millisecond)

		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp2["result"])
	})
}

//
// --- Tests: subscriptions (eth_subscribe / eth_unsubscribe) ---
//

func TestWebSocket_Subscriptions(t *testing.T) {
	// Verifies the full subscribe -> receive notification -> unsubscribe lifecycle
	t.Run("SubscribeReceiveUnsubscribe", func(t *testing.T) {
		notifCh := make(chan struct{}, 1)
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_subscribe":
					subId := "0xdeadbeef12345678"
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": subId})

					go func() {
						time.Sleep(200 * time.Millisecond)
						conn.WriteJSON(map[string]interface{}{
							"jsonrpc": "2.0",
							"method":  "eth_subscription",
							"params": map[string]interface{}{
								"subscription": subId,
								"result":       map[string]interface{}{"number": "0x101", "hash": "0xaaa"},
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
			})
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
		defer cleanup()

		time.Sleep(2 * time.Second)

		conn := dialWs(t, addr)
		defer conn.Close()

		// Subscribe
		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		assert.NotNil(t, resp["result"], "should return subscription ID")
		assert.Nil(t, resp["error"], "should not have error")
		clientSubId, ok := resp["result"].(string)
		require.True(t, ok, "subscription ID should be a string")
		assert.True(t, strings.HasPrefix(clientSubId, "0x"), "subscription ID should start with 0x")

		// Wait for notification from upstream
		select {
		case <-notifCh:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for upstream to send notification")
		}

		// Read the notification delivered to the client
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, notifMsg, err := conn.ReadMessage()
		require.NoError(t, err, "should receive notification")

		var notif map[string]interface{}
		require.NoError(t, json.Unmarshal(notifMsg, &notif))
		assert.Equal(t, "eth_subscription", notif["method"])
		params, ok := notif["params"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, clientSubId, params["subscription"], "notification should use client subscription ID")

		// Unsubscribe
		unsubResp := sendAndReceive(t, conn, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"eth_unsubscribe","params":["%s"]}`, clientSubId))
		assert.Equal(t, true, unsubResp["result"])
	})

	// Verifies eth_subscribe returns an error when no WS upstream is configured
	t.Run("SubscribeWithNoWsUpstreamFails", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		assert.NotNil(t, resp["error"], "should return error when no WS upstream available")
	})

	// Verifies eth_unsubscribe with a nonexistent ID returns an error
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

	// Verifies the server sends eth_unsubscribe to the upstream when a client disconnects
	t.Run("SubscriptionCleanupOnDisconnect", func(t *testing.T) {
		unsubReceived := make(chan struct{}, 1)
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
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
			})
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
		defer cleanup()

		time.Sleep(2 * time.Second)

		conn := dialWs(t, addr)

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp["result"])

		// Disconnect without unsubscribing
		conn.Close()

		select {
		case <-unsubReceived:
			// Server cleaned up the upstream subscription
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for server to unsubscribe from upstream on client disconnect")
		}
	})
}

//
// --- Tests: WebSocket upstream client ---
//

func TestWebSocket_UpstreamClient(t *testing.T) {
	// Verifies regular RPC requests can be routed through a WS upstream
	t.Run("RPCThroughWsUpstream", func(t *testing.T) {
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_getBalance":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xws_upstream_balance"})
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			})
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")

		// Config with ONLY the WS upstream to ensure requests go through WS
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

	// Verifies the WS upstream client automatically reconnects after disconnect
	t.Run("WsUpstreamReconnects", func(t *testing.T) {
		connCount := 0
		var connMu sync.Mutex

		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			connMu.Lock()
			connCount++
			count := connCount
			connMu.Unlock()

			if count == 1 {
				// First connection: accept one request then close abruptly
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
				time.Sleep(100 * time.Millisecond)
				conn.Close()
				return
			}

			// Subsequent connections: handle normally
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_getBalance":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xreconnected"})
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			})
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
		defer cleanup()

		time.Sleep(5 * time.Second)

		connMu.Lock()
		assert.GreaterOrEqual(t, connCount, 2, "WS upstream should have reconnected")
		connMu.Unlock()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.NotNil(t, resp["result"], "should get a response after upstream reconnect")
	})
}

//
// --- Tests: subscription recovery after upstream reconnect ---
//

func TestWebSocket_SubscriptionRecovery(t *testing.T) {
	// Verifies that when the upstream WS connection drops, eRPC closes the
	// client connection with CloseGoingAway (1001) so the client can reconnect
	// and re-subscribe cleanly instead of holding a zombie subscription.
	t.Run("ClientDisconnectedOnUpstreamDrop", func(t *testing.T) {
		closeUpstream := make(chan struct{})

		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				var req map[string]interface{}
				json.Unmarshal(msg, &req)
				method, _ := req["method"].(string)
				id := req["id"]

				switch method {
				case "eth_chainId":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				case "eth_getBlockByNumber":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x1"}})
				case "eth_syncing":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
				case "eth_subscribe":
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xsub123"})
					// Wait for signal then kill the connection
					go func() {
						<-closeUpstream
						conn.Close()
					}()
				default:
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			}
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
		defer cleanup()

		time.Sleep(2 * time.Second)

		conn := dialWs(t, addr)
		defer conn.Close()

		// Subscribe successfully
		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp["result"], "should get a subscription ID")

		// Kill the upstream WS connection
		close(closeUpstream)

		// Client should receive a close frame with GoingAway (1001)
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, _, err := conn.ReadMessage()
		require.Error(t, err, "client should be disconnected")
		closeErr, ok := err.(*websocket.CloseError)
		if ok {
			assert.Equal(t, websocket.CloseGoingAway, closeErr.Code, "close code should be 1001 GoingAway")
			t.Logf("client received close frame: code=%d reason=%q", closeErr.Code, closeErr.Text)
		} else {
			t.Logf("client disconnected with error: %v", err)
		}
	})

	// Verifies that eth_subscribe returns an error when the upstream WS
	// connection isn't established yet (instead of creating a zombie subscription).
	t.Run("SubscribeFailsWhenUpstreamDisconnected", func(t *testing.T) {
		// Create a mock that immediately closes the WS connection,
		// so eRPC's upstream WS stays disconnected.
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			conn.Close()
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
		defer cleanup()

		time.Sleep(2 * time.Second)

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		assert.NotNil(t, resp["error"], "should return error when upstream WS is not connected")
		t.Logf("got expected error: %v", resp["error"])
	})
}

//
// --- Tests: subscription deduplication ---
//

func TestWebSocket_SubscriptionDedup(t *testing.T) {
	// Verifies two clients subscribing to the same event share one upstream subscription
	t.Run("TwoClientsShareOneUpstreamSubscription", func(t *testing.T) {
		subscribeCount := 0
		var subMu sync.Mutex
		upstreamSubId := "0xsharedsub123"

		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_subscribe":
					subMu.Lock()
					subscribeCount++
					count := subscribeCount
					subMu.Unlock()
					conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": upstreamSubId})

					// Send a notification only on first subscribe to avoid duplicates
					if count == 1 {
						go func() {
							time.Sleep(500 * time.Millisecond)
							conn.WriteJSON(map[string]interface{}{
								"jsonrpc": "2.0",
								"method":  "eth_subscription",
								"params": map[string]interface{}{
									"subscription": upstreamSubId,
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
			})
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
		defer cleanup()
		time.Sleep(2 * time.Second)

		// Client 1 subscribes
		conn1 := dialWs(t, addr)
		defer conn1.Close()
		resp1 := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp1["result"])
		clientSubId1 := resp1["result"].(string)

		// Client 2 subscribes to the same event
		conn2 := dialWs(t, addr)
		defer conn2.Close()
		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp2["result"])
		clientSubId2 := resp2["result"].(string)

		assert.NotEqual(t, clientSubId1, clientSubId2, "each client should get a unique subscription ID")

		// Only ONE eth_subscribe should have been sent to the upstream (dedup)
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

		p1 := n1["params"].(map[string]interface{})
		p2 := n2["params"].(map[string]interface{})
		assert.Equal(t, clientSubId1, p1["subscription"])
		assert.Equal(t, clientSubId2, p2["subscription"])
	})
}

//
// --- Tests: rate limiting ---
//

func TestWebSocket_RateLimiting(t *testing.T) {
	// Verifies project-level rate limits are enforced on WebSocket requests
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

		// Align to start of the next minute to avoid rate limit window rollover
		now := time.Now()
		time.Sleep(time.Until(now.Truncate(time.Minute).Add(time.Minute)))

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

//
// --- Tests: graceful shutdown ---
//

func TestWebSocket_GracefulShutdown(t *testing.T) {
	// Verifies clients receive a GoingAway close frame on server shutdown
	t.Run("ServerShutdownClosesWsWithGoingAway", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp["result"])

		cleanup()

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		require.Error(t, err)

		closeErr, ok := err.(*websocket.CloseError)
		if ok {
			assert.Equal(t, websocket.CloseGoingAway, closeErr.Code,
				"should receive CloseGoingAway (1001) on server shutdown")
		}
	})

	// Verifies the server unsubscribes from upstreams during shutdown
	t.Run("ServerShutdownCleansUpSubscriptions", func(t *testing.T) {
		unsubReceived := make(chan struct{}, 1)
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
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
			})
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
		addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
		time.Sleep(2 * time.Second)

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp["result"])

		cleanup()

		select {
		case <-unsubReceived:
			// Subscriptions were cleaned up
		case <-time.After(10 * time.Second):
			t.Fatal("timeout waiting for server to unsubscribe from upstream during shutdown")
		}
	})

	// Verifies all connections are closed when the server shuts down
	t.Run("MultipleConnectionsClosedOnShutdown", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())

		conn1 := dialWs(t, addr)
		defer conn1.Close()
		conn2 := dialWs(t, addr)
		defer conn2.Close()
		conn3 := dialWs(t, addr)
		defer conn3.Close()

		resp1 := sendAndReceive(t, conn1, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp1["result"])
		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp2["result"])
		resp3 := sendAndReceive(t, conn3, `{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "0xabc123", resp3["result"])

		cleanup()

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

//
// --- Tests: method filtering ---
//

func TestWebSocket_MethodFiltering(t *testing.T) {
	// Verifies ignored methods return an unsupported error over WebSocket
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
