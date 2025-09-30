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
		// Enable retryEmpty so the hook converts empty results to errors
		if dirs := req.Directives(); dirs != nil {
			dirs.RetryEmpty = true
		}
		jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
		assert.NoError(t, err)
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithJsonRpcResponse(jrr)

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

func TestUpstreamPostForward_UnexpectedEmpty_RetryEmptyFalse(t *testing.T) {
	methods := []string{
		"eth_getBlockByNumber",
		"eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"debug_traceTransaction",
		"trace_transaction",
	}

	for _, m := range methods {
		// Build a minimal request with method m
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + m + `","params":["0x1"]}`))
		// retryEmpty is false by default, but let's be explicit
		if dirs := req.Directives(); dirs != nil {
			dirs.RetryEmpty = false
		}
		jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
		assert.NoError(t, err)
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithJsonRpcResponse(jrr)

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
		assert.NoError(t, err, m+": expected no error when retryEmpty=false")
	}
}

func TestUpstreamPostForward_UnexpectedEmpty_NonListedMethods(t *testing.T) {
	// Methods that should NOT trigger error conversion even with empty results
	methods := []string{
		"eth_call",
		"eth_getBalance",
		"eth_getCode",
		"eth_getStorageAt",
		"eth_estimateGas",
	}

	for _, m := range methods {
		// Build a minimal request with method m
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + m + `","params":["0x1"]}`))
		// Enable retryEmpty - but these methods should still not convert to errors
		if dirs := req.Directives(); dirs != nil {
			dirs.RetryEmpty = true
		}
		jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
		assert.NoError(t, err)
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithJsonRpcResponse(jrr)

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
		assert.NoError(t, err, m+": expected no error for non-listed method even with retryEmpty=true")
	}
}
