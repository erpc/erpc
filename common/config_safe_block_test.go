package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetworkConfig_SafeBlockSourceInheritance(t *testing.T) {
	defaults := &NetworkDefaults{Evm: &EvmNetworkConfig{SafeBlockSource: "tier:default"}}

	inherited := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}}
	require.NoError(t, inherited.SetDefaults(nil, defaults))
	assert.Equal(t, "tier:default", inherited.Evm.SafeBlockSource)

	overridden := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{
		ChainId: 1, SafeBlockSource: "tier:network",
	}}
	require.NoError(t, overridden.SetDefaults(nil, defaults))
	assert.Equal(t, "tier:network", overridden.Evm.SafeBlockSource)
}

func TestEvmNetworkConfig_Validate_SafeBlockSource(t *testing.T) {
	cfg := baseValidEvmNetworkConfig()
	cfg.SafeBlockSource = "tier:source &"

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "safeBlockSource")
	assert.Contains(t, err.Error(), "invalid selector")
}
