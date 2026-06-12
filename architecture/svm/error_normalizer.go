package svm

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// JsonRpcErrorExtractor translates SVM-native RPC error codes and HTTP failure
// shapes into eRPC's StandardError taxonomy. It intentionally no-ops on non-SVM
// upstreams so a mixed EVM/SVM project can share a single composite extractor.
type JsonRpcErrorExtractor struct{}

func NewJsonRpcErrorExtractor() *JsonRpcErrorExtractor {
	return &JsonRpcErrorExtractor{}
}

// SVM JSON-RPC error codes.
//
// Standard JSON-RPC 2.0 codes (-32600 .. -32700) come from the JSON-RPC spec
// and are used by solana-validator's rpc crate for malformed-request handling.
// The -32000 .. -32016 range is Solana-specific; each code below is documented
// in the validator source at:
//
//	https://github.com/anza-xyz/agave/blob/master/rpc-client-api/src/custom_error.rs
//
// The constant values (-32002, -32004 … -32016) come directly from the
// RpcCustomError enum's JSON_RPC_SERVER_ERROR_* numeric assignments there.
// Vendor RPCs (Helius, Triton, QuickNode, PublicNode) faithfully forward these
// codes — vendor-specific wording variations are disambiguated by message-text
// matching in the -32000 bucket below.
//
// The normalized taxonomy is intentionally narrower than EVM: SVM lacks
// "execution reverted" semantics at the error level, and rate-limit hints are
// almost always conveyed by HTTP 429 rather than a JSON-RPC code.
const (
	svmCodeInvalidRequest       = -32600 // JSON-RPC 2.0 spec
	svmCodeMethodNotFound       = -32601 // JSON-RPC 2.0 spec
	svmCodeInvalidParams        = -32602 // JSON-RPC 2.0 spec
	svmCodeInternalError        = -32603 // JSON-RPC 2.0 spec
	svmCodeParseError           = -32700 // JSON-RPC 2.0 spec
	svmCodeServerError          = -32000 // Broad bucket: preflight, blockhash, rate-limit (disambiguated by message)
	svmCodeTransactionSimFailed = -32002 // SendTransactionPreflightFailure
	svmCodeTransactionError     = -32003 // TransactionError
	svmCodeBlockNotAvailable    = -32004 // BlockNotAvailable
	svmCodeNodeUnhealthy        = -32005 // NodeUnhealthy (a.k.a. NodeBehind)
	svmCodeNodeTooBehind        = -32006 // TransactionPrecompileVerificationFailure / NodeUnhealthy (legacy)
	svmCodeSlotSkipped          = -32007 // SlotSkipped
	svmCodeNoSnapshot           = -32008 // NoSnapshot
	svmCodeLongTermStorageSlot  = -32009 // LongTermStorageSlotSkipped
	svmCodeTransactionHistory   = -32013 // TransactionSignatureVerificationFailure / TransactionHistoryNotAvailable
	svmCodeBlockStatusNotAvail  = -32014 // BlockStatusNotAvailableYet
	svmCodeNodeTimeout          = -32015 // UnsupportedTransactionVersion / NodeTimeout
	svmCodeMinContextSlot       = -32016 // MinContextSlotNotReached
)

