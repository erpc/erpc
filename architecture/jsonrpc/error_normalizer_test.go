package jsonrpc

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtract_SpecCodes_AreClientSideNonRetryable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code int
		msg  string
	}{
		{-32700, "Parse error"},
		{-32600, "Invalid Request"},
		{-32601, "Method not found"},
		{-32602, "Invalid params"},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			t.Parallel()
			err := extract(t, tc.code, tc.msg, 200)
			require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException), "got %T: %v", err, err)
			assert.False(t, common.IsRetryableTowardNetwork(err), "a malformed request gets the same verdict everywhere")
			assert.Equal(t, common.JsonRpcErrorNumber(tc.code), wireCodeOf(t, err), "the upstream's code reaches the client")
		})
	}
}

func TestExtract_AppErrorUnder200_IsServerSideWithDataPreserved(t *testing.T) {
	t.Parallel()
	jrErr := common.NewErrJsonRpcExceptionExternal(20, "Contract not found", "")
	jrErr.Data = map[string]interface{}{"contract": "0xabc"}
	err := extractWith(t, jrErr, 200)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException), "got %T: %v", err, err)
	assert.True(t, common.IsRetryableTowardNetwork(err), "the class is retryable; the replay guard decides on dispatch")
	assert.Equal(t, common.JsonRpcErrorNumber(20), wireCodeOf(t, err))
	assert.Equal(t, map[string]interface{}{"contract": "0xabc"}, dataOf(t, err))
	var sse *common.ErrEndpointServerSideException
	require.True(t, errors.As(err, &sse))
	assert.Equal(t, 200, sse.ErrorStatusCode(), "an application rejection under 200 stays 200")
}

func TestExtract_InternalError_IsServerSide(t *testing.T) {
	t.Parallel()
	err := extract(t, -32603, "Internal error", 500)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException), "got %T: %v", err, err)
	assert.Equal(t, common.JsonRpcErrorNumber(-32603), wireCodeOf(t, err))
}

func TestExtract_HttpStatusOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		status   int
		wantCode common.ErrorCode
	}{
		{"429", 429, common.ErrCodeEndpointCapacityExceeded},
		{"401", 401, common.ErrCodeEndpointUnauthorized},
		{"403", 403, common.ErrCodeEndpointUnauthorized},
		{"503", 503, common.ErrCodeEndpointServerSideException},
		{"400", 400, common.ErrCodeEndpointClientSideException},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := NewJsonRpcErrorExtractor()
			r := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			err := e.Extract(r, nil, common.MustNewJsonRpcResponse(1, nil, nil), newJsonRpcStub())
			require.True(t, common.HasErrorCode(err, tc.wantCode), "HTTP %d: got %T: %v", tc.status, err, err)
		})
	}
}

func TestExtract_UnparseableBodyUnderFailingStatus_ClassifiesByStatus(t *testing.T) {
	t.Parallel()
	synthesized := common.NewErrJsonRpcExceptionExternal(int(common.JsonRpcErrorParseException), "cannot parse json-rpc response", "")
	err := extractWith(t, synthesized, 503)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointServerSideException), "got %T: %v", err, err)
}

func TestExtract_NonJsonRpcUpstream_NoOps(t *testing.T) {
	t.Parallel()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: 500, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32603, "boom", ""))
	assert.NoError(t, e.Extract(r, nil, jr, &stubUpstream{id: "evm", typ: common.UpstreamTypeEvm}))
	assert.NoError(t, e.Extract(r, nil, jr, &stubUpstream{id: "svm", typ: common.UpstreamTypeSvm}))
	assert.NoError(t, e.Extract(r, nil, jr, nil))
}

func extract(t *testing.T, code int, msg string, status int) error {
	t.Helper()
	return extractWith(t, common.NewErrJsonRpcExceptionExternal(code, msg, ""), status)
}

func extractWith(t *testing.T, jrErr *common.ErrJsonRpcExceptionExternal, status int) error {
	t.Helper()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: status, Header: http.Header{}}
	return e.Extract(r, nil, common.MustNewJsonRpcResponse(1, nil, jrErr), newJsonRpcStub())
}

func wireCodeOf(t *testing.T, err error) common.JsonRpcErrorNumber {
	t.Helper()
	var jre *common.ErrJsonRpcExceptionInternal
	require.True(t, errors.As(err, &jre), "expected ErrJsonRpcExceptionInternal in chain, got %T: %v", err, err)
	return jre.NormalizedCode()
}

func dataOf(t *testing.T, err error) interface{} {
	t.Helper()
	var jre *common.ErrJsonRpcExceptionInternal
	require.True(t, errors.As(err, &jre), "expected ErrJsonRpcExceptionInternal in chain, got %T: %v", err, err)
	return jre.Details["data"]
}

func newJsonRpcStub() common.Upstream {
	return &stubUpstream{id: "jsonrpc-stub", typ: common.UpstreamTypeJsonRpc}
}

// stubUpstream satisfies the full common.Upstream interface. The extractor only
// reads Config().Type; the rest of the methods are no-ops.
type stubUpstream struct {
	id  string
	typ common.UpstreamType
}

func (s *stubUpstream) Id() string           { return s.id }
func (s *stubUpstream) VendorName() string   { return "" }
func (s *stubUpstream) NetworkId() string    { return "jsonrpc:starknet" }
func (s *stubUpstream) NetworkLabel() string { return "" }
func (s *stubUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: s.id, Type: s.typ}
}
func (s *stubUpstream) Logger() *zerolog.Logger { l := zerolog.Nop(); return &l }
func (s *stubUpstream) Vendor() common.Vendor   { return nil }
func (s *stubUpstream) Tracker() common.HealthTracker {
	return nil
}
func (s *stubUpstream) Forward(_ context.Context, _ *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (s *stubUpstream) ShouldHandleMethod(string) (bool, error) { return true, nil }
func (s *stubUpstream) Cordon(string, string)                   {}
func (s *stubUpstream) Uncordon(string, string)                 {}
func (s *stubUpstream) IgnoreMethod(string)                     {}
