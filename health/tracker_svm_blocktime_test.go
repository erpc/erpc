package health

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestSvmWallClockSlotTimeEMA verifies that SVM head advances with timestamp=0
// warm GetNetworkBlockTime via wall-clock Δ / slot gap, while the same calls on
// an EVM network stay cold (on-chain timestamps required).
func TestSvmWallClockSlotTimeEMA(t *testing.T) {
	t.Run("SvmTimestampZeroWarmsEMA", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		ups := common.NewFakeUpstream("svm-a", common.WithFakeUpstreamNetworkID("svm:devnet"))

		slot := int64(10_000_000)
		tracker.SetLatestBlockNumber(ups, slot, 0)

		// Need ≥3 EMA samples after the first observation. Integer-second wall
		// clock means we must cross several Unix-second boundaries; advance a
		// few slots each tick so gap normalization recovers sub-second rates.
		// Do not stop on the first positive publication: Unix-second phase can
		// make the first EMA sample ~273ms (still correct gap normalization),
		// which would flake the ≤250ms ceiling. Keep sampling until the
		// estimate settles into the expected band or the deadline expires.
		const (
			minSlotTime = 10 * time.Millisecond
			maxSlotTime = 250 * time.Millisecond
		)
		deadline := time.Now().Add(5 * time.Second)
		var d time.Duration
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			slot += 3
			tracker.SetLatestBlockNumber(ups, slot, 0)
			d = tracker.GetNetworkBlockTime("svm:devnet")
			if d >= minSlotTime && d <= maxSlotTime {
				break
			}
		}

		require.Greater(t, d, time.Duration(0), "SVM wall-clock path should warm GetNetworkBlockTime")
		// Advances are +3 slots every ~50ms → ~17ms/slot before integer-second
		// quantization. Treating each second-tick as one "block" (no gap
		// division) yields ~1s — well above this ceiling. Cap is loose enough
		// for slow CI scheduling but still catches that class of bug.
		assert.GreaterOrEqual(t, d, minSlotTime, "should be within EMA sanity floor")
		assert.LessOrEqual(t, d, maxSlotTime, "should stay near Δt/Δslots, not ~1s poller cadence")
		// Tip-age timestamp must stay unset — wall-clock is not an on-chain tip ts.
		assert.Equal(t, int64(0), networkLatestTimestamp(tracker, "svm:devnet"))
		// Shared consumers (failsafe block-unavailable delay, selection lag
		// seconds, …) use the same GetNetworkBlockTime estimate. Once warm,
		// the default delay multiplier yields a positive retry delay — the
		// cold path previously always returned 0 on SVM.
		delay := time.Duration(float64(d) * common.DefaultBlockUnavailableDelayMultiplier)
		assert.Greater(t, delay, time.Duration(0),
			"warmed SVM slot-time must produce a positive block-unavailable delay")
	})

	t.Run("EvmTimestampZeroDoesNotWarmEMA", func(t *testing.T) {
		tracker := NewTracker(&log.Logger, "test-project", 5*time.Minute)
		ups := common.NewFakeUpstream("evm-a") // default network id evm:123
		net := ups.NetworkId()

		// Several head advances with timestamp=0 must not enter the wall-clock
		// sampler at all. Asserting the EMA anchor stays zero is stronger than
		// waiting for Unix-second ticks (publishing needs ≥3 advancing samples,
		// so a short sleep cannot prove the gate).
		for i := int64(1); i <= 10; i++ {
			tracker.SetLatestBlockNumber(ups, 1_000_000+i*3, 0)
		}

		ntwMeta := tracker.getMetadata(metadataKey{nil, net})
		assert.Equal(t, int64(0), ntwMeta.evmBlockTimePrevBlock,
			"EVM ts=0 path must not anchor the block-time sampler")
		assert.Equal(t, int64(0), ntwMeta.evmBlockTimePrevTimestamp)
		assert.Equal(t, 0, ntwMeta.evmBlockTimeSamples)
		assert.Equal(t, time.Duration(0), tracker.GetNetworkBlockTime(net),
			"EVM OnValue(..., 0) must not attribute local wall-clock to block-time EMA")
	})
}
