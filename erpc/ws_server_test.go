package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/golang-jwt/jwt/v4"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

type wsMsg struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Method string `json:"method"`
	Params struct {
		Subscription string          `json:"subscription"`
		Result       json.RawMessage `json:"result"`
	} `json:"params"`
}

type wsClient struct {
	t    *testing.T
	c    *websocket.Conn
	msgs chan wsMsg
	done chan error
	id   int
	// pending buffers notifications read while waiting for another sub.
	pending map[string][]json.RawMessage
}

func wsURL(base, query string) string {
	u := strings.Replace(base, "http://", "ws://", 1) + "/test_project/evm/123"
	if query != "" {
		u += "?" + query
	}
	return u
}

func dialWs(t *testing.T, url string, hdr http.Header) (*wsClient, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, resp, err
	}
	c.SetReadLimit(8 << 20)
	w := &wsClient{t: t, c: c, msgs: make(chan wsMsg, 4096), done: make(chan error, 1), pending: map[string][]json.RawMessage{}}
	go func() {
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				w.done <- err
				close(w.msgs)
				return
			}
			var m wsMsg
			if err := json.Unmarshal(data, &m); err != nil {
				w.t.Errorf("invalid websocket response JSON %q: %v", data, err)
				continue
			}
			w.msgs <- m
		}
	}()
	t.Cleanup(func() { _ = c.CloseNow() })
	return w, resp, nil
}

func (w *wsClient) call(method, params string) wsMsg {
	w.t.Helper()
	w.id++
	id := fmt.Sprintf("%d", w.id)
	require.NoError(w.t, w.c.Write(context.Background(), websocket.MessageText,
		[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, id, method, params))))
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-w.msgs:
			require.True(w.t, ok, "connection closed while waiting for reply")
			if string(m.ID) == id {
				return m
			}
			if m.Method == "eth_subscription" {
				w.pending[m.Params.Subscription] = append(w.pending[m.Params.Subscription], m.Params.Result)
			}
		case <-deadline:
			w.t.Fatalf("timeout waiting for reply to %s", method)
		}
	}
}

// next returns the next notification for sub.
func (w *wsClient) next(sub string) json.RawMessage {
	w.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if q := w.pending[sub]; len(q) > 0 {
			w.pending[sub] = q[1:]
			return q[0]
		}
		select {
		case m, ok := <-w.msgs:
			require.True(w.t, ok, "connection closed while waiting for notification")
			if m.Method == "eth_subscription" {
				w.pending[m.Params.Subscription] = append(w.pending[m.Params.Subscription], m.Params.Result)
			}
		case <-deadline:
			w.t.Fatalf("timeout waiting for notification")
			return nil
		}
	}
}

func wsBlockStoreCfg(up *scriptedEvmUpstream, ws *common.WebSocketServerConfig) *common.Config {
	cfg := blockStoreTestConfig(up.URL(), &common.EvmBlockStoreConfig{
		Enabled: true, Depth: 16, PollInterval: common.Duration(100 * time.Millisecond),
	})
	cfg.Server.WebSocket = ws
	return cfg
}

// waitHead waits until the network knows head n (state pollers). The block
// store does not follow headers before a subscriber exists.
func waitHead(t *testing.T, e *ERPC, n int64) {
	prj, err := e.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.NotNil(t, nw.BlockStore())
	require.Eventually(t, func() bool { return nw.EvmHighestLatestBlockNumber(t.Context()) == n }, 10*time.Second, 50*time.Millisecond)
}

// waitFollowing waits until a subscriber started header following and the
// verified window reached head n, so blocks mined afterwards are delivered.
func waitFollowing(t *testing.T, e *ERPC, n int64) {
	t.Helper()
	hc := blockStoreOf(t, e)
	require.Eventually(t, func() bool { return hc.Head() == n }, 10*time.Second, 20*time.Millisecond)
}

func subCount(t *testing.T, e *ERPC) int {
	t.Helper()
	prj, err := e.GetProject("test_project")
	require.NoError(t, err)
	nw, err := prj.GetNetwork(t.Context(), "evm:123")
	require.NoError(t, err)
	require.NotNil(t, nw.BlockStore())
	return nw.BlockStore().SubscriberCount()
}

