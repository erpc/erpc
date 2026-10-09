package wsupstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test doubles -------------------------------------------------------

type fakeNetworkHandle struct{}

func (fakeNetworkHandle) Id() string                            { return "evm:123" }
func (fakeNetworkHandle) SuggestLatestBlock(string, int64) bool { return true }

type fakeSink struct {
	events chan indexer.StreamEvent
}

func (s *fakeSink) Ingest(ev indexer.StreamEvent) { s.events <- ev }

// notifyServer is a WS upstream that answers eth_subscribe with ids from a
// per-connection counter ("0x1", "0x2", …, as real nodes do), follows each
// logs subscribe response with one notification for it, answers any other
// method with true, and can push notification frames.
type notifyServer struct {
	srv     *httptest.Server
	newConn chan *notifyConn
	// gate, when set, is received from after each eth_subscribe is
	// recorded and before it is answered.
	gate chan struct{}
}

type notifyConn struct {
	conn     *websocket.Conn
	writeMu  sync.Mutex
	requests chan notifyRequest
}

type notifyRequest struct {
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
	// SubID is the id this connection assigned (eth_subscribe only).
	SubID string `json:"-"`
}

func newNotifyServer(t *testing.T) *notifyServer {
	n := &notifyServer{newConn: make(chan *notifyConn, 16)}
	upgrader := websocket.Upgrader{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		nc := &notifyConn{conn: conn, requests: make(chan notifyRequest, 64)}
		n.newConn <- nc
		go nc.serve(n.gate)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *notifyServer) wsURL(t *testing.T) *url.URL {
	u, err := url.Parse(n.srv.URL)
	require.NoError(t, err)
	u.Scheme = "ws"
	return u
}

func (nc *notifyConn) serve(gate chan struct{}) {
	subs := 0
	for {
		_, msg, err := nc.conn.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			notifyRequest
			ID json.RawMessage `json:"id"`
		}
		if common.SonicCfg.Unmarshal(msg, &req) != nil {
			continue
		}
		if req.Method != methodEthSubscribe {
			nc.write(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":true}`)
			nc.requests <- req.notifyRequest
			continue
		}
		subs++
		req.SubID = fmt.Sprintf("0x%x", subs)
		nc.requests <- req.notifyRequest
		if gate != nil {
			<-gate
		}
		nc.write(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":"` + req.SubID + `"}`)
		if req.Params[0] == indexer.SubTypeLogs {
			nc.sendLog(req.SubID, 1)
		}
	}
}

func (nc *notifyConn) write(frame string) {
	nc.writeMu.Lock()
	defer nc.writeMu.Unlock()
	_ = nc.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = nc.conn.WriteMessage(websocket.TextMessage, []byte(frame))
}

func (nc *notifyConn) sendNewHead(subID string, num int64) {
	nc.write(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":%q,"result":{"number":"0x%x","hash":"0xhash%x","parentHash":"0xhash%x"}}}`,
		subID, num, num, num-1))
}

func (nc *notifyConn) sendLog(subID string, num int64) {
	nc.write(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":%q,"result":{"blockNumber":"0x%x","blockHash":"0xhash%x"}}}`,
		subID, num, num))
}

// nextRequest returns the next request with method, skipping others.
func (nc *notifyConn) nextRequest(t *testing.T, method string) notifyRequest {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case req := <-nc.requests:
			if req.Method == method {
				return req
			}
		case <-timeout:
			t.Fatalf("server never received %s", method)
		}
	}
}

func compressResubRetry(t *testing.T) {
	origMin, origMax := resubRetryMin, resubRetryMax
	resubRetryMin = 20 * time.Millisecond
	resubRetryMax = 100 * time.Millisecond
	t.Cleanup(func() { resubRetryMin, resubRetryMax = origMin, origMax })
}

// newTestAdapter wires an adapter to a real WS client for server. Its
// forward sends straight through the client (the real one rides
// upstream.Forward).
func newTestAdapter(t *testing.T, u *url.URL) (*Adapter, *fakeSink) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel)
	ci, err := clients.NewWsJsonRpcClient(ctx, &logger, "test-project", common.NewFakeUpstream("test-ws-upstream"), u, nil, nil)
	require.NoError(t, err)
	wsc := ci.(*clients.WsJsonRpcClient)
	a := &Adapter{
		upstreamID: "test-ws-upstream",
		networkID:  "evm:123",
		wsClient:   wsc,
		logger:     &logger,
		filters:    make(map[string]*filterSub),
		forward: func(ctx context.Context, nq *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
			return wsc.SendRequest(ctx, nq)
		},
		retryMin:       resubRetryMin,
		retryMax:       resubRetryMax,
		attemptTimeout: resubAttemptTimeout,
	}
	return a, &fakeSink{events: make(chan indexer.StreamEvent, 64)}
}

