package evm

import (
	"testing"

	"github.com/flair-sdk/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestExtractBlockReference(t *testing.T) {
	tests := []struct {
		name     string
		request  *common.JsonRpcRequest
		expected string
		expUint  uint64
		expErr   bool
	}{
		// METHODS
		{
			name: "eth_getBlockByNumber",
			request: &common.JsonRpcRequest{
				Method: "eth_getBlockByNumber",
				Params: []interface{}{"0xc5043f", false},
			},
			expected: "12911679",
			expUint:  12911679,
			expErr:   false,
		},
		{
			name: "eth_getUncleByBlockNumberAndIndex",
			request: &common.JsonRpcRequest{
				Method: "eth_getUncleByBlockNumberAndIndex",
				Params: []interface{}{"0x1b4"},
			},
			expected: "436",
			expUint:  436,
			expErr:   false,
		},
		{
			name: "eth_getTransactionByBlockNumberAndIndex",
			request: &common.JsonRpcRequest{
				Method: "eth_getTransactionByBlockNumberAndIndex",
				Params: []interface{}{"0xc5043f", "0x0"},
			},
			expected: "12911679",
			expUint:  12911679,
			expErr:   false,
		},
		{
			name: "eth_getUncleCountByBlockNumber",
			request: &common.JsonRpcRequest{
				Method: "eth_getUncleCountByBlockNumber",
				Params: []interface{}{"0xc5043f"},
			},
			expected: "12911679",
			expUint:  12911679,
			expErr:   false,
		},
		{
			name: "eth_getBlockTransactionCountByNumber",
			request: &common.JsonRpcRequest{
				Method: "eth_getUncleCountByBlockNumber",
				Params: []interface{}{"0xc5043f"},
			},
			expected: "12911679",
			expUint:  12911679,
			expErr:   false,
		},
		{
			name: "eth_getBlockReceipts",
			request: &common.JsonRpcRequest{
				Method: "eth_getBlockReceipts",
				Params: []interface{}{"0xc5043f"},
			},
			expected: "12911679",
			expUint:  12911679,
			expErr:   false,
		},
		{
			name: "eth_getLogs",
			request: &common.JsonRpcRequest{
				Method: "eth_getLogs",
				Params: []interface{}{
					map[string]interface{}{
						"fromBlock": "0x1b4",
						"toBlock":   "0x1b5",
					},
				},
			},
			expected: "0x1b4-0x1b5",
			expUint:  437,
			expErr:   false,
		},
		{
			name: "eth_getBalance",
			request: &common.JsonRpcRequest{
				Method: "eth_getBalance",
				Params: []interface{}{"0xAddress", "0x1b4"},
			},
			expected: "436",
			expUint:  436,
			expErr:   false,
		},
		{
			name: "eth_getBlockByHash",
			request: &common.JsonRpcRequest{
				Method: "eth_getBlockByHash",
				Params: []interface{}{"0xBlockHash"},
			},
			expected: "0xBlockHash",
			expUint:  0,
			expErr:   false,
		},
		{
			name: "eth_getProof",
			request: &common.JsonRpcRequest{
				Method: "eth_getProof",
				Params: []interface{}{"0xAddress", "keys", "0x1b4"},
			},
			expected: "436",
			expUint:  436,
			expErr:   false,
		},
		{
			name:     "nil request",
			request:  nil,
			expected: "",
			expUint:  0,
			expErr:   true,
		},
		{
			name: "invalid hex in eth_getBlockByNumber",
			request: &common.JsonRpcRequest{
				Method: "eth_getBlockByNumber",
				Params: []interface{}{"invalidHex"},
			},
			expected: "",
			expUint:  0,
			expErr:   false,
		},
		{
			name: "missing parameters in eth_getLogs",
			request: &common.JsonRpcRequest{
				Method: "eth_getLogs",
				Params: []interface{}{},
			},
			expected: "",
			expUint:  0,
			expErr:   false,
		},
		{
			name: "missing parameters in eth_getBalance",
			request: &common.JsonRpcRequest{
				Method: "eth_getBalance",
				Params: []interface{}{"0xAddress"},
			},
			expected: "",
			expUint:  0,
			expErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, resultUint, err := ExtractBlockReference(tt.request)
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
