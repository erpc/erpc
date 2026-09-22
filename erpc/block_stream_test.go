package erpc

import (
	"testing"
	"time"
)

func newTestHub() *blockStreamHub {
	return &blockStreamHub{subs: make(map[*blockStreamSub]struct{})}
}

func TestBlockStreamHub_AdvanceMonotonicAndSignals(t *testing.T) {
	h := newTestHub()
	sub := h.subscribe()

	h.advance(100)
	if got := h.head.Load(); got != 100 {
		t.Fatalf("head = %d, want 100", got)
	}
	select {
	case <-sub.ch:
	default:
		t.Fatal("expected a wake signal after advance to 100")
	}

	// A lower value is ignored (head is monotonic) and produces no signal.
	h.advance(50)
	if got := h.head.Load(); got != 100 {
		t.Fatalf("head after rollback = %d, want 100 (monotonic)", got)
	}
	select {
	case <-sub.ch:
		t.Fatal("did not expect a signal for a non-advancing head")
	default:
	}

	h.advance(101)
	if got := h.head.Load(); got != 101 {
		t.Fatalf("head = %d, want 101", got)
	}
	select {
	case <-sub.ch:
	default:
		t.Fatal("expected a wake signal after forward advance to 101")
	}
}

func TestBlockStreamHub_AdvanceIsNonBlocking(t *testing.T) {
	h := newTestHub()
	_ = h.subscribe() // deliberately never drained

	// advance runs inside the poller's synchronous update path: buffered(1) +
	// non-blocking send means a burst against an undrained subscriber must never
	// block.
	done := make(chan struct{})
	go func() {
		for i := int64(1); i <= 1000; i++ {
			h.advance(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("advance blocked with an undrained subscriber")
	}
	if got := h.head.Load(); got != 1000 {
		t.Fatalf("head = %d, want 1000", got)
	}
}

func TestBlockStreamHub_Unsubscribe(t *testing.T) {
	h := newTestHub()
	sub := h.subscribe()
	h.unsubscribe(sub)

	if n := len(h.subs); n != 0 {
		t.Fatalf("subs = %d after unsubscribe, want 0", n)
	}
	// Advancing after unsubscribe must not panic and must not signal a gone sub.
	h.advance(10)
	select {
	case <-sub.ch:
		t.Fatal("an unsubscribed subscriber should not be signaled")
	default:
	}
}

func TestBlockStreamHub_FansOutToAllSubscribers(t *testing.T) {
	h := newTestHub()
	a := h.subscribe()
	b := h.subscribe()

	h.advance(7)
	for i, s := range []*blockStreamSub{a, b} {
		select {
		case <-s.ch:
		default:
			t.Fatalf("subscriber %d was not signaled on advance", i)
		}
	}
}
