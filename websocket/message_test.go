package websocket

import (
	"encoding/json"
	"testing"
)

func TestNewRpcError(t *testing.T) {
	err := NewRpcError(ErrCodeInvalidParams, "Invalid params", "address is required")

	if err.Code != ErrCodeInvalidParams {
		t.Errorf("expected code %d, got %d", ErrCodeInvalidParams, err.Code)
	}
	if err.Message != "Invalid params" {
		t.Errorf("expected message 'Invalid params', got '%s'", err.Message)
	}
	if err.Data != "address is required" {
		t.Errorf("expected data 'address is required', got '%v'", err.Data)
	}
}

func TestNewErrorResponse(t *testing.T) {
	resp := NewErrorResponse(1, ErrCodeMethodNotFound, "Method not found", nil)

	if resp.Jsonrpc != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got '%s'", resp.Jsonrpc)
	}
	if resp.Id != 1 {
		t.Errorf("expected id 1, got %v", resp.Id)
	}
	if resp.Error == nil {
		t.Error("expected error to be non-nil")
	}
	if resp.Result != nil {
		t.Error("expected result to be nil in error response")
	}
}

func TestNewResultResponse(t *testing.T) {
	subId := "0x1234567890abcdef"
	resp := NewResultResponse(1, subId)

	if resp.Jsonrpc != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got '%s'", resp.Jsonrpc)
	}
	if resp.Id != 1 {
		t.Errorf("expected id 1, got %v", resp.Id)
	}
	if resp.Result != subId {
		t.Errorf("expected result '%s', got '%v'", subId, resp.Result)
	}
	if resp.Error != nil {
		t.Error("expected error to be nil in success response")
	}
}

func TestNewNotification(t *testing.T) {
	subId := "0x1234567890abcdef"
	block := map[string]interface{}{
		"number": "0x123",
		"hash":   "0xabc...",
	}

	notification := NewNotification(subId, block)

	if notification.Jsonrpc != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got '%s'", notification.Jsonrpc)
	}
	if notification.Method != "eth_subscription" {
		t.Errorf("expected method 'eth_subscription', got '%s'", notification.Method)
	}
	if notification.Params.Subscription != subId {
		t.Errorf("expected subscription '%s', got '%s'", subId, notification.Params.Subscription)
	}
}

func TestJsonRpcRequest_Marshal(t *testing.T) {
	req := &JsonRpcRequest{
		Jsonrpc: "2.0",
		Id:      1,
		Method:  "eth_subscribe",
		Params:  json.RawMessage(`["newHeads"]`),
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	var decoded JsonRpcRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal request: %v", err)
	}

	if decoded.Method != "eth_subscribe" {
		t.Errorf("expected method 'eth_subscribe', got '%s'", decoded.Method)
	}
}
