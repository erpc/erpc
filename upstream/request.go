package upstream

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/evm"
	"github.com/rs/zerolog"
)

var _ common.NormalizedRequest = &NormalizedRequest{}

type NormalizedRequest struct {
	Attempt int

	network        common.Network
	body           []byte
	directives     *common.RequestDirectives
	jsonRpcRequest *common.JsonRpcRequest

	respMu            sync.Mutex
	lastValidResponse *NormalizedResponse
	lastUpstream      common.Upstream
}

type UniqueRequestKey struct {
	Method string
	Params string
}

func NewNormalizedRequest(body []byte) *NormalizedRequest {
	return &NormalizedRequest{
		body: body,
		directives: &common.RequestDirectives{
			RetryEmpty: true,
		},
	}
}

func (r *NormalizedRequest) SetLastUpstream(upstream common.Upstream) *NormalizedRequest {
	r.lastUpstream = upstream
	return r
}

func (r *NormalizedRequest) LastUpstream() common.Upstream {
	if r == nil {
		return nil
	}
	return r.lastUpstream
}

func (r *NormalizedRequest) SetLastValidResponse(response *NormalizedResponse) {
	r.respMu.Lock()
	defer r.respMu.Unlock()
	r.lastValidResponse = response
}

func (r *NormalizedRequest) LastValidResponse() common.NormalizedResponse {
	r.respMu.Lock()
	defer r.respMu.Unlock()
	return r.lastValidResponse
}

func (r *NormalizedRequest) Network() common.Network {
	if r == nil {
		return nil
	}
	return r.network
}

func (r *NormalizedRequest) SetNetwork(network common.Network) {
	r.network = network
}

func (r *NormalizedRequest) ApplyDirectivesFromHttpHeaders(headers http.Header) {
	drc := &common.RequestDirectives{
		RetryEmpty: headers.Get("x-erpc-retry-empty") != "false",
	}
	r.directives = drc
}

func (r *NormalizedRequest) Directives() *common.RequestDirectives {
	if r == nil {
		return nil
	}

	return r.directives
}

// Extract and prepare the request for forwarding.
func (r *NormalizedRequest) JsonRpcRequest() (*common.JsonRpcRequest, error) {
	if r == nil {
		return nil, nil
	}

	if r.jsonRpcRequest != nil {
		return r.jsonRpcRequest, nil
	}

	rpcReq := new(common.JsonRpcRequest)
	if err := json.Unmarshal(r.body, rpcReq); err != nil {
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

	r.jsonRpcRequest = rpcReq

	return rpcReq, nil
}

func (r *NormalizedRequest) Method() (string, error) {
	rpcReq, err := r.JsonRpcRequest()
	if err != nil {
		return "", err
	}

	return rpcReq.Method, nil
}

func (r *NormalizedRequest) MarshalZerologObject(e *zerolog.Event) {
	e.Str("body", string(r.body))
}

func (r *NormalizedRequest) EvmBlockNumber() (uint64, error) {
	rpcReq, err := r.JsonRpcRequest()
	if err != nil {
		return 0, err
	}

	_, bn, err := evm.ExtractBlockReference(rpcReq)
	if err != nil {
		return 0, err
	}

	return bn, nil
}

func (r *NormalizedRequest) MarshalJSON() ([]byte, error) {
	if r.body != nil {
		return r.body, nil
	}

	if r.jsonRpcRequest != nil {
		return json.Marshal(r.jsonRpcRequest)
	}

	if m, _ := r.Method(); m != "" {
		return json.Marshal(map[string]interface{}{
			"method": m,
		})
	}

	return nil, nil
}

func (r *NormalizedRequest) CacheHash() (string, error) {
	rq, _ := r.JsonRpcRequest()
	if rq != nil {
		return rq.CacheHash()
	}

	return "", fmt.Errorf("request is not valid to generate cache hash")
}
