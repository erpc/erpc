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
	cfg           *common.NetworkConfig
	finalityState common.DataFinalityState
	highestLatest int64
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
	return t.highestLatest
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
	return t.finalityState
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

	t.Run("HyperEVMSystemTxs_SyntheticSignature", func(t *testing.T) {
		// Real HyperEVM testnet block 57659287 (0x36fcf97): two native/L1
		// system txs with non-zero gas and real-looking from addresses, but a
		// synthetic signature where r=0x1 and gasPrice=0. The Polygon heuristic
		// misses these; the HyperEVM marker must catch them.
		txs := []any{
			map[string]interface{}{
				"from":     "0xf9b10ef826e9aa275f1813034e3bd9b80224e535",
				"gas":      "0x30d40",
				"gasPrice": "0x0",
				"r":        "0x1",
				"s":        "0xf9b10ef826e9aa275f1813034e3bd9b80224e535",
			},
			map[string]interface{}{
				"from":     "0x2000000000000000000000000000000000000000",
				"gas":      "0x30d40",
				"gasPrice": "0x0",
				"r":        "0x1",
				"s":        "0x2000000000000000000000000000000000000000",
			},
		}
		assert.True(t, allPhantomTransactions(txs))
	})

	t.Run("HyperEVMHypeBridge_SIsOneNotFrom", func(t *testing.T) {
		// Real HyperEVM testnet block 59707830: HYPE bridge system tx encodes
		// sender 0x2222… as s=1 (not s==from). r=0x1 + gasPrice=0 must still
		// classify it as phantom — the old s==from heuristic misses this.
		txs := []any{
			map[string]interface{}{
				"from":     "0x2222222222222222222222222222222222222222",
				"gas":      "0x7530",
				"gasPrice": "0x0",
				"r":        "0x1",
				"s":        "0x1",
			},
		}
		assert.True(t, allPhantomTransactions(txs))
	})

	t.Run("HyperEVMSystemTx_CaseInsensitive", func(t *testing.T) {
		txs := []any{
			map[string]interface{}{
				"from":     "0xF9B10EF826E9AA275F1813034E3BD9B80224E535",
				"gas":      "0x30d40",
				"gasPrice": "0x00",
				"r":        "0x01",
			},
		}
		assert.True(t, allPhantomTransactions(txs))
	})

	t.Run("RealTx_ROneButNonZeroGasPrice_NotPhantom", func(t *testing.T) {
		// A signed tx can legitimately have r==1 by chance, but gasPrice will
		// not be zero — must NOT be treated as phantom.
		txs := []any{
			map[string]interface{}{
				"from":     "0xdead000000000000000000000000000000000001",
				"gas":      "0x5208",
				"gasPrice": "0x3b9aca00",
				"r":        "0x1",
				"s":        "0x8f31c2a0b4e5d6f7089a1b2c3d4e5f60718293a4b5c6d7e8f9012345678990ab",
			},
		}
		assert.False(t, allPhantomTransactions(txs))
	})

	t.Run("RealTx_GasPriceZeroButRNotOne_NotPhantom", func(t *testing.T) {
		txs := []any{
			map[string]interface{}{
				"from":     "0xf9b10ef826e9aa275f1813034e3bd9b80224e535",
				"gas":      "0x5208",
				"gasPrice": "0x0",
				"r":        "0x2c9a1f0000000000000000000000000000000000000000000000000000000000",
				"s":        "0xf9b10ef826e9aa275f1813034e3bd9b80224e535",
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

func TestValidateBlock_HyperEVMSystemTx(t *testing.T) {
	// Real HyperEVM testnet block 57659287 (0x36fcf97): empty trie root,
	// gasUsed=0, and two native/L1 system txs with non-zero gas, real-looking
	// from addresses, and a synthetic signature (r=0x1, gasPrice=0). This must
	// pass transactions-root validation.
	hyperEVMBlockJSON := `{
		"number": "0x36fcf97",
		"hash": "0xaee844ce1767d67eaef2537ddb88bfee14d900610791d6e8de76b544acc55415",
		"parentHash": "0x586fea959adbed32ec0341782bd9d1a2c3f0cb25cb72fa707706d1b0fa7d7a2d",
		"stateRoot": "0x0000000000000000000000000000000000000000000000000000000000000000",
		"receiptsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
		"gasUsed": "0x0",
		"gasLimit": "0x1c9c380",
		"transactions": [
			{
				"blockNumber": "0x36fcf97",
				"chainId": "0x3e6",
				"from": "0xf9b10ef826e9aa275f1813034e3bd9b80224e535",
				"gas": "0x30d40",
				"gasPrice": "0x0",
				"hash": "0x75d287e0a8a32757718551e340c297ad01aa095866e18a7381304c751d167f4e",
				"nonce": "0x3e6",
				"r": "0x1",
				"s": "0xf9b10ef826e9aa275f1813034e3bd9b80224e535",
				"to": "0x2b3370ee501b4a559b57d449569354196457d8ab",
				"transactionIndex": "0x0",
				"type": "0x0",
				"v": "0x7f0",
				"value": "0x0"
			},
			{
				"blockNumber": "0x36fcf97",
				"chainId": "0x3e6",
				"from": "0x2000000000000000000000000000000000000000",
				"gas": "0x30d40",
				"gasPrice": "0x0",
				"hash": "0xab5b62d03bc4e2fe3e650ef1ace54f0f0bc40b0364ca69c2cad7b3ed927259a4",
				"nonce": "0x246a",
				"r": "0x1",
				"s": "0x2000000000000000000000000000000000000000",
				"to": "0x0b80659a4076e9e93c7dbe0f10675a16a3e5c206",
				"transactionIndex": "0x1",
				"type": "0x0",
				"v": "0x7f0",
				"value": "0x0"
			}
		],
		"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"uncles": []
	}`

	ctx := context.Background()

	jrpcResp, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(hyperEVMBlockJSON), nil)
	assert.NoError(t, err)
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrpcResp)

	dirs := &common.RequestDirectives{
		ValidateTransactionsRoot: true,
	}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x36fcf97",true]}`))
	req.SetDirectives(dirs)
	resp.WithRequest(req)

	err = validateBlock(ctx, nil, dirs, resp)
	assert.NoError(t, err, "HyperEVM system-tx block should pass transactions root validation")
}

func TestValidateBlock_HyperEVMHypeBridgeSystemTx(t *testing.T) {
	// Real HyperEVM testnet block 59707830 (0x38f1176): empty trie root with a
	// single HYPE bridge system tx (from=0x2222…, r=0x1, s=0x1, gasPrice=0).
	// s != from here — gasPrice+r marker must accept this block.
	hyperEVMBlockJSON := `{
		"number": "0x38f1176",
		"hash": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"parentHash": "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"stateRoot": "0x0000000000000000000000000000000000000000000000000000000000000000",
		"receiptsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"logsBloom": "0x00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
		"gasUsed": "0x0",
		"gasLimit": "0x1c9c380",
		"transactions": [
			{
				"blockNumber": "0x38f1176",
				"from": "0x2222222222222222222222222222222222222222",
				"gas": "0x7530",
				"gasPrice": "0x0",
				"hash": "0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				"nonce": "0x0",
				"r": "0x1",
				"s": "0x1",
				"to": "0xdddddddddddddddddddddddddddddddddddddddd",
				"transactionIndex": "0x0",
				"type": "0x0",
				"v": "0x7f0",
				"value": "0x0"
			}
		],
		"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"uncles": []
	}`

	ctx := context.Background()

	jrpcResp, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(hyperEVMBlockJSON), nil)
	assert.NoError(t, err)
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrpcResp)

	dirs := &common.RequestDirectives{
		ValidateTransactionsRoot: true,
	}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x38f1176",true]}`))
	req.SetDirectives(dirs)
	resp.WithRequest(req)

	err = validateBlock(ctx, nil, dirs, resp)
	assert.NoError(t, err, "HyperEVM HYPE-bridge system-tx block should pass transactions root validation")
}

