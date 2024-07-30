package erpc

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestExtractBlockReferenceFromResponse(t *testing.T) {
	tests := []struct {
		name     string
		rpcReq   *common.JsonRpcRequest
		rpcResp  *common.JsonRpcResponse
		expected string
		expUint  uint64
		expErr   bool
	}{
		{
			name: "eth_getTransactionReceipt with valid block number",
			rpcReq: &common.JsonRpcRequest{
				Method: "eth_getTransactionReceipt",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: map[string]interface{}{
					"blockNumber": "0x1b4",
				},
			},
			expected: "436",
			expUint:  436,
			expErr:   false,
		},
		{
			name: "eth_getTransactionReceipt with invalid block number",
			rpcReq: &common.JsonRpcRequest{
				Method: "eth_getTransactionReceipt",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: map[string]interface{}{
					"blockNumber": "invalid",
				},
			},
			expected: "",
			expUint:  0,
			expErr:   true,
		},
		{
			name: "eth_getTransactionReceipt with missing block number",
			rpcReq: &common.JsonRpcRequest{
				Method: "eth_getTransactionReceipt",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: map[string]interface{}{},
			},
			expected: "",
			expUint:  0,
			expErr:   false,
		},
		{
			name: "eth_getTransactionByHash with blockHash",
			rpcReq: &common.JsonRpcRequest{
				Method: "eth_getTransactionByHash",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: map[string]interface{}{
					"blockHash": "0xabc123",
				},
			},
			expected: "0xabc123",
			expUint:  0,
			expErr:   false,
		},
		{
			name: "eth_getTransactionByHash with blockNumber",
			rpcReq: &common.JsonRpcRequest{
				Method: "eth_getTransactionByHash",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: map[string]interface{}{
					"blockNumber": "0x1b4",
				},
			},
			expected: "436",
			expUint:  436,
			expErr:   false,
		},
		{
			name: "eth_getTransactionByHash with pending transaction",
			rpcReq: &common.JsonRpcRequest{
				Method: "eth_getTransactionByHash",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: map[string]interface{}{
					"blockHash":   nil,
					"blockNumber": nil,
				},
			},
			expected: "",
			expUint:  0,
			expErr:   false,
		},
		{
			name: "arbtrace_replayTransaction",
			rpcReq: &common.JsonRpcRequest{
				Method: "arbtrace_replayTransaction",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: nil,
			},
			expected: "nil",
			expUint:  1,
			expErr:   false,
		},
		{
			name: "eth_chainId",
			rpcReq: &common.JsonRpcRequest{
				Method: "eth_chainId",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: nil,
			},
			expected: "all",
			expUint:  1,
			expErr:   false,
		},
		{
			name: "web3_sha",
			rpcReq: &common.JsonRpcRequest{
				Method: "web3_sha3",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: nil,
			},
			expected: "all",
			expUint:  1,
			expErr:   false,
		},
		{
			name: "invalid method",
			rpcReq: &common.JsonRpcRequest{
				Method: "invalid_method",
			},
			rpcResp: &common.JsonRpcResponse{
				Result: nil,
			},
			expected: "",
			expUint:  0,
			expErr:   false,
		},
		{
			name:   "nil request",
			rpcReq: nil,
			rpcResp: &common.JsonRpcResponse{
				Result: nil,
			},
			expected: "",
			expUint:  0,
			expErr:   true,
		},
		{
			name: "nil response",
			rpcReq: &common.JsonRpcRequest{
				Method: "eth_chainId",
			},
			rpcResp:  nil,
			expected: "",
			expUint:  0,
			expErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, resultUint, err := extractBlockReferenceFromResponse(tt.rpcReq, tt.rpcResp)
			if tt.expErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expected, result)
			assert.Equal(t, tt.expUint, resultUint)
		})
	}
}
