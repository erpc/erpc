package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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

	t.Run("zero trajectoryWindow disables the referee", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{EnabledFor: []string{"latest"}, TrajectoryWindow: Duration(0).Ptr()}
		require.NoError(t, e.Validate())
	})

	// A window outside the range the mechanism can honour is a configuration
	// that LOOKS enabled and can never act: below a minute a fit is noise, and
	// beyond what the sample ring can span the referee stands down forever while
	// nothing in the metrics says so.
	t.Run("trajectoryWindow range", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			window Duration
			valid  bool
		}{
			{"the default", Duration(DefaultServedTipTrajectoryWindow), true},
			{"exactly the minimum", Duration(MinServedTipTrajectoryWindow), true},
			{"exactly the maximum", Duration(MaxServedTipTrajectoryWindow), true},
			{"one second below the minimum", Duration(MinServedTipTrajectoryWindow - time.Second), false},
			{"one second above the maximum", Duration(MaxServedTipTrajectoryWindow + time.Second), false},
			{"negative", Duration(-1), false},
			{"5 milliseconds (a bare YAML 5)", Duration(5 * time.Millisecond), false},
		} {
			t.Run(c.name, func(t *testing.T) {
				e := baseValidEvmNetworkConfig()
				e.ServedTip = &EvmServedTipConfig{EnabledFor: []string{"latest"}, TrajectoryWindow: c.window.Ptr()}
				err := e.Validate()
				if c.valid {
					require.NoError(t, err)
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), "trajectoryWindow")
				assert.Contains(t, err.Error(), "MILLISECONDS",
					"the error must name the trap it exists for: a bare YAML number is milliseconds")
			})
		}
	})

	// End to end through the YAML an operator actually writes.
	t.Run("trajectoryWindow YAML round-trip", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		require.NoError(t, yaml.Unmarshal([]byte("enabledFor: [latest]\ntrajectoryWindow: \"10m\"\n"), &e.ServedTip))
		require.Equal(t, 10*time.Minute, e.ServedTip.TrajectoryWindow.Duration())
		require.NoError(t, e.Validate())

		// A bare number is the trap the error message names. The config loader's
		// yaml.v3 rejects it outright here; a Duration that reaches the config
		// any other way (a JSON tool, yaml.v2, a programmatic literal) carries
		// the millisecond reading instead, which is what the range check above
		// catches — 5 means 5ms, never 5 minutes, on every path.
		bare := baseValidEvmNetworkConfig()
		err := yaml.Unmarshal([]byte("enabledFor: [latest]\ntrajectoryWindow: 5\n"), &bare.ServedTip)
		require.Error(t, err, "a bare number must never be read as 5 minutes")
		assert.Contains(t, err.Error(), "duration")
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
