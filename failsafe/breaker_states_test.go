package failsafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type transition struct {
	from, to State
	reason   string
}

// recordTransitions wires OnTransition to a buffered channel; the hook runs
// on its own goroutine, so tests read transitions from the channel.
func recordTransitions(b *Breaker) <-chan transition {
	ch := make(chan transition, 16)
	b.OnTransition = func(from, to State, reason string) { ch <- transition{from, to, reason} }
	return ch
}

func nextTransition(t *testing.T, ch <-chan transition) transition {
	t.Helper()
	select {
	case tr := <-ch:
		return tr
	case <-time.After(5 * time.Second):
		t.Fatal("expected a state transition")
		return transition{}
	}
}

func TestState_String(t *testing.T) {
	assert.Equal(t, "closed", StateClosed.String())
	assert.Equal(t, "open", StateOpen.String())
	assert.Equal(t, "half_open", StateHalfOpen.String())
	assert.Equal(t, "unknown", State(42).String())
}

func TestErrCircuitOpen(t *testing.T) {
	assert.EqualError(t, ErrCircuitOpen, "circuit breaker is open")
	wrapped := errors.Join(errors.New("upstream x"), ErrCircuitOpen)
	assert.ErrorIs(t, wrapped, ErrCircuitOpen)
}

func TestBreaker_NilIsPermissive(t *testing.T) {
	b := NewBreaker(nil, nil)
	require.Nil(t, b)
	assert.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure) // no-op on nil
	assert.Equal(t, StateClosed, b.State())
	f, s, e := b.Metrics()
	assert.Zero(t, f)
	assert.Zero(t, s)
	assert.Zero(t, e)
}

func TestBreaker_FullCycleWithHooksAndLogs(t *testing.T) {
	var buf bytes.Buffer
	var bufMu sync.Mutex
	logger := zerolog.New(zerolog.SyncWriter(&lockedWriter{w: &buf, mu: &bufMu}))
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    2,
		FailureThresholdCapacity: 3,
		HalfOpenAfter:            0, // Open → HalfOpen on the next permit
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
	}, &logger)
	trs := recordTransitions(b)

	// Window of 3, threshold of 2: the window must fill before opening.
	b.Record(OutcomeFailure)
	b.Record(OutcomeFailure)
	assert.Equal(t, StateClosed, b.State(), "capacity not met yet")
	b.Record(OutcomeSuccess)
	assert.Equal(t, StateOpen, b.State(), "2 failures in a full window of 3 opens")
	assert.Equal(t, transition{StateClosed, StateOpen, "failure_threshold"}, nextTransition(t, trs))

	// Outcomes recorded while Open only bump lifetime counters.
	b.Record(OutcomeSuccess)
	b.Record(OutcomeFailure)
	assert.Equal(t, StateOpen, b.State())

	require.True(t, b.TryAcquirePermit(), "zero HalfOpenAfter permits immediately")
	assert.Equal(t, StateHalfOpen, b.State())
	assert.Equal(t, transition{StateOpen, StateHalfOpen, "half_open_delay_elapsed"}, nextTransition(t, trs))

	b.Record(OutcomeSuccess)
	assert.Equal(t, StateClosed, b.State())
	assert.Equal(t, transition{StateHalfOpen, StateClosed, "half_open_success_threshold"}, nextTransition(t, trs))

	failures, successes, executions := b.Metrics()
	assert.Equal(t, uint64(3), failures)
	assert.Equal(t, uint64(3), successes)
	assert.Equal(t, uint64(6), executions)

	bufMu.Lock()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	bufMu.Unlock()
	require.Len(t, lines, 3)
	var first map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	assert.Equal(t, "warn", first["level"])
	assert.Equal(t, "closed", first["from"])
	assert.Equal(t, "open", first["to"])
	assert.Equal(t, "failure_threshold", first["reason"])
	assert.Equal(t, "circuit breaker state changed", first["message"])
	assert.EqualValues(t, 3, first["executions"])
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestBreaker_ClosedWindowEvictsOldest(t *testing.T) {
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    2,
		FailureThresholdCapacity: 3,
		HalfOpenAfter:            common.Duration(time.Hour),
	}, nil)

	// F S S → 1 failure in window.
	b.Record(OutcomeFailure)
	b.Record(OutcomeSuccess)
	b.Record(OutcomeSuccess)
	// Evicts F: S S F → 1 failure.
	b.Record(OutcomeFailure)
	assert.Equal(t, StateClosed, b.State(), "the evicted failure must not count")
	// Evicts S: S F S → still 1.
	b.Record(OutcomeSuccess)
	assert.Equal(t, StateClosed, b.State())
	// Evicts S: F S F → 2 failures, opens.
	b.Record(OutcomeFailure)
	assert.Equal(t, StateOpen, b.State())
}

