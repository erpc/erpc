package common

import (
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
	err          error

	fromCache bool
	attempts  int
	retries   int
	hedges    int
	upstream  Upstream

	jsonRpcResponse atomic.Pointer[JsonRpcResponse]
	evmBlockNumber  atomic.Value
	evmBlockRef     atomic.Value
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
	if r == nil {
		return false
	}

	return r.fromCache
}

func (r *NormalizedResponse) SetFromCache(fromCache bool) *NormalizedResponse {
	r.fromCache = fromCache
	return r
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
	if r == nil {
		return nil
	}

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

func (r *NormalizedResponse) EvmBlockRefAndNumber() (string, int64, error) {
	if r == nil {
		return "", 0, nil
	}

	var blockNumber int64
	var blockRef string

	// Try to load from local cache
	if n := r.evmBlockNumber.Load(); n != nil {
		blockNumber = n.(int64)
	}
	if br := r.evmBlockRef.Load(); br != nil {
		blockRef = br.(string)
	}
	if blockRef != "" && blockNumber != 0 {
		return blockRef, blockNumber, nil
	}

	// Try to load from response (enriched with request context)
	if r.request == nil {
		return blockRef, blockNumber, nil
	}
	jrr := r.jsonRpcResponse.Load()
	if jrr == nil {
		return blockRef, blockNumber, nil
	}
	rq, err := r.request.JsonRpcRequest()
	if err != nil {
		return blockRef, blockNumber, err
	}
	br, bn, err := ExtractEvmBlockReferenceFromResponse(rq, jrr)
	if br != "" {
		blockRef = br
	}
	if bn != 0 {
		blockNumber = bn
	}
	if err != nil {
		return blockRef, blockNumber, err
	}

	// Store to local cache
	if blockNumber != 0 {
		r.evmBlockNumber.Store(blockNumber)
	}
	if blockRef != "" {
		r.evmBlockRef.Store(blockRef)
	}

	return blockRef, blockNumber, nil
}

func (r *NormalizedResponse) FinalityState() (finality DataFinalityState) {
	finality = DataFinalityStateUnknown

	if r == nil {
		return
	}

	method, _ := r.request.Method()
	if _, ok := EvmStaticMethods[method]; ok {
		// Static methods are not expected to change over time so we can consider them finalized
		finality = DataFinalityStateFinalized
		return
	}

	if r.request == nil {
		return
	}

	ntw := r.request.Network()
	if ntw == nil {
		return
	}

	_, blockNumber, _ := r.request.EvmBlockRefAndNumber()

	if blockNumber > 0 {
		upstream := r.Upstream()
		if upstream != nil {
			stp := ntw.EvmStatePollerOf(upstream.Config().Id)
			if stp != nil {
				if isFinalized, err := stp.IsBlockFinalized(blockNumber); err == nil {
					if isFinalized {
						finality = DataFinalityStateFinalized
					} else {
						finality = DataFinalityStateUnfinalized
					}
				}
			}
		}
	} else {
		// Certain methods that return data for 'pending' blocks/transactions are always considered unfinalized
		switch method {
		case "eth_getBlockByNumber",
			"eth_getBlockByHash",
			"eth_getTransactionByHash",
			"eth_getTransactionReceipt",
			"eth_getTransactionByBlockHashAndIndex",
			"eth_getTransactionByBlockNumberAndIndex":
			// No block number means the data is for 'pending' block/transaction
			finality = DataFinalityStateUnfinalized
		}
	}

	return
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
			e.RawJSON("error", jrr.errBytes)
		}
		if len(jrr.Result) < 100*1024 {
			e.RawJSON("result", jrr.Result)
		} else {
			head := 50 * 1024
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
