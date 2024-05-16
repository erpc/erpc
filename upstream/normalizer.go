package upstream

import (
	"encoding/json"

	"github.com/flair-sdk/erpc/common"
)

type NormalizedRequest struct {
	body           []byte
	jsonRpcRequest *JsonRpcRequest
}

func NewNormalizedRequest(body []byte) *NormalizedRequest {
	return &NormalizedRequest{
		body: body,
	}
}

// Extract and prepare the request for forwarding.
func (n *NormalizedRequest) JsonRpcRequest() (*JsonRpcRequest, error) {
	if n.jsonRpcRequest != nil {
		return n.jsonRpcRequest, nil
	}

	rpcReq := new(JsonRpcRequest)
	if err := json.Unmarshal(n.body, rpcReq); err != nil {
		return nil, common.NewErrJsonRpcRequestUnmarshal(err)
	}

	method := rpcReq.Method
	if method == "" {
		return nil, common.NewErrJsonRpcRequestUnresolvableMethod(rpcReq)
	}

	if rpcReq.JSONRPC == "" {
		rpcReq.JSONRPC = "2.0"
	}

	n.jsonRpcRequest = rpcReq

	return rpcReq, nil
}

func (n *NormalizedRequest) Method() (string, error) {
	rpcReq, err := n.JsonRpcRequest()
	if err != nil {
		return "", err
	}

	return rpcReq.Method, nil
}
