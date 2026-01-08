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

// Test that HttpJsonRpcErrorResponse.MarshalZerologObject produces correct JSON structure
func TestHttpJsonRpcErrorResponse_MarshalZerologObject(t *testing.T) {
	// Temporarily enable logging for this test
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
				Error: map[string]interface{}{
					"code":    -32603,
					"message": "network not found",
					"data": map[string]interface{}{
						"code": "ErrNetworkNotFound",
					},
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

// Test that addResponseToLog correctly routes different response types
func TestAddResponseToLog(t *testing.T) {
	// Temporarily enable logging for this test
	origLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	defer zerolog.SetGlobalLevel(origLevel)

	tests := []struct {
		name         string
		resp         interface{}
		wantContains string
	}{
		{
			name: "HttpJsonRpcErrorResponse uses Object marshaler",
			resp: &HttpJsonRpcErrorResponse{
				Jsonrpc: "2.0",
				Id:      123,
				Error: map[string]interface{}{
					"code":    -32603,
					"message": "test error",
				},
			},
			wantContains: `"jsonrpc":"2.0"`,
		},
		{
			name: "BaseError pointer uses Object marshaler",
			resp: &common.BaseError{
				Code:    "ErrTest",
				Message: "test error message",
			},
			wantContains: `"code":"ErrTest"`,
		},
		{
			name:         "unknown type falls back to Interface",
			resp:         map[string]string{"foo": "bar"},
			wantContains: `"foo":"bar"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := zerolog.New(&buf)

			event := logger.Info()
			addResponseToLog(event, tt.resp).Msg("test")

			assert.Contains(t, buf.String(), tt.wantContains)
		})
	}
}

// Test that error field in HttpJsonRpcErrorResponse is properly serialized
func TestHttpJsonRpcErrorResponse_ErrorFieldSerialization(t *testing.T) {
	// Temporarily enable logging for this test
	origLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	defer zerolog.SetGlobalLevel(origLevel)

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	resp := &HttpJsonRpcErrorResponse{
		Jsonrpc: "2.0",
		Id:      42,
		Error: map[string]interface{}{
			"code":    -32601,
			"message": "method not found",
			"data": map[string]interface{}{
				"code":    "ErrUpstreamsExhausted",
				"message": "all upstream attempts failed",
				"details": map[string]interface{}{
					"attempts": 3,
					"retries":  2,
				},
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
