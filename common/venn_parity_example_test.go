package common

import (
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

// The shipped example must keep loading and validating with the documented
// coordination features switched on.
func TestVennParityExampleConfigLoads(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("CHEAP_RPC_URL", "http://cheap1.invalid:8545")
	t.Setenv("CHEAP2_RPC_URL", "http://cheap2.invalid:8545")
	t.Setenv("PAID_RPC_URL", "https://paid.invalid/key")
	cfg, err := LoadConfig(afero.NewOsFs(), "../erpc.venn-parity.example.yaml", &DefaultOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Projects)
	nw := cfg.Projects[0].Networks[0]
	require.NotNil(t, nw.CacheFill)
	require.True(t, nw.CacheFill.Enabled)
	require.NotNil(t, nw.Evm.HeadPolling)
	require.Equal(t, HeadPollingModeLease, nw.Evm.HeadPolling.Mode)
	require.NotNil(t, nw.Evm.HeadCache)
	require.Equal(t, HeadCacheModeShared, nw.Evm.HeadCache.Mode)
	require.Equal(t, HeadCacheHeadSourceServed, nw.Evm.HeadCache.HeadSource)
	require.NotNil(t, cfg.Server.WebSocket)
	require.Equal(t, 100, cfg.Server.WebSocket.MaxBatchSize)
	var prios []int
	for _, u := range cfg.Projects[0].Upstreams {
		prios = append(prios, u.EffectivePriority())
	}
	require.Equal(t, []int{0, 0, 10}, prios)
}
