package failsafe

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHedgeTimerPool_BorrowReleaseLifecycle — the basic pool
// contract: borrow returns a timer Reset to the requested duration,
// release stops + drains it for the next consumer. Reused timers
// must NOT fire on a stale tick from a previous lifecycle.
func TestHedgeTimerPool_BorrowReleaseLifecycle(t *testing.T) {
	// First borrow + release: arm a short timer, let it fire, then
	// release. The pool must drain the channel before storing.
	t1 := borrowHedgeTimer(5 * time.Millisecond)
	<-t1.C // let it fire — channel now has the value queued
	releaseHedgeTimer(t1)

	// Second borrow: get a timer (may or may not be the same one
	// from the pool — sync.Pool is opaque). Arm for a longer
	// duration. The C channel MUST start empty — a stale tick from
	// the previous lifecycle would fire immediately and break
	// hedge timing.
	t2 := borrowHedgeTimer(50 * time.Millisecond)
	select {
	case stale := <-t2.C:
		t.Fatalf("pool returned a timer with a stale tick queued (%v) — drain failed", stale)
	case <-time.After(10 * time.Millisecond):
		// Good — no stale tick.
	}
	// And the new arming must still work.
	select {
	case <-t2.C:
		// fired around 50ms — fine.
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("borrowed timer did not fire on its new Reset duration")
	}
	releaseHedgeTimer(t2)
}

// TestHedgeTimerPool_ReleaseUnfiredTimer — releasing a timer that
// hasn't fired yet must Stop it cleanly. After release, the
// channel must be empty (no late tick leaks to the next consumer).
func TestHedgeTimerPool_ReleaseUnfiredTimer(t *testing.T) {
	t1 := borrowHedgeTimer(1 * time.Second)
	// Don't wait — release immediately while the timer is still armed.
	releaseHedgeTimer(t1)

	// Reborrow: if the previous timer were still armed, we'd risk
	// the next consumer seeing a tick at the ~1s mark.
	t2 := borrowHedgeTimer(10 * time.Millisecond)
	select {
	case <-t2.C:
		// Good — fired on the new duration, not the old 1s arming.
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("borrowed timer did not fire — pool may have lost the C signal")
	}
	releaseHedgeTimer(t2)
}

// TestHedgeTimerPool_NilSafe — releaseHedgeTimer(nil) must be a no-op
// so callers can defer release without checking nil.
func TestHedgeTimerPool_NilSafe(t *testing.T) {
	assert.NotPanics(t, func() { releaseHedgeTimer(nil) })
}

// TestRunHedged_PrimaryWinsNoHedgeFired — the hedge timer pool
// integration. When the primary returns fast (well before the hedge
// delay), maxHedges should never fire. The timer must be borrowed
// AND released exactly once with no leaked goroutines or panics.
func TestRunHedged_PrimaryWinsNoHedgeFired(t *testing.T) {
	var fireCount atomic.Int32
	delayFn := func(idx int) time.Duration { return 100 * time.Millisecond }
	inner := func(ctx context.Context) ([]byte, error) {
		fireCount.Add(1)
		return []byte("primary"), nil
	}
	keep := func(r []byte, err error) bool { return err == nil && r != nil }

	r, err := RunHedged[[]byte](
		context.Background(),
		2, // maxHedges
		delayFn,
		inner,
		keep,
		nil,
		HedgeHooks{},
	)
	require.NoError(t, err)
	assert.Equal(t, []byte("primary"), r)
	assert.Equal(t, int32(1), fireCount.Load(),
		"primary won fast → no hedge attempts should have fired")
}

// TestRunHedged_HedgeFiresOnSlowPrimary — when primary is slower
// than the hedge delay, the hedge must fire. Confirms the pooled
// timer correctly drives the wake-up.
func TestRunHedged_HedgeFiresOnSlowPrimary(t *testing.T) {
	var fireCount atomic.Int32
	delayFn := func(idx int) time.Duration { return 20 * time.Millisecond }
	inner := func(ctx context.Context) ([]byte, error) {
		idx := fireCount.Add(1)
		if idx == 1 {
			// Primary: deliberately slower than hedge delay.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return []byte("primary"), nil
			}
		}
		// Hedge attempt: returns quickly.
		return []byte("hedge"), nil
	}
	keep := func(r []byte, err error) bool { return err == nil && r != nil }

	start := time.Now()
	r, err := RunHedged[[]byte](
		context.Background(),
		1, // maxHedges
		delayFn,
		inner,
		keep,
		nil,
		HedgeHooks{},
	)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, []byte("hedge"), r,
		"hedge should win the race since primary is slow")
	assert.Equal(t, int32(2), fireCount.Load(),
		"both primary and hedge should have fired")
	// Hedge fires at ~20ms; should win before primary at 200ms.
	assert.Less(t, elapsed, 150*time.Millisecond,
		"completed time should reflect the hedge winning, not the slow primary")
}

// TestRunHedged_ContextCanceled — when the parent context is
// canceled while the hedge timer is armed, the function must
// return cleanly. This exercises the timer-cleanup defer path.
func TestRunHedged_ContextCanceled(t *testing.T) {
	delayFn := func(idx int) time.Duration { return 1 * time.Second }
	inner := func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	keep := func(r []byte, err error) bool { return err == nil }

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := RunHedged[[]byte](
		ctx,
		1,
		delayFn,
		inner,
		keep,
		nil,
		HedgeHooks{},
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled),
		"parent-cancel must surface as context.Canceled")
}

// BenchmarkHedgeTimerPool_vs_NewTimer — pins the pool win. At high
// RPS this allocation differential is the entire reason for the
// pool's existence; if `New` is ever <= `Pool` we should reconsider.
func BenchmarkHedgeTimerPool_vs_NewTimer(b *testing.B) {
	d := 5 * time.Millisecond

	b.Run("DirectNewTimer", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			t := time.NewTimer(d)
			t.Stop()
		}
	})

	b.Run("BorrowReleasePool", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			t := borrowHedgeTimer(d)
			releaseHedgeTimer(t)
		}
	})
}
