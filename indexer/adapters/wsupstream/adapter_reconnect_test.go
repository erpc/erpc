package wsupstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// --- test doubles -------------------------------------------------------

type fakeNetworkHandle struct{}

func (fakeNetworkHandle) Id() string                       { return "evm:324" }
func (fakeNetworkHandle) FinalityDepth() int64             { return 0 }
func (fakeNetworkHandle) SuggestLatestBlock(string, int64) {}

type fakeSink struct {
	events chan indexer.StreamEvent
}

func (s *fakeSink) Ingest(ev indexer.StreamEvent) { s.events <- ev }

// notifyServer is a WS server that only accepts connections and pushes
// subscription notification frames; subscribe RPCs never reach it because
// the adapter's forward func is stubbed (the real one rides
// upstream.Forward, i.e. the failsafe/circuit-breaker pipeline).
type notifyServer struct {
	srv     *httptest.Server
	newConn chan *notifyConn
}

type notifyConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func newNotifyServer(t *testing.T) *notifyServer {
	n := &notifyServer{newConn: make(chan *notifyConn, 16)}
	upgrader := websocket.Upgrader{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		nc := &notifyConn{conn: conn}
		n.newConn <- nc
		// Keep reading so pings get ponged and client writes are drained.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
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

func (nc *notifyConn) sendNewHead(subID string, num int64) {
	notif, _ := common.SonicCfg.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_subscription",
		"params": map[string]interface{}{
			"subscription": subID,
			"result": map[string]interface{}{
				"number":     fmt.Sprintf("0x%x", num),
				"hash":       fmt.Sprintf("0xhash%x", num),
				"parentHash": fmt.Sprintf("0xhash%x", num-1),
			},
		},
	})
	nc.writeMu.Lock()
	defer nc.writeMu.Unlock()
	_ = nc.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = nc.conn.WriteMessage(websocket.TextMessage, notif)
}

func compressResubRetry(t *testing.T) {
	origMin, origMax := resubRetryMin, resubRetryMax
	resubRetryMin = 20 * time.Millisecond
	resubRetryMax = 100 * time.Millisecond
	t.Cleanup(func() { resubRetryMin, resubRetryMax = origMin, origMax })
}

func syntheticSubscribeResponse(subID string) *common.NormalizedResponse {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":900000001,"result":"%s"}`, subID)
	return common.NewNormalizedResponse().WithBody(io.NopCloser(strings.NewReader(body)))
}

// --- the regression test -------------------------------------------------

