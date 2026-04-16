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
