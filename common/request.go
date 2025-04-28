package common

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const (
	CompositeTypeNone               = "none"
	CompositeTypeLogsSplitOnError   = "logs-split-on-error"
	CompositeTypeLogsSplitProactive = "logs-split-proactive"
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

	// Instruct the proxy to bypass method exclusion checks.
	ByPassMethodExclusion bool `json:"byPassMethodExclusion"`
}

func (d *RequestDirectives) Clone() *RequestDirectives {
	return &RequestDirectives{
		RetryEmpty:            d.RetryEmpty,
		RetryPending:          d.RetryPending,
		SkipCacheRead:         d.SkipCacheRead,
		UseUpstream:           d.UseUpstream,
		ByPassMethodExclusion: d.ByPassMethodExclusion,
	}
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

	compositeType   atomic.Value // Type of composite request (e.g., "logs-split")
	parentRequestId atomic.Value // ID of the parent request (for sub-requests)
}

func NewNormalizedRequest(body []byte) *NormalizedRequest {
	nr := &NormalizedRequest{
		body: body,
		directives: &RequestDirectives{
			RetryEmpty: true,
		},
	}
	nr.compositeType.Store(CompositeTypeNone)
	return nr
}

func NewNormalizedRequestFromJsonRpcRequest(jsonRpcRequest *JsonRpcRequest) *NormalizedRequest {
	nr := &NormalizedRequest{
		jsonRpcRequest: jsonRpcRequest,
		directives: &RequestDirectives{
			RetryEmpty: true,
		},
	}
	nr.compositeType.Store(CompositeTypeNone)
	return nr
}

func (r *NormalizedRequest) SetLastUpstream(upstream Upstream) *NormalizedRequest {
	if r == nil || upstream == nil {
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
	if r == nil || response == nil {
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

func (r *NormalizedRequest) CacheDal() CacheDAL {
	if r == nil {
		return nil
	}
	return r.cacheDal
}

func (r *NormalizedRequest) SetDirectives(directives *RequestDirectives) {
	if r == nil {
		return
	}
	r.directives = directives
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
	r.directives.RetryPending = headers.Get("X-ERPC-Retry-Pending") == "true"
	r.directives.SkipCacheRead = headers.Get("X-ERPC-Skip-Cache-Read") == "true"
	r.directives.UseUpstream = headers.Get("X-ERPC-Use-Upstream")

	if useUpstream := queryArgs.Get("use-upstream"); useUpstream != "" {
		r.directives.UseUpstream = strings.TrimSpace(useUpstream)
	}

	if retryEmpty := queryArgs.Get("retry-empty"); retryEmpty != "" {
		r.directives.RetryEmpty = strings.ToLower(strings.TrimSpace(retryEmpty)) != "false"
	}

	if retryPending := queryArgs.Get("retry-pending"); retryPending != "" {
		r.directives.RetryPending = strings.ToLower(strings.TrimSpace(retryPending)) == "true"
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

func (r *NormalizedRequest) LockWithTrace(ctx context.Context) {
	_, span := StartDetailSpan(ctx, "Request.Lock")
	defer span.End()
	r.Lock()
}

func (r *NormalizedRequest) RLockWithTrace(ctx context.Context) {
	_, span := StartDetailSpan(ctx, "Request.RLock")
	defer span.End()
	r.RLock()
}

// Extract and prepare the request for forwarding.
func (r *NormalizedRequest) JsonRpcRequest(ctx ...context.Context) (*JsonRpcRequest, error) {
	if len(ctx) > 0 {
		_, span := StartDetailSpan(ctx[0], "Request.ResolveJsonRpc")
		defer span.End()
	}

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
		return nil, NewErrJsonRpcRequestUnmarshal(err, r.body)
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
			return "", NewErrJsonRpcRequestUnmarshal(err, r.body)
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
	if r == nil {
		return
	}

	if lu := r.lastUpstream.Load(); lu != nil {
		if lup := lu.(Upstream); lup != nil {
			if lup.Config() != nil {
				e.Str("lastUpstream", lup.Config().Id)
			} else {
				e.Str("lastUpstream", fmt.Sprintf("%p", lup))
			}
		} else {
			e.Str("lastUpstream", "nil")
		}
	} else {
		e.Str("lastUpstream", "nil")
	}

	if r.network != nil {
		e.Str("network", r.network.Id())
	}

	if r.jsonRpcRequest != nil {
		e.Object("jsonRpc", r.jsonRpcRequest)
	} else if r.body != nil {
		if IsSemiValidJson(r.body) {
			e.RawJSON("body", r.body)
		} else {
			e.Str("body", util.B2Str(r.body))
		}
	}
}

// TODO Move evm specific data to RequestMetadata struct so we can have multiple architectures besides evm
func (r *NormalizedRequest) EvmBlockRef() interface{} {
	if r == nil {
		return nil
	}
	return r.evmBlockRef.Load()
}

func (r *NormalizedRequest) SetEvmBlockRef(blockRef interface{}) {
	if r == nil || blockRef == nil {
		return
	}
	r.evmBlockRef.Store(blockRef)
}

func (r *NormalizedRequest) EvmBlockNumber() interface{} {
	if r == nil {
		return nil
	}
	return r.evmBlockNumber.Load()
}

func (r *NormalizedRequest) SetEvmBlockNumber(blockNumber interface{}) {
	if r == nil || blockNumber == nil {
		return
	}
	r.evmBlockNumber.Store(blockNumber)
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

func (r *NormalizedRequest) CacheHash(ctx ...context.Context) (string, error) {
	rq, err := r.JsonRpcRequest(ctx...)
	if err != nil {
		return "", err
	}

	if rq != nil {
		return rq.CacheHash(ctx...)
	}

	return "", fmt.Errorf("request is not valid to generate cache hash")
}

func (r *NormalizedRequest) Validate() error {
	if r == nil {
		return NewErrInvalidRequest(fmt.Errorf("request is nil"))
	}

	if r.body == nil && r.jsonRpcRequest == nil {
		return NewErrInvalidRequest(fmt.Errorf("request body is nil"))
	}

	method, err := r.Method()
	if err != nil {
		return NewErrInvalidRequest(err)
	}

	if method == "" {
		return NewErrInvalidRequest(fmt.Errorf("method is required"))
	}

	return nil
}

// IsCompositeRequest returns whether this request is a top-level composite request
func (r *NormalizedRequest) IsCompositeRequest() bool {
	if r == nil {
		return false
	}
	return r.CompositeType() != CompositeTypeNone
}

func (r *NormalizedRequest) CompositeType() string {
	if r == nil {
		return ""
	}
	if ct := r.compositeType.Load(); ct != nil {
		return ct.(string)
	}
	return ""
}

func (r *NormalizedRequest) SetCompositeType(compositeType string) {
	if r == nil || compositeType == "" {
		return
	}
	r.compositeType.Store(compositeType)
}

func (r *NormalizedRequest) ParentRequestId() interface{} {
	if r == nil {
		return nil
	}
	return r.parentRequestId.Load()
}

func (r *NormalizedRequest) SetParentRequestId(parentId interface{}) {
	if r == nil || parentId == nil {
		return
	}
	r.parentRequestId.Store(parentId)
}
