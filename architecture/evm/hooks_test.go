package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// newTestNetworkWithMarkEmptyMethods creates a testNetwork (defined in eth_getBlockByNumber_test.go)
// with the specified MarkEmptyAsErrorMethods configuration
func newTestNetworkWithMarkEmptyMethods(methods []string) *testNetwork {
	return &testNetwork{
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				MarkEmptyAsErrorMethods: methods,
			},
		},
	}
}

func TestUpstreamPostForward_UnexpectedEmpty_ListedMethods(t *testing.T) {
	methods := []string{
		// Blocks (eth_getBlockByHash excluded - subgraphs return empty for it)
		"eth_getBlockByNumber",
		"eth_getBlockReceipts",
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

	// Create a test network with the default methods configured
	network := newTestNetworkWithMarkEmptyMethods(common.DefaultMarkEmptyAsErrorMethods)

	for _, m := range methods {
		// Build a minimal request with method m
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + m + `","params":["0x1"]}`))
		// Enable retryEmpty so the hook converts empty results to errors
		req.SetDirectives(&common.RequestDirectives{
			RetryEmpty: true,
		})
		jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
		assert.NoError(t, err)
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithJsonRpcResponse(jrr)

		outResp, err := HandleUpstreamPostForward(
			context.Background(),
			network,
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
		"eth_getBlockReceipts",
		"eth_getTransactionByHash",
		"eth_getTransactionReceipt",
		"debug_traceTransaction",
		"trace_transaction",
	}

	// Create a test network with the default methods configured
	network := newTestNetworkWithMarkEmptyMethods(common.DefaultMarkEmptyAsErrorMethods)

	for _, m := range methods {
		// Build a minimal request with method m
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + m + `","params":["0x1"]}`))
		// Explicitly set retryEmpty to false - should NOT convert empty to error
		req.SetDirectives(&common.RequestDirectives{
			RetryEmpty: false,
		})
		jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
		assert.NoError(t, err)
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithJsonRpcResponse(jrr)

		outResp, err := HandleUpstreamPostForward(
			context.Background(),
			network,
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

	// Create a test network with the default methods configured
	network := newTestNetworkWithMarkEmptyMethods(common.DefaultMarkEmptyAsErrorMethods)

	for _, m := range methods {
		// Build a minimal request with method m
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"` + m + `","params":["0x1"]}`))
		// Enable retryEmpty - but these methods should still not convert to errors
		req.SetDirectives(&common.RequestDirectives{
			RetryEmpty: true,
		})
		jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
		assert.NoError(t, err)
		resp := common.NewNormalizedResponse().
			WithRequest(req).
			WithJsonRpcResponse(jrr)

		outResp, err := HandleUpstreamPostForward(
			context.Background(),
			network,
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

func TestUpstreamPostForward_UnexpectedEmpty_CustomConfiguredMethods(t *testing.T) {
	// Test that users can configure custom methods to trigger the mark-empty behavior
	customMethods := []string{"custom_method", "another_custom"}
	network := newTestNetworkWithMarkEmptyMethods(customMethods)

	// Test that a configured custom method does trigger the error
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"custom_method","params":["0x1"]}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
	assert.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	_, err = HandleUpstreamPostForward(context.Background(), network, nil, req, resp, nil, false)
	assert.Error(t, err, "custom_method: expected error for empty result")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData), "custom_method: expected ErrEndpointMissingData")

	// Test that a non-configured method does NOT trigger the error (even if it's in the defaults)
	req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionByHash","params":["0x1"]}`))
	req2.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	jrr2, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
	assert.NoError(t, err)
	resp2 := common.NewNormalizedResponse().WithRequest(req2).WithJsonRpcResponse(jrr2)

	_, err = HandleUpstreamPostForward(context.Background(), network, nil, req2, resp2, nil, false)
	assert.NoError(t, err, "eth_getTransactionByHash: expected NO error when not in custom config")
}

func TestUpstreamPostForward_UnexpectedEmpty_EmptyConfig(t *testing.T) {
	// Test that an empty config disables the feature entirely
	network := newTestNetworkWithMarkEmptyMethods([]string{})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1"]}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`"1"`), []byte("null"), nil)
	assert.NoError(t, err)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	_, err = HandleUpstreamPostForward(context.Background(), network, nil, req, resp, nil, false)
	assert.NoError(t, err, "eth_getBlockByNumber: expected NO error when markEmptyAsErrorMethods is empty")
}
