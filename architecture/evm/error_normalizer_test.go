package evm

import (
	"net/http"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// classificationCount reads erpc_upstream_error_classification_total for a
// request that carries neither upstream nor network — every label except
// `category` and `classifier` degrades to "n/a". Tests therefore isolate
// themselves by using a unique method name per test, which keeps them safe to
// run in parallel with each other.
func classificationCount(method, classifier string) float64 {
	return testutil.ToFloat64(telemetry.MetricUpstreamErrorClassification.
		WithLabelValues("n/a", "n/a", "n/a", "n/a", method, classifier))
}

func requestForMethod(method string) *common.NormalizedResponse {
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","method":"` + method + `","params":[],"id":1}`))
	return common.NewNormalizedResponse().WithRequest(req)
}

func detailValue(t *testing.T, err error, key string) interface{} {
	t.Helper()
	se, ok := err.(common.StandardError)
	if !ok {
		t.Fatalf("expected a common.StandardError, got %T: %v", err, err)
	}
	return se.DeepSearch(key)
}

// TestExtractJsonRpcError_RequestTooLargeNormalization verifies that
// provider-specific "eth_getLogs too large" error messages are normalized to
// ErrEndpointRequestTooLarge so that network-level getLogsSplitOnError can
// split the request and retry across upstreams.
func TestExtractJsonRpcError_RequestTooLargeNormalization(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		message string
	}{
		{
			name:    "existing: specify less number of address",
			message: "please specify less number of address in the getLogs query",
		},
		{
			name:    "alchemy/drpc: exceed max addresses or topics per search position",
			message: "exceed max addresses or topics per search position",
		},
		{
			name:    "infura: filters limit",
			message: "This query contains 5006 filters. The current limit is 5000.",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := &http.Response{StatusCode: 200, Header: http.Header{}}
			jrErr := common.NewErrJsonRpcExceptionExternal(
				int(common.JsonRpcErrorServerSideException),
				tc.message,
				"",
			)
			jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

			err := ExtractJsonRpcError(r, nil, jr, nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge) {
				t.Fatalf("expected ErrEndpointRequestTooLarge, got %T: %v", err, err)
			}
		})
	}
}

// TestExtractJsonRpcError_InsufficientFunds_TracingMethodsRetryable verifies that
// "insufficient funds"/"insufficient balance" replies are retried toward the network
// for tracing methods (trace_*, debug_*, eth_trace*). For those methods the error is a
// state-reconstruction artifact (the traced transaction was mined, so it provably had
// funds; the tracer could not resolve the exact pre-state), and another upstream that
// holds the state typically traces the same block. Writes (eth_sendRawTransaction) and
// live simulations (eth_call) stay deterministic and non-retried.
func TestExtractJsonRpcError_InsufficientFunds_TracingMethodsRetryable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		method        string
		message       string
		wantRetryable bool
	}{
		{
			name:          "trace_block insufficient funds is retried toward network",
			method:        "trace_block",
			message:       "txIndex 2: insufficient funds for gas * price + value: address 0xD9F2 have 22995378262500 want 123605240381889",
			wantRetryable: true,
		},
		{
			name:          "debug_traceTransaction insufficient balance is retried",
			method:        "debug_traceTransaction",
			message:       "insufficient balance",
			wantRetryable: true,
		},
		{
			name:          "eth_traceBlock insufficient funds is retried (eth_trace prefix)",
			method:        "eth_traceBlock",
			message:       "insufficient funds for gas * price + value",
			wantRetryable: true,
		},
		{
			name:          "eth_sendRawTransaction insufficient funds stays non-retried",
			method:        "eth_sendRawTransaction",
			message:       "insufficient funds for gas * price + value",
			wantRetryable: false,
		},
		{
			name:          "eth_call insufficient funds stays non-retried (live simulation)",
			method:        "eth_call",
			message:       "insufficient funds for gas * price + value",
			wantRetryable: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","method":"` + tc.method + `","params":[],"id":1}`))
			nr := common.NewNormalizedResponse().WithRequest(req)

			r := &http.Response{StatusCode: 200, Header: http.Header{}}
			jrErr := common.NewErrJsonRpcExceptionExternal(
				int(common.JsonRpcErrorCallException),
				tc.message,
				"",
			)
			jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

			err := ExtractJsonRpcError(r, nr, jr, nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
				t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
			}
			if got := common.IsRetryableTowardNetwork(err); got != tc.wantRetryable {
				t.Fatalf("IsRetryableTowardNetwork: got %v, want %v (method=%s)", got, tc.wantRetryable, tc.method)
			}
		})
	}
}

