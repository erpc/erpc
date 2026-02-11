package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// testNetwork is a simple test implementation of common.Network interface for this file
type testNetwork struct {
	cfg *common.NetworkConfig
}

func (t *testNetwork) Architecture() common.NetworkArchitecture {
	if t.cfg != nil {
		return t.cfg.Architecture
	}
	return common.ArchitectureEvm
}

func (t *testNetwork) Config() *common.NetworkConfig {
	return t.cfg
}

func (t *testNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return nil, nil
}

func (t *testNetwork) Bootstrap(ctx context.Context) error {
	return nil
}

func (t *testNetwork) Id() string {
	return "test-network"
}

func (t *testNetwork) Label() string {
	return "test"
}

func (t *testNetwork) ProjectId() string {
	return "test-project"
}

func (t *testNetwork) Logger() *zerolog.Logger {
	logger := zerolog.Nop()
	return &logger
}

func (t *testNetwork) EvmHighestLatestBlockNumber(ctx context.Context) int64 {
	return 0
}

func (t *testNetwork) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 {
	return 0
}

func (t *testNetwork) EvmLeaderUpstream(ctx context.Context) common.Upstream {
	return nil
}

func (t *testNetwork) GetMethodMetrics(method string) common.TrackedMetrics {
	return nil
}

func (t *testNetwork) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateFinalized
}

func TestAllPhantomTransactions(t *testing.T) {
	t.Run("EmptySlice", func(t *testing.T) {
		assert.True(t, allPhantomTransactions(nil))
		assert.True(t, allPhantomTransactions([]any{}))
	})

	t.Run("SinglePhantomTx_FullAddress", func(t *testing.T) {
		txs := []any{
			map[string]interface{}{
				"from": "0x0000000000000000000000000000000000000000",
				"gas":  "0x0",
				"to":   "0x0000000000000000000000000000000000000000",
			},
		}
		assert.True(t, allPhantomTransactions(txs))
	})

	t.Run("SinglePhantomTx_ShortAddress", func(t *testing.T) {
		// Some RPCs return the short form "0x0" instead of full zero address
		txs := []any{
			map[string]interface{}{
				"from": "0x0",
				"gas":  "0x0",
			},
		}
		assert.True(t, allPhantomTransactions(txs))
	})

	t.Run("SingleRealTx", func(t *testing.T) {
		txs := []any{
			map[string]interface{}{
				"from": "0xdead000000000000000000000000000000000001",
				"gas":  "0x5208",
			},
		}
		assert.False(t, allPhantomTransactions(txs))
	})

	t.Run("MixOfPhantomAndReal", func(t *testing.T) {
		txs := []any{
			map[string]interface{}{
				"from": "0x0000000000000000000000000000000000000000",
				"gas":  "0x0",
			},
			map[string]interface{}{
				"from": "0xdead000000000000000000000000000000000001",
				"gas":  "0x5208",
			},
		}
		assert.False(t, allPhantomTransactions(txs))
	})

	t.Run("HashOnlyTxs_ConservativelyNotPhantom", func(t *testing.T) {
		txs := []any{
			"0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		}
		assert.False(t, allPhantomTransactions(txs))
	})

	t.Run("PhantomWithNonZeroGas_NotPhantom", func(t *testing.T) {
		txs := []any{
			map[string]interface{}{
				"from": "0x0000000000000000000000000000000000000000",
				"gas":  "0x1",
			},
		}
		assert.False(t, allPhantomTransactions(txs))
	})

	t.Run("PhantomWithNonZeroFrom_NotPhantom", func(t *testing.T) {
		txs := []any{
			map[string]interface{}{
				"from": "0x0000000000000000000000000000000000000001",
				"gas":  "0x0",
			},
		}
		assert.False(t, allPhantomTransactions(txs))
	})
}

func TestIsZeroishHex(t *testing.T) {
	assert.True(t, isZeroishHex("0x0"))
	assert.True(t, isZeroishHex("0x00"))
	assert.True(t, isZeroishHex("0x0000"))
	assert.False(t, isZeroishHex("0x1"))
	assert.False(t, isZeroishHex("0x10"))
	assert.False(t, isZeroishHex(""))
	assert.True(t, isZeroishHex("0x"))
}

func TestValidateBlock_PolygonPhantomTx(t *testing.T) {
	// This is a real Polygon block (0x2ab1350) that contains a phantom
	// system tx (0x0→0x0, gas=0) yet has the empty trie root.
	// The validation must NOT reject this block.
	polygonBlockJSON := `{
		"baseFeePerGas": "0x127a0af2af",
		"difficulty": "0x17",
		"extraData": "0xd782040083626f7289676f312e31392e3130856c696e757800000000000000002cb8b173f2887a24de99668adf6fb0102f0c8f5aed8fc8351c6606b6be70d2d429664231a27d2b92ea26a37839ccc2754e25b9fe4e63846d9108a20f39aea82e00",
		"gasLimit": "0x1c9c380",
		"gasUsed": "0x0",
		"hash": "0xb6d57cf8dd6a6fa80bfc3c8e73bf95f9317ddfe688a7d00e13f33987cd1efa84",
		"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
		"miner": "0x0000000000000000000000000000000000000000",
		"mixHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
		"nonce": "0x0000000000000000",
		"number": "0x2ab1350",
		"parentHash": "0x7f7f6b3850a66a0b35691c8c369955174591dc66834ce6c5bdbc111e3820417c",
		"receiptsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"sha3Uncles": "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
		"size": "0x269",
		"stateRoot": "0xdb5c7921cfc64e7f7f66d33f25aecc18b81191b3294ae711c78faf8a338ca687",
		"timestamp": "0x64a73189",
		"transactions": [
			{
				"blockHash": "0xb6d57cf8dd6a6fa80bfc3c8e73bf95f9317ddfe688a7d00e13f33987cd1efa84",
				"blockNumber": "0x2ab1350",
				"from": "0x0000000000000000000000000000000000000000",
				"gas": "0x0",
				"gasPrice": "0x0",
				"hash": "0x51a02e573c5d7bb8156dcbd074e76d38da875c2ee9ce0801992a90d0e60b2cb6",
				"input": "0x",
				"nonce": "0x0",
				"to": "0x0000000000000000000000000000000000000000",
				"transactionIndex": "0x0",
				"value": "0x0",
				"type": "0x0",
				"v": "0x0",
				"r": "0x0",
				"s": "0x0"
			}
		],
		"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"uncles": []
	}`

	ctx := context.Background()

	// Build a NormalizedResponse wrapping the JSON-RPC result
	jrpcResp, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(polygonBlockJSON), nil)
	assert.NoError(t, err)
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrpcResp)

	dirs := &common.RequestDirectives{
		ValidateTransactionsRoot: true,
	}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x2ab1350",true]}`))
	req.SetDirectives(dirs)
	resp.WithRequest(req)

	err = validateBlock(ctx, nil, dirs, resp)
	assert.NoError(t, err, "Polygon phantom tx block should pass transactions root validation")
}

