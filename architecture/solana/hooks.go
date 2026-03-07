package solana

import (
	"context"
	"fmt"
	"net/http"

	"github.com/erpc/erpc/common"
)

// NormalizeHttpJsonRpc performs Solana-specific normalization on an inbound JSON-RPC request.
// For Phase 1 this is a no-op — Solana JSON-RPC is already well-formed.
func NormalizeHttpJsonRpc(_ context.Context, _ *common.NormalizedRequest, _ *common.JsonRpcRequest) {
}

// HandleNetworkPreForward is called before a request is forwarded to any upstream.
// Returns (handled=true, response, err) to short-circuit, or (false, nil, nil) to continue.
func HandleNetworkPreForward(
	_ context.Context,
	_ common.Network,
	_ []common.Upstream,
	_ *common.NormalizedRequest,
) (bool, *common.NormalizedResponse, error) {
	// Phase 1: no Solana-specific pre-forward logic
	return false, nil, nil
}

// HandleUpstreamPreForward is called before a request is sent to a specific upstream.
func HandleUpstreamPreForward(
	_ context.Context,
	_ common.Network,
	_ common.Upstream,
	_ *common.NormalizedRequest,
	_ bool,
) (bool, *common.NormalizedResponse, error) {
	// Phase 1: no Solana-specific pre-forward logic
	return false, nil, nil
}

// HandleUpstreamPostForward is called after a request returns from an upstream.
// It can enrich, correct, or wrap the response.
func HandleUpstreamPostForward(
	ctx context.Context,
	n common.Network,
	u common.Upstream,
	req *common.NormalizedRequest,
	resp *common.NormalizedResponse,
	err error,
	skipCacheRead bool,
) (*common.NormalizedResponse, error) {
	// Phase 1: pass through as-is
	return resp, err
}

// JsonRpcErrorExtractor extracts Solana-specific JSON-RPC errors.
type JsonRpcErrorExtractor struct{}

func NewJsonRpcErrorExtractor() common.JsonRpcErrorExtractor {
	return &JsonRpcErrorExtractor{}
}

func (e *JsonRpcErrorExtractor) Extract(
	resp *http.Response,
	nr *common.NormalizedResponse,
	jr *common.JsonRpcResponse,
	upstream common.Upstream,
) error {
	if jr != nil && jr.Error != nil {
		// Solana error codes:
		//  -32002: Transaction simulation failed
		//  -32003: Transaction rejected
		//  -32004: Block not available
		//  -32005: Node is unhealthy
		//  -32007: Slot skipped
		//  -32009: Requested account not found
		//  -32010: Requested program not found
		//  -32011: Minimum context slot has not been reached
		//  -32012: Long-term storage is temporarily unavailable
		switch jr.Error.Code {
		case -32005: // Node unhealthy
			return common.NewErrEndpointServerSideException(
				fmt.Errorf("solana node unhealthy: %s", jr.Error.Message), nil, resp.StatusCode,
			)
		case -32004, -32007: // Block not available / slot skipped
			return common.NewErrEndpointMissingData(fmt.Errorf("%s", jr.Error.Message), upstream)
		}
		if jr.Error.Code != 0 {
			return common.NewErrEndpointClientSideException(fmt.Errorf("%s", jr.Error.Message))
		}
	}
	return nil
}
