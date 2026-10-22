package erpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/h2non/gock"
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

// mockWriteMus holds one write mutex per mock-upstream connection for as
// long as the connection exists: gorilla supports one concurrent writer,
// and mock handlers and pushes may still write after the read loop ended,
// so dropping the mutex earlier would let two writers in at once.
var mockWriteMus sync.Map // weak.Pointer[websocket.Conn] -> *sync.Mutex

// mockWriteJSON is the only way mock upstream handlers write to their
// connection.
func mockWriteJSON(conn *websocket.Conn, v interface{}) error {
	key := weak.Make(conn)
	muRaw, loaded := mockWriteMus.LoadOrStore(key, &sync.Mutex{})
	if !loaded {
		runtime.AddCleanup(conn, func(k weak.Pointer[websocket.Conn]) { mockWriteMus.Delete(k) }, key)
	}
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

	// As init.go does: a configured EVM cache is built before the instance.
	var evmCache *evm.EvmJsonRpcCache
	if cfg.Database != nil && cfg.Database.EvmJsonRpcCache != nil {
		evmCache, err = evm.NewEvmJsonRpcCache(ctx, &logger, cfg.Database.EvmJsonRpcCache)
		require.NoError(t, err)
	}

	erpcInstance, err := NewERPC(ctx, &logger, ssr, evmCache, nil, cfg)
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
		errObj, ok := resp["error"].(map[string]interface{})
		require.True(t, ok, "should return error when no upstream is a subscription source: %v", resp)
		assert.EqualValues(t, common.JsonRpcErrorUnsupportedException, errObj["code"], "ErrNoSubscriptionSource maps to -32601")
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

	// Two wss sources push the same pending transactions; the client gets
	// each once, in order.
	t.Run("TwoSourcesPushOnePendingTransactionOnce", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()
		chain := newMockChain(0x100, false)
		a, urlA := newMockWsVendor(t, chain, nil)
		b, urlB := newMockWsVendor(t, chain, nil)
		addr, cleanup := setupTestERPCServer(t, sourcesConfig(nil, sourceUpstream("rpc-a", urlA, nil), sourceUpstream("rpc-b", urlB, nil)))
		defer cleanup()
		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newPendingTransactions"]}`)
		subID, ok := resp["result"].(string)
		require.True(t, ok, "subscribe failed: %v", resp)
		require.Eventually(t, func() bool { return a.pendingLive() == 1 && b.pendingLive() == 1 },
			5*time.Second, 10*time.Millisecond, "each source subscribes the topic once")

		tx1, tx2 := "0x"+strings.Repeat("a1", 32), "0x"+strings.Repeat("b2", 32)
		for _, tx := range []string{tx1, tx2} {
			a.pushTx(tx)
			b.pushTx(tx)
		}
		assert.Equal(t, tx1, nextPendingTx(t, conn, subID, 5*time.Second))
		assert.Equal(t, tx2, nextPendingTx(t, conn, subID, 5*time.Second))
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
		_, msg, err := conn.ReadMessage()
		require.Error(t, err, "each transaction reaches the client once, got %s", msg)
	})

	// Every source refuses a new variant as bad params: the client gets the
	// upstream's -32602, and so does a second client that subscribes the
	// same variant while the first waits for the answers.
	t.Run("EverySourceRefusesNewVariant", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()
		chain := newMockChain(0x100, false)
		answer := make(chan struct{})
		refuse := func(v *mockWsVendor) {
			v.refusePending = func(int32) map[string]interface{} {
				<-answer
				return map[string]interface{}{"code": -32602, "message": "invalid argument 1: unknown field"}
			}
		}
		a, urlA := newMockWsVendor(t, chain, refuse)
		b, urlB := newMockWsVendor(t, chain, refuse)
		addr, cleanup := setupTestERPCServer(t, sourcesConfig(nil, sourceUpstream("rpc-a", urlA, nil), sourceUpstream("rpc-b", urlB, nil)))
		defer cleanup()
		const subscribe = `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newPendingTransactions",{"bogus":true}]}`
		conn1, conn2 := dialWs(t, addr), dialWs(t, addr)
		defer conn1.Close()
		defer conn2.Close()

		require.NoError(t, conn1.WriteMessage(websocket.TextMessage, []byte(subscribe)))
		require.Eventually(t, func() bool { return a.pendingSubscribes.Load() == 1 && b.pendingSubscribes.Load() == 1 },
			5*time.Second, 10*time.Millisecond, "each source is asked for the variant")
		require.NoError(t, conn2.WriteMessage(websocket.TextMessage, []byte(subscribe)))
		time.Sleep(300 * time.Millisecond)
		close(answer)

		for i, conn := range []*websocket.Conn{conn1, conn2} {
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(15*time.Second)))
			_, msg, err := conn.ReadMessage()
			require.NoError(t, err)
			var resp map[string]interface{}
			require.NoError(t, json.Unmarshal(msg, &resp))
			errObj, ok := resp["error"].(map[string]interface{})
			require.True(t, ok, "client %d's subscribe must fail: %s", i+1, msg)
			assert.EqualValues(t, -32602, errObj["code"], "client %d: %v", i+1, errObj)
		}
		assert.EqualValues(t, 1, a.pendingSubscribes.Load(), "the joiner shares the first open")
		assert.EqualValues(t, 1, b.pendingSubscribes.Load(), "the joiner shares the first open")
	})

	// Every source answers a new variant with a capacity error: the
	// subscribe opens, and the sources' retry delivers.
	t.Run("CapacityAnswersOpenAndRetryDelivers", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()
		chain := newMockChain(0x100, false)
		busyOnce := func(v *mockWsVendor) {
			v.refusePending = func(n int32) map[string]interface{} {
				if n == 1 {
					return map[string]interface{}{"code": -32005, "message": "limit exceeded"}
				}
				return nil
			}
		}
		a, urlA := newMockWsVendor(t, chain, busyOnce)
		b, urlB := newMockWsVendor(t, chain, busyOnce)
		addr, cleanup := setupTestERPCServer(t, sourcesConfig(nil, sourceUpstream("rpc-a", urlA, nil), sourceUpstream("rpc-b", urlB, nil)))
		defer cleanup()
		conn := dialWs(t, addr)
		defer conn.Close()

		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newPendingTransactions"]}`)
		subID, ok := resp["result"].(string)
		require.True(t, ok, "capacity answers open the subscription: %v", resp)
		assert.Zero(t, a.pendingLive()+b.pendingLive(), "no source took the topic yet")
		require.Eventually(t, func() bool { return a.pendingLive() == 1 && b.pendingLive() == 1 },
			10*time.Second, 10*time.Millisecond, "each source retries until the topic is live")

		tx := "0x" + strings.Repeat("c3", 32)
		a.pushTx(tx)
		assert.Equal(t, tx, nextPendingTx(t, conn, subID, 5*time.Second))
	})
}

