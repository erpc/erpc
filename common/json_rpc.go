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
	"github.com/rs/zerolog"
)

type JsonRpcResponse struct {
	sync.RWMutex

	JSONRPC string                       `json:"jsonrpc,omitempty"`
	ID      interface{}                  `json:"id,omitempty"`
	Result  []byte                       `json:"result,omitempty"`
	Error   *ErrJsonRpcExceptionExternal `json:"error,omitempty"`

	cachedNode *ast.Node
}

func NewJsonRpcResponse(id interface{}, result interface{}, rpcError *ErrJsonRpcExceptionExternal) (*JsonRpcResponse, error) {
	resultRaw, err := SonicCfg.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &JsonRpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  resultRaw,
		Error:   rpcError,
	}, nil
}

// PeekStringByPath is a convenience method to get a string value from the json-rpc response.
// It uses an optimized path-based approach to get the value, which is faster than full json unmarshalling.
func (r *JsonRpcResponse) PeekStringByPath(path ...interface{}) (string, error) {
	r.RLock()
	if r.cachedNode != nil {
		defer r.RUnlock()
		return r.peekStringFromCachedNode(path...)
	}
	r.RUnlock()

	r.Lock()
	defer r.Unlock()

	// Double-check locking pattern
	if r.cachedNode == nil {
		if err := r.ensureCachedNode(); err != nil {
			return "", err
		}
	}

	return r.peekStringFromCachedNode(path...)
}

func (r *JsonRpcResponse) peekStringFromCachedNode(path ...interface{}) (string, error) {
	if len(path) == 0 {
		return r.cachedNode.String()
	}

	result := r.cachedNode.GetByPath(path...)

	if result.Valid() {
		return result.String()
	}

	return "", fmt.Errorf("cannot get string value (path: %v) from json-rpc response: %s", path, result.Error())
}

// PeekBoolByPath is similar to PeekStringByPath, but for boolean values.
func (r *JsonRpcResponse) PeekBoolByPath(path ...interface{}) (bool, error) {
	r.RLock()
	if r.cachedNode != nil {
		defer r.RUnlock()
		return r.peekBoolFromCachedNode(path...)
	}
	r.RUnlock()

	r.Lock()
	defer r.Unlock()

	if r.cachedNode == nil {
		if err := r.ensureCachedNode(); err != nil {
			return false, err
		}
	}

	return r.peekBoolFromCachedNode(path...)
}

func (r *JsonRpcResponse) PeekInterfaceByPath(path ...interface{}) (interface{}, error) {
	r.RLock()
	if r.cachedNode != nil {
		defer r.RUnlock()
		return r.peekInterfaceFromCachedNode(path...)
	}
	r.RUnlock()

	r.Lock()
	defer r.Unlock()

	if r.cachedNode == nil {
		if err := r.ensureCachedNode(); err != nil {
			return nil, err
		}
	}

	return r.peekInterfaceFromCachedNode(path...)
}

func (r *JsonRpcResponse) peekInterfaceFromCachedNode(path ...interface{}) (interface{}, error) {
	if len(path) == 0 {
		return r.cachedNode.Interface()
	}

	result := r.cachedNode.GetByPath(path...)

	if result.Valid() {
		return result.Interface()
	}

	return nil, fmt.Errorf("cannot get value (path: %v) from json-rpc response: %s", path, result.Error())
}

func (r *JsonRpcResponse) ensureCachedNode() error {
	node, err := sonic.GetWithOptions(r.Result, ast.SearchOptions{
		CopyReturn:     true,
		ValidateJSON:   false,
		ConcurrentRead: true,
	})

	if err != nil {
		return err
	}
	r.cachedNode = &node
	return nil
}

func (r *JsonRpcResponse) peekBoolFromCachedNode(path ...interface{}) (bool, error) {
	if len(path) == 0 {
		return r.cachedNode.Bool()
	}

	result := r.cachedNode.GetByPath(path...)

	if result.Valid() {
		return result.Bool()
	}

	return false, fmt.Errorf("cannot get bool value (path: %v) from json-rpc response: %s", path, result.Error())
}

