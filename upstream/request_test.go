package upstream

import (
	"testing"

	"github.com/erpc/erpc/common"
)

func TestNormalizedRequest_BodyCannotBeDecoded(t *testing.T) {
	normReq := common.NewNormalizedRequest([]byte(`{"method":"test","params":"invalid"}`))

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
