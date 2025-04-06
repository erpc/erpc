package common

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type NormalizedResponse struct {
	sync.RWMutex

	request      *NormalizedRequest
	body         io.ReadCloser
	expectedSize int

	fromCache bool
	attempts  int
	retries   int
	hedges    int
	upstream  Upstream

	jsonRpcResponse atomic.Pointer[JsonRpcResponse]
	evmBlockNumber  atomic.Value
	evmBlockRef     atomic.Value
}

var _ ResponseMetadata = &NormalizedResponse{}

type ResponseMetadata interface {
	FromCache() bool
	Attempts() int
	Retries() int
	Hedges() int
	UpstreamId() string
	IsObjectNull(ctx ...context.Context) bool
}

func LookupResponseMetadata(err error) ResponseMetadata {
	if err == nil {
		return nil
	}

	if uer, ok := err.(ResponseMetadata); ok {
		return uer
	}

	if ser, ok := err.(StandardError); ok {
		if cs := ser.GetCause(); cs != nil {
			return LookupResponseMetadata(cs)
		}
	}

	return nil
}

func NewNormalizedResponse() *NormalizedResponse {
	return &NormalizedResponse{}
}

func (r *NormalizedResponse) LockWithTrace(ctx context.Context) {
	_, span := StartDetailSpan(ctx, "Response.Lock")
	defer span.End()
	r.Lock()
}

func (r *NormalizedResponse) RLockWithTrace(ctx context.Context) {
	_, span := StartDetailSpan(ctx, "Response.RLock")
	defer span.End()
	r.RLock()
}

func (r *NormalizedResponse) FromCache() bool {
	if r == nil {
		return false
	}
	return r.fromCache
}

func (r *NormalizedResponse) SetFromCache(fromCache bool) *NormalizedResponse {
	r.fromCache = fromCache
	return r
}

func (r *NormalizedResponse) EvmBlockRef() interface{} {
	if r == nil {
		return nil
	}
	return r.evmBlockRef.Load()
}

func (r *NormalizedResponse) SetEvmBlockRef(blockRef interface{}) {
	if r == nil || blockRef == nil {
		return
	}
	r.evmBlockRef.Store(blockRef)
}

func (r *NormalizedResponse) EvmBlockNumber() interface{} {
	if r == nil {
		return nil
	}
	return r.evmBlockNumber.Load()
}

func (r *NormalizedResponse) SetEvmBlockNumber(blockNumber interface{}) {
	if r == nil || blockNumber == nil {
		return
	}
	r.evmBlockNumber.Store(blockNumber)
}

func (r *NormalizedResponse) Attempts() int {
	if r == nil {
		return 0
	}
	return r.attempts
}

func (r *NormalizedResponse) SetAttempts(attempts int) *NormalizedResponse {
	r.attempts = attempts
	return r
}

func (r *NormalizedResponse) Retries() int {
	if r == nil {
		return 0
	}
	return r.retries
}

func (r *NormalizedResponse) SetRetries(retries int) *NormalizedResponse {
	r.retries = retries
	return r
}

func (r *NormalizedResponse) Hedges() int {
	if r == nil {
		return 0
	}
	return r.hedges
}

func (r *NormalizedResponse) SetHedges(hedges int) *NormalizedResponse {
	r.hedges = hedges
	return r
}

func (r *NormalizedResponse) Upstream() Upstream {
	if r == nil || r.upstream == nil {
		return nil
	}

	return r.upstream
}

func (r *NormalizedResponse) UpstreamId() string {
	if r == nil || r.upstream == nil {
		return ""
	}

	if r.upstream.Config() == nil {
		return ""
	}

	return r.upstream.Config().Id
}

func (r *NormalizedResponse) SetUpstream(upstream Upstream) *NormalizedResponse {
	r.upstream = upstream
	return r
}

func (r *NormalizedResponse) WithRequest(req *NormalizedRequest) *NormalizedResponse {
	r.request = req
	return r
}

func (r *NormalizedResponse) WithFromCache(fromCache bool) *NormalizedResponse {
	r.fromCache = fromCache
	return r
}

func (r *NormalizedResponse) JsonRpcResponse(ctx ...context.Context) (*JsonRpcResponse, error) {
	if len(ctx) > 0 {
		_, span := StartDetailSpan(ctx[0], "Response.ResolveJsonRpc")
		defer span.End()
	}

	if r == nil {
		return nil, nil
	}

	if jrr := r.jsonRpcResponse.Load(); jrr != nil {
		return jrr, nil
	}

	jrr := &JsonRpcResponse{}

	if r.body != nil {
		err := jrr.ParseFromStream(ctx, r.body, r.expectedSize)
		if err != nil {
			return nil, err
		}
		r.jsonRpcResponse.Store(jrr)
		return jrr, nil
	}

	return nil, nil
}

