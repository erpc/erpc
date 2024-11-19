package erpc

import (
	"fmt"
	"io"

	"github.com/erpc/erpc/common"
)

// BatchResponseWriter efficiently writes multiple responses without buffering
type BatchResponseWriter struct {
	responses []interface{}
}

func NewBatchResponseWriter(responses []interface{}) *BatchResponseWriter {
	return &BatchResponseWriter{
		responses: responses,
	}
}

func (b *BatchResponseWriter) WriteTo(w io.Writer) (n int64, err error) {
	// Write opening bracket
	nn, err := w.Write([]byte{'['})
	if err != nil {
		return int64(nn), err
	}
	n += int64(nn)

	for i, resp := range b.responses {
		if i > 0 {
			// Write comma separator
			nn, err = w.Write([]byte{','})
			if err != nil {
				return n + int64(nn), err
			}
			n += int64(nn)
		}

		var written int64
		switch v := resp.(type) {
		case *common.NormalizedResponse:
			written, err = v.WriteTo(w)
		case *HttpJsonRpcErrorResponse:
			written, err = writeJsonRpcError(w, v)
		case error:
			// TODO we should determine the format when we have others besides json-rpc
			errResp := &HttpJsonRpcErrorResponse{
				Jsonrpc: "2.0",
				Id:      nil, // This is unexpected error where we can't determine the request ID
				Error: &common.ErrJsonRpcExceptionExternal{
					Code:    int(common.JsonRpcErrorServerSideException), // -32603
					Message: v.Error(),
				},
			}
			written, err = writeJsonRpcError(w, errResp)
		default:
			// Fallback to regular JSON encoding for unknown types
			var buf []byte
			buf, err = common.SonicCfg.Marshal(v)
			if err != nil {
				return n, err
			}
			var wn int
			wn, err = w.Write(buf)
			written = int64(wn)
		}
		if err != nil {
			return n + written, err
		}
		if written == 0 {
			return n, fmt.Errorf("no bytes written for response %d error: %w", i, err)
		}
		n += written
	}

	// Write closing bracket
	nn, err = w.Write([]byte{']'})
	return n + int64(nn), err
}

// writeJsonRpcError efficiently writes JSON-RPC error response without buffering
func writeJsonRpcError(w io.Writer, resp *HttpJsonRpcErrorResponse) (n int64, err error) {
	parts := [][]byte{
		[]byte(`{"jsonrpc":"`),
		[]byte(resp.Jsonrpc),
		[]byte(`","id":`),
	}

	// Write initial parts
	for _, part := range parts {
		nn, err := w.Write(part)
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	}

	// Write ID
	idBytes, err := common.SonicCfg.Marshal(resp.Id)
	if err != nil {
		return n, err
	}
	nn, err := w.Write(idBytes)
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)

	// Write error part
	nn, err = w.Write([]byte(`,"error":`))
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)

	// Write error object
	errorBytes, err := common.SonicCfg.Marshal(resp.Error)
	if err != nil {
		return n, err
	}
	nn, err = w.Write(errorBytes)
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)

	// Write closing brace
	nn, err = w.Write([]byte{'}'})
	return n + int64(nn), err
}
