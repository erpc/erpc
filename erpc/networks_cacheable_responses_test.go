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

// countingCacheDAL records Get/Set calls for Forward cache-path assertions.
type countingCacheDAL struct {
	gets atomic.Int32
	sets atomic.Int32
	hit  *common.NormalizedResponse
}

func (c *countingCacheDAL) Get(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	c.gets.Add(1)
	if c.hit == nil {
		return nil, nil
	}
	return c.hit.WithRequest(req), nil
}

func (c *countingCacheDAL) Set(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) error {
	c.sets.Add(1)
	return nil
}

func (c *countingCacheDAL) IsObjectNull() bool { return false }

// fakeSelectorConsensus makes consensusPolicySelectorEnabled return true
// without running a real consensus round.
type fakeSelectorConsensus struct{}

func (fakeSelectorConsensus) Run(
	ctx context.Context,
	req *common.NormalizedRequest,
	inner func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error),
) (*common.NormalizedResponse, error) {
	return inner(ctx, req)
}

func (fakeSelectorConsensus) HasSelector() bool { return true }

// TestNetworkForward_SelectorReopensCache locks the write-side admission
// redesign: under a customPolicy selector, Forward consults and writes the
// cache again (historical !selectorOn skip removed). Multiplexing stays gated.
func TestNetworkForward_SelectorReopensCache(t *testing.T) {
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

	t.Run("selector_cacheHitServed", func(t *testing.T) {
		hit := common.NewNormalizedResponse().
			WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0xcached"`), nil))
		cache := &countingCacheDAL{hit: hit}
		network.cacheDal = cache

		resp, err := forwardBalance()
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0xcached"`, jrr.GetResultString())
		assert.Equal(t, int32(1), cache.gets.Load())
		assert.Equal(t, int32(0), cache.sets.Load())
	})

	t.Run("selector_missWritesThrough", func(t *testing.T) {
		cache := &countingCacheDAL{}
		network.cacheDal = cache

		resp, err := forwardBalance()
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `"0xupstream"`, jrr.GetResultString())
		assert.Equal(t, int32(1), cache.gets.Load())
		require.Eventually(t, func() bool { return cache.sets.Load() >= 1 }, 2*time.Second, 10*time.Millisecond,
			"selector-enabled networks must write through Cache.Set (admission lives in Set)")
	})
}

// TestNetworkForward_NoConsensusUnchanged keeps the non-selector path green:
// cache Get/Set still run when no customPolicy selector is configured.
func TestNetworkForward_NoConsensusUnchanged(t *testing.T) {
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
	require.False(t, network.consensusPolicySelectorEnabled())

	cache := &countingCacheDAL{}
	network.cacheDal = cache

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","0x1"]}`,
	))
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"0xupstream"`, jrr.GetResultString())
	assert.Equal(t, int32(1), cache.gets.Load())
	require.Eventually(t, func() bool { return cache.sets.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
}
