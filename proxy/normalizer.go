package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/flair-sdk/erpc/upstream"
)

type Normalizer struct{}

func NewNormalizer() *Normalizer {
	return &Normalizer{}
}

// Extract and prepare the request for forwarding.
func (n *Normalizer) NormalizeJsonRpcRequest(r *http.Request) (*upstream.JsonRpcRequest, error) {
	body, err := io.ReadAll(r.Body)

	if err != nil {
		return nil, err
	}

	rpcReq := new(upstream.JsonRpcRequest)
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

	return rpcReq, nil
}
