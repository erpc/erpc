package erpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/h2non/gock"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//
// --- Upgrade detection ---
//

// A client advertising gzip (every browser, most WS libraries) must still be
// able to upgrade under the default config, where response gzip is enabled.
func TestWebSocket_UpgradeWithAcceptEncodingGzip(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	cfg := httpOnlyConfig()
	cfg.Server.EnableGzip = util.BoolPtr(true)
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	hdr := http.Header{}
	hdr.Set("Accept-Encoding", "gzip, deflate, br")
	conn, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", addr), hdr)
	require.NoError(t, err, "upgrade must succeed when the client accepts gzip")
	defer conn.Close()
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	res := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
	assert.Equal(t, "0xabc123", res["result"])
}

// A plain JSON-RPC POST that merely carries an Upgrade header is not a
// WebSocket handshake and must stay bounded by server.maxTimeout.
func TestWebSocket_UpgradeHeaderDoesNotBypassMaxTimeout(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc1.localhost").
		Post("/").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		Delay(3 * time.Second).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

	cfg := httpOnlyConfig()
	d := common.Duration(300 * time.Millisecond)
	cfg.Server.MaxTimeout = &d
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/test_ws/evm/123", addr),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Upgrade", "websocket")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second, "request must be cut off near maxTimeout, took %s (body=%s)", elapsed, body)
	assert.Contains(t, string(body), "timeout")
}

// Header tokens are case-insensitive (RFC 6455 section 4.2.1); a handshake
// spelled "Upgrade: WebSocket" must upgrade, not be answered as a
// healthcheck.
func TestWebSocket_UpgradeHeaderIsCaseInsensitive(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
	defer cleanup()

	raw, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))

	_, err = fmt.Fprintf(raw, "GET /test_ws/evm/123 HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: WebSocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", addr)
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
}

//
// --- Connection lifecycle ---
//

// subscribingWsUpstream answers eth_subscribe/eth_unsubscribe on top of the
// state-poller methods.
func subscribingWsUpstream(t *testing.T) string {
	t.Helper()
	srv := mockWsUpstream(t, func(conn *websocket.Conn) {
		standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
			result := interface{}("0x1")
			switch method {
			case "eth_subscribe":
				result = "0xupstreamsub"
			case "eth_unsubscribe":
				result = true
			}
			_ = mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
		})
	})
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func wsConnCount(s *HttpServer) (active, subscribed int) {
	s.activeWsConns.Range(func(_, _ interface{}) bool { active++; return true })
	s.subscriptionManager.connMu.Lock()
	subscribed = len(s.subscriptionManager.conns)
	s.subscriptionManager.connMu.Unlock()
	return active, subscribed
}

// A client that subscribes and immediately closes must not leave an egress
// adapter attached to the indexer (it would receive every event forever).
func TestWebSocket_SubscribeThenCloseDoesNotLeak(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	srv, addr, cancel := startTestERPCServer(t, standardWsConfig(subscribingWsUpstream(t)))
	defer func() { _ = srv.Shutdown(&log.Logger); cancel() }()

	for i := 0; i < 20; i++ {
		conn := dialWs(t, addr)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)))
		require.NoError(t, conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")))
		_ = conn.Close()
	}

	require.Eventually(t, func() bool {
		active, subscribed := wsConnCount(srv)
		return active == 0 && subscribed == 0
	}, 5*time.Second, 50*time.Millisecond, "connections must be fully cleaned up")
}