func TestWs_SubscriptionsReorg(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	_, _, base, shutdown, e := createServerTestFixtures(wsBlockStoreCfg(up, &common.WebSocketServerConfig{Enabled: true}), t)
	defer shutdown()
	waitHead(t, e, 20)

	w, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)

	heads := w.call("eth_subscribe", `["newHeads"]`)
	require.Nil(t, heads.Error)
	var headsId string
	require.NoError(t, json.Unmarshal(heads.Result, &headsId))

	even := w.call("eth_subscribe", fmt.Sprintf(`["logs",{"address":%q,"topics":[%q]}]`, scriptedEmitter, scriptedTopicEven))
	require.Nil(t, even.Error)
	var evenId string
	require.NoError(t, json.Unmarshal(even.Result, &evenId))
	require.NotEqual(t, headsId, evenId)
	waitFollowing(t, e, 20)

	// New block 21 (odd topic) and 22 (even topic).
	up.Mine(2)
	h := w.next(headsId)
	require.NotContains(t, string(h), `"transactions"`)
	require.Contains(t, string(h), `"number":"0x15"`)
	require.Contains(t, string(w.next(headsId)), `"number":"0x16"`)
	l := w.next(evenId)
	require.Contains(t, string(l), `"blockNumber":"0x16"`, "odd-topic log at 21 must be filtered out")
	require.Contains(t, string(l), `"removed":false`)

	// Reorg 22 away: removed:true for the orphan log first, then the new one.
	orphan := up.HashAt(22)
	up.Reorg(22, "b")
	up.Mine(1)
	rem := w.next(evenId)
	require.Contains(t, string(rem), orphan)
	require.Contains(t, string(rem), `"removed":true`)
	add := w.next(evenId)
	require.Contains(t, string(add), up.HashAt(22))
	require.Contains(t, string(add), `"removed":false`)

	// Unsubscribe is connection-scoped.
	other, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	require.Equal(t, "false", string(other.call("eth_unsubscribe", fmt.Sprintf(`[%q]`, headsId)).Result))
	require.Equal(t, "true", string(w.call("eth_unsubscribe", fmt.Sprintf(`[%q]`, headsId)).Result))
	require.Equal(t, "false", string(w.call("eth_unsubscribe", fmt.Sprintf(`[%q]`, headsId)).Result))

	// Unsupported kinds and bad filters are JSON-RPC errors, not disconnects.
	bad := w.call("eth_subscribe", `["newPendingTransactions"]`)
	require.NotNil(t, bad.Error)
	// Geth: subscriptionNotFoundError is -32601.
	require.Equal(t, -32601, bad.Error.Code)
	// Invalid or unsupported filter members are -32602 and register nothing:
	// only address and topics are streamed.
	for _, params := range []string{
		`["logs",{"topics":[1]}]`,
		`["logs","notanobject"]`,
		fmt.Sprintf(`["logs",{"address":%q,"blockHash":%q}]`, scriptedEmitter, "0x"+strings.Repeat("1", 64)),
		`["logs",{"fromBlock":"latest"}]`,
		`["logs",{"toBlock":"0x1"}]`,
		`["logs",{"fromBlock":"0x1","toBlock":"0x2","topics":[]}]`,
		`["logs",{"someFutureMember":true}]`,
	} {
		bad = w.call("eth_subscribe", params)
		require.NotNil(t, bad.Error, params)
		require.Equal(t, -32602, bad.Error.Code, params)
	}

	// Disconnect releases head cache subscriptions.
	require.Equal(t, 1, subCount(t, e))
	_ = w.c.Close(websocket.StatusNormalClosure, "")
	require.Eventually(t, func() bool { return subCount(t, e) == 0 }, 5*time.Second, 20*time.Millisecond)
}

