package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJsonRpcRequest_BodyCannotBeDecoded(t *testing.T) {
	// Create a new HTTP request with an invalid JSON body
	reqBody := strings.NewReader(`{"method": "test", "params": "invalid"}`)
	req := httptest.NewRequest(http.MethodPost, "/", reqBody)

	// Create a NormalizedRequest instance
	normReq := NewNormalizedRequest(req)

	// Call the JsonRpcRequest method to parse and normalize the request
	jsonRpcReq, err := normReq.JsonRpcRequest()

	// Check the result
	if jsonRpcReq != nil {
		t.Errorf("Expected nil, got %v", jsonRpcReq)
	}

	// Check the error
	if err == nil {
		t.Error("Expected an error, got nil")
	}
}
