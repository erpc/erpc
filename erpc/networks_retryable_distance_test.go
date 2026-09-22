package erpc

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRetryDistanceNetwork builds the minimum Network the derivation reads:
// a config, a tracker and a network id. blocksPerSecond <= 0 leaves the
// tracker's estimate unset, which is the cold-start case.
func newRetryDistanceNetwork(t *testing.T, configured *int64, blocksPerSecond int64) *Network {
	t.Helper()
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", 2*time.Second)
	n := &Network{
		networkId:      "evm:1",
		projectId:      "test",
		logger:         &logger,
		cfg:            &common.NetworkConfig{Evm: &common.EvmNetworkConfig{MaxRetryableBlockDistance: configured}},
		metricsTracker: tracker,
	}
	if blocksPerSecond > 0 {
		// Settle the tracker's block-time EMA by replaying observations through
		// the same entry point the poller uses: blocksPerSecond blocks for every
		// one second of block timestamp. blockTimeMinSamples is 3, so eight
		// rounds is comfortably past the gate.
		up := common.NewFakeUpstream("ups-1", common.WithFakeUpstreamNetworkID("evm:1"))
		var num int64 = 1_000_000
		var ts int64 = 1_700_000_000
		for i := 0; i < 8; i++ {
			num += blocksPerSecond
			ts++
			tracker.SetLatestBlockNumber(up, num, ts)
		}
		require.NotZero(t, tracker.GetNetworkBlockTime("evm:1"),
			"precondition: the block-time EMA must have settled")
	}
	return n
}

func TestMaxRetryableBlockDistance(t *testing.T) {
	t.Run("UnknownBlockTimeKeepsTheBlockCountDefault", func(t *testing.T) {
		n := newRetryDistanceNetwork(t, nil, 0)
		assert.Equal(t, defaultMaxRetryableBlockDistance, n.maxRetryableBlockDistance())
	})

	t.Run("SlowChainKeepsTheBlockCountDefault", func(t *testing.T) {
		// 1 block/s: 60s of chain is 60 blocks, inside the 128 default, so the
		// larger of the two leaves this chain untouched.
		n := newRetryDistanceNetwork(t, nil, 1)
		assert.Equal(t, defaultMaxRetryableBlockDistance, n.maxRetryableBlockDistance())
	})

	t.Run("FastChainWidensToTheTimeHorizon", func(t *testing.T) {
		// 10 blocks/s: 128 blocks is only ~13 seconds of this chain, so a block
		// half a minute out would be refused a retry it would have won. 60s of
		// chain progress is 600 blocks.
		n := newRetryDistanceNetwork(t, nil, 10)
		assert.Equal(t, int64(600), n.maxRetryableBlockDistance())
		assert.Greater(t, n.maxRetryableBlockDistance(), defaultMaxRetryableBlockDistance,
			"widening is the safe direction: it can cost retries, never a failed request")
	})

	t.Run("ExplicitConfigIsNeverOverridden", func(t *testing.T) {
		// An operator who pins this has made a deliberate choice; the derived
		// floor must not widen it, even on a chain where it otherwise would.
		pinned := int64(4)
		n := newRetryDistanceNetwork(t, &pinned, 10)
		assert.Equal(t, int64(4), n.maxRetryableBlockDistance())
	})
}