func (r *JsonRpcResponse) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}

	r.Lock()
	defer r.Unlock()

	e.Interface("id", r.ID).
		Interface("result", r.Result).
		Interface("error", r.Error)
}

// Custom unmarshal method for JsonRpcResponse
func (r *JsonRpcResponse) UnmarshalJSON(data []byte) error {
	if r == nil {
		return nil
	}

	r.Lock()
	defer r.Unlock()

	type Alias JsonRpcResponse
	aux := &struct {
		Error []byte `json:"error,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}

	if err := SonicCfg.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Special case upstream does not return proper json-rpc response
	if aux.Error == nil && aux.Result == nil && aux.ID == nil {
		// Special case #1: there is numeric "code" and "message" in the "data"
		sp1 := &struct {
			Code    int    `json:"code,omitempty"`
			Message string `json:"message,omitempty"`
			Data    string `json:"data,omitempty"`
		}{}
		if err := sonic.Unmarshal(data, &sp1); err == nil {
			if sp1.Code != 0 || sp1.Message != "" || sp1.Data != "" {
				r.Error = NewErrJsonRpcExceptionExternal(
					sp1.Code,
					sp1.Message,
					sp1.Data,
				)
				return nil
			}
		}
		// Special case #2: there is only "error" field with string in the body
		sp2 := &struct {
			Error string `json:"error"`
		}{}
		if err := sonic.Unmarshal(data, &sp2); err == nil && sp2.Error != "" {
			r.Error = NewErrJsonRpcExceptionExternal(
				int(JsonRpcErrorServerSideException),
				sp2.Error,
				"",
			)
			return nil
		}

		if len(data) == 0 {
			r.Error = NewErrJsonRpcExceptionExternal(
				int(JsonRpcErrorServerSideException),
				"unexpected empty response from upstream endpoint",
				"",
			)
		} else if data[0] == '{' || data[0] == '[' {
			r.Error = NewErrJsonRpcExceptionExternal(
				int(JsonRpcErrorServerSideException),
				fmt.Sprintf("unexpected response json structure from upstream: %s", string(data)),
				"",
			)
		} else {
			r.Error = NewErrJsonRpcExceptionExternal(
				int(JsonRpcErrorServerSideException),
				string(data),
				"",
			)
		}
		return nil
	}

	if aux.Error != nil {
		var code int
		var msg string
		var data string

		var customObjectError map[string]interface{}
		if err := SonicCfg.Unmarshal(aux.Error, &customObjectError); err == nil {
			if c, ok := customObjectError["code"]; ok {
				if cf, ok := c.(float64); ok {
					code = int(cf)
				}
			}
			if m, ok := customObjectError["message"]; ok {
				if tm, ok := m.(string); ok {
					msg = tm
				}
			}
			if d, ok := customObjectError["data"]; ok {
				if dt, ok := d.(string); ok {
					data = dt
				} else {
					data = fmt.Sprintf("%v", d)
				}
			}
		} else {
			var customStringError string
			if err := sonic.Unmarshal(aux.Error, &customStringError); err == nil {
				code = int(JsonRpcErrorServerSideException)
				msg = customStringError
			}
		}

		r.Error = NewErrJsonRpcExceptionExternal(
			code,
			msg,
			data,
		)
	}

	return nil
}

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
		_, err := h.Write([]byte(fmt.Sprintf("%t", t)))
		return err
	case int:
		_, err := h.Write([]byte(fmt.Sprintf("%d", t)))
		return err
	case float64:
		_, err := h.Write([]byte(fmt.Sprintf("%f", t)))
		return err
	case string:
		_, err := h.Write([]byte(strings.ToLower(t)))
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
			if _, err := h.Write([]byte(k)); err != nil {
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
