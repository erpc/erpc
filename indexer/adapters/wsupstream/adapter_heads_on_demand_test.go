package wsupstream

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/stretchr/testify/require"
)

// noHeadsSubscribe fails if conn receives a newHeads eth_subscribe within d.
func noHeadsSubscribe(t *testing.T, conn *notifyConn, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case req := <-conn.requests:
			if req.Method == methodEthSubscribe && req.Params[0] == indexer.SubTypeNewHeads {
				t.Fatalf("unexpected newHeads subscribe")
			}
		case <-deadline:
			return
		}
	}
}

// nextHeadsSubscribe returns the next newHeads eth_subscribe on conn.
func nextHeadsSubscribe(t *testing.T, conn *notifyConn) notifyRequest {
	t.Helper()
	for {
		req := conn.nextRequest(t, methodEthSubscribe)
		if req.Params[0] == indexer.SubTypeNewHeads {
			return req
		}
	}
}

// TestAdapterHeadsOnDemand: an on-demand adapter connects and carries
// filters without newHeads, subscribes it when wanted, unsubscribes it when
// no longer wanted, and keeps the wanted state across reconnects.
func TestAdapterHeadsOnDemand(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	a.headsOnDemand = true
	conn1 := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, a.Start(context.Background(), fakeNetworkHandle{}, sink))

	params := logsParams("0xabc")
	require.NoError(t, a.EnsureFilter(context.Background(), indexer.SubTypeLogs, indexer.BuildParamsKey(params), params))
	noHeadsSubscribe(t, conn1, 300*time.Millisecond)
	require.False(t, a.HeadsLive())
	require.Eventually(t, func() bool {
		a.subsMu.Lock()
		defer a.subsMu.Unlock()
		return a.resubDone
	}, 3*time.Second, 10*time.Millisecond, "resubscribe loop must settle without newHeads")

	a.SetHeadsWanted(context.Background(), true)
	sub := nextHeadsSubscribe(t, conn1)
	require.Eventually(t, a.HeadsLive, 3*time.Second, 10*time.Millisecond)

	a.SetHeadsWanted(context.Background(), false)
	unsub := conn1.nextRequest(t, methodEthUnsubscribe)
	require.Equal(t, sub.SubID, unsub.Params[0])
	require.False(t, a.HeadsLive())

	// Not wanted: a new connection resubscribes the filter only.
	_ = conn1.conn.UnderlyingConn().Close()
	conn2 := <-server.newConn
	logsSub := conn2.nextRequest(t, methodEthSubscribe)
	require.Equal(t, indexer.SubTypeLogs, logsSub.Params[0])
	noHeadsSubscribe(t, conn2, 300*time.Millisecond)

	// Wanted: it is subscribed now and after the next reconnect.
	a.SetHeadsWanted(context.Background(), true)
	nextHeadsSubscribe(t, conn2)
	require.Eventually(t, a.HeadsLive, 3*time.Second, 10*time.Millisecond)
	_ = conn2.conn.UnderlyingConn().Close()
	conn3 := <-server.newConn
	nextHeadsSubscribe(t, conn3)
	require.Eventually(t, a.HeadsLive, 3*time.Second, 10*time.Millisecond)
}

// TestAdapterHeadsWantedIgnoredWhenNotOnDemand: a regular adapter always
// subscribes newHeads and SetHeadsWanted(false) leaves it alone.
func TestAdapterHeadsWantedIgnoredWhenNotOnDemand(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn1 := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, a.Start(context.Background(), fakeNetworkHandle{}, sink))
	nextHeadsSubscribe(t, conn1)
	require.Eventually(t, a.HeadsLive, 3*time.Second, 10*time.Millisecond)

	a.SetHeadsWanted(context.Background(), false)
	time.Sleep(100 * time.Millisecond)
	require.True(t, a.HeadsLive())
}
