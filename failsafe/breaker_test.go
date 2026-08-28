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
