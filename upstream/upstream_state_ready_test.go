package upstream

import (
	"context"
	"math"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// makeUpstream constructs a minimal Upstream for EvmAssertBlockAvailability
// tests. Each test supplies its own state poller.
func makeUpstream(poller common.EvmStatePoller) *Upstream {
	logger := zerolog.Nop()
	return &Upstream{
		config: &common.UpstreamConfig{
			Id:   "test-upstream",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{},
		},
		logger:         &logger,
		evmStatePoller: poller,
		networkId:      util.AtomicValue("evm:137"),
	}
}

// TestEvmAssertBlockAvailability_StateReady_HeadVsTrieRace mirrors the
// production trace a96e11c29cdf5be9e26b05c7c15773b8: an upstream advanced its
// head pointer to block N (claiming availability) while its state DB had not
// committed slots for block N, causing eth_getBalance to silently return 0x0.
//
// With AvailbilityConfidenceStateReady, the upstream's state-readiness probe
// keeps StateReadyBlock at N-1, so EvmAssertBlockAvailability rejects N and
// the retry shortcut in upstream/failsafe.go is no longer fooled by a fresh
// head pointer when the state is stale.
func TestEvmAssertBlockAvailability_StateReady_HeadVsTrieRace(t *testing.T) {
	t.Run("HeadAheadOfStateReady_StateReadyConfidence_Rejects", func(t *testing.T) {
		// Simulates the moment of the captured trace: head pointer advanced
		// to 86811349 but state DB only consistent through 86811348.
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:             86811349,
			finalizedBlock:          86811000,
			stateReadyBlockOverride: 86811348,
		}
		up := makeUpstream(poller)

		// BlockHead confidence accepts the request because the head pointer
		// is at the requested block — legacy behavior, must NOT regress.
		canHead, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceBlockHead, false, 86811349)
		assert.NoError(t, err)
		assert.True(t, canHead,
			"BlockHead confidence trusts the head pointer alone — legacy behavior")

		// StateReady confidence consults the probe and rejects.
		canReady, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 86811349)
		assert.NoError(t, err)
		assert.False(t, canReady,
			"StateReady rejects when stateReadyBlock < requested block")
	})

	t.Run("StateReadyConfidence_AcceptsAtExactBoundary", func(t *testing.T) {
		// Block exactly equal to stateReadyBlock — must accept (boundary).
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:             86811349,
			finalizedBlock:          86811000,
			stateReadyBlockOverride: 86811349,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 86811349)
		assert.NoError(t, err)
		assert.True(t, can, "block == stateReadyBlock must be accepted")
	})

	t.Run("StateReadyConfidence_AcceptsAtEarlierBlock", func(t *testing.T) {
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:             86811349,
			finalizedBlock:          86811000,
			stateReadyBlockOverride: 86811348,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 86811348)
		assert.NoError(t, err)
		assert.True(t, can, "earlier block must be accepted")
	})

	t.Run("StateReadyConfidence_AcceptsZeroBlock", func(t *testing.T) {
		// blockNumber=0 (genesis) should be accepted under StateReady the same
		// way it is under BlockHead — boundary parity.
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:             86811349,
			finalizedBlock:          86811000,
			stateReadyBlockOverride: 86811349,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 0)
		assert.NoError(t, err)
		assert.True(t, can, "block=0 must be accepted (genesis)")
	})

	t.Run("StateReadyConfidence_AcceptsNegativeBlock", func(t *testing.T) {
		// Negative block numbers shouldn't crash and should follow the same
		// permissive semantics as BlockHead (mirrors existing edge-case test).
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:             86811349,
			finalizedBlock:          86811000,
			stateReadyBlockOverride: 86811349,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, -1)
		assert.NoError(t, err)
		assert.True(t, can, "negative block must follow BlockHead semantics")
	})

	t.Run("StateReadyConfidence_NoProbeOverride_FallsThroughToHead", func(t *testing.T) {
		// Without a probe override (mock's stateReadyBlockOverride=0), the
		// fake's StateReadyBlock() falls back to latestBlock. StateReady
		// confidence behaves identically to BlockHead — backward-compat.
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:    86811349,
			finalizedBlock: 86811000,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 86811349)
		assert.NoError(t, err)
		assert.True(t, can,
			"without configured probe, StateReady must be no-op equivalent to BlockHead")
	})

	t.Run("StateReadyConfidence_RejectsBlocksAheadOfStateReady_NoForceFresh", func(t *testing.T) {
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:             86811349,
			finalizedBlock:          86811000,
			stateReadyBlockOverride: 86811348,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 86811350)
		assert.NoError(t, err)
		assert.False(t, can)
		assert.Equal(t, 0, poller.pollStateReadyCallCount,
			"must not poll when forceFreshIfStale=false")
	})

	t.Run("StateReadyConfidence_ForcePollsWhenStale", func(t *testing.T) {
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:             86811349,
			finalizedBlock:          86811000,
			stateReadyBlockOverride: 86811348,
		}
		up := makeUpstream(poller)

		// stateReadyBlockOverride is sticky — PollStateReady returns the same
		// override. The request stays rejected but the poll fires.
		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, true, 86811349)
		assert.NoError(t, err)
		assert.False(t, can)
		assert.GreaterOrEqual(t, poller.pollStateReadyCallCount, 1,
			"must poll state-readiness probe at least once when forceFreshIfStale=true")
	})

	t.Run("StateReadyConfidence_ForceFresh_NoOpWhenNotStale", func(t *testing.T) {
		// When block <= stateReadyBlock from the start, force-fresh is unnecessary
		// and should NOT trigger a poll.
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:             86811349,
			finalizedBlock:          86811000,
			stateReadyBlockOverride: 86811349,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, true, 86811348)
		assert.NoError(t, err)
		assert.True(t, can)
		assert.Equal(t, 0, poller.pollStateReadyCallCount,
			"must not poll when block is already within stateReadyBlock")
	})
}