// eth_unsubscribe only reaches the caller's own subscriptions.
func TestWebSocket_UnsubscribeIsScopedToConnection(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	addr, cleanup := setupTestERPCServer(t, standardWsConfig(subscribingWsUpstream(t)))
	defer cleanup()

	connA := dialWs(t, addr)
	defer connA.Close()
	connB := dialWs(t, addr)
	defer connB.Close()

	resp := sendAndReceive(t, connA, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	subID, ok := resp["result"].(string)
	require.True(t, ok, "subscribe failed: %v", resp)

	unsub := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"eth_unsubscribe","params":["%s"]}`, subID)
	resp = sendAndReceive(t, connB, unsub)
	assert.NotNil(t, resp["error"], "another connection must not remove the subscription")

	resp = sendAndReceive(t, connA, unsub)
	assert.Equal(t, true, resp["result"], "owner must still be able to unsubscribe: %v", resp)
}

// Shutdown closes client connections with 1001 as soon as it starts, not
// after waitBeforeShutdown, and lets in-flight requests answer first.
func TestWebSocket_ShutdownClosesPromptlyAfterInflightRequests(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc1.localhost").
		Post("/").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		Delay(500 * time.Millisecond).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xabc123"})

	cfg := standardWsConfig(subscribingWsUpstream(t))
	cfg.Server.WaitBeforeShutdown = common.Duration(5 * time.Second).Ptr()
	srv, addr, cancel := startTestERPCServer(t, cfg)
	defer func() { cancel(); _ = srv.Shutdown(&log.Logger) }()

	subscriber := dialWs(t, addr)
	defer subscriber.Close()
	resp := sendAndReceive(t, subscriber, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.NotNil(t, resp["result"], "subscribe failed: %v", resp)

	requester := dialWs(t, addr)
	defer requester.Close()
	require.NoError(t, requester.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":7,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)))
	time.Sleep(100 * time.Millisecond) // let the request start

	start := time.Now()
	cancel()

	var closeErr *websocket.CloseError
	_ = subscriber.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err := subscriber.ReadMessage()
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseGoingAway, closeErr.Code)
	assert.Less(t, time.Since(start), time.Second, "close must not wait for waitBeforeShutdown")

	_ = requester.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := requester.ReadMessage()
	require.NoError(t, err, "in-flight request must be answered before the close")
	assert.Contains(t, string(msg), "0xabc123")
	_, _, err = requester.ReadMessage()
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseGoingAway, closeErr.Code)

	// A draining server closes new connections straight away.
	late := dialWs(t, addr)
	defer late.Close()
	_ = late.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = late.ReadMessage()
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseGoingAway, closeErr.Code)
}

// A peer that never answers pings is disconnected.
func TestWebSocket_UnresponsivePeerIsDisconnected(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	cfg := httpOnlyConfig()
	cfg.Server.WebSocket = &common.WebSocketServerConfig{PingInterval: common.Duration(200 * time.Millisecond).Ptr()}
	srv, addr, cancel := startTestERPCServer(t, cfg)
	defer func() { _ = srv.Shutdown(&log.Logger); cancel() }()

	conn := dialWs(t, addr)
	defer conn.Close()
	conn.SetPingHandler(func(string) error { return nil }) // never pong
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	require.Eventually(t, func() bool {
		active, _ := wsConnCount(srv)
		return active == 0
	}, 3*time.Second, 50*time.Millisecond)
}

// A connection runs at most maxConcurrentRequestsPerConnection requests at
// once; the rest wait instead of failing.
func TestWebSocket_ConcurrentRequestsAreBounded(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	gock.New("http://rpc1.localhost").
		Post("/").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		Delay(300 * time.Millisecond).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xabc123"})

	cfg := httpOnlyConfig()
	cfg.Server.WebSocket = &common.WebSocketServerConfig{MaxConcurrentRequestsPerConnection: 1}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	conn := dialWs(t, addr)
	defer conn.Close()

	start := time.Now()
	for i := 0; i < 3; i++ {
		// Distinct params so multiplexing can't merge them.
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance","params":["0x%d","latest"]}`, i, i)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(msg)))
	}
	for i := 0; i < 3; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.Contains(t, string(msg), "0xabc123")
	}
	assert.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond, "requests must run one at a time")
}

//
// --- Error shapes ---
//

func TestWebSocket_ErrorShapes(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	addr, cleanup := setupTestERPCServer(t, standardWsConfig(subscribingWsUpstream(t)))
	defer cleanup()
	conn := dialWs(t, addr)
	defer conn.Close()

	t.Run("UnknownSubscriptionTypeIsInvalidParams", func(t *testing.T) {
		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newBananas"]}`)
		require.NotNil(t, resp["error"])
		assert.EqualValues(t, common.JsonRpcErrorInvalidArgument, resp["error"].(map[string]interface{})["code"])
	})

	t.Run("BatchWithLeadingWhitespace", func(t *testing.T) {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(" \n[{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"eth_getBalance\",\"params\":[\"0xaaaa\",\"latest\"]}]")))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := conn.ReadMessage()
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(msg), "["), "expected a batch response, got %s", msg)
		assert.Contains(t, string(msg), `"result"`)
	})

	t.Run("EmptyBatchIsSingleInvalidRequest", func(t *testing.T) {
		resp := sendAndReceive(t, conn, `[]`)
		require.NotNil(t, resp["error"])
		assert.EqualValues(t, common.JsonRpcErrorClientSideException, resp["error"].(map[string]interface{})["code"])
	})
}

