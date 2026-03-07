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
	// on-chain state.
	//
	// We always wrap as ClientSideException so that the network-level upstream
	// loop's `IsClientError` check fires immediately, stopping iteration. Setting
	// retryableTowardNetwork=false additionally stops the upstream-level failsafe.
	switch strings.ToLower(method) {
	case "sendtransaction", "sendrawtransaction":
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
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}

	// ── HTTP-level errors — checked before the JSON-RPC body ─────────────────
	// These fire when a provider rate-limits or rejects auth at the HTTP layer,
	// which may return no JSON body at all.
	if statusCode == 429 {
		return common.NewErrEndpointCapacityExceeded(
			fmt.Errorf("solana upstream rate limited (HTTP 429)"),
		)
	}
	if statusCode == 401 || statusCode == 403 {
		return common.NewErrEndpointUnauthorized(
			fmt.Errorf("solana upstream unauthorized (HTTP %d)", statusCode),
		)
	}
	if statusCode == 500 || statusCode == 502 || statusCode == 503 || statusCode == 504 {
		return common.NewErrEndpointServerSideException(
			fmt.Errorf("solana upstream server error (HTTP %d)", statusCode), nil, statusCode,
		)
	}

	// ── JSON-RPC error body ───────────────────────────────────────────────────
	if jr == nil || jr.Error == nil || jr.Error.Code == 0 {
		return nil
	}

	msg := jr.Error.Message

	// Solana error codes:
	//  -32000: Generic server-side error (rate limits, overloaded, simulation, etc.)
	//  -32002: Transaction simulation failed  → client-side (bad tx)
	//  -32003: Transaction rejected           → client-side (bad params)
	//  -32004: Block not available            → missing data, try another upstream
	//  -32005: Node is unhealthy / behind     → server-side, triggers failover
	//  -32006: Node is behind / not yet impl  → server-side, triggers failover
	//  -32007: Slot skipped                   → missing data, try another upstream
	//  -32008: No snapshot available          → missing data, try another upstream
	//  -32009: Requested account not found    → missing data on this node
	//  -32010: Requested program not found    → missing data on this node
	//  -32011: Min context slot not reached   → node is behind, try another upstream
	//  -32012: Long-term storage unavailable  → server-side, triggers failover
	//  -32015: Block status not yet available → missing data, try another upstream
	//  -32601: Method not found               → unsupported, try another upstream
	switch jr.Error.Code {

	case -32005, -32006: // Node unhealthy / behind — failover to another upstream
		return common.NewErrEndpointServerSideException(
			fmt.Errorf("solana node unhealthy: %s", msg), nil, statusCode,
		)

	case -32000, -32012: // Generic server / storage unavailable — failover
		return common.NewErrEndpointServerSideException(
			fmt.Errorf("%s", msg), nil, statusCode,
		)

	case -32004, -32007, -32008, -32015: // Block/slot/snapshot not available — try another upstream
		return common.NewErrEndpointMissingData(fmt.Errorf("%s", msg), upstream)

	case -32009, -32010: // Account / program not found OR provider indexing restriction.
		// Check for provider-specific "excluded from secondary indexes" before
		// treating as generic missing data — another provider may index the program.
		if strings.Contains(msg, "excluded from account secondary indexes") ||
			strings.Contains(msg, "this RPC method unavailable for key") {
			return common.NewErrEndpointUnsupported(fmt.Errorf("%s", msg))
		}
		return common.NewErrEndpointMissingData(fmt.Errorf("%s", msg), upstream)

	case -32011, -32016: // Min context slot not reached — node is behind, try another upstream.
		// Note: the spec uses -32011; QuickNode uses -32016 for the same condition.
		return common.NewErrEndpointServerSideException(
			fmt.Errorf("solana upstream behind requested slot: %s", msg), nil, statusCode,
		)

	case -32601: // Method not found — this upstream doesn't support it, try another.
		return common.NewErrEndpointUnsupported(fmt.Errorf("%s", msg))
	}

	// Text-based patterns: vendor messages that don't fit standard codes.
	if strings.Contains(msg, "excluded from account secondary indexes") ||
		strings.Contains(msg, "this RPC method unavailable for key") {
		// Provider doesn't index this program — another provider might.
		return common.NewErrEndpointUnsupported(fmt.Errorf("%s", msg))
	}
	if strings.Contains(msg, "Connection rate limits exceeded") ||
		strings.Contains(strings.ToLower(msg), "rate limit") ||
		strings.Contains(strings.ToLower(msg), "too many requests") {
		return common.NewErrEndpointCapacityExceeded(fmt.Errorf("%s", msg))
	}

	// Everything else is a deterministic client-side error (bad params, wrong
	// encoding, invalid address, etc.) — no point retrying another upstream.
	return common.NewErrEndpointClientSideException(fmt.Errorf("%s", msg))
}
