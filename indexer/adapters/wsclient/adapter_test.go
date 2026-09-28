package wsclient

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWriter struct {
	mu      sync.Mutex
	written []string
	block   chan struct{} // writes wait on it while non-nil
	dropped []bool        // lossy flag per NotificationDropped call
}

func (w *fakeWriter) WriteSubscriptionNotification(_ string, result json.RawMessage) error {
	if w.block != nil {
		<-w.block
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = append(w.written, string(result))
	return nil
}

func (w *fakeWriter) NotificationDropped(_ Subscription, lossy bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropped = append(w.dropped, lossy)
}

func (w *fakeWriter) snapshot() (written []string, dropped []bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.written...), append([]bool(nil), w.dropped...)
}

func newTestAdapter(w *fakeWriter, bufferSize int) *Adapter {
	lg := zerolog.Nop()
	return New("c1", w, &lg, bufferSize)
}

func deliver(a *Adapter, kind indexer.EventKind, filterHash string, n int) {
	for i := 0; i < n; i++ {
		a.Deliver(indexer.IndexedEvent{StreamEvent: indexer.StreamEvent{
			Kind: kind, NetworkId: "evm:1", FilterHash: filterHash, Payload: json.RawMessage(fmt.Sprintf("%d", i)),
		}})
	}
}

func TestAdapter_AddAfterDrainIsRefused(t *testing.T) {
	a := newTestAdapter(&fakeWriter{}, 8)
	require.NoError(t, a.AddSubscription("s1", "evm:1", indexer.KindLog, "f", 10))

	removed := a.Drain()
	require.Len(t, removed, 1)
	assert.Equal(t, "s1", removed[0].ClientSubID)

	assert.ErrorIs(t, a.AddSubscription("s2", "evm:1", indexer.KindLog, "f", 10), ErrClosed)
	assert.Equal(t, 0, a.Count())
}

// A sub removed by RemoveSubscription is not returned by a later Drain, so
// its filter reference is released exactly once.
func TestAdapter_RemoveAndDrainAreExclusive(t *testing.T) {
	a := newTestAdapter(&fakeWriter{}, 8)
	require.NoError(t, a.AddSubscription("s1", "evm:1", indexer.KindLog, "f", 10))
	require.NoError(t, a.AddSubscription("s2", "evm:1", indexer.KindLog, "f", 10))

	_, _, _, existed := a.RemoveSubscription("s1")
	require.True(t, existed)
	_, _, _, existed = a.RemoveSubscription("s1")
	assert.False(t, existed)

	removed := a.Drain()
	require.Len(t, removed, 1)
	assert.Equal(t, "s2", removed[0].ClientSubID)
}

func TestAdapter_SubscriptionLimitIsAtomic(t *testing.T) {
	a := newTestAdapter(&fakeWriter{}, 8)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if a.AddSubscription(fmt.Sprintf("s%d", i), "evm:1", indexer.KindNewHead, "", 10) == nil {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	assert.EqualValues(t, 10, ok.Load())
	assert.Equal(t, 10, a.Count())
	a.Drain()
}

// A burst within the buffer reaches a client that keeps up.
func TestAdapter_BurstWithinBufferIsDelivered(t *testing.T) {
	w := &fakeWriter{}
	a := newTestAdapter(w, 256)
	require.NoError(t, a.AddSubscription("s1", "evm:1", indexer.KindLog, "f", 10))
	defer a.Drain()

	deliver(a, indexer.KindLog, "f", 200)
	require.Eventually(t, func() bool {
		written, _ := w.snapshot()
		return len(written) == 200
	}, 2*time.Second, 10*time.Millisecond)
	_, dropped := w.snapshot()
	assert.Empty(t, dropped)
}

// Overflow on a lossy kind evicts the oldest queued notification and
// reports it as lossy.
func TestAdapter_NewHeadsOverflowDropsOldest(t *testing.T) {
	w := &fakeWriter{block: make(chan struct{})}
	a := newTestAdapter(w, 2)
	require.NoError(t, a.AddSubscription("s1", "evm:1", indexer.KindNewHead, "", 10))
	defer a.Drain()

	// The writer takes one event and blocks on it; two more fill the buffer.
	deliver(a, indexer.KindNewHead, "", 1)
	time.Sleep(50 * time.Millisecond)
	deliver(a, indexer.KindNewHead, "", 5)
	close(w.block)

	require.Eventually(t, func() bool {
		written, _ := w.snapshot()
		return len(written) == 3
	}, 2*time.Second, 10*time.Millisecond)
	written, dropped := w.snapshot()
	assert.Equal(t, []string{"0", "3", "4"}, written, "newest heads must survive")
	assert.Equal(t, []bool{true, true, true}, dropped)
}

// Overflow on logs keeps what is queued and reports the loss as not lossy,
// so the transport can end the connection.
func TestAdapter_LogsOverflowIsReportedNotEvicted(t *testing.T) {
	w := &fakeWriter{block: make(chan struct{})}
	a := newTestAdapter(w, 2)
	require.NoError(t, a.AddSubscription("s1", "evm:1", indexer.KindLog, "f", 10))
	defer a.Drain()

	deliver(a, indexer.KindLog, "f", 1)
	time.Sleep(50 * time.Millisecond)
	deliver(a, indexer.KindLog, "f", 5)
	close(w.block)

	require.Eventually(t, func() bool {
		written, _ := w.snapshot()
		return len(written) == 3
	}, 2*time.Second, 10*time.Millisecond)
	written, dropped := w.snapshot()
	assert.Equal(t, []string{"0", "0", "1"}, written, "queued logs must be kept in order")
	assert.Equal(t, []bool{false, false, false}, dropped)
}
