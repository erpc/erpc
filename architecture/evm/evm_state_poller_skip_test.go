package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// skipStatePoller starts no poller, so a metered upstream is never polled in
// the background; heads pushed by a WebSocket subscription still land.
func TestStatePollerSkip(t *testing.T) {
	evmCfg := &common.EvmUpstreamConfig{ChainId: 1, SkipStatePoller: util.BoolPtr(true)}
	require.NoError(t, evmCfg.SetDefaults(nil))
	require.NotZero(t, evmCfg.StatePollerInterval, "the interval keeps its default")

	up := newSuggestGateUpstream(1, "0x1", nil)
	up.cfg.Evm = evmCfg
	p := newGateTestPoller(t, up)
	require.NoError(t, p.Bootstrap(context.Background()))
	require.False(t, p.Enabled, "poller must not start")
	require.Zero(t, p.LatestBlock())

	p.SuggestLatestBlock(100)
	require.Equal(t, int64(100), p.LatestBlock())
}

func TestStatePollerSkipInheritsFromUpstreamDefaults(t *testing.T) {
	defaults := &common.EvmUpstreamConfig{SkipStatePoller: util.BoolPtr(true)}
	inherits := &common.EvmUpstreamConfig{ChainId: 1}
	require.NoError(t, inherits.SetDefaults(defaults))
	require.True(t, *inherits.SkipStatePoller)

	overrides := &common.EvmUpstreamConfig{ChainId: 1, SkipStatePoller: util.BoolPtr(false)}
	require.NoError(t, overrides.SetDefaults(defaults))
	require.False(t, *overrides.SkipStatePoller)
}
