package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvmHeadCacheConfig_MaxBlockBytesDefaultClampedToMaxBytes(t *testing.T) {
	small := &EvmHeadCacheConfig{Enabled: true, ConnectorId: "r", MaxBytes: 8 << 20}
	small.SetDefaults()
	require.Equal(t, int64(8<<20), small.MaxBlockBytes)
	require.NoError(t, small.Validate())

	def := &EvmHeadCacheConfig{Enabled: true, ConnectorId: "r"}
	def.SetDefaults()
	require.Equal(t, int64(16<<20), def.MaxBlockBytes)

	explicit := &EvmHeadCacheConfig{Enabled: true, ConnectorId: "r", MaxBytes: 8 << 20, MaxBlockBytes: 16 << 20}
	explicit.SetDefaults()
	require.Error(t, explicit.Validate(), "explicit oversize is still rejected")
}