func TestBreaker_IgnoreDoesNotMoveClosedWindow(t *testing.T) {
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		FailureThresholdCapacity: 1,
	}, nil)
	for range 5 {
		b.Record(OutcomeIgnore)
	}
	assert.Equal(t, StateClosed, b.State())
	_, _, executions := b.Metrics()
	assert.Zero(t, executions, "ignored outcomes are not executions")
}

func TestBreaker_ThresholdConfigFallbacks(t *testing.T) {
	cases := []struct {
		name        string
		cfg         common.CircuitBreakerPolicyConfig
		failsToOpen int // -1: never opens
	}{
		{"capacity falls back to count", common.CircuitBreakerPolicyConfig{FailureThresholdCount: 3}, 3},
		{"zero count never opens", common.CircuitBreakerPolicyConfig{FailureThresholdCapacity: 2}, -1},
		{"nothing configured never opens", common.CircuitBreakerPolicyConfig{}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.HalfOpenAfter = common.Duration(time.Hour)
			b := NewBreaker(&cfg, nil)
			if tc.failsToOpen < 0 {
				for range 10 {
					b.Record(OutcomeFailure)
				}
				assert.Equal(t, StateClosed, b.State())
				return
			}
			for i := 1; i < tc.failsToOpen; i++ {
				b.Record(OutcomeFailure)
			}
			assert.Equal(t, StateClosed, b.State())
			b.Record(OutcomeFailure)
			assert.Equal(t, StateOpen, b.State())
		})
	}
}

func TestBreaker_OpenRejectsUntilDelayElapses(t *testing.T) {
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{
		FailureThresholdCount: 1,
		HalfOpenAfter:         common.Duration(time.Hour),
	}, nil)
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())
	for range 3 {
		assert.False(t, b.TryAcquirePermit())
	}
	assert.Equal(t, StateOpen, b.State())

	// Pretend the delay elapsed.
	b.mu.Lock()
	b.openedAt = time.Now().Add(-2 * time.Hour)
	b.mu.Unlock()
	assert.True(t, b.TryAcquirePermit())
	assert.Equal(t, StateHalfOpen, b.State())
}

func TestBreaker_HalfOpenPermitCapacity(t *testing.T) {
	cases := []struct {
		name    string
		cfg     common.CircuitBreakerPolicyConfig
		permits int
	}{
		{"capacity", common.CircuitBreakerPolicyConfig{SuccessThresholdCount: 1, SuccessThresholdCapacity: 3}, 3},
		{"count when capacity unset", common.CircuitBreakerPolicyConfig{SuccessThresholdCount: 2}, 2},
		{"one when nothing set", common.CircuitBreakerPolicyConfig{}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.FailureThresholdCount = 1
			// HalfOpenAfter 0: permits flow immediately and a full trial is
			// never considered stale.
			b := NewBreaker(&cfg, nil)
			b.Record(OutcomeFailure)
			require.Equal(t, StateOpen, b.State())
			for i := range tc.permits {
				require.True(t, b.TryAcquirePermit(), "permit %d", i+1)
			}
			assert.Equal(t, StateHalfOpen, b.State())
			assert.False(t, b.TryAcquirePermit(), "permits beyond capacity are refused")
			assert.False(t, b.TryAcquirePermit())
		})
	}
}