// A WebSocket connection is bound to one network, so a path that doesn't
// name one is refused at the handshake.
func TestWebSocket_UpgradeRequiresNetwork(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
	defer cleanup()

	_, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws", addr), nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// A request that panics must still get a reply with its own id: WebSocket
// clients match replies by id, so without one they wait for their timeout.
func TestWebSocket_PanickedRequestRepliesWithItsId(t *testing.T) {
	// No upgrade request: resolving the client IP panics inside handleRequest.
	wsc := &WsConnection{
		id:        "test",
		logger:    &log.Logger,
		networkId: "evm:123",
		server:    &HttpServer{serverCfg: &common.ServerConfig{}},
		project:   &PreparedProject{Config: &common.ProjectConfig{Id: "main"}},
	}
	startedAt := time.Now()

	out, _ := wsc.handleRequest(t.Context(), []byte(`{"jsonrpc":"2.0","id":7,"method":"eth_chainId"}`), &startedAt, false)
	require.NotNil(t, out)
	body, err := common.SonicCfg.Marshal(out)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"id":7`)
	assert.Contains(t, string(body), `"error"`)
}

// Every message on a WebSocket connection authenticates with the upgrade
// request's credentials, so an upgrade whose credentials no strategy accepts
// is refused instead of holding a connection that can never be served.
// Method scoping still applies per message.
func TestWebSocket_UpgradeRequiresAcceptedCredentials(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	cfg := httpOnlyConfig()
	cfg.Projects[0].Auth = &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{{
		Type:          common.AuthTypeSecret,
		Secret:        &common.SecretStrategyConfig{Value: "s3cret"},
		IgnoreMethods: []string{"*"},
		AllowMethods:  []string{"eth_getBalance"},
	}}}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	_, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123", addr), nil)
	require.Error(t, err, "an upgrade without credentials must be refused")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	_, resp, err = websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123?secret=wrong", addr), nil)
	require.Error(t, err, "an upgrade with a rejected secret must be refused")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/evm/123?secret=s3cret", addr), nil)
	require.NoError(t, err)
	defer conn.Close()

	ok := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000001","latest"]}`)
	assert.Equal(t, "0xabc123", ok["result"])

	// The strategy only covers eth_getBalance; other methods stay unauthorized.
	denied := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":2,"method":"eth_blockNumber","params":[]}`)
	assert.NotNil(t, denied["error"], "method scoping must still apply per message")
	assert.Nil(t, denied["result"])
}

// closeRecorder is a response body that records whether it was closed.
type closeRecorder struct{ closed atomic.Bool }

func (c *closeRecorder) Read([]byte) (int, error) { return 0, io.EOF }
func (c *closeRecorder) Close() error             { c.closed.Store(true); return nil }

// A response is released even when its write never starts, as on the batch
// and HTTP paths.
func TestWebSocket_ResponseReleasedWhenWriteNeverStarts(t *testing.T) {
	wsc := &WsConnection{
		id:     "test",
		logger: &log.Logger,
		server: &HttpServer{serverCfg: &common.ServerConfig{}},
	}
	wsc.closed.Store(true)

	body := &closeRecorder{}
	wsc.writeNormalizedResponse(common.NewNormalizedResponse().WithBody(body))
	assert.Eventually(t, body.closed.Load, time.Second, 10*time.Millisecond)
}

// Errors raised before a WebSocket request is bound to its network carry
// the connection's architecture, as on HTTP: an SVM consumer rate-limit
// error uses the SVM wire code, not -32005 (NodeUnhealthy to Solana clients).
func TestWebSocket_SvmAuthRateLimitUsesSvmWireCode(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{{
			Id: "test_ws",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureSvm,
				Svm:          &common.SvmNetworkConfig{Cluster: "mainnet-beta"},
			}},
			Upstreams: []*common.UpstreamConfig{{
				Id:       "svm-ws-test",
				Type:     common.UpstreamTypeSvm,
				Endpoint: "http://svm-ws-rpc1.localhost",
				Svm:      &common.SvmUpstreamConfig{Cluster: "mainnet-beta"},
			}},
			Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{{
				Type:            common.AuthTypeSecret,
				Secret:          &common.SecretStrategyConfig{Value: "s3cret"},
				RateLimitBudget: "one-per-minute",
			}}},
		}},
		RateLimiters: &common.RateLimiterConfig{Budgets: []*common.RateLimitBudgetConfig{{
			Id:    "one-per-minute",
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 1, Period: common.RateLimitPeriodMinute}},
		}}},
	}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/test_ws/svm/mainnet-beta?secret=s3cret", addr), nil)
	require.NoError(t, err)
	defer conn.Close()

	// Both run concurrently: one spends the budget (and may wait on the
	// unreachable upstream), the other is rejected at auth straight away.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`)))
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":2,"method":"getSlot","params":[]}`)))
	var limited map[string]interface{}
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	for limited == nil {
		_, msg, err := conn.ReadMessage()
		require.NoError(t, err)
		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(msg, &resp))
		if _, isErr := resp["error"]; isErr {
			limited = resp
		}
	}
	errObj, ok := limited["error"].(map[string]interface{})
	require.True(t, ok, "one request must be rate limited: %v", limited)
	assert.EqualValues(t, -32000, errObj["code"])
}

