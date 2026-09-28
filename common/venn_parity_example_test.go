package common

import (
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

// The shipped example must keep loading and validating: a Redis-backed head
// cache referencing a cache connector, the WebSocket endpoint, and hedging off
// so cheap-first selection never speculatively calls a paid upstream.
func TestVennParityExampleConfigLoads(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("CHEAP_RPC_URL", "http://cheap1.invalid:8545")
	t.Setenv("CHEAP2_RPC_URL", "http://cheap2.invalid:8545")
	t.Setenv("PAID_RPC_URL", "https://paid.invalid/key")
	cfg, err := LoadConfig(afero.NewOsFs(), "../erpc.venn-parity.example.yaml", &DefaultOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Projects)
	nw := cfg.Projects[0].Networks[0]
	require.NotNil(t, nw.Evm.HeadCache)
	require.True(t, nw.Evm.HeadCache.Enabled)
	require.NoError(t, nw.Evm.HeadCache.ValidateConnector(cfg))
	require.NotNil(t, cfg.Server.WebSocket)
	require.True(t, cfg.Server.WebSocket.Enabled)
	for _, fs := range nw.Failsafe {
		require.True(t, fs.Hedge == nil || fs.Hedge.MaxCount <= 0, "example must keep hedging off")
	}
}
