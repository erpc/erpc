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
	"github.com/erpc/erpc/test/chainsim"
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

		// More subscriptions, so taps are still writing a head when the
		// unsubscribes arrive (§19 D5).
		subs := []string{clientSubId}
		for id := 3; id < 23; id++ {
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_subscribe","params":["newHeads"]}`, id))))
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for len(subs) < 21 {
			_, msg, err := conn.ReadMessage()
			require.NoError(t, err)
			var m map[string]interface{}
			require.NoError(t, json.Unmarshal(msg, &m))
			if id, ok := m["result"].(string); ok && m["id"] != nil {
				subs = append(subs, id)
			}
		}
		select {
		case <-notifCh:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for upstream to send notification")
		}

		// Unsubscribe every one as a head is written; no notification of a
		// subscription follows its unsubscribe answer.
		answerOf := map[float64]string{}
		for i, sub := range subs {
			id := 100 + i
			answerOf[float64(id)] = sub
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_unsubscribe","params":["%s"]}`, id, sub))))
		}
		removed := map[string]bool{}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var m map[string]interface{}
			require.NoError(t, json.Unmarshal(msg, &m))
			if id, ok := m["id"].(float64); ok {
				assert.Equal(t, true, m["result"])
				removed[answerOf[id]] = true
				continue
			}
			params, _ := m["params"].(map[string]interface{})
			sub, _ := params["subscription"].(string)
			assert.False(t, removed[sub], "notification for %s after its unsubscribe answer", sub)
		}
		assert.Len(t, removed, len(subs), "every unsubscribe answered")
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
	// subscribe opens, and the sources' retry, after their backoff,
	// delivers. The variant is asked while each source's newHeads
	// subscribe is still in flight, as on a fresh or reconnected socket.
	t.Run("CapacityAnswersOpenAndRetryDelivers", func(t *testing.T) {
		setupGock()
		defer util.ResetGock()
		chain := newMockChain(0x100, false)
		held := make(chan struct{})
		busyOnce := func(v *mockWsVendor) {
			v.headsHeld = held
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
		sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
		require.Eventually(t, func() bool { return a.subscribes.Load() == 1 && b.subscribes.Load() == 1 },
			10*time.Second, 10*time.Millisecond, "each source subscribes newHeads")

		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":2,"method":"eth_subscribe","params":["newPendingTransactions"]}`)))
		time.Sleep(300 * time.Millisecond)
		close(held)
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(15*time.Second)))
		_, msg, err := conn.ReadMessage()
		require.NoError(t, err)
		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(msg, &resp))
		subID, ok := resp["result"].(string)
		require.True(t, ok, "capacity answers open the subscription: %s", msg)
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
	start := func(s *chaosSeed) {
		s.startERPC()
		s.chain.Start()
	}
	receiptsAndLogsCalls := func(s *chaosSeed) (calls int64) {
		for _, v := range s.vendors {
			st := v.Stats()
			calls += st.ReceiptsCalls + st.LogsCalls + st.LogsSubscribes
		}
		return calls
	}

	t.Run("baseline", func(t *testing.T) {
		s := newChaosSeed(t, 5, true, chaosOpts{})
		start(s)
		defer s.stopERPC()
		s.blocks(3)

		// No logs tap yet: the feed fetches no receipts, so any receipts,
		// eth_getLogs or logs subscribe call would come from the bad filter.
		malformed, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", s.addr), nil)
		require.NoError(t, err)
		defer malformed.Close()
		// geth's limits: 1,000 addresses, 4 topic positions, 1,000 values
		// per position; one more of any fails as a malformed filter does.
		values := func(n int, width int) string {
			vs := make([]string, n)
			for i := range vs {
				vs[i] = fmt.Sprintf(`"0x%0*x"`, 2*width, i)
			}
			return "[" + strings.Join(vs, ",") + "]"
		}
		for name, filter := range map[string]string{
			"a malformed address":                `{"address":"0x123"}`,
			"1,001 addresses":                    `{"address":` + values(1001, 20) + `}`,
			"5 topic positions":                  `{"topics":[null,null,null,null,null]}`,
			"1,001 values in one topic position": `{"topics":[null,` + values(1001, 32) + `]}`,
		} {
			resp := sendAndReceive(t, malformed, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["logs",`+filter+`]}`)
			errObj, ok := resp["error"].(map[string]interface{})
			require.True(t, ok, "%s fails: %v", name, resp)
			assert.EqualValues(t, common.JsonRpcErrorInvalidArgument, errObj["code"], name)
		}
		s.blocks(2)
		assert.Zero(t, receiptsAndLogsCalls(s), "a filter that does not parse makes no upstream call")

		sent := telemetry.MetricWebsocketNotificationsSentTotal.WithLabelValues("test_ws", "evm:123", "logs")
		sentBytes := telemetry.MetricWebsocketNotificationBytesTotal.WithLabelValues("test_ws", "evm:123", "logs")
		sentBefore, bytesBefore := testutil.ToFloat64(sent), testutil.ToFloat64(sentBytes)
		s.dialLogs("base")
		s.blocks(3)

		plan := []chainsim.Kind{}
		for range 8 {
			plan = append(plan, chainsim.KindMatching)
		}
		plan = append(plan, chainsim.KindQuiet)
		for range 4 {
			plan = append(plan, chainsim.KindMatching)
		}
		from := s.chain.Plan(plan...).Number + 1
		s.blocks(len(plan) + 2)

		tip := s.chain.Tip()
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
		require.Len(t, s.openLogs(), len(chainsim.Filters))
		for i, kind := range plan {
			b := s.chain.CanonAt(from + uint64(i))
			require.NotNil(t, b)
			require.Equal(t, kind, b.Kind())
			want := 1
			if kind == chainsim.KindQuiet {
				want = 0
			}
			assert.Equal(t, want, s.receiptsCallsOf(b.Hash), "receipts calls for block %d (%d of the plan)", b.Number, i+1)
			for f, filter := range chainsim.Filters {
				matches := slices.ContainsFunc(b.Logs(), filter.Matches)
				assert.Equal(t, kind == chainsim.KindMatching, matches, "block %d holds logs of filter %d only when planned to match", b.Number, f)
			}
		}
		for _, v := range s.vendors {
			assert.Zero(t, v.Stats().LogsSubscribes, "%s gets no eth_subscribe logs", v.ID)
			assert.Zero(t, v.Stats().LogsCalls, "%s gets no eth_getLogs while receipts serve", v.ID)
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
		t.Logf("planned blocks %d..%d, head %d", from, from+uint64(len(plan))-1, tip.Number)
		requireInvariantsHeld(t, "test_ws", "evm:123")

		// A 3-block reorg: each client drops the reverted logs it holds
		// (removed:true) and gets the new branch's; no invariant breaks.
		reorged := s.reorg(3)
		s.blocks(3)
		require.Eventually(t, func() bool {
			for _, cl := range s.openLogs() {
				cl.mu.Lock()
				d := cl.logsDiffLocked(s, from, reorged.Number)
				cl.mu.Unlock()
				if d != "" {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond, "both clients hold the canonical logs through the reorg")
		requireInvariantsHeld(t, "test_ws", "evm:123")

		// Receipts every vendor answers 4s late: later than one attempt's
		// share of a sweep (10s over 4 upstreams, plus 1s), well within
		// the fetch's 10s. A sweep cuts every attempt; the block's next
		// attempt names one upstream with the whole 10s, so both clients
		// get the logs of the blocks minted meanwhile, and no tap ends.
		// The chain halts so no block leaves the window during the wait.
		s.setAll(func(f *chainsim.Faults) { f.SlowReceipts = 4 * time.Second })
		slowFrom := s.chain.Plan(chainsim.KindMatching, chainsim.KindMatching).Number + 1
		s.blocks(3)
		s.chain.Halt(true)
		slowTo := s.chain.Tip().Number
		require.GreaterOrEqual(t, slowTo, slowFrom+1, "both slow blocks minted before the halt")
		require.Eventually(t, func() bool {
			for _, cl := range s.openLogs() {
				cl.mu.Lock()
				d := cl.logsDiffLocked(s, from, slowTo)
				cl.mu.Unlock()
				if d != "" {
					return false
				}
			}
			return true
		}, 30*time.Second, 50*time.Millisecond, "both clients hold the logs of blocks %d..%d, whose receipts every vendor answers 4s late", slowFrom, slowTo)
		require.Len(t, s.openLogs(), len(chainsim.Filters), "no logs tap ended while receipts were slow")
		s.setAll(func(f *chainsim.Faults) { f.SlowReceipts = 0 })
		s.chain.Halt(false)
	})

	// A cold instance pins ws-a's heads while the other sources, held,
	// have not reported: the delivery gate stays shut, then opens several
	// blocks up at once. A logs client subscribed while it was shut gets
	// the logs of every block minted after its subscription id arrived.
	t.Run("subscribed before the first sendable head", func(t *testing.T) {
		s := newChaosSeed(t, 9, true, chaosOpts{})
		for _, v := range []*chainsim.Vendor{s.wsB, s.httpA, s.httpB} {
			v.Update(func(f *chainsim.Faults) { f.Held = true })
		}
		start(s)
		defer s.stopERPC()
		tr := s.tracker()
		require.Eventually(t, func() bool { _, ok := tr.Tip(); return ok }, 10*time.Second, 10*time.Millisecond, "ws-a's heads are pinned")
		_, sendable := tr.Confirmed()
		require.False(t, sendable, "the delivery gate waits for the held sources")
		s.dialLogs("cold")
		from := s.chain.Tip().Number + 1
		s.blocks(4)
		_, sendable = tr.Confirmed()
		require.False(t, sendable, "the gate is still shut %d blocks after the subscription", 4)
		for _, v := range []*chainsim.Vendor{s.wsB, s.httpA, s.httpB} {
			v.Update(func(f *chainsim.Faults) { f.Held = false })
		}
		s.blocks(4)
		tip := s.chain.Tip()
		diff := "not checked"
		for deadline := time.Now().Add(10 * time.Second); diff != "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			diff = ""
			for _, cl := range s.openLogs() {
				cl.mu.Lock()
				if d := cl.logsDiffLocked(s, from, tip.Number); d != "" {
					diff = fmt.Sprintf("%s (first log at %d): %s", cl.name, cl.first, d)
				}
				cl.mu.Unlock()
			}
		}
		require.Empty(t, diff, "both clients hold the matching logs of every block from %d, minted after they subscribed", from)
		require.Len(t, s.openLogs(), len(chainsim.Filters))
	})

	// Taps whose loops start after their clients have the ids, and twice
	// the lag bound of blocks after the subscribes, still send every block
	// confirmed since, from their start with no lag: neither a newHeads
	// restart nor a 1013 (spec §10.2). The logs clients opened first keep
	// receipts wanted, so the late taps' blocks reach the logs head while
	// their loops wait, as on a loaded instance with many subscribers. Only
	// the backlog's first and last blocks match, so writing it takes much
	// less than the lag bound: a backlog whose writes take longer than the
	// bound is lag (§10.2), and not what this test is about.
	t.Run("tap loop starts late", func(t *testing.T) {
		s := newChaosSeed(t, 10, true, chaosOpts{maxBackfill: 20 * time.Second})
		start(s)
		defer s.stopERPC()
		s.blocks(3)
		s.dialLogs("demand")
		m := chainsim.KindMatching
		s.chain.Plan(m, m)
		s.blocks(4)
		require.Eventually(t, func() bool {
			for _, cl := range s.openLogs() {
				cl.mu.Lock()
				notes := cl.notes
				cl.mu.Unlock()
				if notes == 0 {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond, "the demand clients' taps run")

		hold := make(chan struct{})
		held := func() { <-hold }
		wsTapLoopStart.Store(&held)
		released := false
		defer func() {
			wsTapLoopStart.Store(nil)
			if !released {
				close(hold)
			}
		}()
		s.dialLogs("late")
		late := s.openLogs()[len(chainsim.Filters):]
		heads := s.dialClient("late-heads", nil)
		q := chainsim.KindQuiet
		plan := make([]chainsim.Kind, 2*chaosReorgWindow)
		for i := range plan {
			plan[i] = q
		}
		plan[0], plan[1], plan[len(plan)-2], plan[len(plan)-1] = m, m, m, m
		from := s.chain.Plan(plan...).Number + 1
		to := from + uint64(len(plan)) - 1
		s.blocks(len(plan))
		tr := s.tracker()
		require.Eventually(t, func() bool { c, ok := tr.Confirmed(); return ok && c.Number >= to }, 10*time.Second, 10*time.Millisecond, "block %d is sendable before the loops start", to)
		logsEnds, headsEnds := tapEndsOf(indexer.SubTypeLogs), tapEndsOf(indexer.SubTypeNewHeads)
		close(hold)
		released = true
		s.blocks(3)

		diff := "not checked"
		for deadline := time.Now().Add(10 * time.Second); diff != "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			diff = ""
			for _, cl := range late {
				cl.mu.Lock()
				if d := cl.logsDiffLocked(s, from, to); d != "" {
					diff = fmt.Sprintf("%s (logs of blocks %d..%d, close %d, logs tap ends %s): %s", cl.name, cl.first, cl.high, cl.closeCode, endsSince(logsEnds, tapEndsOf(indexer.SubTypeLogs)), d)
				}
				cl.mu.Unlock()
			}
		}
		require.Empty(t, diff, "the late logs clients hold the matching logs of blocks %d..%d, confirmed before their loops ran", from, to)
		require.Eventually(t, func() bool {
			heads.mu.Lock()
			defer heads.mu.Unlock()
			return heads.last >= to
		}, 10*time.Second, 20*time.Millisecond, "the late newHeads client reaches block %d", to)
		heads.mu.Lock()
		assert.LessOrEqual(t, heads.first, from, "the late newHeads tap starts at its subscribe, not at its loop")
		assert.Empty(t, heads.problems, "late newHeads client")
		heads.mu.Unlock()
		for _, cl := range late {
			cl.mu.Lock()
			assert.Zero(t, cl.closeCode, "logs client %s is not closed: %s", cl.name, cl.why)
			assert.Empty(t, cl.problems, "logs client %s", cl.name)
			cl.mu.Unlock()
		}
		assert.Equal(t, logsEnds, tapEndsOf(indexer.SubTypeLogs), "no logs tap ended")
		assert.Equal(t, headsEnds, tapEndsOf(indexer.SubTypeNewHeads), "no newHeads tap ended")
	})

	t.Run("no upstream serves receipts or eth_getLogs", func(t *testing.T) {
		s := newChaosSeed(t, 6, true, chaosOpts{})
		s.ignore = []string{"eth_getBlockReceipts", "eth_getLogs"}
		start(s)
		defer s.stopERPC()
		s.blocks(3)
		conn, rerr := s.subscribeLogs(0)
		require.Nil(t, conn)
		require.NotNil(t, rerr, "logs subscribe is refused")
		assert.EqualValues(t, common.JsonRpcErrorUnsupportedException, rerr["code"], "ErrNoSubscriptionSource maps to -32601: %v", rerr)
		assert.Contains(t, fmt.Sprint(rerr), "subscription source", "the refusal names the missing source: %v", rerr)
		assert.Zero(t, receiptsAndLogsCalls(s), "no receipts, eth_getLogs or upstream logs subscription")
	})

	// With integrity on, eth_getLogs by blockHash stands in only while no
	// upstream serves receipts; its logs pass the bloom check and
	// logIndexContiguity, and a block's logs are held only once 2 vendors
	// answered the same ones. Vendor-b, the one the sweep asks, caps every
	// answer one log short over the first planned blocks: those wait for it
	// to answer in full, and no client ever holds a block with a log missing.
	t.Run("integrity on, no upstream serves receipts: logs by eth_getLogs", func(t *testing.T) {
		s := newChaosSeed(t, 7, true, chaosOpts{integrity: true, matchLast: true})
		s.ignore = []string{"eth_getBlockReceipts"}
		s.setVendor("vendor-b", func(f *chainsim.Faults) { f.CapLogs = true })
		start(s)
		defer s.stopERPC()
		s.blocks(3)
		s.dialLogs("fallback")
		s.blocks(2)
		m, q := chainsim.KindMatching, chainsim.KindQuiet
		from := s.chain.Plan(m, m, q, m, m, m, m, m).Number + 1
		s.blocks(5)
		s.setVendor("vendor-b", func(f *chainsim.Faults) { f.CapLogs = false })
		s.blocks(5)
		require.Eventually(t, func() bool {
			for _, cl := range s.openLogs() {
				cl.mu.Lock()
				d := cl.logsDiffLocked(s, from, from+7)
				cl.mu.Unlock()
				if d != "" {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond, "both clients hold the matching logs of every planned block, none missing")
		require.Len(t, s.openLogs(), len(chainsim.Filters))
		for _, cl := range s.openLogs() {
			cl.mu.Lock()
			assert.Empty(t, cl.problems, "logs client %s", cl.name)
			cl.mu.Unlock()
		}
		assert.Positive(t, s.wsB.Stats().CappedLogs+s.httpB.Stats().CappedLogs, "vendor-b cut answers short")
		assert.Positive(t, s.wsA.Stats().LogsCalls+s.httpA.Stats().LogsCalls, "vendor-a answers eth_getLogs by blockHash")
	})

	t.Run("integrity on, one vendor serves eth_getLogs and none receipts", func(t *testing.T) {
		s := newChaosSeed(t, 8, true, chaosOpts{integrity: true, oneVendor: true})
		s.ignore = []string{"eth_getBlockReceipts"}
		start(s)
		defer s.stopERPC()
		s.blocks(3)
		conn, rerr := s.subscribeLogs(0)
		require.Nil(t, rerr, "on a one-vendor network the evidence quorum is that vendor: its eth_getLogs stand in")
		require.NotNil(t, conn)
		defer conn.Close()
		s.blocks(5)
		assert.Positive(t, receiptsAndLogsCalls(s), "logs fetched by eth_getLogs")
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

// rewind reorgs the chain to a shorter branch whose head is to: every
// height from to up is replaced, and to is pushed as the new head.
func (c *mockChain) rewind(to int64) {
	c.mu.Lock()
	for n := to; n <= c.head; n++ {
		c.branch[n]++
	}
	c.head = to
	watchers := slices.Clone(c.watchers)
	c.mu.Unlock()
	for _, w := range watchers {
		select {
		case w <- to:
		default:
		}
	}
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

// block is block n as a vendor serves it, transactions included, with the
// gas fields a gRPC StreamBlocks header needs.
func (c *mockChain) block(n int64) map[string]interface{} {
	h := mockHead(n)
	c.mu.Lock()
	h["hash"], h["parentHash"] = c.hashLocked(n), c.hashLocked(n-1)
	c.mu.Unlock()
	h["transactions"] = []interface{}{fmt.Sprintf("0x%064x", n+1_000_000)}
	h["miner"] = "0x0000000000000000000000000000000000000001"
	h["gasLimit"], h["gasUsed"] = "0x1c9c380", "0x5208"
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
	requests    atomic.Int64 // every JSON-RPC request, batched ones each
	latestPolls atomic.Int64
	headNumbers atomic.Int64 // eth_blockNumber requests
	chainIdAsks atomic.Int64
	// syncing answers eth_syncing as a node still syncing.
	syncing atomic.Bool
	// noLatest refuses eth_getBlockByNumber(latest) as unsupported.
	noLatest atomic.Bool
	// wrongChain answers for another chain: eth_chainId 0x1, and latest a
	// block mockWrongChainLead above the chain's head.
	wrongChain atomic.Bool
	// failLatest, while set, is the HTTP status every latest poll fails with.
	failLatest atomic.Int32
	// slowFail holds the next latest poll for that long, then fails it with
	// a 503; slowFailed counts the polls failed so.
	slowFail   atomic.Int64
	slowFailed atomic.Int32
	// lag, while set, is how many blocks below the chain's head latest
	// and eth_blockNumber answer: a node restarted behind.
	lag atomic.Int64
	// onLatest, while set, runs at each latest poll the vendor answers.
	onLatest atomic.Pointer[func()]
}

// mockWrongChainLead is how far another chain's latest is above the
// mock chain's: a major head move on any block time.
const mockWrongChainLead = 100_000

// mockSyncingAnswer is eth_syncing from a node still syncing.
var mockSyncingAnswer = map[string]interface{}{"startingBlock": "0x0", "currentBlock": "0x10", "highestBlock": "0x100000"}

func newMockHttpVendor(t *testing.T, chain *mockChain) (*mockHttpVendor, *httptest.Server) {
	t.Helper()
	v := &mockHttpVendor{chain: chain}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if status := v.latestFault(body); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"node failure"}}`))
			return
		}
		answer := func(req map[string]interface{}) map[string]interface{} {
			v.requests.Add(1)
			params, _ := req["params"].([]interface{})
			var result interface{} = "0x1"
			switch req["method"] {
			case "eth_chainId":
				v.chainIdAsks.Add(1)
				result = "0x7b"
				if v.wrongChain.Load() {
					result = "0x1"
				}
			case "eth_syncing":
				result = false
				if v.syncing.Load() {
					result = mockSyncingAnswer
				}
			case "eth_blockNumber":
				v.headNumbers.Add(1)
				result = fmt.Sprintf("0x%x", chain.Head()-v.lag.Load())
			case "eth_getBlockByNumber":
				tag, _ := params[0].(string)
				if f := v.onLatest.Load(); f != nil && tag == "latest" {
					(*f)()
				}
				switch {
				case tag == "latest" && v.noLatest.Load():
					v.latestPolls.Add(1)
					return map[string]interface{}{"jsonrpc": "2.0", "id": req["id"],
						"error": map[string]interface{}{"code": -32004, "message": "tag not supported"}}
				case tag == "latest" && v.wrongChain.Load():
					v.latestPolls.Add(1)
					result = mockHead(chain.Head() + mockWrongChainLead)
				case tag == "latest" && v.lag.Load() > 0:
					v.latestPolls.Add(1)
					result = chain.block(chain.Head() - v.lag.Load())
				default:
					result = chain.answerBlock(params, true, &v.latestPolls)
				}
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

// latestFault is the HTTP status a single latest poll in body fails with,
// 0 for none; it counts the poll it fails.
func (v *mockHttpVendor) latestFault(body []byte) int {
	var req struct {
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || req.Method != "eth_getBlockByNumber" || len(req.Params) == 0 || req.Params[0] != "latest" {
		return 0
	}
	if d := v.slowFail.Swap(0); d > 0 {
		v.latestPolls.Add(1)
		time.Sleep(time.Duration(d))
		v.slowFailed.Add(1)
		return http.StatusServiceUnavailable
	}
	if status := v.failLatest.Load(); status != 0 {
		v.latestPolls.Add(1)
		return int(status)
	}
	return 0
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
	// syncing answers eth_syncing as a node still syncing.
	syncing bool
	// chainIdHeld, if set, holds the eth_chainId answer until it closes,
	// so the upstream registers only then.
	chainIdHeld chan struct{}
	// headsHeld, if set, holds each newHeads subscribe answer until it
	// closes; the socket keeps answering every other request meanwhile.
	headsHeld chan struct{}
	// refusePending, if set, gives the JSON-RPC error answering the n-th
	// newPendingTransactions subscribe (from 1), nil to accept it.
	refusePending func(n int32) map[string]interface{}
	pushing       atomic.Bool
	subscribes    atomic.Int32
	latestPolls   atomic.Int64

	pendingSubscribes atomic.Int32
	pendingMu         sync.Mutex
	pendingSubs       map[string]*websocket.Conn // live newPendingTransactions subscriptions
	conns             map[*websocket.Conn]struct{}
}

// dropConns closes every open socket of the vendor.
func (v *mockWsVendor) dropConns() {
	v.pendingMu.Lock()
	defer v.pendingMu.Unlock()
	for c := range v.conns {
		_ = c.Close()
	}
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
	v := &mockWsVendor{chain: chain, pendingSubs: make(map[string]*websocket.Conn), conns: make(map[*websocket.Conn]struct{})}
	v.pushing.Store(true)
	if configure != nil {
		configure(v)
	}
	first := chain.Head()
	srv := mockWsUpstream(t, func(conn *websocket.Conn) {
		done := make(chan struct{})
		defer close(done)
		v.pendingMu.Lock()
		v.conns[conn] = struct{}{}
		v.pendingMu.Unlock()
		defer func() {
			v.pendingMu.Lock()
			delete(v.conns, conn)
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
				if v.chainIdHeld != nil {
					<-v.chainIdHeld
				}
				reply["result"] = "0x7b"
			case "eth_syncing":
				reply["result"] = false
				if v.syncing {
					reply["result"] = mockSyncingAnswer
				}
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
					if v.headsHeld != nil {
						select {
						case <-done:
							return
						case <-v.headsHeld:
						}
						mockWriteJSON(conn, reply)
					}
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
				if v.headsHeld != nil {
					continue
				}
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
// transactions. Beside it, sources that cannot serve heads: a syncing HTTP
// source is never polled as a source and a syncing wss source never votes
// (#202); an HTTP source that refuses latest as unsupported stops being
// polled after 10 refusals and is asked again once its give-up expires
// (#240). The client's heads stay contiguous throughout.
func TestWebSocket_HttpSourcePolledHeads(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	prevBackoff, prevExpiry := evm.AskMaxBackoff.Load(), evm.PollerGiveUpExpiry.Load()
	evm.AskMaxBackoff.Store(200 * time.Millisecond)
	evm.PollerGiveUpExpiry.Store(3 * time.Second)
	defer func() {
		evm.AskMaxBackoff.Store(prevBackoff)
		evm.PollerGiveUpExpiry.Store(prevExpiry)
	}()

	chain := newMockChain(0x100, true)
	_, srv := newMockHttpVendor(t, chain)
	syncingHttp, syncingSrv := newMockHttpVendor(t, chain)
	syncingHttp.syncing.Store(true)
	noLatest, noLatestSrv := newMockHttpVendor(t, chain)
	noLatest.noLatest.Store(true)
	_, syncingWsURL := newMockWsVendor(t, chain, func(v *mockWsVendor) { v.syncing = true })
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 150*time.Millisecond)

	erpcSrv, addr, cancel := startTestERPCServer(t, sourcesConfig(nil,
		sourceUpstream("rpc-a", srv.URL, util.BoolPtr(true)),
		sourceUpstream("rpc-syncing", syncingSrv.URL, util.BoolPtr(true)),
		sourceUpstream("rpc-no-latest", noLatestSrv.URL, util.BoolPtr(true)),
		sourceUpstream("ws-syncing", syncingWsURL, nil),
	))
	defer func() { _ = erpcSrv.Shutdown(&log.Logger); cancel() }()
	prj, err := erpcSrv.erpc.GetProject("test_ws")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(ctx, "evm:123")
	require.NoError(t, err)
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

	for _, id := range []string{"rpc-syncing", "ws-syncing"} {
		require.Eventually(t, func() bool {
			for _, u := range nw.upstreamsRegistry.GetNetworkUpstreams(ctx, nw.networkId) {
				if u.Id() == id {
					return u.EvmSyncingState() == common.EvmSyncingStateSyncing
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "%s reports itself syncing", id)
	}
	heads := func(outcome string) float64 {
		return testutil.ToFloat64(telemetry.MetricChainTrackerSourceHeadsTotal.WithLabelValues("test_ws", "evm:123", "ws-syncing", outcome))
	}
	pollsSettle(t, &syncingHttp.latestPolls, 700*time.Millisecond, "a syncing source is not polled for heads")
	syncingPolls, voted, gated := syncingHttp.latestPolls.Load(), heads("voted"), heads("gated_voter")
	require.Eventually(t, func() bool { return heads("gated_voter") >= gated+3 }, 10*time.Second, 10*time.Millisecond,
		"a syncing wss source's heads reach intake and are held back")
	prev = requireContiguous(t, conn, 3, prev)
	assert.Equal(t, syncingPolls, syncingHttp.latestPolls.Load(), "a syncing source is not polled for heads")
	assert.Equal(t, voted, heads("voted"), "a syncing wss source's heads never vote")

	pollsSettle(t, &noLatest.latestPolls, 700*time.Millisecond, "a source that refuses latest stops being polled")
	refused := noLatest.latestPolls.Load()
	assert.GreaterOrEqual(t, refused, int64(10), "polled until 10 refusals")
	require.Eventually(t, func() bool { return noLatest.latestPolls.Load() > refused }, 10*time.Second, 20*time.Millisecond,
		"latest is asked again once the give-up expires")
	requireContiguous(t, conn, 3, prev)
}

// A poll source that starts answering for another chain, with a head far
// above, is cordoned by the chain-identity gate. Its cordon lifts only
// after 2 matching eth_chainId checks a recheck interval apart (#997), and
// a major move right after the lift goes through the gate again and
// cordons it again. The client's heads stay contiguous: the other chain's
// head never reaches it.
func TestWebSocket_CrossWiredSourceCordonedUntilTwoMatches(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	prevRecheck := evm.ChainIdCordonRecheck.Load()
	evm.ChainIdCordonRecheck.Store(500 * time.Millisecond)
	defer evm.ChainIdCordonRecheck.Store(prevRecheck)

	chain := newMockChain(0x100, false)
	_, srv := newMockHttpVendor(t, chain)
	crossed, crossedSrv := newMockHttpVendor(t, chain)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 150*time.Millisecond)

	crossedCfg := sourceUpstream("rpc-crossed", crossedSrv.URL, util.BoolPtr(true))
	// Its own state polls reach it while it is no poll source, cordoned.
	crossedCfg.Evm.StatePollerInterval = common.Duration(200 * time.Millisecond)
	// Quorum 1: the cordoned upstream's frozen head must not hold latest,
	// which bounds what the client receives.
	erpcSrv, addr, cancel := startTestERPCServer(t, sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Evm.ServedTip = &common.EvmServedTipConfig{Quorum: 1}
	}, sourceUpstream("rpc-a", srv.URL, util.BoolPtr(true)), crossedCfg))
	defer func() { _ = erpcSrv.Shutdown(&log.Logger); cancel() }()
	prj, err := erpcSrv.erpc.GetProject("test_ws")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(ctx, "evm:123")
	require.NoError(t, err)
	var up common.Upstream
	for _, u := range nw.upstreamsRegistry.GetNetworkUpstreams(ctx, nw.networkId) {
		if u.Id() == "rpc-crossed" {
			up = u
		}
	}
	require.NotNil(t, up)
	cordoned := func() bool { return nw.metricsTracker.IsCordoned(up, "eth_getBlockByNumber") }
	conn := dialWs(t, addr)
	defer conn.Close()
	subscribeNewHeads(t, conn)
	last := requireContiguous(t, conn, 3, 0)

	crossed.wrongChain.Store(true)
	require.Eventually(t, cordoned, 5*time.Second, 10*time.Millisecond, "a major move answered by another chain cordons the upstream")
	last = requireContiguous(t, conn, 2, last)
	crossed.wrongChain.Store(false)
	asks := crossed.chainIdAsks.Load()
	require.Eventually(t, func() bool { return crossed.chainIdAsks.Load() > asks }, 5*time.Second, 10*time.Millisecond,
		"the cordon is checked again")
	// Well inside the recheck interval: the first check's answer is in.
	time.Sleep(200 * time.Millisecond)
	assert.True(t, cordoned(), "one matching check does not lift the cordon")
	require.Eventually(t, func() bool { return !cordoned() }, 5*time.Second, 10*time.Millisecond,
		"a second matching check lifts the cordon")
	assert.GreaterOrEqual(t, crossed.chainIdAsks.Load(), asks+2, "lifted after 2 matching checks")

	crossed.wrongChain.Store(true)
	require.Eventually(t, cordoned, 5*time.Second, 10*time.Millisecond,
		"a major move right after the lift goes through the chain-identity gate again")
	requireContiguous(t, conn, 3, last)
}

// A network with a wss source and an HTTP poll source: each head reaches
// the client once. A 2-block reorg on both vendors then reaches the client
// as each head of the new branch, oldest first, and the client's chain
// ends equal to the vendors'. A jump of more than reorgWindow blocks then
// reaches the client as every block of it, in order, with no lag restart:
// the jump's blocks just arrived (spec §10.2). One tap is open while the
// client is subscribed, none after it leaves.
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
	lagged := telemetry.MetricTapEndTotal.WithLabelValues(append(tapLabels, indexer.TapEndLagging, indexer.OutOfRangeLag)...)
	require.Eventually(t, func() bool { return openTaps() == 1 }, 5*time.Second, 10*time.Millisecond, "one tap per subscription")
	requireInvariantsHeld(t, "test_ws", "evm:123")

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
	requireInvariantsHeld(t, "test_ws", "evm:123")

	// The sources stall, then return more than reorgWindow (default 32)
	// blocks ahead: the tracker re-anchors there and fills back down to the
	// client's cursor. The filled blocks are fresh, so the client reads them
	// all, in batches, and never restarts.
	lagBefore := testutil.ToFloat64(lagged)
	chain.setPaused(true)
	time.Sleep(300 * time.Millisecond)
	for last < chain.Head() {
		next("heads must arrive once each, in order")
	}
	jumped := chain.jump(40)
	for last < jumped {
		next("every block of a jump arrives once, in order")
	}
	require.Equal(t, chain.Hash(jumped), client[jumped], "the client's head is the vendors' after the jump")
	chain.setPaused(false)
	for range 2 {
		next("heads continue in order after the jump")
	}
	assert.Equal(t, lagBefore, testutil.ToFloat64(lagged), "a jump is no lag")
	assert.Equal(t, 1.0, openTaps(), "the subscription keeps its one tap")

	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool { return openTaps() == 0 }, 5*time.Second, 10*time.Millisecond, "the tap closes with the client")
}

// With failover on, a fallback-tier poll source polls only while no primary
// has been a hash voter for primaryLeadAfter; the client's heads stay
// contiguous across both hand-overs. A fallback-tier wss source subscribes
// newPendingTransactions only while topic admission admits it too.
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
	// Its polls move the lag reference past a primary that stops pushing,
	// so the primary's hash vote ages out (spec §6.1).
	wsFallbackCfg.Evm.StatePollerInterval = common.Duration(300 * time.Millisecond)
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
	// The primary's polls still vote, so the client has the block at once;
	// the primary stops streaming W after that block reached this instance
	// (spec §6.2).
	// W is 3s: 3 × the mock's 1s block time.
	w := 3 * time.Second
	require.Eventually(t, func() bool { return fallback.latestPolls.Load() > stalled }, w+time.Second, 10*time.Millisecond,
		"the fallback polls again once the primary stops streaming")
	require.Eventually(t, pendingOn(1, 1), 10*time.Second, 10*time.Millisecond,
		"the fallback subscribes newPendingTransactions again once the primary stops streaming")

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
// A wss upstream that registers after its network started votes from its
// first head: the selection policy takes it in at once, not at its next
// evalInterval.
func TestWebSocket_SourcesSubscribeAtInitWithoutWebSocketServer(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	chain := newMockChain(0x100, false)
	stale := func(v *mockWsVendor) { v.staleRPC = true }
	vendorA, urlA := newMockWsVendor(t, chain, stale)
	vendorB, urlB := newMockWsVendor(t, chain, stale)
	held := make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	vendorLate, urlLate := newMockWsVendor(t, chain, func(v *mockWsVendor) { v.staleRPC, v.chainIdHeld = true, held })
	t.Cleanup(release)

	cfg := sourcesConfig(nil, sourceUpstream("ws-a", urlA, nil), sourceUpstream("ws-joins-late", urlLate, nil))
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
	lateHeads := func(outcome string) float64 {
		return testutil.ToFloat64(telemetry.MetricChainTrackerSourceHeadsTotal.WithLabelValues("test_ws", "evm:123", "ws-joins-late", outcome))
	}
	voted, gated := lateHeads("voted"), lateHeads("gated_voter")
	release()
	require.Eventually(t, func() bool { return vendorLate.subscribes.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the late upstream's source subscribes newHeads")

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
	require.Eventually(t, func() bool { return lateHeads("voted") >= voted+3 }, 5*time.Second, 10*time.Millisecond,
		"the late upstream's heads vote")
	assert.Equal(t, gated, lateHeads("gated_voter"), "no head of the late upstream is held back from the vote")
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

// What integrity follow costs upstreams on an HTTP-only network, the shape
// of most production networks (no subscription source, so §6.5 rule 6
// Asks): an idle network, no client yet, sends nothing; the first client
// request starts the follow tap and the state probes (spec §13, §6.11).
// The follow tap alone needs a head once per integrity.follow.interval
// (1s), not every 300ms block (spec §3 demand), so in a window of T the
// head Asks that fetch are, by reason:
//   - no_source: rule 6 Asks at the follow pace and again at the answer's
//     Next, a pace after its fetch, at most T/pace + 1;
//   - catch_up: gate sets catchUpAt at least a pace ahead for either kind
//     of round, at most T/pace + 1 rounds; a round Asks only the voters
//     Latest lacks to reach the pin (quorum 2), and the pin's own voter is
//     at it, so one voter per round;
//   - refusal: none, since a pinned probe goes only to upstreams at its
//     block (spec §13); settle, refresh and poll: none on one linear chain
//     with no client request and an hour's poll interval.
//
// Then one voter's node falls 40 blocks behind its own counter (a restart,
// §19 P1): its answers sit below the counter, which only rises (§14), so
// its report time stays where it was. Catch-up orders by evidence, which
// counts this instance's Asks of it (§6.8), so it never takes the slot
// twice in a row while the voters that could move Latest wait.
func TestWebSocket_IntegrityFollowCost(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	chain := newMockChain(0x100, false)
	var vendors []*mockHttpVendor
	var ups []*common.UpstreamConfig
	ids := []string{"rpc-a", "rpc-b", "rpc-c", "rpc-d", "rpc-e"}
	for _, id := range ids {
		v, srv := newMockHttpVendor(t, chain)
		vendors = append(vendors, v)
		up := sourceUpstream(id, srv.URL, nil)
		up.Evm.StatePollerInterval = common.Duration(time.Hour)
		ups = append(ups, up)
	}
	requests := func() (n int64) {
		for _, v := range vendors {
			n += v.requests.Load()
		}
		return n
	}
	asks := func() (n int64) {
		for _, v := range vendors {
			n += v.latestPolls.Load() + v.headNumbers.Load()
		}
		return n
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go chain.run(ctx, 300*time.Millisecond)

	erpcSrv, addr, cancel := startTestERPCServer(t, sourcesConfig(func(nw *common.NetworkConfig) {
		nw.Integrity = &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
			Follow:     &common.IntegrityFollowConfig{Enabled: util.BoolPtr(true)},
			StateProbe: &common.IntegrityStateProbeConfig{Enabled: util.BoolPtr(true)},
		}}
	}, ups...))
	defer func() { _ = erpcSrv.Shutdown(&log.Logger); cancel() }()

	// Idle: once the upstreams' bootstrap calls are done, nothing.
	start := time.Now()
	settled := assert.Eventually(t, func() bool {
		before := requests()
		time.Sleep(time.Second)
		return requests() == before
	}, 10*time.Second, 10*time.Millisecond, "an idle network sends no requests")
	from, at := requests(), time.Now()
	time.Sleep(3 * time.Second)
	idle := float64(requests()-from) / time.Since(at).Minutes()
	t.Logf("idle network: %.0f requests/min (settled %v after %s)", idle, settled, time.Since(start).Round(time.Second))
	assert.Zero(t, idle, "an idle network sends no requests per minute")

	resp, err := http.Post(fmt.Sprintf("http://%s/test_ws/evm/123", addr), "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	require.NoError(t, err)
	_ = resp.Body.Close()
	pace := common.DefaultIntegrityFollowInterval
	reasons := []common.AskReason{common.AskNoSource, common.AskCatchUp, common.AskRefusal, common.AskSettle, common.AskRefresh, common.AskPoll}
	fetched := func() map[common.AskReason]float64 {
		out := map[common.AskReason]float64{}
		for _, r := range reasons {
			out[r] = testutil.ToFloat64(telemetry.MetricHeadAskTotal.WithLabelValues("test_ws", "evm:123", string(r), string(common.AskFetched)))
		}
		return out
	}
	time.Sleep(time.Second)
	from, at = requests(), time.Now()
	asked, before := asks(), fetched()
	time.Sleep(5 * time.Second)
	after, vendorAsks, secs := fetched(), asks()-asked, time.Since(at).Seconds()
	follow := float64(requests()-from) / secs
	rounds := float64(int(secs/pace.Seconds()) + 1)
	by := map[common.AskReason]float64{}
	for _, r := range reasons {
		by[r] = after[r] - before[r]
	}
	t.Logf("follow-only network over %.1fs: %.1f requests/s, %d vendor head requests, fetched Asks by reason %v", secs, follow, vendorAsks, by)
	assert.Positive(t, follow, "the first client request starts the follow tap and the probes")
	assert.LessOrEqual(t, by[common.AskNoSource], rounds, "rule 6 Asks once per follow pace")
	assert.LessOrEqual(t, by[common.AskCatchUp], rounds, "one catch-up round per follow pace, one lagging voter each")
	for _, r := range []common.AskReason{common.AskRefusal, common.AskSettle, common.AskRefresh, common.AskPoll} {
		assert.Zero(t, by[r], "no %s Asks on a follow-only network", r)
	}
	assert.LessOrEqual(t, float64(vendorAsks), 2*rounds, "every vendor head request is one of those Asks")

	prj, err := erpcSrv.erpc.GetProject("test_ws")
	require.NoError(t, err)
	network, err := prj.GetNetwork(context.Background(), "evm:123")
	require.NoError(t, err)
	voters := network.EvmHashVoters("", network.chainTracker.W(), false).Voters
	require.Len(t, voters, len(ids))
	// Rule 6 Asks the first voter; every other voter's latest poll is a
	// catch-up slot. The lagging node is the other voter with the oldest
	// report: next in line for the slot.
	var lagging string
	var counter int64
	var oldest time.Time
	for _, u := range network.upstreamsRegistry.GetNetworkUpstreams(context.Background(), network.networkId) {
		sp := u.EvmStatePoller()
		if at := sp.LatestReportAt(); u.Id() != voters[0] && (lagging == "" || at.Before(oldest)) {
			lagging, counter, oldest = u.Id(), sp.LatestBlock(), at
		}
	}
	var slotsMu sync.Mutex
	var slots []string
	for i, v := range vendors {
		id := ids[i]
		polled := func() {
			if id == voters[0] {
				return
			}
			slotsMu.Lock()
			slots = append(slots, id)
			slotsMu.Unlock()
		}
		v.onLatest.Store(&polled)
	}
	vendors[slices.Index(ids, lagging)].lag.Store(chain.Head() - counter + 40)
	// 40 blocks of 300ms: its answers stay below its counter all along,
	// and its report stays younger than the 30s bench.
	time.Sleep(8 * time.Second)
	for _, v := range vendors {
		v.onLatest.Store(nil)
	}
	slotsMu.Lock()
	defer slotsMu.Unlock()
	t.Logf("catch-up slots with %s 40 blocks behind its counter: %v", lagging, slots)
	require.GreaterOrEqual(t, len(slots), 3, "catch-up keeps Asking while Latest holds the pin back")
	for i := 1; i < len(slots); i++ {
		require.False(t, slots[i] == lagging && slots[i-1] == lagging,
			"%s, answering below its counter, takes the catch-up slot twice in a row: %v", lagging, slots)
	}
}
