package upstream

import (
	"encoding/json"
	"math"
	"math/rand"
	"net/http"

	"github.com/flair-sdk/erpc/common"
	"github.com/rs/zerolog"
)

type NormalizedRequest struct {
	NetworkId string
	Upstream  *PreparedUpstream

	body []byte

	jsonRpcRequest *JsonRpcRequest

	DirectiveRetryEmpty bool
}

func NewNormalizedRequest(networkId string, body []byte) *NormalizedRequest {
	return &NormalizedRequest{
		NetworkId: networkId,
		body:      body,
	}
}

func (r *NormalizedRequest) ApplyDirectivesFromHttpHeaders(headers http.Header) error {
	r.DirectiveRetryEmpty = headers.Get("x-erpc-retry-empty") != "false"
	return nil
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

	if rpcReq.ID == nil {
		rpcReq.ID = rand.Intn(math.MaxInt32)
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

func (n *NormalizedRequest) MarshalZerologObject(e *zerolog.Event) {
	e.Str("body", string(n.body))
}
