package erpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/h2non/gock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

// mockWriteMus serialises writes per mock-upstream connection: gorilla
// supports one concurrent writer, and several mock handlers push
// notifications from a goroutine while the read loop answers requests.
var mockWriteMus sync.Map // *websocket.Conn -> *sync.Mutex

// mockWriteJSON is the only way mock upstream handlers write to their
// connection.
func mockWriteJSON(conn *websocket.Conn, v interface{}) error {
	muRaw, _ := mockWriteMus.LoadOrStore(conn, &sync.Mutex{})
	mu := muRaw.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return conn.WriteJSON(v)
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
		defer mockWriteMus.Delete(c)
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

// multiWsConfig returns a config with one HTTP upstream (gock) and N WS upstreams.
func multiWsConfig(wsURLs ...string) *common.Config {
	upstreams := []*common.UpstreamConfig{
		{
			Id:       "http-upstream",
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc1.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		},
	}
	for i, wsURL := range wsURLs {
		upstreams = append(upstreams, &common.UpstreamConfig{
			Id:       fmt.Sprintf("ws-upstream-%d", i),
			Type:     common.UpstreamTypeEvm,
			Endpoint: wsURL,
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		})
	}
	return &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_ws",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
					},
				},
				Upstreams: upstreams,
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

// readCostFrame reads one cost-frame message and splits it into its header
// block and JSON body.
func readCostFrame(t *testing.T, conn *websocket.Conn) (textproto.MIMEHeader, []byte) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(msg)))
	h, err := r.ReadMIMEHeader()
	require.NoError(t, err, "message should start with a header block: %q", msg)
	body, err := io.ReadAll(r.R)
	require.NoError(t, err)
	return h, body
}

// setupTestERPCServer boots an eRPC instance with the given config, returns
// the listen address and a cleanup function that shuts everything down.
func setupTestERPCServer(t *testing.T, cfg *common.Config) (string, context.CancelFunc) {
	t.Helper()
	httpServer, addr, cancel := startTestERPCServer(t, cfg)
	cleanup := func() {
		_ = httpServer.Shutdown(&log.Logger)
		cancel()
	}
	return addr, cleanup
}

// startTestERPCServer is setupTestERPCServer for tests that need the server
// itself or to cancel the app context on their own.
func startTestERPCServer(t *testing.T, cfg *common.Config) (*HttpServer, string, context.CancelFunc) {
	t.Helper()

	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())

	// These servers exist to serve WebSocket; production configs opt in.
	if cfg.Server.WebSocket == nil {
		cfg.Server.WebSocket = &common.WebSocketServerConfig{}
	}
	if cfg.Server.WebSocket.Enabled == nil {
		cfg.Server.WebSocket.Enabled = util.BoolPtr(true)
	}
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

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, nil, cfg)
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

	return httpServer, fmt.Sprintf("127.0.0.1:%d", port), cancel
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
			mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
		case "eth_getBlockByNumber":
			mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{"number": "0x100", "timestamp": "0x6702a8f0"}})
		case "eth_syncing":
			mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": false})
		default:
			if customHandler != nil {
				customHandler(method, id, req)
			} else {
				mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
			}
		}
	}
}

// mockHead is block n of the chain the mock WS upstreams push: a header that
// passes intake parsing.
func mockHead(n int64) map[string]interface{} {
	return map[string]interface{}{
		"number":     fmt.Sprintf("0x%x", n),
		"hash":       fmt.Sprintf("0x%064x", n),
		"parentHash": fmt.Sprintf("0x%064x", n-1),
		"timestamp":  fmt.Sprintf("0x%x", 1_700_000_000+n),
	}
}

