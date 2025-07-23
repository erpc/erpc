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

const RequestContextKey ContextKey = "rq"
const UpstreamsContextKey ContextKey = "ups"

type RequestDirectives struct {
	// Instruct the proxy to retry if response from the upstream appears to be empty
	// indicating missing or non-synced data (empty array for logs, null for block, null for tx receipt, etc).
	// By default this value is "false" – meaning empty responses will NOT be retried unless explicitly
	// enabled via project/network directive defaults or incoming HTTP header / query parameter.
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
	UseUpstream string `json:"useUpstream,omitempty"`

	// Instruct the proxy to bypass method exclusion checks.
	ByPassMethodExclusion bool `json:"-"`
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
	jsonRpcRequest atomic.Pointer[JsonRpcRequest]

	// Upstream selection fields - protected by upstreamMutex
	upstreamMutex    sync.Mutex
	UpstreamIdx      uint32
	ErrorsByUpstream sync.Map
	EmptyResponses   sync.Map // TODO we can potentially remove this map entirely, only use is to report "wrong empty responses"

	// New fields for centralized upstream management
	upstreamList      []Upstream // Available upstreams for this request
	ConsumedUpstreams *sync.Map  // Tracks upstreams that provided valid responses

	lastValidResponse atomic.Pointer[NormalizedResponse]
	lastUpstream      atomic.Value
	evmBlockRef       atomic.Value
	evmBlockNumber    atomic.Value

	compositeType   atomic.Value // Type of composite request (e.g., "logs-split")
	parentRequestId atomic.Value // ID of the parent request (for sub-requests)

	finality atomic.Value // Cached finality state
}

func NewNormalizedRequest(body []byte) *NormalizedRequest {
	nr := &NormalizedRequest{
		body: body,
		directives: &RequestDirectives{
			RetryEmpty: false,
		},
	}
	nr.compositeType.Store(CompositeTypeNone)
	return nr
}

