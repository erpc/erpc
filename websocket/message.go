package websocket

import "encoding/json"

// JsonRpcRequest represents an incoming JSON-RPC 2.0 request
type JsonRpcRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	Id      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JsonRpcResponse represents an outgoing JSON-RPC 2.0 response
type JsonRpcResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	Id      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RpcError   `json:"error,omitempty"`
}

// JsonRpcNotification represents a subscription notification (no id field)
type JsonRpcNotification struct {
	Jsonrpc string                    `json:"jsonrpc"`
	Method  string                    `json:"method"`
	Params  *SubscriptionNotification `json:"params"`
}

// SubscriptionNotification is the params field for eth_subscription notifications
type SubscriptionNotification struct {
	Subscription string      `json:"subscription"`
	Result       interface{} `json:"result"`
}

// RpcError represents a JSON-RPC 2.0 error object
type RpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Standard JSON-RPC error codes
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603

	// Custom error codes
	ErrCodeSubscriptionNotFound = -32001
	ErrCodeSubscriptionFailed   = -32002
	ErrCodeTooManySubscriptions = -32005
	ErrCodeWebSocketOnly        = -32000
)

// NewRpcError creates a new RPC error
func NewRpcError(code int, message string, data interface{}) *RpcError {
	return &RpcError{
		Code:    code,
		Message: message,
		Data:    data,
	}
}

// NewErrorResponse creates an error response
func NewErrorResponse(id interface{}, code int, message string, data interface{}) *JsonRpcResponse {
	return &JsonRpcResponse{
		Jsonrpc: "2.0",
		Id:      id,
		Error:   NewRpcError(code, message, data),
	}
}

// NewResultResponse creates a success response
func NewResultResponse(id interface{}, result interface{}) *JsonRpcResponse {
	return &JsonRpcResponse{
		Jsonrpc: "2.0",
		Id:      id,
		Result:  result,
	}
}

// NewNotification creates a subscription notification
func NewNotification(subId string, result interface{}) *JsonRpcNotification {
	return &JsonRpcNotification{
		Jsonrpc: "2.0",
		Method:  "eth_subscription",
		Params: &SubscriptionNotification{
			Subscription: subId,
			Result:       result,
		},
	}
}