// pushMockHeads pushes mockHead(from), from+1, … on subId, one per receive
// from trigger, or every 200ms with a nil trigger, until the connection
// closes; it signals sent, if set, after each push.
func pushMockHeads(conn *websocket.Conn, subId string, from int64, trigger <-chan struct{}, sent chan<- struct{}) {
	for n := from; ; n++ {
		if trigger == nil {
			time.Sleep(200 * time.Millisecond)
		} else if _, ok := <-trigger; !ok {
			return
		}
		if mockWriteJSON(conn, map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_subscription",
			"params":  map[string]interface{}{"subscription": subId, "result": mockHead(n)},
		}) != nil {
			return
		}
		if sent != nil {
			select {
			case sent <- struct{}{}:
			default:
			}
		}
	}
}

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

	// An unknown method (-32601 from every upstream) answers after one sweep
	// of the upstreams, over WebSocket and HTTP alike: no network retry or
	// hedge re-asks them.
	t.Run("UnknownMethodFailsFast", func(t *testing.T) {
		util.ResetGock()
		util.SetupMocksForEvmStatePoller()
		defer util.ResetGock()

		var calls [2]atomic.Int32
		for i, host := range []string{"http://rpc1.localhost", "http://rpc2.localhost"} {
			gock.New(host).
				Post("/").
				Persist().
				Filter(func(request *http.Request) bool {
					return strings.Contains(util.SafeReadBody(request), "eth_doesNotExist")
				}).
				Reply(200).
				// gock runs filters for every host's mocks; Map runs once per reply.
				Map(func(res *http.Response) *http.Response {
					calls[i].Add(1)
					return res
				}).
				Delay(50 * time.Millisecond).
				JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"error": map[string]interface{}{
						"code":    -32601,
						"message": "the method eth_doesNotExist does not exist/is not available",
					},
				})
		}

		// Production's network policy for "*": retries and a hedge.
		cfg := httpOnlyConfig()
		cfg.Projects[0].Upstreams = append(cfg.Projects[0].Upstreams, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Endpoint: "http://rpc2.localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		})
		cfg.Projects[0].Networks[0].Failsafe = []*common.FailsafeConfig{{
			MatchMethod: "*",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 6},
			Hedge: &common.HedgePolicyConfig{
				MaxCount: 1,
				Delay:    &common.AdaptiveDuration{Base: common.Duration(500 * time.Millisecond)},
			},
		}}
		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		const req = `{"jsonrpc":"2.0","id":1,"method":"eth_doesNotExist","params":[]}`
		assertOneSweep := func(t *testing.T, elapsed time.Duration, errObj interface{}) {
			t.Helper()
			e, ok := errObj.(map[string]interface{})
			require.True(t, ok, "expected an error, got %v", errObj)
			assert.Equal(t, float64(-32601), e["code"])
			assert.Less(t, elapsed, 500*time.Millisecond, "unknown method must not be retried or hedged")
			assert.Equal(t, int32(1), calls[0].Load(), "rpc1 asked once")
			assert.Equal(t, int32(1), calls[1].Load(), "rpc2 asked once")
			calls[0].Store(0)
			calls[1].Store(0)
		}

		conn := dialWs(t, addr)
		defer conn.Close()
		// The first request bootstraps the network; keep that out of the timing.
		warm := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":0,"method":"eth_getBlockByNumber","params":["latest",false]}`)
		require.NotNil(t, warm["result"], "warm-up failed: %v", warm)
		start := time.Now()
		resp := sendAndReceive(t, conn, req)
		assertOneSweep(t, time.Since(start), resp["error"])

		start = time.Now()
		cleanClient := &http.Client{Transport: &http.Transport{}}
		httpResp, err := cleanClient.Post(fmt.Sprintf("http://%s/test_ws/evm/123", addr), "application/json", strings.NewReader(req))
		require.NoError(t, err)
		defer httpResp.Body.Close()
		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(httpResp.Body).Decode(&body))
		assertOneSweep(t, time.Since(start), body["error"])
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
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": subId})

					// newHeads is subscribed at network init, before any
					// client, so the chain keeps moving.
					go pushMockHeads(conn, subId, 0x101, nil, notifCh)
				case "eth_unsubscribe":
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
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

	// With server.costHeaders on, a client that opts in with
	// X-ERPC-Cost-Frames: 1 gets every message prefixed with its cost header
	// block; a client that does not gets plain JSON.
	t.Run("CostFrames", func(t *testing.T) {
		notifCh := make(chan struct{}, 1)
		// One head, pushed once the client subscribed, so the frames that
		// follow are the replies.
		pushOne := make(chan struct{}, 1)
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_subscribe":
					subId := "0xdeadbeef12345678"
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": subId})
					go pushMockHeads(conn, subId, 0x101, pushOne, notifCh)
				case "eth_getBalance":
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xabc123"})
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			})
		})
		defer mockUpstream.Close()

		setupGock()
		defer util.ResetGock()

		cfg := standardWsConfig("ws" + strings.TrimPrefix(mockUpstream.URL, "http"))
		cfg.Server.CostHeaders = util.BoolPtr(true)
		addr, cleanup := setupTestERPCServer(t, cfg)
		defer cleanup()

		time.Sleep(2 * time.Second)

		conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", addr),
			http.Header{"X-ERPC-Cost-Frames": {"1"}})
		require.NoError(t, err)
		defer conn.Close()

		send := func(req string) (textproto.MIMEHeader, []byte) {
			t.Helper()
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(req)))
			return readCostFrame(t, conn)
		}
		assertNetwork := func(h textproto.MIMEHeader) {
			t.Helper()
			assert.Equal(t, "evm:123", h.Get("X-ERPC-Network-Id"))
			assert.Equal(t, "evm:123", h.Get("X-ERPC-Network-Alias"))
		}

		// newHeads: the subscribe reply, then each notification as 1 billable call.
		h, body := send(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		assert.Equal(t, "1", h.Get("X-ERPC-Calls"))
		assert.Equal(t, "eth_subscribe", h.Get("X-ERPC-Methods"))
		pushOne <- struct{}{}
		var subResp map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &subResp))
		clientSubId, ok := subResp["result"].(string)
		require.True(t, ok, "subscription ID should be a string: %s", body)

		select {
		case <-notifCh:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for upstream to send notification")
		}
		h, body = readCostFrame(t, conn)
		assert.Equal(t, textproto.MIMEHeader{
			"X-Erpc-Calls":         {"1"},
			"X-Erpc-Billable":      {"1"},
			"X-Erpc-Methods":       {"eth_subscription"},
			"X-Erpc-Subscription":  {"newHeads"},
			"X-Erpc-Network-Id":    {"evm:123"},
			"X-Erpc-Network-Alias": {"evm:123"},
		}, h)
		var notif map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &notif))
		assert.Equal(t, "eth_subscription", notif["method"])
		assert.Equal(t, clientSubId, notif["params"].(map[string]interface{})["subscription"])

		// A single call.
		h, body = send(`{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, "1", h.Get("X-ERPC-Calls"))
		assert.Equal(t, "1", h.Get("X-ERPC-Billable"))
		assert.Equal(t, "eth_getBalance", h.Get("X-ERPC-Methods"))
		assertNetwork(h)
		var single map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &single))
		assert.Equal(t, "0xabc123", single["result"])

		// A batch: one block for the whole batch.
		h, body = send(`[{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xaaaa","latest"]},{"jsonrpc":"2.0","id":4,"method":"eth_getBalance","params":["0xbbbb","latest"]}]`)
		assert.Equal(t, "2", h.Get("X-ERPC-Calls"))
		assert.Equal(t, "2", h.Get("X-ERPC-Billable"))
		assert.Equal(t, "eth_getBalance", h.Get("X-ERPC-Methods"))
		assertNetwork(h)
		var batch []map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &batch))
		assert.Len(t, batch, 2)

		// A batch that is not JSON routed nothing: empty block.
		h, body = send(`[{"jsonrpc":"2.0",`)
		assert.Empty(t, h)
		var parseErr map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &parseErr))
		assert.NotNil(t, parseErr["error"])

		// A single unparsable request is a call HTTP would count, unbilled.
		h, body = send(`{not valid json`)
		assert.Equal(t, "1", h.Get("X-ERPC-Calls"))
		assert.Equal(t, "0", h.Get("X-ERPC-Billable"))
		var invalid map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &invalid))
		assert.NotNil(t, invalid["error"])

		// An id-less call gets no reply but still ran: a metering-only
		// message, the block with an empty body.
		h, body = send(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xcccc","latest"]}`)
		assert.Equal(t, "1", h.Get("X-ERPC-Calls"))
		assert.Equal(t, "1", h.Get("X-ERPC-Billable"))
		assert.Equal(t, "eth_getBalance", h.Get("X-ERPC-Methods"))
		assert.Empty(t, body)

		// Without the header: plain JSON, and nothing for the id-less call,
		// so the next message is the reply to id 5.
		plain := dialWs(t, addr)
		defer plain.Close()
		require.NoError(t, plain.WriteMessage(websocket.TextMessage,
			[]byte(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["0xcccc","latest"]}`)))
		resp := sendAndReceive(t, plain, `{"jsonrpc":"2.0","id":5,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
		assert.Equal(t, float64(5), resp["id"])
		assert.Equal(t, "0xabc123", resp["result"])
		plain.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, extra, err := plain.ReadMessage()
		assert.Error(t, err, "no message for the id-less call, got %q", extra)
	})

	// An HTTP-only network whose upstream is not a subscription source has
	// nothing to feed newHeads.
	t.Run("SubscribeWithoutSubscriptionSourceFails", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.NotNil(t, resp["error"], "should return error when no upstream is a subscription source")
		assert.Contains(t, fmt.Sprint(resp["error"]), "needs a subscription source")
	})

	// Verifies eth_unsubscribe with a nonexistent ID fails as geth's does
	t.Run("UnsubscribeUnknownIDFails", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()

		addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
		defer cleanup()

		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_unsubscribe","params":["0xnonexistent"]}`)
		errObj, ok := resp["error"].(map[string]interface{})
		require.True(t, ok, "should return error for unknown subscription ID: %v", resp)
		assert.Equal(t, float64(-32000), errObj["code"])
		assert.Equal(t, "subscription not found", errObj["message"])
	})

	// Verifies client disconnect cleanly removes the client from the fan-out
	// group without disturbing the persistent upstream newHeads subscription.
	t.Run("SubscriptionCleanupOnDisconnect", func(t *testing.T) {
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_subscribe":
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xsub123"})
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
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

		// Disconnect without unsubscribing — newHeads upstream sub stays active
		conn.Close()

		// Allow cleanup to complete
		time.Sleep(500 * time.Millisecond)

		// Verify a new client can still subscribe (upstream sub still alive)
		conn2 := dialWs(t, addr)
		defer conn2.Close()

		resp2 := sendAndReceive(t, conn2, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		assert.NotNil(t, resp2["result"], "new client should be able to subscribe after previous client disconnected")
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
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xws_upstream_balance"})
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
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
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x7b"})
				}
				time.Sleep(100 * time.Millisecond)
				conn.Close()
				return
			}

			// Subsequent connections: handle normally
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_getBalance":
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xreconnected"})
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
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
	// Verifies that eth_subscribe returns an error when the only WS upstream
	// never initialises (it closes every connection before answering).
	t.Run("SubscribeFailsWhenWsUpstreamNeverInitialises", func(t *testing.T) {
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
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": upstreamSubId})

					// Only the first subscribe pushes, so a second upstream
					// subscription would show as missing heads.
					if count == 1 {
						go pushMockHeads(conn, upstreamSubId, 0x101, nil, nil)
					}
				case "eth_unsubscribe":
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": true})
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
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

		// No need to align to the minute: at most one window rollover fits in
		// this loop, which still allows only 2*MaxCount of the 10 requests.
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
		var closeErr *websocket.CloseError
		require.ErrorAs(t, err, &closeErr)
		assert.Equal(t, websocket.CloseGoingAway, closeErr.Code,
			"should receive CloseGoingAway (1001) on server shutdown")
	})

	// Verifies the server unsubscribes from upstreams during shutdown
	// Verifies server shutdown closes the upstream WS connection (which implicitly
	// terminates all upstream subscriptions) and closes all client connections.
	t.Run("ServerShutdownCleansUpSubscriptions", func(t *testing.T) {
		mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_subscribe":
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xshutdownsub"})
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
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

		// After shutdown, the client connection should be closed
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		var closeErr *websocket.CloseError
		require.ErrorAs(t, err, &closeErr)
		assert.Equal(t, websocket.CloseGoingAway, closeErr.Code)
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

		for _, conn := range []*websocket.Conn{conn1, conn2, conn3} {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, _, err := conn.ReadMessage()
			var closeErr *websocket.CloseError
			require.ErrorAs(t, err, &closeErr)
			assert.Equal(t, websocket.CloseGoingAway, closeErr.Code)
		}
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

// TestWebSocket_FailedSubscribeKeepsConnectionOpen: a failed subscribe must
// not close the connection and its other subscriptions.
func TestWebSocket_FailedSubscribeKeepsConnectionOpen(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
	defer cleanup()

	conn := dialWs(t, addr)
	defer conn.Close()

	// Subscribe MUST fail (no WS upstream configured) but the connection
	// must remain open for subsequent requests.
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.NotNil(t, resp["error"], "subscribe should fail")

	// Connection should still work — send a normal RPC after failed subscribe.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp2 := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":2,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
	assert.Equal(t, "0xabc123", resp2["result"], "connection should remain open after subscribe failure")
}

// TestWebSocket_EveryUpstreamHeadReachesItsPoller: a head delivered by
// several upstreams updates every one of their state pollers, though the
// client receives it once.
func TestWebSocket_EveryUpstreamHeadReachesItsPoller(t *testing.T) {
	// Two WS upstreams that both deliver the same block.
	upSubId := "0xupsub"

	deliverNotification := func(conn *websocket.Conn) {
		mockWriteJSON(conn, map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_subscription",
			"params": map[string]interface{}{
				"subscription": upSubId,
				"result":       mockHead(0x123),
			},
		})
	}

	// Both upstreams push block 0x123 once the client subscribed: newHeads is
	// subscribed at network init, before any client.
	release := make(chan struct{})
	makeMock := func() *httptest.Server {
		return mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_subscribe":
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": upSubId})
					go func() {
						<-release
						deliverNotification(conn)
					}()
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			})
		})
	}
	mock1 := makeMock()
	defer mock1.Close()
	mock2 := makeMock()
	defer mock2.Close()

	setupGock()
	defer util.ResetGock()

	url1 := "ws" + strings.TrimPrefix(mock1.URL, "http")
	url2 := "ws" + strings.TrimPrefix(mock2.URL, "http")
	srv, addr, cancel := startTestERPCServer(t, multiWsConfig(url1, url2))
	defer func() { _ = srv.Shutdown(&log.Logger); cancel() }()
	time.Sleep(2 * time.Second)

	conn := dialWs(t, addr)
	defer conn.Close()
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.NotNil(t, resp["result"], "subscribe should succeed with multiple WS upstreams")
	close(release)

	// Both upstreams delivered block 0x123, so both pollers must know it.
	prj, err := srv.erpc.GetProject("test_ws")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		seen := 0
		for _, up := range prj.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:123") {
			if strings.HasPrefix(up.Id(), "ws-upstream-") && up.EvmStatePoller().LatestBlock() >= 0x123 {
				seen++
			}
		}
		return seen == 2
	}, 3*time.Second, 20*time.Millisecond, "every delivering upstream's poller must see the block")

	// Client should receive exactly one fan-out (deduped across upstreams).
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err, "should receive deduped notification")
	var notif map[string]interface{}
	require.NoError(t, json.Unmarshal(msg, &notif))
	assert.Equal(t, "eth_subscription", notif["method"])

	// A second notification should NOT arrive — dedup must drop the duplicate
	// from the second upstream.
	conn.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	assert.Error(t, err, "duplicate block from second upstream must be deduped")
}