func TestWs_AuthOriginAndDisabled(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsBlockStoreCfg(up, &common.WebSocketServerConfig{Enabled: true, PingInterval: common.Duration(200 * time.Millisecond)})
	cfg.Projects[0].Auth = &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
		{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "s1", Value: "s3cret"}},
	}}
	cfg.Projects[0].CORS = &common.CORSConfig{AllowedOrigins: []string{"https://good.example"}}
	_, _, base, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	_, resp, err := dialWs(t, wsURL(base, ""), nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	_, resp, err = dialWs(t, wsURL(base, "secret=wrong"), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	_, resp, err = dialWs(t, wsURL(base, "secret=s3cret"), http.Header{"Origin": {"https://evil.example"}})
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	w, _, err := dialWs(t, wsURL(base, "secret=s3cret"), http.Header{"Origin": {"https://good.example"}})
	require.NoError(t, err)
	r := w.call("eth_subscribe", `["newHeads"]`)
	require.Nil(t, r.Error)

	w2, _, err := dialWs(t, wsURL(base, ""), http.Header{"X-ERPC-Secret-Token": {"s3cret"}})
	require.NoError(t, err)
	require.Nil(t, w2.call("eth_subscribe", `["newHeads"]`).Error)
}

func TestWs_DisabledAndNoBlockStore(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()

	// WS disabled: an Upgrade request is handled as plain HTTP (no 101).
	cfg := wsBlockStoreCfg(up, nil)
	_, _, base, shutdown, _ := createServerTestFixtures(cfg, t)
	_, resp, err := dialWs(t, wsURL(base, ""), nil)
	require.Error(t, err)
	if resp != nil {
		require.NotEqual(t, http.StatusSwitchingProtocols, resp.StatusCode)
	}
	shutdown()

	// WS enabled but no head cache for the network: the upgrade is refused
	// before accepting, so no connection can fall back to upstream calls.
	cfg2 := blockStoreTestConfig(up.URL(), nil)
	cfg2.Server.WebSocket = &common.WebSocketServerConfig{Enabled: true}
	_, _, base2, shutdown2, _ := createServerTestFixtures(cfg2, t)
	defer shutdown2()
	_, resp, err = dialWs(t, wsURL(base2, ""), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestWs_ConnectionMetricsTrackLifecycleAndBoundCloseReason(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	_, _, base, shutdown, e := createServerTestFixtures(wsBlockStoreCfg(up, &common.WebSocketServerConfig{Enabled: true}), t)
	defer shutdown()
	waitHead(t, e, 20)

	const project, network = "test_project", "evm:123"
	connections := telemetry.MetricWebSocketConnections.WithLabelValues(project, network)
	peerClosures := telemetry.MetricWebSocketClosuresTotal.WithLabelValues(project, network, "peer")
	initialConnections := testutil.ToFloat64(connections)
	initialPeerClosures := testutil.ToFloat64(peerClosures)
	w, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return testutil.ToFloat64(connections) == initialConnections+1 }, 5*time.Second, 10*time.Millisecond)

	// The peer-provided text is intentionally untrusted and must not become a metric label.
	require.NoError(t, w.c.Close(websocket.StatusNormalClosure, "client supplied close detail"))
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(connections) == initialConnections && testutil.ToFloat64(peerClosures) == initialPeerClosures+1
	}, 5*time.Second, 10*time.Millisecond)
}

func TestWs_Caps(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsBlockStoreCfg(up, &common.WebSocketServerConfig{
		Enabled: true, MaxConnections: 2, MaxSubscriptionsPerConnection: 1, MaxMessageBytes: 1024,
	})
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)

	a, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	b, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	_, resp, err := dialWs(t, wsURL(base, ""), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	require.Nil(t, a.call("eth_subscribe", `["newHeads"]`).Error)
	over := a.call("eth_subscribe", `["newHeads"]`)
	require.NotNil(t, over.Error)
	require.Contains(t, over.Error.Message, "too many subscriptions")

	// Oversized frame closes the connection with 1009.
	big := `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["logs",{"address":"` + strings.Repeat("a", 2048) + `"}]}`
	require.NoError(t, b.c.Write(context.Background(), websocket.MessageText, []byte(big)))
	select {
	case err := <-b.done:
		require.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(err))
	case <-time.After(5 * time.Second):
		t.Fatal("oversized message did not close the connection")
	}

	// A closed slot can be reused.
	require.Eventually(t, func() bool {
		c, _, err := dialWs(t, wsURL(base, ""), nil)
		if err != nil {
			return false
		}
		_ = c.c.CloseNow()
		return true
	}, 5*time.Second, 50*time.Millisecond)
}

