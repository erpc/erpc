package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeJsonRpcRequest_RequestBodyCannotBeDecoded(t *testing.T) {
	n := NewNormalizer()

	// Create a new request with an invalid JSON body
	reqBody := strings.NewReader(`{"method": "test", "params": "invalid"}`)
	req := httptest.NewRequest(http.MethodPost, "/", reqBody)

	// Call the function under test
	result, err := n.NormalizeJsonRpcRequest(req)

	// Check the result
	if result != nil {
		t.Errorf("Expected nil, got %v", result)
	}

	// Check the error
	if err == nil {
		t.Error("Expected an error, got nil")
	}
}
