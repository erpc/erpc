package solana

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

// HandleProjectPreForward is the early, project-level pre-forward hook executed
// before cache lookup and before upstream selection. Return (true, resp, err) to
// short-circuit, or (false, nil, nil) to continue with the normal forward path.
//
// Phase 1: no Solana-specific short-circuit logic yet — this is the correct hook
// point for future additions (e.g. serving getGenesisHash from local state).
func HandleProjectPreForward(
	_ context.Context,
	_ common.Network,
	_ *common.NormalizedRequest,
) (handled bool, resp *common.NormalizedResponse, err error) {
	return false, nil, nil
}

// HandleNetworkPreForward is called after upstream selection for upstream-aware logic.
func HandleNetworkPreForward(
	_ context.Context,
	_ common.Network,
	_ []common.Upstream,
	_ *common.NormalizedRequest,
) (bool, *common.NormalizedResponse, error) {
	// Phase 1: no Solana-specific pre-forward logic.
	return false, nil, nil
}

// HandleNetworkPostForward is called after a response is received from the upstream
// loop, at the network level. Used to enrich, fix, or reject responses.
func HandleNetworkPostForward(
	_ context.Context,
	_ common.Network,
	_ *common.NormalizedRequest,
	resp *common.NormalizedResponse,
	err error,
) (*common.NormalizedResponse, error) {
	// Phase 1: pass through as-is.
	return resp, err
}

// HandleUpstreamPreForward is called before a request is sent to a specific upstream.
func HandleUpstreamPreForward(
	_ context.Context,
	_ common.Network,
	_ common.Upstream,
	_ *common.NormalizedRequest,
	_ bool,
) (bool, *common.NormalizedResponse, error) {
	// Phase 1: no Solana-specific pre-forward logic.
	return false, nil, nil
}

// HandleUpstreamPostForward is called after a request returns from an upstream.
// Key responsibility: mark sendTransaction / sendRawTransaction errors as
// non-retryable toward the network so the failsafe retry policy never
// re-submits the same transaction to a different upstream.
func HandleUpstreamPostForward(
	_ context.Context,
	_ common.Network,
	_ common.Upstream,
	req *common.NormalizedRequest,
	resp *common.NormalizedResponse,
	err error,
	_ bool,
) (*common.NormalizedResponse, error) {
	if err == nil {
		return resp, nil
	}

	method, mErr := req.Method()
	if mErr != nil {
		return resp, err
	}

	// sendTransaction and sendRawTransaction must NEVER be retried against a
	// different upstream — duplicate submissions cause double-spend or confusing
	// on-chain state. Mark the error as non-retryable toward the network so
	// the failsafe executor stops after the first attempt.
	switch strings.ToLower(method) {
	case "sendtransaction", "sendrawtransaction":
		if re, ok := err.(common.RetryableError); ok {
			return resp, re.WithRetryableTowardNetwork(false)
		}
		// Wrap non-RetryableError types in a client-side exception with the flag set.
		return resp, common.NewErrEndpointClientSideException(
			fmt.Errorf("%w", err),
		).WithRetryableTowardNetwork(false)
	}

	return resp, err
}

// NormalizeHttpJsonRpc performs Solana-specific normalization on an inbound JSON-RPC request.
// For Phase 1 this is a no-op — Solana JSON-RPC is already well-formed.
func NormalizeHttpJsonRpc(_ context.Context, _ *common.NormalizedRequest, _ *common.JsonRpcRequest) {
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
		//  -32005: Node is unhealthy / behind
		//  -32007: Slot skipped
		//  -32009: Requested account not found
		//  -32010: Requested program not found
		//  -32011: Minimum context slot has not been reached
		//  -32012: Long-term storage is temporarily unavailable
		switch jr.Error.Code {
		case -32005: // Node unhealthy — treat as server-side, triggers failover
			return common.NewErrEndpointServerSideException(
				fmt.Errorf("solana node unhealthy: %s", jr.Error.Message), nil, resp.StatusCode,
			)
		case -32004, -32007: // Block not available / slot skipped — missing data, try another upstream
			return common.NewErrEndpointMissingData(fmt.Errorf("%s", jr.Error.Message), upstream)
		}
		if jr.Error.Code != 0 {
			return common.NewErrEndpointClientSideException(fmt.Errorf("%s", jr.Error.Message))
		}
	}
	return nil
}