func (e *JsonRpcErrorExtractor) Extract(
	resp *http.Response,
	nr *common.NormalizedResponse,
	jr *common.JsonRpcResponse,
	upstream common.Upstream,
) error {
	if upstream == nil || upstream.Config() == nil || upstream.Config().Type != common.UpstreamTypeSvm {
		// Not an SVM upstream — let the composite extractor fall through to EVM/other.
		return nil
	}
	if resp == nil {
		return nil
	}

	// Extract details up front — reused by every branch below.
	details := map[string]interface{}{
		"statusCode": resp.StatusCode,
		"headers":    util.ExtractUsefulHeaders(resp),
	}

	// Prefer JSON-RPC error body; fall back to HTTP status if upstream returned a
	// bare HTTP failure (no JSON body at all).
	if jr == nil || jr.Error == nil {
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorCapacityExceeded,
					fmt.Sprintf("svm upstream rate limited (HTTP %d)", resp.StatusCode),
					nil, details),
			)
		case resp.StatusCode >= 500 && resp.StatusCode <= 599:
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorServerSideException,
					fmt.Sprintf("svm upstream http failure %d", resp.StatusCode),
					nil, details),
				details, resp.StatusCode,
			)
		case resp.StatusCode >= 400 && resp.StatusCode <= 499:
			// 4xx without a JSON body is typically an auth/config issue — treat as client-side,
			// do not retry across upstreams.
			wrapped := common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorClientSideException,
				fmt.Sprintf("svm upstream http failure %d", resp.StatusCode),
				nil, details)
			return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)
		}
		return nil
	}

	code := jr.Error.Code
	msg := jr.Error.Message

	switch code {
	// --- Unsupported method ---------------------------------------------------
	case svmCodeMethodNotFound:
		return common.NewErrEndpointUnsupported(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorUnsupportedException, msg, nil, details),
		)

	// --- Missing data (retryable across upstreams) ----------------------------
	case svmCodeBlockNotAvailable, svmCodeSlotSkipped, svmCodeNoSnapshot,
		svmCodeLongTermStorageSlot, svmCodeBlockStatusNotAvail:
		return common.NewErrEndpointMissingData(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorMissingData, msg, nil, details),
			upstream,
		)

	// --- Node health issues (failover, but treat as server-side) --------------
	case svmCodeNodeUnhealthy, svmCodeNodeTooBehind, svmCodeNodeTimeout, svmCodeMinContextSlot:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorServerSideException, msg, nil, details),
			details, resp.StatusCode,
		)

	// --- Client-side errors — do NOT retry across upstreams -------------------
	//
	// SVM simulation failures, transaction-history misses, and param-validation errors
	// are fundamentally caller problems. Retrying against another upstream will return
	// the same answer and waste quota. WithRetryableTowardNetwork(false) scopes the
	// opt-out to SVM only — EVM ClientSideException still retries (its default).
	case svmCodeTransactionSimFailed, svmCodeTransactionError, svmCodeTransactionHistory,
		svmCodeInvalidRequest, svmCodeInvalidParams, svmCodeParseError:
		wrapped := common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details)
		return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)

	// --- Generic -32000 bucket — disambiguate by message ----------------------
	case svmCodeServerError:
		low := strings.ToLower(msg)
		switch {
		case isRateLimitMessage(low):
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorCapacityExceeded, msg, nil, details),
			)
		case strings.Contains(low, "preflight") ||
			strings.Contains(low, "transaction simulation failed") ||
			strings.Contains(low, "blockhash not found"):
			// Preflight / blockhash failures — the caller's transaction is the problem.
			// Mark non-retryable to guard against double-spend on retry.
			wrapped := common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details)
			return common.NewErrEndpointExecutionException(wrapped)
		case strings.Contains(low, "invalid") && (strings.Contains(low, "signature") || strings.Contains(low, "transaction") || strings.Contains(low, "instruction")):
			wrapped := common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details)
			return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)
		}
		// Default bucket — treat as retryable server-side error.
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorServerSideException, msg, nil, details),
			details, resp.StatusCode,
		)

	// --- Internal error (retry across upstreams) ------------------------------
	case svmCodeInternalError:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorServerSideException, msg, nil, details),
			details, resp.StatusCode,
		)
	}

	// Unknown JSON-RPC code — keep the raw code, mark as server-side so the network
	// can try another upstream. Solana validator adds new codes occasionally; this
	// makes the default safe rather than surprising.
	return common.NewErrEndpointServerSideException(
		common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorServerSideException, msg, nil, details),
		details, resp.StatusCode,
	)
}

// isRateLimitMessage covers vendor-specific rate-limit wording that arrives in
// the generic -32000 bucket rather than as an HTTP 429.
func isRateLimitMessage(lowerMsg string) bool {
	if lowerMsg == "" {
		return false
	}
	// Note: short substrings like "rate" alone would false-positive on
	// "rate-reduction" style messages; use multi-word markers only.
	for _, marker := range []string{
		"too many requests",
		"rate limit",
		"rate-limit",
		"requests per second",
		"request limit reached",
		"throttled",
	} {
		if strings.Contains(lowerMsg, marker) {
			return true
		}
	}
	return false
}
