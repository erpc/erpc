package common

import (
	"bytes"
	"encoding/json"
	"testing"

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

	t.Run("maxRegressionBlocks below -1 rejected", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{MaxRegressionBlocks: -2}
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "maxRegressionBlocks")
	})

	t.Run("maxRegressionBlocks -1 disables the guard", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{MaxRegressionBlocks: -1}
		require.NoError(t, e.Validate())
	})

	t.Run("zero maxRegressionBlocks means the default tolerance", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{}
		require.NoError(t, e.Validate())
	})

	t.Run("guaranteedFor accepts id and tag selectors", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{
			GuaranteedFor: []string{"type:internal", "int*", "!type:external"},
		}
		require.NoError(t, e.Validate())
	})

	t.Run("guaranteedFor rejects an invalid selector", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{
			GuaranteedFor: []string{"(type:internal"},
		}
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "guaranteedFor")
	})

	t.Run("empty guaranteedFor is the default", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{}
		require.NoError(t, e.Validate())
		assert.Empty(t, e.ServedTip.GuaranteedFor)
	})

	t.Run("quorum below -1 rejected", func(t *testing.T) {
		e := baseValidEvmNetworkConfig()
		e.ServedTip = &EvmServedTipConfig{Quorum: -2}
		err := e.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "quorum")
	})

	// End to end through the YAML and JSON an operator actually writes, with
	// the config loader's strict decoding.
	t.Run("quorum YAML/JSON", func(t *testing.T) {
		decode := func(src string) (*EvmServedTipConfig, error) {
			var st EvmServedTipConfig
			dec := yaml.NewDecoder(bytes.NewBufferString(src))
			dec.KnownFields(true)
			return &st, dec.Decode(&st)
		}

		st, err := decode("quorum: 3\n")
		require.NoError(t, err)
		assert.Equal(t, ServedTipQuorum(3), st.Quorum)
		assert.Equal(t, 3, st.Quorum.Of(5))
		assert.Equal(t, 2, st.Quorum.Of(2), "a ballot smaller than the quorum serves what all of it has")

		st, err = decode("quorum: majority\n")
		require.NoError(t, err)
		assert.Equal(t, ServedTipQuorumMajority, st.Quorum)
		assert.Equal(t, 3, st.Quorum.Of(5))

		st, err = decode("guaranteedFor: [\"type:internal\"]\n")
		require.NoError(t, err)
		assert.Equal(t, DefaultServedTipQuorum, st.Quorum.Of(5), "unset is the default quorum")

		for _, bad := range []string{"quorum: 0\n", "quorum: -1\n", "quorum: half\n"} {
			_, err = decode(bad)
			require.Error(t, err, bad)
			assert.Contains(t, err.Error(), "quorum", bad)
		}
		for _, removed := range []string{"enabledFor: [latest]\n", "clusterDelta: 3\n", "trajectoryWindow: 10m\n"} {
			_, err = decode(removed)
			require.Error(t, err, "removed key must be rejected: %s", removed)
		}

		out, err := yaml.Marshal(&EvmServedTipConfig{Quorum: ServedTipQuorumMajority})
		require.NoError(t, err)
		assert.Equal(t, "quorum: majority\n", string(out))

		var js EvmServedTipConfig
		require.NoError(t, json.Unmarshal([]byte(`{"quorum":"majority"}`), &js))
		assert.Equal(t, ServedTipQuorumMajority, js.Quorum)
		require.NoError(t, json.Unmarshal([]byte(`{"quorum":4}`), &js))
		assert.Equal(t, ServedTipQuorum(4), js.Quorum)
		require.Error(t, json.Unmarshal([]byte(`{"quorum":0}`), &js))
		raw, err := json.Marshal(&EvmServedTipConfig{Quorum: ServedTipQuorumMajority})
		require.NoError(t, err)
		assert.JSONEq(t, `{"quorum":"majority"}`, string(raw))
	})
}

// TestNetworkConfig_SetDefaults_InheritsServedTip ensures a global
// networkDefaults.evm.servedTip propagates to a network that defines its own
// evm block (e.g. just chainId) — the common deployment shape.
func TestNetworkConfig_SetDefaults_InheritsServedTip(t *testing.T) {
	defaults := &NetworkDefaults{
		Evm: &EvmNetworkConfig{
			ServedTip: &EvmServedTipConfig{Quorum: ServedTipQuorumMajority},
		},
	}

	// Network with its own evm block (chainId) and no servedTip → inherits it.
	n := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}}
	require.NoError(t, n.SetDefaults(nil, defaults))
	require.NotNil(t, n.Evm.ServedTip, "must inherit servedTip from networkDefaults")
	assert.Equal(t, ServedTipQuorumMajority, n.Evm.ServedTipQuorum())

	// A network that sets its own servedTip keeps it (default does not clobber).
	n2 := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{
		ChainId:   1,
		ServedTip: &EvmServedTipConfig{Quorum: 3},
	}}
	require.NoError(t, n2.SetDefaults(nil, defaults))
	assert.Equal(t, ServedTipQuorum(3), n2.Evm.ServedTipQuorum(), "explicit network servedTip must win")
}
