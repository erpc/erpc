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

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test doubles -------------------------------------------------------

type txEvent struct {
	variant string
	raw     string
}

type answer struct {
	topic indexer.Topic
	err   error
}

// fakeSink is the adapter's intake.
type fakeSink struct {
	heads   chan json.RawMessage
	txs     chan txEvent
	answers chan answer
}

func (s *fakeSink) Header(_ string, raw json.RawMessage) { s.heads <- raw }

func (s *fakeSink) Subscribed(_ string, t indexer.Topic, err error) {
	select {
	case s.answers <- answer{t, err}:
	default:
	}
}

func (s *fakeSink) PendingTx(_ string, variant string, raw json.RawMessage) {
	s.txs <- txEvent{variant, string(raw)}
}

// notifyServer is a WS upstream that answers eth_subscribe with ids from a
// per-connection counter ("0x1", "0x2", …, as real nodes do), follows each
// newPendingTransactions subscribe response with one notification for it, answers any other
// method with true, and can push notification frames.
type notifyServer struct {
	srv     *httptest.Server
	newConn chan *notifyConn
	// gate, when set, is received from after each eth_subscribe is
	// recorded and before it is answered.
	gate chan struct{}
	// rejectNewHeads answers newHeads subscribes with -32601.
	rejectNewHeads bool
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
		go nc.serve(n.gate, n.rejectNewHeads)
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

func (nc *notifyConn) serve(gate chan struct{}, rejectNewHeads bool) {
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
		if rejectNewHeads && req.Params[0] == indexer.SubTypeNewHeads {
			nc.write(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"error":{"code":-32601,"message":"the method eth_subscribe does not exist/is not available"}}`)
			continue
		}
		nc.write(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":"` + req.SubID + `"}`)
		if req.Params[0] == indexer.SubTypeNewPendingTransactions {
			nc.sendTx(req.SubID, 1)
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

func (nc *notifyConn) sendTx(subID string, num int64) {
	nc.write(fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":%q,"result":"0xtx%x"}}`,
		subID, num))
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
	ci, err := clients.NewWsJsonRpcClient(ctx, &logger, "test-project", common.NewFakeUpstream("test-ws-upstream"), u, nil, evm.NewJsonRpcErrorExtractor())
	require.NoError(t, err)
	wsc := ci.(*clients.WsJsonRpcClient)
	sink := &fakeSink{heads: make(chan json.RawMessage, 64), txs: make(chan txEvent, 64), answers: make(chan answer, 64)}
	a := &Adapter{
		upstreamID:   "test-ws-upstream",
		projectID:    "test-project",
		networkLabel: "evm:123",
		wsClient:     wsc,
		logger:       &logger,
		topics:       make(map[indexer.Topic]*pendingSub),
		resubWake:    make(chan struct{}, 1),
		intake:       sink,
		forward: func(ctx context.Context, nq *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
			return wsc.SendRequest(ctx, nq)
		},
		retryMin:       resubRetryMin,
		retryMax:       resubRetryMax,
		attemptTimeout: resubAttemptTimeout,
	}
	return a, sink
}

// startHeadsSource starts a as the network starts a wss source: feeding
// newHeads.
func startHeadsSource(a *Adapter) {
	a.Set([]indexer.Topic{indexer.TopicNewHeads})
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

// txTopic is a newPendingTransactions topic; tag tells topics apart.
func txTopic(t *testing.T, tag string) indexer.Topic {
	t.Helper()
	v, err := indexer.PendingTxVariantOf([]interface{}{indexer.SubTypeNewPendingTransactions, map[string]interface{}{"tag": tag}})
	require.NoError(t, err)
	return indexer.PendingTxTopic(v)
}

// topicLive reports whether topic's subscription is live on the current
// connection.
func topicLive(a *Adapter, topic indexer.Topic) bool {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	sub := a.topics[topic]
	return sub != nil && sub.id != ""
}

// nextHead returns the number of the next header the adapter hands intake.
func nextHead(t *testing.T, sink *fakeSink) int64 {
	t.Helper()
	select {
	case raw := <-sink.heads:
		var h struct {
			Number string `json:"number"`
		}
		require.NoError(t, json.Unmarshal(raw, &h))
		n, err := common.HexToInt64(h.Number)
		require.NoError(t, err)
		return n
	case <-time.After(3 * time.Second):
		t.Fatal("no head reached intake")
		return 0
	}
}

// nextTx returns the next pending transaction the adapter hands intake.
func nextTx(t *testing.T, sink *fakeSink) txEvent {
	t.Helper()
	select {
	case tx := <-sink.txs:
		return tx
	case <-time.After(3 * time.Second):
		t.Fatal("no pending transaction reached intake")
		return txEvent{}
	}
}

// nextAnswer returns the next subscribe answer for topic, skipping others.
func nextAnswer(t *testing.T, sink *fakeSink, topic indexer.Topic) error {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ans := <-sink.answers:
			if ans.topic == topic {
				return ans.err
			}
		case <-timeout:
			t.Fatalf("no subscribe answer for %s", topic)
			return nil
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

	startHeadsSource(a)

	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond,
		"adapter never subscribed newHeads despite the failures stopping after %d", failuresPerEpoch)
	require.GreaterOrEqual(t, forwardCalls.Load(), int64(failuresPerEpoch+1),
		"expected the subscribe to be retried through failures")

	conn1.sendNewHead(conn1.nextRequest(t, methodEthSubscribe).SubID, 100)
	require.Equal(t, int64(100), nextHead(t, sink))

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
	require.Equal(t, int64(101), nextHead(t, sink))
}

// TestAdapterResubscribesAllTopicsAfterReconnect: subscription ids come
// from a per-connection counter, so the new connection reuses the old
// ids for different subscriptions. Every topic must deliver after the
// resubscribe, and each must be subscribed exactly once.
func TestAdapterResubscribesAllTopicsAfterReconnect(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn1 := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)

	const numTopics = 8
	topics := []indexer.Topic{indexer.TopicNewHeads}
	for i := range numTopics {
		topics = append(topics, txTopic(t, fmt.Sprintf("0x%x", i)))
	}
	a.Set(topics)
	for range numTopics + 1 {
		conn1.nextRequest(t, methodEthSubscribe)
	}
	require.Eventually(t, func() bool {
		for _, topic := range topics[1:] {
			if !topicLive(a, topic) {
				return false
			}
		}
		return subscribedHeads(a)()
	}, 3*time.Second, 10*time.Millisecond)
	for range numTopics {
		nextTx(t, sink)
	}

	_ = conn1.conn.UnderlyingConn().Close()
	conn2 := <-server.newConn
	subscribes := make(map[string]string) // sub id -> first param
	for range numTopics + 1 {
		req := conn2.nextRequest(t, methodEthSubscribe)
		subscribes[req.SubID] = fmt.Sprint(req.Params[0])
	}
	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond)

	for id, subType := range subscribes {
		if subType == indexer.SubTypeNewPendingTransactions {
			conn2.sendTx(id, 2)
		}
	}
	delivered := make(map[string]bool)
	for len(delivered) < numTopics {
		if tx := nextTx(t, sink); tx.raw == `"0xtx2"` {
			delivered[tx.variant] = true
		}
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
// transaction right behind the subscribe response; it must not be dropped.
func TestAdapterDeliversFirstNotificationAfterSubscribe(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	<-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)

	topic := txTopic(t, "0xabc")
	a.Set([]indexer.Topic{topic})
	require.NoError(t, nextAnswer(t, sink, topic))
	variant, _ := topic.PendingTxVariant()
	assert.Equal(t, txEvent{variant: variant, raw: `"0xtx1"`}, nextTx(t, sink))
}

// TestAdapterDropTopicDuringSubscribe: a Set that drops a topic while its
// subscribe is in flight must not leave the late subscription behind
// upstream.
func TestAdapterDropTopicDuringSubscribe(t *testing.T) {
	server := newNotifyServer(t)
	server.gate = make(chan struct{})
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)

	topic := txTopic(t, "0xabc")
	a.Set([]indexer.Topic{topic})
	subID := conn.nextRequest(t, methodEthSubscribe).SubID

	a.Set(nil)
	server.gate <- struct{}{}

	unsub := conn.nextRequest(t, methodEthUnsubscribe)
	assert.Equal(t, []interface{}{subID}, unsub.Params)
	a.subsMu.Lock()
	assert.Empty(t, a.topics)
	a.subsMu.Unlock()
	select {
	case ans := <-sink.answers:
		t.Fatalf("a dropped topic got an answer: %+v", ans)
	default:
	}
}