func TestBreaker_HalfOpenDefaultsCloseOnFirstSuccessReopenOnFirstFailure(t *testing.T) {
	for _, tc := range []struct {
		outcome Outcome
		want    State
		reason  string
	}{
		{OutcomeSuccess, StateClosed, "half_open_success_threshold"},
		{OutcomeFailure, StateOpen, "half_open_failure"},
	} {
		b := NewBreaker(&common.CircuitBreakerPolicyConfig{FailureThresholdCount: 1}, nil)
		b.Record(OutcomeFailure)
		require.True(t, b.TryAcquirePermit())
		require.Equal(t, StateHalfOpen, b.State())
		trs := recordTransitions(b)
		b.Record(tc.outcome)
		assert.Equal(t, tc.want, b.State())
		assert.Equal(t, transition{StateHalfOpen, tc.want, tc.reason}, nextTransition(t, trs))
	}
}

func TestBreaker_HalfOpenRecordWithoutPermit(t *testing.T) {
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		SuccessThresholdCount:    2,
		SuccessThresholdCapacity: 2,
	}, nil)
	b.Record(OutcomeFailure)
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeIgnore) // releases the only held permit
	b.Record(OutcomeIgnore) // nothing held: must not underflow
	// A record with no permit held still counts toward the trial.
	b.Record(OutcomeSuccess)
	assert.Equal(t, StateHalfOpen, b.State())
	// Two permits are still available: the inflight counter never went negative.
	assert.True(t, b.TryAcquirePermit())
	assert.True(t, b.TryAcquirePermit())
	assert.False(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)
	assert.Equal(t, StateClosed, b.State())
}

func TestBreaker_ClosedAfterHalfOpenStartsWithEmptyWindow(t *testing.T) {
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    2,
		FailureThresholdCapacity: 2,
	}, nil)
	b.Record(OutcomeFailure)
	b.Record(OutcomeFailure)
	require.Equal(t, StateOpen, b.State())
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeSuccess)
	require.Equal(t, StateClosed, b.State())

	// The window was reset on close: a single failure must not reopen.
	b.Record(OutcomeFailure)
	assert.Equal(t, StateClosed, b.State())
	b.Record(OutcomeFailure)
	assert.Equal(t, StateOpen, b.State())
}

func TestBreaker_UnknownStatePermits(t *testing.T) {
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{FailureThresholdCount: 1}, nil)
	b.state.Store(99)
	assert.True(t, b.TryAcquirePermit())
	assert.Equal(t, "unknown", b.State().String())
}


func TestBreaker_StaleHalfOpenTrialIsRearmed(t *testing.T) {
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		HalfOpenAfter:            common.Duration(time.Hour),
		SuccessThresholdCount:    1,
		SuccessThresholdCapacity: 1,
	}, nil)
	b.Record(OutcomeFailure)
	b.mu.Lock()
	b.openedAt = time.Now().Add(-2 * time.Hour)
	b.mu.Unlock()
	require.True(t, b.TryAcquirePermit())
	require.False(t, b.TryAcquirePermit(), "trial is fresh and full")

	b.mu.Lock()
	b.halfOpenAt = time.Now().Add(-2 * time.Hour)
	b.mu.Unlock()
	require.True(t, b.TryAcquirePermit(), "stale trial is re-armed")
	require.False(t, b.TryAcquirePermit(), "re-armed trial holds exactly one permit")
	b.Record(OutcomeSuccess)
	assert.Equal(t, StateClosed, b.State())
}

func TestBreaker_SuccessCountAboveCapacityToleratesNoFailure(t *testing.T) {
	// Count > capacity is a misconfiguration; the trial must not tolerate a
	// negative number of failures, it re-opens on the first one.
	b := NewBreaker(&common.CircuitBreakerPolicyConfig{
		FailureThresholdCount:    1,
		SuccessThresholdCount:    3,
		SuccessThresholdCapacity: 1,
	}, nil)
	b.Record(OutcomeFailure)
	require.True(t, b.TryAcquirePermit())
	b.Record(OutcomeFailure)
	assert.Equal(t, StateOpen, b.State())
}