// subscribedHeads reports whether a is connected with a live newHeads
// subscription.
func subscribedHeads(a *Adapter) func() bool {
	return func() bool {
		if !a.wsClient.IsConnected() {
			return false
		}
		a.subsMu.Lock()
		defer a.subsMu.Unlock()
		return a.heads.id != ""
	}
}

func logsParams(address string) []interface{} {
	return []interface{}{indexer.SubTypeLogs, map[string]interface{}{"address": address}}
}

func nextEvent(t *testing.T, sink *fakeSink, kind indexer.EventKind) indexer.StreamEvent {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev := <-sink.events:
			if ev.Kind == kind {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %v event delivered", kind)
		}
	}
}

// --- tests ----------------------------------------------------------------

// TestAdapterResubscribesWithRetryAfterReconnect: a failed resubscribe must
// be retried rather than leave the adapter without heads until the next
// reconnect.
func TestAdapterResubscribesWithRetryAfterReconnect(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn1 := <-server.newConn

	var forwardCalls, failuresLeft atomic.Int64
	const failuresPerEpoch = 2
	failuresLeft.Store(failuresPerEpoch)
	send := a.forward
	a.forward = func(ctx context.Context, nq *common.NormalizedRequest, bypass bool) (*common.NormalizedResponse, error) {
		forwardCalls.Add(1)
		if failuresLeft.Add(-1) >= 0 {
			return nil, errors.New("circuit breaker is open on upstream-level")
		}
		return send(ctx, nq, bypass)
	}

	require.False(t, a.HeadsLive(), "no heads before the subscribe succeeds")
	require.NoError(t, a.Start(context.Background(), fakeNetworkHandle{}, sink))

	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond,
		"adapter never subscribed newHeads despite the failures stopping after %d", failuresPerEpoch)
	require.True(t, a.HeadsLive())
	require.GreaterOrEqual(t, forwardCalls.Load(), int64(failuresPerEpoch+1),
		"expected the subscribe to be retried through failures")

	conn1.sendNewHead(conn1.nextRequest(t, methodEthSubscribe).SubID, 100)
	require.Equal(t, int64(100), nextEvent(t, sink, indexer.KindNewHead).Block.Number)

	// Ungraceful upstream death: kill the TCP connection with no close
	// handshake. The adapter must clear its stale subscription, then retry
	// the resubscribe through a fresh round of failures.
	failuresLeft.Store(failuresPerEpoch)
	_ = conn1.conn.UnderlyingConn().Close()
	var conn2 *notifyConn
	select {
	case conn2 = <-server.newConn:
	case <-time.After(3 * time.Second):
		t.Fatal("client never re-dialed after the upstream connection was killed")
	}

	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond,
		"adapter never re-established the newHeads subscription after reconnect")
	conn2.sendNewHead(conn2.nextRequest(t, methodEthSubscribe).SubID, 101)
	require.Equal(t, int64(101), nextEvent(t, sink, indexer.KindNewHead).Block.Number)
}

