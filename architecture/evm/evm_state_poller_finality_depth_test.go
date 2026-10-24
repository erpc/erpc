package evm

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driveBlockTimeEMA settles the tracker's block-time estimate at
// (tsDelta/numDelta) seconds per block by replaying observations through the
// same entry point the poller uses. The first sample lands on the third
// observation, so a handful of rounds is plenty.
func driveBlockTimeEMA(t *testing.T, p *EvmStatePoller, up common.Upstream, numDelta, tsDelta int64) {
	t.Helper()
	var num int64 = 1_000_000
	var ts int64 = 1_700_000_000
	for range 8 {
		num += numDelta
		ts += tsDelta
		p.tracker.SetLatestBlockNumber(up, num, ts)
	}
	require.NotZero(t, p.tracker.GetNetworkBlockTime(up.NetworkId()),
		"precondition: the block-time EMA must have settled")
}

func TestFallbackFinalityDepth(t *testing.T) {
	t.Run("UnknownBlockTimeReadsTheColdBlockTime", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)

		// Cold start: no measurement, so 30 minutes counts at the cold 1s
		// block time every reader uses, deeper than the configured 1024.
		require.Zero(t, p.tracker.GetNetworkBlockTime(up.NetworkId()))
		assert.Equal(t, int64(1800), p.fallbackFinalityDepth())
	})

	t.Run("SlowChainKeepsTheBlockDepth", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)
		driveBlockTimeEMA(t, p, up, 1, 12) // 12s blocks

		// 30 minutes is 150 blocks here — shallower than the 1024 already
		// configured, so the deeper of the two leaves this chain untouched.
		assert.Equal(t, int64(common.DefaultEvmFinalityDepth), p.fallbackFinalityDepth())
	})

	t.Run("FastChainDeepensToTheMinimumAge", func(t *testing.T) {
		up := newSuggestGateUpstream(123, "0x7b", nil)
		p := newGateTestPoller(t, up)
		driveBlockTimeEMA(t, p, up, 10, 1) // 100ms blocks

		// 1024 blocks is only ~1.7 minutes of this chain — potentially shallower
		// than its real reorg risk. 30 minutes is 18,000 blocks at 100ms; the
		// block time floor may read the blocks seen shorter than 100ms, which
		// only deepens it, but never to twice the minimum age.
		d := p.fallbackFinalityDepth()
		assert.GreaterOrEqual(t, time.Duration(d)*100*time.Millisecond, 30*time.Minute)
		assert.Less(t, d, int64(2*18_000))
	})
}
