package svm

import (
	"context"
	"errors"
	"net/http"
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

func TestExtract_NonSvmUpstream_IsNoOp(t *testing.T) {
	t.Parallel()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: 500, Header: http.Header{}}
	if got := e.Extract(r, nil, nil, nil); got != nil {
		t.Fatalf("expected nil for nil upstream, got %v", got)
	}
}

// TestExtract_AllMappedCodes is a table-driven lock-in for the full error
// mapping from the design doc. Each row pairs a JSON-RPC error code with the
// expected eRPC error category; adding a new row (or changing an existing
// one) should be a deliberate, reviewable change to the normalizer contract.
func TestExtract_AllMappedCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		code        int
		msg         string
		wantErrCode common.ErrorCode
		nonRetry    bool // true if retryableTowardNetwork:false must be set
	}{
		// Missing-data family — retryable across upstreams (another node can have it).
		{"-32001 block cleaned up", -32001, "Block cleaned up, does not exist on node", common.ErrCodeEndpointMissingData, false},
		{"-32004 block not available", -32004, "Block not available", common.ErrCodeEndpointMissingData, false},
		{"-32007 slot skipped", -32007, "Slot was skipped", common.ErrCodeEndpointMissingData, false},
		{"-32008 no snapshot", -32008, "No snapshot available", common.ErrCodeEndpointMissingData, false},
		{"-32010 key excluded from secondary index", -32010, "Key excluded from secondary index", common.ErrCodeEndpointMissingData, false},
		{"-32011 transaction history not available", -32011, "Transaction history is not available from this node", common.ErrCodeEndpointMissingData, false},
		{"-32014 block status not available", -32014, "Block status not available", common.ErrCodeEndpointMissingData, false},

		// Authoritatively-missing data — right class (MissingData) but terminal at
		// network scope: long-term storage was consulted, every upstream agrees.
		// The MissingData+non-retryable PAIR is the invariant (skip, no failover).
		{"-32009 long-term storage slot skipped", -32009, "Slot 12345 was skipped, or missing in long-term storage", common.ErrCodeEndpointMissingData, true},

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
		{"-32700 parse error", -32700, "JSON parse error", common.ErrCodeEndpointClientSideException, true},

		// Epoch-global chain-state condition — identical answer cluster-wide, so
		// ExecutionException (non-retryable by construction in common/errors.go).
		{"-32017 epoch rewards period active", -32017, "Epoch rewards period still active at slot 12345", common.ErrCodeEndpointExecutionException, true},

		// Internal error (retryable).
		{"-32603 internal error", -32603, "Internal server error", common.ErrCodeEndpointServerSideException, false},

		// -32000 disambiguation by message text. Preflight/blockhash failures are
		// client-side (invalid tx state) with retryableTowardNetwork:false.
		{"-32000 blockhash not found → execution", -32000, "Blockhash not found in recent list", common.ErrCodeEndpointClientSideException, true},
		{"-32000 invalid signature → client-side", -32000, "Invalid signature on tx", common.ErrCodeEndpointClientSideException, true},
		{"-32000 long-term storage → terminal missing-data", -32000, "Slot 12345 was skipped, or missing in long-term storage", common.ErrCodeEndpointMissingData, true},
		{"-32000 ledger jump → retryable missing-data", -32000, "Slot 12345 was skipped, or missing due to ledger jump to recent snapshot", common.ErrCodeEndpointMissingData, false},
		{"-32000 generic → server-side", -32000, "something unexpected happened", common.ErrCodeEndpointServerSideException, false},

		// Unknown codes still funnel to server-side so the network can failover —
		// both future agave appends (-32042) and out-of-range vendor codes (-39999).
		{"-32042 unknown future agave code", -32042, "Brand new solana error", common.ErrCodeEndpointServerSideException, false},
		{"-39999 unknown code", -39999, "Brand new solana error", common.ErrCodeEndpointServerSideException, false},
	}

	for _, tc := range cases {
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
		})
	}
}

// -32007 folds two physical causes: "slot skipped" (global) and "missing due
// to ledger jump to recent snapshot" (node-local, post-restart). The node-local
// half means another provider can genuinely serve the slot, so the class stays
// retryable; the truly-skipped half is bounded by the retry budget, and the raw
// -32007 reaching the caller lets clients stop on their side. Contrast -32009,
// which is authoritative (long-term storage consulted) and terminal.
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

func TestExtract_LongTermStorage_IsNonRetryableAndPreservesCode(t *testing.T) {
	t.Parallel()
	err := extract(t, -32009, "Long-term storage slot not reachable", 200)
	if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
		t.Fatalf("expected ErrEndpointMissingData, got %T: %v", err, err)
	}
	if common.IsRetryableTowardNetwork(err) {
		t.Fatal("-32009 (permanent) must be non-retryable toward network")
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

// ---- helpers ---------------------------------------------------------------

func extract(t *testing.T, code int, msg string, status int) error {
	t.Helper()
	e := NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: status, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, ""))
	return e.Extract(r, nil, jr, newSvmStub())
}

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
