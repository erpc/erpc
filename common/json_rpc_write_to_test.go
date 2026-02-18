package common

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJsonRpcResponse_WriteTo_EmptyIDBytesWritesNull(t *testing.T) {
	t.Parallel()

	jrr := &JsonRpcResponse{
		result: []byte(`{"ok":true}`),
	}

	var buf bytes.Buffer
	_, err := jrr.WriteTo(&buf)
	require.NoError(t, err)
	require.JSONEq(t, `{"jsonrpc":"2.0","id":null,"result":{"ok":true}}`, buf.String())
}