func (r *NormalizedResponse) WithBody(body io.ReadCloser) *NormalizedResponse {
	r.body = body
	return r
}

func (r *NormalizedResponse) WithExpectedSize(expectedSize int) *NormalizedResponse {
	r.expectedSize = expectedSize
	return r
}

func (r *NormalizedResponse) WithJsonRpcResponse(jrr *JsonRpcResponse) *NormalizedResponse {
	r.jsonRpcResponse.Store(jrr)
	return r
}

func (r *NormalizedResponse) Request() *NormalizedRequest {
	if r == nil {
		return nil
	}
	return r.request
}

func (r *NormalizedResponse) IsResultEmptyish(ctx ...context.Context) bool {
	jrr, err := r.JsonRpcResponse(ctx...)
	if err != nil {
		return false
	}

	if jrr != nil {
		return jrr.IsResultEmptyish(ctx...)
	}

	return true
}

func (r *NormalizedResponse) IsObjectNull(ctx ...context.Context) bool {
	if r == nil {
		return true
	}

	jrr, _ := r.JsonRpcResponse(ctx...)
	if jrr == nil {
		return true
	}

	if len(ctx) > 0 {
		_, span := StartDetailSpan(ctx[0], "Response.IsObjectNull")
		defer span.End()
	}

	jrr.resultMu.RLock()
	defer jrr.resultMu.RUnlock()

	jrr.errMu.RLock()
	defer jrr.errMu.RUnlock()

	if len(jrr.Result) == 0 && jrr.Error == nil && jrr.ID() == 0 {
		return true
	}

	return false
}

func (r *NormalizedResponse) MarshalJSON() ([]byte, error) {
	r.RLock()
	defer r.RUnlock()

	if jrr := r.jsonRpcResponse.Load(); jrr != nil {
		return SonicCfg.Marshal(jrr)
	}

	return nil, nil
}

func (r *NormalizedResponse) WriteTo(w io.Writer) (n int64, err error) {
	if r == nil {
		return 0, fmt.Errorf("unexpected nil response when calling NormalizedResponse.WriteTo")
	}

	if jrr := r.jsonRpcResponse.Load(); jrr != nil {
		return jrr.WriteTo(w)
	}

	return 0, fmt.Errorf("unexpected empty response when calling NormalizedResponse.WriteTo")
}

func (r *NormalizedResponse) Release() {
	if r == nil {
		return
	}

	r.Lock()
	defer r.Unlock()

	// If body is not closed yet, close it
	if r.body != nil {
		err := r.body.Close()
		if err != nil {
			log.Error().Err(err).Object("response", r).Msg("failed to close response body")
		}
		r.body = nil
	}
}

func (r *NormalizedResponse) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}

	jrr := r.jsonRpcResponse.Load()
	if jrr != nil {
		e.Interface("id", jrr.ID())
		if jrr.errBytes != nil && len(jrr.errBytes) > 0 {
			if IsSemiValidJson(jrr.errBytes) {
				e.RawJSON("error", jrr.errBytes)
			} else {
				e.Str("error", util.Mem2Str(jrr.errBytes))
			}
		}
		if len(jrr.Result) < 300*1024 {
			if IsSemiValidJson(jrr.Result) {
				e.RawJSON("result", jrr.Result)
			} else {
				e.Str("result", util.Mem2Str(jrr.Result))
			}
		} else {
			head := 150 * 1024
			tail := len(jrr.Result) - head
			if tail < head {
				head = tail
			}
			e.Str("resultHead", util.Mem2Str(jrr.Result[:head])).Str("resultTail", util.Mem2Str(jrr.Result[tail:]))
		}
	}
}

// CopyResponseForRequest creates a copy of the response for another request
// We use references for underlying Result and Error fields to save memory.
func CopyResponseForRequest(ctx context.Context, resp *NormalizedResponse, req *NormalizedRequest) (*NormalizedResponse, error) {
	req.RLockWithTrace(ctx)
	defer req.RUnlock()

	if resp == nil {
		return nil, nil
	}

	r := NewNormalizedResponse()
	r.WithRequest(req)
	r.SetUpstream(resp.Upstream())
	r.SetFromCache(resp.FromCache())
	r.SetAttempts(resp.Attempts())
	r.SetRetries(resp.Retries())
	r.SetHedges(resp.Hedges())
	r.SetEvmBlockRef(resp.EvmBlockRef())
	r.SetEvmBlockNumber(resp.EvmBlockNumber())

	if ejrr := resp.jsonRpcResponse.Load(); ejrr != nil {
		// We need to use request ID because response ID can be different for multiplexed requests
		// where we only sent 1 actual request to the upstream.
		jrr, err := ejrr.Clone()
		if err != nil {
			return nil, err
		}
		err = jrr.SetID(req.jsonRpcRequest.ID)
		if err != nil {
			return nil, err
		}
		r.WithJsonRpcResponse(jrr)
	}

	return r, nil
}
