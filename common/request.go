package common

import (
	"fmt"
	"math"
	"math/rand"
	"sync"

	"github.com/bytedance/sonic"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

type RequestDirectives struct {
	RetryEmpty bool
}

type NormalizedRequest struct {
	Attempt int

	network        Network
	body           []byte
	directives     *RequestDirectives
	jsonRpcRequest *JsonRpcRequest

	respMu            sync.Mutex
	lastValidResponse *NormalizedResponse
	lastUpstream      Upstream
}

type UniqueRequestKey struct {
	Method string
	Params string
}

func NewNormalizedRequest(body []byte) *NormalizedRequest {
	return &NormalizedRequest{
		body: body,
		directives: &RequestDirectives{
			RetryEmpty: true,
		},
	}
}

func (r *NormalizedRequest) SetLastUpstream(upstream Upstream) *NormalizedRequest {
	r.lastUpstream = upstream
	return r
}

func (r *NormalizedRequest) LastUpstream() Upstream {
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

func (r *NormalizedRequest) LastValidResponse() *NormalizedResponse {
	r.respMu.Lock()
	defer r.respMu.Unlock()
	return r.lastValidResponse
}

func (r *NormalizedRequest) Network() Network {
	if r == nil {
		return nil
	}
	return r.network
}

func (r *NormalizedRequest) NetworkId() string {
	if r == nil || r.network == nil {
		// For certain requests such as internal eth_chainId requests, network might not be available yet.
		return "n/a"
	}
	return r.network.Id()
}

func (r *NormalizedRequest) SetNetwork(network Network) {
	r.network = network
}

func (r *NormalizedRequest) ApplyDirectivesFromHttpHeaders(headers *fasthttp.RequestHeader) {
	drc := &RequestDirectives{
		RetryEmpty: string(headers.Peek("X-ERPC-Retry-Empty")) != "false",
	}
	r.directives = drc
}

func (r *NormalizedRequest) Directives() *RequestDirectives {
	if r == nil {
		return nil
	}

	return r.directives
}

// Extract and prepare the request for forwarding.
func (r *NormalizedRequest) JsonRpcRequest() (*JsonRpcRequest, error) {
	if r == nil {
		return nil, nil
	}

	if r.jsonRpcRequest != nil {
		return r.jsonRpcRequest, nil
	}

	rpcReq := new(JsonRpcRequest)
	if err := sonic.Unmarshal(r.body, rpcReq); err != nil {
		return nil, NewErrJsonRpcRequestUnmarshal(err)
	}

	method := rpcReq.Method
	if method == "" {
		return nil, NewErrJsonRpcRequestUnresolvableMethod(rpcReq)
	}

	if rpcReq.JSONRPC == "" {
		rpcReq.JSONRPC = "2.0"
	}

	if rpcReq.ID == nil {
		rpcReq.ID = float64(rand.Intn(math.MaxInt32))
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

func (r *NormalizedRequest) Body() []byte {
	return r.body
}

func (r *NormalizedRequest) MarshalZerologObject(e *zerolog.Event) {
	e.Str("body", string(r.body))
}

func (r *NormalizedRequest) EvmBlockNumber() (int64, error) {
	rpcReq, err := r.JsonRpcRequest()
	if err != nil {
		return 0, err
	}

	_, bn, err := ExtractEvmBlockReferenceFromRequest(rpcReq)
	if err != nil {
		return 0, err
	} else if bn > 0 {
		return bn, nil
	}

	if r.lastValidResponse == nil {
		return 0, nil
	}

	bn, err = r.lastValidResponse.EvmBlockNumber()
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
		return sonic.Marshal(r.jsonRpcRequest)
	}

	if m, _ := r.Method(); m != "" {
		return sonic.Marshal(map[string]interface{}{
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
