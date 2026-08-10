package svm

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

func TestExtract_MethodNotFound_ReturnsUnsupported(t *testing.T) {
	t.Parallel()
	err := extract(t, -32601, "Method not found", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointUnsupported) {
		t.Fatalf("expected ErrEndpointUnsupported, got %T: %v", err, err)
	}
}

func TestExtract_SlotSkipped_ReturnsMissingData(t *testing.T) {
	t.Parallel()
	err := extract(t, -32007, "Slot 123 was skipped", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		t.Fatalf("expected ErrEndpointMissingData, got %T: %v", err, err)
	}
}

func TestExtract_NodeBehind_ReturnsServerSide(t *testing.T) {
	t.Parallel()
	err := extract(t, -32005, "Node is behind by 42 slots", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
		t.Fatalf("expected ErrEndpointServerSideException, got %T: %v", err, err)
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("NodeBehind must stay retryable across upstreams")
	}
}

func TestExtract_TransactionSimFailed_IsNotRetryableAcrossUpstreams(t *testing.T) {
	t.Parallel()
	err := extract(t, -32002, "Transaction simulation failed", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
		t.Fatalf("expected ErrEndpointClientSideException, got %T: %v", err, err)
	}
	if common.IsRetryableTowardNetwork(err) {
		t.Fatal("Transaction simulation failure must be non-retryable to guard against double-spend")
	}
}

func TestExtract_RateLimitInMessage_BecomesCapacityExceeded(t *testing.T) {
	t.Parallel()
	err := extract(t, -32000, "300/second request limit reached", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
		t.Fatalf("expected ErrEndpointCapacityExceeded, got %T: %v", err, err)
	}
}

func TestExtract_HTTP429_NoJsonBody_BecomesCapacityExceeded(t *testing.T) {
	t.Parallel()
	err := extractNoJr(t, 429)
	if !common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
		t.Fatalf("expected ErrEndpointCapacityExceeded, got %T: %v", err, err)
	}
}

func TestExtract_HTTP500_NoJsonBody_BecomesServerSide(t *testing.T) {
	t.Parallel()
	err := extractNoJr(t, 500)
	if !common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
		t.Fatalf("expected ErrEndpointServerSideException, got %T: %v", err, err)
	}
}

// TestExtract_SynthesizedParseError_ClassifiedFromFailingStatus pins the gate
// that makes the two tests above reachable at all.
//
// The `jr == nil` shape they pass in cannot occur in production:
// NormalizedResponse.JsonRpcResponse() synthesizes a -32700 error object for
// any unparseable body, so a plaintext/HTML/empty 429 from a CDN arrives here
// looking exactly like a JSON-RPC parse error. Classification must therefore
// come from the STATUS whenever the status is itself a failure — a -32700 next
// to a 4xx/5xx means "the body told us nothing", not "the caller sent bad
// JSON". Only on a 2xx does the -32700 case get to speak for itself.
//
// The end-to-end rows live in erpc/svm_hardening_e2e_test.go
// (TestSvm_BareHttpFailure_ClassifiedFromStatusAndFailsOver); this is the
// unit-level statement of the same rule.
func TestExtract_SynthesizedParseError_ClassifiedFromFailingStatus(t *testing.T) {
	t.Parallel()
	// What the parse layer actually synthesizes (common/response.go).
	synthesized := func() *common.ErrJsonRpcExceptionExternal {
		return common.NewErrJsonRpcExceptionExternal(
			int(common.JsonRpcErrorParseException),
			"cannot parse json-rpc response: invalid char", "")
	}
	for _, tc := range []struct {
		name      string
		status    int
		wantCode  common.ErrorCode
		wantWire  common.JsonRpcErrorNumber
		retryable bool
	}{
		{"429 is a quota verdict", 429, common.ErrCodeEndpointCapacityExceeded, -32000, true},
		{"503 is an upstream failure", 503, common.ErrCodeEndpointServerSideException, -32603, true},
		{"400 is a request/config problem", 400, common.ErrCodeEndpointClientSideException, -32600, false},
		// No failing status to defer to: the upstream still produced bytes eRPC
		// could not parse, which is the UPSTREAM's fault, and the raw -32700
		// reaches the client.
		{"200 keeps the parse-error passthrough", 200, common.ErrCodeEndpointServerSideException, -32700, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := extractWith(t, synthesized(), tc.status)
			if !common.HasErrorCode(err, tc.wantCode) {
				t.Fatalf("HTTP %d + synthesized -32700: expected %s, got %T: %v",
					tc.status, tc.wantCode, err, err)
			}
			if got := wireCodeOf(t, err); got != tc.wantWire {
				t.Errorf("HTTP %d + synthesized -32700: wire code %d, want %d",
					tc.status, got, tc.wantWire)
			}
			if got := common.IsRetryableTowardNetwork(err); got != tc.retryable {
				t.Errorf("HTTP %d + synthesized -32700: retryableTowardNetwork=%v, want %v",
					tc.status, got, tc.retryable)
			}
		})
	}
}

