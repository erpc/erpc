package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachePartitionKey(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "evm:998:64321354", CachePartitionKey("evm:998", "", "64321354"),
		"empty suffix must keep the historical {networkId}:{ref} key")
	assert.Equal(t, "evm:998:systx:64321354", CachePartitionKey("evm:998", "systx", "64321354"))
	assert.Equal(t, "svm:mainnet-beta:systx:*", CachePartitionKey("svm:mainnet-beta", "systx", "*"))
}

func TestCacheKeySuffix_Validation(t *testing.T) {
	t.Parallel()

	valid := func(suffix string) *NetworkConfig {
		n := &NetworkConfig{
			Architecture:   ArchitectureEvm,
			Evm:            &EvmNetworkConfig{ChainId: 998},
			CacheKeySuffix: suffix,
		}
		require.NoError(t, n.SetDefaults(nil, nil))
		return n
	}

	require.NoError(t, valid("").Validate(&Config{}))
	require.NoError(t, valid("systx").Validate(&Config{}))
	require.NoError(t, valid("sys-tx_1").Validate(&Config{}))

	err := valid("sys:tx").Validate(&Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cacheKeySuffix")
}

func TestNetworkDefaults_CacheKeySuffix_Validation(t *testing.T) {
	t.Parallel()

	require.NoError(t, (&NetworkDefaults{CacheKeySuffix: "systx"}).Validate())
	require.NoError(t, (&NetworkDefaults{}).Validate())

	err := (&NetworkDefaults{CacheKeySuffix: "sys:tx"}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cacheKeySuffix")
}

func TestSetDefaults_CacheKeySuffixInherits(t *testing.T) {
	t.Parallel()

	defaults := &NetworkDefaults{CacheKeySuffix: "systx"}

	inherited := &NetworkConfig{
		Architecture: ArchitectureEvm,
		Evm:          &EvmNetworkConfig{ChainId: 998},
	}
	require.NoError(t, inherited.SetDefaults(nil, defaults))
	assert.Equal(t, "systx", inherited.CacheKeySuffix)

	overridden := &NetworkConfig{
		Architecture:   ArchitectureEvm,
		Evm:            &EvmNetworkConfig{ChainId: 998},
		CacheKeySuffix: "other",
	}
	require.NoError(t, overridden.SetDefaults(nil, defaults))
	assert.Equal(t, "other", overridden.CacheKeySuffix)
}
