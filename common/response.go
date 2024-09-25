package common

import (
	// "bufio"
	// "bytes"
	// "bytes"
	"fmt"
	"io"
	// "fmt"
	// "strings"

	// "io"

	// "strings"
	"sync"
	// "unsafe"
	// "github.com/erpc/erpc/util"
	// "github.com/erpc/erpc/util"
	// "github.com/erpc/erpc/util"
	// "github.com/valyala/fasthttp"
)

type NormalizedResponse struct {
	sync.RWMutex

	request *NormalizedRequest
	body    io.ReadCloser
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

func (r *NormalizedResponse) JsonRpcResponse() (*JsonRpcResponse, error) {
	if r == nil {
		return nil, nil
	}

	if r.jsonRpcResponse != nil {
		return r.jsonRpcResponse, nil
	}

	jrr := &JsonRpcResponse{}

	if r.body != nil {
		err := jrr.ParseFromStream(r.body)
		if err != nil {
			return nil, err
		}
	}

	r.jsonRpcResponse = jrr
	return jrr, nil
}

func (r *NormalizedResponse) WithBody(body io.ReadCloser) *NormalizedResponse {
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
	if jrr == nil || (len(jrr.Result) == 0 && jrr.Error == nil && jrr.ID() == nil) {
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
	if r.err != nil {
		return r.err.Error()
	}
	if r.jsonRpcResponse != nil {
		return fmt.Sprintf("ID=%v,Error=%v,ResultSize=%d", r.jsonRpcResponse.ID(), r.jsonRpcResponse.Error, len(r.jsonRpcResponse.Result))
	}
	return "<nil>"
}

func (r *NormalizedResponse) MarshalJSON() ([]byte, error) {
	if r.jsonRpcResponse != nil {
		return SonicCfg.Marshal(r.jsonRpcResponse)
	}

	return nil, nil
}

func (r *NormalizedResponse) WriteTo(w io.Writer) (int64, error) {
	if r.jsonRpcResponse != nil {
		return r.jsonRpcResponse.WriteTo(w)
	}

	return 0, nil
}

func (r *NormalizedResponse) Release() {
	r.Lock()
	defer r.Unlock()

	// If body is not closed yet, close it
	if r.body != nil {
		r.body.Close()
		r.body = nil
	}
}

// CopyResponseForRequest creates a copy of the response for another request.
func CopyResponseForRequest(resp *NormalizedResponse, req *NormalizedRequest) (*NormalizedResponse, error) {
	req.RLock()
	defer req.RUnlock()

	if resp == nil {
		return nil, nil
	}

	r := NewNormalizedResponse()
	r.WithRequest(req)

	if resp.jsonRpcResponse != nil {
		jrr, err := resp.jsonRpcResponse.Clone()
		if err != nil {
			return nil, err
		}
		r.WithJsonRpcResponse(jrr)
		r.jsonRpcResponse.SetID(req.jsonRpcRequest.ID)
	}

	return r, nil
}
