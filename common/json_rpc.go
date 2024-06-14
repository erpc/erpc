package common

import (
	"fmt"

	"github.com/rs/zerolog"
)

type JsonRpcRequest struct {
	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      interface{}   `json:"id,omitempty"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type JsonRpcResponse struct {
	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      interface{}   `json:"id"`
	Result  interface{}   `json:"result"`
	Error   *JsonRpcError `json:"error,omitempty"`
}

type JsonRpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Cause   interface{} `json:"cause,omitempty"`
}

func WrapJsonRpcError(r *JsonRpcError) error {
	if r == nil {
		return nil
	}

	return &BaseError{
		Code:    "JsonRpcError",
		Message: fmt.Sprintf("%d: %s", r.Code, r.Message),
		Cause:   r.Cause.(error),
		Details: map[string]interface{}{
			"jsonRpcErrorCode": r.Code,
		},
	}
}

func (r *JsonRpcError) Error() string {
	return fmt.Sprintf("%d: %s", r.Code, r.Message)
}

func (r *JsonRpcRequest) MarshalZerologObject(e *zerolog.Event) {
	e.Str("method", r.Method).Interface("params", r.Params).Interface("id", r.ID)
}

func (r *JsonRpcResponse) MarshalZerologObject(e *zerolog.Event) {
	e.Interface("id", r.ID).Interface("result", r.Result).Interface("error", r.Error)
}
