package failsafe

import (
	"context"
	"math/rand"
	"time"

	"github.com/erpc/erpc/common"
)

// ComputeBackoff returns the delay before the next retry attempt for the
// given attempt index (0-based: attempt 0 is the first retry, etc.).
//
// The math is: fixed Delay → exponential factor capped at BackoffMaxDelay
// → additive jitter in [0, Jitter). When cfg is nil or Delay <= 0 the
// result is 0 (no delay).
//
// This is a pure function — it has no knowledge of which error triggered
// the retry. Special-case delays (EmptyResultDelay, BlockUnavailableDelay)
// are the caller's responsibility; they bypass ComputeBackoff entirely.
func ComputeBackoff(cfg *common.RetryPolicyConfig, attempt int) time.Duration {
	if cfg == nil {
		return 0
	}
	base := cfg.Delay.Duration()
	if base <= 0 {
		return 0
	}

	d := base
	if cfg.BackoffFactor > 0 && attempt > 0 {
		factor := float64(1)
		for i := 0; i < attempt; i++ {
			factor *= float64(cfg.BackoffFactor)
		}
		d = time.Duration(float64(base) * factor)
	}

	if maxd := cfg.BackoffMaxDelay.Duration(); maxd > 0 && d > maxd {
		d = maxd
	}

	if jt := cfg.Jitter.Duration(); jt > 0 {
		// nolint:gosec // jitter doesn't need crypto randomness
		d += time.Duration(rand.Int63n(int64(jt)))
	}

	return d
}

// SleepCtx sleeps for the given duration or returns ctx.Err() if the
// context is canceled first. A zero (or negative) duration returns
// immediately without checking the context.
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
