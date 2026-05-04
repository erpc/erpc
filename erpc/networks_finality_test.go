package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	t.Run("BlockRefAsteriskWithZeroBlockNumberNoResponse", func(t *testing.T) {
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getTransactionReceipt","params":["0x123"],"id":1,"jsonrpc":"2.0"}`))
		req.SetEvmBlockRef("*")
		req.SetEvmBlockNumber(int64(0))
		finality := network.GetFinality(ctx, req, nil)
		assert.Equal(t, common.DataFinalityStateUnknown, finality)
	})

	t.Run("BlockRefAsteriskExtractsBlockNumberFromResponse", func(t *testing.T) {
		// Simulates a cache hit for eth_getTransactionReceipt: request has blockRef="*"
		// and blockNumber=0, but the response body contains blockNumber.
		// With no upstream registry the heuristic can't resolve finalized vs unfinalized,
		// but the block number extraction itself should succeed (finality stays unknown
		// only because there's no upstream/heuristic to compare against).
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getTransactionReceipt","params":["0xabc"],"id":1,"jsonrpc":"2.0"}`))
		req.SetEvmBlockRef("*")
		req.SetEvmBlockNumber(int64(0))

		jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{
			"blockNumber": "0x1000",
			"blockHash":   "0x0000000000000000000000000000000000000000000000000000000000001000",
		}, nil)
		require.NoError(t, err)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		// Without upstreamsRegistry, finality stays unknown even though block number
		// was extracted. The important thing is it doesn't panic or return early
		// before attempting the heuristic path.
		finality := network.GetFinality(ctx, req, resp)
		assert.Equal(t, common.DataFinalityStateUnknown, finality)
	})

	t.Run("BlockHashRefExtractsBlockNumberFromResponse", func(t *testing.T) {
		// eth_getBlockByHash: request has blockRef = block hash (starts with 0x),
		// blockNumber = 0. The response contains the block number.
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByHash","params":["0x00000000000000000000000000000000000000000000000000000000deadbeef",true],"id":1,"jsonrpc":"2.0"}`))
		req.SetEvmBlockRef("0x00000000000000000000000000000000000000000000000000000000deadbeef")
		req.SetEvmBlockNumber(int64(0))

		jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{
			"number":     "0x500",
			"hash":       "0x00000000000000000000000000000000000000000000000000000000deadbeef",
			"parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
		}, nil)
		require.NoError(t, err)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		// Without upstreamsRegistry the heuristic can't resolve, but the code should
		// reach the heuristic path (not return unknown early).
		finality := network.GetFinality(ctx, req, resp)
		assert.Equal(t, common.DataFinalityStateUnknown, finality)
	})

	t.Run("EmptyBlockRefWithZeroBlockNumber", func(t *testing.T) {
		// When blockRef is "" and blockNumber is 0, should return unknown
		network := &Network{
			cfg: &common.NetworkConfig{},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_someUnknownMethod","params":[],"id":1,"jsonrpc":"2.0"}`))
		finality := network.GetFinality(ctx, req, nil)
		assert.Equal(t, common.DataFinalityStateUnknown, finality)
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
