package websocket

import (
	"context"

	"github.com/erpc/erpc/common"
)

// ForwardFunc is a function that forwards JSON-RPC requests to the network
// This decouples the websocket package from the Network implementation
type ForwardFunc func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)

// NetworkInfo provides minimal network information needed by WebSocket
type NetworkInfo interface {
	Id() string
	ProjectId() string
	Architecture() common.NetworkArchitecture
}
