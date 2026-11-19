package evm

import (
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// A -32602 means two different things. When the node's JSON decoder rejects
// the request text itself, every node gives the same answer, so a retry only
// multiplies load. Other -32602 answers can depend on the node or on time, so
// they keep their retry.

// classifyInvalidParams runs the normalizer over a -32602 reply.
func classifyInvalidParams(t *testing.T, message string, data interface{}) error {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`))
	nr := common.NewNormalizedResponse().WithRequest(req)
	jrErr := common.NewErrJsonRpcExceptionExternal(-32602, message, "")
	if data != nil {
		jrErr.Data = data
	}
	jr := common.MustNewJsonRpcResponse(1, nil, jrErr)
	return ExtractJsonRpcError(&http.Response{StatusCode: 200, Header: http.Header{}}, nr, jr, nil)
}

func TestExtractJsonRpcError_ADecodeRejectionIsNotRetried(t *testing.T) {
	t.Parallel()

	// reth answers these with message "Invalid params" and the serde_json
	// error as the "data" string.
	cases := []struct {
		name string
		data string
	}{
		{"odd-length hex (reth)", `invalid value: string "0x70a082310", expected a valid hex string at line 1 column 72`},
		{"wrong-length address (reth)", `invalid length 19, expected a 20 byte address at line 1 column 50`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyInvalidParams(t, "Invalid params", tc.data)
			require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException), "got %v", err)
			require.False(t, common.IsRetryableTowardNetwork(err),
				"every node decodes the same bytes the same way; a retry cannot help")
		})
	}
}

func TestExtractJsonRpcError_AnyOtherInvalidParamsKeepsItsRetry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		message string
		data    interface{}
	}{
		{"bare invalid params", "Invalid params", nil},
		// reth answers eth_feeHistory for the block past its head this way.
		// The block arrives a moment later, so the request can succeed.
		{"request beyond head block (reth eth_feeHistory)", "request beyond head block: requested 21000001, head 21000000", nil},
		{"range limit worded as params", "Invalid params", "block range exceeds the node's limit of 1000"},
		{"position without a decoder phrase", "Invalid params", "something at line 1 column 3"},
		// These read like decoder errors, but they depend on the node's
		// version or vendor, so another node can serve the request.
		{"version skew: unknown field", "Invalid params", "unknown field `authorizationList`, expected one of `from`, `to` at line 1 column 120"},
		{"version skew: unknown variant", "Invalid params", "unknown variant `0x4`, expected one of `0x0`, `0x1`, `0x2` at line 1 column 88"},
		{"missing field", "Invalid params", "missing field `to` at line 1 column 30"},
		{"wrong type", "Invalid params", "invalid type: integer `5`, expected a string at line 1 column 9"},
		// The rule reads "data" only, only as a string, from start to end.
		{"decoder phrase in the message only", `invalid value: string "0x1", expected a valid hex string at line 1 column 72`, nil},
		{"decoder phrase inside a data object", "Invalid params", map[string]interface{}{"reason": `invalid value: string "0x1", expected a valid hex string at line 1 column 72`}},
		{"decoder phrase not at the start of data", "Invalid params", `upstream said: invalid value: string "0x1" at line 1 column 72`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyInvalidParams(t, tc.message, tc.data)
			require.True(t, common.IsRetryableTowardNetwork(err),
				"another node, or the same node later, may answer differently, got %v", err)
		})
	}
}