// TestExtractJsonRpcError_FallbackIsObservable pins the visibility contract for
// the unmatched path: an upstream error that no matcher recognizes still gets
// the (deliberately weak) server-side/retryable classification, but it is no
// longer silent — it increments the fallback classifier and tags the error
// details. Explicitly matched errors must NOT increment it, otherwise a rising
// fallback rate would stop meaning "vendor phrasing drifted".
func TestExtractJsonRpcError_FallbackIsObservable(t *testing.T) {
	t.Parallel()

	const method = "eth_getBalanceFallbackObservabilityProbe"
	before := classificationCount(method, telemetry.ErrorClassifierFallback)

	// 1. A recognized error (capacity) must not touch the fallback counter.
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	matched := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(
		int(common.JsonRpcErrorServerSideException),
		"your app has exceeded its concurrent requests capacity",
		"",
	))
	err := ExtractJsonRpcError(r, requestForMethod(method), matched, nil)
	if !common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
		t.Fatalf("expected ErrEndpointCapacityExceeded, got %T: %v", err, err)
	}
	if got := classificationCount(method, telemetry.ErrorClassifierFallback); got != before {
		t.Fatalf("matched error incremented the fallback counter: before=%v after=%v", before, got)
	}
	if cb := detailValue(t, err, "classifiedBy"); cb != nil {
		t.Fatalf("matched error should not be tagged classifiedBy, got %v", cb)
	}

	// 2. An unrecognized wording lands on the fallback and is counted.
	unmatched := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(
		int(common.JsonRpcErrorServerSideException),
		"clogged widget flux in the third manifold",
		"",
	))
	err = ExtractJsonRpcError(r, requestForMethod(method), unmatched, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
		t.Fatalf("fallback classification changed: got %T: %v", err, err)
	}
	if got := classificationCount(method, telemetry.ErrorClassifierFallback); got != before+1 {
		t.Fatalf("fallback counter: got %v, want %v", got, before+1)
	}
	if cb := detailValue(t, err, "classifiedBy"); cb != telemetry.ErrorClassifierFallback {
		t.Fatalf("classifiedBy detail: got %v, want %q", cb, telemetry.ErrorClassifierFallback)
	}
}

// bootstrapStubUpstream reproduces the shape of the throwaway upstreams vendors
// build for their SupportsNetwork probe (thirdparty/phony.go): it satisfies
// common.Upstream through a NIL embedded interface, so every method it does not
// override panics when called. Telemetry must never be the code that discovers
// that, which is why classification labels come from the request's LastUpstream
// rather than from the upstream the transport was constructed with.
type bootstrapStubUpstream struct {
	common.Upstream
}

func (u *bootstrapStubUpstream) Id() string { return "temp-bootstrap-probe" }

// TestExtractJsonRpcError_DoesNotTouchTransportUpstream is a regression test for
// a production crash path: vendor bootstrap probes call ExtractJsonRpcError with
// an incomplete stub upstream, and reading vendor/network metadata off it
// segfaults inside the provider initializer.
func TestExtractJsonRpcError_DoesNotTouchTransportUpstream(t *testing.T) {
	t.Parallel()

	const method = "eth_chainIdBootstrapProbe"
	before := classificationCount(method, telemetry.ErrorClassifierFallback)

	// Sanity check, so this test cannot pass vacuously: the stub really is the
	// hazardous shape — a method it does not override panics when called.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatalf("stub upstream no longer panics; this test would no longer guard anything")
			}
		}()
		_ = (&bootstrapStubUpstream{}).VendorName()
	}()

	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(
		int(common.JsonRpcErrorServerSideException),
		"provider bootstrap probe said something nobody matched",
		"",
	))

	// Must not panic: the request never went through Upstream.Forward, so it has
	// no LastUpstream, and the stub handed in by the transport is left alone.
	err := ExtractJsonRpcError(r, requestForMethod(method), jr, &bootstrapStubUpstream{})
	if !common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
		t.Fatalf("expected ErrEndpointServerSideException, got %T: %v", err, err)
	}
	if got := classificationCount(method, telemetry.ErrorClassifierFallback); got != before+1 {
		t.Fatalf("fallback counter: got %v, want %v", got, before+1)
	}
}