func TestExtract_NonSvmUpstream_IsNoOp(t *testing.T) {
	t.Parallel()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: 500, Header: http.Header{}}
	if got := e.Extract(r, nil, nil, nil); got != nil {
		t.Fatalf("expected nil for nil upstream, got %v", got)
	}
}

// mappedCodeCase is one row of the normalizer's code→class mapping. It lives at
// package scope because two tests must drive the SAME surface: the per-row
// lock-in below, and TestExtract_NoMissingDataVerdictIsTerminal, which asserts a
// family-wide invariant over every row.
type mappedCodeCase struct {
	name        string
	code        int
	msg         string
	wantErrCode common.ErrorCode
	nonRetry    bool // true if retryableTowardNetwork:false must be set
}

func mappedCodeCases() []mappedCodeCase {
	return []mappedCodeCase{
		// Missing-data family — retryable across upstreams (another node can have it).
		{"-32001 block cleaned up", -32001, "Block cleaned up, does not exist on node", common.ErrCodeEndpointMissingData, false},
		{"-32004 block not available", -32004, "Block not available", common.ErrCodeEndpointMissingData, false},
		{"-32007 slot skipped", -32007, "Slot was skipped", common.ErrCodeEndpointMissingData, false},
		{"-32008 no snapshot", -32008, "No snapshot available", common.ErrCodeEndpointMissingData, false},
		{"-32010 key excluded from secondary index", -32010, "Key excluded from secondary index", common.ErrCodeEndpointMissingData, false},
		{"-32011 transaction history not available", -32011, "Transaction history is not available from this node", common.ErrCodeEndpointMissingData, false},
		{"-32014 block status not available", -32014, "Block status not available", common.ErrCodeEndpointMissingData, false},

		// -32009 folds "skipped" (chain truth) with "missing in long-term storage"
		// (per-provider archive policy: BigTable/Old Faithful presence, backfill
		// depth, retention). The archive half means another provider can still
		// serve the slot, so the verdict is SWEPT, not terminal — the same axis
		// pair as -32007. Terminal let the first upstream returning -32009 end the
		// sweep, reporting a slot another archive holds as permanently skipped.
		{"-32009 long-term storage slot skipped", -32009, "Slot 12345 was skipped, or missing in long-term storage", common.ErrCodeEndpointMissingData, false},

		// Node-health family — retryable (server-side); another node succeeds.
		{"-32005 node unhealthy", -32005, "Node is behind by 42 slots", common.ErrCodeEndpointServerSideException, false},
		{"-32012 scan error", -32012, "Scan aborted by rooted-slot movement", common.ErrCodeEndpointServerSideException, false},
		{"-32016 min context slot", -32016, "Min context slot not reached", common.ErrCodeEndpointServerSideException, false},
		{"-32019 long-term storage unreachable", -32019, "Long-term storage unreachable", common.ErrCodeEndpointServerSideException, false},

		// Client-side non-retryable family — the request/transaction itself is the
		// problem; every upstream answers identically. Scoped via
		// WithRetryableTowardNetwork(false).
		{"-32003 signature verification failure", -32003, "Transaction signature verification failure", common.ErrCodeEndpointClientSideException, true},
		{"-32006 precompile verification", -32006, "Transaction precompile verification failure", common.ErrCodeEndpointClientSideException, true},
		{"-32013 signature len mismatch", -32013, "Transaction signature length mismatch", common.ErrCodeEndpointClientSideException, true},
		{"-32015 unsupported tx version", -32015, "Transaction version (0) is not supported by the requesting client", common.ErrCodeEndpointClientSideException, true},
		{"-32018 slot not epoch boundary", -32018, "Slot 12345 is not an epoch boundary", common.ErrCodeEndpointClientSideException, true},
		{"-32600 invalid request", -32600, "Malformed request", common.ErrCodeEndpointClientSideException, true},
		{"-32602 invalid params", -32602, "Invalid parameters", common.ErrCodeEndpointClientSideException, true},

		// -32700 is NOT in the client-side family. eRPC serializes the outbound
		// request itself, so a parse error never means "the caller sent bad JSON";
		// it means eRPC could not parse what THIS upstream sent back (the parse
		// layer synthesizes -32700 for any unparseable body). Upstream fault =>
		// server-side and retryable, so the request fails over to a healthy node.
		{"-32700 parse error", -32700, "JSON parse error", common.ErrCodeEndpointServerSideException, false},

		// Epoch-global chain-state condition — identical answer cluster-wide, so
		// ExecutionException (non-retryable by construction in common/errors.go).
		{"-32017 epoch rewards period active", -32017, "Epoch rewards period still active at slot 12345", common.ErrCodeEndpointExecutionException, true},

		// Internal error (retryable).
		{"-32603 internal error", -32603, "Internal server error", common.ErrCodeEndpointServerSideException, false},

		// -32000 disambiguation by message text. Preflight/blockhash failures are
		// client-side (invalid tx state) with retryableTowardNetwork:false.
		{"-32000 blockhash not found → execution", -32000, "Blockhash not found in recent list", common.ErrCodeEndpointClientSideException, true},
		{"-32000 invalid signature → client-side", -32000, "Invalid signature on tx", common.ErrCodeEndpointClientSideException, true},
		{"-32000 long-term storage → swept missing-data", -32000, "Slot 12345 was skipped, or missing in long-term storage", common.ErrCodeEndpointMissingData, false},
		{"-32000 ledger jump → retryable missing-data", -32000, "Slot 12345 was skipped, or missing due to ledger jump to recent snapshot", common.ErrCodeEndpointMissingData, false},
		{"-32000 generic → server-side", -32000, "something unexpected happened", common.ErrCodeEndpointServerSideException, false},

		// Unknown codes still funnel to server-side so the network can failover —
		// both future agave appends (-32042) and out-of-range vendor codes (-39999).
		{"-32042 unknown future agave code", -32042, "Brand new solana error", common.ErrCodeEndpointServerSideException, false},
		{"-39999 unknown code", -39999, "Brand new solana error", common.ErrCodeEndpointServerSideException, false},
	}
}

