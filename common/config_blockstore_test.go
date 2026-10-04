package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// An explicit depth below the maxPerTick default must not fail startup on a field
// the operator never set (LoadConfig runs SetDefaults then Validate).
func TestEvmBlockStoreConfig_MaxPerTickDefaultClampedToDepth(t *testing.T) {
	for _, depth := range []int64{1, 8, 15, 16, 128} {
		c := &EvmBlockStoreConfig{Enabled: true, ConnectorId: "r", Depth: depth}
		c.SetDefaults()
		require.Equal(t, min(int64(16), depth), c.MaxPerTick, "depth %d", depth)
		require.NoError(t, c.Validate(), "depth %d", depth)
	}
	explicit := &EvmBlockStoreConfig{Enabled: true, ConnectorId: "r", Depth: 8, MaxPerTick: 16}
	explicit.SetDefaults()
	require.ErrorContains(t, explicit.Validate(), "maxPerTick", "an explicit maxPerTick above depth is still rejected")
}

func TestEvmBlockStoreConfig_MaxBlockBytesDefaultClampedToMaxBytes(t *testing.T) {
	small := &EvmBlockStoreConfig{Enabled: true, ConnectorId: "r", MaxBytes: 8 << 20}
	small.SetDefaults()
	require.Equal(t, int64(8<<20), small.MaxBlockBytes)
	require.NoError(t, small.Validate())

	def := &EvmBlockStoreConfig{Enabled: true, ConnectorId: "r"}
	def.SetDefaults()
	require.Equal(t, int64(16<<20), def.MaxBlockBytes)

	explicit := &EvmBlockStoreConfig{Enabled: true, ConnectorId: "r", MaxBytes: 8 << 20, MaxBlockBytes: 16 << 20}
	explicit.SetDefaults()
	require.Error(t, explicit.Validate(), "explicit oversize is still rejected")
}

func TestEvmBlockStoreHistoricalConfig_DefaultsAndValidation(t *testing.T) {
	store := &EvmBlockStoreConfig{}
	store.SetDefaults()
	require.False(t, store.Historical.Enabled)
	require.Equal(t, Duration(time.Hour), store.Historical.TTL)

	store = &EvmBlockStoreConfig{Historical: EvmBlockStoreHistoricalConfig{Enabled: true}}
	store.SetDefaults()
	require.Equal(t, Duration(time.Hour), store.Historical.TTL)
	require.ErrorContains(t, store.Validate(), "connectorId")

	store.ConnectorId = "redis"
	require.NoError(t, store.Validate(), "historical-only config should not require live window limits")

	store.Historical.TTL = 0
	require.ErrorContains(t, store.Validate(), "historical.ttl")
	store.Historical.TTL = Duration(-time.Second)
	require.ErrorContains(t, store.Validate(), "historical.ttl")

	for _, tc := range []struct {
		name string
		set  func(*EvmBlockStoreConfig)
	}{
		{"depth", func(c *EvmBlockStoreConfig) { c.Depth = -1 }},
		{"maxBytes", func(c *EvmBlockStoreConfig) { c.MaxBytes = -1 }},
		{"concurrency", func(c *EvmBlockStoreConfig) { c.Concurrency = -1 }},
		{"fetchTimeout", func(c *EvmBlockStoreConfig) { c.FetchTimeout = -1 }},
		{"maxLogsRange", func(c *EvmBlockStoreConfig) { c.MaxLogsRange = -1 }},
		{"maxBlockBytes", func(c *EvmBlockStoreConfig) { c.MaxBlockBytes = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &EvmBlockStoreConfig{ConnectorId: "redis", Historical: EvmBlockStoreHistoricalConfig{Enabled: true}}
			c.SetDefaults()
			tc.set(c)
			require.ErrorContains(t, c.Validate(), tc.name)
		})
	}
}

func TestNetworkConfig_ValidatesConnectorForHistoricalOnlyBlockStore(t *testing.T) {
	store := &EvmBlockStoreConfig{ConnectorId: "missing", Historical: EvmBlockStoreHistoricalConfig{Enabled: true}}
	store.SetDefaults()
	network := &NetworkConfig{Architecture: "evm", Evm: &EvmNetworkConfig{BlockStore: store}}
	require.NoError(t, network.Evm.SetDefaults())
	require.ErrorContains(t, network.Validate(&Config{}), "not found")
}

func TestEvmBlockStoreLogsFillConfig_DefaultsAndValidation(t *testing.T) {
	store := &EvmBlockStoreConfig{}
	store.SetDefaults()
	require.False(t, store.LogsFill.Enabled, "logs fill is opt-in")
	require.Equal(t, int64(10), store.LogsFill.MaxRange)
	require.Equal(t, Duration(time.Hour), store.LogsFill.FinalizedTTL)
	require.Equal(t, Duration(0), store.LogsFill.UnfinalizedTTL, "0 = derived from block time")
	require.Equal(t, int64(2), store.LogsFill.EmptyTipGuard)
	require.Equal(t, int64(64<<20), store.LogsFill.MemoryMaxBytes)
	require.Equal(t, Duration(1500*time.Millisecond).Ptr(), store.LogsFill.PeerWait)
	require.NoError(t, store.Validate())

	disabled := &EvmBlockStoreConfig{LogsFill: EvmBlockStoreLogsFillConfig{Enabled: true, PeerWait: Duration(0).Ptr()}}
	disabled.SetDefaults()
	require.Equal(t, Duration(0), *disabled.LogsFill.PeerWait, "explicit 0 disables cross-replica locking")
	require.NoError(t, disabled.Validate())

	standalone := &EvmBlockStoreConfig{LogsFill: EvmBlockStoreLogsFillConfig{Enabled: true}}
	standalone.SetDefaults()
	require.NoError(t, standalone.Validate(), "logs fill alone needs no connector, live window or historical cache")
	require.False(t, standalone.NeedsConnector(), "without connectorId it uses process memory")
	standalone.ConnectorId = "redis"
	require.True(t, standalone.NeedsConnector(), "a configured connectorId must resolve to redis")

	for _, tc := range []struct {
		name string
		set  func(*EvmBlockStoreLogsFillConfig)
	}{
		{"maxRange", func(c *EvmBlockStoreLogsFillConfig) { c.MaxRange = -1 }},
		{"maxRange", func(c *EvmBlockStoreLogsFillConfig) { c.MaxRange = 1001 }},
		{"finalizedTtl", func(c *EvmBlockStoreLogsFillConfig) { c.FinalizedTTL = -1 }},
		{"unfinalizedTtl", func(c *EvmBlockStoreLogsFillConfig) { c.UnfinalizedTTL = -1 }},
		{"emptyTipGuard", func(c *EvmBlockStoreLogsFillConfig) { c.EmptyTipGuard = -1 }},
		{"memoryMaxBytes", func(c *EvmBlockStoreLogsFillConfig) { c.MemoryMaxBytes = 1024 }},
		{"peerWait", func(c *EvmBlockStoreLogsFillConfig) { c.PeerWait = Duration(-1).Ptr() }},
		{"peerWait", func(c *EvmBlockStoreLogsFillConfig) { c.PeerWait = Duration(2 * time.Minute).Ptr() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &EvmBlockStoreConfig{LogsFill: EvmBlockStoreLogsFillConfig{Enabled: true}}
			c.SetDefaults()
			tc.set(&c.LogsFill)
			require.ErrorContains(t, c.Validate(), "logsFill."+tc.name)
			c.LogsFill.Enabled = false
			require.NoError(t, c.Validate(), "disabled logs fill is not validated")
		})
	}
}
