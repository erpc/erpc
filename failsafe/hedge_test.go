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

// TestRunHedged_PrimaryWinsNoHedgeFired — when the primary returns fast
// (well before the hedge delay), maxHedges should never fire.
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
