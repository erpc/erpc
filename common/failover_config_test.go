package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestFailoverConfig_OnDemandFallbackHeads(t *testing.T) {
	t.Run("off unless set with onDefaultsExhausted", func(t *testing.T) {
		assert.False(t, (*FailoverConfig)(nil).FallbackHeadsOnDemand())
		assert.False(t, (&FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)}).FallbackHeadsOnDemand())
		assert.False(t, (&FailoverConfig{OnDemandFallbackHeads: util.BoolPtr(true)}).FallbackHeadsOnDemand())
		assert.True(t, (&FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true), OnDemandFallbackHeads: util.BoolPtr(true)}).FallbackHeadsOnDemand())
	})

	t.Run("refused without onDefaultsExhausted", func(t *testing.T) {
		require.NoError(t, (*FailoverConfig)(nil).Validate("network.*.failover"))
		require.NoError(t, (&FailoverConfig{OnDemandFallbackHeads: util.BoolPtr(false)}).Validate("network.*.failover"))
		err := (&FailoverConfig{OnDemandFallbackHeads: util.BoolPtr(true)}).Validate("network.*.failover")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network.*.failover.onDemandFallbackHeads requires network.*.failover.onDefaultsExhausted")

		n := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1},
			Failover: &FailoverConfig{OnDemandFallbackHeads: util.BoolPtr(true)}}
		require.Error(t, n.Validate(&Config{}))
		require.Error(t, (&NetworkDefaults{Failover: &FailoverConfig{OnDemandFallbackHeads: util.BoolPtr(true)}}).Validate())
	})

	t.Run("parsed and inherited from networkDefaults", func(t *testing.T) {
		var d NetworkDefaults
		require.NoError(t, yaml.Unmarshal([]byte("failover:\n  onDefaultsExhausted: true\n  onDemandFallbackHeads: true\n"), &d))
		n := &NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}}
		require.NoError(t, n.SetDefaults(nil, &d))
		require.True(t, n.Failover.FallbackHeadsOnDemand())
		// A deep copy: changing the network's never changes the defaults'.
		*n.Failover.OnDemandFallbackHeads = false
		assert.True(t, *d.Failover.OnDemandFallbackHeads)
	})
}