func TestValidateBlock_EmptyRootWithRealTx_Fails(t *testing.T) {
	// A block with the empty trie root but a REAL transaction should still fail.
	blockJSON := `{
		"hash": "0xabc123",
		"parentHash": "0xdef456",
		"stateRoot": "0x1234",
		"receiptsRoot": "0x1234",
		"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
		"number": "0x100",
		"transactions": [
			{
				"blockHash": "0xabc123",
				"blockNumber": "0x100",
				"from": "0xdeadbeef00000000000000000000000000000001",
				"gas": "0x5208",
				"gasPrice": "0x1",
				"hash": "0xfff111",
				"input": "0x",
				"nonce": "0x0",
				"to": "0xdeadbeef00000000000000000000000000000002",
				"transactionIndex": "0x0",
				"value": "0x1",
				"type": "0x0",
				"v": "0x1b",
				"r": "0x1",
				"s": "0x1"
			}
		],
		"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"
	}`

	ctx := context.Background()

	jrpcResp, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(blockJSON), nil)
	assert.NoError(t, err)
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrpcResp)

	dirs := &common.RequestDirectives{
		ValidateTransactionsRoot: true,
	}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x100",true]}`))
	req.SetDirectives(dirs)
	resp.WithRequest(req)

	err = validateBlock(ctx, nil, dirs, resp)
	assert.Error(t, err, "Block with empty trie root but real tx should fail validation")
	assert.Contains(t, err.Error(), "non-phantom")
}

func TestEnforceNonNullTaggedBlocks(t *testing.T) {
	t.Run("TaggedBlockWithEnforcementDisabled_ReturnsNull", func(t *testing.T) {
		// Create a request with a block tag ("pending") and directive disabled
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: false,
		})

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(request, response)

		// Assert: Should return null without error for tagged blocks when enforcement is disabled
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})

	t.Run("NumericBlockWithEnforcementDisabled_StillReturnsError", func(t *testing.T) {
		// Create a request with a numeric block (hex number) and directive disabled
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"0x1234", true}),
		)
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: false,
		})

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(request, response)

		// Assert: Numeric blocks ALWAYS return error when null, regardless of directive
		// This is the key behavior: numeric null blocks indicate real data problems (pruned/missing)
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "block not found")
	})

	t.Run("TaggedBlockWithEnforcementEnabled_ReturnsError", func(t *testing.T) {
		// Create a request with a block tag ("pending") and directive enabled
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: true,
		})

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(request, response)

		// Assert: Should return error for tagged blocks when enforcement is enabled
		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "block not found")
	})

	t.Run("LatestTagWithEnforcementDisabled_ReturnsNull", func(t *testing.T) {
		// Create a request with "latest" tag and directive disabled
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", true}),
		)
		request.SetDirectives(&common.RequestDirectives{
			EnforceNonNullTaggedBlocks: false,
		})

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(request, response)

		// Assert: Should return null without error for "latest" tag when enforcement is disabled
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})

	t.Run("TaggedBlockWithNilDirectives_DefaultsToNoEnforce", func(t *testing.T) {
		// Create a request WITHOUT setting directives (nil)
		// This tests the behavior when directives are not set
		request := common.NewNormalizedRequestFromJsonRpcRequest(
			common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"pending", true}),
		)
		// Note: directives are nil by default

		// Create a response with null result
		jsonResp, _ := common.NewJsonRpcResponse(1, nil, nil)
		response := common.NewNormalizedResponse().
			WithRequest(request).
			WithJsonRpcResponse(jsonResp)

		// Call enforceNonNullBlock
		result, err := enforceNonNullBlock(request, response)

		// Assert: When directives are nil, should NOT enforce (allow null)
		// The defaults are applied at network level via DirectiveDefaults.SetDefaults()
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})
}
