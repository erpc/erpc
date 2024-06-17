package upstream

import (
	"testing"
)

func TestNormalizedRequest_BodyCannotBeDecoded(t *testing.T) {
	normReq := NewNormalizedRequest("123", []byte(`{"method": "test", "params": "invalid"}`))

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
