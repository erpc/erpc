package failsafe

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBreaker_HalfOpenIgnoreReleasesPermit — regression for the HalfOpen
// permit leak. Record(OutcomeIgnore) must release the trial permit taken
// by TryAcquirePermit; before the fix, a run of ignored outcomes (cache
// misses, canceled attempts) pinned halfOpenInflight at capacity and
// wedged the breaker in HalfOpen forever, rejecting all traffic.
func TestBreaker_HalfOpenIgnoreReleasesPermit(t *testing.T) {
	cfg := &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    2,
		FailureThresholdCapacity: 2,
		HalfOpenAfter:            common.Duration(20 * time.Millisecond),
		SuccessThresholdCount:    2,
		SuccessThresholdCapacity: 2,
	}
	b := NewBreaker(cfg, nil)
	require.NotNil(t, b)

	// Drive to Open.
	b.Record(OutcomeFailure)
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())

	// Elapse HalfOpenAfter so the next permit transitions to HalfOpen.
	time.Sleep(40 * time.Millisecond)

	// More ignore cycles than the trial capacity (2). Each acquire takes a
	// permit; each Ignore must give it back. Pre-fix, the third acquire
	// returned false — and every acquire after it, forever.
	for i := range 4 {
		require.True(t, b.TryAcquirePermit(), "ignore cycle %d must be granted a trial permit", i+1)
		require.Equal(t, StateHalfOpen, b.State())
		b.Record(OutcomeIgnore)
	}
	require.True(t, b.TryAcquirePermit(),
		"permits must still be available after >capacity ignored outcomes")

	// Two successes close the breaker (permit from the assertion above is
	// still held; acquire one more for the second trial).
	b.Record(OutcomeSuccess)
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)

	assert.Equal(t, StateClosed, b.State(), "success threshold must close the breaker")
	assert.True(t, b.TryAcquirePermit(), "closed breaker always permits")
}

// TestBreaker_HalfOpenHonoursSuccessThresholdCount — regression for
// SuccessThresholdCount being dead config. With "3 of 5" the trial must
// tolerate the 2 failures it says it tolerates. Before the fix, the first
// failure re-opened the breaker unconditionally whenever the trial had not
// yet reached SuccessThresholdCapacity, so a failure could never contribute
// toward the capacity check and the only path back to Closed was 5
// consecutive clean successes.
func TestBreaker_HalfOpenHonoursSuccessThresholdCount(t *testing.T) {
	cfg := &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(10 * time.Millisecond),
		SuccessThresholdCount:    3,
		SuccessThresholdCapacity: 5,
	}
	b := NewBreaker(cfg, nil)
	require.NotNil(t, b)

	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())
	time.Sleep(20 * time.Millisecond)

	// Trial: fail, succeed, fail, succeed, succeed. maxFailures = 5-3 = 2, so
	// the two failures must NOT re-open, and the third success must close.
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure)
	require.Equal(t, StateHalfOpen, b.State(),
		"1 failure with maxFailures=2 must not re-open")

	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure)
	require.Equal(t, StateHalfOpen, b.State(),
		"2 failures with maxFailures=2 must not re-open")

	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)

	assert.Equal(t, StateClosed, b.State(),
		"3 successes must close even though 2 failures occurred")
}

// TestBreaker_HalfOpenReopensPastMaxFailures — the other side of the same
// fix: once failures exceed SuccessThresholdCapacity-SuccessThresholdCount
// the trial can no longer reach its success target, so it must re-open
// immediately rather than keep admitting probes.
func TestBreaker_HalfOpenReopensPastMaxFailures(t *testing.T) {
	cfg := &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(10 * time.Millisecond),
		SuccessThresholdCount:    3,
		SuccessThresholdCapacity: 5,
	}
	b := NewBreaker(cfg, nil)
	require.NotNil(t, b)

	b.Record(OutcomeFailure)
	time.Sleep(20 * time.Millisecond)

	for i := range 3 {
		require.True(t, b.TryAcquirePermit(), "failure %d must be admitted", i+1)
		b.Record(OutcomeFailure)
	}
	assert.Equal(t, StateOpen, b.State(),
		"3 failures exceeds maxFailures=2 and must re-open")
}

// TestBreaker_HalfOpenTrialIsTimeBounded — regression for HalfOpen having no
// escape hatch. Open falls back on HalfOpenAfter, but HalfOpen had no delay
// of its own, so a permit acquired and never recorded (panic between
// TryAcquirePermit and Record, caller returning early) pinned
// halfOpenInflight at capacity and wedged the breaker with no path out.
func TestBreaker_HalfOpenTrialIsTimeBounded(t *testing.T) {
	cfg := &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(20 * time.Millisecond),
		SuccessThresholdCount:    2,
		SuccessThresholdCapacity: 2,
	}
	b := NewBreaker(cfg, nil)
	require.NotNil(t, b)

	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())
	time.Sleep(40 * time.Millisecond)

	// Acquire every trial permit and never record — simulating attempts that
	// died between TryAcquirePermit and Record.
	require.True(t, b.TryAcquirePermit())
	require.Equal(t, StateHalfOpen, b.State())
	require.True(t, b.TryAcquirePermit())
	require.False(t, b.TryAcquirePermit(), "capacity reached, must reject")

	// Pre-fix this stayed false forever. The trial must be re-armed once it
	// has outlived HalfOpenAfter without concluding.
	time.Sleep(40 * time.Millisecond)
	require.True(t, b.TryAcquirePermit(),
		"a stale HalfOpen trial must be re-armed instead of wedging")

	b.Record(OutcomeSuccess)
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)
	assert.Equal(t, StateClosed, b.State())
}
