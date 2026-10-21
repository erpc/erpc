package wsupstream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func compressHeadsWatch(t *testing.T) {
	orig := headsWatchInterval
	headsWatchInterval = 50 * time.Millisecond
	t.Cleanup(func() { headsWatchInterval = orig })
}

// TestAdapterReconnectsSilentNewHeads: a newHeads subscription that goes
// silent on a live connection while the upstream's polled head advances is
// recovered by reconnecting.
func TestAdapterReconnectsSilentNewHeads(t *testing.T) {
	compressResubRetry(t)
	compressHeadsWatch(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	var polled atomic.Int64
	a.polledHead = polled.Load
	conn1 := <-server.newConn
	startHeadsSource(t, a, sink)
	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond)

	stalls := telemetry.MetricUpstreamWebsocketHeadsStallReconnectsTotal.WithLabelValues("test-project", "evm:123", "test-ws-upstream")
	stallsBefore := testutil.ToFloat64(stalls)

	conn1.sendNewHead(conn1.nextRequest(t, methodEthSubscribe).SubID, 100)
	require.Equal(t, int64(100), nextHead(t, sink))
	polled.Store(105)

	var conn2 *notifyConn
	select {
	case conn2 = <-server.newConn:
	case <-time.After(3 * time.Second):
		t.Fatal("silent newHeads subscription was never recovered")
	}
	require.Equal(t, stallsBefore+1, testutil.ToFloat64(stalls), "a forced reconnect is counted")
	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond)
	conn2.sendNewHead(conn2.nextRequest(t, methodEthSubscribe).SubID, 106)
	require.Equal(t, int64(106), nextHead(t, sink))
}

// TestAdapterKeepsSilentNewHeadsWhenChainHalts: silence while the upstream
// reports no newer block is a halted chain, not a dead subscription.
func TestAdapterKeepsSilentNewHeadsWhenChainHalts(t *testing.T) {
	compressResubRetry(t)
	compressHeadsWatch(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	var polled atomic.Int64
	a.polledHead = polled.Load
	conn1 := <-server.newConn
	startHeadsSource(t, a, sink)
	require.Eventually(t, subscribedHeads(a), 3*time.Second, 10*time.Millisecond)

	conn1.sendNewHead(conn1.nextRequest(t, methodEthSubscribe).SubID, 100)
	require.Equal(t, int64(100), nextHead(t, sink))
	polled.Store(100)

	select {
	case <-server.newConn:
		t.Fatal("reconnected although the upstream reported no newer block")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestAdapterWatchHeadsStopsWithClient: the watch loop ends with the
// upstream client instead of outliving it.
func TestAdapterWatchHeadsStopsWithClient(t *testing.T) {
	compressHeadsWatch(t)
	server := newNotifyServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	ci, err := clients.NewWsJsonRpcClient(ctx, &logger, "test-project", common.NewFakeUpstream("test-ws-upstream"), server.wsURL(t), nil, nil)
	require.NoError(t, err)
	a := &Adapter{wsClient: ci.(*clients.WsJsonRpcClient), logger: &logger, polledHead: func() int64 { return 0 }}

	done := make(chan struct{})
	go func() {
		a.watchHeads()
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchHeads kept running after the upstream client shut down")
	}
}