// TestExtract_AllMappedCodes is a table-driven lock-in for the full error
// mapping from the design doc. Each row pairs a JSON-RPC error code with the
// expected eRPC error category; adding a new row (or changing an existing
// one) should be a deliberate, reviewable change to the normalizer contract.
func TestExtract_AllMappedCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range mappedCodeCases() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := extract(t, tc.code, tc.msg, 200)
			if !common.HasErrorCode(err, tc.wantErrCode) {
				t.Fatalf("code %d %q: got %T %v, want ErrorCode=%s", tc.code, tc.msg, err, err, tc.wantErrCode)
			}
			if !common.IsRetryableTowardNetwork(err) != tc.nonRetry {
				t.Fatalf("code %d %q: retryable-opt-out mismatch (got %v, want %v)",
					tc.code, tc.msg, !common.IsRetryableTowardNetwork(err), tc.nonRetry)
			}
			// The wire code is ALWAYS the upstream's native code. eRPC's verdict
			// lives in wantErrCode above, never in the number the client
			// dispatches on — @solana/kit maps -32002/-32005/-32016/… to named
			// error classes, so collapsing them to -32600/-32603 broke every
			// code-aware Solana client.
			if got := wireCodeOf(t, err); got != common.JsonRpcErrorNumber(tc.code) {
				t.Fatalf("code %d %q: wire code must stay native, got %v", tc.code, tc.msg, got)
			}
		})
	}
}

