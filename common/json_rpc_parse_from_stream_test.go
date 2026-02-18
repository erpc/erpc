package common

import (
	"bytes"
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJsonRpcResponse_ParseFromStream_LargeResultStaysStable(t *testing.T) {
	t.Parallel()

	payload := benchMakeLargeJsonRpcResponse(80 << 10)
	var rr bytes.Reader
	rr.Reset(payload)

	var resp JsonRpcResponse
	require.NoError(t, resp.ParseFromStream([]context.Context{context.Background()}, &rr, len(payload)))

	orig := append([]byte(nil), resp.GetResultBytes()...)
	require.NotEmpty(t, orig)

	// Exercise parser/buffer churn to catch accidental aliasing/lifetime issues.
	for i := 0; i < 200; i++ {
		var tmp JsonRpcResponse
		small := bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"result":"x"}`))
		require.NoError(t, tmp.ParseFromStream([]context.Context{context.Background()}, small, 0))
	}
	runtime.GC()

	require.Equal(t, orig, resp.GetResultBytes())
}
