package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestNetworkGetFinality(t *testing.T) {
	ctx := context.Background()

	t.Run("MethodConfigFinalized", func(t *testing.T) {
		network := &Network{
			cfg: &common.NetworkConfig{
				Methods: &common.MethodsConfig{
					Definitions: map[string]*common.CacheMethodConfig{
						"eth_chainId": {Finalized: true},
					},
				},
			},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_chainId","params":[],"id":1,"jsonrpc":"2.0"}`))
		finality := network.GetFinality(ctx, req, nil)
		assert.Equal(t, common.DataFinalityStateFinalized, finality)
	})

	t.Run("MethodConfigRealtime", func(t *testing.T) {
		network := &Network{
			cfg: &common.NetworkConfig{
				Methods: &common.MethodsConfig{
					Definitions: map[string]*common.CacheMethodConfig{
						"eth_blockNumber": {Realtime: true},
					},
				},
			},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_blockNumber","params":[],"id":1,"jsonrpc":"2.0"}`))
		finality := network.GetFinality(ctx, req, nil)
		assert.Equal(t, common.DataFinalityStateRealtime, finality)
	})

	t.Run("BlockRefLatest", func(t *testing.T) {
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["latest",true],"id":1,"jsonrpc":"2.0"}`))
		req.SetEvmBlockRef("latest")
		finality := network.GetFinality(ctx, req, nil)
		assert.Equal(t, common.DataFinalityStateRealtime, finality)
	})

	t.Run("BlockRefPending", func(t *testing.T) {
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["pending",true],"id":1,"jsonrpc":"2.0"}`))
		req.SetEvmBlockRef("pending")
		finality := network.GetFinality(ctx, req, nil)
		assert.Equal(t, common.DataFinalityStateRealtime, finality)
	})

	t.Run("BlockRefFinalized", func(t *testing.T) {
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["finalized",true],"id":1,"jsonrpc":"2.0"}`))
		req.SetEvmBlockRef("finalized")
		finality := network.GetFinality(ctx, req, nil)
		assert.Equal(t, common.DataFinalityStateRealtime, finality)
	})

	t.Run("BlockRefAsteriskWithZeroBlockNumber", func(t *testing.T) {
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getTransactionReceipt","params":["0x123"],"id":1,"jsonrpc":"2.0"}`))
		req.SetEvmBlockRef("*")
		req.SetEvmBlockNumber(int64(0))
		finality := network.GetFinality(ctx, req, nil)
		assert.Equal(t, common.DataFinalityStateUnfinalized, finality)
	})

	t.Run("FinalityCachingBehavior", func(t *testing.T) {
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
		req.SetNetwork(network)

		// First call returns unknown (no upstream available, no block number set)
		finality1 := req.Finality(ctx)
		assert.Equal(t, common.DataFinalityStateUnknown, finality1)

		// Now set block ref to "latest" which should return realtime
		req.SetEvmBlockRef("latest")

		// Second call should recalculate (not use cached unknown)
		finality2 := req.Finality(ctx)
		assert.Equal(t, common.DataFinalityStateRealtime, finality2)

		// Third call should use cached realtime value
		finality3 := req.Finality(ctx)
		assert.Equal(t, common.DataFinalityStateRealtime, finality3)
	})
}