// A request without an id is a notification: it runs but gets no reply
// (JSON-RPC 2.0 §4.1), alone or in a batch. "id": null is a request and is
// answered.
func TestWebSocket_NotificationsGetNoReply(t *testing.T) {
	setupGock()
	defer util.ResetGock()
	addr, cleanup := setupTestERPCServer(t, httpOnlyConfig())
	defer cleanup()
	conn := dialWs(t, addr)
	defer conn.Close()

	const params = `"params":["0x0000000000000000000000000000000000000001","latest"]`

	nullID := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":null,"method":"eth_getBalance",`+params+`}`)
	assert.Equal(t, "0xabc123", nullID["result"], `"id": null is a request and must be answered`)

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","method":"eth_getBalance",`+params+`}`)))
	reply := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":7,"method":"eth_getBalance",`+params+`}`)
	assert.EqualValues(t, 7, reply["id"], "the first reply must be the request's, not the notification's")
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`[{"jsonrpc":"2.0","method":"eth_getBalance",`+params+`},{"jsonrpc":"2.0","method":"eth_getBalance",`+params+`}]`)))

	// Neither the notification nor the batch of notifications is answered.
	// (A gorilla read timeout ends the connection's reads, so check once.)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	_, msg, err := conn.ReadMessage()
	require.Error(t, err, "notifications must not be answered, got %s", msg)
}

// Two projects can define the same network, each with its own upstreams. A
// subscription on the second project must be served by that project's
// upstreams, not by the first project's network that was bootstrapped first.
func TestWebSocket_ProjectsSharingANetworkUseTheirOwnUpstreams(t *testing.T) {
	setupGock()
	defer util.ResetGock()

	var subscribesA, subscribesB atomic.Int32
	mockWith := func(count *atomic.Int32, subID string) *httptest.Server {
		return mockWsUpstream(t, func(conn *websocket.Conn) {
			standardMockWsHandler(conn, func(method string, id interface{}, _ map[string]interface{}) {
				result := interface{}("0x1")
				if method == "eth_subscribe" {
					count.Add(1)
					result = subID
				}
				mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
			})
		})
	}
	upA, upB := mockWith(&subscribesA, "0xa"), mockWith(&subscribesB, "0xb")
	defer upA.Close()
	defer upB.Close()

	cfg := standardWsConfig("ws" + strings.TrimPrefix(upA.URL, "http"))
	second := *cfg.Projects[0]
	second.Id = "test_ws2"
	second.Upstreams = []*common.UpstreamConfig{
		{Id: "http-upstream", Type: common.UpstreamTypeEvm, Endpoint: "http://rpc1.localhost", Evm: &common.EvmUpstreamConfig{ChainId: 123}},
		{Id: "ws-upstream", Type: common.UpstreamTypeEvm, Endpoint: "ws" + strings.TrimPrefix(upB.URL, "http"), Evm: &common.EvmUpstreamConfig{ChainId: 123}},
	}
	cfg.Projects = append(cfg.Projects, &second)
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	subscribe := func(project string) {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/%s/evm/123", addr, project), nil)
		require.NoError(t, err)
		t.Cleanup(func() { conn.Close() })
		resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
		require.Nil(t, resp["error"], "subscribe on %s: %v", project, resp)
	}

	subscribe("test_ws")
	require.Eventually(t, func() bool { return subscribesA.Load() > 0 }, 5*time.Second, 20*time.Millisecond)
	subscribe("test_ws2")
	assert.Eventually(t, func() bool { return subscribesB.Load() > 0 }, 5*time.Second, 20*time.Millisecond,
		"the second project's subscription must subscribe on its own upstream")
}
