package common

import "fmt"

type JsonRpcRequest struct {
	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      interface{}   `json:"id,omitempty"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type JsonRpcResponse struct {
	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      int           `json:"id"`
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
