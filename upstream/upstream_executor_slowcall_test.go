package upstream

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpstreamBreakerOutcome_SlowCall pins the classifier contract after
// slowCallThreshold became generic: canceled/skipped are ignored BEFORE the
// slow check (their duration reflects the canceller), then any completion —
// success or error — at or above the threshold is a breaker failure, then
// the pre-existing error classification applies. slowCall == 0 disables
// slow-call classification entirely.
func TestUpstreamBreakerOutcome_SlowCall(t *testing.T) {
	endpoint, err := url.Parse("http://rpc.localhost")
	require.NoError(t, err)

	const threshold = 50 * time.Millisecond

	cases := []struct {
		name     string
		resp     *common.NormalizedResponse
		err      error
		dur      time.Duration
		slowCall time.Duration
		want     failsafe.Outcome
	}{
		{
			name:     "fast success closes",
			resp:     common.NewNormalizedResponse(),
			dur:      threshold - time.Millisecond,
			slowCall: threshold,
			want:     failsafe.OutcomeSuccess,
		},
		{
			name:     "slow success trips (>= boundary)",
			resp:     common.NewNormalizedResponse(),
			dur:      threshold,
			slowCall: threshold,
			want:     failsafe.OutcomeFailure,
		},
		{
			name:     "unset threshold never classifies",
			resp:     common.NewNormalizedResponse(),
			dur:      time.Hour,
			slowCall: 0,
			want:     failsafe.OutcomeSuccess,
		},
		{
			name:     "cancellation beats slow",
			err:      common.NewErrEndpointRequestCanceled(errors.New("context canceled")),
			dur:      2 * threshold,
			slowCall: threshold,
			want:     failsafe.OutcomeIgnore,
		},
		{
			name:     "skipped beats slow",
			err:      common.NewErrUpstreamRequestSkipped(errors.New("method ignored"), "rpc1"),
			dur:      2 * threshold,
			slowCall: threshold,
			want:     failsafe.OutcomeIgnore,
		},
		{
			name:     "fast transport failure still fails",
			err:      common.NewErrEndpointTransportFailure(endpoint, errors.New("connection refused")),
			dur:      threshold - time.Millisecond,
			slowCall: threshold,
			want:     failsafe.OutcomeFailure,
		},
		{
			name:     "fast ignorable error stays ignored",
			err:      common.NewErrEndpointCapacityExceeded(errors.New("overloaded")),
			dur:      threshold - time.Millisecond,
			slowCall: threshold,
			want:     failsafe.OutcomeIgnore,
		},
		{
			name:     "slow ignorable error counts as failure",
			err:      common.NewErrEndpointCapacityExceeded(errors.New("overloaded")),
			dur:      2 * threshold,
			slowCall: threshold,
			want:     failsafe.OutcomeFailure,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, upstreamBreakerOutcome(tc.resp, tc.err, tc.dur, tc.slowCall))
		})
	}
}

// slowCallExecutor builds a real upstreamExecutor whose breaker opens after
// two failures, with the given slowCallThreshold. No retry/hedge/timeout —
// Run degrades to a single breaker-wrapped attempt.
func slowCallExecutor(t *testing.T, threshold time.Duration) *upstreamExecutor {
	t.Helper()
	lg := zerolog.Nop()
	exec, err := NewUpstreamExecutor(&common.UpstreamFailsafeConfig{
		MatchMethod: "*",
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    2,
			FailureThresholdCapacity: 2,
			SuccessThresholdCount:    1,
			SuccessThresholdCapacity: 1,
			HalfOpenAfter:            common.Duration(time.Minute),
			SlowCallThreshold:        common.NewStaticDuration(threshold),
		},
	}, &lg)
	require.NoError(t, err)
	return exec
}

// TestUpstreamExecutor_SlowCallTripsBreaker exercises the wiring end to end:
// NewUpstreamExecutor picks slowCallSpec off the breaker config, and
// callBreakerWithTimeout measures the attempt and records slow SUCCESSES as
// breaker failures — so sustained slowness opens the circuit and the next
// attempt is rejected without calling inner.
func TestUpstreamExecutor_SlowCallTripsBreaker(t *testing.T) {
	// 1ms threshold + 10ms sleep: dur >= threshold is guaranteed
	// (time.Sleep sleeps at LEAST the requested duration).
	exec := slowCallExecutor(t, time.Millisecond)
	req := common.NewNormalizedRequest([]byte(`{"method":"eth_blockNumber"}`))

	innerCalls := 0
	inner := func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
		innerCalls++
		time.Sleep(10 * time.Millisecond)
		return common.NewNormalizedResponse(), nil
	}

	for range 2 {
		resp, err := exec.Run(context.Background(), req, inner)
		require.NoError(t, err, "slow calls still succeed toward the caller")
		require.NotNil(t, resp)
	}
	require.Equal(t, failsafe.StateOpen, exec.Breaker().State(),
		"two slow successes must open the breaker")

	_, err := exec.Run(context.Background(), req, inner)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen),
		"open breaker must reject with ErrFailsafeCircuitBreakerOpen, got: %v", err)
	assert.Equal(t, 2, innerCalls, "rejected attempt must not reach inner")
}

// TestUpstreamExecutor_FastCallsDoNotTripSlowBreaker is the control: with a
// generous threshold, fast successes keep the breaker closed.
func TestUpstreamExecutor_FastCallsDoNotTripSlowBreaker(t *testing.T) {
	exec := slowCallExecutor(t, time.Hour)
	req := common.NewNormalizedRequest([]byte(`{"method":"eth_blockNumber"}`))

	inner := func(ctx context.Context, isHedge bool) (*common.NormalizedResponse, error) {
		return common.NewNormalizedResponse(), nil
	}

	for range 4 {
		_, err := exec.Run(context.Background(), req, inner)
		require.NoError(t, err)
	}
	assert.Equal(t, failsafe.StateClosed, exec.Breaker().State(),
		"fast successes must never count as slow-call failures")
}