// nextPendingTx reads the next notification of subscription subID and
// returns its transaction hash.
func nextPendingTx(t *testing.T, conn *websocket.Conn, subID string, timeout time.Duration) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err, "no newPendingTransactions notification within %s", timeout)
	var notif struct {
		Method string `json:"method"`
		Params struct {
			Subscription string `json:"subscription"`
			Result       string `json:"result"`
		} `json:"params"`
	}
	require.NoError(t, json.Unmarshal(msg, &notif), "%s", msg)
	require.Equal(t, "eth_subscription", notif.Method, "%s", msg)
	require.Equal(t, subID, notif.Params.Subscription, "%s", msg)
	return notif.Params.Result
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

// TestWebSocket_LogsSubscribeOnAllUpstreamsAndDedupe is the no-fault
// baseline of ws logs from the chain feed: 2 filters over 12 blocks with
// transactions, one eth_getBlockReceipts per block, no upstream eth_subscribe
// logs, and after 8 matching blocks no fetch for a block no filter may
// match. A filter that does not parse fails with -32602 before any fetch;
// a network where no upstream serves receipts or eth_getLogs refuses logs
// with ErrNoSubscriptionSource.
func TestWebSocket_LogsSubscribeOnAllUpstreamsAndDedupe(t *testing.T) {
	tick := func(s *chaosSeed, n int) {
		for range n {
			s.chain.tick()
			time.Sleep(chaosBlock)
		}
	}
	receiptsAndLogsCalls := func(s *chaosSeed) (calls int64) {
		s.chain.mu.Lock()
		defer s.chain.mu.Unlock()
		for _, v := range s.vendors {
			for _, n := range v.receiptsCalls {
				calls += int64(n)
			}
			calls += v.getLogsCalls.Load() + v.logsSubscribe.Load()
		}
		return calls
	}

	t.Run("baseline", func(t *testing.T) {
		s := newChaosSeed(t, 5, true, chaosOpts{})
		s.startERPC()
		defer s.stopERPC()
		tick(s, 3)

		// No logs tap yet: the feed fetches no receipts, so any receipts,
		// eth_getLogs or logs subscribe call would come from the bad filter.
		malformed, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", s.addr), nil)
		require.NoError(t, err)
		defer malformed.Close()
		resp := sendAndReceive(t, malformed, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["logs",{"address":"0x123"}]}`)
		errObj, ok := resp["error"].(map[string]interface{})
		require.True(t, ok, "a malformed address fails: %v", resp)
		assert.EqualValues(t, common.JsonRpcErrorInvalidArgument, errObj["code"])
		tick(s, 2)
		assert.Zero(t, receiptsAndLogsCalls(s), "a filter that does not parse makes no upstream call")

		sent := telemetry.MetricWebsocketNotificationsSentTotal.WithLabelValues("test_ws", "evm:123", "logs")
		sentBytes := telemetry.MetricWebsocketNotificationBytesTotal.WithLabelValues("test_ws", "evm:123", "logs")
		sentBefore, bytesBefore := testutil.ToFloat64(sent), testutil.ToFloat64(sentBytes)
		s.dialLogs("base")
		tick(s, 3)

		s.chain.mu.Lock()
		from := s.chain.tipLocked().number + 1
		plan := []simKind{}
		for range 8 {
			plan = append(plan, simMatching)
		}
		plan = append(plan, simQuiet)
		for range 4 {
			plan = append(plan, simMatching)
		}
		s.chain.plan = plan
		s.chain.mu.Unlock()
		tick(s, len(plan)+2)

		tip := s.chain.tip()
		require.Eventually(t, func() bool {
			for _, cl := range s.openLogs() {
				cl.mu.Lock()
				d := cl.logsDiffLocked(s, from, from+uint64(len(plan))-1)
				cl.mu.Unlock()
				if d != "" {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond, "both clients hold the matching logs of every planned block")
		require.Len(t, s.openLogs(), len(simFilters))
		for i, kind := range plan {
			b := s.chain.canonAt(from + uint64(i))
			require.NotNil(t, b)
			require.Equal(t, kind, b.kind)
			want := 1
			if kind == simQuiet {
				want = 0
			}
			assert.Equal(t, want, s.receiptsCallsOf(b.hash), "receipts calls for block %d (%d of the plan)", b.number, i+1)
			for f := range simFilters {
				assert.Equal(t, kind == simMatching, len(b.matched(f)) > 0, "block %d holds logs of filter %d only when planned to match", b.number, f)
			}
		}
		for _, v := range s.vendors {
			assert.Zero(t, v.logsSubscribe.Load(), "%s gets no eth_subscribe logs", v.id)
			assert.Zero(t, v.getLogsCalls.Load(), "%s gets no eth_getLogs while receipts serve", v.id)
		}
		// The counters move after the write returns, so after the client
		// may have read it.
		received := func() (notes, size int) {
			for _, cl := range s.openLogs() {
				cl.mu.Lock()
				notes, size = notes+cl.notes, size+cl.noteBytes
				cl.mu.Unlock()
			}
			return notes, size
		}
		require.Eventually(t, func() bool {
			notes, size := received()
			return notes > 0 && float64(notes) == testutil.ToFloat64(sent)-sentBefore && float64(size) == testutil.ToFloat64(sentBytes)-bytesBefore
		}, 5*time.Second, 20*time.Millisecond, "logs notifications sent and their bytes equal what the clients received")
		t.Logf("planned blocks %d..%d, head %d", from, from+uint64(len(plan))-1, tip.number)
	})

	t.Run("no upstream serves receipts or eth_getLogs", func(t *testing.T) {
		s := newChaosSeed(t, 6, true, chaosOpts{})
		s.ignore = []string{"eth_getBlockReceipts", "eth_getLogs"}
		s.startERPC()
		defer s.stopERPC()
		tick(s, 3)
		conn, rerr := s.subscribeLogs(0)
		require.Nil(t, conn)
		require.NotNil(t, rerr, "logs subscribe is refused")
		assert.EqualValues(t, common.JsonRpcErrorUnsupportedException, rerr["code"], "ErrNoSubscriptionSource maps to -32601: %v", rerr)
		assert.Contains(t, fmt.Sprint(rerr), "subscription source", "the refusal names the missing source: %v", rerr)
		assert.Zero(t, receiptsAndLogsCalls(s), "no receipts, eth_getLogs or upstream logs subscription")
	})

	// With integrity on, eth_getLogs by blockHash stands in only while no
	// upstream serves receipts; its logs pass the bloom check and
	// logIndexContiguity.
	t.Run("integrity on, no upstream serves receipts: logs by eth_getLogs", func(t *testing.T) {
		s := newChaosSeed(t, 7, true, chaosOpts{integrity: true})
		s.ignore = []string{"eth_getBlockReceipts"}
		s.startERPC()
		defer s.stopERPC()
		tick(s, 3)
		s.dialLogs("fallback")
		tick(s, 2)
		from := s.chain.tip().number + 1
		s.chain.mu.Lock()
		s.chain.plan = []simKind{simMatching, simMatching, simQuiet, simMatching}
		s.chain.mu.Unlock()
		tick(s, 6)
		require.Eventually(t, func() bool {
			for _, cl := range s.openLogs() {
				cl.mu.Lock()
				d := cl.logsDiffLocked(s, from, from+3)
				cl.mu.Unlock()
				if d != "" {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond, "both clients hold the matching logs of every planned block")
		require.Len(t, s.openLogs(), len(simFilters))
		for _, cl := range s.openLogs() {
			cl.mu.Lock()
			assert.Empty(t, cl.problems, "logs client %s", cl.name)
			cl.mu.Unlock()
		}
		var getLogs int64
		for _, v := range s.vendors {
			getLogs += v.getLogsCalls.Load()
		}
		assert.Positive(t, getLogs, "logs come from eth_getLogs by blockHash")
	})
}

// TestWebSocket_ReconnectAfterUnsubscribe: an upstream reconnect after the
// last newPendingTransactions subscriber left must not touch the released
// filter.
func TestWebSocket_ReconnectAfterUnsubscribe(t *testing.T) {
	txSubId := "0xunsubtx"
	mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
		standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
			if method == "eth_subscribe" {
				params, _ := req["params"].([]interface{})
				if len(params) > 0 && params[0] == "newPendingTransactions" {
					mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": txSubId})
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

	// Subscribe to newPendingTransactions (creates filter sub group).
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newPendingTransactions"]}`)
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
	// branch is the reorg count of each height; 0 is the first branch.
	branch map[int64]int64
}

func newMockChain(head int64, gated bool) *mockChain {
	return &mockChain{head: head, served: head - 1, gated: gated, branch: make(map[int64]int64)}
}

// Hash is block n's hash on the canonical branch.
func (c *mockChain) Hash(n int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hashLocked(n)
}

func (c *mockChain) hashLocked(n int64) string {
	return fmt.Sprintf("0x%048x%016x", c.branch[n], n)
}

// reorg replaces the top depth blocks with a new branch, adds a block on
// it and tells every watcher; it returns the new head.
func (c *mockChain) reorg(depth int64) int64 {
	c.mu.Lock()
	for n := c.head - depth + 1; n <= c.head; n++ {
		c.branch[n]++
	}
	c.mu.Unlock()
	return c.advance()
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
	return c.jump(1)
}

// jump adds blocks at once and tells every watcher only the new head.
func (c *mockChain) jump(blocks int64) int64 {
	c.mu.Lock()
	c.head += blocks
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
	c.mu.Lock()
	h["hash"], h["parentHash"] = c.hashLocked(n), c.hashLocked(n-1)
	c.mu.Unlock()
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
// its newHeads subscription, and the transactions pushTx names on its
// newPendingTransactions subscriptions.
type mockWsVendor struct {
	chain *mockChain
	// refuseHeads answers newHeads subscribes as unsupported.
	refuseHeads bool
	// staleRPC answers latest with the first block, so only pushes move
	// the upstream's head.
	staleRPC bool
	// refusePending, if set, gives the JSON-RPC error answering the n-th
	// newPendingTransactions subscribe (from 1), nil to accept it.
	refusePending func(n int32) map[string]interface{}
	pushing       atomic.Bool
	subscribes    atomic.Int32
	latestPolls   atomic.Int64

	pendingSubscribes atomic.Int32
	pendingMu         sync.Mutex
	pendingSubs       map[string]*websocket.Conn // live newPendingTransactions subscriptions
}

// pendingLive counts the vendor's live newPendingTransactions subscriptions.
func (v *mockWsVendor) pendingLive() int {
	v.pendingMu.Lock()
	defer v.pendingMu.Unlock()
	return len(v.pendingSubs)
}

// pushTx pushes hash on every live newPendingTransactions subscription.
func (v *mockWsVendor) pushTx(hash string) {
	v.pendingMu.Lock()
	subs := maps.Clone(v.pendingSubs)
	v.pendingMu.Unlock()
	for id, conn := range subs {
		mockWriteJSON(conn, map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_subscription",
			"params":  map[string]interface{}{"subscription": id, "result": hash},
		})
	}
}

func newMockWsVendor(t *testing.T, chain *mockChain, configure func(v *mockWsVendor)) (*mockWsVendor, string) {
	t.Helper()
	v := &mockWsVendor{chain: chain, pendingSubs: make(map[string]*websocket.Conn)}
	v.pushing.Store(true)
	if configure != nil {
		configure(v)
	}
	first := chain.Head()
	srv := mockWsUpstream(t, func(conn *websocket.Conn) {
		done := make(chan struct{})
		defer close(done)
		defer func() {
			v.pendingMu.Lock()
			for id, c := range v.pendingSubs {
				if c == conn {
					delete(v.pendingSubs, id)
				}
			}
			v.pendingMu.Unlock()
		}()
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
				if id, ok := params[0].(string); ok {
					v.pendingMu.Lock()
					delete(v.pendingSubs, id)
					v.pendingMu.Unlock()
				}
				reply["result"] = true
			case "eth_subscribe":
				if params[0] == "newPendingTransactions" {
					n := v.pendingSubscribes.Add(1)
					if v.refusePending != nil {
						if e := v.refusePending(n); e != nil {
							delete(reply, "result")
							reply["error"] = e
							break
						}
					}
					subID := fmt.Sprintf("0xtx%d", n)
					v.pendingMu.Lock()
					v.pendingSubs[subID] = conn
					v.pendingMu.Unlock()
					reply["result"] = subID
					break
				}
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
// the client once. A 2-block reorg on both vendors then reaches the client
// as each head of the new branch, oldest first, and the client's chain
// ends equal to the vendors'. A jump of more than reorgWindow blocks then
// restarts the client's tap after the head, counted as lag, and heads go
// on from there. One tap is open while the client is subscribed, none
// after it leaves.
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
	tapLabels := []string{"test_ws", "evm:123", "ws", indexer.SubTypeNewHeads}
	openTaps := func() float64 { return testutil.ToFloat64(telemetry.MetricTaps.WithLabelValues(tapLabels...)) }
	lagged := telemetry.MetricTapOutOfRangeTotal.WithLabelValues(append(tapLabels, indexer.OutOfRangeLag)...)
	require.Eventually(t, func() bool { return openTaps() == 1 }, 5*time.Second, 10*time.Millisecond, "one tap per subscription")

	// The client applies each head as a geth client does: a number it
	// already has replaces that number and every number above it.
	client := make(map[int64]string)
	var first, last int64
	apply := func(n int64, head map[string]interface{}) {
		for k := range client {
			if k >= n {
				delete(client, k)
			}
		}
		client[n], _ = head["hash"].(string)
		if first == 0 {
			first = n
		}
		last = n
	}
	next := func(msg string) {
		n, head := nextNewHead(t, conn, 10*time.Second)
		if last != 0 {
			require.Equal(t, last+1, n, msg)
		}
		apply(n, head)
	}

	polledBefore := httpVendor.latestPolls.Load()
	for range 8 {
		next("heads must arrive once each, in order")
	}
	assert.Greater(t, httpVendor.latestPolls.Load(), polledBefore+2, "the HTTP source polls while a client wants heads")

	// Hold the chain past one tick, so the client reaches its head before
	// the reorg.
	chain.setPaused(true)
	time.Sleep(300 * time.Millisecond)
	for last < chain.Head() {
		next("heads must arrive once each, in order")
	}
	tip := chain.reorg(2)
	for _, want := range []int64{tip - 2, tip - 1, tip} {
		n, head := nextNewHead(t, conn, 10*time.Second)
		require.Equal(t, want, n, "after a reorg each head of the new branch arrives, oldest first")
		require.Equal(t, chain.Hash(n), head["hash"], "block %d is the new branch's", n)
		apply(n, head)
	}
	chain.setPaused(false)
	for range 3 {
		next("heads continue in order after the reorg")
	}
	for n := first; n <= last; n++ {
		require.Equal(t, chain.Hash(n), client[n], "the client's block %d is the vendors' canonical one", n)
	}

	// The sources stall, then return more than reorgWindow (default 32)
	// blocks ahead: the tracker re-anchors there and fills back down to the
	// client's cursor, so the client's next batch starts too far behind.
	lagBefore := testutil.ToFloat64(lagged)
	chain.setPaused(true)
	time.Sleep(300 * time.Millisecond)
	for last < chain.Head() {
		next("heads must arrive once each, in order")
	}
	jumped := chain.jump(40)
	require.Eventually(t, func() bool { return testutil.ToFloat64(lagged) == lagBefore+1 }, 15*time.Second, 10*time.Millisecond,
		"a client more than reorgWindow behind restarts once, as lag")
	chain.setPaused(false)
	n, head := nextNewHead(t, conn, 10*time.Second)
	require.Equal(t, jumped+1, n, "the restarted client continues after the head")
	require.Equal(t, chain.Hash(n), head["hash"])
	last = n
	for range 2 {
		next("heads continue in order after the restart")
	}
	assert.Equal(t, lagBefore+1, testutil.ToFloat64(lagged), "one lag restart")
	assert.Equal(t, 1.0, openTaps(), "the restarted tap is the subscription's one tap")

	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool { return openTaps() == 0 }, 5*time.Second, 10*time.Millisecond, "the tap closes with the client")
}

// With failover on, a fallback-tier poll source polls only while no primary
// source has been live for primaryLeadAfter; the client's heads stay
// contiguous across both hand-overs. A fallback-tier wss source subscribes
// newPendingTransactions only while it is authoritative too.
func TestWebSocket_FallbackPollsUntilPrimaryLeads(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	prevLead := primaryLeadAfter.Load()
	primaryLeadAfter.Store(1500 * time.Millisecond)
	defer primaryLeadAfter.Store(prevLead)

	chain := newMockChain(0x100, false)
	fallback, srv := newMockHttpVendor(t, chain)
	primary, wsURL := newMockWsVendor(t, chain, nil)
	// Its heads would vote while it is authoritative; only its pending
	// subscription is under test.
	wsFallback, wsFallbackURL := newMockWsVendor(t, chain, func(v *mockWsVendor) { v.pushing.Store(false) })

	fallbackCfg := sourceUpstream("rpc-fallback", srv.URL, util.BoolPtr(true))
	fallbackCfg.Tags = []string{common.TagTierFallback}
	wsFallbackCfg := sourceUpstream("rpc-fallback-ws", wsFallbackURL, nil)
	wsFallbackCfg.Tags = []string{common.TagTierFallback}
	primaryCfg := sourceUpstream("rpc-primary", wsURL, nil)
	// The primary's own polls let the network see blocks it stops pushing.
	primaryCfg.Evm.StatePollerInterval = common.Duration(300 * time.Millisecond)
	cfg := sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Failover = &common.FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)}
		// Every upstream eligible; the identity function itself would select the rich default.
		nw.SelectionPolicy = &common.SelectionPolicyConfig{EvalFunc: `(upstreams, ctx) => upstreams.filter(u => true)`}
		nw.Evm.ServedTip = &common.EvmServedTipConfig{Quorum: 1}
	}, primaryCfg, fallbackCfg, wsFallbackCfg)
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()
	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)
	pending := dialWs(t, addr)
	defer pending.Close()
	resp := sendAndReceive(t, pending, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newPendingTransactions"]}`)
	require.NotNil(t, resp["result"], "subscribe failed: %v", resp)
	pendingOn := func(primaryLive, fallbackLive int) func() bool {
		return func() bool { return primary.pendingLive() == primaryLive && wsFallback.pendingLive() == fallbackLive }
	}
	require.Eventually(t, pendingOn(1, 1), 5*time.Second, 10*time.Millisecond,
		"while no primary leads, the fallback subscribes newPendingTransactions too")

	// The chain starts after the subscribe, so no primary leads yet.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 200*time.Millisecond)

	last := requireContiguous(t, conn, 3, 0)
	assert.Greater(t, fallback.latestPolls.Load(), int64(2), "the fallback polls while no primary leads")

	pollsSettle(t, &fallback.latestPolls, 700*time.Millisecond, "the fallback stops polling once the primary leads")
	require.Eventually(t, pendingOn(1, 0), 5*time.Second, 10*time.Millisecond,
		"the fallback drops newPendingTransactions once the primary leads")
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
	require.Eventually(t, pendingOn(1, 1), 10*time.Second, 10*time.Millisecond,
		"the fallback subscribes newPendingTransactions again once the primary is not live")

	chain.setGated(true)
	chain.setPaused(false)
	requireContiguous(t, conn, 4, last)
}

