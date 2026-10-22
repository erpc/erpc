package wsupstream

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Residual of the 6057bccb cap safety net: a release whose eth_unsubscribe
// is rejected before reaching the wire leaves the node-side subscription
// live. A noisy one is reclaimed by the orphan path on its next
// notification; a quiet one holds a per-connection slot until reconnect.
func TestAdapterQuietSubscriptionReleasedDespiteFailedUnsubscribe(t *testing.T) {
	server := newNotifyServer(t)
	a, sink := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	a.sink = sink
	failed := failFirstUnsubscribe(a)

	params := []interface{}{indexer.SubTypeNewPendingTransactions}
	hash := indexer.BuildParamsKey(params)
	require.NoError(t, a.EnsureFilter(context.Background(), indexer.SubTypeNewPendingTransactions, hash, params))
	subID := conn.nextRequest(t, methodEthSubscribe).SubID

	require.NoError(t, a.RemoveFilter(context.Background(), indexer.SubTypeNewPendingTransactions, hash))
	require.True(t, failed.Load())
	assert.Equal(t, []interface{}{subID}, conn.nextRequest(t, methodEthUnsubscribe).Params)
}
