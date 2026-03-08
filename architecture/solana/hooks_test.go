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

// ── JsonRpcErrorExtractor — new error classifications ────────────────────────

func TestJsonRpcErrorExtractor_HTTP429_CapacityExceeded(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(429), nil, nil, nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded),
		"HTTP 429 should map to ErrEndpointCapacityExceeded")
}

func TestJsonRpcErrorExtractor_HTTP401_Unauthorized(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(401), nil, nil, nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized),
		"HTTP 401 should map to ErrEndpointUnauthorized")
}

func TestJsonRpcErrorExtractor_HTTP403_Unauthorized(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(403), nil, nil, nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized),
		"HTTP 403 should map to ErrEndpointUnauthorized")
}

func TestJsonRpcErrorExtractor_Minus32000_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	// -32000 with a generic server-side message (not simulation/blockhash/rate-limit)
	// should be ServerSideException so the proxy fails over to the next upstream.
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32000, "Server is overloaded, try again later"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"-32000 with generic server error should be ServerSideException (triggers failover)")
}

func TestJsonRpcErrorExtractor_Minus32012_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32012, "Long-term storage is temporarily unavailable"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"-32012 (storage unavailable) should be server-side (triggers failover)")
}

func TestJsonRpcErrorExtractor_Minus32009_MissingData(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32009, "Requested account not found"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
		"-32009 (account not found) should be MissingData so another upstream is tried")
}

func TestJsonRpcErrorExtractor_Minus32010_MissingData(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32010, "Requested program not found"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
		"-32010 (program not found) should be MissingData so another upstream is tried")
}

func TestJsonRpcErrorExtractor_Minus32011_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	// Node is behind the requested minContextSlot — failover to a faster upstream.
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32011, "Minimum context slot has not been reached"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"-32011 (node behind requested slot) should be server-side to trigger failover")
}

func TestJsonRpcErrorExtractor_AccountSecondaryIndexExclusion_Unsupported(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	// QuickNode returns -32010 for "excluded from account secondary indexes"
	// (same code it uses for "program not found"). The message check inside the
	// -32010 case overrides the generic MissingData classification.
	msg := "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA excluded from account secondary indexes; this RPC method unavailable for key"
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32010, msg), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
		"account secondary index exclusion should be Unsupported so another upstream is tried")
}

func TestJsonRpcErrorExtractor_Minus32016_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	// QuickNode uses -32016 for "Minimum context slot has not been reached" (non-standard).
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32016, "Minimum context slot has not been reached"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"-32016 (QuickNode minContextSlot) should be server-side to trigger failover")
}

func TestJsonRpcErrorExtractor_RateLimitMessage_CapacityExceeded(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	// Some providers return -32000 with a rate-limit message in the body.
	// Our text-based check catches this even if the code is otherwise handled.
	// Here we use a code that would fall to the text-check path.
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32603, "Too many requests, please try again later"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded),
		"rate-limit message should map to ErrEndpointCapacityExceeded")
}

// ── New codes: HTTP 5xx, -32006, -32008, -32015, -32601 ──────────────────────

func TestJsonRpcErrorExtractor_HTTP500_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(500), nil, nil, nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"HTTP 500 should be server-side to trigger failover")
}

func TestJsonRpcErrorExtractor_HTTP502_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(502), nil, nil, nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"HTTP 502 should be server-side to trigger failover")
}

func TestJsonRpcErrorExtractor_HTTP503_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(503), nil, nil, nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"HTTP 503 should be server-side to trigger failover")
}

func TestJsonRpcErrorExtractor_HTTP504_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(504), nil, nil, nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"HTTP 504 should be server-side to trigger failover")
}

func TestJsonRpcErrorExtractor_Minus32006_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32006, "Node is behind"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"-32006 (node behind / not yet implemented) should trigger failover")
}

func TestJsonRpcErrorExtractor_Minus32008_MissingData(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32008, "No snapshot"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
		"-32008 (no snapshot) should be missing data so another upstream is tried")
}

func TestJsonRpcErrorExtractor_Minus32015_MissingData(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32015, "Block status not yet available"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData),
		"-32015 (block status not yet available) should be missing data so another upstream is tried")
}

func TestJsonRpcErrorExtractor_Minus32601_Unsupported(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil, fakeJrrWithError(-32601, "Method not found"), nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
		"-32601 (method not found) should be unsupported so another upstream is tried")
}

// ── Phase 2: -32000 message-based disambiguation ─────────────────────────────

func TestJsonRpcErrorExtractor_Minus32000_SimulationFailed_ClientSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil,
		fakeJrrWithError(-32000, "Transaction simulation failed: Error processing Instruction 0: custom program error: 0x1"),
		nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"-32000 with 'Transaction simulation failed' must be ClientSideException — all upstreams will return the same error for a bad tx")
	assert.False(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"-32000 simulation failure must NOT trigger failover")
}

func TestJsonRpcErrorExtractor_Minus32000_BlockhashNotFound_ClientSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil,
		fakeJrrWithError(-32000, "Transaction simulation failed: Blockhash not found"),
		nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"-32000 with 'Blockhash not found' must be ClientSideException")
}

func TestJsonRpcErrorExtractor_Minus32000_Overloaded_ServerSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil,
		fakeJrrWithError(-32000, "Server is overloaded, try again later"),
		nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException),
		"-32000 with generic server error should still be ServerSideException (failover)")
}

func TestJsonRpcErrorExtractor_Minus32000_RateLimit_CapacityExceeded(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil,
		fakeJrrWithError(-32000, "Connection rate limits exceeded"),
		nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded),
		"-32000 with rate limit message should be CapacityExceeded")
}

// ── Phase 2: explicit -32002 / -32003 / -32013 client-side classification ────

func TestJsonRpcErrorExtractor_Minus32002_TransactionSimulationFailed_ClientSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil,
		fakeJrrWithError(-32002, "Transaction simulation failed: insufficient funds"),
		nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"-32002 (tx simulation failed) must be ClientSideException — bad tx is deterministic")
}

func TestJsonRpcErrorExtractor_Minus32003_SignatureVerificationFailed_ClientSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil,
		fakeJrrWithError(-32003, "Transaction signature verification failure"),
		nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"-32003 (signature verification) must be ClientSideException — bad signature cannot be fixed by retrying")
}

func TestJsonRpcErrorExtractor_Minus32013_SignatureLengthMismatch_ClientSide(t *testing.T) {
	extractor := NewJsonRpcErrorExtractor()
	err := extractor.Extract(fakeResp(200), nil,
		fakeJrrWithError(-32013, "Transaction signature length mismatch"),
		nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"-32013 (signature length mismatch) must be ClientSideException")
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