// TestAdapterSetWhileDisconnected: Set must report the failure at once (so
// the reconciler knows) and keep the topic, to subscribe it once connected.
func TestAdapterSetWhileDisconnected(t *testing.T) {
	server := newNotifyServer(t)
	u := server.wsURL(t)
	server.srv.Close()
	a, sink := newTestAdapter(t, u)

	topic := txTopic(t, "0xabc")
	a.Set([]indexer.Topic{topic})
	require.ErrorIs(t, nextAnswer(t, sink, topic), errNotConnected)
	assert.False(t, topicLive(a, topic))
	a.subsMu.Lock()
	assert.Len(t, a.topics, 1)
	a.subsMu.Unlock()
}

// TestAdapterRetriesFailedTopic: a topic whose subscribe failed on a live
// connection is retried in the background until it is live, and a Set
// without it ends that.
func TestAdapterRetriesFailedTopic(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	<-server.newConn
	startHeadsSource(a)
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

	topic := txTopic(t, "0xabc")
	a.Set([]indexer.Topic{indexer.TopicNewHeads, topic})
	require.Error(t, nextAnswer(t, sink, topic))
	require.Eventually(t, func() bool { return topicLive(a, topic) },
		3*time.Second, 10*time.Millisecond, "the failed topic must be retried until live")

	a.Set([]indexer.Topic{indexer.TopicNewHeads})
	assert.False(t, topicLive(a, topic))
}