// -32007 folds two physical causes: "slot skipped" (global) and "missing due
// to ledger jump to recent snapshot" (node-local, post-restart). The node-local
// half means another provider can genuinely serve the slot, so the class stays
// retryable; the truly-skipped half is bounded by the retry budget, and the raw
// -32007 reaching the caller lets clients stop on their side. -32009 folds the
// same shape (skip OR per-provider archive gap) and is treated identically.
func TestExtract_SlotSkipped_IsRetryableAndPreservesCode(t *testing.T) {
	t.Parallel()
	err := extract(t, -32007, "Slot 12345 was skipped", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		t.Fatalf("expected ErrEndpointMissingData, got %T: %v", err, err)
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("-32007 (ambiguous: ledger-jump half is node-local) must stay retryable toward network")
	}
	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(err, &jre) {
		t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T", err)
	}
	if jre.NormalizedCode() != common.JsonRpcErrorNumber(-32007) {
		t.Fatalf("wire code must be -32007, got %v", jre.NormalizedCode())
	}
}

// -32009 is "skipped, OR missing in long-term storage", and the "or" is
// load-bearing: the second half is whether THIS provider runs a long-term
// archive and how far it backfilled, which is per-provider operational policy,
// not chain truth. So the verdict sweeps every upstream once instead of ending
// the sweep on the first -32009. The raw code still reaches the caller, so a
// client that wants to stop can.
func TestExtract_LongTermStorage_IsSweptAndPreservesCode(t *testing.T) {
	t.Parallel()
	err := extract(t, -32009, "Slot 12345 was skipped, or missing in long-term storage", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		t.Fatalf("expected ErrEndpointMissingData, got %T: %v", err, err)
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("-32009 must sweep every upstream once: the long-term-storage half is per-provider archive policy, not chain truth")
	}
	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(err, &jre) {
		t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T", err)
	}
	if jre.NormalizedCode() != common.JsonRpcErrorNumber(-32009) {
		t.Fatalf("wire code must be -32009, got %v", jre.NormalizedCode())
	}
}

func TestExtract_BlockNotAvailable_IsRetryableAndPreservesRawCode(t *testing.T) {
	t.Parallel()
	err := extract(t, -32004, "Block not available", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		t.Fatalf("expected ErrEndpointMissingData, got %T: %v", err, err)
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("-32004 (transient) must remain retryable toward network")
	}
	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(err, &jre) {
		t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T", err)
	}
	// Wire code must preserve raw -32004, NOT normalize to -32014 (JsonRpcErrorMissingData).
	// Normalizing to -32014 caused sol-client BlockNotAvailableException → infinite retry loop.
	if jre.NormalizedCode() != common.JsonRpcErrorNumber(-32004) {
		t.Fatalf("wire code must preserve raw -32004, got %v", jre.NormalizedCode())
	}
}

