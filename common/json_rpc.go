package common

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/ast"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type JsonRpcResponse struct {
	sync.RWMutex

	id      interface{}
	idBytes []byte

	Error    *ErrJsonRpcExceptionExternal
	errBytes []byte

	Result     []byte
	cachedNode *ast.Node
}

type RawNode []byte

func (r RawNode) MarshalJSON() ([]byte, error) {
	return r, nil
}

func (r *RawNode) UnmarshalJSON(data []byte) error {
	*r = data
	return nil
}

func NewJsonRpcResponse(id interface{}, result interface{}, rpcError *ErrJsonRpcExceptionExternal) (*JsonRpcResponse, error) {
	resultRaw, err := SonicCfg.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &JsonRpcResponse{
		id:     id,
		Result: resultRaw,
		Error:  rpcError,
	}, nil
}

func NewJsonRpcResponseFromBytes(id []byte, resultRaw []byte, errBytes []byte) (*JsonRpcResponse, error) {
	jr := &JsonRpcResponse{
		idBytes:  id,
		Result:   resultRaw,
		errBytes: errBytes,
	}

	if len(errBytes) > 0 {
		var rpcErr ErrJsonRpcExceptionExternal
		if err := SonicCfg.UnmarshalFromString(util.Mem2Str(errBytes), &rpcErr); err != nil {
			return nil, err
		}
		jr.Error = &rpcErr
	}

	return jr, nil
}

func (r *JsonRpcResponse) ID() interface{} {
	if r.id == nil && len(r.idBytes) > 0 {
		SonicCfg.Unmarshal(r.idBytes, &r.id)
	}
	return r.id
}

func (r *JsonRpcResponse) SetID(id interface{}) {
	r.id = id
	if idBytes, err := SonicCfg.Marshal(id); err == nil {
		r.idBytes = idBytes
	}
}

func (r *JsonRpcResponse) SetIDBytes(idBytes []byte) {
	r.idBytes = idBytes
}

func (r *JsonRpcResponse) ParseFromStream(reader io.Reader) error {
	data, err := util.ReadAll(reader, 64*1024) // 64KB
	if err != nil {
		return err
	}

	// Parse the JSON data into an ast.Node
	searcher := ast.NewSearcher(util.Mem2Str(data))
	searcher.CopyReturn = false
	searcher.ConcurrentRead = false
	searcher.ValidateJSON = false

	// Extract the "id" field
	if idNode, err := searcher.GetByPath("id"); err == nil {
		if rawID, err := idNode.Raw(); err == nil {
			r.idBytes = util.Str2Mem(rawID)
		}
	}

	if resultNode, err := searcher.GetByPath("result"); err == nil {
		if rawResult, err := resultNode.Raw(); err == nil {
			r.Result = util.Str2Mem(rawResult)
		} else {
			return err
		}
	} else if errorNode, err := searcher.GetByPath("error"); err == nil {
		if rawError, err := errorNode.Raw(); err == nil {
			r.errBytes = util.Str2Mem(rawError)
			var rpcErr ErrJsonRpcExceptionExternal
			if err := SonicCfg.UnmarshalFromString(rawError, &rpcErr); err != nil {
				return err
			}
			r.Error = &rpcErr
		} else {
			return err
		}
	} else {
		return fmt.Errorf("neither 'result' nor 'error' found in response")
	}

	return nil
}

func (r *JsonRpcResponse) PeekStringByPath(path ...interface{}) (string, error) {
	r.ensureCachedNode()

	n := r.cachedNode.GetByPath(path...)
	if n == nil {
		return "", fmt.Errorf("could not get '%s' from json-rpc response", path)
	}
	if n.Error() != "" {
		return "", fmt.Errorf("error getting '%s' from json-rpc response: %s", path, n.Error())
	}

	return n.String()
}

func (r *JsonRpcResponse) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}

	r.Lock()
	defer r.Unlock()

	// e.Interface("id", r.ID).
	// 	Interface("result", r.Result).
	// 	Interface("error", r.Error)
	e.Interface("error", r.Error)
}

func (r *JsonRpcResponse) ensureCachedNode() {
	if r.cachedNode == nil {
		n, _ := sonic.GetFromString(util.Mem2Str(r.Result))
		r.cachedNode = &n
	}
}

func (r *JsonRpcResponse) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("json-rpc response must be written using WriteTo()")
}