// TestExtractJsonRpcError_RevertResultSniffIsObservable covers the 200-OK
// result-byte heuristic. It also pins the KNOWN under-match: a Panic(uint256)
// payload in the same position is still served as a success. That is the
// current behavior on purpose — no fixture of the revert-in-result client shape
// exists to justify broadening the claim — and this test is what will fail
// loudly if someone widens the selector list without adding one.
func TestExtractJsonRpcError_RevertResultSniffIsObservable(t *testing.T) {
	t.Parallel()

	const method = "eth_callRevertSniffProbe"
	before := classificationCount(method, telemetry.ErrorClassifierRevertResultSniff)
	r := &http.Response{StatusCode: 200, Header: http.Header{}}

	// Error(string) selector in `result` => reclassified as a revert. The value
	// is marshaled to a JSON string, so the selector lands at dt[1:11] exactly
	// as it does on the wire.
	revert := common.MustNewJsonRpcResponse(1,
		"0x08c379a0000000000000000000000000000000000000000000000000000000000000020", nil)
	err := ExtractJsonRpcError(r, requestForMethod(method), revert, nil)
	if err == nil {
		t.Fatalf("expected a revert error, got nil")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
		t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
	}
	if got := classificationCount(method, telemetry.ErrorClassifierRevertResultSniff); got != before+1 {
		t.Fatalf("revert sniff counter: got %v, want %v", got, before+1)
	}
	if cb := detailValue(t, err, "classifiedBy"); cb != telemetry.ErrorClassifierRevertResultSniff {
		t.Fatalf("classifiedBy detail: got %v, want %q", cb, telemetry.ErrorClassifierRevertResultSniff)
	}

	// Panic(uint256) selector in the same position => NOT matched today.
	panicSel := common.MustNewJsonRpcResponse(1,
		"0x4e487b710000000000000000000000000000000000000000000000000000000000000012", nil)
	if err := ExtractJsonRpcError(r, requestForMethod(method), panicSel, nil); err != nil {
		t.Fatalf("Panic(uint256) in result is expected to pass through as success today, got %T: %v", err, err)
	}
	if got := classificationCount(method, telemetry.ErrorClassifierRevertResultSniff); got != before+1 {
		t.Fatalf("Panic(uint256) must not increment the revert sniff counter: got %v, want %v", got, before+1)
	}
}

// TestExtractJsonRpcError_InvalidJumpPreservesOriginalMessage verifies the
// chain-specific rewrite keeps the tooling-compatible summary AND stops
// destroying the upstream's own wording, which is now retained in
// details["originalMessage"].
func TestExtractJsonRpcError_InvalidJumpPreservesOriginalMessage(t *testing.T) {
	t.Parallel()

	const method = "eth_callInvalidJumpProbe"
	const upstreamMessage = "execution failed: EVM error: InvalidJump at pc=1234"
	before := classificationCount(method, telemetry.ErrorClassifierInvalidJumpRewrite)

	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(
		int(common.JsonRpcErrorCallException),
		upstreamMessage,
		"",
	))

	err := ExtractJsonRpcError(r, requestForMethod(method), jr, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
		t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "revert: invalid jump destination") {
		t.Fatalf("normalized summary lost: %v", err)
	}
	if got := detailValue(t, err, "originalMessage"); got != upstreamMessage {
		t.Fatalf("originalMessage detail: got %v, want %q", got, upstreamMessage)
	}
	if got := classificationCount(method, telemetry.ErrorClassifierInvalidJumpRewrite); got != before+1 {
		t.Fatalf("invalid jump counter: got %v, want %v", got, before+1)
	}
}
