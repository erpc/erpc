package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractBlockReference(t *testing.T) {
	tests := []struct {
		name        string
		request     *JsonRpcRequest
		response    *JsonRpcResponse
		expectedRef string
		expectedNum int64
		expectedErr bool
	}{
		{
			name:        "nil request",
			request:     nil,
			expectedRef: "",
			expectedNum: 0,
			expectedErr: true,
		},
		{
			name: "eth_getBlockByNumber",
			request: &JsonRpcRequest{
				Method: "eth_getBlockByNumber",
				Params: []interface{}{"0xc5043f", false},
			},
			expectedRef: "12911679",
			expectedNum: 12911679,
			expectedErr: false,
		},
		{
			name: "invalid hex in eth_getBlockByNumber",
			request: &JsonRpcRequest{
				Method: "eth_getBlockByNumber",
				Params: []interface{}{"invalidHex"},
			},
			expectedRef: "",
			expectedNum: 0,
			expectedErr: false,
		},
		{
			name: "eth_getUncleByBlockNumberAndIndex",
			request: &JsonRpcRequest{
				Method: "eth_getUncleByBlockNumberAndIndex",
				Params: []interface{}{"0x1b4"},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_getTransactionByBlockNumberAndIndex",
			request: &JsonRpcRequest{
				Method: "eth_getTransactionByBlockNumberAndIndex",
				Params: []interface{}{"0xc5043f", "0x0"},
			},
			expectedRef: "12911679",
			expectedNum: 12911679,
			expectedErr: false,
		},
		{
			name: "eth_getUncleCountByBlockNumber",
			request: &JsonRpcRequest{
				Method: "eth_getUncleCountByBlockNumber",
				Params: []interface{}{"0xc5043f"},
			},
			expectedRef: "12911679",
			expectedNum: 12911679,
			expectedErr: false,
		},
		{
			name: "eth_getBlockTransactionCountByNumber",
			request: &JsonRpcRequest{
				Method: "eth_getBlockTransactionCountByNumber",
				Params: []interface{}{"0xc5043f"},
			},
			expectedRef: "12911679",
			expectedNum: 12911679,
			expectedErr: false,
		},
		{
			name: "eth_getBlockReceipts",
			request: &JsonRpcRequest{
				Method: "eth_getBlockReceipts",
				Params: []interface{}{"0xc5043f"},
			},
			expectedRef: "12911679",
			expectedNum: 12911679,
			expectedErr: false,
		},
		{
			name: "eth_getLogs",
			request: &JsonRpcRequest{
				Method: "eth_getLogs",
				Params: []interface{}{
					map[string]interface{}{
						"fromBlock": "0x1b4",
						"toBlock":   "0x1b5",
					},
				},
			},
			expectedRef: "0x1b4-0x1b5",
			expectedNum: 437,
			expectedErr: false,
		},
		{
			name: "missing parameters in eth_getLogs",
			request: &JsonRpcRequest{
				Method: "eth_getLogs",
				Params: []interface{}{},
			},
			expectedRef: "",
			expectedNum: 0,
			expectedErr: false,
		},
		{
			name: "eth_getBalance",
			request: &JsonRpcRequest{
				Method: "eth_getBalance",
				Params: []interface{}{"0xAddress", "0x1b4"},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "missing parameters in eth_getBalance",
			request: &JsonRpcRequest{
				Method: "eth_getBalance",
				Params: []interface{}{"0xAddress"},
			},
			expectedRef: "",
			expectedNum: 0,
			expectedErr: true,
		},
		{
			name: "eth_getCode",
			request: &JsonRpcRequest{
				Method: "eth_getCode",
				Params: []interface{}{"0xAddress", "0x1b4"},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_getTransactionCount",
			request: &JsonRpcRequest{
				Method: "eth_getTransactionCount",
				Params: []interface{}{"0xAddress", "0x1b4"},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_call",
			request: &JsonRpcRequest{
				Method: "eth_call",
				Params: []interface{}{
					map[string]interface{}{
						"from": nil,
						"to":   "0x6b175474e89094c44da98b954eedeac495271d0f",
						"data": "0x70a082310000000000000000000000006E0d01A76C3Cf4288372a29124A26D4353EE51BE",
					},
					"0x1b4",
					map[string]interface{}{
						"0x1111111111111111111111111111111111111111": map[string]interface{}{
							"balance": "0xFFFFFFFFFFFFFFFFFFFF",
						},
					},
				},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_call with blockHash object",
			request: &JsonRpcRequest{
				Method: "eth_call",
				Params: []interface{}{
					map[string]interface{}{
						"from": nil,
						"to":   "0x6b175474e89094c44da98b954eedeac495271d0f",
						"data": "0x70a082310000000000000000000000006E0d01A76C3Cf4288372a29124A26D4353EE51BE",
					},
					map[string]interface{}{
						"blockHash": "0x3f07a9c83155594c000642e7d60e8a8a00038d03e9849171a05ed0e2d47acbb3",
					},
				},
			},
			expectedRef: "0x3f07a9c83155594c000642e7d60e8a8a00038d03e9849171a05ed0e2d47acbb3",
			expectedNum: 0,
			expectedErr: false,
		},
		{
			name: "eth_call with blockNumber object",
			request: &JsonRpcRequest{
				Method: "eth_call",
				Params: []interface{}{
					map[string]interface{}{
						"from": nil,
						"to":   "0x6b175474e89094c44da98b954eedeac495271d0f",
						"data": "0x70a082310000000000000000000000006E0d01A76C3Cf4288372a29124A26D4353EE51BE",
					},
					map[string]interface{}{
						"blockNumber": "0x1b4",
					},
				},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_feeHistory",
			request: &JsonRpcRequest{
				Method: "eth_feeHistory",
				Params: []interface{}{"0x8D97689C9818892B700e27F316cc3E41e17fBeb9", "0x1b4"},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_getAccount",
			request: &JsonRpcRequest{
				Method: "eth_getAccount",
				Params: []interface{}{4, "0x1b4", []interface{}{25, 75}},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_getBlockByHash",
			request: &JsonRpcRequest{
				Method: "eth_getBlockByHash",
				Params: []interface{}{"0x3f07a9c83155594c000642e7d60e8a8a00038d03e9849171a05ed0e2d47acbb3", false},
			},
			expectedRef: "0x3f07a9c83155594c000642e7d60e8a8a00038d03e9849171a05ed0e2d47acbb3",
			expectedNum: 0,
			expectedErr: false,
		},
		{
			name: "eth_getTransactionByBlockHashAndIndex",
			request: &JsonRpcRequest{
				Method: "eth_getTransactionByBlockHashAndIndex",
				Params: []interface{}{"0x829df9bb801fc0494abf2f443423a49ffa32964554db71b098d332d87b70a48b", "0x0"},
			},
			expectedRef: "0x829df9bb801fc0494abf2f443423a49ffa32964554db71b098d332d87b70a48b",
			expectedNum: 0,
			expectedErr: false,
		},
		{
			name: "eth_getBlockTransactionCountByHash",
			request: &JsonRpcRequest{
				Method: "eth_getBlockTransactionCountByHash",
				Params: []interface{}{"0x829df9bb801fc0494abf2f443423a49ffa32964554db71b098d332d87b70a48b"},
			},
			expectedRef: "0x829df9bb801fc0494abf2f443423a49ffa32964554db71b098d332d87b70a48b",
			expectedNum: 0,
			expectedErr: false,
		},
		{
			name: "eth_getUncleCountByBlockHash",
			request: &JsonRpcRequest{
				Method: "eth_getUncleCountByBlockHash",
				Params: []interface{}{"0x829df9bb801fc0494abf2f443423a49ffa32964554db71b098d332d87b70a48b"},
			},
			expectedRef: "0x829df9bb801fc0494abf2f443423a49ffa32964554db71b098d332d87b70a48b",
			expectedNum: 0,
			expectedErr: false,
		},
		{
			name: "eth_getProof",
			request: &JsonRpcRequest{
				Method: "eth_getProof",
				Params: []interface{}{
					"0x7F0d15C7FAae65896648C8273B6d7E43f58Fa842",
					[]interface{}{"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"},
					"0x1b4",
				},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_getProof with blockHash object",
			request: &JsonRpcRequest{
				Method: "eth_getProof",
				Params: []interface{}{
					"0x7F0d15C7FAae65896648C8273B6d7E43f58Fa842",
					[]interface{}{"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"},
					map[string]interface{}{
						"blockHash": "0x3f07a9c83155594c000642e7d60e8a8a00038d03e9849171a05ed0e2d47acbb3",
					},
				},
			},
			expectedRef: "0x3f07a9c83155594c000642e7d60e8a8a00038d03e9849171a05ed0e2d47acbb3",
			expectedNum: 0,
			expectedErr: false,
		},
		{
			name: "eth_getProof with blockNumber object",
			request: &JsonRpcRequest{
				Method: "eth_getProof",
				Params: []interface{}{
					"0x7F0d15C7FAae65896648C8273B6d7E43f58Fa842",
					[]interface{}{"0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"},
					map[string]interface{}{
						"blockNumber": "0x1b4",
					},
				},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_getStorageAt",
			request: &JsonRpcRequest{
				Method: "eth_getStorageAt",
				Params: []interface{}{
					"0xE592427A0AEce92De3Edee1F18E0157C05861564",
					"0x0",
					"0x1b4",
				},
			},
			expectedRef: "436",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_getTransactionReceipt with valid block number",
			request: &JsonRpcRequest{
				Method: "eth_getTransactionReceipt",
			},
			response: &JsonRpcResponse{
				Result: []byte(`{"blockNumber":"0x1b4","blockHash":"0xaaaaaabbbbccccc"}`),
			},
			expectedRef: "*",
			expectedNum: 436,
			expectedErr: false,
		},
		{
			name: "eth_chainId",
			request: &JsonRpcRequest{
				Method: "eth_chainId",
			},
			expectedRef: "*",
			expectedNum: 0,
			expectedErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var blkRef string
			var blkNum int64
			var err error

			if tt.response == nil {
				blkRef, blkNum, err = ExtractEvmBlockReferenceFromRequest(tt.request)
			} else {
				blkRef, blkNum, err = ExtractEvmBlockReference(tt.request, tt.response)
			}

			if tt.expectedErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			assert.Equal(t, tt.expectedRef, blkRef)
			assert.Equal(t, tt.expectedNum, blkNum)
		})
	}
}
