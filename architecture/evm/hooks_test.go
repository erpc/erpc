package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestUpstreamPostForward_UnexpectedEmpty_ListedMethods(t *testing.T) {
	methods := []string{
		// Blocks
		"eth_getBlockByNumber",
		"eth_getBlockByHash",
		// Transactions
		"eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"eth_getTransactionByBlockHashAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		// Uncles/ommers
		"eth_getUncleByBlockHashAndIndex",
		"eth_getUncleByBlockNumberAndIndex",
		// Traces
		"debug_traceTransaction",
		"trace_transaction",
		"trace_block",
		"trace_get",
	}

	for _, m := range methods {
		// Build a minimal request with method m
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + m + `","params":["0x1"]}`))
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithJsonRpcResponse(&common.JsonRpcResponse{Result: []byte("null")})

		outResp, err := HandleUpstreamPostForward(
			context.Background(),
			nil,
			nil,
			req,
			resp,
			nil,
			false,
		)

		assert.Equal(t, resp, outResp, m+": response pointer should be unchanged")
		assert.Error(t, err, m+": expected error for empty result")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), m+": expected ErrEndpointMissingData")
	}
}
