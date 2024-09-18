package common

import (
	"io"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

type NormalizedResponse struct {
	sync.RWMutex

	request      *NormalizedRequest
	body         io.ReadCloser
	expectedSize int
	err          error

	fromCache bool
	attempts  int
	retries   int
	hedges    int
	upstream  Upstream

	jsonRpcResponse atomic.Pointer[JsonRpcResponse]
	evmBlockNumber  atomic.Int64
}

type ResponseMetadata interface {
	FromCache() bool
	Attempts() int
	Retries() int
	Hedges() int
	UpstreamId() string
}

func NewNormalizedResponse() *NormalizedResponse {
	return &NormalizedResponse{}
}

func (r *NormalizedResponse) FromCache() bool {
	return r.fromCache
}

func (r *NormalizedResponse) SetFromCache(fromCache bool) *NormalizedResponse {
	r.fromCache = fromCache
	return r
}

func (r *NormalizedResponse) Attempts() int {
	return r.attempts
}

func (r *NormalizedResponse) SetAttempts(attempts int) *NormalizedResponse {
	r.attempts = attempts
	return r
}

func (r *NormalizedResponse) Retries() int {
	return r.retries
}

func (r *NormalizedResponse) SetRetries(retries int) *NormalizedResponse {
	r.retries = retries
	return r
}

func (r *NormalizedResponse) Hedges() int {
	return r.hedges
}

func (r *NormalizedResponse) SetHedges(hedges int) *NormalizedResponse {
	r.hedges = hedges
	return r
}

func (r *NormalizedResponse) Upstream() Upstream {
	if r.upstream == nil {
		return nil
	}

	return r.upstream
}

func (r *NormalizedResponse) UpstreamId() string {
	if r.upstream == nil {
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

func (r *NormalizedResponse) JsonRpcResponse() (*JsonRpcResponse, error) {
	if r == nil {
		return nil, nil
	}

	if jrr := r.jsonRpcResponse.Load(); jrr != nil {
		return jrr, nil
	}

	jrr := &JsonRpcResponse{}

	if r.body != nil {
		err := jrr.ParseFromStream(r.body, r.expectedSize)
		if err != nil {
			return nil, err
		}
	}

	r.jsonRpcResponse.Store(jrr)
	return jrr, nil
}

func (r *NormalizedResponse) WithBody(body io.ReadCloser) *NormalizedResponse {
	r.body = body
	return r
}

func (r *NormalizedResponse) WithExpectedSize(expectedSize int) *NormalizedResponse {
	r.expectedSize = expectedSize
	return r
}

func (r *NormalizedResponse) WithError(err error) *NormalizedResponse {
	r.err = err
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

func (r *NormalizedResponse) Error() error {
	if r.err != nil {
		return r.err
	}

	return nil
}

func (r *NormalizedResponse) IsResultEmptyish() bool {
	jrr, err := r.JsonRpcResponse()

	jrr.resultMu.RLock()
	defer jrr.resultMu.RUnlock()

	if err == nil {
		if jrr == nil {
			return true
		}

		lnr := len(jrr.Result)
		if lnr == 0 ||
			(lnr == 4 && jrr.Result[0] == '"' && jrr.Result[1] == '0' && jrr.Result[2] == 'x' && jrr.Result[3] == '"') ||
			(lnr == 4 && jrr.Result[0] == 'n' && jrr.Result[1] == 'u' && jrr.Result[2] == 'l' && jrr.Result[3] == 'l') ||
			(lnr == 2 && jrr.Result[0] == '"' && jrr.Result[1] == '"') ||
			(lnr == 2 && jrr.Result[0] == '[' && jrr.Result[1] == ']') ||
			(lnr == 2 && jrr.Result[0] == '{' && jrr.Result[1] == '}') {
			return true
		}
	}

	return false
}

func (r *NormalizedResponse) IsObjectNull() bool {
	if r == nil {
		return true
	}

	jrr, _ := r.JsonRpcResponse()
	if jrr == nil {
		return true
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

func (r *NormalizedResponse) EvmBlockNumber() (int64, error) {
	if r == nil {
		return 0, nil
	}

	if n := r.evmBlockNumber.Load(); n != 0 {
		return n, nil
	}
	r.RUnlock()

	if r.request == nil {
		return 0, nil
	}

	rq, err := r.request.JsonRpcRequest()
	if err != nil {
		return 0, err
	}

	jrr := r.jsonRpcResponse.Load()
	if jrr == nil {
		return 0, nil
	}

	_, bn, err := ExtractEvmBlockReferenceFromResponse(rq, jrr)
	if err != nil {
		return 0, err
	}

	r.evmBlockNumber.Store(bn)

	return bn, nil
}

func (r *NormalizedResponse) MarshalJSON() ([]byte, error) {
	r.RLock()
	defer r.RUnlock()

	if jrr := r.jsonRpcResponse.Load(); jrr != nil {
		return SonicCfg.Marshal(jrr)
	}

	return nil, nil
}

func (r *NormalizedResponse) GetReader() (io.Reader, error) {
	r.RLock()
	defer r.RUnlock()

	if jrr := r.jsonRpcResponse.Load(); jrr != nil {
		return jrr.GetReader()
	}

	return nil, nil
}

func (r *NormalizedResponse) Release() {
	r.Lock()
	defer r.Unlock()

	// If body is not closed yet, close it
	if r.body != nil {
		err := r.body.Close()
		if err != nil {
			log.Error().Err(err).Interface("response", r).Msg("failed to close response body")
		}
		r.body = nil
	}

	r.jsonRpcResponse.Store(nil)
}

// CopyResponseForRequest creates a copy of the response for another request
// We use references for underlying Result and Error fields to save memory.
func CopyResponseForRequest(resp *NormalizedResponse, req *NormalizedRequest) (*NormalizedResponse, error) {
	req.RLock()
	defer req.RUnlock()

	if resp == nil {
		return nil, nil
	}

	r := NewNormalizedResponse()
	r.WithRequest(req)

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
