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
// The -32001 .. -32019 range is Solana-specific; each constant below maps 1:1
// to the RpcCustomError enum's JSON_RPC_SERVER_ERROR_* assignments in the
// validator source (names kept aligned so the mapping can be audited):
//
//	https://github.com/anza-xyz/agave/blob/master/rpc-client-api/src/custom_error.rs
//
// Vendor RPCs (Helius, Triton, QuickNode, PublicNode) faithfully forward these
// codes — vendor-specific wording variations are disambiguated by message-text
// matching in the -32000 bucket below. Codes newer than -32019 (agave keeps
// appending) intentionally fall to the safe retryable server-side default.
//
// The normalized taxonomy is intentionally narrower than EVM: SVM lacks
// "execution reverted" semantics at the error level, and rate-limit hints are
// almost always conveyed by HTTP 429 rather than a JSON-RPC code.
const (
	svmCodeInvalidRequest = -32600 // JSON-RPC 2.0 spec
	svmCodeMethodNotFound = -32601 // JSON-RPC 2.0 spec
	svmCodeInvalidParams  = -32602 // JSON-RPC 2.0 spec
	svmCodeInternalError  = -32603 // JSON-RPC 2.0 spec
	svmCodeParseError     = -32700 // JSON-RPC 2.0 spec
	svmCodeServerError    = -32000 // Broad bucket: preflight, blockhash, rate-limit (disambiguated by message)

	svmCodeBlockCleanedUp             = -32001 // BlockCleanedUp: pruned from this node's local ledger
	svmCodeSendTxPreflightFailure     = -32002 // SendTransactionPreflightFailure
	svmCodeTxSignatureVerifyFailure   = -32003 // TransactionSignatureVerificationFailure
	svmCodeBlockNotAvailable          = -32004 // BlockNotAvailable: not yet propagated to this node
	svmCodeNodeUnhealthy              = -32005 // NodeUnhealthy ("Node is behind by N slots")
	svmCodeTxPrecompileVerifyFailure  = -32006 // TransactionPrecompileVerificationFailure
	svmCodeSlotSkipped                = -32007 // SlotSkipped: skipped OR missing from this node's recent ledger
	svmCodeNoSnapshot                 = -32008 // NoSnapshot
	svmCodeLongTermStorageSlotSkipped = -32009 // LongTermStorageSlotSkipped: authoritative, checked long-term storage
	svmCodeKeyExcludedFromIndex       = -32010 // KeyExcludedFromSecondaryIndex: per-node --account-index config
	svmCodeTxHistoryNotAvailable      = -32011 // TransactionHistoryNotAvailable: no history/bigtable on this node
	svmCodeScanError                  = -32012 // ScanError: scan aborted by rooted-slot movement (transient)
	svmCodeTxSignatureLenMismatch     = -32013 // TransactionSignatureLenMismatch
	svmCodeBlockStatusNotAvail        = -32014 // BlockStatusNotAvailableYet
	svmCodeUnsupportedTxVersion       = -32015 // UnsupportedTransactionVersion: caller must set maxSupportedTransactionVersion
	svmCodeMinContextSlotNotReached   = -32016 // MinContextSlotNotReached: node hasn't caught up to minContextSlot yet
	svmCodeEpochRewardsPeriodActive   = -32017 // EpochRewardsPeriodActive: epoch-global condition, identical on every node
	svmCodeSlotNotEpochBoundary       = -32018 // SlotNotEpochBoundary: caller-supplied slot invalid for this query
	svmCodeLongTermStorageUnreachable = -32019 // LongTermStorageUnreachable: this node's bigtable backend is down
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
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// Surface auth failures as a distinct, actionable class (missing/invalid
			// API key) rather than a generic client-side error.
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorClientSideException,
					fmt.Sprintf("svm upstream unauthorized (HTTP %d)", resp.StatusCode),
					nil, details),
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
	//
	// The raw Solana code is preserved on the wire (JsonRpcErrorNumber(code))
	// so callers receive -32004/-32008/-32014/… instead of a normalized
	// -32014 — normalizing everything to JsonRpcErrorMissingData sent
	// sol-client into an infinite BlockNotAvailableException retry loop for
	// unindexed finalized slots.
	//
	// Another upstream can genuinely have this data:
	//   -32001: pruned from this node's local ledger; bigtable-backed nodes have it.
	//   -32004: block exists but hasn't reached this node yet (tip propagation).
	//   -32008: node currently holds no snapshot.
	//   -32010: key excluded from this node's secondary index; index coverage is
	//           per-provider (--account-index), so an indexed provider serves it.
	//           Deliberate divergence from routers that mark this terminal.
	//   -32011: this node has no transaction history (no bigtable); others do.
	//   -32014: block status not computed yet on this node.
	case svmCodeBlockCleanedUp, svmCodeBlockNotAvailable, svmCodeNoSnapshot,
		svmCodeKeyExcludedFromIndex, svmCodeTxHistoryNotAvailable, svmCodeBlockStatusNotAvail:
		return common.NewErrEndpointMissingData(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorNumber(code), msg, nil, details),
			upstream,
		)

	// --- Skipped slot (sweep providers once, but do NOT wait-and-retry) -------
	//
	// -32007: "skipped OR missing due to ledger jump to recent snapshot". The
	// ledger-jump half is node-local (post-snapshot restart), so another provider
	// may still hold the slot — this stays retryable across upstreams and the
	// sweep tries them all. But a *skipped* slot never materializes, so it is
	// marked permanent: once every provider has returned it, the network layer
	// must not run a time-delayed re-sweep (waiting cannot un-skip a slot). The
	// raw -32007 reaches the caller either way.
	case svmCodeSlotSkipped:
		return newSweptSkipMissingData(code, msg, details, upstream)

	// --- Authoritatively-missing data (do NOT retry other upstreams) ----------
	//
	// -32009 is emitted only after the node consulted long-term storage: the
	// slot was skipped, permanently, cluster-wide. Every upstream returns the
	// same verdict, so failing over burns the retry budget (latency + quota)
	// for a deterministic answer. A node whose long-term storage is *down*
	// emits -32019 instead — that one stays retryable below. The raw -32009
	// reaches the caller (JsonRpcErrorNumber preservation) so clients can
	// distinguish the permanent skip from transient -32004/-32014.
	case svmCodeLongTermStorageSlotSkipped:
		return newAuthoritativeMissingData(code, msg, details, upstream)

	// --- Node health issues (failover, but treat as server-side) --------------
	//   -32005: node unhealthy / behind.
	//   -32012: scan aborted by rooted-slot movement — transient, another node
	//           (or a plain retry) succeeds.
	//   -32016: node hasn't reached the request's minContextSlot; a fresher
	//           upstream satisfies it (selection also pre-filters on this).
	//   -32019: this node's long-term storage backend is unreachable.
	case svmCodeNodeUnhealthy, svmCodeScanError, svmCodeMinContextSlotNotReached, svmCodeLongTermStorageUnreachable:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorServerSideException, msg, nil, details),
			details, resp.StatusCode,
		)

	// --- Client-side errors — do NOT retry across upstreams -------------------
	//
	// The request itself is the problem; every upstream answers identically.
	//   -32002/-32003/-32006/-32013: the transaction content is invalid
	//     (preflight, signature verification, precompile verification, signature
	//     length).
	//   -32015: caller omitted/undersized maxSupportedTransactionVersion — a
	//     versioned transaction is present regardless of which node answers.
	//   -32018: caller-supplied slot is not an epoch boundary.
	// WithRetryableTowardNetwork(false) scopes the opt-out to SVM only — EVM
	// ClientSideException still retries (its default).
	case svmCodeSendTxPreflightFailure, svmCodeTxSignatureVerifyFailure,
		svmCodeTxPrecompileVerifyFailure, svmCodeTxSignatureLenMismatch,
		svmCodeUnsupportedTxVersion, svmCodeSlotNotEpochBoundary,
		svmCodeInvalidRequest, svmCodeInvalidParams, svmCodeParseError:
		wrapped := common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details)
		return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)

	// --- Chain-state condition (non-retryable, not the caller's fault) --------
	//
	// -32017: epoch-rewards distribution is in progress for the queried epoch;
	// the whole cluster reports the same until it completes. Surface unretried —
	// ExecutionException is eRPC's "the chain said no, retrying won't change it"
	// class (same treatment as preflight failures in the -32000 bucket).
	case svmCodeEpochRewardsPeriodActive:
		wrapped := common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details)
		return common.NewErrEndpointExecutionException(wrapped)

	// --- Generic -32000 bucket — disambiguate by message ----------------------
	case svmCodeServerError:
		low := strings.ToLower(msg)
		switch {
		case isRateLimitMessage(low):
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorCapacityExceeded, msg, nil, details),
			)
		case strings.Contains(low, "missing in long-term storage"):
			// Codeless -32009 variant: some vendor proxies strip/rewrite the code
			// but keep agave's message. Same authoritative-skip treatment.
			return newAuthoritativeMissingData(code, msg, details, upstream)
		case strings.Contains(low, "ledger jump"):
			// Codeless -32007 variant ("missing due to ledger jump to recent
			// snapshot") — this node lost the slot locally; others have it.
			return newSweptSkipMissingData(code, msg, details, upstream)
		case strings.Contains(low, "preflight") ||
			strings.Contains(low, "transaction simulation failed") ||
			strings.Contains(low, "blockhash not found"):
			// Preflight / blockhash failures — the caller's transaction is the problem.
			// Mark non-retryable to guard against double-spend on retry.
			wrapped := common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorClientSideException, msg, nil, details)
			return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)
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

