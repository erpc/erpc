package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestEvmJsonRpcCache_Set_RespectsSkipCacheWriteDirective verifies that a request
// carrying the skipCacheWrite directive does not persist its response to the
// shared cache, while the same request without the directive does. This is the
// write-side complement to skipCacheRead, used by co-resident projects that share
// a cache connector (the cache key omits projectId) so one tier cannot backfill
// another tier's cache.
func TestEvmJsonRpcCache_Set_RespectsSkipCacheWriteDirective(t *testing.T) {
	t.Run("skipCacheWrite_true_does_not_persist", func(t *testing.T) {
		ctx := context.Background()
		conns, network, upstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
		})
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
		}, conns[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x2",false],"id":1}`))
		req.SetNetwork(network)
		req.SetCacheDal(cache)
		req.SetDirectives(&common.RequestDirectives{SkipCacheWrite: true})
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(stringToReaderCloser(`{"result":{"hash":"0xabc","number":"0x2"}}`))
		resp.SetUpstream(upstreams[0])
		req.SetLastValidResponse(ctx, resp)

		conns[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)
		assert.NoError(t, err)
		conns[0].AssertNotCalled(t, "Set")
	})

	t.Run("skipCacheWrite_false_persists", func(t *testing.T) {
		ctx := context.Background()
		conns, network, upstreams, cache := createCacheTestFixtures(ctx, []upsTestCfg{
			{id: "upsA", syncing: common.EvmSyncingStateUnknown, finBn: 10, lstBn: 15},
		})
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network: "evm:123",
			Method:  "eth_getBlockByNumber",
		}, conns[0])
		require.NoError(t, err)
		cache.SetPolicies([]*data.CachePolicy{policy})

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x2",false],"id":1}`))
		req.SetNetwork(network)
		req.SetCacheDal(cache)
		req.SetDirectives(&common.RequestDirectives{SkipCacheWrite: false})
		resp := common.NewNormalizedResponse().WithRequest(req).WithBody(stringToReaderCloser(`{"result":{"hash":"0xabc","number":"0x2"}}`))
		resp.SetUpstream(upstreams[0])
		req.SetLastValidResponse(ctx, resp)

		conns[0].On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

		err = cache.Set(context.Background(), req, resp)
		assert.NoError(t, err)
		conns[0].AssertCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})
}