// TestAdapterResubscribesAllFiltersAfterReconnect: subscription ids come
// from a per-connection counter, so the new connection reuses the old
// ids for different subscriptions. Every filter must deliver after the
// resubscribe, and each must be subscribed exactly once.
func TestAdapterResubscribesAllFiltersAfterReconnect(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn1 := <-server.newConn
	require.NoError(t, a.Start(context.Background(), fakeNetworkHandle{}, sink))
	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond)

	const numFilters = 8
	for i := 0; i < numFilters; i++ {
		params := logsParams(fmt.Sprintf("0x%x", i))
		require.NoError(t, a.EnsureFilter(context.Background(), indexer.SubTypeLogs, indexer.BuildParamsKey(params), params))
	}

	_ = conn1.conn.UnderlyingConn().Close()
	conn2 := <-server.newConn
	subscribes := make(map[string]string) // sub id -> first param
	for i := 0; i < numFilters+1; i++ {
		req := conn2.nextRequest(t, methodEthSubscribe)
		subscribes[req.SubID] = fmt.Sprint(req.Params[0])
	}
	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond)

	for id, subType := range subscribes {
		if subType == indexer.SubTypeLogs {
			conn2.sendLog(id, 2)
		}
	}
	delivered := make(map[string]bool)
	for len(delivered) < numFilters {
		delivered[nextEvent(t, sink, indexer.KindLog).FilterHash] = true
	}

	select {
	case req := <-conn2.requests:
		if req.Method == methodEthSubscribe {
			t.Fatalf("unexpected extra subscribe after resubscribe: %v", req.Params)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAdapterDeliversFirstNotificationAfterSubscribe: the upstream sends a
// log right behind the subscribe response; it must not be dropped.
func TestAdapterDeliversFirstNotificationAfterSubscribe(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	<-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink

	params := logsParams("0xabc")
	require.NoError(t, a.EnsureFilter(context.Background(), indexer.SubTypeLogs, indexer.BuildParamsKey(params), params))
	assert.Equal(t, indexer.BuildParamsKey(params), nextEvent(t, sink, indexer.KindLog).FilterHash)
}

// TestAdapterRemoveFilterDuringSubscribe: RemoveFilter while the subscribe
// is in flight must not leave the late subscription behind upstream.
func TestAdapterRemoveFilterDuringSubscribe(t *testing.T) {
	server := newNotifyServer(t)
	server.gate = make(chan struct{})
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink

	params := logsParams("0xabc")
	hash := indexer.BuildParamsKey(params)
	ensured := make(chan error, 1)
	go func() { ensured <- a.EnsureFilter(context.Background(), indexer.SubTypeLogs, hash, params) }()
	subID := conn.nextRequest(t, methodEthSubscribe).SubID

	require.NoError(t, a.RemoveFilter(context.Background(), indexer.SubTypeLogs, hash))
	server.gate <- struct{}{}
	require.NoError(t, <-ensured)

	unsub := conn.nextRequest(t, methodEthUnsubscribe)
	assert.Equal(t, []interface{}{subID}, unsub.Params)
	a.subsMu.Lock()
	assert.Empty(t, a.filters)
	a.subsMu.Unlock()
}

// TestAdapterEnsureFilterWhileDisconnected: EnsureFilter must report the
// failure (so the indexer tries other ingresses) and keep the filter, to
// subscribe it once connected.
func TestAdapterEnsureFilterWhileDisconnected(t *testing.T) {
	server := newNotifyServer(t)
	u := server.wsURL(t)
	server.srv.Close()
	a, _ := newTestAdapter(t, u)

	params := logsParams("0xabc")
	hash := indexer.BuildParamsKey(params)
	err := a.EnsureFilter(context.Background(), indexer.SubTypeLogs, hash, params)
	require.ErrorIs(t, err, errNotConnected)
	assert.False(t, a.FilterLive(indexer.SubTypeLogs, hash))
	a.subsMu.Lock()
	assert.Len(t, a.filters, 1)
	a.subsMu.Unlock()
}

// TestAdapterRetriesFailedEnsureFilter: a filter whose subscribe failed on
// a live connection is retried in the background until it is live, and
// RemoveFilter ends that.
func TestAdapterRetriesFailedEnsureFilter(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	<-server.newConn
	require.NoError(t, a.Start(context.Background(), fakeNetworkHandle{}, sink))
	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond)

	var failuresLeft atomic.Int64
	failuresLeft.Store(2)
	send := a.forward
	a.forward = func(ctx context.Context, nq *common.NormalizedRequest, bypass bool) (*common.NormalizedResponse, error) {
		if m, _ := nq.Method(); m == methodEthSubscribe && failuresLeft.Add(-1) >= 0 {
			return nil, errors.New("circuit breaker is open on upstream-level")
		}
		return send(ctx, nq, bypass)
	}

	params := logsParams("0xabc")
	hash := indexer.BuildParamsKey(params)
	require.Error(t, a.EnsureFilter(context.Background(), indexer.SubTypeLogs, hash, params))
	require.Eventually(t, func() bool { return a.FilterLive(indexer.SubTypeLogs, hash) },
		3*time.Second, 10*time.Millisecond, "the failed filter must be retried until live")

	require.NoError(t, a.RemoveFilter(context.Background(), indexer.SubTypeLogs, hash))
	assert.False(t, a.FilterLive(indexer.SubTypeLogs, hash))
}

// Cancelling a subscription upstream must not depend on the caller's
// context: a client that unsubscribes and disconnects at once still releases
// the upstream subscription.
func TestAdapterRemoveFilterWithCancelledContextUnsubscribes(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink
	// Like the real path, refuse to send once the context is done.
	send := a.forward
	a.forward = func(ctx context.Context, nq *common.NormalizedRequest, bypass bool) (*common.NormalizedResponse, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return send(ctx, nq, bypass)
	}

	params := logsParams("0xabc")
	hash := indexer.BuildParamsKey(params)
	require.NoError(t, a.EnsureFilter(context.Background(), indexer.SubTypeLogs, hash, params))
	subID := conn.nextRequest(t, methodEthSubscribe).SubID

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, a.RemoveFilter(ctx, indexer.SubTypeLogs, hash))
	unsub := conn.nextRequest(t, methodEthUnsubscribe)
	assert.Equal(t, []interface{}{subID}, unsub.Params)
}