// TestWebSocket_LogsSubscribeOnAllUpstreamsAndDedupe: a logs subscription
// is made on every WS upstream and its notifications are deduplicated by
// blockHash + txHash + logIndex.
func TestWebSocket_LogsSubscribeOnAllUpstreamsAndDedupe(t *testing.T) {
	subscribeCount := int64(0)
	logSubId := "0xlogsub"

	deliverLog := func(conn *websocket.Conn, blockHash, txHash, logIndex string) {
		mockWriteJSON(conn, map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_subscription",
			"params": map[string]interface{}{
				"subscription": logSubId,
				"result": map[string]interface{}{
					"address":          "0xabc",
					"blockHash":        blockHash,
					"transactionHash":  txHash,
					"logIndex":         logIndex,
					"removed":          false,
					"topics":           []string{},
					"data":             "0x",
					"blockNumber":      "0x123",
					"transactionIndex": "0x0",
				},
			},
		})
	}

	makeMock := func() *httptest.Server {
		return mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
				switch method {
				case "eth_subscribe":
					params, _ := req["params"].([]interface{})
					if len(params) > 0 && params[0] == "logs" {
						atomic.AddInt64(&subscribeCount, 1)
						mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": logSubId})
						go func() {
							time.Sleep(300 * time.Millisecond)
							// Both upstreams deliver the same log.
							deliverLog(conn, "0xblock1", "0xtx1", "0x0")
						}()
					} else {
						mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xothersub"})
					}
				default:
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
				}
			})
		})
	}
	mock1 := makeMock()
	defer mock1.Close()
	mock2 := makeMock()
	defer mock2.Close()

	setupGock()
	defer util.ResetGock()

	url1 := "ws" + strings.TrimPrefix(mock1.URL, "http")
	url2 := "ws" + strings.TrimPrefix(mock2.URL, "http")
	addr, cleanup := setupTestERPCServer(t, multiWsConfig(url1, url2))
	defer cleanup()
	time.Sleep(2 * time.Second)

	counter := func(c *prometheus.CounterVec, labels ...string) float64 {
		return testutil.ToFloat64(c.WithLabelValues(labels...))
	}
	ingress := func() float64 {
		return counter(telemetry.MetricUpstreamSubscriptionEventsTotal, "test_ws", "evm:123", "ws-upstream-0", "logs") +
			counter(telemetry.MetricUpstreamSubscriptionEventsTotal, "test_ws", "evm:123", "ws-upstream-1", "logs")
	}
	ingressBefore := ingress()
	dedupBefore := counter(telemetry.MetricSubscriptionDedupDroppedTotal, "test_ws", "evm:123", "logs")
	sentBefore := counter(telemetry.MetricWebsocketNotificationsSentTotal, "test_ws", "evm:123", "logs")
	bytesBefore := counter(telemetry.MetricWebsocketNotificationBytesTotal, "test_ws", "evm:123", "logs")

	conn := dialWs(t, addr)
	defer conn.Close()
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["logs",{}]}`)
	require.NotNil(t, resp["result"])

	// Wait for both upstreams to be subscribed.
	require.Eventually(t, func() bool {
		return atomic.LoadInt64(&subscribeCount) >= 2
	}, 5*time.Second, 100*time.Millisecond, "both WS upstreams should be subscribed for logs")

	// Client should receive exactly one log notification (deduped).
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err, "should receive deduped log notification")
	var notif map[string]interface{}
	require.NoError(t, json.Unmarshal(msg, &notif))
	assert.Equal(t, "eth_subscription", notif["method"])

	// Duplicate from second upstream must be deduped.
	conn.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	assert.Error(t, err, "duplicate log from second upstream must be deduped by blockHash+txHash+logIndex")

	// Each upstream logs subscribe sends the same log once: all but the
	// first event in are dedup drops, and one notification goes out.
	in := float64(atomic.LoadInt64(&subscribeCount))
	assert.Equal(t, in, ingress()-ingressBefore)
	assert.Equal(t, in-1, counter(telemetry.MetricSubscriptionDedupDroppedTotal, "test_ws", "evm:123", "logs")-dedupBefore)
	assert.Equal(t, 1.0, counter(telemetry.MetricWebsocketNotificationsSentTotal, "test_ws", "evm:123", "logs")-sentBefore)
	assert.Equal(t, float64(len(msg)), counter(telemetry.MetricWebsocketNotificationBytesTotal, "test_ws", "evm:123", "logs")-bytesBefore)
	assert.GreaterOrEqual(t, testutil.ToFloat64(telemetry.MetricSubscriptionDedupEntries.WithLabelValues("test_ws", "evm:123")), 1.0)
}

// TestWebSocket_ReconnectAfterUnsubscribe: an upstream reconnect after the
// last logs subscriber left must not touch the released filter.
func TestWebSocket_ReconnectAfterUnsubscribe(t *testing.T) {
	logSubId := "0xunsublog"
	mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
		standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
			if method == "eth_subscribe" {
				params, _ := req["params"].([]interface{})
				if len(params) > 0 && params[0] == "logs" {
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": logSubId})
					return
				}
			}
			mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xothersub"})
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

	// Subscribe to logs (creates filter sub group).
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["logs",{}]}`)
	require.NotNil(t, resp["result"])
	clientSubId := resp["result"].(string)

	// Unsubscribe (tears down the filter group).
	unsubResp := sendAndReceive(t, conn, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"eth_unsubscribe","params":["%s"]}`, clientSubId))
	assert.Equal(t, true, unsubResp["result"])

	// Force the upstream WS to drop so it reconnects after the release.
	mockUpstream.CloseClientConnections()

	// Sleep long enough for reconnect + callback to fire.
	time.Sleep(3 * time.Second)

	// eRPC should still be alive — make a normal RPC call.
	resp2 := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":3,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
	assert.NotNil(t, resp2, "eRPC should not have crashed from reconnect-after-unsubscribe")
}

// TestWebSocket_InternalRequestIdsAreUnique: internal eth_subscribe requests
// use unique wire ids, so their responses cannot be misrouted.
func TestWebSocket_InternalRequestIdsAreUnique(t *testing.T) {
	subscribeIds := make([]int64, 0)
	var idMu sync.Mutex

	mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
		standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
			if method == "eth_subscribe" {
				idMu.Lock()
				if f, ok := id.(float64); ok {
					subscribeIds = append(subscribeIds, int64(f))
				}
				idMu.Unlock()
				mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0xabc"})
				return
			}
			mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
		})
	})
	defer mockUpstream.Close()

	setupGock()
	defer util.ResetGock()

	wsUpstreamURL := "ws" + strings.TrimPrefix(mockUpstream.URL, "http")
	addr, cleanup := setupTestERPCServer(t, standardWsConfig(wsUpstreamURL))
	defer cleanup()
	time.Sleep(2 * time.Second)

	// Trigger an internal upstream subscribe via a client subscribe. The
	// upstream eth_subscribe is dispatched asynchronously, so poll for it.
	conn := dialWs(t, addr)
	defer conn.Close()
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.NotNil(t, resp["result"])

	require.Eventually(t, func() bool {
		idMu.Lock()
		defer idMu.Unlock()
		return len(subscribeIds) >= 1
	}, 3*time.Second, 20*time.Millisecond, "should have observed at least one internal eth_subscribe")

	idMu.Lock()
	defer idMu.Unlock()

	// All IDs unique.
	seen := make(map[int64]bool)
	for _, id := range subscribeIds {
		assert.False(t, seen[id], "internal id %d duplicated", id)
		seen[id] = true
	}
}

//
// --- Tests: subscription sources ---
//

// mockChain is one simulated chain that mock vendors serve. A gated chain
// moves only after a polled vendor served its head, so a poll source sees
// every block whatever the scheduler does.
type mockChain struct {
	mu       sync.Mutex
	head     int64
	served   int64
	gated    bool
	paused   bool
	watchers []chan int64
}

func newMockChain(head int64, gated bool) *mockChain {
	return &mockChain{head: head, served: head - 1, gated: gated}
}

func (c *mockChain) Head() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head
}

// latest answers a latest poll; markServed opens the gate.
func (c *mockChain) latest(markServed bool) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if markServed {
		c.served = c.head
	}
	return c.head
}

func (c *mockChain) setPaused(p bool) {
	c.mu.Lock()
	c.paused = p
	c.mu.Unlock()
}

func (c *mockChain) setGated(g bool) {
	c.mu.Lock()
	c.gated = g
	c.served = c.head - 1
	c.mu.Unlock()
}

// advance adds a block and tells every watcher.
func (c *mockChain) advance() int64 {
	c.mu.Lock()
	c.head++
	n := c.head
	watchers := slices.Clone(c.watchers)
	c.mu.Unlock()
	for _, w := range watchers {
		select {
		case w <- n:
		default:
		}
	}
	return n
}

func (c *mockChain) watch() <-chan int64 {
	ch := make(chan int64, 256)
	c.mu.Lock()
	c.watchers = append(c.watchers, ch)
	c.mu.Unlock()
	return ch
}

// run adds a block every interval until ctx ends, unless paused or gated.
func (c *mockChain) run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		hold := c.paused || (c.gated && c.served < c.head)
		c.mu.Unlock()
		if !hold {
			c.advance()
		}
	}
}

// block is block n as a vendor serves it, transactions included.
func (c *mockChain) block(n int64) map[string]interface{} {
	h := mockHead(n)
	h["transactions"] = []interface{}{fmt.Sprintf("0x%064x", n+1_000_000)}
	h["miner"] = "0x0000000000000000000000000000000000000001"
	return h
}

// answerBlock answers eth_getBlockByNumber from the chain; latest polls are
// counted in polls.
func (c *mockChain) answerBlock(params []interface{}, markServed bool, polls *atomic.Int64) interface{} {
	tag, _ := params[0].(string)
	head := c.Head()
	var n int64
	switch tag {
	case "latest":
		polls.Add(1)
		n = c.latest(markServed)
	case "finalized", "safe":
		n = head - 5
	default:
		v, err := common.HexToInt64(tag)
		if err != nil || v > head {
			return nil
		}
		n = v
	}
	return c.block(n)
}

// mockHttpVendor is an HTTP JSON-RPC vendor of chain.
type mockHttpVendor struct {
	chain       *mockChain
	latestPolls atomic.Int64
}

func newMockHttpVendor(t *testing.T, chain *mockChain) (*mockHttpVendor, *httptest.Server) {
	t.Helper()
	v := &mockHttpVendor{chain: chain}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		answer := func(req map[string]interface{}) map[string]interface{} {
			params, _ := req["params"].([]interface{})
			var result interface{} = "0x1"
			switch req["method"] {
			case "eth_chainId":
				result = "0x7b"
			case "eth_syncing":
				result = false
			case "eth_blockNumber":
				result = fmt.Sprintf("0x%x", chain.Head())
			case "eth_getBlockByNumber":
				result = chain.answerBlock(params, true, &v.latestPolls)
			}
			return map[string]interface{}{"jsonrpc": "2.0", "id": req["id"], "result": result}
		}
		w.Header().Set("Content-Type", "application/json")
		if bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
			var reqs []map[string]interface{}
			_ = json.Unmarshal(body, &reqs)
			out := make([]map[string]interface{}, 0, len(reqs))
			for _, req := range reqs {
				out = append(out, answer(req))
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		var req map[string]interface{}
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(answer(req))
	}))
	t.Cleanup(srv.Close)
	return v, srv
}

// mockWsVendor is a WebSocket vendor of chain that pushes each new head on
// its newHeads subscription.
type mockWsVendor struct {
	chain *mockChain
	// refuseHeads answers newHeads subscribes as unsupported.
	refuseHeads bool
	// staleRPC answers latest with the first block, so only pushes move
	// the upstream's head.
	staleRPC    bool
	pushing     atomic.Bool
	subscribes  atomic.Int32
	latestPolls atomic.Int64
}

func newMockWsVendor(t *testing.T, chain *mockChain, configure func(v *mockWsVendor)) (*mockWsVendor, string) {
	t.Helper()
	v := &mockWsVendor{chain: chain}
	v.pushing.Store(true)
	if configure != nil {
		configure(v)
	}
	first := chain.Head()
	srv := mockWsUpstream(t, func(conn *websocket.Conn) {
		done := make(chan struct{})
		defer close(done)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]interface{}
			if json.Unmarshal(msg, &req) != nil {
				continue
			}
			id := req["id"]
			params, _ := req["params"].([]interface{})
			reply := map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"}
			switch req["method"] {
			case "eth_chainId":
				reply["result"] = "0x7b"
			case "eth_syncing":
				reply["result"] = false
			case "eth_blockNumber":
				reply["result"] = fmt.Sprintf("0x%x", first)
			case "eth_getBlockByNumber":
				if tag, _ := params[0].(string); v.staleRPC && tag == "latest" {
					reply["result"] = chain.block(first)
				} else {
					reply["result"] = chain.answerBlock(params, v.refuseHeads, &v.latestPolls)
				}
			case "eth_unsubscribe":
				reply["result"] = true
			case "eth_subscribe":
				if params[0] != "newHeads" {
					reply["result"] = "0xother"
					break
				}
				v.subscribes.Add(1)
				if v.refuseHeads {
					delete(reply, "result")
					reply["error"] = map[string]interface{}{"code": -32601, "message": "the method eth_subscribe does not exist/is not available"}
					break
				}
				subID := fmt.Sprintf("0xheads%d", v.subscribes.Load())
				reply["result"] = subID
				heads := chain.watch()
				go func() {
					for {
						select {
						case <-done:
							return
						case n := <-heads:
							if v.pushing.Load() {
								mockWriteJSON(conn, map[string]interface{}{
									"jsonrpc": "2.0",
									"method":  "eth_subscription",
									"params":  map[string]interface{}{"subscription": subID, "result": chain.block(n)},
								})
							}
						}
					}
				}()
			}
			mockWriteJSON(conn, reply)
		}
	})
	t.Cleanup(srv.Close)
	return v, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// sourcesConfig is one project, test_ws, with one evm network of chain 123.
func sourcesConfig(network func(nw *common.NetworkConfig), ups ...*common.UpstreamConfig) *common.Config {
	nw := &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}
	if network != nil {
		network(nw)
	}
	return &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{{
			Id:        "test_ws",
			Networks:  []*common.NetworkConfig{nw},
			Upstreams: ups,
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

// sourceUpstream polls at 100ms while it is a poll source.
func sourceUpstream(id, endpoint string, source *bool) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeEvm,
		Endpoint: endpoint,
		Evm: &common.EvmUpstreamConfig{
			ChainId:             123,
			SubscriptionSource:  source,
			StatePollerDebounce: common.Duration(100 * time.Millisecond),
		},
	}
}

func subscribeNewHeads(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.Nil(t, resp["error"], "subscribe failed: %v", resp["error"])
	id, ok := resp["result"].(string)
	require.True(t, ok, "no subscription id: %v", resp)
	return id
}

// nextNewHead reads the next newHeads notification: its number and payload.
func nextNewHead(t *testing.T, conn *websocket.Conn, timeout time.Duration) (int64, map[string]interface{}) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err, "no newHeads notification within %s", timeout)
	var notif struct {
		Method string `json:"method"`
		Params struct {
			Result map[string]interface{} `json:"result"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal(msg, &notif), "%s", msg)
	require.Equal(t, "eth_subscription", notif.Method, "%s", msg)
	numHex, _ := notif.Params.Result["number"].(string)
	n, err := common.HexToInt64(numHex)
	require.NoError(t, err, "%s", msg)
	return n, notif.Params.Result
}

