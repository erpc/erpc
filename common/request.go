package common

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/rs/zerolog"
)

const RequestContextKey ContextKey = "request"

type RequestDirectives struct {
	// Instruct the proxy to retry if response from the upstream appears to be empty
	// indicating a missing data or non-synced data (empty array for logs, null for block, null for tx receipt, etc).
	// This value is "true" by default which means more requests for such cases will be sent.
	// You can override this directive via Headers if you expect results to be empty and fine with eventual consistency (i.e. receiving empty results intermittently).
	RetryEmpty bool `json:"retryEmpty"`

	// Instruct the proxy to retry if response from the upstream appears to be a pending tx,
	// for example when blockNumber/blockHash are still null.
	// This value is "true" by default, to give a chance to receive the full TX.
	// If you intentionally require to get pending TX data immediately without waiting,
	// you can set this value to "false" via Headers.
	RetryPending bool `json:"retryPending"`

	// Instruct the proxy to skip cache reads for example to force freshness,
	// or override some cache corruption.
	SkipCacheRead bool `json:"skipCacheRead"`

	// Instruct the proxy to forward the request to a specific upstream(s) only.
	// Value can use "*" star char as a wildcard to target multiple upstreams.
	// For example "alchemy" or "my-own-*", etc.
	UseUpstream string `json:"useUpstream"`
}

type NormalizedRequest struct {
	sync.RWMutex

	network  Network
	cacheDal CacheDAL
	body     []byte

	method         string
	directives     *RequestDirectives
	jsonRpcRequest *JsonRpcRequest

	lastValidResponse atomic.Pointer[NormalizedResponse]
	lastUpstream      atomic.Value
	evmBlockRef       atomic.Value
	evmBlockNumber    atomic.Value
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

func (r *NormalizedRequest) ID() interface{} {
	if r == nil {
		return nil
	}

	if jrr, _ := r.JsonRpcRequest(); jrr != nil {
		return jrr.ID
	}

	return nil
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

func (r *NormalizedRequest) SetCacheDal(cacheDal CacheDAL) {
	r.cacheDal = cacheDal
}

func (r *NormalizedRequest) ApplyDirectiveDefaults(directiveDefaults *DirectiveDefaultsConfig) {
	if directiveDefaults == nil {
		return
	}
	r.Lock()
	defer r.Unlock()

	if r.directives == nil {
		r.directives = &RequestDirectives{}
	}

	if directiveDefaults.RetryEmpty != nil {
		r.directives.RetryEmpty = *directiveDefaults.RetryEmpty
	}
	if directiveDefaults.RetryPending != nil {
		r.directives.RetryPending = *directiveDefaults.RetryPending
	}
	if directiveDefaults.SkipCacheRead != nil {
		r.directives.SkipCacheRead = *directiveDefaults.SkipCacheRead
	}
	if directiveDefaults.UseUpstream != nil {
		r.directives.UseUpstream = *directiveDefaults.UseUpstream
	}
}

func (r *NormalizedRequest) ApplyDirectivesFromHttp(headers http.Header, queryArgs url.Values) {
	r.Lock()
	defer r.Unlock()

	if r.directives == nil {
		r.directives = &RequestDirectives{}
	}

	r.directives.RetryEmpty = headers.Get("X-ERPC-Retry-Empty") != "false"
	r.directives.RetryPending = headers.Get("X-ERPC-Retry-Pending") != "false"
	r.directives.SkipCacheRead = headers.Get("X-ERPC-Skip-Cache-Read") == "true"
	r.directives.UseUpstream = headers.Get("X-ERPC-Use-Upstream")

	if useUpstream := queryArgs.Get("use-upstream"); useUpstream != "" {
		r.directives.UseUpstream = strings.TrimSpace(useUpstream)
	}

	if retryEmpty := queryArgs.Get("retry-empty"); retryEmpty != "" {
		r.directives.RetryEmpty = strings.ToLower(strings.TrimSpace(retryEmpty)) != "false"
	}

	if retryPending := queryArgs.Get("retry-pending"); retryPending != "" {
		r.directives.RetryPending = strings.ToLower(strings.TrimSpace(retryPending)) != "false"
	}

	if skipCacheRead := queryArgs.Get("skip-cache-read"); skipCacheRead != "" {
		r.directives.SkipCacheRead = strings.ToLower(strings.TrimSpace(skipCacheRead)) != "false"
	}
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
	if r == nil {
		return "", nil
	}

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
			e.RawJSON("body", r.body)
		}
	}
}

// EvmBlockRefAndNumber extracts any possible block reference and number from the request and from response if it exists.
// This method is more 'complete' than NormalizedResponse.EvmBlockRefAndNumber() because we might not even have a response
// in certain situations (for example, when we are using failsafe retries).
func (r *NormalizedRequest) EvmBlockRefAndNumber() (string, int64, error) {
	if r == nil {
		return "", 0, nil
	}

	var blockNumber int64
	var blockRef string

	// Try to load from local cache
	if bn := r.evmBlockNumber.Load(); bn != nil {
		blockNumber = bn.(int64)
	}
	if br := r.evmBlockRef.Load(); br != nil {
		blockRef = br.(string)
	}

	if blockRef != "" && blockNumber != 0 {
		return blockRef, blockNumber, nil
	}

	// Try to load from JSON-RPC request
	rpcReq, err := r.JsonRpcRequest()
	if err != nil {
		return blockRef, blockNumber, err
	}

	br, bn, err := ExtractEvmBlockReferenceFromRequest(r.cacheDal, rpcReq)
	if br != "" {
		blockRef = br
	}
	if bn > 0 {
		blockNumber = bn
	}
	if err != nil {
		return blockRef, blockNumber, err
	}

	// Try to load from last valid response
	if blockRef == "" || blockNumber == 0 {
		lvr := r.lastValidResponse.Load()
		if lvr != nil {
			br, bn, err = lvr.EvmBlockRefAndNumber()
			if br != "" && (blockRef == "" || blockRef == "*") {
				// The condition (blockRef == ""|"*") makes sure if request already has a ref we won't override it from response.
				// For example eth_getBlockByNumber(latest) will have a "latest" ref, so it'll be cached under "latest" ref,
				// and we don't want it to be stored as the actual blockHash returned in the response, so that we can have a cache hit.
				// In case of "*" since it means any block, we can still augment it from response ref, because during cache.Get()
				// we'll be using reverse index (i.e. ignoring ref), but after reorg invalidation is added a specific block ref is useful.
				//
				// TODO An ideal version stores the data for all eth_getBlockByNumber(latest) and eth_getBlockByNumber(blockNumber),
				// and eth_getBlockByNumber(blockHash) where blockNumber/blockHash are the actual values returned in the response.
				// So that if user gets the latest block, then cache is populated for when they provide that specific block as well.
				// When implementing that feature remember that CacheHash() must be calculated separately for each number/hash combo.
				blockRef = br
			}
			if bn > 0 {
				blockNumber = bn
			}
			if err != nil {
				return blockRef, blockNumber, err
			}
		}
	}

	// Store to local cache
	if blockNumber > 0 {
		r.evmBlockNumber.Store(blockNumber)
	}
	if blockRef != "" {
		r.evmBlockRef.Store(blockRef)
	}

	return blockRef, blockNumber, nil
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
