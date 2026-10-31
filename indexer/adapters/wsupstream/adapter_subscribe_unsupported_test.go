package wsupstream

import (
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/stretchr/testify/require"
)

// TestAdapterGivesUpNewHeadsWhenUnsupported: an upstream that rejects
// newHeads as an unsupported method is asked once per connection, and its
// topics still settle.
func TestAdapterGivesUpNewHeadsWhenUnsupported(t *testing.T) {
	compressResubRetry(t)
	server := newNotifyServer(t)
	server.rejectNewHeads = true
	a, _ := newTestAdapter(t, server.wsURL(t))
	conn1 := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.Set([]indexer.Topic{indexer.TopicNewHeads, txTopic(t, "0xabc")})

	var headsSubscribes int
	deadline := time.After(time.Second)
	for done := false; !done; {
		select {
		case req := <-conn1.requests:
			if req.Method == methodEthSubscribe && req.Params[0] == indexer.SubTypeNewHeads {
				headsSubscribes++
			}
		case <-deadline:
			done = true
		}
	}
	require.Equal(t, 1, headsSubscribes, "newHeads must not be retried after an unsupported rejection")
	a.subsMu.Lock()
	settled := a.resubDone
	a.subsMu.Unlock()
	require.True(t, settled, "resubscribe loop must settle without newHeads")

	_ = conn1.conn.UnderlyingConn().Close()
	conn2 := <-server.newConn
	require.Equal(t, indexer.SubTypeNewHeads, conn2.nextRequest(t, methodEthSubscribe).Params[0],
		"a new connection asks for newHeads again")
	require.Eventually(t, func() bool {
		a.subsMu.Lock()
		defer a.subsMu.Unlock()
		return a.resubDone
	}, 3*time.Second, 10*time.Millisecond, "resubscribe loop must settle on the new connection")
}
