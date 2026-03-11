package evm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestComputePollBackoff(t *testing.T) {
	eth := 12 * time.Second        // Ethereum ~12s blocks
	base := 2 * time.Second        // Base ~2s blocks
	arb := 250 * time.Millisecond  // Arbitrum ~250ms blocks
	defaultInt := 30 * time.Second

	// --- Positive delay: block not yet due ---

	t.Run("BlockNotYetDue_ReturnsDelay", func(t *testing.T) {
		d := computePollBackoff(5*time.Second, eth, defaultInt)
		assert.Equal(t, 5*time.Second, d)
	})

	t.Run("DelayExceedsDefault_Capped", func(t *testing.T) {
		d := computePollBackoff(60*time.Second, eth, defaultInt)
		assert.Equal(t, defaultInt, d)
	})

	t.Run("NearThreshold_SmallPositiveDelay", func(t *testing.T) {
		// 30ms until expected — below minPollInterval (50ms).
		// Enters the backoff path with overdue ~= 0.
		d := computePollBackoff(30*time.Millisecond, eth, defaultInt)
		assert.Equal(t, 1200*time.Millisecond, d,
			"small positive delay below 50ms should use base retry")
	})

	// --- Phase 1: Normal jitter (overdue < 5× blockTime) ---
	// Cap at blockTime/2 for responsiveness.

	t.Run("ETH_JustDue_BaseRetry", func(t *testing.T) {
		// overdue=0, retry = 12s/10 + 0 = 1.2s
		d := computePollBackoff(0, eth, defaultInt)
		assert.Equal(t, 1200*time.Millisecond, d)
	})

	t.Run("ETH_SlightlyLate_BaseRetry", func(t *testing.T) {
		// 500ms late, retry = 1.2s + 50ms = 1.25s
		d := computePollBackoff(-500*time.Millisecond, eth, defaultInt)
		assert.Equal(t, 1250*time.Millisecond, d)
	})

	t.Run("ETH_ModeratelyLate_GrowsRetry", func(t *testing.T) {
		// 6s late, retry = 1.2s + 600ms = 1.8s (still < 5× blockTime)
		d := computePollBackoff(-6*time.Second, eth, defaultInt)
		assert.Equal(t, 1800*time.Millisecond, d)
	})

	t.Run("ETH_NormalJitter_CappedAtHalf", func(t *testing.T) {
		// 48s late (4× blockTime, still in normal jitter phase).
		// retry = 1.2s + 4.8s = 6s → capped at blockTime/2 = 6s
		d := computePollBackoff(-48*time.Second, eth, defaultInt)
		assert.Equal(t, 6*time.Second, d)
	})

	t.Run("Base_JustDue_BaseRetry", func(t *testing.T) {
		d := computePollBackoff(0, base, defaultInt)
		assert.Equal(t, 200*time.Millisecond, d)
	})

	t.Run("Base_3sLate_GrowsRetry", func(t *testing.T) {
		// 3s late, retry = 200ms + 300ms = 500ms
		d := computePollBackoff(-3*time.Second, base, defaultInt)
		assert.Equal(t, 500*time.Millisecond, d)
	})

	t.Run("Arb_JustDue_Floor", func(t *testing.T) {
		// Arb 250ms: base retry = 25ms → floor at 50ms
		d := computePollBackoff(0, arb, defaultInt)
		assert.Equal(t, 50*time.Millisecond, d)
	})

	t.Run("Arb_Late_StillInNormalPhase", func(t *testing.T) {
		// 1s late (4× blockTime, still in normal jitter).
		// retry = 25ms + 100ms = 125ms → cap at blockTime/2 = 125ms
		d := computePollBackoff(-1*time.Second, arb, defaultInt)
		assert.Equal(t, 125*time.Millisecond, d)
	})

	// --- Phase 2: Extended outage (overdue ≥ 5× blockTime) ---
	// Retry grows via overdue/10 toward defaultInterval.

	t.Run("ETH_ExtendedOutage_GrowsPastHalf", func(t *testing.T) {
		// 60s late (5× blockTime), enters extended outage phase.
		// retry = 1.2s + 6s = 7.2s (no longer capped at 6s)
		d := computePollBackoff(-60*time.Second, eth, defaultInt)
		assert.Equal(t, 7200*time.Millisecond, d)
	})

	t.Run("ETH_LongOutage_ApproachesDefault", func(t *testing.T) {
		// 5 min late. retry = 1.2s + 30s = 31.2s → capped at defaultInterval.
		d := computePollBackoff(-5*time.Minute, eth, defaultInt)
		assert.Equal(t, defaultInt, d)
	})

	t.Run("Base_ExtendedOutage_GrowsPastHalf", func(t *testing.T) {
		// 30s late (15× blockTime). retry = 200ms + 3s = 3.2s
		d := computePollBackoff(-30*time.Second, base, defaultInt)
		assert.Equal(t, 3200*time.Millisecond, d)
	})

	t.Run("Base_LongOutage_ApproachesDefault", func(t *testing.T) {
		// 5 min late. retry = 200ms + 30s = 30.2s → capped at defaultInterval.
		d := computePollBackoff(-5*time.Minute, base, defaultInt)
		assert.Equal(t, defaultInt, d)
	})

	t.Run("Arb_ExtendedOutage_GrowsPastHalf", func(t *testing.T) {
		// 10s late (40× blockTime). retry = 25ms + 1s = 1.025s
		d := computePollBackoff(-10*time.Second, arb, defaultInt)
		assert.Equal(t, 1025*time.Millisecond, d)
	})

	t.Run("Arb_LongOutage_ApproachesDefault", func(t *testing.T) {
		// 5 min late. retry = 25ms + 30s = 30.025s → capped at defaultInterval.
		d := computePollBackoff(-5*time.Minute, arb, defaultInt)
		assert.Equal(t, defaultInt, d)
	})

	// --- Resource usage under sustained outage ---

	t.Run("TotalPollCount_ETH_1BlockPeriodLate", func(t *testing.T) {
		// Count polls for 1 block period of lateness.
		totalPolls := 0
		overdue := time.Duration(0)
		for overdue < 12*time.Second {
			retry := computePollBackoff(-overdue, eth, defaultInt)
			overdue += retry
			totalPolls++
		}
		assert.Less(t, totalPolls, 15,
			"should need fewer than 15 polls for 1 block period of lateness")
		assert.Greater(t, totalPolls, 3,
			"should still be responsive (more than 3 polls)")
		t.Logf("ETH 12s late: %d polls (was ~240 with fixed 50ms)", totalPolls)
	})

	t.Run("Arb_5MinOutage_ReasonablePollCount", func(t *testing.T) {
		// If Arb upstream is down for 5 minutes, how many polls do we burn?
		// Old behavior (capped at 125ms): 5min / 125ms = 2400 polls.
		// New behavior: should be far fewer as retry grows toward 30s.
		totalPolls := 0
		overdue := time.Duration(0)
		for overdue < 5*time.Minute {
			retry := computePollBackoff(-overdue, arb, defaultInt)
			overdue += retry
			totalPolls++
		}
		assert.Less(t, totalPolls, 100,
			"5 min Arb outage should not burn hundreds of polls")
		t.Logf("Arb 5min outage: %d polls (was ~2400 with old blockTime/2 cap)", totalPolls)
	})

	t.Run("Base_5MinOutage_ReasonablePollCount", func(t *testing.T) {
		totalPolls := 0
		overdue := time.Duration(0)
		for overdue < 5*time.Minute {
			retry := computePollBackoff(-overdue, base, defaultInt)
			overdue += retry
			totalPolls++
		}
		assert.Less(t, totalPolls, 100,
			"5 min Base outage should not burn hundreds of polls")
		t.Logf("Base 5min outage: %d polls (was ~300 with old blockTime/2 cap)", totalPolls)
	})

	t.Run("ETH_30MinOutage_ReasonablePollCount", func(t *testing.T) {
		totalPolls := 0
		overdue := time.Duration(0)
		for overdue < 30*time.Minute {
			retry := computePollBackoff(-overdue, eth, defaultInt)
			overdue += retry
			totalPolls++
		}
		assert.Less(t, totalPolls, 100,
			"30 min ETH outage should be well-bounded")
		t.Logf("ETH 30min outage: %d polls (was ~300 with old blockTime/2 cap)", totalPolls)
	})
}
