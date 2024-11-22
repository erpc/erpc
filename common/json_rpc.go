package common

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/bytedance/sonic/ast"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type JsonRpcResponse struct {
	// ID is mainly set based on incoming request.
	// During parsing of response from upstream we'll still parse it
	// so that batch requests are correctly identified.
	// The "id" field is already parsed-value used within erpc,
	// and idBytes is a lazy-loaded byte representation of the ID used when writing responses.
	id      interface{}
	idBytes []byte
	idMu    sync.RWMutex

	// Error is the parsed error from the response bytes, and potentially normalized based on vendor-specific drivers.
	// errBytes is a lazy-loaded byte representation of the Error used when writing responses.
	// When upstream response is received we're going to set the errBytes to incoming bytes and parse the error.
	// Internal components (any error not initiated by upstream) will set the Error field directly and then we'll
	// lazy-load the errBytes for writing responses by marshalling the Error field.
	Error    *ErrJsonRpcExceptionExternal
	errBytes []byte
	errMu    sync.RWMutex

	// Result is the raw bytes of the result from the response, and is used when writing responses.
	// Ideally we don't need to parse these bytes. In cases where we need a specific field (e.g. blockNumber)
	// we use Sonic library to traverse directly to target such field (vs marshalling the whole result in memory).
	Result     []byte
	resultMu   sync.RWMutex
	cachedNode *ast.Node
}

func NewJsonRpcResponse(id interface{}, result interface{}, rpcError *ErrJsonRpcExceptionExternal) (*JsonRpcResponse, error) {
	resultRaw, err := SonicCfg.Marshal(result)
	if err != nil {
		return nil, err
	}
	idBytes, err := SonicCfg.Marshal(id)
	if err != nil {
		return nil, err
	}
	return &JsonRpcResponse{
		id:      id,
		idBytes: idBytes,
		Result:  resultRaw,
		Error:   rpcError,
	}, nil
}

func NewJsonRpcResponseFromBytes(id []byte, resultRaw []byte, errBytes []byte) (*JsonRpcResponse, error) {
	jr := &JsonRpcResponse{
		idBytes: id,
		Result:  resultRaw,
	}

	if len(errBytes) > 0 {
		err := jr.ParseError(util.Mem2Str(errBytes))
		if err != nil {
			return nil, err
		}
	}

	if len(id) > 0 {
		err := jr.parseID()
		if err != nil {
			return nil, err
		}
	}

	return jr, nil
}

func (r *JsonRpcResponse) parseID() error {
	r.idMu.Lock()
	defer r.idMu.Unlock()

	var rawID interface{}
	err := SonicCfg.Unmarshal(r.idBytes, &rawID)
	if err != nil {
		return err
	}

	switch v := rawID.(type) {
	case float64:
		r.id = int64(v)
		// Update idBytes with the parsed int64 value
		r.idBytes, err = SonicCfg.Marshal(r.id)
		return err
	case string:
		r.id = v
		return nil
	case nil:
		return nil
	default:
		return fmt.Errorf("unsupported ID type: %T", v)
	}
}

func (r *JsonRpcResponse) SetID(id interface{}) error {
	r.idMu.Lock()
	defer r.idMu.Unlock()

	r.id = id
	var err error
	r.idBytes, err = SonicCfg.Marshal(id)
	if err != nil {
		return err
	}

	return nil
}

func (r *JsonRpcResponse) ID() interface{} {
	r.idMu.RLock()

	if r.id != nil {
		r.idMu.RUnlock()
		return r.id
	}
	r.idMu.RUnlock()

	r.idMu.Lock()
	defer r.idMu.Unlock()

	if r.idBytes == nil || len(r.idBytes) == 0 {
		return nil
	}

	err := SonicCfg.Unmarshal(r.idBytes, &r.id)
	if err != nil {
		log.Error().Err(err).Bytes("idBytes", r.idBytes).Msg("failed to unmarshal JsonRpcResponse.ID")
	}

	return r.id
}

func (r *JsonRpcResponse) SetIDBytes(idBytes []byte) error {
	r.idMu.Lock()
	defer r.idMu.Unlock()

	r.idBytes = idBytes
	return r.parseID()
}

