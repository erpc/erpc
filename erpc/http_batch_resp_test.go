package erpc

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func TestBatchResponseWriter_WriteTo_BodyBackedResponse(t *testing.T) {
	t.Parallel()

	resp := common.NewNormalizedResponse().
		WithBody(io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)))

	bw := NewBatchResponseWriter([]interface{}{resp})
	var buf bytes.Buffer

	_, err := bw.WriteTo(&buf)
	require.NoError(t, err)
	require.JSONEq(t, `[{"jsonrpc":"2.0","id":1,"result":{"ok":true}}]`, buf.String())
}

func TestBatchResponseWriter_WriteTo_EmptyNormalizedResponseEmitsJsonRpcError(t *testing.T) {
	t.Parallel()

	resp := common.NewNormalizedResponse()
	bw := NewBatchResponseWriter([]interface{}{resp})
	var buf bytes.Buffer

	_, err := bw.WriteTo(&buf)
	require.NoError(t, err)
	require.JSONEq(t, `[{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"no body available to parse JsonRpcResponse","data":""}}]`, buf.String())
}