// requireContiguous reads count heads and requires each to follow the one
// before; it returns the last.
func requireContiguous(t *testing.T, conn *websocket.Conn, count int, prev int64) int64 {
	t.Helper()
	for range count {
		n, _ := nextNewHead(t, conn, 10*time.Second)
		if prev != 0 {
			require.Equal(t, prev+1, n, "heads must arrive once each, in order")
		}
		prev = n
	}
	return prev
}

// pollsSettle waits until vendor polls stop for a whole window.
func pollsSettle(t *testing.T, polls *atomic.Int64, window time.Duration, msg string) {
	t.Helper()
	require.Eventually(t, func() bool {
		before := polls.Load()
		time.Sleep(window)
		return polls.Load() == before
	}, 10*time.Second, 10*time.Millisecond, msg)
}

// An HTTP-only network whose upstream is a subscription source feeds
// newHeads by polling: heads arrive in order, projected without
// transactions.
func TestWebSocket_HttpSourcePolledHeads(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	chain := newMockChain(0x100, true)
	_, srv := newMockHttpVendor(t, chain)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 150*time.Millisecond)

	addr, cleanup := setupTestERPCServer(t, sourcesConfig(nil, sourceUpstream("rpc-a", srv.URL, util.BoolPtr(true))))
	defer cleanup()
	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)

	var prev int64
	for range 5 {
		n, head := nextNewHead(t, conn, 10*time.Second)
		if prev != 0 {
			require.Equal(t, prev+1, n, "polled heads arrive in order")
		}
		prev = n
		assert.Equal(t, fmt.Sprintf("0x%064x", n), head["hash"])
		assert.NotContains(t, head, "transactions", "newHeads is projected")
	}
}

