package wsupstream

import (
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
	a, _ := newTestAdapter(t, server.wsURL(t))
	conn := <-server.newConn
	require.Eventually(t, a.wsClient.IsConnected, 3*time.Second, 10*time.Millisecond)
	failed := failFirstUnsubscribe(a)

	variant, err := indexer.PendingTxVariantOf([]interface{}{indexer.SubTypeNewPendingTransactions})
	require.NoError(t, err)
	topic := indexer.PendingTxTopic(variant)
	a.Set([]indexer.Topic{topic})
	subID := conn.nextRequest(t, methodEthSubscribe).SubID
	require.Eventually(t, func() bool { return topicLive(a, topic) }, 3*time.Second, 10*time.Millisecond)

	a.Set(nil)
	require.Eventually(t, failed.Load, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, []interface{}{subID}, conn.nextRequest(t, methodEthUnsubscribe).Params)
}
