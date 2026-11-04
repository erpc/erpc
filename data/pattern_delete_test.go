package data

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestBuildPurgePatterns(t *testing.T) {
	t.Run("network wide unsuffixed", func(t *testing.T) {
		main, rvi := BuildPurgePatterns(common.CachePurgeScope{NetworkId: "evm:1"})
		assert.Equal(t, []string{"evm:1:*"}, main)
		assert.Equal(t, []string{"rvi#evm:1:*#*"}, rvi)
	})
	t.Run("network wide suffixed", func(t *testing.T) {
		main, rvi := BuildPurgePatterns(common.CachePurgeScope{NetworkId: "evm:1", Suffix: "systx"})
		assert.Equal(t, []string{"evm:1:systx:*"}, main)
		assert.Equal(t, []string{"rvi#evm:1:systx:*#*"}, rvi)
	})
	t.Run("method scoped", func(t *testing.T) {
		main, rvi := BuildPurgePatterns(common.CachePurgeScope{NetworkId: "evm:1", Method: "eth_blockNumber"})
		assert.Equal(t, []string{"evm:1:*:eth_blockNumber:*"}, main)
		assert.Equal(t, []string{"rvi#evm:1:*#eth_blockNumber:*"}, rvi)
	})
	t.Run("blockRef scoped skips rvi", func(t *testing.T) {
		main, rvi := BuildPurgePatterns(common.CachePurgeScope{NetworkId: "evm:1", BlockRef: "12345"})
		assert.Equal(t, []string{"evm:1:12345:*"}, main)
		assert.Nil(t, rvi, "blockRef scope must not wipe network-wide rvi companions")
	})
}

func TestMatchUnsuffixedMainKey(t *testing.T) {
	assert.True(t, matchUnsuffixedMainKey("evm:1", "evm:1:12345:eth_blockNumber:abcd", ""))
	assert.False(t, matchUnsuffixedMainKey("evm:1", "evm:1:systx:12345:eth_blockNumber:abcd", ""))
	assert.True(t, matchUnsuffixedMainKey("evm:1", "evm:1:foo\\:bar:eth_call:abcd", ""),
		"escaped colon in blockRef stays unsuffixed")
	assert.True(t, matchUnsuffixedRviKey("evm:1", "rvi#evm:1:*#eth_blockNumber:abcd"))
	assert.False(t, matchUnsuffixedRviKey("evm:1", "rvi#evm:1:systx:*#eth_blockNumber:abcd"))

	// SVM methods may contain ':' — broad unsuffixed purge must still keep them
	// (SQL/Dynamo do via separate columns); only real cacheKeySuffix siblings drop.
	assert.True(t, matchUnsuffixedMainKey("svm:mainnet-beta",
		"svm:mainnet-beta:42:custom:method:deadbeef", ""),
		"colon-in-method stays unsuffixed on network-wide purge")
	assert.True(t, matchUnsuffixedMainKey("svm:mainnet-beta",
		"svm:mainnet-beta:42:custom:method:deadbeef", "custom:method"),
		"method filter matches the full method, not a prefix")
	assert.False(t, matchUnsuffixedMainKey("svm:mainnet-beta",
		"svm:mainnet-beta:42:custom:method:deadbeef", "custom"),
		"method=custom must not match custom:method")
	assert.False(t, matchUnsuffixedMainKey("svm:mainnet-beta",
		"svm:mainnet-beta:systx:42:custom:method:deadbeef", "custom:method"),
		"suffixed sibling still rejected when method is known")
	assert.False(t, matchUnsuffixedMainKey("svm:mainnet-beta",
		"svm:mainnet-beta:systx:42:custom:method:deadbeef", ""),
		"suffixed sibling rejected on network-wide purge")
}

func TestRangeMatchesMethod_Exact(t *testing.T) {
	assert.True(t, rangeMatchesMethod("custom:method:deadbeef", "custom:method"))
	assert.False(t, rangeMatchesMethod("custom:method:deadbeef", "custom"))
	assert.True(t, rangeMatchesMethod("eth_blockNumber:abcd", "eth_blockNumber"))
	assert.True(t, rangeMatchesMethod("eth_blockNumber", "eth_blockNumber"))
}

func TestGlobMatch(t *testing.T) {
	require.True(t, globMatch("evm:1:*", "evm:1:123:eth_blockNumber:ab"))
	require.True(t, globMatch("evm:1:*:eth_blockNumber:*", "evm:1:123:eth_blockNumber:ab"))
	require.False(t, globMatch("evm:1:*:eth_blockNumber:*", "evm:1:123:eth_call:ab"))
	require.True(t, globMatch("rvi#evm:1:*#*", "rvi#evm:1:*#eth_blockNumber:ab"))
	require.True(t, globMatch("evm:1:systx:*", "evm:1:systx:123:eth_blockNumber:ab"))
}