func NewNormalizedRequestFromJsonRpcRequest(jsonRpcRequest *JsonRpcRequest) *NormalizedRequest {
	nr := &NormalizedRequest{
		directives: &RequestDirectives{
			RetryEmpty: false,
		},
	}
	nr.jsonRpcRequest.Store(jsonRpcRequest)
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

func (r *NormalizedRequest) SetLastValidResponse(ctx context.Context, nrs *NormalizedResponse) bool {
	if r == nil || nrs == nil {
		return false
	}
	prevLV := r.lastValidResponse.Load()
	prevIsEmpty := prevLV == nil || prevLV.IsObjectNull(ctx) || prevLV.IsResultEmptyish(ctx)
	newIsEmpty := nrs.IsObjectNull(ctx) || nrs.IsResultEmptyish(ctx)
	if prevIsEmpty || !newIsEmpty {
		r.lastValidResponse.Store(nrs)
		if !nrs.IsObjectNull(ctx) {
			ups := nrs.Upstream()
			if ups != nil {
				r.SetLastUpstream(ups)
			}
		}
		return true
	}
	return false
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

	// Headers have precedence over directive defaults, but should only override when explicitly provided.
	if hv := headers.Get("X-ERPC-Retry-Empty"); hv != "" {
		r.directives.RetryEmpty = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get("X-ERPC-Retry-Pending"); hv != "" {
		r.directives.RetryPending = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get("X-ERPC-Skip-Cache-Read"); hv != "" {
		r.directives.SkipCacheRead = strings.ToLower(strings.TrimSpace(hv)) == "true"
	}
	if hv := headers.Get("X-ERPC-Use-Upstream"); hv != "" {
		r.directives.UseUpstream = hv
	}

	// Query parameters come after headers so they can still override when explicitly present in URL.
	if useUpstream := queryArgs.Get("use-upstream"); useUpstream != "" {
		r.directives.UseUpstream = strings.TrimSpace(useUpstream)
	}

	if retryEmpty := queryArgs.Get("retry-empty"); retryEmpty != "" {
		r.directives.RetryEmpty = strings.ToLower(strings.TrimSpace(retryEmpty)) == "true"
	}

	if retryPending := queryArgs.Get("retry-pending"); retryPending != "" {
		r.directives.RetryPending = strings.ToLower(strings.TrimSpace(retryPending)) == "true"
	}

	if skipCacheRead := queryArgs.Get("skip-cache-read"); skipCacheRead != "" {
		r.directives.SkipCacheRead = strings.ToLower(strings.TrimSpace(skipCacheRead)) == "true"
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
	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		return jrq, nil
	}

	rpcReq := new(JsonRpcRequest)
	if err := SonicCfg.Unmarshal(r.body, rpcReq); err != nil {
		return nil, NewErrJsonRpcRequestUnmarshal(err, r.body)
	}

	method := rpcReq.Method
	if method == "" {
		return nil, NewErrJsonRpcRequestUnresolvableMethod(rpcReq)
	}

	r.jsonRpcRequest.Store(rpcReq)

	return rpcReq, nil
}

func (r *NormalizedRequest) Method() (string, error) {
	if r == nil {
		return "", nil
	}

	if r.method != "" {
		return r.method, nil
	}

	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		r.method = jrq.Method
		return jrq.Method, nil
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
				e.Str("lastUpstream", lup.Id())
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
		e.Str("networkId", r.network.Id())
	}

	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		e.Object("jsonRpc", jrq)
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

	if jrq := r.jsonRpcRequest.Load(); jrq != nil {
		return SonicCfg.Marshal(map[string]interface{}{
			"method": jrq.Method,
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

	if r.body == nil && r.jsonRpcRequest.Load() == nil {
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

func (r *NormalizedRequest) Finality(ctx context.Context) DataFinalityState {
	if r == nil {
		return DataFinalityStateUnknown
	}

	// Check if we have a cached value
	if f := r.finality.Load(); f != nil {
		return f.(DataFinalityState)
	}

	// Calculate and cache the finality
	if r.network != nil {
		finality := r.network.GetFinality(ctx, r, nil)
		// Only cache if we got a definitive answer (not unknown)
		if finality != DataFinalityStateUnknown {
			r.finality.Store(finality)
		}
		return finality
	}

	return DataFinalityStateUnknown
}

func (r *NormalizedRequest) SetUpstreams(upstreams []Upstream) {
	if r == nil {
		return
	}
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()

	r.upstreamList = upstreams
	if r.ConsumedUpstreams == nil {
		r.ConsumedUpstreams = &sync.Map{}
	}
}

func (r *NormalizedRequest) NextUpstream() (Upstream, error) {
	if r == nil {
		return nil, fmt.Errorf("unexpected uninitialized request")
	}

	// Lock for 100% guaranteed sequential upstream selection
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()

	if len(r.upstreamList) == 0 {
		return nil, fmt.Errorf("no upstreams available for this request")
	}

	// Check if UseUpstream directive is set
	if r.directives != nil && r.directives.UseUpstream != "" {
		// Handle UseUpstream directive - find matching upstream
		for _, upstream := range r.upstreamList {
			match, err := WildcardMatch(r.directives.UseUpstream, upstream.Id())
			if err != nil {
				continue
			}
			if match {
				// Check if this upstream has already been consumed
				if _, loaded := r.ConsumedUpstreams.LoadOrStore(upstream, true); !loaded {
					return upstream, nil
				}
			}
		}
		// If no matching upstream found or all consumed, return error
		return nil, fmt.Errorf("no more non-consumed matching upstreams left")
	}

	upstreamCount := len(r.upstreamList)

	// Try all upstreams starting from current index with guaranteed sequential access
	for attempts := 0; attempts < upstreamCount; attempts++ {
		// Get current index and increment - this is now atomic within the mutex
		idx := r.UpstreamIdx % uint32(upstreamCount) // #nosec G115
		r.UpstreamIdx++                              // Guaranteed increment for next caller

		upstream := r.upstreamList[idx]

		// Skip if already consumed (gave valid response or consensus-valid error)
		if _, consumed := r.ConsumedUpstreams.Load(upstream); consumed {
			continue
		}

		// Skip if already responded with non-retryable error
		if prevErr, exists := r.ErrorsByUpstream.Load(upstream); exists {
			if pe, ok := prevErr.(error); ok && !IsRetryableTowardsUpstream(pe) {
				continue
			}
		}

		// Skip if already responded empty
		if _, isEmpty := r.EmptyResponses.Load(upstream); isEmpty {
			continue
		}

		// Try to reserve this upstream atomically
		// If LoadOrStore returns loaded=false, we successfully reserved it
		if _, loaded := r.ConsumedUpstreams.LoadOrStore(upstream, true); !loaded {
			// Successfully reserved this upstream
			return upstream, nil
		}
		// If we reach here, another goroutine reserved this upstream, continue to next
	}

	// All upstreams exhausted
	return nil, fmt.Errorf("no more good upstreams left")
}

func (r *NormalizedRequest) MarkUpstreamCompleted(ctx context.Context, upstream Upstream, resp *NormalizedResponse, err error) {
	r.upstreamMutex.Lock()
	defer r.upstreamMutex.Unlock()

	hasResponse := resp != nil && !resp.IsObjectNull(ctx)
	// We can re-use the same upstream on next rotation if there's no response or the error is retryable towards the upstream
	canReUse := !hasResponse || (err != nil && IsRetryableTowardsUpstream(err))

	if err != nil {
		r.ErrorsByUpstream.Store(upstream, err)
	} else if resp != nil && resp.IsResultEmptyish(ctx) {
		jr, err := resp.JsonRpcResponse(ctx)
		if jr == nil {
			r.ErrorsByUpstream.Store(upstream, NewErrEndpointMissingData(fmt.Errorf("upstream responded emptyish but cannot extract json-rpc response: %v", err), upstream))
		} else {
			r.ErrorsByUpstream.Store(upstream, NewErrEndpointMissingData(fmt.Errorf("upstream responded emptyish: %v", jr.Result), upstream))
		}
		r.EmptyResponses.Store(upstream, true)
	}

	if canReUse {
		r.ConsumedUpstreams.Delete(upstream)
	}
}
