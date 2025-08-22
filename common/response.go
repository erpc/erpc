package common

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type NormalizedResponse struct {
	sync.RWMutex

	request      *NormalizedRequest
	body         io.ReadCloser
	expectedSize int

	fromCache atomic.Bool
	attempts  atomic.Value
	retries   atomic.Value
	hedges    atomic.Value
	upstream  atomic.Value

	jsonRpcResponse atomic.Pointer[JsonRpcResponse]
	evmBlockNumber  atomic.Value
	evmBlockRef     atomic.Value
	finality        atomic.Value // Cached finality state

	// parseOnce ensures JsonRpcResponse is parsed only once
	parseOnce sync.Once
	parseErr  error

	duration time.Duration

	// pendingOps tracks background users (e.g., async cache writes).
	// Release will wait for all pending ops to finish before freeing buffers.
	pendingOps sync.WaitGroup

	// bodyOnce ensures the response body is closed exactly once
	bodyOnce sync.Once
	// bodyMu coordinates concurrent parsing and releasing of the body
	bodyMu sync.RWMutex
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
	return r.fromCache.Load()
}

func (r *NormalizedResponse) SetFromCache(fromCache bool) *NormalizedResponse {
	if r == nil {
		return r
	}
	r.fromCache.Store(fromCache)
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

func (r *NormalizedResponse) Finality(ctx context.Context) DataFinalityState {
	if r == nil {
		return DataFinalityStateUnknown
	}

	// Check if we have a cached value
	if f := r.finality.Load(); f != nil {
		return f.(DataFinalityState)
	}

	// Calculate and cache the finality
	if r.request != nil && r.request.network != nil {
		finality := r.request.network.GetFinality(ctx, r.request, r)
		// Only cache if we got a definitive answer (not unknown)
		if finality != DataFinalityStateUnknown {
			r.finality.Store(finality)
		}
		return finality
	}

	return DataFinalityStateUnknown
}

func (r *NormalizedResponse) Duration() time.Duration {
	if r == nil {
		return 0
	}
	return r.duration
}

func (r *NormalizedResponse) SetDuration(dur time.Duration) {
	if r == nil {
		return
	}
	r.duration = dur
}

func (r *NormalizedResponse) Attempts() int {
	if r == nil {
		return 0
	}
	v, ok := r.attempts.Load().(int)
	if !ok {
		return 0
	}
	return v
}

func (r *NormalizedResponse) SetAttempts(attempts int) *NormalizedResponse {
	r.attempts.Store(attempts)
	return r
}

func (r *NormalizedResponse) Retries() int {
	if r == nil {
		return 0
	}
	v, ok := r.retries.Load().(int)
	if !ok {
		return 0
	}
	return v
}

func (r *NormalizedResponse) SetRetries(retries int) *NormalizedResponse {
	r.retries.Store(retries)
	return r
}

func (r *NormalizedResponse) Hedges() int {
	if r == nil {
		return 0
	}
	v, ok := r.hedges.Load().(int)
	if !ok {
		return 0
	}
	return v
}

func (r *NormalizedResponse) SetHedges(hedges int) *NormalizedResponse {
	r.hedges.Store(hedges)
	return r
}

func (r *NormalizedResponse) Upstream() Upstream {
	if r == nil {
		return nil
	}
	upstream := r.upstream.Load()
	if upstream == nil {
		return nil
	}

	return upstream.(Upstream)
}

func (r *NormalizedResponse) UpstreamId() string {
	if r == nil {
		return ""
	}
	upstream := r.upstream.Load()
	if upstream == nil {
		return ""
	}

	up := upstream.(Upstream)
	if up.Config() == nil {
		return ""
	}

	return up.Id()
}

func (r *NormalizedResponse) SetUpstream(upstream Upstream) *NormalizedResponse {
	if r == nil {
		return r
	}

	if upstream != nil {
		r.upstream.Store(upstream)
	}
	return r
}

func (r *NormalizedResponse) WithRequest(req *NormalizedRequest) *NormalizedResponse {
	if r == nil {
		return r
	}
	r.Lock()
	defer r.Unlock()
	r.request = req
	return r
}

func (r *NormalizedResponse) WithFromCache(fromCache bool) *NormalizedResponse {
	if r == nil {
		return r
	}
	r.fromCache.Store(fromCache)
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

	// Check if already parsed
	if jrr := r.jsonRpcResponse.Load(); jrr != nil {
		return jrr, r.parseErr
	}

	// Ensure parsing happens only once
	r.parseOnce.Do(func() {
		// Coordinate with Release(): hold a read lock while parsing
		r.bodyMu.RLock()
		defer r.bodyMu.RUnlock()

		// Snapshot current body/expected size safely
		r.Lock()
		body := r.body
		expectedSize := r.expectedSize
		r.Unlock()

		if body != nil {
			jrr := &JsonRpcResponse{}
			err := jrr.ParseFromStream(ctx, body, expectedSize)
			if err != nil {
				r.parseErr = err
				return
			}
			// Parsing succeeded: eagerly close and clear body to release gzip/flate buffers ASAP
			r.closeBodyOnce()
			r.jsonRpcResponse.Store(jrr)
		} else {
			// No body to parse - this shouldn't happen in normal flow
			// but we need to handle it gracefully
			r.parseErr = fmt.Errorf("no body available to parse JsonRpcResponse")
		}
	})

	// Return the parsed response or error
	jrr := r.jsonRpcResponse.Load()
	return jrr, r.parseErr
}

func (r *NormalizedResponse) WithBody(body io.ReadCloser) *NormalizedResponse {
	if r == nil {
		return r
	}
	r.Lock()
	defer r.Unlock()
	r.body = body
	return r
}

func (r *NormalizedResponse) WithExpectedSize(expectedSize int) *NormalizedResponse {
	if r == nil {
		return r
	}
	r.Lock()
	defer r.Unlock()
	r.expectedSize = expectedSize
	return r
}

// AddRef increments the number of pending operations that use this response.
// Call DoneRef when the operation ends so Release can free resources safely.
func (r *NormalizedResponse) AddRef() {
	if r == nil {
		return
	}
	r.pendingOps.Add(1)
}

// DoneRef decrements the number of pending operations that use this response.
func (r *NormalizedResponse) DoneRef() {
	if r == nil {
		return
	}
	r.pendingOps.Done()
}

func (r *NormalizedResponse) WithJsonRpcResponse(jrr *JsonRpcResponse) *NormalizedResponse {
	r.jsonRpcResponse.Store(jrr)
	return r
}

func (r *NormalizedResponse) Request() *NormalizedRequest {
	if r == nil {
		return nil
	}
	r.RLock()
	defer r.RUnlock()
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

	jrr.errMu.RLock()
	defer jrr.errMu.RUnlock()
	jrr.resultMu.RLock()
	defer jrr.resultMu.RUnlock()

	if len(jrr.result) == 0 && jrr.Error == nil && jrr.ID() == 0 {
		return true
	}

	return false
}

func (r *NormalizedResponse) MarshalJSON() ([]byte, error) {
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

	// Wait for all pending background operations (e.g., cache set) to complete
	// before freeing heavy buffers.
	r.pendingOps.Wait()

	// Clear these synchronously (fast, no I/O)
	r.Lock()
	if jrr := r.jsonRpcResponse.Load(); jrr != nil {
		jrr.Free()
	}
	r.jsonRpcResponse.Store(nil)
	if r.request != nil {
		r.request = nil // Break cycle immediately
	}
	r.Unlock()

	// Close the body exactly once and only after parsing completes
	r.bodyMu.Lock()
	r.closeBodyOnce()
	r.bodyMu.Unlock()
}

// closeBodyOnce closes and detaches the response body exactly once.
func (r *NormalizedResponse) closeBodyOnce() {
	if r == nil {
		return
	}
	r.bodyOnce.Do(func() {
		// Detach under lock first
		r.Lock()
		body := r.body
		r.body = nil
		r.Unlock()
		if body != nil {
			if err := body.Close(); err != nil {
				log.Error().Err(err).Msg("failed to close response body")
			}
		}
	})
}

func (r *NormalizedResponse) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}

	fromCache := r.fromCache.Load()
	upstream := r.upstream.Load()

	e.Bool("fromCache", fromCache)
	e.Int("attempts", r.Attempts())
	e.Int("retries", r.Retries())
	e.Int("hedges", r.Hedges())
	e.Interface("evmBlockRef", r.evmBlockRef.Load())
	e.Interface("evmBlockNumber", r.evmBlockNumber.Load())

	if upstream != nil {
		up := upstream.(Upstream)
		if up.Config() != nil {
			e.Str("upstream", up.Id())
		} else {
			e.Str("upstream", fmt.Sprintf("%p", up))
		}
	} else {
		e.Str("upstream", "nil")
	}

	jrr := r.jsonRpcResponse.Load()
	if jrr != nil {
		e.Object("jsonRpc", jrr)
	}
}

