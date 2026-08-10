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

	code := 0
	msg := ""
	if jr != nil && jr.Error != nil {
		code = jr.Error.Code
		msg = jr.Error.Message

		// Carry the upstream's "data" member through verbatim. Solana packs the
		// actionable half of an error in there — -32002 carries an
		// RpcSimulateTransactionResult (err, logs, unitsConsumed, accounts),
		// -32005 carries numSlotsBehind — and buildErrorResponseBody re-emits
		// details["data"] as the JSON-RPC error's "data" member. Dropping it is
		// what broke @solana/web3.js SendTransactionError (reads data.logs) and
		// @solana/kit. Unlike EVM there is no prefix-stripping to do here: SVM
		// payloads are structured objects, not revert strings.
		if d := jr.Error.Data; d != nil {
			// An empty string is ParseError's "no data" placeholder, not a payload.
			if s, isStr := d.(string); !isStr || s != "" {
				details["data"] = d
			}
		}
	}

	// wireCode is the JSON-RPC "code" the client ends up seeing (it becomes
	// ErrJsonRpcExceptionInternal.NormalizedCode, which buildErrorResponseBody
	// writes as error.code). It is a faithful passthrough of the upstream's own
	// code, because Solana clients dispatch on the exact number: @solana/kit
	// maps -32002/-32005/-32016/… to named error classes and reads error.data
	// alongside, so rewriting the number to an eRPC code silently breaks them.
	// eRPC's routing verdict lives entirely in the OUTER StandardError class
	// (retryable / capacity / client-vs-server), never in this number.
	//
	// Consequence — the eRPC/Solana code collision: common.JsonRpcErrorNumber
	// reuses -32005 for CapacityExceeded and -32016 for Unauthorized, while
	// Solana assigns those to NodeUnhealthy and MinContextSlotNotReached. The
	// invariant that keeps them apart is that on an SVM path eRPC NEVER
	// synthesizes either number: -32005/-32016 in an SVM error body always came
	// from the upstream and always mean the agave semantic. eRPC's own verdicts
	// are conveyed by the outer class and the HTTP status (capacity → 429), with
	// the fallbacks below picking collision-free codes.
	//
	// fallback applies only when the upstream sent an error object with no
	// numeric code (agave always sends one; a few vendor proxies do not). It is
	// required because NewErrJsonRpcExceptionInternal drops a zero normalized
	// code, which would put "code": 0 on the wire. Only two branches can
	// actually reach a fallback — the 401/403 handler and the unknown-code
	// default; each keeps the constant its branch emitted before native-code
	// preservation, so a codeless upstream sees no change. The rest are
	// unreachable by construction (their case constants are non-zero) and name
	// the class they belong to.
	wireCode := func(fallback common.JsonRpcErrorNumber) common.JsonRpcErrorNumber {
		if code == 0 {
			return fallback
		}
		return common.JsonRpcErrorNumber(code)
	}

	// --- HTTP 401/403 outrank the JSON-RPC body -------------------------------
	//
	// An auth failure is a verdict about the credential, not about the RPC call,
	// so it must be classified from the status even when a body is present.
	// Helius and QuickNode return 401/403 *with* a JSON-RPC error object; reading
	// that body first sent an expired API key down whichever generic path its
	// code happened to map to and never reached eRPC's unauthorized/billing
	// handling. The upstream's own message and data still ride along.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		authMsg := msg
		if authMsg == "" {
			authMsg = fmt.Sprintf("svm upstream unauthorized (HTTP %d)", resp.StatusCode)
		}
		// The codeless fallback is -32600 and deliberately NOT
		// common.JsonRpcErrorUnauthorized: that constant is -32016, which an SVM
		// client decodes as MinContextSlotNotReached and answers by retrying
		// against a fresher node forever instead of fixing its key.
		return common.NewErrEndpointUnauthorized(
			common.NewErrJsonRpcExceptionInternal(code, wireCode(svmCodeInvalidRequest), authMsg, nil, details),
		)
	}

	// Prefer a genuine JSON-RPC error body; fall back to HTTP status when the
	// upstream returned a bare HTTP failure.
	//
	// `jr.Error == nil` alone is NOT a sufficient test. The parse layer never
	// yields a nil error for an unparseable body: normalizeJsonRpcError takes jr
	// from nr.JsonRpcResponse(), which SYNTHESIZES a -32700 error object instead.
	// So this gate never fired in the production HTTP path, and a plaintext or
	// HTML 429 from a CDN in front of a provider (Cloudflare and nginx emit
	// neither JSON nor a JSON-RPC envelope) fell through to the -32700 case
	// below and became a hard non-retryable client error — no failover to a
	// healthy upstream, no capacity signal for rate-limit auto-tune, and the
	// caller saw a parse error instead of a rate limit. Measured end to end
	// through the real HTTP path, not inferred.
	//
	// A synthesized -32700 next to a failing status means "the body told us
	// nothing", so classify from the status — the only trustworthy signal left.
	unparseableBody := code == svmCodeParseError && resp.StatusCode >= 400
	if jr == nil || jr.Error == nil || unparseableBody {
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			// -32000 (agave's generic server-error bucket, and what vendors use
			// when they DO send a body with a 429) rather than
			// common.JsonRpcErrorCapacityExceeded: that constant is -32005, which
			// an SVM client decodes as NodeUnhealthy. The 429 status and the outer
			// ErrEndpointCapacityExceeded already carry the quota verdict.
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(0, svmCodeServerError,
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
			// do not retry across upstreams. (401/403 were already handled above.)
			wrapped := common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorClientSideException,
				fmt.Sprintf("svm upstream http failure %d", resp.StatusCode),
				nil, details)
			return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)
		}
		return nil
	}

	switch code {
	// --- Unsupported method ---------------------------------------------------
	case svmCodeMethodNotFound:
		return common.NewErrEndpointUnsupported(
			common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorUnsupportedException), msg, nil, details),
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
			common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorMissingData), msg, nil, details),
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

	// -32009 is "skipped, OR missing in long-term storage" — and the "or" is
	// load-bearing. Only the first half is chain truth. The second half is
	// whether THIS provider runs a long-term archive (BigTable / Old Faithful)
	// and how complete it is, which is per-provider operational policy: archive
	// backfill depth, retention, and how far a given operator chose to go.
	//
	// This was previously terminal at network scope. That made one provider's
	// archive gap answer for the whole cluster: the first upstream to return
	// -32009 ended the sweep (erpc/networks.go breaks out on a non-retryable
	// verdict), so a slot another provider's archive holds was reported to the
	// caller as permanently skipped. Answering "permanently skipped" when the
	// data exists somewhere can make a consumer skip a real block for good;
	// answering "not yet" only costs a retry — the same asymmetry that already
	// governs TranslateToJsonRpcException's retryable-first cause selection.
	//
	// The code already concedes archive state is per-node: -32019
	// (LongTermStorageUnreachable) exists precisely because a backend can be
	// DOWN. Incomplete is the partial case of unavailable, so treating one as
	// node-local and the other as cluster truth was inconsistent.
	//
	// So: swept like -32007 — retryable across upstreams for one pass, still
	// permanent against a time-delayed re-fetch (waiting cannot un-skip a slot,
	// and cannot backfill someone else's archive either). The raw -32009 still
	// reaches the caller, so a client that wants to stop can.
	case svmCodeLongTermStorageSlotSkipped:
		return newSweptSkipMissingData(code, msg, details, upstream)

	// --- Node health issues (failover, but treat as server-side) --------------
	//   -32005: node unhealthy / behind. Reaches the client as a native -32005
	//           carrying agave's {numSlotsBehind} data; eRPC never synthesizes
	//           this number itself (see the wireCode note on the collision with
	//           common.JsonRpcErrorCapacityExceeded).
	//   -32012: scan aborted by rooted-slot movement — transient, another node
	//           (or a plain retry) succeeds.
	//   -32016: node hasn't reached the request's minContextSlot; a fresher
	//           upstream satisfies it (selection also pre-filters on this).
	//           Also reaches the client natively — clients back off on this one
	//           rather than treating it as a hard failure.
	//   -32019: this node's long-term storage backend is unreachable.
	case svmCodeNodeUnhealthy, svmCodeScanError, svmCodeMinContextSlotNotReached, svmCodeLongTermStorageUnreachable:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorServerSideException), msg, nil, details),
			details, resp.StatusCode,
		)

	// --- Client-side errors — do NOT retry across upstreams -------------------
	//
	// The request itself is the problem; every upstream answers identically.
	//   -32002/-32003/-32006/-32013: the transaction content is invalid
	//     (preflight, signature verification, precompile verification, signature
	//     length). -32002 in particular MUST keep its native code and data: the
	//     data is an RpcSimulateTransactionResult and @solana/web3.js only
	//     raises SendTransactionError (with .logs) when it sees code -32002.
	//   -32015: caller omitted/undersized maxSupportedTransactionVersion — a
	//     versioned transaction is present regardless of which node answers.
	//   -32018: caller-supplied slot is not an epoch boundary.
	//   -32602: standard JSON-RPC invalid-params; passed through rather than
	//     collapsed to -32600, which lost a distinction every JSON-RPC client
	//     already understands.
	// WithRetryableTowardNetwork(false) scopes the opt-out to SVM only — EVM
	// ClientSideException still retries (its default).
	case svmCodeSendTxPreflightFailure, svmCodeTxSignatureVerifyFailure,
		svmCodeTxPrecompileVerifyFailure, svmCodeTxSignatureLenMismatch,
		svmCodeUnsupportedTxVersion, svmCodeSlotNotEpochBoundary,
		svmCodeInvalidRequest, svmCodeInvalidParams:
		wrapped := common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorClientSideException), msg, nil, details)
		return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)

	// --- Unparseable upstream response ----------------------------------------
	//
	// -32700 is NOT the caller's fault here and must not sit with the client
	// errors above. eRPC serializes the outbound request itself, so a parse
	// error never means "the caller sent bad JSON" — it means eRPC could not
	// parse what THIS upstream sent back (the parse layer synthesizes -32700 for
	// any unparseable body). That is an upstream fault, so it stays retryable
	// and the request can fail over to a healthy node. The failing-status case
	// is intercepted earlier and classified from the status instead.
	case svmCodeParseError:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorServerSideException), msg, nil, details),
			details, resp.StatusCode,
		)

	// --- Chain-state condition (non-retryable, not the caller's fault) --------
	//
	// -32017: epoch-rewards distribution is in progress for the queried epoch;
	// the whole cluster reports the same until it completes. Surface unretried —
	// ExecutionException is eRPC's "the chain said no, retrying won't change it"
	// class (same treatment as preflight failures in the -32000 bucket).
	case svmCodeEpochRewardsPeriodActive:
		wrapped := common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorClientSideException), msg, nil, details)
		return common.NewErrEndpointExecutionException(wrapped)

	// --- Generic -32000 bucket — disambiguate by message ----------------------
	//
	// Every branch here keeps -32000 on the wire. The message-text match changes
	// eRPC's *routing* verdict, not the upstream's claim about what happened, and
	// a heuristic on vendor wording is far too weak a basis for rewriting the
	// code a client dispatches on.
	case svmCodeServerError:
		low := strings.ToLower(msg)
		switch {
		case isRateLimitMessage(low):
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(code, wireCode(svmCodeServerError), msg, nil, details),
			)
		case strings.Contains(low, "missing in long-term storage"):
			// Codeless -32009 variant: some vendor proxies strip/rewrite the code
			// but keep agave's message. Same swept treatment as the coded case —
			// archive completeness is per-provider, so this must not be terminal.
			return newSweptSkipMissingData(code, msg, details, upstream)
		case strings.Contains(low, "ledger jump"):
			// Codeless -32007 variant ("missing due to ledger jump to recent
			// snapshot") — this node lost the slot locally; others have it.
			return newSweptSkipMissingData(code, msg, details, upstream)
		case strings.Contains(low, "preflight") ||
			strings.Contains(low, "transaction simulation failed") ||
			strings.Contains(low, "blockhash not found"):
			// Preflight / blockhash failures — the caller's transaction is the problem.
			// Mark non-retryable to guard against double-spend on retry.
			wrapped := common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorClientSideException), msg, nil, details)
			return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)
		case strings.Contains(low, "invalid") && (strings.Contains(low, "signature") || strings.Contains(low, "transaction") || strings.Contains(low, "instruction")):
			wrapped := common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorClientSideException), msg, nil, details)
			return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)
		}
		// Default bucket — treat as retryable server-side error.
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorServerSideException), msg, nil, details),
			details, resp.StatusCode,
		)

	// --- Internal error (retry across upstreams) ------------------------------
	case svmCodeInternalError:
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorServerSideException), msg, nil, details),
			details, resp.StatusCode,
		)
	}

	// Unknown JSON-RPC code — keep the raw code, mark as server-side so the network
	// can try another upstream. Solana validator adds new codes occasionally; this
	// makes the default safe rather than surprising. This is also the only path a
	// codeless error object reaches, hence the -32603 fallback.
	return common.NewErrEndpointServerSideException(
		common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorServerSideException), msg, nil, details),
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

// newSweptSkipMissingData builds a MissingData error for a slot the node reports
// as skipped, lost to a ledger jump (-32007), or absent from its long-term
// storage (-32009). It stays retryable across
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
