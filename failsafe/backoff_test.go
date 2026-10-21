package failsafe

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeBackoff(t *testing.T) {
	ms := func(n int) common.Duration { return common.Duration(time.Duration(n) * time.Millisecond) }
	cases := []struct {
		name    string
		cfg     *common.RetryPolicyConfig
		attempt int
		want    time.Duration
	}{
		{"nil config", nil, 3, 0},
		{"zero delay", &common.RetryPolicyConfig{BackoffFactor: 2}, 3, 0},
		{"negative delay", &common.RetryPolicyConfig{Delay: ms(-10)}, 0, 0},
		{"fixed delay without factor", &common.RetryPolicyConfig{Delay: ms(100)}, 5, 100 * time.Millisecond},
		{"first retry ignores factor", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 2}, 0, 100 * time.Millisecond},
		{"exponential growth", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 2}, 3, 800 * time.Millisecond},
		{"fractional factor", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 1.5}, 2, 225 * time.Millisecond},
		{"capped at max delay", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 2, BackoffMaxDelay: ms(300)}, 3, 300 * time.Millisecond},
		{"below cap untouched", &common.RetryPolicyConfig{Delay: ms(100), BackoffFactor: 2, BackoffMaxDelay: ms(300)}, 1, 200 * time.Millisecond},
		{"cap applies to fixed delay", &common.RetryPolicyConfig{Delay: ms(500), BackoffMaxDelay: ms(50)}, 0, 50 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ComputeBackoff(tc.cfg, tc.attempt))
		})
	}
}

func TestComputeBackoff_JitterIsAdditiveAndBounded(t *testing.T) {
	cfg := &common.RetryPolicyConfig{
		Delay:           common.Duration(100 * time.Millisecond),
		BackoffFactor:   2,
		BackoffMaxDelay: common.Duration(150 * time.Millisecond),
		Jitter:          common.Duration(20 * time.Millisecond),
	}
	seen := map[time.Duration]bool{}
	for range 200 {
		d := ComputeBackoff(cfg, 2)
		// Jitter is added after the cap, in [0, Jitter).
		require.GreaterOrEqual(t, d, 150*time.Millisecond)
		require.Less(t, d, 170*time.Millisecond)
		seen[d] = true
	}
	assert.Greater(t, len(seen), 1, "jitter must vary the delay")
}

func TestSleepCtx(t *testing.T) {
	t.Run("non-positive duration returns immediately even when canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.NoError(t, SleepCtx(ctx, 0))
		assert.NoError(t, SleepCtx(ctx, -time.Second))
	})

	t.Run("elapses", func(t *testing.T) {
		start := time.Now()
		require.NoError(t, SleepCtx(context.Background(), 5*time.Millisecond))
		assert.GreaterOrEqual(t, time.Since(start), 5*time.Millisecond)
	})

	t.Run("canceled context interrupts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.ErrorIs(t, SleepCtx(ctx, time.Hour), context.Canceled)
	})

	t.Run("deadline interrupts", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		assert.ErrorIs(t, SleepCtx(ctx, time.Hour), context.DeadlineExceeded)
	})
}
