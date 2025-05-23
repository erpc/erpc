package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestUpstream_SkipLogic(t *testing.T) {
	t.Run("SingleSimpleMethod", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_getBalance"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("SingleWildcardMethod", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleMethods", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_getBalance", "eth_getBlockByNumber"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_call"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleMethodsWithWildcard", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*", "net_version"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"web3_clientVersion"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsOneIgnoredAnotherNot", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
			logger: &zerolog.Logger{},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test2",
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsBothIgnoreDifferentThings", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
			logger: &zerolog.Logger{},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test2",
				IgnoreMethods: []string{"eth_getBlockByNumber"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")
	})

	t.Run("MultipleUpstreamsAllIgnoredAMethod", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test1",
				IgnoreMethods: []string{"eth_getBalance"},
			},
			logger: &zerolog.Logger{},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test2",
				IgnoreMethods: []string{"eth_*"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")
	})

	t.Run("OneUpstreamWithoutAnythingIgnored", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test",
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("MultipleUpstreamsWithoutAnythingIgnored", func(t *testing.T) {
		upstream1 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test1",
			},
			logger: &zerolog.Logger{},
		}
		upstream2 := &Upstream{
			config: &common.UpstreamConfig{
				Id: "test2",
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream1.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream2.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("CombinationOfWildcardAndSpecificMethods", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*", "net_version", "web3_clientVersion"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"net_version"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"web3_clientVersion"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"personal_sign"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)
	})

	t.Run("NestedWildcards", func(t *testing.T) {
		upstream := &Upstream{
			config: &common.UpstreamConfig{
				Id:            "test",
				IgnoreMethods: []string{"eth_*_*"},
			},
			logger: &zerolog.Logger{},
		}

		reason, skip := upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_get_balance"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance"}`)))
		assert.False(t, skip)
		assert.Nil(t, reason)

		reason, skip = upstream.shouldSkip(context.TODO(), common.NewNormalizedRequest([]byte(`{"method":"eth_get_block_by_number"}`)))
		assert.True(t, skip)
		assert.Contains(t, reason.Error(), "ErrUpstreamMethodIgnored")
	})
}
