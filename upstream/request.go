package upstream

import (
	"encoding/json"
	"math"
	"math/rand"
	"net/http"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/evm"
	"github.com/rs/zerolog"
)

type NormalizedRequest struct {
	network        common.Network
	upstream       common.Upstream
	body           []byte
	directives     *common.RequestDirectives
	jsonRpcRequest *common.JsonRpcRequest
}

func NewNormalizedRequest(body []byte) *NormalizedRequest {
	return &NormalizedRequest{
		body: body,
		directives: &common.RequestDirectives{
			RetryEmpty: true,
		},
	}
}

func (r *NormalizedRequest) Clone() common.NormalizedRequest {
	return &NormalizedRequest{
		network:        r.network,
		upstream:       r.upstream,
		body:           r.body,
		directives:     r.directives,
		jsonRpcRequest: r.jsonRpcRequest,
	}
}

func (r *NormalizedRequest) Network() common.Network {
	if r == nil {
		return nil
	}
	return r.network
}

func (r *NormalizedRequest) Upstream() common.Upstream {
	if r == nil {
		return nil
	}
	return r.upstream
}

func (r *NormalizedRequest) WithNetwork(network common.Network) *NormalizedRequest {
	r.network = network
	return r
}

func (r *NormalizedRequest) WithUpstream(upstream common.Upstream) *NormalizedRequest {
	r.upstream = upstream
	return r
}

func (r *NormalizedRequest) ApplyDirectivesFromHttpHeaders(headers http.Header) *NormalizedRequest {
	drc := &common.RequestDirectives{
		RetryEmpty: headers.Get("x-erpc-retry-empty") != "false",
	}
	r.directives = drc
	return r
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