// Cancelling a subscription upstream must not depend on a done context: a
// dropped topic still releases the upstream subscription.
func TestAdapterDroppedTopicUnsubscribes(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	// Like the real path, refuse to send once the context is done.
	send := a.forward
	a.forward = func(ctx context.Context, nq *common.NormalizedRequest, bypass bool) (*common.NormalizedResponse, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return send(ctx, nq, bypass)
	}

	topic := txTopic(t, "0xabc")
	a.Set([]indexer.Topic{topic})
	subID := conn.nextRequest(t, methodEthSubscribe).SubID
	require.NoError(t, nextAnswer(t, sink, topic))

	a.Set(nil)
	unsub := conn.nextRequest(t, methodEthUnsubscribe)
	assert.Equal(t, []interface{}{subID}, unsub.Params)
}

// A Set that drops a topic never waits on the upstream's unsubscribe, so
// the reconciler keeps its pace when the upstream stops answering; the
// unsubscribe itself stops at the attempt timeout.
func TestAdapterSetDoesNotWaitOnRelease(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	a.attemptTimeout = 300 * time.Millisecond
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)

	topic := txTopic(t, "0xabc")
	a.Set([]indexer.Topic{topic})
	_ = conn.nextRequest(t, methodEthSubscribe)
	require.NoError(t, nextAnswer(t, sink, topic))

	// The upstream never answers the unsubscribe.
	released := make(chan time.Duration, 1)
	a.forward = func(ctx context.Context, _ *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
		start := time.Now()
		<-ctx.Done()
		released <- time.Since(start)
		return nil, ctx.Err()
	}
	start := time.Now()
	a.Set(nil)
	assert.Less(t, time.Since(start), 100*time.Millisecond, "Set must not wait on the unsubscribe")
	select {
	case d := <-released:
		assert.Less(t, d, 2*time.Second, "the unsubscribe must stop at the attempt timeout")
	case <-time.After(3 * time.Second):
		t.Fatal("the unsubscribe never ended")
	}
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

// A dropped topic's unsubscribe fails while the connection stays up; the
// still-live upstream subscription must be cancelled when it next notifies.
func TestAdapterFailedUnsubscribeIsRetried(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	failed := failFirstUnsubscribe(a)

	topic := txTopic(t, "0xabc")
	a.Set([]indexer.Topic{topic})
	subID := conn.nextRequest(t, methodEthSubscribe).SubID
	nextTx(t, sink)

	a.Set(nil)
	require.Eventually(t, failed.Load, 3*time.Second, 10*time.Millisecond)

	conn.sendTx(subID, 2) // the upstream subscription is still live
	unsub := conn.nextRequest(t, methodEthUnsubscribe)
	assert.Equal(t, []interface{}{subID}, unsub.Params)
	select {
	case tx := <-sink.txs:
		t.Fatalf("dropped topic delivered %+v", tx)
	default:
	}
}

// The same, for an in-flight subscribe whose result is released because its
// topic was dropped meanwhile.
func TestAdapterFailedReleaseOfUncommittedSubscribeIsRetried(t *testing.T) {
	server := newNotifyServer(t)
	gate := make(chan struct{})
	server.gate = gate
	a, _ := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	failed := failFirstUnsubscribe(a)

	a.Set([]indexer.Topic{txTopic(t, "0xabc")})
	subID := conn.nextRequest(t, methodEthSubscribe).SubID
	a.Set(nil)
	close(gate)
	require.Eventually(t, failed.Load, 3*time.Second, 10*time.Millisecond)

	conn.sendTx(subID, 2)
	unsub := conn.nextRequest(t, methodEthUnsubscribe)
	assert.Equal(t, []interface{}{subID}, unsub.Params)
}

// A subscribe whose reply never arrives must still end at the attempt
// timeout and report its failure, so the topic is retried and a waiting
// client subscribe gets an answer.
func TestAdapterSubscribeIsBounded(t *testing.T) {
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
	// The upstream never answers the subscribe.
	a.forward = func(ctx context.Context, _ *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	topic := txTopic(t, "0xabc")
	start := time.Now()
	a.Set([]indexer.Topic{topic})
	require.Error(t, nextAnswer(t, sink, topic))
	assert.Less(t, time.Since(start), 3*time.Second, "the subscribe must stop at the attempt timeout")
}