func (r *NormalizedResponse) Hash(ctx ...context.Context) (string, error) {
	if r == nil {
		return "", nil
	}

	jrr, err := r.JsonRpcResponse(ctx...)
	if err != nil {
		return "", err
	}

	return jrr.CanonicalHash(ctx...)
}

func (r *NormalizedResponse) HashWithIgnoredFields(ignoreFields []string, ctx ...context.Context) (string, error) {
	if r == nil {
		return "", nil
	}

	jrr, err := r.JsonRpcResponse(ctx...)
	if err != nil {
		return "", err
	}

	return jrr.CanonicalHashWithIgnoredFields(ignoreFields, ctx...)
}

func (r *NormalizedResponse) Size(ctx ...context.Context) (int, error) {
	if r == nil {
		return 0, nil
	}

	jrr, err := r.JsonRpcResponse(ctx...)
	if err != nil {
		return 0, err
	}

	return jrr.Size(ctx...)
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

	// Try to load the already–parsed JsonRpcResponse from the original response.
	ejrr := resp.jsonRpcResponse.Load()

	// If the original response has not yet been parsed (common with multiplexed
	// requests), parse it now so that we can clone a *complete* JsonRpcResponse.
	if ejrr == nil {
		parsedJrr, err := resp.JsonRpcResponse(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to parse original response: %w", err)
		}
		// Use the parsed response directly instead of loading from atomic pointer
		// This avoids race conditions where the parsing might have failed
		ejrr = parsedJrr
	}

	if ejrr == nil {
		return nil, fmt.Errorf("cannot copy original response since jsonRpcResponse is nil")
	}

	// Use request ID because the multiplexed upstream call may carry a
	// different ID.
	jrr, err := ejrr.Clone()
	if err != nil {
		return nil, err
	}

	jrq := req.jsonRpcRequest.Load()
	if jrq == nil {
		return nil, fmt.Errorf("request jsonRpcRequest is nil")
	}
	if err := jrr.SetID(jrq.ID); err != nil {
		return nil, err
	}
	r.WithJsonRpcResponse(jrr)

	return r, nil
}