// newAuthoritativeMissingData builds a MissingData error that is terminal at
// network scope: the data is authoritatively absent cluster-wide (e.g. -32009
// after a long-term-storage check), so failing over to another upstream cannot
// change the answer. Keeping the MissingData class (rather than ClientSide)
// preserves metrics/alerting semantics — this is a data-availability verdict,
// not a malformed request.
func newAuthoritativeMissingData(code int, msg string, details map[string]interface{}, upstream common.Upstream) error {
	err := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorNumber(code), msg, nil, details),
		upstream,
	)
	if me, ok := err.(*common.ErrEndpointMissingData); ok {
		me.WithRetryableTowardNetwork(false)
		me.WithPermanentMissingData(true)
	}
	return err
}

// newSweptSkipMissingData builds a MissingData error for a slot the node reports
// as skipped or lost to a ledger jump (-32007). It stays retryable across
// upstreams (the default) — a ledger jump is node-local, so another provider may
// still hold the slot and the sweep tries them all — but is marked permanent: a
// skipped slot never materializes, so once every provider has been tried the
// network layer must not run a time-delayed re-sweep. The raw code reaches the
// caller.
func newSweptSkipMissingData(code int, msg string, details map[string]interface{}, upstream common.Upstream) error {
	err := common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorNumber(code), msg, nil, details),
		upstream,
	)
	if me, ok := err.(*common.ErrEndpointMissingData); ok {
		me.WithPermanentMissingData(true)
	}
	return err
}