// A network with a wss source and an HTTP poll source: each head reaches
// the client once.
func TestWebSocket_PushAndPollSourcesDeliverEachHeadOnce(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	chain := newMockChain(0x100, false)
	httpVendor, srv := newMockHttpVendor(t, chain)
	_, wsURL := newMockWsVendor(t, chain, nil)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 200*time.Millisecond)

	addr, cleanup := setupTestERPCServer(t, sourcesConfig(nil,
		sourceUpstream("rpc-http", srv.URL, util.BoolPtr(true)),
		sourceUpstream("rpc-ws", wsURL, nil),
	))
	defer cleanup()
	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)

	polledBefore := httpVendor.latestPolls.Load()
	requireContiguous(t, conn, 8, 0)
	assert.Greater(t, httpVendor.latestPolls.Load(), polledBefore+2, "the HTTP source polls while a client wants heads")
}

// With failover on, a fallback-tier poll source polls only while no primary
// source has been live for primaryLeadAfter; the client's heads stay
// contiguous across both hand-overs.
func TestWebSocket_FallbackPollsUntilPrimaryLeads(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	prevLead := primaryLeadAfter.Load()
	primaryLeadAfter.Store(1500 * time.Millisecond)
	defer primaryLeadAfter.Store(prevLead)

	chain := newMockChain(0x100, false)
	fallback, srv := newMockHttpVendor(t, chain)
	primary, wsURL := newMockWsVendor(t, chain, nil)

	fallbackCfg := sourceUpstream("rpc-fallback", srv.URL, util.BoolPtr(true))
	fallbackCfg.Tags = []string{common.TagTierFallback}
	primaryCfg := sourceUpstream("rpc-primary", wsURL, nil)
	// The primary's own polls let the network see blocks it stops pushing.
	primaryCfg.Evm.StatePollerInterval = common.Duration(300 * time.Millisecond)
	cfg := sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Failover = &common.FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)}
		// Every upstream eligible; the identity function itself would select the rich default.
		nw.SelectionPolicy = &common.SelectionPolicyConfig{EvalFunc: `(upstreams, ctx) => upstreams.filter(u => true)`}
		nw.Evm.ServedTip = &common.EvmServedTipConfig{Quorum: 1}
	}, primaryCfg, fallbackCfg)
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()
	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)

	// The chain starts after the subscribe, so no primary leads yet.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 200*time.Millisecond)

	last := requireContiguous(t, conn, 3, 0)
	assert.Greater(t, fallback.latestPolls.Load(), int64(2), "the fallback polls while no primary leads")

	pollsSettle(t, &fallback.latestPolls, 700*time.Millisecond, "the fallback stops polling once the primary leads")
	last = requireContiguous(t, conn, 3, last)

	// The primary goes silent once its pushes are out; the next block is
	// seen only over RPC.
	chain.setPaused(true)
	time.Sleep(300 * time.Millisecond)
	primary.pushing.Store(false)
	if h := chain.Head(); h > last {
		last = requireContiguous(t, conn, int(h-last), last)
	}
	stalled := fallback.latestPolls.Load()
	chain.advance()
	last = requireContiguous(t, conn, 1, last)
	assert.Greater(t, fallback.latestPolls.Load(), stalled, "the fallback polls again once the primary is not live")

	chain.setGated(true)
	chain.setPaused(false)
	requireContiguous(t, conn, 4, last)
}

