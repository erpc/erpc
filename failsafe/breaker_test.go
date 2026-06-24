package failsafe

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func newWedgeTestBreaker() *Breaker {
	cfg := &common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
		HalfOpenAfter:            common.Duration(1 * time.Millisecond),
	}
	lg := zerolog.Nop()
	return NewBreaker(cfg, &lg)
}

func openThenHalfOpen(t *testing.T, b *Breaker) {
	t.Helper()
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure) // one failure in Closed opens (threshold 1)
	require.Equal(t, StateOpen, State(b.state.Load()), "breaker should be open after a failure")
	time.Sleep(5 * time.Millisecond) // exceed HalfOpenAfter
	require.True(t, b.TryAcquirePermit(), "first half-open trial permit must be granted")
	require.Equal(t, StateHalfOpen, State(b.state.Load()))
}

// TestBreaker_HalfOpenPermitReleasedOnIgnoredOutcome is the regression for the
// production probe/selection wedge (incident 2026-06-24): a HalfOpen trial that
// returns an IGNORABLE outcome (timeout / cancellation / soft error) must
// release the trial permit it reserved in TryAcquirePermit. If it doesn't,
// halfOpenInflight leaks, saturates the trial capacity, and TryAcquirePermit
// denies every subsequent caller — wedging the breaker open and failing both
// real traffic AND the selection-recovery probes until a process restart.
func TestBreaker_HalfOpenPermitReleasedOnIgnoredOutcome(t *testing.T) {
	b := newWedgeTestBreaker()
	openThenHalfOpen(t, b)

	// The trial is inconclusive (e.g. the request was canceled or timed out —
	// exactly what a transient redis/upstream blip produces).
	b.Record(OutcomeIgnore)

	b.mu.Lock()
	inflight, hoSucc, hoFail := b.halfOpenInflight, b.halfOpenSuccess, b.halfOpenFailure
	b.mu.Unlock()

	require.Equal(t, 0, inflight, "ignored half-open outcome must release the trial permit (no leak)")
	require.Equal(t, 0, hoSucc, "ignored outcome must not count as a half-open success")
	require.Equal(t, 0, hoFail, "ignored outcome must not count as a half-open failure")
	require.True(t, b.TryAcquirePermit(),
		"breaker must not be wedged: a fresh trial permit must be grantable after an ignored outcome")
}

// TestBreaker_RepeatedIgnoredTrialsDoNotWedge proves the breaker keeps offering
// trial permits across many inconclusive trials, and a real success still closes
// it. Before the fix this Fatals on the first iteration (permit leaked → denied).
func TestBreaker_RepeatedIgnoredTrialsDoNotWedge(t *testing.T) {
	b := newWedgeTestBreaker()
	openThenHalfOpen(t, b)
	// First reserved permit (from openThenHalfOpen) resolves as ignored.
	b.Record(OutcomeIgnore)

	for i := 0; i < 50; i++ {
		if !b.TryAcquirePermit() {
			t.Fatalf("breaker wedged after %d ignored trials: half-open permit leaked", i)
		}
		b.Record(OutcomeIgnore)
	}

	require.True(t, b.TryAcquirePermit(), "trial permit still grantable")
	b.Record(OutcomeSuccess)
	require.Equal(t, StateClosed, State(b.state.Load()),
		"a successful trial must still close the breaker after a run of ignored trials")
}
