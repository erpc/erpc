package common

import (
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

// The shipped example must load with Redis-backed head-cache and WebSocket configuration.
func TestHeadCacheExampleConfigLoads(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("RPC_URL", "http://rpc.invalid:8545")
	cfg, err := LoadConfig(afero.NewOsFs(), "../erpc.headcache.example.yaml", &DefaultOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Projects)
	nw := cfg.Projects[0].Networks[0]
	require.NotNil(t, nw.Evm.HeadCache)
	require.True(t, nw.Evm.HeadCache.Enabled)
	require.NoError(t, nw.Evm.HeadCache.ValidateConnector(cfg))
	require.NotNil(t, cfg.Server.WebSocket)
	require.True(t, cfg.Server.WebSocket.Enabled)
}