// TestEvmAssertBlockAvailability_StateReady_RegressionGuards confirms that
// adding the new confidence didn't break the existing confidences.
func TestEvmAssertBlockAvailability_StateReady_RegressionGuards(t *testing.T) {
	t.Run("BlockHead_StillWorks", func(t *testing.T) {
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:    1000,
			finalizedBlock: 900,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceBlockHead, false, 1000)
		assert.NoError(t, err)
		assert.True(t, can)

		can, err = up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceBlockHead, false, 1001)
		assert.NoError(t, err)
		assert.False(t, can, "block beyond latestBlock must be rejected")
	})

	t.Run("Finalized_StillWorks", func(t *testing.T) {
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:    1000,
			finalizedBlock: 900,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceFinalized, false, 900)
		assert.NoError(t, err)
		assert.True(t, can, "Finalized accepts block <= finalizedBlock")

		can, err = up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceFinalized, false, 950)
		assert.NoError(t, err)
		assert.False(t, can, "Finalized rejects unfinalized block")
	})

	t.Run("UnknownConfidence_StillErrors", func(t *testing.T) {
		poller := &mockEvmStatePollerEnhanced{
			latestBlock:    1000,
			finalizedBlock: 900,
		}
		up := makeUpstream(poller)

		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidence(999), false, 500)
		assert.False(t, can)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported block availability confidence")
	})
}

// TestEvmAssertBlockAvailability_StateReady_NilGuards confirms the new
// confidence's nil-guard paths match the existing confidences'.
func TestEvmAssertBlockAvailability_StateReady_NilGuards(t *testing.T) {
	t.Run("NilUpstream", func(t *testing.T) {
		var up *Upstream
		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 100)
		assert.False(t, can)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "upstream or config is nil")
	})

	t.Run("NilConfig", func(t *testing.T) {
		up := &Upstream{}
		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 100)
		assert.False(t, can)
		assert.Error(t, err)
	})

	t.Run("NonEvmUpstream", func(t *testing.T) {
		logger := zerolog.Nop()
		up := &Upstream{
			config: &common.UpstreamConfig{
				Id:   "non-evm",
				Type: "solana", // not EvmType
			},
			logger:         &logger,
			evmStatePoller: &mockEvmStatePollerEnhanced{latestBlock: 100},
			networkId:      util.AtomicValue("solana"),
		}
		can, err := up.EvmAssertBlockAvailability(context.Background(),
			"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 50)
		assert.False(t, can)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not an EVM type")
	})
}

// TestEvmAssertBlockAvailability_StateReady_LowerBound_Disabled exercises the
// interaction between the new confidence and the legacy MaxAvailableRecentBlocks
// lower-bound check.
func TestEvmAssertBlockAvailability_StateReady_LowerBound_Disabled(t *testing.T) {
	logger := zerolog.Nop()
	up := &Upstream{
		config: &common.UpstreamConfig{
			Id:   "test-upstream",
			Type: common.UpstreamTypeEvm,
			Evm: &common.EvmUpstreamConfig{
				MaxAvailableRecentBlocks: 0, // no lower bound
			},
		},
		logger: &logger,
		evmStatePoller: &mockEvmStatePollerEnhanced{
			latestBlock:             1000,
			finalizedBlock:          900,
			stateReadyBlockOverride: 1000,
		},
		networkId: util.AtomicValue("evm:1"),
	}

	can, err := up.EvmAssertBlockAvailability(context.Background(),
		"eth_getBalance", common.AvailbilityConfidenceStateReady, false, 1)
	assert.NoError(t, err)
	assert.True(t, can, "with no MaxAvailableRecentBlocks, any block <= stateReady is accepted")
}

// TestEvmAssertBlockAvailability_StateReady_MaxInt64 is a boundary smoke
// test — passing a max int64 block must reject without overflowing.
func TestEvmAssertBlockAvailability_StateReady_MaxInt64(t *testing.T) {
	up := makeUpstream(&mockEvmStatePollerEnhanced{
		latestBlock:             1000,
		finalizedBlock:          900,
		stateReadyBlockOverride: 1000,
	})

	can, err := up.EvmAssertBlockAvailability(context.Background(),
		"eth_getBalance", common.AvailbilityConfidenceStateReady, false, math.MaxInt64)
	assert.NoError(t, err)
	assert.False(t, can, "max int64 must be rejected, not overflow")
}
