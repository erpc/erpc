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

	t.Run("maxRegressionBlocks below -1 rejected", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{MaxRegressionBlocks: -2}
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "maxRegressionBlocks")
	})

	t.Run("maxRegressionBlocks -1 disables the guard", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{EnabledFor: []string{"latest"}, MaxRegressionBlocks: -1}
		require.NoError(t, e.Validate())
	})

	t.Run("zero maxRegressionBlocks means the default tolerance", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{EnabledFor: []string{"latest"}}
		require.NoError(t, e.Validate())
	})

	t.Run("negative trajectoryWindow rejected", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{TrajectoryWindow: Duration(-1).Ptr()}
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "trajectoryWindow")
	})

	t.Run("zero trajectoryWindow disables the referee", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{EnabledFor: []string{"latest"}, TrajectoryWindow: Duration(0).Ptr()}
		require.NoError(t, e.Validate())
	})
}

// TestNetworkConfig_SetDefaults_InheritsServedTip ensures a global
// networkDefaults.evm.servedTip propagates to a network that defines its own
// evm block (e.g. just chainId) — the common deployment shape.
func TestNetworkConfig_SetDefaults_InheritsServedTip(t *testing.T) {
	defaults := &NetworkDefaults{
		Evm: &EvmNetworkConfig{
			ServedTip: &EvmServedTipConfig{EnabledFor: []string{"latest"}},
		},
	}

	// Network with its own evm block (chainId) and no servedTip → inherits it.
	n := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}}
	require.NoError(t, n.SetDefaults(nil, defaults))
	require.NotNil(t, n.Evm.ServedTip, "must inherit servedTip from networkDefaults")
	assert.True(t, n.Evm.ServedTipEnabledFor("latest"))

	// A network that sets its own servedTip keeps it (default does not clobber).
	n2 := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{
		ChainId:   1,
		ServedTip: &EvmServedTipConfig{EnabledFor: []string{"finalized"}},
	}}
	require.NoError(t, n2.SetDefaults(nil, defaults))
	assert.False(t, n2.Evm.ServedTipEnabledFor("latest"))
	assert.True(t, n2.Evm.ServedTipEnabledFor("finalized"), "explicit network servedTip must win")
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
