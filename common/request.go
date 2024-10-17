package common

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"
)

const RequestContextKey ContextKey = "request"

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

	network Network
	body    []byte

	uid            atomic.Value
	method         string
	directives     *RequestDirectives
	jsonRpcRequest *JsonRpcRequest

	lastValidResponse atomic.Pointer[NormalizedResponse]
	lastUpstream      atomic.Value
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
	r.lastUpstream.Store(upstream)
	return r
}

func (r *NormalizedRequest) LastUpstream() Upstream {
	if r == nil {
		return nil
	}
	if lu := r.lastUpstream.Load(); lu != nil {
		return lu.(Upstream)
	}
	return nil
}

func (r *NormalizedRequest) SetLastValidResponse(response *NormalizedResponse) {
	if r == nil {
		return
	}
	r.lastValidResponse.Store(response)
}

func (r *NormalizedRequest) LastValidResponse() *NormalizedResponse {
	if r == nil {
		return nil
	}
	return r.lastValidResponse.Load()
}

func (r *NormalizedRequest) Network() Network {
	if r == nil {
		return nil
	}
	return r.network
}

func (r *NormalizedRequest) Id() int64 {
	if r == nil {
		return 0
	}

	if r.jsonRpcRequest != nil {
		return r.jsonRpcRequest.ID
	}

	if len(r.body) > 0 {
		idnode, err := sonic.Get(r.body, "id")
		if err == nil {
			ids, err := idnode.String()
			if err != nil {
				idf, err := idnode.Float64()
				if err != nil {
					idn, err := idnode.Int64()
					if err != nil {
						r.uid.Store(idn)
						return idn
					}
				} else {
					uid := int64(idf)
					r.uid.Store(uid)
					return uid
				}
			} else {
				uid, err := strconv.ParseInt(ids, 0, 64)
				if err != nil {
					uid = 0
				}
				r.uid.Store(uid)
				return uid
			}
		}
	}

	return 0
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
		RetryEmpty:    util.Mem2Str(headers.Peek("X-ERPC-Retry-Empty")) != "false",
		RetryPending:  util.Mem2Str(headers.Peek("X-ERPC-Retry-Pending")) != "false",
		SkipCacheRead: util.Mem2Str(headers.Peek("X-ERPC-Skip-Cache-Read")) == "true",
		UseUpstream:   util.Mem2Str(headers.Peek("X-ERPC-Use-Upstream")),
	}

	if useUpstream := util.Mem2Str(queryArgs.Peek("use-upstream")); useUpstream != "" {
		drc.UseUpstream = useUpstream
	}

	if retryEmpty := util.Mem2Str(queryArgs.Peek("retry-empty")); retryEmpty != "" {
		drc.RetryEmpty = retryEmpty != "false"
	}

	if retryPending := util.Mem2Str(queryArgs.Peek("retry-pending")); retryPending != "" {
		drc.RetryPending = retryPending != "false"
	}

	if skipCacheRead := util.Mem2Str(queryArgs.Peek("skip-cache-read")); skipCacheRead != "" {
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
	if err := SonicCfg.Unmarshal(r.body, rpcReq); err != nil {
		return nil, NewErrJsonRpcRequestUnmarshal(err)
	}

	method := rpcReq.Method
	if method == "" {
		return nil, NewErrJsonRpcRequestUnresolvableMethod(rpcReq)
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
			return "", NewErrJsonRpcRequestUnmarshal(err)
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
			e.Str("body", util.Mem2Str(r.body))
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

	lvr := r.lastValidResponse.Load()
	if lvr == nil {
		return 0, nil
	}

	bn, err = lvr.EvmBlockNumber()
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
		return SonicCfg.Marshal(map[string]interface{}{
			"method": r.jsonRpcRequest.Method,
		})
	}

	if m, _ := r.Method(); m != "" {
		return SonicCfg.Marshal(map[string]interface{}{
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
