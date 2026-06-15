package consensus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWaitCap_MaxWaitOnResult_BoundsTailLatency verifies that once one
// non-empty response arrives, the analyzer resolves within maxWaitOnResult
// even if a sibling participant is still running.
func TestWaitCap_MaxWaitOnResult_BoundsTailLatency(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(3).
		WithAgreementThreshold(3). // require 3 to disable short-circuit
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		WithMaxWaitOnResult(common.NewStaticDuration(100 * time.Millisecond)).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		switch idx {
		case 1:
			// fast non-empty response — arms maxWaitOnResult deadline
			return validResponseWithValue("0xfast"), nil
		case 2:
			// medium speed (well within cap) — also non-empty
			time.Sleep(20 * time.Millisecond)
			return validResponseWithValue("0xfast"), nil
		default:
			// slow straggler — exceeds the cap, should be cancelled
			time.Sleep(2 * time.Second)
			return validResponseWithValue("0xslow"), nil
		}
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Less(t, elapsed, 800*time.Millisecond,
		"wait cap must bound elapsed time well below the straggler's 2s")
}

// TestWaitCap_MaxWaitOnEmpty_TighterFloor verifies that when ONLY empty
// responses have arrived, the (typically larger) maxWaitOnEmpty cap
// applies — bounded even if no real answer is ever produced.
func TestWaitCap_MaxWaitOnEmpty_TighterFloor(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(3).
		WithAgreementThreshold(3).
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		WithMaxWaitOnEmpty(common.NewStaticDuration(150 * time.Millisecond)).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	_, _ = pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		switch idx {
		case 1:
			// first response is empty — arms maxWaitOnEmpty
			return validResponseWithValue(""), nil
		case 2:
			time.Sleep(20 * time.Millisecond)
			return validResponseWithValue(""), nil
		default:
			// straggler way over the cap
			time.Sleep(2 * time.Second)
			return validResponseWithValue("0xslow"), nil
		}
	})
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 800*time.Millisecond,
		"maxWaitOnEmpty must bound elapsed time well below the straggler's 2s")
}

// TestWaitCap_NoCap_WaitsForEveryone confirms the default behavior is
// unchanged: zero caps = wait for every participant.
func TestWaitCap_NoCap_WaitsForEveryone(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(2).
		WithAgreementThreshold(2).
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	_, _ = pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		if idx == 1 {
			return validResponseWithValue("0x1"), nil
		}
		time.Sleep(250 * time.Millisecond)
		return validResponseWithValue("0x1"), nil
	})
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond,
		"with no wait cap, consensus must wait for the slower participant")
}

// skippedParticipantErr builds the error a participant slot produces when its
// drawn upstream statically skips the method (e.g. ignoreMethods).
func skippedParticipantErr(method, upstreamId string) error {
	return common.NewErrUpstreamRequestSkipped(
		common.NewErrUpstreamMethodIgnored(method, upstreamId), upstreamId)
}

// TestWaitCap_NotArmedBySkippedParticipants verifies that participants which
// never reached an upstream (config-static skips, exhausted cursors whose
// recorded errors are all skips) do NOT arm the wait caps: the round must
// keep waiting for the only real participant even though it is far slower
// than both caps.
func TestWaitCap_NotArmedBySkippedParticipants(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(3).
		WithAgreementThreshold(3). // disable short-circuit; the collection loop must decide
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		WithMaxWaitOnEmpty(common.NewStaticDuration(30 * time.Millisecond)).
		WithMaxWaitOnResult(common.NewStaticDuration(30 * time.Millisecond)).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		switch idx {
		case 1:
			// Config-static skip — produced locally in microseconds.
			return nil, skippedParticipantErr("eth_getLogs", "archive-1")
		case 2:
			// A slot that exhausted the shared cursor after burning its
			// retries on skipped upstreams — also produced in microseconds.
			return nil, common.NewErrFailsafeRetryExceeded(common.ScopeNetwork,
				common.NewErrUpstreamsExhaustedWithCause(errors.Join(
					skippedParticipantErr("eth_getLogs", "archive-1"),
					skippedParticipantErr("eth_getLogs", "archive-2"),
				)), nil)
		default:
			// The only real participant — far beyond both caps.
			time.Sleep(150 * time.Millisecond)
			return validResponseWithValue("0xreal"), nil
		}
	})
	elapsed := time.Since(start)

	require.NoError(t, err,
		"instant skips must not resolve the round with only infrastructure errors")
	require.NotNil(t, resp)
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond,
		"round must wait for the real participant; skips must not start the countdown")
}