func TestWs_SlowConsumerDisconnected(t *testing.T) {
	// Kernel socket buffers make an end-to-end overflow nondeterministic,
	// so exercise the bounded queue directly: a full queue must cancel the
	// connection with 1008 instead of blocking or buffering without bound.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	c := &wsConn{ctx: ctx, cancel: cancel, out: make(chan []byte, 1)}
	c.send([]byte("a"))
	require.NoError(t, ctx.Err())
	c.send([]byte("b"))
	require.Error(t, ctx.Err())
	var ce *wsCloseErr
	require.ErrorAs(t, context.Cause(ctx), &ce)
	require.Equal(t, websocket.StatusPolicyViolation, ce.code)
	require.Contains(t, ce.reason, wsCloseSlowConsumer)
	// Sends after close are dropped without panicking.
	c.send([]byte("c"))
}

func testWsConn(t *testing.T, queue int) (*wsConn, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return &wsConn{
		ctx: ctx, cancel: cancel, out: make(chan []byte, queue),
		ws: &wsServer{cfg: &common.WebSocketServerConfig{WriteTimeout: common.Duration(2 * time.Second)}},
	}, cancel
}

func manyLogsRecord(n int64, logs int) *blockstore.BlockRecord {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < logs; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"address":%q,"topics":[%q],"data":"0x","blockNumber":"0x%x","blockHash":"0xh%d","transactionHash":"0xt","transactionIndex":"0x0","logIndex":"0x%x","removed":false}`,
			scriptedEmitter, scriptedTopicEven, n, n, i)
	}
	b.WriteByte(']')
	return &blockstore.BlockRecord{Number: n, Hash: fmt.Sprintf("0xh%d", n), Block: []byte(`{}`), Logs: []byte(b.String())}
}

func TestWs_BusyBlockDeliveredToReadingClient(t *testing.T) {
	c, _ := testWsConn(t, 16)
	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for got < 1000 {
			select {
			case <-c.out:
				got++
			case <-c.ctx.Done():
				return
			}
		}
	}()
	s := &wsSub{id: "0x1", logs: true}
	require.NoError(t, c.emit(s, blockstore.Event{Added: []*blockstore.BlockRecord{manyLogsRecord(10, 1000)}}))
	<-done
	require.NoError(t, c.ctx.Err(), "a reading client must not be disconnected")
	require.Equal(t, 1000, got)
}

func TestWs_UnsubscribeDiscardsBufferedEvents(t *testing.T) {
	c, _ := testWsConn(t, 4)
	s := &wsSub{id: "0x1", sub: &blockstore.Subscription{C: make(chan blockstore.Event, 2)}}
	c.subs = map[string]*wsSub{s.id: s}
	s.sub.C <- blockstore.Event{Added: []*blockstore.BlockRecord{manyLogsRecord(10, 0)}}
	s.sub.C <- blockstore.Event{Added: []*blockstore.BlockRecord{manyLogsRecord(11, 0)}}

	reply := c.unsubscribe(&wsRequest{ID: json.RawMessage(`1`), Params: json.RawMessage(`["0x1"]`)})
	require.Contains(t, string(reply), `"result":true`)
	require.True(t, c.send(reply))
	c.wg.Add(1)
	go c.pump(s)
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscription pump did not stop after unsubscribe")
	}
	require.Equal(t, reply, <-c.out)
	select {
	case notification := <-c.out:
		t.Fatalf("notification delivered after unsubscribe reply: %s", notification)
	default:
	}
}

func TestWs_StreamStalledClientDisconnected(t *testing.T) {
	c, _ := testWsConn(t, 4)
	c.ws.cfg.WriteTimeout = common.Duration(100 * time.Millisecond)
	s := &wsSub{id: "0x1", logs: true}
	require.NoError(t, c.emit(s, blockstore.Event{Added: []*blockstore.BlockRecord{manyLogsRecord(10, 50)}}))
	var ce *wsCloseErr
	require.ErrorAs(t, context.Cause(c.ctx), &ce)
	require.Equal(t, websocket.StatusPolicyViolation, ce.code)
}

// wsLoopbackConn returns a server-side *websocket.Conn wired to a real client
// over an httptest server, plus the client end.
func wsLoopbackConn(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	srvCh := make(chan *websocket.Conn, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		srvCh <- c
		<-r.Context().Done()
	}))
	t.Cleanup(hs.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cl, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.CloseNow() })
	return <-srvCh, cl
}

func TestWs_WriteLoopLabelsOnlyRealTimeouts(t *testing.T) {
	// writeLoop's shutdown path reads the server app context and logger.
	wire := func(c *wsConn) {
		lg := zerolog.Nop()
		c.lg = &lg
		c.ws.s = &HttpServer{appCtx: context.Background()}
	}
	t.Run("peer gone is not a write timeout", func(t *testing.T) {
		srv, cl := wsLoopbackConn(t)
		c, _ := testWsConn(t, 4)
		c.conn = srv
		wire(c)
		require.NoError(t, cl.CloseNow()) // abrupt peer loss
		_ = srv.CloseNow()                // writes now fail immediately, well before the deadline
		done := make(chan struct{})
		go c.writeLoop(done)
		c.out <- []byte(`{"jsonrpc":"2.0"}`)
		require.Eventually(t, func() bool { return c.ctx.Err() != nil }, 5*time.Second, 10*time.Millisecond)
		<-done
		require.NotEqual(t, "write_timeout", websocketCloseReason(context.Cause(c.ctx), context.Background()))
		var ce *wsCloseErr
		require.False(t, errors.As(context.Cause(c.ctx), &ce), "a transport error is not reported as a policy close")
	})
	t.Run("stalled reader is a write timeout", func(t *testing.T) {
		srv, _ := wsLoopbackConn(t) // client never reads
		c, _ := testWsConn(t, 64)
		c.conn = srv
		wire(c)
		c.ws.cfg.WriteTimeout = common.Duration(100 * time.Millisecond)
		done := make(chan struct{})
		go c.writeLoop(done)
		big := []byte(`"` + strings.Repeat("x", 1<<20) + `"`)
		go func() {
			for c.ctx.Err() == nil {
				select {
				case c.out <- big:
				case <-c.ctx.Done():
				}
			}
		}()
		require.Eventually(t, func() bool { return c.ctx.Err() != nil }, 10*time.Second, 10*time.Millisecond)
		<-done
		require.Equal(t, "write_timeout", websocketCloseReason(context.Cause(c.ctx), context.Background()))
	})
}

func TestWs_ColdFillKeepsSubscriptionOpen(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsBlockStoreCfg(up, &common.WebSocketServerConfig{Enabled: true})
	// One backfilled header per 100ms tick: the 16-block window takes ~1.6s to fill.
	cfg.Projects[0].Networks[0].Evm.BlockStore.MaxPerTick = 1
	_, _, base, shutdown, e := createServerTestFixtures(cfg, t)
	defer shutdown()
	waitHead(t, e, 20)

	w, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	var id string
	require.NoError(t, json.Unmarshal(w.call("eth_subscribe", `["newHeads"]`).Result, &id))
	waitFollowing(t, e, 20)

	// Backfill below the delivered range and new heads above it are both
	// continuous; neither may close the stream while the window fills.
	for n := 21; n <= 24; n++ {
		up.Mine(1)
		require.Contains(t, string(w.next(id)), fmt.Sprintf(`"number":"0x%x"`, n))
		time.Sleep(500 * time.Millisecond)
	}
	select {
	case err := <-w.done:
		t.Fatalf("subscription closed during cold fill: %v", err)
	default:
	}
	require.Equal(t, 1, subCount(t, e))
}

func TestWs_GapDetection(t *testing.T) {
	rec := func(n int64) *blockstore.BlockRecord { return &blockstore.BlockRecord{Number: n} }
	ev := func(rem []int64, add ...int64) blockstore.Event {
		e := blockstore.Event{}
		for _, n := range rem {
			e.Removed = append(e.Removed, rec(n))
		}
		for _, n := range add {
			e.Added = append(e.Added, rec(n))
		}
		return e
	}
	require.NoError(t, checkContinuity(0, ev(nil, 50, 51)))
	require.NoError(t, checkContinuity(10, ev(nil, 11, 12)))
	require.ErrorIs(t, checkContinuity(10, ev(nil, 12)), errWsGap)            // skipped 11
	require.ErrorIs(t, checkContinuity(10, ev(nil, 11, 13)), errWsGap)        // hole inside
	require.ErrorIs(t, checkContinuity(10, ev(nil, 100)), errWsGap)           // window jump
	require.NoError(t, checkContinuity(10, ev([]int64{10, 9}, 9, 10, 11)))    // reorg rewind
	require.NoError(t, checkContinuity(10, ev([]int64{10}, 10)))              // same-height
	require.ErrorIs(t, checkContinuity(10, ev([]int64{10, 9}, 10)), errWsGap) // rewind mismatch
	require.ErrorIs(t, checkContinuity(10, ev([]int64{12}, 12)), errWsGap)    // removed beyond seen

	// emit closes via pump on gap; emit itself reports it without sending.
	c, _ := testWsConn(t, 16)
	s := &wsSub{id: "0x1"}
	require.NoError(t, c.emit(s, blockstore.Event{Added: []*blockstore.BlockRecord{manyLogsRecord(5, 0)}}))
	require.ErrorIs(t, c.emit(s, blockstore.Event{Added: []*blockstore.BlockRecord{manyLogsRecord(7, 0)}}), errWsGap)
}

func TestWs_PingAndReauthClosesExpiredJwt(t *testing.T) {
	const key = "ws-test-hmac"
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsBlockStoreCfg(up, &common.WebSocketServerConfig{Enabled: true, PingInterval: common.Duration(200 * time.Millisecond)})
	cfg.Projects[0].Auth = &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
		{Type: common.AuthTypeJwt, Jwt: &common.JwtStrategyConfig{
			VerificationKeys: map[string]string{"default": key}, AllowedAlgorithms: []string{"HS256"},
		}},
	}}
	_, _, base, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()
	sign := func(exp time.Duration) http.Header {
		tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": "u", "exp": time.Now().Add(exp).Unix(),
		}).SignedString([]byte(key))
		require.NoError(t, err)
		return http.Header{"Authorization": {"Bearer " + tok}}
	}

	// Long-lived token: survives several ping+reauth rounds.
	long, _, err := dialWs(t, wsURL(base, ""), sign(time.Hour))
	require.NoError(t, err)
	require.Nil(t, long.call("eth_subscribe", `["newHeads"]`).Error)

	// Token expiring in ~2s: accepted at upgrade, closed after exp.
	short, _, err := dialWs(t, wsURL(base, ""), sign(2*time.Second))
	require.NoError(t, err)
	require.Nil(t, short.call("eth_subscribe", `["newHeads"]`).Error)
	select {
	case err := <-short.done:
		require.Equal(t, websocket.StatusPolicyViolation, websocket.CloseStatus(err))
	case <-time.After(8 * time.Second):
		t.Fatal("expired JWT kept streaming")
	}
	require.Nil(t, long.call("eth_subscribe", `["newHeads"]`).Error)
}

func TestWs_PingWaitsForReaderAfterSlowFrame(t *testing.T) {
	const writeTimeout = 50 * time.Millisecond
	const pingInterval = 300 * time.Millisecond
	started := make(chan struct{})
	state := make(chan context.Context, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		c := &wsConn{conn: conn, ctx: ctx, cancel: cancel, project: &PreparedProject{}, req: r,
			ws: &wsServer{cfg: &common.WebSocketServerConfig{WriteTimeout: common.Duration(writeTimeout), PingInterval: common.Duration(pingInterval)}, s: &HttpServer{}}}
		_, _, err = conn.Read(context.Background())
		if err != nil {
			return
		}
		state <- ctx
		close(started)
		go c.pingLoop()
		// While this frame handler runs, Read cannot consume the pong.
		time.Sleep(420 * time.Millisecond)
		if ctx.Err() != nil {
			return
		}
		for ctx.Err() == nil {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, strings.Replace(server.URL, "http://", "ws://", 1), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	go func() {
		for {
			if _, _, err := client.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	require.NoError(t, client.Write(ctx, websocket.MessageText, []byte("frame")))
	<-started
	serverCtx := <-state
	// The first ping fires at 300ms and must survive the extra 120ms of
	// frame handling, which exceeds WriteTimeout but not PingInterval.
	time.Sleep(460 * time.Millisecond)
	require.NoError(t, serverCtx.Err(), "slow frame handling must not trigger a ping timeout")
}

func TestWs_PerProjectCap(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	cfg := wsBlockStoreCfg(up, &common.WebSocketServerConfig{Enabled: true, MaxConnections: 10, MaxConnectionsPerProject: 1})
	other := wsBlockStoreCfg(up, nil).Projects[0]
	other.Id = "other"
	cfg.Projects = append(cfg.Projects, other)
	_, _, base, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()
	a, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	_, resp, err := dialWs(t, wsURL(base, ""), nil)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	// Another project is unaffected.
	_, _, err = dialWs(t, strings.Replace(wsURL(base, ""), "/test_project/", "/other/", 1), nil)
	require.NoError(t, err)
	// Slot released on close.
	_ = a.c.Close(websocket.StatusNormalClosure, "")
	require.Eventually(t, func() bool {
		c, _, err := dialWs(t, wsURL(base, ""), nil)
		if err != nil {
			return false
		}
		_ = c.c.CloseNow()
		return true
	}, 5*time.Second, 50*time.Millisecond)
}

// newHeads needs only headers; per-block logs are fetched only while a logs
// subscription exists, once per block regardless of how many subscribe.
func TestWs_LogsFetchedOnlyForLogsSubscribers(t *testing.T) {
	up := newScriptedEvmUpstream(123, 20)
	defer up.Close()
	_, _, base, shutdown, e := createServerTestFixtures(wsBlockStoreCfg(up, &common.WebSocketServerConfig{Enabled: true}), t)
	defer shutdown()
	waitHead(t, e, 20)
	hc := blockStoreOf(t, e)

	w, _, err := dialWs(t, wsURL(base, ""), nil)
	require.NoError(t, err)
	var headsID string
	require.NoError(t, json.Unmarshal(w.call("eth_subscribe", `["newHeads"]`).Result, &headsID))
	waitFollowing(t, e, 20)
	for n := 21; n <= 23; n++ {
		up.Mine(1)
		require.Contains(t, string(w.next(headsID)), fmt.Sprintf(`"number":"0x%x"`, n))
	}
	require.Zero(t, up.BlockHashLogCalls(), "newHeads subscribers fetch no logs")
	require.Zero(t, up.FullBlockCalls(), "newHeads subscribers fetch no block bodies")
	require.Zero(t, hc.LogsSubscriberCount())

	// Two logs subscriptions (different filters): one logs fetch per block.
	var allID, evenID string
	require.NoError(t, json.Unmarshal(w.call("eth_subscribe", `["logs",{}]`).Result, &allID))
	require.NoError(t, json.Unmarshal(w.call("eth_subscribe", fmt.Sprintf(`["logs",{"topics":[%q]}]`, scriptedTopicEven)).Result, &evenID))
	require.Equal(t, 2, hc.LogsSubscriberCount())
	for n := 24; n <= 27; n++ {
		up.Mine(1)
		require.Contains(t, string(w.next(headsID)), fmt.Sprintf(`"number":"0x%x"`, n))
		require.Contains(t, string(w.next(allID)), fmt.Sprintf(`"blockNumber":"0x%x"`, n))
		if n%2 == 0 {
			require.Contains(t, string(w.next(evenID)), fmt.Sprintf(`"blockNumber":"0x%x"`, n))
		}
	}
	require.Equal(t, int64(4), up.BlockHashLogCalls(), "one logs fetch per new block, shared by both subscriptions")
	require.Zero(t, up.FullBlockCalls())

	// After unsubscribing, logs fetching stops.
	require.Equal(t, "true", string(w.call("eth_unsubscribe", fmt.Sprintf(`[%q]`, allID)).Result))
	require.Equal(t, "true", string(w.call("eth_unsubscribe", fmt.Sprintf(`[%q]`, evenID)).Result))
	require.Eventually(t, func() bool { return hc.LogsSubscriberCount() == 0 }, 5*time.Second, 20*time.Millisecond)
	for n := 28; n <= 30; n++ {
		up.Mine(1)
		require.Contains(t, string(w.next(headsID)), fmt.Sprintf(`"number":"0x%x"`, n))
	}
	require.Equal(t, int64(4), up.BlockHashLogCalls(), "no logs subscribers, no logs fetches")
}
