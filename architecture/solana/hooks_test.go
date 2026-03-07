package solana

import (
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeResp builds a minimal *http.Response with the given status code.
func fakeResp(statusCode int) *http.Response {
	return &http.Response{StatusCode: statusCode}
}

// fakeJrr builds a JsonRpcResponse with an error set.
func fakeJrrWithError(code int, message string) *common.JsonRpcResponse {
	return &common.JsonRpcResponse{
		Error: common.NewErrJsonRpcExceptionExternal(code, message, ""),
	}
}

// fakeJrrOK builds a JsonRpcResponse with no error.
func fakeJrrOK() *common.JsonRpcResponse {
	return &common.JsonRpcResponse{}
}

func TestJsonRpcErrorExtractor_NodeUnhealthy(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32005, "Node is unhealthy"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "solana node unhealthy")
	assert.Contains(t, err.Error(), "Node is unhealthy")
}

func TestJsonRpcErrorExtractor_BlockNotAvailable(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32004, "Block not available"), nil)
	require.Error(t, err)
	// Should be mapped to ErrEndpointMissingData
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
		"error should be ErrEndpointMissingData for -32004")
}

func TestJsonRpcErrorExtractor_SlotSkipped(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32007, "Slot 1234 was skipped"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
		"error should be ErrEndpointMissingData for -32007")
}

func TestJsonRpcErrorExtractor_GenericRpcError(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32602, "Invalid params"), nil)
	require.Error(t, err)
	// Any non-zero code that isn't -32005/-32004/-32007 should be client-side exception
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"error should be ErrEndpointClientSideException for generic JSON-RPC error")
}

func TestJsonRpcErrorExtractor_NoError(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrOK(), nil)
	assert.NoError(t, err, "no error should be returned when JsonRpcResponse.Error is nil")
}

func TestJsonRpcErrorExtractor_NilJrr(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, nil, nil)
	assert.NoError(t, err, "no error should be returned when jrr is nil")
}
