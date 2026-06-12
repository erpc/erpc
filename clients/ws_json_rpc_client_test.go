package clients

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWsServer is a minimal JSON-RPC WebSocket upstream. Each accepted
// connection can be "black-holed": the TCP connection stays open but pings
// are swallowed (no pong reply) and nothing is ever written — exactly what
// an intermediate proxy does when the real upstream pod vanishes without a
// FIN/RST. This is the failure mode from the 2026-06-12 zkSync incident:
// the old client believed such a connection was healthy forever.
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

func subscribeNewHeads(t *testing.T, c *WsJsonRpcClient) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	nq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`))
	resp, err := c.SendRequest(ctx, nq)
	require.NoError(t, err)
	jr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	subID := strings.Trim(string(jr.GetResultBytes()), "\"")
	require.NotEmpty(t, subID)
	return subID
}

// TestWsClientDetectsSilentPeerAndReconnects is the regression test for the
// 2026-06-12 zkSync incident: the upstream socket dies WITHOUT a close
// handshake (peer keeps TCP open but stops responding — equivalent to a
// proxy black-holing frames after the real upstream pod was deleted). The
// client must declare the connection dead via the ping/pong liveness
// deadline, re-dial, and resume delivering subscription notifications.
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
	subID1 := subscribeNewHeads(t, client)

	heads := make(chan []byte, 16)
	client.RegisterSubscriptionHandler(subID1, func(params []byte) {
		heads <- params
	})
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

	// Re-subscribe on the new connection (in production the wsupstream
	// adapter does this from its reconnect hook) and verify notifications
	// flow again.
	subID2 := subscribeNewHeads(t, client)
	client.RegisterSubscriptionHandler(subID2, func(params []byte) {
		heads <- params
	})
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