// The permanent-flag dimension is orthogonal to retryable-toward-network. A
// skipped slot (-32007) is permanent — no time-delayed re-fetch can un-skip it —
// yet the ledger-jump half is node-local, so the class still sweeps every
// upstream once. Both axes must hold. Pins newSweptSkipMissingData's
// WithPermanentMissingData(true) in error_normalizer.go.
func TestExtract_SlotSkipped_IsPermanentButRetryable(t *testing.T) {
	t.Parallel()
	err := extract(t, -32007, "Slot 123 was skipped", 200)
	if !common.IsPermanentlyMissingData(err) {
		t.Fatal("-32007 (skipped slot) must be permanent: a wait-and-retry cannot un-skip it")
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("-32007 must still sweep other upstreams once (ledger-jump half is node-local)")
	}
}

// Codeless -32000 variant carrying agave's ledger-jump message: same treatment
// as -32007 (permanent, but sweeps every upstream once). Pins the
// strings.Contains(low, "ledger jump") branch → newSweptSkipMissingData.
func TestExtract_LedgerJump_IsPermanentButRetryable(t *testing.T) {
	t.Parallel()
	err := extract(t, -32000, "Slot 12345 was skipped, or missing due to ledger jump to recent snapshot", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		t.Fatalf("expected ErrEndpointMissingData, got %T: %v", err, err)
	}
	if !common.IsPermanentlyMissingData(err) {
		t.Fatal("codeless ledger-jump -32000 must be permanent (same class as -32007)")
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("codeless ledger-jump -32000 must stay retryable toward network")
	}
}

// The two axes are orthogonal, so a future change could break either one alone.
// -32009 is permanent (no time-delayed re-fetch can un-skip a slot, nor backfill
// another operator's archive) AND retryable toward the network (sweep every
// upstream once, because archive completeness is per-provider). Pins
// newSweptSkipMissingData's WithPermanentMissingData(true) on the -32009 path.
func TestExtract_LongTermStorage_IsPermanentAndRetryable(t *testing.T) {
	t.Parallel()
	err := extract(t, -32009, "Slot 12345 was skipped, or missing in long-term storage", 200)
	if !common.IsPermanentlyMissingData(err) {
		t.Fatal("-32009 must stay permanent: waiting cannot un-skip a slot or backfill an archive")
	}
	if !common.IsRetryableTowardNetwork(err) {
		t.Fatal("-32009 must be retryable toward network: another provider's archive may hold the slot")
	}
}

// Transient missing-data (-32004 block not available, -32014 block status not
// available) must NOT be permanent: the block may still appear on a wait-retry,
// so the time-delayed re-sweep is worthwhile. Guards the permanent/transient
// boundary against over-classifying the whole MissingData family as permanent.
func TestExtract_Transient_NotPermanent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code int
		msg  string
	}{
		{"-32004 block not available", -32004, "Block not available"},
		{"-32014 block status not available", -32014, "Block status not available"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := extract(t, tc.code, tc.msg, 200)
			if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
				t.Fatalf("expected ErrEndpointMissingData, got %T: %v", err, err)
			}
			if common.IsPermanentlyMissingData(err) {
				t.Fatalf("%s must stay transient: a not-yet-indexed block can appear on a wait-retry", tc.name)
			}
		})
	}
}

