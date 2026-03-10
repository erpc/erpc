package evm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestComputePollBackoff(t *testing.T) {
	eth := 12 * time.Second   // Ethereum ~12s blocks
	base := 2 * time.Second   // Base ~2s blocks
	arb := 250 * time.Millisecond // Arbitrum ~250ms blocks
	defaultInt := 30 * time.Second

	t.Run("BlockNotYetDue_ReturnsDelay", func(t *testing.T) {
		// 5s until expected block → just wait.
		d := computePollBackoff(5*time.Second, eth, defaultInt)
		assert.Equal(t, 5*time.Second, d)
	})

	t.Run("DelayExceedsDefault_Capped", func(t *testing.T) {
		d := computePollBackoff(60*time.Second, eth, defaultInt)
		assert.Equal(t, defaultInt, d)
	})

	t.Run("ETH_JustDue_BaseRetry", func(t *testing.T) {
		// Block just due (delay ~0). Should get base retry = 12s/10 = 1.2s.
		d := computePollBackoff(0, eth, defaultInt)
		assert.Equal(t, 1200*time.Millisecond, d)
	})

	t.Run("ETH_SlightlyLate_BaseRetry", func(t *testing.T) {
		// 500ms late. overdue=500ms, retry = 1.2s + 50ms = 1.25s.
		d := computePollBackoff(-500*time.Millisecond, eth, defaultInt)
		assert.Equal(t, 1250*time.Millisecond, d)
	})

	t.Run("ETH_ModeratelyLate_GrowsRetry", func(t *testing.T) {
		// 6s late. overdue=6s, retry = 1.2s + 600ms = 1.8s.
		d := computePollBackoff(-6*time.Second, eth, defaultInt)
		assert.Equal(t, 1800*time.Millisecond, d)
	})

	t.Run("ETH_VeryLate_CappedAtHalfBlockTime", func(t *testing.T) {
		// 60s late. overdue=60s, retry = 1.2s + 6s = 7.2s → capped at 6s.
		d := computePollBackoff(-60*time.Second, eth, defaultInt)
		assert.Equal(t, 6*time.Second, d)
	})

	t.Run("Base_JustDue_BaseRetry", func(t *testing.T) {
		// Base 2s: base retry = 200ms.
		d := computePollBackoff(0, base, defaultInt)
		assert.Equal(t, 200*time.Millisecond, d)
	})

	t.Run("Base_3sLate_GrowsRetry", func(t *testing.T) {
		// 3s late. overdue=3s, retry = 200ms + 300ms = 500ms.
		d := computePollBackoff(-3*time.Second, base, defaultInt)
		assert.Equal(t, 500*time.Millisecond, d)
	})

	t.Run("Base_VeryLate_CappedAtHalfBlockTime", func(t *testing.T) {
		// 30s late. retry = 200ms + 3s = 3.2s → capped at 1s.
		d := computePollBackoff(-30*time.Second, base, defaultInt)
		assert.Equal(t, 1*time.Second, d)
	})

	t.Run("Arb_JustDue_Floor", func(t *testing.T) {
		// Arb 250ms: base retry = 25ms → floor at 50ms.
		d := computePollBackoff(0, arb, defaultInt)
		assert.Equal(t, 50*time.Millisecond, d)
	})

	t.Run("Arb_Late_Floor", func(t *testing.T) {
		// 1s late. retry = 25ms + 100ms = 125ms → cap at 125ms (blockTime/2).
		d := computePollBackoff(-1*time.Second, arb, defaultInt)
		assert.Equal(t, 125*time.Millisecond, d)
	})

	t.Run("Arb_VeryLate_CappedAtHalfBlockTime", func(t *testing.T) {
		// 10s late. retry = 25ms + 1s → capped at 125ms.
		d := computePollBackoff(-10*time.Second, arb, defaultInt)
		assert.Equal(t, 125*time.Millisecond, d)
	})

	t.Run("NearThreshold_SmallPositiveDelay", func(t *testing.T) {
		// 30ms until expected — below minPollInterval (50ms).
		// Should enter the backoff path with overdue ~= 0.
		d := computePollBackoff(30*time.Millisecond, eth, defaultInt)
		assert.Equal(t, 1200*time.Millisecond, d,
			"small positive delay below 50ms should use base retry")
	})

	t.Run("TotalPollCount_ETH_1BlockPeriodLate", func(t *testing.T) {
		// Count how many polls would happen if ETH block is 12s late.
		// With backoff: fewer polls than the old flat 50ms (was ~240).
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
}
