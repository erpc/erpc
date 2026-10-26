package failsafe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// releaseLog records every result handed to release.
type releaseLog struct {
	mu   sync.Mutex
	vals []string
}

func (l *releaseLog) release(r *string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.vals = append(l.vals, *r)
}

func (l *releaseLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.vals...)
}

func strp(s string) *string { return &s }

func keepNoErr(r *string, err error) bool { return err == nil && r != nil }

func TestRunHedged_MaxHedgesBoundsAttemptsAndAllFailuresReturnLast(t *testing.T) {
	var calls atomic.Int32
	var fired []int
	var firedMu sync.Mutex
	inner := func(ctx context.Context) (*string, error) {
		n := calls.Add(1)
		return nil, errors.New("fail-" + string(rune('0'+n)))
	}
	_, err := RunHedged[*string](
		context.Background(),
		2,
		func(idx int) time.Duration { return 0 },
		inner,
		keepNoErr,
		nil,
		HedgeHooks{OnFire: func(idx int, _ time.Duration) {
			firedMu.Lock()
			fired = append(fired, idx)
			firedMu.Unlock()
		}},
	)
	require.Error(t, err)
	assert.Regexp(t, `^fail-[123]$`, err.Error())
	assert.Equal(t, int32(3), calls.Load(), "primary + 2 hedges, never more")
	firedMu.Lock()
	assert.Equal(t, []int{1, 2}, fired, "OnFire reports hedges only, not the primary")
	firedMu.Unlock()
}

func TestRunHedged_NegativeMaxHedgesRunsPrimaryOnly(t *testing.T) {
	var calls atomic.Int32
	r, err := RunHedged[*string](
		context.Background(), -3, nil,
		func(ctx context.Context) (*string, error) {
			calls.Add(1)
			return nil, errors.New("primary failed")
		},
		keepNoErr, nil, HedgeHooks{},
	)
	assert.Nil(t, r)
	assert.EqualError(t, err, "primary failed")
	assert.Equal(t, int32(1), calls.Load())
}

func TestRunHedged_NilKeepAcceptsFirstResult(t *testing.T) {
	r, err := RunHedged[*string](
		context.Background(), 0, nil,
		func(ctx context.Context) (*string, error) { return strp("partial"), errors.New("soft error") },
		nil, nil, HedgeHooks{},
	)
	require.NotNil(t, r)
	assert.Equal(t, "partial", *r)
	assert.EqualError(t, err, "soft error", "nil keep returns the first result with its error")
}

func TestRunHedged_HedgeRecoversAfterFastPrimaryFailure(t *testing.T) {
	var calls atomic.Int32
	hedgeGo := make(chan struct{})
	inner := func(ctx context.Context) (*string, error) {
		if calls.Add(1) == 1 {
			close(hedgeGo)
			return nil, errors.New("primary transient")
		}
		return strp("hedge"), nil
	}
	r, err := RunHedged[*string](
		context.Background(), 1,
		func(idx int) time.Duration {
			<-hedgeGo // arm the hedge timer only once the primary is running
			return 5 * time.Millisecond
		},
		inner, keepNoErr, nil, HedgeHooks{},
	)
	require.NoError(t, err)
	assert.Equal(t, "hedge", *r, "a scheduled hedge still fires after the primary failed")
}

func TestRunHedged_NegativeDelayFiresHedgeImmediately(t *testing.T) {
	var calls atomic.Int32
	primaryRelease := make(chan struct{})
	defer close(primaryRelease)
	inner := func(ctx context.Context) (*string, error) {
		if calls.Add(1) == 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-primaryRelease:
				return strp("primary"), nil
			}
		}
		return strp("hedge"), nil
	}
	r, err := RunHedged[*string](
		context.Background(), 1,
		func(idx int) time.Duration { return -time.Second },
		inner, keepNoErr, nil, HedgeHooks{},
	)
	require.NoError(t, err)
	assert.Equal(t, "hedge", *r)
}

func TestRunHedged_LosersAreReleasedAndCanceled(t *testing.T) {
	var calls atomic.Int32
	losersCanceled := make(chan struct{}, 2)
	winnerGo := make(chan struct{})
	started := make(chan struct{}, 3)
	inner := func(ctx context.Context) (*string, error) {
		n := calls.Add(1)
		started <- struct{}{}
		if n == 3 {
			<-winnerGo
			return strp("winner"), nil
		}
		<-ctx.Done()
		losersCanceled <- struct{}{}
		return strp("loser"), nil // non-nil: must be released
	}
	var rel releaseLog
	go func() {
		for range 3 {
			<-started
		}
		close(winnerGo)
	}()
	r, err := RunHedged[*string](
		context.Background(), 2,
		func(idx int) time.Duration { return 0 },
		inner, keepNoErr, rel.release, HedgeHooks{},
	)
	require.NoError(t, err)
	assert.Equal(t, "winner", *r)
	for range 2 {
		select {
		case <-losersCanceled:
		case <-time.After(5 * time.Second):
			t.Fatal("losers must observe sibling cancellation")
		}
	}
	// RunHedged drains every in-flight attempt before returning.
	assert.ElementsMatch(t, []string{"loser", "loser"}, rel.snapshot(), "winner is never released")
}

func TestRunHedged_NotKeptResultsAreNotReleasedWhenReturned(t *testing.T) {
	var rel releaseLog
	r, err := RunHedged[*string](
		context.Background(), 0, nil,
		func(ctx context.Context) (*string, error) { return strp("rejected"), errors.New("bad") },
		keepNoErr, rel.release, HedgeHooks{},
	)
	require.EqualError(t, err, "bad")
	assert.Equal(t, "rejected", *r, "caller owns the returned non-kept result")
	assert.Empty(t, rel.snapshot())
}

func TestRunHedged_ParentCancelReleasesInFlightResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	inner := func(c context.Context) (*string, error) {
		started <- struct{}{}
		<-c.Done()
		return strp("abandoned"), nil
	}
	go func() {
		<-started
		<-started
		cancel()
	}()
	var rel releaseLog
	r, err := RunHedged[*string](
		ctx, 1,
		func(idx int) time.Duration { return 0 },
		inner, keepNoErr, rel.release, HedgeHooks{},
	)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, r)
	assert.Equal(t, []string{"abandoned", "abandoned"}, rel.snapshot())
}

func TestRunHedged_ParentCancelSkipsZeroResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		<-started
		cancel()
	}()
	var rel releaseLog
	_, err := RunHedged[*string](
		ctx, 0, nil,
		func(c context.Context) (*string, error) {
			close(started)
			<-c.Done()
			return nil, c.Err()
		},
		keepNoErr, rel.release, HedgeHooks{},
	)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, rel.snapshot(), "nil results are never passed to release")
}

func TestRunHedged_PanicInAttemptBecomesError(t *testing.T) {
	var calls atomic.Int32
	r, err := RunHedged[*string](
		context.Background(), 1,
		func(idx int) time.Duration { return 0 },
		func(ctx context.Context) (*string, error) {
			if calls.Add(1) == 1 {
				panic("attempt exploded")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		func(r *string, err error) bool { return err != nil },
		nil, HedgeHooks{},
	)
	assert.Nil(t, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hedge attempt panic: attempt exploded")
}