// TestAdapterResubscribesWithRetryAfterReconnect reproduces the wedge from
// the 2026-06-12 zkSync incident at the adapter layer:
//
//  1. subscribe RPCs ride the upstream's failsafe pipeline, whose circuit
//     breaker is typically still OPEN at the instant the WS layer
//     reconnects after an upstream outage;
//  2. the old adapter attempted the resubscribe exactly once per reconnect,
//     so an open breaker meant no newHeads subscription (and therefore no
//     heads for any client) until the *next* disconnect, i.e. potentially
//     forever.
//
// The adapter must keep retrying until the breaker lets a subscribe
// through, and report Healthy()==false until it has a live subscription.
func TestAdapterResubscribesWithRetryAfterReconnect(t *testing.T) {
	compressResubRetry(t)

	server := newNotifyServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel)
	up := common.NewFakeUpstream("test-ws-upstream")
	ci, err := clients.NewWsJsonRpcClient(ctx, &logger, "test-project", up, server.wsURL(t), nil, nil)
	require.NoError(t, err)
	wsc, ok := ci.(*clients.WsJsonRpcClient)
	require.True(t, ok)

	var conn1 *notifyConn
	select {
	case conn1 = <-server.newConn:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the initial connection")
	}
	_ = conn1

	// forward simulates the failsafe pipeline: the first failuresPerEpoch
	// calls of each epoch fail as an open circuit breaker would, then the
	// subscribe succeeds with an epoch-scoped subscription id.
	var (
		forwardCalls atomic.Int64
		epoch        atomic.Int64
		failuresLeft atomic.Int64
	)
	const failuresPerEpoch = 2
	epoch.Store(1)
	failuresLeft.Store(failuresPerEpoch)

	sink := &fakeSink{events: make(chan indexer.StreamEvent, 16)}
	a := &Adapter{
		upstreamID: "test-ws-upstream",
		networkID:  "evm:324",
		wsClient:   wsc,
		logger:     &logger,
		filters:    make(map[string]*filterSub),
		forward: func(ctx context.Context, nq *common.NormalizedRequest, _ bool) (*common.NormalizedResponse, error) {
			forwardCalls.Add(1)
			if failuresLeft.Add(-1) >= 0 {
				return nil, errors.New("circuit breaker is open on upstream-level")
			}
			return syntheticSubscribeResponse(fmt.Sprintf("0xsub-epoch%d", epoch.Load())), nil
		},
	}

	require.NoError(t, a.Start(ctx, fakeNetworkHandle{}, sink))

	// Epoch 1: the initial subscribe must survive the two breaker
	// failures and eventually succeed.
	require.Eventually(t, a.Healthy, 3*time.Second, 10*time.Millisecond,
		"adapter never became healthy despite the breaker closing after %d failures", failuresPerEpoch)
	require.GreaterOrEqual(t, forwardCalls.Load(), int64(failuresPerEpoch+1),
		"expected the subscribe to be retried through breaker failures")

	// Heads flow on the first connection.
	conn1.sendNewHead("0xsub-epoch1", 100)
	select {
	case ev := <-sink.events:
		require.Equal(t, indexer.KindNewHead, ev.Kind)
		require.Equal(t, int64(100), ev.Block.Number)
	case <-time.After(2 * time.Second):
		t.Fatal("no head delivered to the sink on the initial connection")
	}

	// Ungraceful upstream death: kill the TCP connection with no close
	// handshake. The client reconnects; the adapter must clear its stale
	// subscription (Healthy()==false), then retry the resubscribe through
	// a fresh round of breaker failures.
	epoch.Store(2)
	failuresLeft.Store(failuresPerEpoch)
	_ = conn1.conn.UnderlyingConn().Close()

	var conn2 *notifyConn
	select {
	case conn2 = <-server.newConn:
	case <-time.After(3 * time.Second):
		t.Fatal("client never re-dialed after the upstream connection was killed")
	}

	require.Eventually(t, a.Healthy, 3*time.Second, 10*time.Millisecond,
		"adapter never re-established the newHeads subscription after reconnect")

	// Heads must flow again on the new connection with the new sub id —
	// this is the incident's acceptance criterion at this layer.
	conn2.sendNewHead("0xsub-epoch2", 101)
	select {
	case ev := <-sink.events:
		require.Equal(t, indexer.KindNewHead, ev.Kind)
		require.Equal(t, int64(101), ev.Block.Number)
	case <-time.After(2 * time.Second):
		t.Fatal("no head delivered after upstream recovery — adapter did not self-heal")
	}
}

// TestAdapterHealthyReportsFalseWhenDisconnected pins the Healthy()
// contract the subscription-refusal path depends on.
func TestAdapterHealthyReportsFalseWhenDisconnected(t *testing.T) {
	a := &Adapter{filters: make(map[string]*filterSub)}
	// No wsClient at all would panic — Healthy is only called on adapters
	// built by New, which always have one. Use a disconnected client.
	server := newNotifyServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	up := common.NewFakeUpstream("test-ws-upstream")
	ci, err := clients.NewWsJsonRpcClient(ctx, &logger, "test-project", up, server.wsURL(t), nil, nil)
	require.NoError(t, err)
	a.wsClient = ci.(*clients.WsJsonRpcClient)

	// Connected but no newHeads subscription yet.
	require.False(t, a.Healthy())

	a.subsMu.Lock()
	a.newHeadsSubID = "0xsub"
	a.subsMu.Unlock()
	require.True(t, a.Healthy())
}
