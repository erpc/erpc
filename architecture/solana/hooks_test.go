package solana

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── test helpers ─────────────────────────────────────────────────────────────

func fakeResp(statusCode int) *http.Response {
	return &http.Response{StatusCode: statusCode}
}

func fakeJrrWithError(code int, message string) *common.JsonRpcResponse {
	return &common.JsonRpcResponse{
		Error: common.NewErrJsonRpcExceptionExternal(code, message, ""),
	}
}

func fakeJrrOK() *common.JsonRpcResponse {
	return &common.JsonRpcResponse{}
}

func makeRequest(method string) *common.NormalizedRequest {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, method)
	return common.NewNormalizedRequest([]byte(body))
}

// ── Gap 5: HandleProjectPreForward ───────────────────────────────────────────

func TestHandleProjectPreForward_ReturnsNotHandled(t *testing.T) {
	req := makeRequest("getSlot")
	handled, resp, err := HandleProjectPreForward(context.Background(), nil, req)
	assert.False(t, handled, "HandleProjectPreForward should not handle any Solana methods in Phase 1")
	assert.Nil(t, resp)
	assert.NoError(t, err)
}

func TestHandleProjectPreForward_NilNetwork(t *testing.T) {
	req := makeRequest("getBalance")
	assert.NotPanics(t, func() {
		handled, resp, err := HandleProjectPreForward(context.Background(), nil, req)
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})
}

func TestHandleProjectPreForward_NilRequest(t *testing.T) {
	assert.NotPanics(t, func() {
		handled, resp, err := HandleProjectPreForward(context.Background(), nil, nil)
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})
}

// ── Gap 5: HandleNetworkPreForward ───────────────────────────────────────────

func TestHandleNetworkPreForward_ReturnsNotHandled(t *testing.T) {
	req := makeRequest("getSlot")
	handled, resp, err := HandleNetworkPreForward(context.Background(), nil, nil, req)
	assert.False(t, handled)
	assert.Nil(t, resp)
	assert.NoError(t, err)
}

// ── Gap 5: HandleNetworkPostForward ──────────────────────────────────────────

func TestHandleNetworkPostForward_PassesThroughSuccess(t *testing.T) {
	req := makeRequest("getSlot")
	jrr, err := common.NewJsonRpcResponse(1, 404782323, nil)
	require.NoError(t, err)
	fakeResp := common.NewNormalizedResponse().
		WithRequest(req).
		WithJsonRpcResponse(jrr)

	outResp, outErr := HandleNetworkPostForward(context.Background(), nil, req, fakeResp, nil)
	assert.NoError(t, outErr)
	assert.Equal(t, fakeResp, outResp)
}

func TestHandleNetworkPostForward_PassesThroughError(t *testing.T) {
	req := makeRequest("getSlot")
	origErr := errors.New("upstream gone")

	outResp, outErr := HandleNetworkPostForward(context.Background(), nil, req, nil, origErr)
	assert.Equal(t, origErr, outErr, "error should be passed through unchanged")
	assert.Nil(t, outResp)
}

// ── Gap 4: HandleUpstreamPostForward — sendTransaction no-retry ──────────────

func TestHandleUpstreamPostForward_NoError_PassesThrough(t *testing.T) {
	req := makeRequest("sendTransaction")
	jrr, _ := common.NewJsonRpcResponse(1, "txsig123", nil)
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

	outResp, outErr := HandleUpstreamPostForward(context.Background(), nil, nil, req, resp, nil, false)
	assert.NoError(t, outErr)
	assert.Equal(t, resp, outResp)
}

func TestHandleUpstreamPostForward_SendTransaction_MarkedNonRetryable(t *testing.T) {
	req := makeRequest("sendTransaction")
	origErr := common.NewErrEndpointClientSideException(errors.New("bad tx"))

	_, outErr := HandleUpstreamPostForward(context.Background(), nil, nil, req, nil, origErr, false)
	require.Error(t, outErr)
	assert.False(t, common.IsRetryableTowardNetwork(outErr),
		"sendTransaction errors must NOT be retryable toward network")
}

func TestHandleUpstreamPostForward_SendRawTransaction_MarkedNonRetryable(t *testing.T) {
	req := makeRequest("sendRawTransaction")
	origErr := common.NewErrEndpointClientSideException(errors.New("simulation failed"))

	_, outErr := HandleUpstreamPostForward(context.Background(), nil, nil, req, nil, origErr, false)
	require.Error(t, outErr)
	assert.False(t, common.IsRetryableTowardNetwork(outErr),
		"sendRawTransaction errors must NOT be retryable toward network")
}

