package data

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newTestSubscriberChannel() *subscriberChannel {
	return &subscriberChannel{
		ch:   make(chan CounterInt64State, 1),
		done: make(chan struct{}),
	}
}

// Updates arriving faster than the consumer drains its buffer must evict the
// stale buffered value rather than drop the newer one.
func TestSubscriberChannel_SendKeepLatestEvictsStale(t *testing.T) {
	sc := newTestSubscriberChannel()

	first := CounterInt64State{Value: 100, UpdatedAt: 1}
	second := CounterInt64State{Value: 101, UpdatedAt: 2}
	third := CounterInt64State{Value: 102, UpdatedAt: 3}

	// No consumer — three publishes land back-to-back, overflowing the
	// single-slot buffer twice. Only the latest value must survive.
	sc.sendKeepLatest(first)
	sc.sendKeepLatest(second)
	sc.sendKeepLatest(third)

	select {
	case got := <-sc.ch:
		assert.Equal(t, third, got, "full buffer must be updated to the latest published value")
	default:
		t.Fatalf("subscriber channel empty after three publishes")
	}

	select {
	case extra := <-sc.ch:
		t.Fatalf("unexpected extra value on subscriber channel: %+v", extra)
	default:
	}
}

// A stale value (e.g. Subscribe's initial GET landing after a pubsub
// message) must not replace a fresher one still in the buffer.
func TestSubscriberChannel_SendKeepLatestKeepsFresherBuffered(t *testing.T) {
	sc := newTestSubscriberChannel()

	fresh := CounterInt64State{Value: 101, UpdatedAt: 2}
	sc.sendKeepLatest(fresh)
	sc.sendKeepLatest(CounterInt64State{Value: 100, UpdatedAt: 1})

	assert.Equal(t, fresh, <-sc.ch, "buffered fresher value must survive a stale send")
}

func TestSubscriberChannel_SendKeepLatestSequentialDeliversEachValue(t *testing.T) {
	sc := newTestSubscriberChannel()

	for _, v := range []int64{10, 11, 12, 13} {
		st := CounterInt64State{Value: v, UpdatedAt: v}
		sc.sendKeepLatest(st)
		got := <-sc.ch
		assert.Equal(t, st, got, "sequential publish+drain should deliver each value untouched")
	}
}

// Sends to a closed subscriber return immediately instead of blocking.
func TestSubscriberChannel_SendKeepLatestReturnsAfterClose(t *testing.T) {
	sc := newTestSubscriberChannel()

	// Fill the buffer so a subsequent send would spin in the drain loop
	// if it didn't respect the done signal.
	sc.ch <- CounterInt64State{Value: 1, UpdatedAt: 1}
	sc.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc.sendKeepLatest(CounterInt64State{Value: 99, UpdatedAt: 99})
	}()

	select {
	case <-done:
		// ok — send returned promptly
	case <-time.After(time.Second):
		t.Fatalf("sendKeepLatest did not return after subscriber was closed")
	}
}

// stop() and the per-subscriber cleanup can both close a subscriber.
func TestSubscriberChannel_CloseIsIdempotent(t *testing.T) {
	sc := newTestSubscriberChannel()
	sc.close()
	sc.close()
	sc.close()

	select {
	case <-sc.done:
		// expected
	default:
		t.Fatalf("done channel must be closed after close()")
	}
}

// notifySubscribers keeps the latest value on every channel for the key.
func TestRedisPubSubManager_NotifySubscribersKeepsLatest(t *testing.T) {
	m := &RedisPubSubManager{}
	a := newTestSubscriberChannel()
	b := newTestSubscriberChannel()
	m.addSubscriber("finalized", a)
	m.addSubscriber("finalized", b)

	m.notifySubscribers("finalized", CounterInt64State{Value: 1, UpdatedAt: 1})
	m.notifySubscribers("finalized", CounterInt64State{Value: 2, UpdatedAt: 2})

	for _, sc := range []*subscriberChannel{a, b} {
		got := <-sc.ch
		assert.Equal(t, int64(2), got.Value, "each subscriber should observe the latest value")
	}
}

// Concurrent sends and closes never panic or block.
func TestSubscriberChannel_ConcurrentSendAndClose(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		sc := newTestSubscriberChannel()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				sc.sendKeepLatest(CounterInt64State{Value: int64(i), UpdatedAt: int64(i + 1)})
			}
		}()
		go func() {
			defer wg.Done()
			// Close mid-burst.
			time.Sleep(time.Microsecond * 50)
			sc.close()
		}()
		wg.Wait()
	}
}
