package upstream

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// expiredEctx returns a context whose WithTimeoutCause deadline has already
// elapsed by the time it's returned. cancel must be called by the caller.
func expiredEctx(cause error) (context.Context, context.CancelFunc) {
	return context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Millisecond), cause)
}

func TestInjectDynamicTimeoutSentinelIfDeadlinePassed(t *testing.T) {
	t.Run("welds sentinel into ErrEndpointRequestTimeout that lost it", func(t *testing.T) {
		ectx, cancel := expiredEctx(common.ErrDynamicTimeoutExceeded)
		defer cancel()

		// Inner err that LOST the sentinel — cause is a bare context.DeadlineExceeded.
		// This mirrors what the batched HTTP client produces under the race.
		err := common.NewErrEndpointRequestTimeout(100*time.Millisecond, context.DeadlineExceeded)
		assert.False(t, errors.Is(err, common.ErrDynamicTimeoutExceeded), "precondition: inner err should not yet contain sentinel")

		got := injectDynamicTimeoutSentinelIfDeadlinePassed(err, ectx, true)

		assert.True(t, errors.Is(got, common.ErrDynamicTimeoutExceeded),
			"after welding, errors.Is should locate the sentinel — TranslateFailsafeError relies on this")
		// The original DeadlineExceeded is still reachable so downstream classification
		// (e.g. ErrEndpointRequestTimeout code) survives.
		assert.True(t, errors.Is(got, context.DeadlineExceeded),
			"original cause must remain in chain alongside the welded sentinel")
		assert.True(t, common.HasErrorCode(got, common.ErrCodeEndpointRequestTimeout),
			"original error code must be preserved")
	})

	t.Run("idempotent when err already carries the sentinel", func(t *testing.T) {
		ectx, cancel := expiredEctx(common.ErrDynamicTimeoutExceeded)
		defer cancel()

		err := common.NewErrEndpointRequestTimeout(100*time.Millisecond, common.ErrDynamicTimeoutExceeded)
		base := err.(common.StandardError).Base()
		causeBefore := base.Cause

		got := injectDynamicTimeoutSentinelIfDeadlinePassed(err, ectx, true)

		assert.Same(t, causeBefore, got.(common.StandardError).Base().Cause,
			"cause pointer should not be mutated when sentinel already present")
		assert.True(t, errors.Is(got, common.ErrDynamicTimeoutExceeded))
	})

	t.Run("no-op when ourTimeoutSet is false", func(t *testing.T) {
		// When this scope did NOT install a WithTimeoutCause, we must not
		// claim the timeout — the parent scope (network) owns it.
		ectx, cancel := expiredEctx(common.ErrDynamicTimeoutExceeded)
		defer cancel()

		err := common.NewErrEndpointRequestTimeout(100*time.Millisecond, context.DeadlineExceeded)
		got := injectDynamicTimeoutSentinelIfDeadlinePassed(err, ectx, false)

		assert.False(t, errors.Is(got, common.ErrDynamicTimeoutExceeded))
	})

	t.Run("no-op when deadline has not yet passed", func(t *testing.T) {
		// ectx is canceled but the cause is something else; deadline far in
		// the future. A non-deadline cancellation is not our timeout firing.
		ectx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(10*time.Second), common.ErrDynamicTimeoutExceeded)
		defer cancel()

		err := common.NewErrEndpointRequestTimeout(100*time.Millisecond, context.DeadlineExceeded)
		got := injectDynamicTimeoutSentinelIfDeadlinePassed(err, ectx, true)

		assert.False(t, errors.Is(got, common.ErrDynamicTimeoutExceeded))
	})

	t.Run("no-op when ectx has no deadline", func(t *testing.T) {
		// e.g. a plain Background ctx — nothing for us to attribute.
		err := common.NewErrEndpointRequestTimeout(100*time.Millisecond, context.DeadlineExceeded)
		got := injectDynamicTimeoutSentinelIfDeadlinePassed(err, context.Background(), true)

		assert.False(t, errors.Is(got, common.ErrDynamicTimeoutExceeded))
	})

	t.Run("no-op when err is not timeout-shaped", func(t *testing.T) {
		// Transport failures (connection refused, etc.) must not be reclassified
		// as upstream-scope timeouts.
		ectx, cancel := expiredEctx(common.ErrDynamicTimeoutExceeded)
		defer cancel()

		dummyURL := &url.URL{Scheme: "http", Host: "rpc.example"}
		err := common.NewErrEndpointTransportFailure(dummyURL, errors.New("connection refused"))
		got := injectDynamicTimeoutSentinelIfDeadlinePassed(err, ectx, true)

		assert.False(t, errors.Is(got, common.ErrDynamicTimeoutExceeded))
	})

	t.Run("no-op when err is nil", func(t *testing.T) {
		ectx, cancel := expiredEctx(common.ErrDynamicTimeoutExceeded)
		defer cancel()

		got := injectDynamicTimeoutSentinelIfDeadlinePassed(nil, ectx, true)
		assert.NoError(t, got)
	})

	t.Run("preserves a non-sentinel pre-existing cause via errors.Join", func(t *testing.T) {
		ectx, cancel := expiredEctx(common.ErrDynamicTimeoutExceeded)
		defer cancel()

		originalCause := errors.New("upstream-specific failure")
		err := common.NewErrEndpointRequestTimeout(100*time.Millisecond, originalCause)

		got := injectDynamicTimeoutSentinelIfDeadlinePassed(err, ectx, true)

		// Both the original cause AND the sentinel must remain reachable
		// so neither downstream classification nor diagnostics regress.
		assert.True(t, errors.Is(got, originalCause), "original cause must survive welding")
		assert.True(t, errors.Is(got, common.ErrDynamicTimeoutExceeded), "welded sentinel must be reachable")
	})
}
