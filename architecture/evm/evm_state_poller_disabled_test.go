package evm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// TestStatePollerNegativeIntervalDisables: a negative statePollerInterval
// survives SetDefaults (0 would become the 30s default) and Bootstrap then
// starts no poller, so a metered upstream is never polled in the background.
func TestStatePollerNegativeIntervalDisables(t *testing.T) {
	evmCfg := &common.EvmUpstreamConfig{ChainId: 1, StatePollerInterval: common.Duration(-time.Second)}
	require.NoError(t, evmCfg.SetDefaults(nil))
	require.Less(t, evmCfg.StatePollerInterval.Duration(), time.Duration(0))

	up := newSuggestGateUpstream(1, "0x1", nil)
	up.cfg.Evm = evmCfg
	p := newGateTestPoller(t, up)
	require.NoError(t, p.Bootstrap(context.Background()))
	require.False(t, p.Enabled, "poller must not start")
	require.Zero(t, p.LatestBlock())

	// Heads pushed by a WebSocket subscription are still accepted.
	p.SuggestLatestBlock(100)
	require.Equal(t, int64(100), p.LatestBlock())
}

// TestStatePollerZeroIntervalGetsDefault documents why 0 cannot disable it.
func TestStatePollerZeroIntervalGetsDefault(t *testing.T) {
	evmCfg := &common.EvmUpstreamConfig{ChainId: 1}
	require.NoError(t, evmCfg.SetDefaults(nil))
	require.Equal(t, 30*time.Second, evmCfg.StatePollerInterval.Duration())
}
