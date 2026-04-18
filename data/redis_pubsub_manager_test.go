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

// Regression test for the propagation gap that caused cross-pod finalized-
// block regressions in production: when two counter updates for the same
// key arrived faster than the consumer goroutine drained its cap=1 buffer,
// the newer message was silently dropped. For monotonic counters the
// freshest value is the only one that matters, so sendKeepLatest must
// evict the stale buffered value instead of refusing the new one.
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

func TestSubscriberChannel_SendKeepLatestSequentialDeliversEachValue(t *testing.T) {
	sc := newTestSubscriberChannel()

	for _, v := range []int64{10, 11, 12, 13} {
		st := CounterInt64State{Value: v, UpdatedAt: v}
		sc.sendKeepLatest(st)
		got := <-sc.ch
		assert.Equal(t, st, got, "sequential publish+drain should deliver each value untouched")
	}
}

// Once the subscriber has been closed, further sends must not block the
// publisher — they return immediately. Prevents any single slow consumer
// from wedging the pub/sub manager goroutine after cleanup.
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

// close must be idempotent — stop() and the per-subscriber cleanup can
// both fire for the same subscriber during shutdown.
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

// End-to-end via notifySubscribers: the manager must honour keep-latest
// across every subscribed channel for the key without taking any lock.
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

// Race detector guard: under concurrent publishes + concurrent closes, no
// send ever panics, no goroutine leaks, and the subscriber eventually sees
// either the latest value or nothing (if it was already closed).
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
