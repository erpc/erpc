package upstream

import (
	"testing"

	"github.com/flair-sdk/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestUpstream_SkipLogic(t *testing.T) {
	t.Run("SingleSimpleMethod", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_getBalance"},
			},
		}

		reason, skip := upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("SingleWildcardMethod", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*"},
			},
		}

		reason, skip := upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleMethods", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_getBalance", "eth_getBlockByNumber"},
			},
		}

		reason, skip := upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBlockByNumber", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_call"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleMethodsWithWildcard", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*", "net_version"},
			},
		}

		reason, skip := upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("net_version", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"web3_clientVersion"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsOneIgnoredAnotherNot", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test2",
			},
		}

		reason, skip := upstream1.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test1"))

		reason, skip = upstream2.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsBothIgnoreDifferentThings", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test2",
				IgnoreMethods: []string{"eth_getBlockByNumber"},
			},
		}

		reason, skip := upstream1.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test1"))

		reason, skip = upstream2.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream1.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBlockByNumber", "test2"))
	})

	t.Run("MultipleUpstreamsAllIgnoredAMethod", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test2",
				IgnoreMethods: []string{"eth_*"},
			},
		}

		reason, skip := upstream1.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test1"))

		reason, skip = upstream2.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test2"))
	})

	t.Run("OneUpstreamWithoutAnythingIgnored", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test",
			},
		}

		reason, skip := upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsWithoutAnythingIgnored", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test1",
			},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test2",
			},
		}

		reason, skip := upstream1.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream1.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("CombinationOfWildcardAndSpecificMethods", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*", "net_version", "web3_clientVersion"},
			},
		}

		reason, skip := upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_getBalance", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("net_version", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"web3_clientVersion"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("web3_clientVersion", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"personal_sign"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("NestedWildcards", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*_*"},
			},
		}

		reason, skip := upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_get_balance"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_get_balance", "test"))

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream.shouldSkip(NewNormalizedRequest([]byte(`{"method":"eth_get_block_by_number"}`)))
		assert.True(t, skip)
		assert.ErrorIs(t, reason, common.NewErrUpstreamMethodIgnored("eth_get_block_by_number", "test"))
	})
}