func (r *JsonRpcResponse) WriteTo(w io.Writer) (int64, error) {
	var total int64

	if (r.Error == nil || len(r.errBytes) == 0) && (r.Result == nil || len(r.Result) == 0) {
		return 0, fmt.Errorf("json-rpc response must have an 'error' or a 'result' set")
	}
	if len(r.idBytes) == 0 {
		return 0, fmt.Errorf("json-rpc response must have an ID")
	}

	// Write '{'
	n, err := w.Write([]byte{'{'})
	total += int64(n)
	if err != nil {
		return total, err
	}

	// Write '"jsonrpc":"2.0"'
	n, err = w.Write([]byte(`"jsonrpc":"2.0"`))
	total += int64(n)
	if err != nil {
		return total, err
	}

	// Write ',"id":'
	n, err = w.Write([]byte(`,"id":`))
	total += int64(n)
	if err != nil {
		return total, err
	}

	// Write ID
	n, err = w.Write(r.idBytes)
	total += int64(n)
	if err != nil {
		return total, err
	}

	if r.Error != nil {
		// Write ',"error":'
		n, err = w.Write([]byte(`,"error":`))
		total += int64(n)
		if err != nil {
			return total, err
		}

		// Write errBytes
		n, err = w.Write(r.errBytes)
		total += int64(n)
		if err != nil {
			return total, err
		}
	} else {
		// Write ',"result":'
		n, err = w.Write([]byte(`,"result":`))
		total += int64(n)
		if err != nil {
			return total, err
		}

		// Write Result
		n, err = w.Write(r.Result)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}

	// Write '}'
	n, err = w.Write([]byte{'}'})
	total += int64(n)
	if err != nil {
		return total, err
	}

	return total, nil
}

func (r *JsonRpcResponse) Clone() (*JsonRpcResponse, error) {
	if r == nil {
		return nil, nil
	}

	r.RLock()
	defer r.RUnlock()

	return &JsonRpcResponse{
		id:         r.id,
		idBytes:    r.idBytes,
		Error:      r.Error,
		errBytes:   r.errBytes,
		Result:     r.Result,
		cachedNode: r.cachedNode,
	}, nil
}

//
// JSON-RPC Request
//

type JsonRpcRequest struct {
	sync.RWMutex

	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      interface{}   `json:"id,omitempty"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

func (r *JsonRpcRequest) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}
	e.Str("method", r.Method).
		Interface("params", r.Params).
		Interface("id", r.ID)
}

func (r *JsonRpcRequest) CacheHash() (string, error) {
	if r == nil {
		return "", nil
	}

	r.RLock()
	defer r.RUnlock()

	hasher := sha256.New()

	for _, p := range r.Params {
		err := hashValue(hasher, p)
		if err != nil {
			return "", err
		}
	}

	b := sha256.Sum256(hasher.Sum(nil))
	return fmt.Sprintf("%s:%x", r.Method, b), nil
}

func hashValue(h io.Writer, v interface{}) error {
	switch t := v.(type) {
	case bool:
		_, err := h.Write(util.Str2Mem(fmt.Sprintf("%t", t)))
		return err
	case int:
		_, err := h.Write(util.Str2Mem(fmt.Sprintf("%d", t)))
		return err
	case float64:
		_, err := h.Write(util.Str2Mem(fmt.Sprintf("%f", t)))
		return err
	case string:
		_, err := h.Write(util.Str2Mem(strings.ToLower(t)))
		return err
	case []interface{}:
		for _, i := range t {
			err := hashValue(h, i)
			if err != nil {
				return err
			}
		}
		return nil
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, err := h.Write(util.Str2Mem(k)); err != nil {
				return err
			}
			err := hashValue(h, t[k])
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported type for value during hash: %+v", v)
	}
}

// TranslateToJsonRpcException is mainly responsible to translate internal eRPC errors (not those coming from upstreams) to
// a proper json-rpc error with correct numeric code.
func TranslateToJsonRpcException(err error) error {
	if HasErrorCode(err, ErrCodeJsonRpcExceptionInternal) {
		return err
	}

	if HasErrorCode(
		err,
		ErrCodeAuthRateLimitRuleExceeded,
		ErrCodeProjectRateLimitRuleExceeded,
		ErrCodeNetworkRateLimitRuleExceeded,
		ErrCodeUpstreamRateLimitRuleExceeded,
	) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorCapacityExceeded,
			"rate-limit exceeded",
			err,
			nil,
		)
	}

	if HasErrorCode(
		err,
		ErrCodeAuthUnauthorized,
	) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorUnauthorized,
			"unauthorized",
			err,
			nil,
		)
	}

	var msg = "internal server error"
	if se, ok := err.(StandardError); ok {
		msg = se.DeepestMessage()
	}

	return NewErrJsonRpcExceptionInternal(
		0,
		JsonRpcErrorServerSideException,
		msg,
		err,
		nil,
	)
}
