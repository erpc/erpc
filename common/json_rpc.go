package common

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"

	"github.com/rs/zerolog"
)

type JsonRpcRequest struct {
	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      interface{}   `json:"id,omitempty"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type JsonRpcResponse struct {
	JSONRPC string               `json:"jsonrpc,omitempty"`
	ID      interface{}          `json:"id,omitempty"`
	Result  interface{}          `json:"result,omitempty"`
	Error   *ErrJsonRpcException `json:"error,omitempty"`
}

func (r *JsonRpcRequest) MarshalZerologObject(e *zerolog.Event) {
	e.Str("method", r.Method).Interface("params", r.Params).Interface("id", r.ID)
}

func (r *JsonRpcRequest) CacheHash() (string, error) {
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
		_, err := h.Write([]byte(t))
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
		for k, i := range t {
			if _, err := h.Write([]byte(k)); err != nil {
				return err
			}
			err := hashValue(h, i)
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported type for value during hash: %+v", v)
	}
}

func (r *JsonRpcResponse) MarshalZerologObject(e *zerolog.Event) {
	e.Interface("id", r.ID).Interface("result", r.Result).Interface("error", r.Error)
}

// Custom unmarshal method for JsonRpcResponse
func (r *JsonRpcResponse) UnmarshalJSON(data []byte) error {
	type Alias JsonRpcResponse
	aux := &struct {
		Error json.RawMessage `json:"error,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Special case upstream does not return proper json-rpc response
	if aux.Error == nil && aux.Result == nil && aux.ID == nil {
		// Special case #1: there is numeric "code" and "message" in the "data"
		sp1 := &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{}
		if err := json.Unmarshal(data, &sp1); err == nil {
			r.Error = NewErrJsonRpcException(
				sp1.Code,
				0,
				sp1.Message,
				nil,
			)
			return nil
		}
		// Special case #2: there is "error" field with string in the body
		sp2 := &struct {
			Error string `json:"error"`
		}{}
		if err := json.Unmarshal(data, &sp2); err == nil {
			r.Error = NewErrJsonRpcException(
				int(JsonRpcErrorServerSideException),
				0,
				sp2.Error,
				nil,
			)
			return nil
		}

		r.Error = NewErrJsonRpcException(
			int(JsonRpcErrorServerSideException),
			0,
			string(data),
			nil,
		)
		return nil
	}

	if aux.Error != nil {
		var customError map[string]interface{}
		if err := json.Unmarshal(aux.Error, &customError); err != nil {
			return err
		}
		var code int
		var msg string

		if c, ok := customError["code"]; ok {
			cf, ok := c.(float64)
			if ok {
				code = int(cf)
			}
		}
		if m, ok := customError["message"]; ok {
			msg = m.(string)
		}

		r.Error = NewErrJsonRpcException(
			code,
			0,
			msg,
			nil,
		)
	}

	return nil
}