// TestWaitCap_AllParticipantsSkipped_ResolvesImmediately verifies that when
// every participant skips, the round still terminates promptly: the loop
// collects all (instant) responses and resolves without ever needing a cap.
func TestWaitCap_AllParticipantsSkipped_ResolvesImmediately(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(3).
		WithAgreementThreshold(1).
		// Deliberately long caps: termination must come from collecting all
		// responses, not from any timer.
		WithMaxWaitOnEmpty(common.NewStaticDuration(10 * time.Second)).
		WithMaxWaitOnResult(common.NewStaticDuration(10 * time.Second)).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	_, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		return nil, skippedParticipantErr("eth_getLogs", "archive-"+string(rune('0'+idx)))
	})
	elapsed := time.Since(start)

	require.Error(t, err, "an all-skips round has no result to return")
	assert.Less(t, elapsed, 2*time.Second,
		"all-skips round must resolve as soon as every participant has responded")
}

// TestWaitCap_ArmedByRealAttemptFailures pins the intended cap semantics:
// a genuine attempt failure (connection reset, 5xx, timeout) proves the round
// is live and MUST still arm maxWaitOnEmpty, bounding a slow straggler.
func TestWaitCap_ArmedByRealAttemptFailures(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(2).
		WithAgreementThreshold(2).
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		WithMaxWaitOnEmpty(common.NewStaticDuration(50 * time.Millisecond)).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var slot atomic.Int32
	start := time.Now()
	_, _ = pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		idx := slot.Add(1)
		if idx == 1 {
			// Real attempt failure: a plain transport error, not a skip.
			return nil, errors.New("connection reset by peer")
		}
		time.Sleep(2 * time.Second)
		return validResponseWithValue("0xslow"), nil
	})
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 800*time.Millisecond,
		"a real attempt failure must arm maxWaitOnEmpty and bound the straggler")
}

// TestIsNoAttemptError pins the classification used by considerWaitCap.
func TestIsNoAttemptError(t *testing.T) {
	req := newTestRequest()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"method ignored", common.NewErrUpstreamMethodIgnored("eth_call", "u1"), true},
		{"request skipped wrapping method ignored", skippedParticipantErr("eth_call", "u1"), true},
		{"shadow upstream", common.NewErrUpstreamShadowing("u1"), true},
		{"syncing upstream", common.NewErrUpstreamSyncing("u1"), true},
		{"use-upstream directive mismatch", common.NewErrUpstreamNotAllowed("u9*", "u1"), true},
		{"no upstreams left to select", common.NewErrNoUpstreamsLeftToSelect(req, "all consumed"), true},
		{"exhausted with no recorded errors", common.NewErrUpstreamsExhaustedWithCause(nil), true},
		{"exhausted joining only skips", common.NewErrUpstreamsExhaustedWithCause(errors.Join(
			skippedParticipantErr("eth_call", "u1"),
			skippedParticipantErr("eth_call", "u2"),
		)), true},
		{"exhausted joining a skip and a real failure", common.NewErrUpstreamsExhaustedWithCause(errors.Join(
			skippedParticipantErr("eth_call", "u1"),
			errors.New("connection reset by peer"),
		)), false},
		{"retry-exceeded wrapping exhausted skips", common.NewErrFailsafeRetryExceeded(common.ScopeNetwork,
			common.NewErrUpstreamsExhaustedWithCause(errors.Join(
				skippedParticipantErr("eth_call", "u1"),
			)), nil), true},
		{"retry-exceeded wrapping a real failure", common.NewErrFailsafeRetryExceeded(common.ScopeNetwork,
			errors.New("boom"), nil), false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isNoAttemptError(tc.err))
		})
	}

	t.Run("successful result is an attempt", func(t *testing.T) {
		assert.False(t, isNoAttemptResult(&execResult{Result: validResponseWithValue("0x1")}))
	})
	t.Run("nil result is not an attempt", func(t *testing.T) {
		assert.True(t, isNoAttemptResult(nil))
	})
}
