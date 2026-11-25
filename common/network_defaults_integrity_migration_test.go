package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The legacy evm.integrity block migrates into directiveDefaults only when the
// directive field is still nil, which means "the operator did not say". The
// same config must give the same result at the networkDefaults level and at
// the network level. An unrelated key under networkDefaults.directiveDefaults
// must not cancel the migration.
func TestNetworkDefaults_DirectiveDefaultsBlockDoesNotCancelLegacyIntegrityMigration(t *testing.T) {
	// project builds a project with one EVM network, runs SetDefaults, and
	// returns the network's directive defaults.
	project := func(t *testing.T, nd *NetworkDefaults, n *NetworkConfig) *DirectiveDefaultsConfig {
		t.Helper()
		p := &ProjectConfig{Id: "p", NetworkDefaults: nd, Networks: []*NetworkConfig{n}}
		require.NoError(t, p.SetDefaults(nil))
		require.NotNil(t, p.Networks[0].DirectiveDefaults)
		return p.Networks[0].DirectiveDefaults
	}
	legacyOff := func() *EvmIntegrityConfig {
		return &EvmIntegrityConfig{EnforceHighestBlock: util.BoolPtr(false)}
	}

	t.Run("UnrelatedDirectiveBesideTheLegacyBlock", func(t *testing.T) {
		atDefaults := project(t,
			&NetworkDefaults{
				DirectiveDefaults: &DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(false)},
				Evm:               &EvmNetworkConfig{Integrity: legacyOff()},
			},
			&NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}},
		)
		atNetwork := project(t, nil, &NetworkConfig{
			Architecture:      ArchitectureEvm,
			DirectiveDefaults: &DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(false)},
			Evm:               &EvmNetworkConfig{ChainId: 1, Integrity: legacyOff()},
		})

		require.NotNil(t, atNetwork.EnforceHighestBlock)
		assert.False(t, *atNetwork.EnforceHighestBlock, "the network level migrates the legacy value")
		require.NotNil(t, atDefaults.EnforceHighestBlock)
		assert.False(t, *atDefaults.EnforceHighestBlock,
			"an unrelated directiveDefaults key must not cancel the legacy migration at the networkDefaults level")
		assert.False(t, *atDefaults.RetryEmpty, "the unrelated key still applies")
	})

	t.Run("NothingBesideTheLegacyBlock", func(t *testing.T) {
		dd := project(t,
			&NetworkDefaults{Evm: &EvmNetworkConfig{Integrity: legacyOff()}},
			&NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}},
		)
		require.NotNil(t, dd.EnforceHighestBlock)
		assert.False(t, *dd.EnforceHighestBlock)
	})

	t.Run("OtherDirectivesAreStillDefaulted", func(t *testing.T) {
		dd := project(t,
			&NetworkDefaults{
				DirectiveDefaults: &DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(false)},
				Evm:               &EvmNetworkConfig{Integrity: legacyOff()},
			},
			&NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}},
		)
		require.NotNil(t, dd.EnforceGetLogsBlockRange)
		assert.True(t, *dd.EnforceGetLogsBlockRange)
		require.NotNil(t, dd.EnforceNonNullTaggedBlocks)
		assert.True(t, *dd.EnforceNonNullTaggedBlocks)
	})

	t.Run("AnExplicitDirectiveBeatsTheLegacyBlock", func(t *testing.T) {
		dd := project(t,
			&NetworkDefaults{
				DirectiveDefaults: &DirectiveDefaultsConfig{EnforceHighestBlock: util.BoolPtr(true)},
				Evm:               &EvmNetworkConfig{Integrity: legacyOff()},
			},
			&NetworkConfig{Architecture: ArchitectureEvm, Evm: &EvmNetworkConfig{ChainId: 1}},
		)
		require.NotNil(t, dd.EnforceHighestBlock)
		assert.True(t, *dd.EnforceHighestBlock)
	})
}