func TestHandleUpstreamPostForward_SendTransaction_CaseInsensitive(t *testing.T) {
	// Method names may arrive in mixed case
	for _, method := range []string{"sendTransaction", "SendTransaction", "SENDTRANSACTION"} {
		req := makeRequest(method)
		origErr := common.NewErrEndpointClientSideException(errors.New("bad"))
		_, outErr := HandleUpstreamPostForward(context.Background(), nil, nil, req, nil, origErr, false)
		require.Error(t, outErr)
		assert.False(t, common.IsRetryableTowardNetwork(outErr),
			"method %q: error must be non-retryable", method)
	}
}

func TestHandleUpstreamPostForward_OtherMethods_RetryableUnchanged(t *testing.T) {
	for _, method := range []string{"getSlot", "getBalance", "getBlock", "getLatestBlockhash"} {
		req := makeRequest(method)
		// ErrEndpointMissingData is retryable toward network by default
		origErr := common.NewErrEndpointMissingData(fmt.Errorf("not found"), nil)
		_, outErr := HandleUpstreamPostForward(context.Background(), nil, nil, req, nil, origErr, false)
		require.Error(t, outErr)
		assert.True(t, common.IsRetryableTowardNetwork(outErr),
			"method %q: error should remain retryable", method)
	}
}

func TestHandleUpstreamPostForward_SendTransaction_NonRetryableErrorPreserved(t *testing.T) {
	// If the error is already non-retryable, it must stay non-retryable
	req := makeRequest("sendTransaction")
	origErr := common.NewErrEndpointClientSideException(
		errors.New("bad tx"),
	).WithRetryableTowardNetwork(false)

	_, outErr := HandleUpstreamPostForward(context.Background(), nil, nil, req, nil, origErr, false)
	require.Error(t, outErr)
	assert.False(t, common.IsRetryableTowardNetwork(outErr))
}

func TestHandleUpstreamPostForward_SendTransaction_PlainErrorWrapped(t *testing.T) {
	// A plain Go error (not a RetryableError) should be wrapped and marked non-retryable
	req := makeRequest("sendTransaction")
	origErr := errors.New("connection refused")

	_, outErr := HandleUpstreamPostForward(context.Background(), nil, nil, req, nil, origErr, false)
	require.Error(t, outErr)
	assert.False(t, common.IsRetryableTowardNetwork(outErr),
		"plain errors on sendTransaction should be wrapped and marked non-retryable")
}

// ── JsonRpcErrorExtractor ─────────────────────────────────────────────────────

func TestJsonRpcErrorExtractor_NodeUnhealthy(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32005, "Node is unhealthy"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "solana node unhealthy")
	assert.Contains(t, err.Error(), "Node is unhealthy")
	// -32005 is a server-side exception (triggers failover)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException))
}

func TestJsonRpcErrorExtractor_BlockNotAvailable(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32004, "Block not available"), nil)
	require.Error(t, err)
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
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"generic JSON-RPC error should be ErrEndpointClientSideException")
}

func TestJsonRpcErrorExtractor_NoError(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrOK(), nil)
	assert.NoError(t, err)
}

func TestJsonRpcErrorExtractor_NilJrr(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, nil, nil)
	assert.NoError(t, err)
}

func TestJsonRpcErrorExtractor_ZeroErrorCode_NotExtracted(t *testing.T) {
	// Code == 0 with no message should not be extracted
	extractor := NewJsonRpcErrorExtractor()
	jr := &common.JsonRpcResponse{
		Error: common.NewErrJsonRpcExceptionExternal(0, "", ""),
	}
	err := extractor.Extract(fakeResp(200), nil, jr, nil)
	assert.NoError(t, err, "code 0 should not be extracted as an error")
}

// ── Gap 5: NormalizeHttpJsonRpc is a safe no-op ───────────────────────────────

func TestNormalizeHttpJsonRpc_NoOp(t *testing.T) {
	req := makeRequest("getSlot")
	jrq, err := req.JsonRpcRequest(context.Background())
	require.NoError(t, err)
	assert.NotPanics(t, func() {
		NormalizeHttpJsonRpc(context.Background(), req, jrq)
	})
}
