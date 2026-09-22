package evm

import (
	"errors"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
)

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

// TestExtractJsonRpcError_ResponseTooBig_JsonRpseeSizeCap verifies that
// jsonrpsee's oversized-response rejection — used by reth and anything else
// built on it — normalizes to ErrEndpointRequestTooLarge, so the eth_getLogs /
// trace_filter splitters narrow the range instead of the caller taking a hard
// failure. Before this, the message matched none of the range-shaped phrases
// and fell through to a generic server-side exception, which does not split.
func TestExtractJsonRpcError_ResponseTooBig_JsonRpseeSizeCap(t *testing.T) {
	t.Parallel()

	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jrErr := common.NewErrJsonRpcExceptionExternal(
		-32008,
		"Response is too big",
		"Exceeded max limit of 167772160",
	)
	jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

	err := ExtractJsonRpcError(r, nil, jr, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge) {
		t.Fatalf("expected ErrEndpointRequestTooLarge, got %T: %v", err, err)
	}
}

// TestExtractJsonRpcError_ExceededMaxLimit_IsNotASizeComplaint pins the
// deliberate narrowness of the match above. "Exceeded max limit of ..." is the
// Data half of the jsonrpsee message, but the phrasing says nothing about SIZE
// — matching on it would turn a quota or rate complaint into a range-splitting
// retry, which neither fixes the problem nor reports it honestly.
func TestExtractJsonRpcError_ExceededMaxLimit_IsNotASizeComplaint(t *testing.T) {
	t.Parallel()

	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jrErr := common.NewErrJsonRpcExceptionExternal(
		int(common.JsonRpcErrorServerSideException),
		"Exceeded max limit of 100 requests per second",
		"",
	)
	jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

	err := ExtractJsonRpcError(r, nil, jr, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge) {
		t.Fatalf("a rate/quota complaint must not normalize to ErrEndpointRequestTooLarge, got %v", err)
	}
}

// TestExtractJsonRpcError_MempoolPolicyRejections verifies that mempool policy
// rejections (observed from Monad's EthTxPoolDropReason, all surfaced as
// -32603 with NO data field) normalize to ErrEndpointExecutionException, not
// ErrEndpointServerSideException, so they never count toward the circuit
// breaker. For eth_sendRawTransaction they stay retryable toward the network
// (pool contents/policies are node-local; another upstream may accept the
// broadcast), consistent with the -32003 "transaction rejected" branch. For
// other methods they are terminal.
//
// data is modeled as nil — the wire shape when the "data" field is absent —
// because a non-nil data value used to be appended to the matched message
// (" Data: <nil>"), which silently broke whole-message checks.
func TestExtractJsonRpcError_MempoolPolicyRejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		method    string
		message   string
		data      interface{}
		wantMatch bool
		wantRetry bool
	}{
		{
			name:      "existing transaction had higher priority",
			method:    "eth_sendRawTransaction",
			message:   "An existing transaction had higher priority",
			wantMatch: true,
			wantRetry: true,
		},
		{
			name:      "newer transaction had higher priority",
			method:    "eth_sendRawTransaction",
			message:   "A newer transaction had higher priority",
			wantMatch: true,
			wantRetry: true,
		},
		{
			name:      "transaction fee too low",
			method:    "eth_sendRawTransaction",
			message:   "Transaction fee too low",
			wantMatch: true,
			wantRetry: true,
		},
		{
			name:      "bare rejected with nil data (wire shape)",
			method:    "eth_sendRawTransaction",
			message:   "rejected",
			wantMatch: true,
			wantRetry: true,
		},
		{
			name:      "rejected with whitespace and casing",
			method:    "eth_sendRawTransaction",
			message:   " Rejected ",
			wantMatch: true,
			wantRetry: true,
		},
		{
			name:      "rejected with empty-string data",
			method:    "eth_sendRawTransaction",
			message:   "rejected",
			data:      "",
			wantMatch: true,
			wantRetry: true,
		},
		{
			name:      "non-sendRaw method is matched but terminal",
			method:    "eth_call",
			message:   "An existing transaction had higher priority",
			wantMatch: true,
			wantRetry: false,
		},
		{
			name:      "higher priority fee suggestion is not reclassified",
			method:    "eth_sendRawTransaction",
			message:   "try a higher priority fee",
			wantMatch: false,
		},
		{
			name:      "rejected as substring is not reclassified",
			method:    "eth_sendRawTransaction",
			message:   "transaction rejected by policy",
			wantMatch: false,
		},
		{
			name:      "pool full stays a server-side (breaker-visible) error",
			method:    "eth_sendRawTransaction",
			message:   "Transaction pool is full",
			wantMatch: false,
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
			jrErr := common.NewErrJsonRpcExceptionExternal(-32603, tc.message, "")
			jrErr.Data = tc.data
			jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

			err := ExtractJsonRpcError(r, nr, jr, nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tc.wantMatch {
				if common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
					t.Fatalf("expected not ErrEndpointServerSideException, got %v", err)
				}
				if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
					t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
				}
				if got := common.IsRetryableTowardNetwork(err); got != tc.wantRetry {
					t.Fatalf("IsRetryableTowardNetwork = %v, want %v (err: %v)", got, tc.wantRetry, err)
				}
			} else {
				if common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
					t.Fatalf("expected not ErrEndpointExecutionException for %q, got %v", tc.message, err)
				}
				if !common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
					t.Fatalf("expected ErrEndpointServerSideException fallback for %q, got %T: %v", tc.message, err, err)
				}
			}
		})
	}
}

// TestExtractJsonRpcError_ReplayAttackIdempotency verifies that a re-submission
// of an already-accepted transaction rejected with a "replay attack" error is
// normalized to ErrEndpointNonceException with reason "already known", so
// eth_sendRawTransaction idempotency handling can convert it to success.
func TestExtractJsonRpcError_ReplayAttackIdempotency(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		message string
	}{
		{
			name:    "uppercase errmsg token",
			message: "errcode: 113, errmsg: TX_REPLAY_ATTACK",
		},
		{
			name:    "spaced phrasing",
			message: "transaction rejected: replay attack detected",
		},
	}

	for _, tc := range cases {
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
			if !common.HasErrorCode(err, common.ErrCodeEndpointNonceException) {
				t.Fatalf("expected ErrEndpointNonceException, got %T: %v", err, err)
			}
			var ne *common.ErrEndpointNonceException
			if !errors.As(err, &ne) {
				t.Fatalf("expected *common.ErrEndpointNonceException in chain, got %T", err)
			}
			if got := ne.Details["nonceExceptionReason"]; got != string(common.NonceExceptionReasonAlreadyKnown) {
				t.Fatalf("expected reason %q, got %v", common.NonceExceptionReasonAlreadyKnown, got)
			}
		})
	}
}
