package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWsServer is a minimal JSON-RPC WebSocket upstream whose connections
// can be black-holed: TCP stays open but nothing, not even a pong, is
// written back.
type fakeWsServer struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	conns []*fakeWsConn

	newConn chan *fakeWsConn
}

type fakeWsConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	// silent simulates a black-holed path: pings are swallowed (no pong)
	// and the server never writes, but the TCP connection stays open.
	silent atomic.Bool
	// subscribeCh receives the request id (raw JSON) of each
	// eth_subscribe request the server answers.
	subscribeCh chan string
}

func newFakeWsServer(t *testing.T) *fakeWsServer {
	f := &fakeWsServer{t: t, newConn: make(chan *fakeWsConn, 16)}
	upgrader := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		sc := &fakeWsConn{conn: conn, subscribeCh: make(chan string, 16)}
		conn.SetPingHandler(func(appData string) error {
			if sc.silent.Load() {
				return nil // swallow: black-holed path sends no pong
			}
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})
		f.mu.Lock()
		f.conns = append(f.conns, sc)
		f.mu.Unlock()
		f.newConn <- sc
		go sc.readLoop()
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWsServer) wsURL(t *testing.T) *url.URL {
	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)
	u.Scheme = "ws"
	return u
}

// readLoop answers eth_subscribe with an incrementing subscription id.
// Control frames (pings) are handled inside ReadMessage via the handler
// installed above, so silencing the ping handler is enough to emulate a
// peer that no longer processes anything.
func (sc *fakeWsConn) readLoop() {
	subCounter := 0
	for {
		_, msg, err := sc.conn.ReadMessage()
		if err != nil {
			return
		}
		if sc.silent.Load() {
			continue // black-holed: never respond
		}
		var req struct {
			ID     interface{}   `json:"id"`
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
		}
		if err := common.SonicCfg.Unmarshal(msg, &req); err != nil {
			continue
		}
		if req.Method == "eth_subscribe" {
			subCounter++
			subID := "0xtestsub" + string(rune('0'+subCounter))
			resp, _ := common.SonicCfg.Marshal(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  subID,
			})
			sc.write(websocket.TextMessage, resp)
			sc.subscribeCh <- subID
		}
	}
}

func (sc *fakeWsConn) write(messageType int, data []byte) {
	sc.writeMu.Lock()
	defer sc.writeMu.Unlock()
	_ = sc.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = sc.conn.WriteMessage(messageType, data)
}

func (sc *fakeWsConn) sendNewHead(subID string, blockNumberHex string) {
	notif, _ := common.SonicCfg.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_subscription",
		"params": map[string]interface{}{
			"subscription": subID,
			"result": map[string]interface{}{
				"number":     blockNumberHex,
				"hash":       "0xhash" + blockNumberHex,
				"parentHash": "0xparent" + blockNumberHex,
			},
		},
	})
	sc.write(websocket.TextMessage, notif)
}

// compressWsLiveness shrinks the keepalive windows so dead-peer detection
// happens in milliseconds instead of minutes, restoring them on cleanup.
func compressWsLiveness(t *testing.T) {
	origPing, origPong := wsPingInterval, wsPongWait
	wsPingInterval = 50 * time.Millisecond
	wsPongWait = 150 * time.Millisecond
	t.Cleanup(func() {
		wsPingInterval, wsPongWait = origPing, origPong
	})
}

func newTestWsClient(t *testing.T, u *url.URL) *WsJsonRpcClient {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel)
	up := common.NewFakeUpstream("test-ws-upstream")
	ci, err := NewWsJsonRpcClient(ctx, &logger, "test-project", up, u, nil, nil)
	require.NoError(t, err)
	c, ok := ci.(*WsJsonRpcClient)
	require.True(t, ok)
	return c
}

