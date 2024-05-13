package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type NormalizedRequest struct {
	rawRequest *http.Request

	jsonRpcRequest *JsonRpcRequest
}

func NewNormalizedRequest(rawRequest *http.Request) *NormalizedRequest {
	return &NormalizedRequest{
		rawRequest: rawRequest,
	}
}

// Extract and prepare the request for forwarding.
func (n *NormalizedRequest) JsonRpcRequest() (*JsonRpcRequest, error) {
	if n.jsonRpcRequest != nil {
		return n.jsonRpcRequest, nil
	}

	body, err := io.ReadAll(n.rawRequest.Body)

	if err != nil {
		return nil, err
	}

	rpcReq := new(JsonRpcRequest)
	if err := json.Unmarshal(body, rpcReq); err != nil {
		return nil, err
	}

	method := rpcReq.Method
	if method == "" {
		return nil, fmt.Errorf("missing method in request %v", rpcReq)
	}

	if rpcReq.JSONRPC == "" {
		rpcReq.JSONRPC = "2.0"
	}

	n.jsonRpcRequest = rpcReq

	return rpcReq, nil
}
