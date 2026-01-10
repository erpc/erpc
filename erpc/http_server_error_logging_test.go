package erpc

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHttpJsonRpcErrorResponse_MarshalZerologObject(t *testing.T) {
	origLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	defer zerolog.SetGlobalLevel(origLevel)

	tests := []struct {
		name     string
		resp     *HttpJsonRpcErrorResponse
		wantKeys []string
	}{
		{
			name: "full error response",
			resp: &HttpJsonRpcErrorResponse{
				Jsonrpc: "2.0",
				Id:      1,
				Error: &common.ErrJsonRpcExceptionExternal{
					Code:    -32603,
					Message: "network not found",
					Data:    "ErrNetworkNotFound",
				},
			},
			wantKeys: []string{"jsonrpc", "id", "error"},
		},
		{
			name: "nil error field",
			resp: &HttpJsonRpcErrorResponse{
				Jsonrpc: "2.0",
				Id:      2,
				Error:   nil,
			},
			wantKeys: []string{"jsonrpc", "id"},
		},
		{
			name:     "nil response",
			resp:     nil,
			wantKeys: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := zerolog.New(&buf)

			logger.Info().Object("response", tt.resp).Msg("test")

			if tt.resp == nil {
				// nil response should produce empty object
				assert.Contains(t, buf.String(), `"response":{}`)
				return
			}

			var logEntry map[string]interface{}
			require.NoError(t, json.Unmarshal(buf.Bytes(), &logEntry))

			resp, ok := logEntry["response"].(map[string]interface{})
			require.True(t, ok, "response should be an object")

			for _, key := range tt.wantKeys {
				assert.Contains(t, resp, key, "response should contain key: %s", key)
			}

			// Verify specific values
			if tt.resp.Jsonrpc != "" {
				assert.Equal(t, tt.resp.Jsonrpc, resp["jsonrpc"])
			}
		})
	}
}

func TestLogObjectMarshaler_NativeChaining(t *testing.T) {
	origLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	defer zerolog.SetGlobalLevel(origLevel)

	tests := []struct {
		name         string
		resp         zerolog.LogObjectMarshaler
		wantContains string
	}{
		{
			name: "HttpJsonRpcErrorResponse with native Object()",
			resp: &HttpJsonRpcErrorResponse{
				Jsonrpc: "2.0",
				Id:      123,
				Error: &common.ErrJsonRpcExceptionExternal{
					Code:    -32603,
					Message: "test error",
				},
			},
			wantContains: `"jsonrpc":"2.0"`,
		},
		{
			name: "BaseError with native Object()",
			resp: &common.BaseError{
				Code:    "ErrTest",
				Message: "test error message",
			},
			wantContains: `"code":"ErrTest"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := zerolog.New(&buf)

			// Test native zerolog chaining - this is what we use in processErrorBody
			logger.Info().Object("response", tt.resp).Msg("test")

			assert.Contains(t, buf.String(), tt.wantContains)
		})
	}
}

func TestHttpJsonRpcErrorResponse_ErrorFieldSerialization(t *testing.T) {
	origLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	defer zerolog.SetGlobalLevel(origLevel)

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	resp := &HttpJsonRpcErrorResponse{
		Jsonrpc: "2.0",
		Id:      42,
		Error: &common.ErrJsonRpcExceptionExternal{
			Code:    -32601,
			Message: "method not found",
			Data: map[string]interface{}{
				"code":    "ErrUpstreamsExhausted",
				"message": "all upstream attempts failed",
			},
		},
	}

	logger.Info().Object("response", resp).Msg("test")

	var logEntry map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &logEntry))

	respObj := logEntry["response"].(map[string]interface{})
	errObj := respObj["error"].(map[string]interface{})

	// Verify the error structure is preserved
	assert.Equal(t, float64(-32601), errObj["code"])
	assert.Equal(t, "method not found", errObj["message"])

	// Verify nested data is preserved
	dataObj := errObj["data"].(map[string]interface{})
	assert.Equal(t, "ErrUpstreamsExhausted", dataObj["code"])
}