func subscribeNewHeads(t *testing.T, c *WsJsonRpcClient, handler func(params []byte)) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sub := &WsSubscription{Handler: handler}
	nq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`))
	_, err := c.SendRequest(WithWsSubscription(ctx, sub), nq)
	require.NoError(t, err)
	require.NotEmpty(t, sub.ID)
	require.Equal(t, c.Epoch(), sub.Epoch)
	return sub.ID
}

// TestWsClientDetectsSilentPeerAndReconnects: a peer that keeps TCP open
// but stops responding must be declared dead by the liveness deadline,
// re-dialed, and resume delivering notifications.
func TestWsClientDetectsSilentPeerAndReconnects(t *testing.T) {
	compressWsLiveness(t)
	server := newFakeWsServer(t)
	client := newTestWsClient(t, server.wsURL(t))

	disconnected := make(chan struct{}, 1)
	reconnected := make(chan struct{}, 1)
	client.SetOnDisconnect("test", func() {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	})
	client.SetOnReconnect("test", func() {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})

	// First connection established and subscribed.
	var conn1 *fakeWsConn
	select {
	case conn1 = <-server.newConn:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the initial connection")
	}
	heads := make(chan []byte, 16)
	onHead := func(params []byte) { heads <- params }
	subID1 := subscribeNewHeads(t, client, onHead)
	conn1.sendNewHead(subID1, "0x1")
	select {
	case <-heads:
	case <-time.After(2 * time.Second):
		t.Fatal("never received the first head")
	}

	// Black-hole the connection: TCP stays open, nothing flows back.
	conn1.silent.Store(true)

	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("client never detected the silent (half-open) connection — liveness deadline did not fire")
	}

	select {
	case <-reconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("client never reconnected after detecting the dead connection")
	}

	var conn2 *fakeWsConn
	select {
	case conn2 = <-server.newConn:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the re-dialed connection")
	}
	assert.True(t, client.IsConnected())

	// Re-subscribe on the new connection (the wsupstream adapter does this
	// from its reconnect hook) and verify notifications flow again.
	subID2 := subscribeNewHeads(t, client, onHead)
	conn2.sendNewHead(subID2, "0x2")
	select {
	case <-heads:
	case <-time.After(2 * time.Second):
		t.Fatal("no heads delivered after reconnection — client did not self-heal")
	}
}

// TestWsClientPingWriteFailureForcesReconnect covers the secondary path:
// when the ping write itself errors (connection reset under our feet), the
// client must tear the connection down and re-dial rather than only logging.
func TestWsClientPingWriteFailureForcesReconnect(t *testing.T) {
	compressWsLiveness(t)
	server := newFakeWsServer(t)
	client := newTestWsClient(t, server.wsURL(t))

	reconnected := make(chan struct{}, 1)
	client.SetOnReconnect("test", func() {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})

	var conn1 *fakeWsConn
	select {
	case conn1 = <-server.newConn:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the initial connection")
	}

	// Hard-kill the server side of the TCP connection (RST-ish): the next
	// client ping write (or read) fails.
	_ = conn1.conn.UnderlyingConn().Close()

	select {
	case <-reconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("client never reconnected after the connection was killed")
	}
	select {
	case <-server.newConn:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the re-dialed connection")
	}
}

// relabelledUpstream reports a network label that can change after the
// client is built, as upstream.Upstream's does once it joins a network.
type relabelledUpstream struct {
	common.Upstream
	label atomic.Value
}

func (u *relabelledUpstream) NetworkLabel() string { return u.label.Load().(string) }

// TestWsClientConnectedGaugeFollowsNetworkLabel: the connectivity gauge
// must move to the upstream's current network label and drop the series
// published under the old one.
func TestWsClientConnectedGaugeFollowsNetworkLabel(t *testing.T) {
	compressWsLiveness(t)
	server := newFakeWsServer(t)
	up := &relabelledUpstream{Upstream: common.NewFakeUpstream("test-ws-gauge-upstream")}
	up.label.Store("n/a")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel)
	_, err := NewWsJsonRpcClient(ctx, &logger, "test-project", up, server.wsURL(t), nil, nil)
	require.NoError(t, err)
	<-server.newConn

	gauge := telemetry.MetricUpstreamWebsocketConnected
	labels := func(network string) []string {
		return []string{"test-project", up.VendorName(), network, up.Id()}
	}
	require.Equal(t, 1.0, testutil.ToFloat64(gauge.WithLabelValues(labels("n/a")...)))

	up.label.Store("evm:123")
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(gauge.WithLabelValues(labels("evm:123")...)) == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.False(t, gauge.DeleteLabelValues(labels("n/a")...), "series under the stale label should have been deleted")
}

// newWsTestServer upgrades every request and hands the connection to
// handle, closing it when handle returns.
func newWsTestServer(t *testing.T, handle func(conn *websocket.Conn)) *url.URL {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handle(conn)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	u.Scheme = "ws"
	return u
}

// TestWsClientRedialsWhenConnDiesDuringHandler: the connection dies while
// readLoop is inside a subscription handler and the ping loop notices
// first. readLoop must still tear down and re-dial once the handler
// returns.
func TestWsClientRedialsWhenConnDiesDuringHandler(t *testing.T) {
	compressWsLiveness(t)
	server := newFakeWsServer(t)
	client := newTestWsClient(t, server.wsURL(t))
	conn1 := <-server.newConn

	entered := make(chan struct{})
	release := make(chan struct{})
	subID := subscribeNewHeads(t, client, func([]byte) {
		close(entered)
		<-release
	})
	conn1.sendNewHead(subID, "0x1")
	<-entered

	_ = conn1.conn.UnderlyingConn().Close()
	time.Sleep(3 * client.pingInterval) // a ping write fails while readLoop is busy
	close(release)

	select {
	case <-server.newConn:
	case <-time.After(3 * time.Second):
		t.Fatal("client never re-dialed after the connection died during a handler")
	}
}

// TestWsClientBacksOffWhenPeerDropsAfterHandshake: a peer that accepts the
// handshake and immediately closes must not drive a hot re-dial loop.
func TestWsClientBacksOffWhenPeerDropsAfterHandshake(t *testing.T) {
	var handshakes atomic.Int64
	u := newWsTestServer(t, func(conn *websocket.Conn) {
		handshakes.Add(1)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, ""), time.Now().Add(time.Second))
	})
	newTestWsClient(t, u)

	time.Sleep(time.Second)
	assert.LessOrEqual(t, handshakes.Load(), int64(3))
}

// TestWsClientFailsFastWhileRedialing: while a re-dial is stuck in a slow
// handshake, requests must fail fast with a retryable error.
func TestWsClientFailsFastWhileRedialing(t *testing.T) {
	var requests atomic.Int64
	first := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			time.Sleep(3 * time.Second) // never upgrade within the test
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		first <- conn
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	u.Scheme = "ws"
	client := newTestWsClient(t, u)

	_ = (<-first).UnderlyingConn().Close()
	require.Eventually(t, func() bool { return requests.Load() >= 2 }, 3*time.Second, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = client.SendRequest(ctx, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)))
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure), "got %v", err)
}

// TestWsClientIgnoresDuplicateResponses: repeated responses for one id must
// not wedge readLoop.
func TestWsClientIgnoresDuplicateResponses(t *testing.T) {
	u := newWsTestServer(t, func(conn *websocket.Conn) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				ID     interface{} `json:"id"`
				Method string      `json:"method"`
			}
			_ = common.SonicCfg.Unmarshal(msg, &req)
			resp, _ := common.SonicCfg.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": "0x1"})
			n := 1
			if req.Method == "dup" {
				n = 50
			}
			for i := 0; i < n; i++ {
				_ = conn.WriteMessage(websocket.TextMessage, resp)
			}
		}
	})
	client := newTestWsClient(t, u)

	send := func(method string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := client.SendRequest(ctx, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":[]}`)))
		return err
	}
	for i := 0; i < 50; i++ {
		require.NoError(t, send("dup"))
	}
	require.NoError(t, send("eth_chainId"))
}

type wsTestRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params []interface{}   `json:"params"`
}

// newSubscribeServer answers each eth_subscribe after delay with id
// "0x<n>" from a per-connection counter, immediately followed by one
// notification for it. Other methods get result true. Every request read
// is reported on requests.
func newSubscribeServer(t *testing.T, delay time.Duration, requests chan<- wsTestRequest) *url.URL {
	return newWsTestServer(t, func(conn *websocket.Conn) {
		subs := 0
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req wsTestRequest
			_ = common.SonicCfg.Unmarshal(msg, &req)
			requests <- req
			if req.Method != "eth_subscribe" {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":true}`))
				continue
			}
			time.Sleep(delay)
			subs++
			subID := fmt.Sprintf("0x%x", subs)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":"`+subID+`"}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"`+subID+`","result":"first"}}`))
		}
	})
}

func sendSubscriptionRPC(ctx context.Context, c *WsJsonRpcClient, sub *WsSubscription, method string, params string) error {
	if sub != nil {
		ctx = WithWsSubscription(ctx, sub)
	}
	_, err := c.SendRequest(ctx, common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`)))
	return err
}

// TestWsClientDeliversNotificationRightAfterSubscribe: a notification sent
// right behind the subscribe response must reach the handler.
func TestWsClientDeliversNotificationRightAfterSubscribe(t *testing.T) {
	requests := make(chan wsTestRequest, 16)
	client := newTestWsClient(t, newSubscribeServer(t, 0, requests))

	got := make(chan string, 1)
	sub := &WsSubscription{Handler: func(params []byte) { got <- string(params) }}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, sendSubscriptionRPC(ctx, client, sub, "eth_subscribe", `["logs",{}]`))
	assert.Equal(t, "0x1", sub.ID)

	select {
	case params := <-got:
		assert.Contains(t, params, `"first"`)
	case <-time.After(2 * time.Second):
		t.Fatal("notification sent right after the subscribe response was dropped")
	}
}

// TestWsClientRejectsUnownedSubscriptionMethods: a routed eth_subscribe or
// eth_unsubscribe (no WsSubscription in ctx) would create or cancel
// connection state nobody owns; it must be skipped without reaching the
// upstream.
func TestWsClientRejectsUnownedSubscriptionMethods(t *testing.T) {
	requests := make(chan wsTestRequest, 16)
	client := newTestWsClient(t, newSubscribeServer(t, 0, requests))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, method := range []string{"eth_subscribe", "eth_unsubscribe"} {
		err := sendSubscriptionRPC(ctx, client, nil, method, `["0x1"]`)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped), "%s: got %v", method, err)
		assert.False(t, common.IsRetryableTowardsUpstream(err), method)
	}
	require.NoError(t, sendSubscriptionRPC(ctx, client, nil, "eth_chainId", `[]`))
	assert.Equal(t, "eth_chainId", (<-requests).Method, "the subscription methods must not have been sent")
}

// TestWsClientUnsubscribesAbandonedSubscribe: when the caller times out
// but the upstream still creates the subscription, the client cancels it.
func TestWsClientUnsubscribesAbandonedSubscribe(t *testing.T) {
	requests := make(chan wsTestRequest, 16)
	client := newTestWsClient(t, newSubscribeServer(t, 300*time.Millisecond, requests))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	sub := &WsSubscription{Handler: func([]byte) { t.Error("abandoned subscription delivered a notification") }}
	require.Error(t, sendSubscriptionRPC(ctx, client, sub, "eth_subscribe", `["newHeads"]`))

	assert.Equal(t, "eth_subscribe", (<-requests).Method)
	select {
	case req := <-requests:
		assert.Equal(t, "eth_unsubscribe", req.Method)
		assert.Equal(t, []interface{}{"0x1"}, req.Params)
	case <-time.After(2 * time.Second):
		t.Fatal("late subscription was never unsubscribed")
	}
}

// TestWsClientSubscriptionsAreConnectionScoped: ids restart per connection,
// so after a reconnect an old handler must not receive a reused id's
// notifications, and an old subscription must not be unsubscribed on the
// new connection.
func TestWsClientSubscriptionsAreConnectionScoped(t *testing.T) {
	compressWsLiveness(t)
	server := newFakeWsServer(t)
	client := newTestWsClient(t, server.wsURL(t))
	conn1 := <-server.newConn

	old := make(chan struct{}, 1)
	subID := subscribeNewHeads(t, client, func([]byte) { old <- struct{}{} })
	<-conn1.subscribeCh
	oldEpoch := client.Epoch()

	_ = conn1.conn.UnderlyingConn().Close()
	var conn2 *fakeWsConn
	select {
	case conn2 = <-server.newConn:
	case <-time.After(3 * time.Second):
		t.Fatal("client never re-dialed")
	}
	require.Eventually(t, client.IsConnected, time.Second, 5*time.Millisecond)
	require.NotEqual(t, oldEpoch, client.Epoch())

	conn2.sendNewHead(subID, "0x2")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := sendSubscriptionRPC(ctx, client, &WsSubscription{Epoch: oldEpoch}, "eth_unsubscribe", `["`+subID+`"]`)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamRequestSkipped), "got %v", err)
	select {
	case <-old:
		t.Fatal("handler from the previous connection received a notification")
	case <-time.After(100 * time.Millisecond):
	}
}