// Polls stop sourceIdleStop after the last client leaves; a later client
// gets only heads from after it subscribed.
func TestWebSocket_PollsStopWhenIdle(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	prevIdle := sourceIdleStop.Load()
	sourceIdleStop.Store(500 * time.Millisecond)
	defer sourceIdleStop.Store(prevIdle)

	chain := newMockChain(0x100, true)
	vendor, srv := newMockHttpVendor(t, chain)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 150*time.Millisecond)

	addr, cleanup := setupTestERPCServer(t, sourcesConfig(nil, sourceUpstream("rpc-a", srv.URL, util.BoolPtr(true))))
	defer cleanup()
	first := dialWs(t, addr)
	subscribeNewHeads(t, first)
	seen := requireContiguous(t, first, 3, 0)
	require.NoError(t, first.Close())

	pollsSettle(t, &vendor.latestPolls, 700*time.Millisecond, "polls stop after the idle delay")
	idleAt := chain.Head()

	second := dialWs(t, addr)
	defer second.Close()
	subscribeNewHeads(t, second)
	n, _ := nextNewHead(t, second, 10*time.Second)
	assert.Greater(t, n, seen, "the new client starts after the first one's heads")
	assert.GreaterOrEqual(t, n, idleAt, "the new client gets no head from before it subscribed")
	requireContiguous(t, second, 2, n)
}

