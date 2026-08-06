package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stateProvenUpstream(latest int64) *Upstream {
	return &Upstream{
		config: &common.UpstreamConfig{
			Id:   "u1",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{},
		},
		logger:         &zerolog.Logger{},
		evmStatePoller: &mockEvmStatePoller{latestBlock: latest, finalizedBlock: latest - 64},
	}
}

// The state-proven confidence exists because a node's CLAIMED head says nothing
// about its state trie: nodes answer eth_call from older state while reporting
// a current head. The boundary must therefore be the proven head — and fall
// back to the claimed head only while nothing is proven at all (probing off,
// warming up, or unsupported), so capability gaps degrade to today's behavior
// instead of browning out traffic.
func TestEvmAssertBlockAvailability_StateProven(t *testing.T) {
	t.Run("falls back to the claimed head while nothing is proven", func(t *testing.T) {
		u := stateProvenUpstream(1000)
		ok, err := u.EvmAssertBlockAvailability(context.Background(), "eth_call", common.AvailbilityConfidenceStateProven, false, 900)
		require.NoError(t, err)
		assert.True(t, ok, "proven=0 must behave exactly like blockHead — the gate cannot punish unsupported upstreams")
	})

	t.Run("blocks beyond the proven head are refused once proof exists", func(t *testing.T) {
		u := stateProvenUpstream(1000)
		u.EvmSetStateProvenBlock(950)
		ok, err := u.EvmAssertBlockAvailability(context.Background(), "eth_call", common.AvailbilityConfidenceStateProven, false, 980)
		require.NoError(t, err)
		assert.False(t, ok, "the node CLAIMS 1000 but has only PROVEN 950 — 980 must not route here")
	})

	t.Run("blocks at or below the proven head are served", func(t *testing.T) {
		u := stateProvenUpstream(1000)
		u.EvmSetStateProvenBlock(950)
		for _, n := range []int64{950, 949, 900} {
			ok, err := u.EvmAssertBlockAvailability(context.Background(), "eth_call", common.AvailbilityConfidenceStateProven, false, n)
			require.NoError(t, err)
			assert.True(t, ok, "block %d is within proof", n)
		}
	})

	t.Run("the proven head is monotonic — a stale probe cannot move it back", func(t *testing.T) {
		u := stateProvenUpstream(1000)
		u.EvmSetStateProvenBlock(950)
		u.EvmSetStateProvenBlock(940) // late/racing probe result
		assert.EqualValues(t, 950, u.EvmStateProvenBlock())
	})

	t.Run("the recent-state lower bound still applies under proof", func(t *testing.T) {
		u := stateProvenUpstream(1000)
		u.config.Evm.MaxAvailableRecentBlocks = 128
		u.EvmSetStateProvenBlock(1000)
		ok, err := u.EvmAssertBlockAvailability(context.Background(), "eth_call", common.AvailbilityConfidenceStateProven, false, 500)
		require.NoError(t, err)
		assert.False(t, ok, "a full node's state only reaches back maxAvailableRecentBlocks; proof of the tip does not extend history")
	})
}