func (r *JsonRpcResponse) ParseFromStream(reader io.Reader, expectedSize int) error {
	data, err := util.ReadAll(reader, 16*1024, expectedSize) // 16KB
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
			r.idMu.Lock()
			defer r.idMu.Unlock()
			r.idBytes = util.Str2Mem(rawID)
		}
	}

	if resultNode, err := searcher.GetByPath("result"); err == nil {
		if rawResult, err := resultNode.Raw(); err == nil {
			r.resultMu.Lock()
			defer r.resultMu.Unlock()
			r.Result = util.Str2Mem(rawResult)
			r.cachedNode = &resultNode
		} else {
			return err
		}
	} else if errorNode, err := searcher.GetByPath("error"); err == nil {
		if rawError, err := errorNode.Raw(); err == nil {
			if err := r.ParseError(rawError); err != nil {
				return err
			}
		} else {
			return err
		}
	} else if err := r.ParseError(util.Mem2Str(data)); err != nil {
		return err
	}

	return nil
}

func (r *JsonRpcResponse) ParseError(raw string) error {
	r.errMu.Lock()
	defer r.errMu.Unlock()

	r.errBytes = nil

	// First attempt to unmarshal the error as a typical JSON-RPC error
	var rpcErr ErrJsonRpcExceptionExternal
	if err := SonicCfg.UnmarshalFromString(raw, &rpcErr); err != nil {
		// Special case: check for non-standard error structures in the raw data
		if raw == "" || raw == "null" {
			r.Error = NewErrJsonRpcExceptionExternal(
				int(JsonRpcErrorServerSideException),
				"unexpected empty response from upstream endpoint",
				"",
			)
			return nil
		}
	}

	// Check if the error is well-formed and has necessary fields
	if rpcErr.Code != 0 || rpcErr.Message != "" {
		r.Error = &rpcErr
		r.errBytes = util.Str2Mem(raw)
		return nil
	}

	// Handle further special cases
	// Special case #1: numeric "code", "message", and "data"
	sp1 := &struct {
		Code    int    `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
		Data    string `json:"data,omitempty"`
	}{}
	if err := SonicCfg.UnmarshalFromString(raw, sp1); err == nil {
		if sp1.Code != 0 || sp1.Message != "" || sp1.Data != "" {
			r.Error = NewErrJsonRpcExceptionExternal(
				sp1.Code,
				sp1.Message,
				sp1.Data,
			)
			return nil
		}
	}

	// Special case #2: only "error" field as a string
	sp2 := &struct {
		Error string `json:"error"`
	}{}
	if err := SonicCfg.UnmarshalFromString(raw, sp2); err == nil && sp2.Error != "" {
		r.Error = NewErrJsonRpcExceptionExternal(
			int(JsonRpcErrorServerSideException),
			sp2.Error,
			"",
		)
		return nil
	}

	// If no match, treat the raw data as message string
	r.Error = NewErrJsonRpcExceptionExternal(
		int(JsonRpcErrorServerSideException),
		raw,
		"",
	)
	return nil
}

func (r *JsonRpcResponse) PeekStringByPath(path ...interface{}) (string, error) {
	err := r.ensureCachedNode()
	if err != nil {
		return "", err
	}

	r.resultMu.Lock()
	defer r.resultMu.Unlock()
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

	r.resultMu.RLock()
	defer r.resultMu.RUnlock()
	r.errMu.RLock()
	defer r.errMu.RUnlock()

	e.Interface("id", r.ID()).
		Int("resultSize", len(r.Result)).
		Interface("error", r.Error)
}

func (r *JsonRpcResponse) ensureCachedNode() error {
	r.resultMu.RLock()
	if r.cachedNode == nil {
		srchr := ast.NewSearcher(util.Mem2Str(r.Result))
		srchr.ValidateJSON = false
		srchr.ConcurrentRead = false
		srchr.CopyReturn = false
		n, err := srchr.GetByPath()
		if err != nil {
			r.resultMu.RUnlock()
			return err
		}
		r.resultMu.RUnlock()
		r.resultMu.Lock()
		r.cachedNode = &n
		r.resultMu.Unlock()
		return nil
	}

	r.resultMu.RUnlock()
	return nil
}

// MarshalJSON must not be used as it requires marshalling the whole response into a buffer in memory.
func (r *JsonRpcResponse) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("MarshalJSON must not be used on JsonRpcResponse")
}

// WriteTo implements io.WriterTo interface for JsonRpcResponse and is a custom implementation of marshalling json-rpc response,
// this approach uses minimum memory allocations and is faster than a generic JSON marshaller.
func (r *JsonRpcResponse) WriteTo(w io.Writer) (n int64, err error) {
	r.idMu.RLock()
	defer r.idMu.RUnlock()

	r.errMu.RLock()
	defer r.errMu.RUnlock()

	r.resultMu.RLock()
	defer r.resultMu.RUnlock()

	// Write the response opening
	nn, err := w.Write([]byte(`{"jsonrpc":"2.0","id":`))
	if err != nil {
		return int64(nn), err
	}
	n += int64(nn)

	// Write ID
	nn, err = w.Write(r.idBytes)
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)

	if len(r.errBytes) > 0 {
		// Write error field
		nn, err = w.Write([]byte(`,"error":`))
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)

		nn, err = w.Write(r.errBytes)
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	} else if r.Error != nil {
		// Marshal error on demand if errBytes not set
		r.errBytes, err = SonicCfg.Marshal(r.Error)
		if err != nil {
			return n, err
		}

		nn, err = w.Write([]byte(`,"error":`))
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)

		nn, err = w.Write(r.errBytes)
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	}

	if len(r.Result) > 0 {
		// Write result field
		nn, err = w.Write([]byte(`,"result":`))
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)

		nn, err = w.Write(r.Result)
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	}

	// Write closing brace
	nn, err = w.Write([]byte{'}'})
	return n + int64(nn), err
}

func (r *JsonRpcResponse) Clone() (*JsonRpcResponse, error) {
	if r == nil {
		return nil, nil
	}

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

func (r *JsonRpcRequest) UnmarshalJSON(data []byte) error {
	type Alias JsonRpcRequest
	aux := &struct {
		*Alias
		ID json.RawMessage `json:"id,omitempty"`
	}{
		Alias: (*Alias)(r),
	}
	aux.JSONRPC = "2.0"

	if err := SonicCfg.Unmarshal(data, &aux); err != nil {
		return err
	}

	if aux.ID != nil {
		var id interface{}
		if err := SonicCfg.Unmarshal(aux.ID, &id); err != nil {
			return err
		}
		switch v := id.(type) {
		case float64:
			r.ID = int64(v)
		case string:
			r.ID = v
		}
	}

	if r.ID == nil {
		r.ID = util.RandomID()
	} else {
		switch r.ID.(type) {
		case string:
			if r.ID.(string) == "" {
				r.ID = util.RandomID()
			}
		}
	}

	return nil
}

func (r *JsonRpcRequest) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}

	r.RLock()
	defer r.RUnlock()

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

func (r *JsonRpcRequest) PeekByPath(path ...interface{}) (interface{}, error) {
	if r == nil || len(r.Params) == 0 {
		return nil, fmt.Errorf("cannot peek path on empty params")
	}

	// Start with params array as current value
	var current interface{} = r.Params

	// Traverse through path elements
	for _, p := range path {
		switch idx := p.(type) {
		case int:
			// Array index access
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("expected array at path element %v, got %T", p, current)
			}
			if idx < 0 || idx >= len(arr) {
				return nil, fmt.Errorf("array index %d out of bounds", idx)
			}
			current = arr[idx]

		case string:
			// Map key access
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("expected map at path element %v, got %T", p, current)
			}
			val, exists := m[idx]
			if !exists {
				return nil, fmt.Errorf("key %s not found in map", idx)
			}
			current = val

		default:
			return nil, fmt.Errorf("unsupported path element type: %T", p)
		}
	}

	return current, nil
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

	if HasErrorCode(err, ErrCodeUpstreamMethodIgnored) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorUnsupportedException,
			"method ignored by upstream",
			err,
			nil,
		)
	}

	if HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorParseException,
			"failed to parse json-rpc request",
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
