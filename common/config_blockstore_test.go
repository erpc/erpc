package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
}

func TestNetworkConfig_ValidatesConnectorForHistoricalOnlyBlockStore(t *testing.T) {
	store := &EvmBlockStoreConfig{ConnectorId: "missing", Historical: EvmBlockStoreHistoricalConfig{Enabled: true}}
	store.SetDefaults()
	network := &NetworkConfig{Architecture: "evm", Evm: &EvmNetworkConfig{BlockStore: store}}
	require.NoError(t, network.Evm.SetDefaults())
	require.ErrorContains(t, network.Validate(&Config{}), "not found")
}