// With the WebSocket server off, each project's network subscribes newHeads
// on its wss upstream at init, and HTTP eth_blockNumber follows the pushes.
func TestWebSocket_SourcesSubscribeAtInitWithoutWebSocketServer(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	chain := newMockChain(0x100, false)
	stale := func(v *mockWsVendor) { v.staleRPC = true }
	vendorA, urlA := newMockWsVendor(t, chain, stale)
	vendorB, urlB := newMockWsVendor(t, chain, stale)

	cfg := sourcesConfig(nil, sourceUpstream("ws-a", urlA, nil))
	second := *cfg.Projects[0]
	second.Id = "test_ws2"
	second.Networks = []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}}
	second.Upstreams = []*common.UpstreamConfig{sourceUpstream("ws-b", urlB, nil)}
	cfg.Projects = append(cfg.Projects, &second)
	cfg.Server.WebSocket = &common.WebSocketServerConfig{Enabled: util.BoolPtr(false)}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	require.Eventually(t, func() bool { return vendorA.subscribes.Load() > 0 && vendorB.subscribes.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "each project subscribes newHeads on its own upstream without any client")

	for range 3 {
		chain.advance()
	}
	want := fmt.Sprintf("0x%x", chain.Head())
	for _, project := range []string{"test_ws", "test_ws2"} {
		require.Eventually(t, func() bool {
			resp, err := http.Post(fmt.Sprintf("http://%s/%s/evm/123", addr, project), "application/json",
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			var out map[string]interface{}
			if json.NewDecoder(resp.Body).Decode(&out) != nil {
				return false
			}
			return out["result"] == want
		}, 10*time.Second, 50*time.Millisecond, "%s: eth_blockNumber follows the pushed heads", project)
	}
}

// A wss upstream that refuses newHeads as unsupported feeds them by poll,
// and is asked again once per re-ask interval.
func TestWebSocket_RefusedNewHeadsPolled(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	prevReask := wsupstream.HeadsReaskInterval.Load()
	wsupstream.HeadsReaskInterval.Store(time.Second)
	defer wsupstream.HeadsReaskInterval.Store(prevReask)

	chain := newMockChain(0x100, true)
	vendor, wsURL := newMockWsVendor(t, chain, func(v *mockWsVendor) { v.refuseHeads = true })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 150*time.Millisecond)

	addr, cleanup := setupTestERPCServer(t, sourcesConfig(nil, sourceUpstream("rpc-ws", wsURL, nil)))
	defer cleanup()
	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)

	requireContiguous(t, conn, 3, 0)
	require.Eventually(t, func() bool { return vendor.subscribes.Load() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"newHeads is asked again after the re-ask interval")
	asked := vendor.subscribes.Load()
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, asked, vendor.subscribes.Load(), "asked once per interval, not retried with backoff")
}
