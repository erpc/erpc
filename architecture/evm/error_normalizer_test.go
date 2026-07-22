package evm

import (
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
