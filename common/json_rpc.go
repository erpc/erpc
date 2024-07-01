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
	ID      interface{}          `json:"id"`
	Result  interface{}          `json:"result"`
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

	if aux.Error != nil {
		var customError map[string]interface{}
		if err := json.Unmarshal(aux.Error, &customError); err != nil {
			return err
		}
		r.Error = NewErrJsonRpcException(
			int(customError["code"].(float64)),
			0,
			customError["message"].(string),
			nil,
		)
	}

	return nil
}
