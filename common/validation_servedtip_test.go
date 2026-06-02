package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseValidEvmNetworkConfig() *EvmNetworkConfig {
	return &EvmNetworkConfig{
		ChainId:                     1,
		FallbackFinalityDepth:       1024,
		FallbackStatePollerDebounce: Duration(1),
		GetLogsMaxAllowedRange:      1000,
	}
}

func TestEvmNetworkConfig_Validate_ServedTip(t *testing.T) {
	t.Run("nil servedTip is valid", func(t *testing.T) {
		require.NoError(t, baseValidEvmNetworkConfig().Validate())
	})

	t.Run("valid tags pass (case-insensitive incl. safe)", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{EnabledFor: []string{"latest", "Finalized", "safe"}}
		require.NoError(t, e.Validate())
	})

	t.Run("unknown tag rejected", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{EnabledFor: []string{"lastest"}} // typo
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "enabledFor")
	})

	t.Run("negative clusterDelta rejected", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{ClusterDelta: -1}
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "clusterDelta")
	})
}

func TestServedTipEnabledFor_SafeFollowsFinalized(t *testing.T) {
	safe := &EvmNetworkConfig{ServedTip: &EvmServedTipConfig{EnabledFor: []string{"safe"}}}
	assert.True(t, safe.ServedTipEnabledFor("finalized"), "safe enables the finalized axis")
	assert.False(t, safe.ServedTipEnabledFor("latest"))

	latest := &EvmNetworkConfig{ServedTip: &EvmServedTipConfig{EnabledFor: []string{"latest"}}}
	assert.True(t, latest.ServedTipEnabledFor("latest"))
	assert.False(t, latest.ServedTipEnabledFor("finalized"))

	// Nil-receiver / nil config safe.
	var nilCfg *EvmNetworkConfig
	assert.False(t, nilCfg.ServedTipEnabledFor("latest"))
	assert.False(t, (&EvmNetworkConfig{}).ServedTipEnabledFor("latest"))
}
