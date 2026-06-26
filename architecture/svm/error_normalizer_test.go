package svm

import (
	"context"
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
		// Missing-data family — retryable across upstreams.
		{"-32004 block not available", -32004, "Block not available", common.ErrCodeEndpointMissingData, false},
		{"-32007 slot skipped", -32007, "Slot was skipped", common.ErrCodeEndpointMissingData, false},
		{"-32008 no snapshot", -32008, "No snapshot available", common.ErrCodeEndpointMissingData, false},
		{"-32009 long-term storage slot", -32009, "Long-term storage slot not reachable", common.ErrCodeEndpointMissingData, false},
		{"-32014 block status not available", -32014, "Block status not available", common.ErrCodeEndpointMissingData, false},

		// Node-health family — retryable (server-side).
		{"-32006 node too behind", -32006, "Node too far behind", common.ErrCodeEndpointServerSideException, false},
		{"-32015 node timeout", -32015, "RPC node timeout", common.ErrCodeEndpointServerSideException, false},
		{"-32016 min context slot", -32016, "Min context slot not reached", common.ErrCodeEndpointServerSideException, false},

		// Client-side non-retryable family. Scoped via WithRetryableTowardNetwork(false).
		{"-32003 transaction error", -32003, "Invalid transaction", common.ErrCodeEndpointClientSideException, true},
		{"-32013 transaction history", -32013, "Transaction history not available", common.ErrCodeEndpointClientSideException, true},
		{"-32600 invalid request", -32600, "Malformed request", common.ErrCodeEndpointClientSideException, true},
		{"-32602 invalid params", -32602, "Invalid parameters", common.ErrCodeEndpointClientSideException, true},
		{"-32700 parse error", -32700, "JSON parse error", common.ErrCodeEndpointClientSideException, true},

		// Internal error (retryable).
		{"-32603 internal error", -32603, "Internal server error", common.ErrCodeEndpointServerSideException, false},

		// -32000 disambiguation by message text. ExecutionException carries
		// retryableTowardNetwork:false by construction in common/errors.go —
		// preflight/blockhash failures must never replay against a second upstream.
		{"-32000 blockhash not found → execution", -32000, "Blockhash not found in recent list", common.ErrCodeEndpointExecutionException, true},
		{"-32000 invalid signature → client-side", -32000, "Invalid signature on tx", common.ErrCodeEndpointClientSideException, true},
		{"-32000 generic → server-side", -32000, "something unexpected happened", common.ErrCodeEndpointServerSideException, false},

		// Unknown codes still funnel to server-side so the network can failover.
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
func (s *stubSvm) Cordon(string, string)   {}
func (s *stubSvm) Uncordon(string, string) {}
func (s *stubSvm) IgnoreMethod(string)     {}
