package common

import (
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func TestEvmBlockStoreConfig_DefaultsAndValidation(t *testing.T) {
	store := &EvmBlockStoreConfig{}
	store.SetDefaults()
	require.False(t, store.Enabled, "the block store is opt-in")
	require.Equal(t, int64(10), store.MaxRange)
	require.Equal(t, Duration(time.Hour), store.FinalizedTTL)
	require.Equal(t, Duration(0), store.UnfinalizedTTL, "0 = derived from block time")
	require.Equal(t, int64(2), store.EmptyTipGuard)
	require.Equal(t, int64(64<<20), store.MemoryMaxBytes)
	require.Equal(t, Duration(1500*time.Millisecond).Ptr(), store.PeerWait)
	require.Equal(t, 4, store.Concurrency)
	require.NoError(t, store.Validate())

	noLock := &EvmBlockStoreConfig{Enabled: true, PeerWait: Duration(0).Ptr()}
	noLock.SetDefaults()
	require.Equal(t, Duration(0), *noLock.PeerWait, "explicit 0 disables cross-replica locking")
	require.NoError(t, noLock.Validate())

	memory := &EvmBlockStoreConfig{Enabled: true}
	memory.SetDefaults()
	require.NoError(t, memory.Validate(), "no connector is required")
	require.False(t, memory.NeedsConnector(), "without connectorId it uses process memory")
	memory.ConnectorId = "redis"
	require.True(t, memory.NeedsConnector(), "a configured connectorId must resolve to redis")
	memory.Enabled = false
	require.False(t, memory.NeedsConnector(), "a disabled store never resolves its connector")

	for _, tc := range []struct {
		name string
		set  func(*EvmBlockStoreConfig)
	}{
		{"maxRange", func(c *EvmBlockStoreConfig) { c.MaxRange = -1 }},
		{"maxRange", func(c *EvmBlockStoreConfig) { c.MaxRange = 1001 }},
		{"finalizedTtl", func(c *EvmBlockStoreConfig) { c.FinalizedTTL = -1 }},
		{"unfinalizedTtl", func(c *EvmBlockStoreConfig) { c.UnfinalizedTTL = -1 }},
		{"emptyTipGuard", func(c *EvmBlockStoreConfig) { c.EmptyTipGuard = -1 }},
		{"memoryMaxBytes", func(c *EvmBlockStoreConfig) { c.MemoryMaxBytes = 1024 }},
		{"peerWait", func(c *EvmBlockStoreConfig) { c.PeerWait = Duration(-1).Ptr() }},
		{"peerWait", func(c *EvmBlockStoreConfig) { c.PeerWait = Duration(2 * time.Minute).Ptr() }},
		{"concurrency", func(c *EvmBlockStoreConfig) { c.Concurrency = -1 }},
		{"concurrency", func(c *EvmBlockStoreConfig) { c.Concurrency = 65 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &EvmBlockStoreConfig{Enabled: true}
			c.SetDefaults()
			tc.set(c)
			require.ErrorContains(t, c.Validate(), "evm.blockStore."+tc.name)
			c.Enabled = false
			require.NoError(t, c.Validate(), "a disabled block store is not validated")
		})
	}
}

func TestNetworkConfig_ValidatesBlockStoreConnector(t *testing.T) {
	store := &EvmBlockStoreConfig{Enabled: true, ConnectorId: "missing"}
	network := &NetworkConfig{Architecture: "evm", Evm: &EvmNetworkConfig{ChainId: 1, BlockStore: store}}
	require.NoError(t, network.Evm.SetDefaults())
	require.ErrorContains(t, network.Validate(&Config{}), "not found")
}

const blockStoreRoundTripYaml = `
logLevel: error
database:
  evmJsonRpcCache:
    connectors:
      - id: shared
        driver: redis
        redis:
          uri: redis://localhost:6379
projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
          blockStore:
            enabled: true
            connectorId: shared
            maxRange: 20
            finalizedTtl: 30m
            unfinalizedTtl: 3s
            emptyTipGuard: 3
            memoryMaxBytes: 33554432
            peerWait: 500ms
            concurrency: 8
    upstreams:
      - endpoint: http://rpc1.localhost
`

const blockStoreRoundTripJson = `{
  "logLevel": "error",
  "projects": [{
    "id": "main",
    "networks": [{
      "architecture": "evm",
      "evm": {
        "chainId": 1,
        "blockStore": {"enabled": true, "maxRange": 5}
      }
    }],
    "upstreams": [{"endpoint": "http://rpc1.localhost"}]
  }]
}`

func loadBlockStoreTestConfig(t *testing.T, name, src string) (*Config, error) {
	t.Helper()
	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, name, []byte(src), 0o600))
	return LoadConfig(fs, name, &DefaultOptions{})
}