// A caller deadline still in the future bounds the upstream cancel, so
// connection teardown keeps its budget when the upstream stops answering.
func TestAdapterReleaseHonoursLiveCallerDeadline(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink

	params := logsParams("0xabc")
	hash := indexer.BuildParamsKey(params)
	require.NoError(t, a.EnsureFilter(context.Background(), indexer.SubTypeLogs, hash, params))
	_ = conn.nextRequest(t, methodEthSubscribe)

	// The upstream never answers the unsubscribe.
	a.forward = func(ctx context.Context, _ *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.NoError(t, a.RemoveFilter(ctx, indexer.SubTypeLogs, hash))
	assert.Less(t, time.Since(start), 2*time.Second, "release must stop at the caller's deadline, not the attempt timeout")
}

func failFirstUnsubscribe(a *Adapter) *atomic.Bool {
	var failed atomic.Bool
	send := a.forward
	a.forward = func(ctx context.Context, nq *common.NormalizedRequest, bypass bool) (*common.NormalizedResponse, error) {
		if m, _ := nq.Method(); m == methodEthUnsubscribe && failed.CompareAndSwap(false, true) {
			return nil, errors.New("upstream-level rate limit exceeded") // never reaches the wire
		}
		return send(ctx, nq, bypass)
	}
	return &failed
}

// RemoveFilter's unsubscribe fails while the connection stays up; the
// still-live upstream subscription must be cancelled when it next notifies.
func TestAdapterFailedUnsubscribeIsRetried(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink
	failed := failFirstUnsubscribe(a)

	params := logsParams("0xabc")
	hash := indexer.BuildParamsKey(params)
	require.NoError(t, a.EnsureFilter(context.Background(), indexer.SubTypeLogs, hash, params))
	subID := conn.nextRequest(t, methodEthSubscribe).SubID
	nextEvent(t, sink, indexer.KindLog)

	require.NoError(t, a.RemoveFilter(context.Background(), indexer.SubTypeLogs, hash))
	require.True(t, failed.Load())

	conn.sendLog(subID, 2) // the upstream subscription is still live
	unsub := conn.nextRequest(t, methodEthUnsubscribe)
	assert.Equal(t, []interface{}{subID}, unsub.Params)
	select {
	case ev := <-sink.events:
		t.Fatalf("removed filter delivered %+v", ev)
	default:
	}
}

// The same, for an in-flight subscribe whose result is released because its
// filter was removed meanwhile.
func TestAdapterFailedReleaseOfUncommittedSubscribeIsRetried(t *testing.T) {
	server := newNotifyServer(t)
	gate := make(chan struct{})
	server.gate = gate
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink
	failed := failFirstUnsubscribe(a)

	params := logsParams("0xabc")
	hash := indexer.BuildParamsKey(params)
	done := make(chan error, 1)
	go func() { done <- a.EnsureFilter(context.Background(), indexer.SubTypeLogs, hash, params) }()
	subID := conn.nextRequest(t, methodEthSubscribe).SubID
	require.NoError(t, a.RemoveFilter(context.Background(), indexer.SubTypeLogs, hash))
	close(gate)
	require.NoError(t, <-done)
	require.True(t, failed.Load())

	conn.sendLog(subID, 2)
	unsub := conn.nextRequest(t, methodEthUnsubscribe)
	assert.Equal(t, []interface{}{subID}, unsub.Params)
}

// A filter subscribe whose reply never arrives must still return: callers
// hold locks across it (the indexer's reconcile, client subscribes).
func TestAdapterEnsureFilterIsBounded(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	a.attemptTimeout = 300 * time.Millisecond
	// The failed subscribe starts a background retry that outlives the test.
	nop := zerolog.Nop()
	a.logger = &nop
	t.Cleanup(func() {
		a.subsMu.Lock()
		if a.resubCancel != nil {
			a.resubCancel()
		}
		a.subsMu.Unlock()
	})
	<-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink
	// The upstream never answers the subscribe.
	a.forward = func(ctx context.Context, _ *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	params := logsParams("0xabc")
	start := time.Now()
	err := a.EnsureFilter(context.Background(), indexer.SubTypeLogs, indexer.BuildParamsKey(params), params)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second, "EnsureFilter must stop at the attempt timeout")
}
