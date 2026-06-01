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