func blockStoreOf(t *testing.T, cfg *Config) *EvmBlockStoreConfig {
	t.Helper()
	require.Len(t, cfg.Projects, 1)
	require.Len(t, cfg.Projects[0].Networks, 1)
	require.NotNil(t, cfg.Projects[0].Networks[0].Evm)
	require.NotNil(t, cfg.Projects[0].Networks[0].Evm.BlockStore)
	return cfg.Projects[0].Networks[0].Evm.BlockStore
}

// The flat evm.blockStore keys load from YAML and JSON (LoadConfig runs
// SetDefaults then Validate), and the removed nested logsFill key is rejected
// by strict decoding rather than silently ignored.
func TestLoadConfig_BlockStoreFlatKeysRoundTrip(t *testing.T) {
	t.Run("yaml", func(t *testing.T) {
		cfg, err := loadBlockStoreTestConfig(t, "erpc.yaml", blockStoreRoundTripYaml)
		require.NoError(t, err)
		bs := blockStoreOf(t, cfg)
		require.True(t, bs.Enabled)
		require.Equal(t, "shared", bs.ConnectorId)
		require.Equal(t, int64(20), bs.MaxRange)
		require.Equal(t, Duration(30*time.Minute), bs.FinalizedTTL)
		require.Equal(t, Duration(3*time.Second), bs.UnfinalizedTTL)
		require.Equal(t, int64(3), bs.EmptyTipGuard)
		require.Equal(t, int64(32<<20), bs.MemoryMaxBytes)
		require.Equal(t, Duration(500*time.Millisecond), *bs.PeerWait)
		require.Equal(t, 8, bs.Concurrency)
	})

	t.Run("json", func(t *testing.T) {
		cfg, err := loadBlockStoreTestConfig(t, "erpc.json", blockStoreRoundTripJson)
		require.NoError(t, err)
		bs := blockStoreOf(t, cfg)
		require.True(t, bs.Enabled)
		require.Empty(t, bs.ConnectorId, "no connector = per-replica memory store")
		require.Equal(t, int64(5), bs.MaxRange)
		require.Equal(t, Duration(time.Hour), bs.FinalizedTTL, "defaults applied")
		require.Equal(t, int64(64<<20), bs.MemoryMaxBytes)
		require.Equal(t, 4, bs.Concurrency)
	})

	t.Run("nested logsFill is rejected", func(t *testing.T) {
		src := `
logLevel: error
projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
          blockStore:
            logsFill:
              enabled: true
    upstreams:
      - endpoint: http://rpc1.localhost
`
		_, err := loadBlockStoreTestConfig(t, "erpc.yaml", src)
		require.ErrorContains(t, err, "logsFill")
	})

	t.Run("removed live-window keys are rejected", func(t *testing.T) {
		for _, key := range []string{"depth: 128", "historical:\n              enabled: true", "pollInterval: 2s"} {
			src := `
logLevel: error
projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
          blockStore:
            enabled: true
            ` + key + `
    upstreams:
      - endpoint: http://rpc1.localhost
`
			_, err := loadBlockStoreTestConfig(t, "erpc.yaml", src)
			require.Error(t, err, key)
		}
	})

	t.Run("invalid value fails validation", func(t *testing.T) {
		src := `
logLevel: error
projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
          blockStore:
            enabled: true
            maxRange: 5000
    upstreams:
      - endpoint: http://rpc1.localhost
`
		_, err := loadBlockStoreTestConfig(t, "erpc.yaml", src)
		require.ErrorContains(t, err, "evm.blockStore.maxRange")
	})
}
