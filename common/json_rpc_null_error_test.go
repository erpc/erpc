package common

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// A response that carries "error": null is a success. JSON-RPC 1.0 requires
// both members on every response, and some JSON-RPC 2.0 servers also send
// the null member.

func TestJsonRpcResponse_NullErrorMemberIsNotAnError(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		`{"jsonrpc":"2.0","result":"0x10","error":null,"id":1}`,
		`{"jsonrpc":"2.0","result":"0x10","error": null ,"id":1}`,
	} {
		r := &JsonRpcResponse{}
		require.NoError(t, r.ParseFromStream(nil, bytes.NewReader([]byte(body)), len(body)))
		require.Nil(t, r.Error, "%s: a null error member parsed as an error", body)
		require.Equal(t, `"0x10"`, r.GetResultString())
	}
}

// The batch path extracts the members by name and passes the raw bytes in.
// It must give the same answer as the streaming parser.
func TestJsonRpcResponse_NullErrorMemberFromBytesIsNotAnError(t *testing.T) {
	t.Parallel()

	r, err := NewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x10"`), []byte(`null`))
	require.NoError(t, err)
	require.Nil(t, r.Error, "a null error member parsed as an error")
}

// The negative control: a real error object still parses as an error.
func TestJsonRpcResponse_ErrorObjectNextToNullResultStillParses(t *testing.T) {
	t.Parallel()

	body := `{"jsonrpc":"2.0","result":null,"error":{"code":-32000,"message":"header not found"},"id":1}`
	r := &JsonRpcResponse{}
	require.NoError(t, r.ParseFromStream(nil, bytes.NewReader([]byte(body)), len(body)))
	require.NotNil(t, r.Error, "a real error object parsed as success")
	require.Equal(t, -32000, r.Error.Code)
	require.Equal(t, "header not found", r.Error.Message)
}