// agave attaches an RpcSimulateTransactionResult to -32002. @solana/web3.js
// only raises SendTransactionError (exposing .logs) when it sees that exact
// code, and @solana/kit reads data.err / data.logs off it. The extractor used
// to rewrite the code to -32600 and never read jr.Error.Data at all, so both
// libraries saw an opaque "invalid request" with no simulation output.
func TestExtract_PreflightFailure_PreservesNativeCodeAndData(t *testing.T) {
	t.Parallel()
	data := map[string]interface{}{
		"err":               map[string]interface{}{"InstructionError": []interface{}{float64(0), "InvalidAccountData"}},
		"logs":              []interface{}{"Program 11111111111111111111111111111111 invoke [1]", "Program failed"},
		"unitsConsumed":     float64(1234),
		"accounts":          nil,
		"innerInstructions": nil,
	}
	err := extractWith(t, &common.ErrJsonRpcExceptionExternal{
		Code:    -32002,
		Message: "Transaction simulation failed: Error processing Instruction 0",
		Data:    data,
	}, 200)

	// Outer taxonomy must be untouched: still client-side, still non-retryable
	// (retrying a failed preflight on another node risks a double-spend).
	if !common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
		t.Fatalf("expected ErrEndpointClientSideException, got %T: %v", err, err)
	}
	if common.IsRetryableTowardNetwork(err) {
		t.Fatal("-32002 must stay non-retryable toward network")
	}
	if got := wireCodeOf(t, err); got != common.JsonRpcErrorNumber(-32002) {
		t.Fatalf("wire code must be native -32002, got %v", got)
	}
	if got := dataOf(t, err); !reflect.DeepEqual(got, data) {
		t.Fatalf("error.data must round-trip verbatim\n got: %#v\nwant: %#v", got, data)
	}
}

// ParseError synthesizes Data:"" for bodies that carry no data member. That
// placeholder must not become an empty "data" on the wire, which would shadow
// http_server's includeErrorDetails fallback.
func TestExtract_EmptyStringData_IsNotForwarded(t *testing.T) {
	t.Parallel()
	err := extract(t, -32002, "Transaction simulation failed", 200)
	if got := dataOf(t, err); got != nil {
		t.Fatalf("empty-string data must be dropped, got %#v", got)
	}
}

// Helius and QuickNode answer an expired/over-quota API key with HTTP 401/403
// AND a JSON-RPC error body. Classifying from that body routed the failure to
// whatever generic class its code mapped to and never reached eRPC's
// unauthorized/billing handling, so the status has to win.
func TestExtract_AuthFailure_WithJsonRpcBody_IsUnauthorized(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			err := extractWith(t, &common.ErrJsonRpcExceptionExternal{
				Code:    -32603,
				Message: "invalid api key provided",
				Data:    map[string]interface{}{"plan": "free"},
			}, status)
			if !common.HasErrorCode(err, common.ErrCodeEndpointUnauthorized) {
				t.Fatalf("HTTP %d with a JSON-RPC body must classify as unauthorized, got %T: %v", status, err, err)
			}
			// Upstream message and data still ride along (Fix 1 applies here too).
			var jre *common.ErrJsonRpcExceptionInternal
			if !errors.As(err, &jre) {
				t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T", err)
			}
			if jre.Message != "invalid api key provided" {
				t.Fatalf("upstream message must be preserved, got %q", jre.Message)
			}
			if got := dataOf(t, err); !reflect.DeepEqual(got, map[string]interface{}{"plan": "free"}) {
				t.Fatalf("upstream data must be preserved, got %#v", got)
			}
		})
	}
}

// common.JsonRpcErrorCapacityExceeded is -32005, which Solana assigns to
// NodeUnhealthy. The invariant that keeps a quota verdict distinguishable from
// "this validator is behind" is that eRPC never SYNTHESIZES -32005 on an SVM
// path: a -32005 in an SVM error body always came from the upstream.
func TestExtract_CapacityExceeded_NeverEmitsSolanaNodeUnhealthyCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"bare HTTP 429", extractNoJr(t, 429)},
		{"-32000 rate-limit message", extract(t, -32000, "300/second request limit reached", 200)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !common.HasErrorCode(tc.err, common.ErrCodeEndpointCapacityExceeded) {
				t.Fatalf("expected ErrEndpointCapacityExceeded, got %T: %v", tc.err, tc.err)
			}
			if got := wireCodeOf(t, tc.err); got == common.JsonRpcErrorNumber(-32005) {
				t.Fatal("eRPC capacity verdict must not be emitted as -32005 — an SVM client reads that as NodeUnhealthy")
			}
		})
	}

	// The other half of the invariant: a genuine upstream -32005 reaches the
	// client as -32005, classified as a node-health problem rather than a quota.
	unhealthy := extract(t, -32005, "Node is behind by 42 slots", 200)
	if common.HasErrorCode(unhealthy, common.ErrCodeEndpointCapacityExceeded) {
		t.Fatalf("upstream -32005 must not be read as a capacity error, got %T", unhealthy)
	}
	if got := wireCodeOf(t, unhealthy); got != common.JsonRpcErrorNumber(-32005) {
		t.Fatalf("upstream -32005 must survive to the client, got %v", got)
	}
}

