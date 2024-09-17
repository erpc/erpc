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
	// Instruct the proxy to retry if response from the upstream appears to be empty
	// indicating a missing data or non-synced data (empty array for logs, null for block, null for tx receipt, etc).
	// This value is "true" by default which means more requests for such cases will be sent.
	// You can override this directive via Headers if you expect results to be empty and fine with eventual consistency (i.e. receiving empty results intermittently).
	RetryEmpty bool

	// Instruct the proxy to retry if response from the upstream appears to be a pending tx,
	// for example when blockNumber/blockHash are still null.
	// This value is "true" by default, to give a chance to receive the full TX.
	// If you intentionally require to get pending TX data immediately without waiting,
	// you can set this value to "false" via Headers.
	RetryPending bool

	// Instruct the proxy to skip cache reads for example to force freshness,
	// or override some cache corruption.
	SkipCacheRead bool

	// Instruct the proxy to forward the request to a specific upstream(s) only.
	// Value can use "*" star char as a wildcard to target multiple upstreams.
	// For example "alchemy" or "my-own-*", etc.
	UseUpstream string
}

type NormalizedRequest struct {
	sync.RWMutex

	Attempt int

	network Network
	body    []byte

	uid            string
	method         string
	directives     *RequestDirectives
	jsonRpcRequest *JsonRpcRequest

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
	if r == nil {
		return r
	}
	r.Lock()
	defer r.Unlock()
	r.lastUpstream = upstream
	return r
}

func (r *NormalizedRequest) LastUpstream() Upstream {
	if r == nil {
		return nil
	}
	r.Lock()
	defer r.Unlock()
	return r.lastUpstream
}

func (r *NormalizedRequest) SetLastValidResponse(response *NormalizedResponse) {
	if r == nil {
		return
	}
	r.Lock()
	defer r.Unlock()
	r.lastValidResponse = response
}

func (r *NormalizedRequest) LastValidResponse() *NormalizedResponse {
	if r == nil {
		return nil
	}
	r.RLock()
	defer r.RUnlock()
	return r.lastValidResponse
}

func (r *NormalizedRequest) Network() Network {
	if r == nil {
		return nil
	}
	return r.network
}

func (r *NormalizedRequest) Id() string {
	if r == nil {
		return ""
	}

	if r.uid != "" {
		return r.uid
	}

	r.RLock()
	if r.jsonRpcRequest != nil {
		defer r.RUnlock()
		if id, ok := r.jsonRpcRequest.ID.(string); ok {
			r.uid = id
			return id
		} else if id, ok := r.jsonRpcRequest.ID.(float64); ok {
			r.uid = fmt.Sprintf("%d", int64(id))
			return r.uid
		} else {
			r.uid = fmt.Sprintf("%v", r.jsonRpcRequest.ID)
			return r.uid
		}
	}
	r.RUnlock()

	r.Lock()
	defer r.Unlock()

	if r.uid != "" {
		return r.uid
	}

	if len(r.body) > 0 {
		idnode, err := sonic.Get(r.body, "id")
		if err == nil {
			ids, err := idnode.String()
			if err == nil {
				idf, err := idnode.Float64()
				if err != nil {
					idn, err := idnode.Int64()
					if err != nil {
						r.uid = fmt.Sprintf("%d", idn)
						return r.uid
					}
				} else {
					r.uid = fmt.Sprintf("%d", int64(idf))
					return r.uid
				}
			} else {
				r.uid = ids
			}
			return r.uid
		}
	}

	return ""
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

func (r *NormalizedRequest) ApplyDirectivesFromHttp(
	headers *fasthttp.RequestHeader,
	queryArgs *fasthttp.Args,
) {
	drc := &RequestDirectives{
		RetryEmpty:    string(headers.Peek("X-ERPC-Retry-Empty")) != "false",
		RetryPending:  string(headers.Peek("X-ERPC-Retry-Pending")) != "false",
		SkipCacheRead: string(headers.Peek("X-ERPC-Skip-Cache-Read")) == "true",
		UseUpstream:   string(headers.Peek("X-ERPC-Use-Upstream")),
	}

	if useUpstream := string(queryArgs.Peek("use-upstream")); useUpstream != "" {
		drc.UseUpstream = useUpstream
	}

	if retryEmpty := string(queryArgs.Peek("retry-empty")); retryEmpty != "" {
		drc.RetryEmpty = retryEmpty != "false"
	}

	if retryPending := string(queryArgs.Peek("retry-pending")); retryPending != "" {
		drc.RetryPending = retryPending != "false"
	}

	if skipCacheRead := string(queryArgs.Peek("skip-cache-read")); skipCacheRead != "" {
		drc.SkipCacheRead = skipCacheRead != "false"
	}

	r.directives = drc
}

func (r *NormalizedRequest) SkipCacheRead() bool {
	if r == nil {
		return false
	}
	if r.directives == nil {
		return false
	}
	return r.directives.SkipCacheRead
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
	r.RLock()
	if r.jsonRpcRequest != nil {
		r.RUnlock()
		return r.jsonRpcRequest, nil
	}
	r.RUnlock()

	r.Lock()
	defer r.Unlock()

	// Double-check in case another goroutine initialized it
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
		rpcReq.ID = rand.Intn(math.MaxInt32) // #nosec G404
	}

	r.jsonRpcRequest = rpcReq

	return rpcReq, nil
}

func (r *NormalizedRequest) Method() (string, error) {
	if r.method != "" {
		return r.method, nil
	}

	if r.jsonRpcRequest != nil {
		r.method = r.jsonRpcRequest.Method
		return r.jsonRpcRequest.Method, nil
	}

	if len(r.body) > 0 {
		method, err := sonic.Get(r.body, "method")
		if err != nil {
			r.method = "n/a"
			return r.method, err
		}
		m, err := method.String()
		r.method = m
		return m, err
	}

	return "", NewErrJsonRpcRequestUnresolvableMethod(r.body)
}

func (r *NormalizedRequest) Body() []byte {
	return r.body
}

func (r *NormalizedRequest) MarshalZerologObject(e *zerolog.Event) {
	if r != nil {
		if r.jsonRpcRequest != nil {
			e.Object("jsonRpc", r.jsonRpcRequest)
		} else if r.body != nil {
			e.Str("body", string(r.body))
		}
	}
}

func (r *NormalizedRequest) EvmBlockNumber() (int64, error) {
	if r == nil {
		return 0, nil
	}

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
	rq, err := r.JsonRpcRequest()
	if err != nil {
		return "", err
	}

	if rq != nil {
		return rq.CacheHash()
	}

	return "", fmt.Errorf("request is not valid to generate cache hash")
}