func TestNormHexEqHexIsOneHex(t *testing.T) {
	assert.Equal(t, "0", normHex("0x0"))
	assert.Equal(t, "0", normHex("0x0000"))
	assert.Equal(t, "0", normHex(""))
	assert.Equal(t, "1", normHex("0x01"))
	assert.Equal(t, "abc", normHex("0x0ABC"))

	assert.True(t, eqHex("0xf9b10ef826e9aa275f1813034e3bd9b80224e535", "0xF9B10EF826E9AA275F1813034E3BD9B80224E535"))
	assert.True(t, eqHex("0x01", "0x1"))
	assert.False(t, eqHex("0x1", "0x2"))

	assert.True(t, isOneHex("0x1"))
	assert.True(t, isOneHex("0x01"))
	assert.False(t, isOneHex("0x0"))
	assert.False(t, isOneHex("0x2"))
	assert.False(t, isOneHex(""))
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
		result, err := enforceNonNullBlock(context.Background(), request, response)

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
		result, err := enforceNonNullBlock(context.Background(), request, response)

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
		result, err := enforceNonNullBlock(context.Background(), request, response)

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
		result, err := enforceNonNullBlock(context.Background(), request, response)

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
		result, err := enforceNonNullBlock(context.Background(), request, response)

		// Assert: When directives are nil, should NOT enforce (allow null)
		// The defaults are applied at network level via DirectiveDefaults.SetDefaults()
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.True(t, result.IsResultEmptyish())
	})
}