// ---- helpers ---------------------------------------------------------------

func extract(t *testing.T, code int, msg string, status int) error {
	t.Helper()
	return extractWith(t, common.NewErrJsonRpcExceptionExternal(code, msg, ""), status)
}

// extractWith drives the extractor with a fully-formed upstream error object,
// so tests can supply a structured "data" member.
func extractWith(t *testing.T, jrErr *common.ErrJsonRpcExceptionExternal, status int) error {
	t.Helper()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: status, Header: http.Header{}}
	return e.Extract(r, nil, common.MustNewJsonRpcResponse(1, nil, jrErr), newSvmStub())
}

// wireCodeOf returns the JSON-RPC code the client would see, i.e. exactly what
// http_server's buildErrorResponseBody writes as error.code.
func wireCodeOf(t *testing.T, err error) common.JsonRpcErrorNumber {
	t.Helper()
	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(err, &jre) {
		t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T: %v", err, err)
	}
	return jre.NormalizedCode()
}

// dataOf returns what buildErrorResponseBody would write as error.data.
func dataOf(t *testing.T, err error) interface{} {
	t.Helper()
	var jre *common.ErrJsonRpcExceptionInternal
	if !errors.As(err, &jre) {
		t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T: %v", err, err)
	}
	return jre.Details["data"]
}

// extractNoJr drives the extractor with NO json-rpc response at all.
//
// This shape is NOT producible by the production HTTP path:
// NormalizedResponse.JsonRpcResponse() synthesizes a -32700 error object for
// every unparseable body, so `jr == nil` only ever happens in a test. Treating
// these two helpers as coverage of "a bare HTTP failure" is what let a plaintext
// 429 become a non-retryable parse error for so long. The real coverage is
// TestExtract_SynthesizedParseError_ClassifiedFromFailingStatus (unit) and
// TestSvm_BareHttpFailure_ClassifiedFromStatusAndFailsOver (end to end).
func extractNoJr(t *testing.T, status int) error {
	t.Helper()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: status, Header: http.Header{}}
	return e.Extract(r, nil, nil, newSvmStub())
}

func newSvmStub() common.Upstream { return &stubSvm{id: "svm-stub"} }

// stubSvm satisfies the full common.Upstream interface. The extractor only
// reads Config().Type; the rest of the methods are no-ops.
type stubSvm struct{ id string }

func (s *stubSvm) Id() string           { return s.id }
func (s *stubSvm) VendorName() string   { return "" }
func (s *stubSvm) NetworkId() string    { return "svm:mainnet-beta" }
func (s *stubSvm) NetworkLabel() string { return "" }
func (s *stubSvm) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: s.id, Type: common.UpstreamTypeSvm}
}
func (s *stubSvm) Logger() *zerolog.Logger { l := zerolog.Nop(); return &l }
func (s *stubSvm) Vendor() common.Vendor   { return nil }
func (s *stubSvm) Tracker() common.HealthTracker {
	return nil
}
func (s *stubSvm) Forward(_ context.Context, _ *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (s *stubSvm) ShouldHandleMethod(string) (bool, error) { return true, nil }
func (s *stubSvm) Cordon(string, string)                   {}
func (s *stubSvm) Uncordon(string, string)                 {}
func (s *stubSvm) IgnoreMethod(string)                     {}