// Polls stop sourceIdleStop after the last client leaves. A later client
// starts right after the pod's head where polls stopped: the idle source
// stays live (nothing newer is known), and walks link the blocks the chain
// made while nobody polled.
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

	erpcSrv, addr, cancel := startTestERPCServer(t, sourcesConfig(nil, sourceUpstream("rpc-a", srv.URL, util.BoolPtr(true))))
	defer func() { _ = erpcSrv.Shutdown(&log.Logger); cancel() }()
	prj, err := erpcSrv.erpc.GetProject("test_ws")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(ctx, "evm:123")
	require.NoError(t, err)
	first := dialWs(t, addr)
	subscribeNewHeads(t, first)
	requireContiguous(t, first, 3, 0)
	require.NoError(t, first.Close())

	pollsSettle(t, &vendor.latestPolls, 700*time.Millisecond, "polls stop after the idle delay")

	// Nobody polls while the chain moves on.
	chain.setPaused(true)
	tip, ok := nw.chainTracker.Tip()
	require.True(t, ok)
	idleAt := int64(tip.Number)
	chain.setGated(false)
	for range 5 {
		chain.advance()
	}
	polled := vendor.latestPolls.Load()
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, polled, vendor.latestPolls.Load(), "no polls while idle")
	chain.setPaused(false)

	second := dialWs(t, addr)
	defer second.Close()
	subscribeNewHeads(t, second)
	n, _ := nextNewHead(t, second, 10*time.Second)
	assert.Equal(t, idleAt+1, n, "the new client starts right after the pod's head where polls stopped")
	requireContiguous(t, second, 3, n)
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
	// Just after a re-ask: over 2.5 intervals come exactly 2 more.
	asked := vendor.subscribes.Load()
	time.Sleep(2500 * time.Millisecond)
	assert.Equal(t, asked+2, vendor.subscribes.Load(), "asked once per interval, not retried with backoff")
	requireContiguous(t, conn, 2, 0)
}
