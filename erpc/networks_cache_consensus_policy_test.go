package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// countingCache records Get/Set calls and optionally advertises
// AllowsSelectorCache so Forward's cacheEligible gate can be exercised
// without building a full EvmJsonRpcCache.
type countingCache struct {
	gets  atomic.Int32
	sets  atomic.Int32
	allow bool
	hit   *common.NormalizedResponse // when non-nil, Get returns it
}

func (c *countingCache) Get(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	c.gets.Add(1)
	if c.hit == nil {
		return nil, nil
	}
	return c.hit.WithRequest(req), nil
}

func (c *countingCache) Set(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) error {
	c.sets.Add(1)
	return nil
}

func (c *countingCache) IsObjectNull() bool        { return false }
func (c *countingCache) AllowsSelectorCache() bool { return c.allow }

// fakeSelectorConsensus makes consensusPolicySelectorEnabled return true
// without running a real consensus round (Run just calls the inner).
type fakeSelectorConsensus struct{}

func (fakeSelectorConsensus) Run(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	return inner(ctx, req)
}

func (fakeSelectorConsensus) HasSelector() bool { return true }

func TestCacheEligible(t *testing.T) {
	t.Run("NilCache", func(t *testing.T) {
		assert.False(t, cacheEligible(nil, false))
		assert.False(t, cacheEligible(nil, true))
	})
	t.Run("NoSelectorAlwaysEligible", func(t *testing.T) {
		assert.True(t, cacheEligible(&countingCache{allow: false}, false))
	})
	t.Run("SelectorRequiresAllowsSelectorCache", func(t *testing.T) {
		assert.False(t, cacheEligible(&countingCache{allow: false}, true))
		assert.True(t, cacheEligible(&countingCache{allow: true}, true))
	})
	t.Run("PlainCacheDALSkippedWhenSelectorOn", func(t *testing.T) {
		// pathsCache / MockCacheDal do not implement AllowsSelectorCache —
		// historical skip preserved.
		assert.False(t, cacheEligible(&pathsCache{}, true))
	})
}

// TestNetworkForward_SelectorCacheGate locks the review finding on PR #268:
// when a customPolicy selector is configured, Forward used to skip the cache
// entirely (!selectorOn), making the cacheableConsensusPolicies allowlist a
// no-op. With the gate relaxed via AllowsSelectorCache, allowlist-filtered
// caches are consulted and written; caches that do not advertise the
// capability keep the skip.
func TestNetworkForward_SelectorCacheGate(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_getBalance")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xupstream"}`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupTestNetworkSimple(t, ctx, nil, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	})
	// Mark the network as selector-enabled without replacing the real
	// executors: consensusPolicySelectorEnabled scans any executor for
	// HasSelector, so appending a stub is enough to open the gate.
	network.failsafeExecutors = append(network.failsafeExecutors, &networkExecutor{
		method:    "*",
		consensus: fakeSelectorConsensus{},
	})
	require.True(t, network.consensusPolicySelectorEnabled())

	forwardBalance := func() (*common.NormalizedResponse, error) {
		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","0x1"]}`,
		))
		req.SetNetwork(network)
		return network.Forward(ctx, req)
	}

	t.Run("AllowTrue_CacheHitServed", func(t *testing.T) {
		hit := common.NewNormalizedResponse().
			WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0xcached"`), nil))
		cache := &countingCache{allow: true, hit: hit}
		network.cacheDal = cache

		resp, err := forwardBalance()
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0xcached"`, jrr.GetResultString())
		assert.Equal(t, int32(1), cache.gets.Load(), "selector-on + AllowsSelectorCache must consult the cache")
		assert.Equal(t, int32(0), cache.sets.Load(), "hit path must not write")
	})

	t.Run("AllowFalse_CacheSkippedFallsThrough", func(t *testing.T) {
		hit := common.NewNormalizedResponse().
			WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0xcached"`), nil))
		cache := &countingCache{allow: false, hit: hit}
		network.cacheDal = cache

		resp, err := forwardBalance()
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0xupstream"`, jrr.GetResultString(), "without AllowsSelectorCache the historical skip must stand")
		assert.Equal(t, int32(0), cache.gets.Load())
		assert.Equal(t, int32(0), cache.sets.Load())
	})

	t.Run("AllowTrue_MissWritesThrough", func(t *testing.T) {
		cache := &countingCache{allow: true} // miss
		network.cacheDal = cache

		resp, err := forwardBalance()
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0xupstream"`, jrr.GetResultString())
		assert.Equal(t, int32(1), cache.gets.Load())

		// Set runs in a background goroutine; wait briefly for it.
		require.Eventually(t, func() bool { return cache.sets.Load() >= 1 }, 2*time.Second, 10*time.Millisecond,
			"selector-on + AllowsSelectorCache must write the upstream response through Cache.Set (admission rule lives there)")
	})
}
