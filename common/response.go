package common

import (
	"sync"

	"github.com/bytedance/sonic"
)

type NormalizedResponse struct {
	sync.RWMutex

	request *NormalizedRequest
	body    []byte
	err     error

	fromCache bool
	attempts  int
	retries   int
	hedges    int
	upstream  Upstream

	jsonRpcResponse *JsonRpcResponse
	evmBlockNumber  int64
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

func (r *NormalizedResponse) WithBody(body []byte) *NormalizedResponse {
	r.body = body
	return r
}

func (r *NormalizedResponse) WithError(err error) *NormalizedResponse {
	r.err = err
	return r
}

func (r *NormalizedResponse) WithJsonRpcResponse(jrr *JsonRpcResponse) *NormalizedResponse {
	r.jsonRpcResponse = jrr
	return r
}

func (r *NormalizedResponse) Request() *NormalizedRequest {
	if r == nil {
		return nil
	}
	return r.request
}

func (r *NormalizedResponse) Body() []byte {
	if r == nil {
		return nil
	}
	if r.body != nil {
		return r.body
	}

	jrr, err := r.JsonRpcResponse()
	if err != nil {
		return nil
	}

	r.body, err = sonic.Marshal(jrr)
	if err != nil {
		return nil
	}

	return r.body
}

func (r *NormalizedResponse) Error() error {
	if r.err != nil {
		return r.err
	}

	return nil
}

func (r *NormalizedResponse) IsResultEmptyish() bool {
	jrr, err := r.JsonRpcResponse()
	if err == nil {
		if jrr == nil {
			return true
		}

		// Use raw result to avoid json unmarshalling for performance reasons
		if jrr.Result == nil ||
			len(jrr.Result) == 0 ||
			(jrr.Result[0] == '"' && jrr.Result[1] == '0' && jrr.Result[2] == 'x' && jrr.Result[3] == '"' && len(jrr.Result) == 4) ||
			(jrr.Result[0] == 'n' && jrr.Result[1] == 'u' && jrr.Result[2] == 'l' && jrr.Result[3] == 'l' && len(jrr.Result) == 4) ||
			(jrr.Result[0] == '"' && jrr.Result[1] == '"' && len(jrr.Result) == 2) ||
			(jrr.Result[0] == '[' && jrr.Result[1] == ']' && len(jrr.Result) == 2) ||
			(jrr.Result[0] == '{' && jrr.Result[1] == '}' && len(jrr.Result) == 2) {
			return true
		}
	}

	return false
}

func (r *NormalizedResponse) JsonRpcResponse() (*JsonRpcResponse, error) {
	if r == nil {
		return nil, nil
	}

	if r.jsonRpcResponse != nil {
		return r.jsonRpcResponse, nil
	}

	jrr := &JsonRpcResponse{}
	err := sonic.Unmarshal(r.body, jrr)
	if err != nil {
		if len(r.body) == 0 {
			jrr.Error = NewErrJsonRpcExceptionExternal(
				int(JsonRpcErrorServerSideException),
				"unexpected empty response from upstream endpoint",
				"",
			)
		} else {
			jrr.Error = NewErrJsonRpcExceptionExternal(
				int(JsonRpcErrorServerSideException),
				string(r.body),
				"",
			)
		}
	}

	r.jsonRpcResponse = jrr

	return jrr, nil
}

func (r *NormalizedResponse) IsObjectNull() bool {
	if r == nil {
		return true
	}

	jrr, _ := r.JsonRpcResponse()
	if jrr == nil && r.body == nil {
		return true
	}

	return false
}

func (r *NormalizedResponse) EvmBlockNumber() (int64, error) {
	if r == nil {
		return 0, nil
	}

	r.RLock()
	if r.evmBlockNumber != 0 {
		defer r.RUnlock()
		return r.evmBlockNumber, nil
	}
	r.RUnlock()

	if r.request == nil {
		return 0, nil
	}

	rq, err := r.request.JsonRpcRequest()
	if err != nil {
		return 0, err
	}

	_, bn, err := ExtractEvmBlockReferenceFromResponse(rq, r.jsonRpcResponse)
	if err != nil {
		return 0, err
	}

	r.Lock()
	r.evmBlockNumber = bn
	r.Unlock()

	return bn, nil
}

func (r *NormalizedResponse) String() string {
	if r == nil {
		return "<nil>"
	}
	if r.body != nil && len(r.body) > 0 {
		return string(r.body)
	}
	if r.err != nil {
		return r.err.Error()
	}
	if r.jsonRpcResponse != nil {
		b, _ := sonic.Marshal(r.jsonRpcResponse)
		return string(b)
	}
	return "<nil>"
}

func (r *NormalizedResponse) MarshalJSON() ([]byte, error) {
	if r.body != nil {
		return r.body, nil
	}

	if r.jsonRpcResponse != nil {
		return sonic.Marshal(r.jsonRpcResponse)
	}

	return nil, nil
}

func CopyResponseForRequest(resp *NormalizedResponse, req *NormalizedRequest) (*NormalizedResponse, error) {
	req.RLock()
	defer req.RUnlock()

	if resp == nil {
		return nil, nil
	}

	r := NewNormalizedResponse()
	r.WithRequest(req)

	// We need to use request ID because response ID can be different
	// in case of multiplexed requests, where we only sent 1 actual request to the upstream.
	if resp.jsonRpcResponse != nil {
		jrr, err := req.JsonRpcRequest()
		if err != nil {
			return nil, err
		}
		r.WithJsonRpcResponse(&JsonRpcResponse{
			JSONRPC: resp.jsonRpcResponse.JSONRPC,
			ID:      jrr.ID,
			Result:  resp.jsonRpcResponse.Result,
			Error:   resp.jsonRpcResponse.Error,
		})
	} else if resp.body != nil {
		r.WithBody(resp.body)
		jrr, err := r.JsonRpcResponse()
		if err != nil {
			return nil, err
		}
		r.body = nil // This enforces re-marshalling the response body if anyone needs it
		jrr.ID = req.jsonRpcRequest.ID
	}

	return r, nil
}
