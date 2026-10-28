package evm

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	bdscommon "github.com/blockchain-data-standards/manifesto/common"
	"github.com/erpc/erpc/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covErrNormInternal returns the ErrJsonRpcExceptionInternal carried in err's chain.
func covErrNormInternal(t *testing.T, err error) *common.ErrJsonRpcExceptionInternal {
	t.Helper()
	jre := &common.ErrJsonRpcExceptionInternal{}
	if !errors.As(err, &jre) {
		t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T: %v", err, err)
	}
	return jre
}

func covErrNormResponse(method string) *common.NormalizedResponse {
	if method == "" {
		return nil
	}
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","method":"` + method + `","params":[],"id":1}`))
	return common.NewNormalizedResponse().WithRequest(req)
}

// HTTP failures without a JSON-RPC error body are classified by status code alone.
func TestCovErrNorm_HttpStatusWithoutJsonRpcError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		status         int
		method         string
		wantCode       common.ErrorCode
		wantNormalized common.JsonRpcErrorNumber
		wantMessage    string
	}{
		{name: "500 is server-side", status: 500, wantCode: common.ErrCodeEndpointServerSideException,
			wantNormalized: common.JsonRpcErrorServerSideException, wantMessage: "unexpected http failure with status code 500"},
		{name: "503 is server-side", status: 503, wantCode: common.ErrCodeEndpointServerSideException,
			wantNormalized: common.JsonRpcErrorServerSideException, wantMessage: "unexpected http failure with status code 503"},
		{name: "429 is capacity exceeded", status: 429, wantCode: common.ErrCodeEndpointCapacityExceeded,
			wantNormalized: common.JsonRpcErrorCapacityExceeded, wantMessage: "unexpected http failure with status code 429"},
		{name: "402 is billing", status: 402, wantCode: common.ErrCodeEndpointBillingIssue,
			wantNormalized: common.JsonRpcErrorCapacityExceeded, wantMessage: "unexpected http failure with status code 402"},
		{name: "401 is unauthorized", status: 401, wantCode: common.ErrCodeEndpointUnauthorized,
			wantNormalized: common.JsonRpcErrorUnauthorized, wantMessage: "unexpected http failure with status code 401"},
		{name: "403 is unauthorized", status: 403, wantCode: common.ErrCodeEndpointUnauthorized,
			wantNormalized: common.JsonRpcErrorUnauthorized, wantMessage: "unexpected http failure with status code 403"},
		{name: "405 is unsupported", status: 405, method: "eth_getProof", wantCode: common.ErrCodeEndpointUnsupported,
			wantNormalized: common.JsonRpcErrorUnsupportedException, wantMessage: "The method eth_getProof does not exist/is not available"},
		{name: "415 is unsupported without request", status: 415, wantCode: common.ErrCodeEndpointUnsupported,
			wantNormalized: common.JsonRpcErrorUnsupportedException, wantMessage: "The method  does not exist/is not available"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			err := ExtractJsonRpcError(r, covErrNormResponse(tc.method), nil, nil)
			if !common.HasErrorCode(err, tc.wantCode) {
				t.Fatalf("expected %s, got %T: %v", tc.wantCode, err, err)
			}
			jre := covErrNormInternal(t, err)
			if got := jre.NormalizedCode(); got != tc.wantNormalized {
				t.Fatalf("normalized code: got %d, want %d", got, tc.wantNormalized)
			}
			if got := jre.OriginalCode(); got != int(common.JsonRpcErrorServerSideException) {
				t.Fatalf("original code: got %d, want %d", got, common.JsonRpcErrorServerSideException)
			}
			if jre.Message != tc.wantMessage {
				t.Fatalf("message: got %q, want %q", jre.Message, tc.wantMessage)
			}
			if got := jre.Details["statusCode"]; got != tc.status {
				t.Fatalf("details.statusCode: got %v, want %d", got, tc.status)
			}
		})
	}
}

// The upstream status code is preserved on server-side exceptions.
func TestCovErrNorm_ServerSidePreservesStatusCode(t *testing.T) {
	t.Parallel()

	r := &http.Response{StatusCode: 502, Header: http.Header{}}
	err := ExtractJsonRpcError(r, nil, nil, nil)
	sc, ok := err.(interface{ ErrorStatusCode() int })
	if !ok {
		t.Fatalf("expected ErrorStatusCode on %T", err)
	}
	if sc.ErrorStatusCode() != 502 {
		t.Fatalf("ErrorStatusCode: got %d, want 502", sc.ErrorStatusCode())
	}
}

// The "data" field is folded into details: Alchemy's "Reverted " prefix is
// stripped, structured data passes through as-is, and both feed text checks.
func TestCovErrNorm_DataFieldShapes(t *testing.T) {
	t.Parallel()

	const revertData = "0x08c379a00000000000000000000000000000000000000000000000000000000000000020"

	t.Run("alchemy Reverted prefix is stripped", func(t *testing.T) {
		t.Parallel()
		r := &http.Response{StatusCode: 200, Header: http.Header{}}
		jr := common.MustNewJsonRpcResponse(1, nil,
			common.NewErrJsonRpcExceptionExternal(3, "execution reverted", "Reverted "+revertData))
		err := ExtractJsonRpcError(r, covErrNormResponse("eth_call"), jr, nil)
		if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
			t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
		}
		jre := covErrNormInternal(t, err)
		if jre.NormalizedCode() != common.JsonRpcErrorEvmReverted {
			t.Fatalf("normalized code: got %d", jre.NormalizedCode())
		}
		if got := jre.Details["data"]; got != revertData {
			t.Fatalf("details.data: got %v, want %s", got, revertData)
		}
		if common.IsRetryableTowardNetwork(err) {
			t.Fatalf("eth_call revert must not be retried toward the network")
		}
	})

	t.Run("plain string data is kept verbatim", func(t *testing.T) {
		t.Parallel()
		r := &http.Response{StatusCode: 200, Header: http.Header{}}
		jr := common.MustNewJsonRpcResponse(1, nil,
			common.NewErrJsonRpcExceptionExternal(3, "execution reverted", revertData))
		err := ExtractJsonRpcError(r, nil, jr, nil)
		jre := covErrNormInternal(t, err)
		if got := jre.Details["data"]; got != revertData {
			t.Fatalf("details.data: got %v, want %s", got, revertData)
		}
	})

	t.Run("structured data passes through and is text-matched", func(t *testing.T) {
		t.Parallel()
		r := &http.Response{StatusCode: 200, Header: http.Header{}}
		jrErr := common.NewErrJsonRpcExceptionExternal(-32000, "call failed", "")
		dataMap := map[string]interface{}{"reason": "header not found"}
		jrErr.Data = dataMap
		jr := common.MustNewJsonRpcResponse(1, nil, jrErr)
		err := ExtractJsonRpcError(r, nil, jr, nil)
		// The message alone matches nothing; the "header not found" lives only in data.
		if !common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
			t.Fatalf("expected ErrEndpointMissingData from data text, got %T: %v", err, err)
		}
		jre := covErrNormInternal(t, err)
		got, ok := jre.Details["data"].(map[string]interface{})
		if !ok || got["reason"] != "header not found" {
			t.Fatalf("details.data must pass the map through, got %#v", jre.Details["data"])
		}
		if jre.Message != "call failed" {
			t.Fatalf("message must stay the vendor message, got %q", jre.Message)
		}
	})

	t.Run("data parsed from raw error body", func(t *testing.T) {
		t.Parallel()
		r := &http.Response{StatusCode: 200, Header: http.Header{}}
		jr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), nil,
			[]byte(`{"code":-32600,"message":"Invalid Request","data":{"message":"Cancelled due to validation errors in batch request"}}`))
		err := ExtractJsonRpcError(r, nil, jr, nil)
		if !common.HasErrorCode(err, common.ErrCodeEndpointClientSideException) {
			t.Fatalf("expected ErrEndpointClientSideException, got %T: %v", err, err)
		}
		jre := covErrNormInternal(t, err)
		if jre.NormalizedCode() != common.JsonRpcErrorCallException {
			t.Fatalf("normalized code: got %d, want %d", jre.NormalizedCode(), common.JsonRpcErrorCallException)
		}
		if !common.IsRetryableTowardNetwork(err) {
			t.Fatalf("batch validation cancellation must stay retryable so the batch can be split")
		}
	})
}

// Table of JSON-RPC error code/message → erpc classification for branches not
// exercised by error_normalizer_test.go.
func TestCovErrNorm_MessageClassification(t *testing.T) {
	t.Parallel()

	type tcase struct {
		name           string
		status         int
		code           int
		message        string
		data           interface{}
		method         string
		wantCode       common.ErrorCode
		wantNormalized common.JsonRpcErrorNumber
		wantMessage    string // empty: vendor message verbatim
		wantRetryNw    bool
	}
	cases := []tcase{
		// Range-too-large.
		{name: "alchemy block range hint", code: -32602, message: "Log response size exceeded. You can make eth_getLogs requests with up to a 2K block range and no limit on the response size, or you can request any block range with a cap of 10K logs in the response. Based on your parameters, this block range should work: [0x1, 0x2]",
			wantCode: common.ErrCodeEndpointRequestTooLarge, wantNormalized: common.JsonRpcErrorEvmLargeRange, wantMessage: "request exceeded max allowed range: Log response size exceeded. You can make eth_getLogs requests with up to a 2K block range and no limit on the response size, or you can request any block range with a cap of 10K logs in the response. Based on your parameters, this block range should work: [0x1, 0x2]", wantRetryNw: true},
		{name: "maximum blocks distance", code: -32000, message: "maximum 10000 blocks distance between fromBlock and toBlock",
			wantCode: common.ErrCodeEndpointRequestTooLarge, wantNormalized: common.JsonRpcErrorEvmLargeRange, wantMessage: "request exceeded max allowed range: maximum 10000 blocks distance between fromBlock and toBlock", wantRetryNw: true},

		// Billing vs capacity.
		{name: "alchemy monthly capacity is billing", code: 429, message: "Monthly capacity limit exceeded. Visit https://dashboard.alchemyapi.io/settings/billing to upgrade your scaling policy for continued service.",
			wantCode: common.ErrCodeEndpointBillingIssue, wantNormalized: common.JsonRpcErrorCapacityExceeded, wantRetryNw: true},
		{name: "free tier is billing", code: -32000, message: "You have reached the free tier daily limit",
			wantCode: common.ErrCodeEndpointBillingIssue, wantNormalized: common.JsonRpcErrorCapacityExceeded, wantRetryNw: true},
		{name: "rate limit message", code: -32005, message: "rate limit exceeded",
			wantCode: common.ErrCodeEndpointCapacityExceeded, wantNormalized: common.JsonRpcErrorCapacityExceeded, wantRetryNw: true},
		{name: "429 status with generic message", status: 429, code: -32000, message: "slow down",
			wantCode: common.ErrCodeEndpointCapacityExceeded, wantNormalized: common.JsonRpcErrorCapacityExceeded, wantRetryNw: true},

		// Block tags.
		{name: "pending block not available", code: -32000, message: "pending block is not available",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorClientSideException, wantRetryNw: true},
		{name: "finalized commitment unsupported", code: -32602, message: "finalized is not a supported commitment",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorClientSideException, wantRetryNw: true},

		// Known missing data.
		{name: "missing trie node", code: -32000, message: "missing trie node 0000000000000000000000000000000000000000000000000000000000000000 (path ) <nil>",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryNw: true},
		{name: "required historical state", code: -32000, message: "required historical state is not available",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryNw: true},

		// Node timeout.
		{name: "execution timeout", code: -32000, message: "execution timeout",
			wantCode: common.ErrCodeEndpointServerSideException, wantNormalized: common.JsonRpcErrorNodeTimeout, wantRetryNw: true},

		// Reverts.
		{name: "VM Exception", code: -32000, message: "VM Exception while processing transaction: revert",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorEvmReverted},
		{name: "intrinsic gas too high (case-insensitive)", code: -32000, message: "Intrinsic Gas Too High",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorEvmReverted},
		{name: "revert on sendRawTransaction is retried", code: 3, message: "execution reverted", method: "eth_sendRawTransaction",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorEvmReverted, wantRetryNw: true},

		// EVM halts ("EVM error: <HaltReason>").
		{name: "berachain InvalidJump", code: -32000, message: "EVM error: InvalidJump", method: "eth_call",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorEvmReverted, wantMessage: "revert: invalid jump destination"},
		{name: "revm OutOfGas halt", code: -32603, message: "EVM error: OutOfGas(Basic)", method: "eth_estimateGas",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorEvmReverted, wantMessage: "revert: OutOfGas(Basic)"},
		{name: "halt reason trimmed", code: -32603, message: "EVM error: StackOverflow  ",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorEvmReverted, wantMessage: "revert: StackOverflow"},
		{name: "halt on sendRawTransaction is retried", code: -32603, message: "EVM error: InvalidFEOpcode", method: "eth_sendRawTransaction",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorEvmReverted, wantMessage: "revert: InvalidFEOpcode", wantRetryNw: true},

		// Nonce / duplicate.
		{name: "geth already known", code: -32000, message: "already known", method: "eth_sendRawTransaction",
			wantCode: common.ErrCodeEndpointNonceException, wantNormalized: common.JsonRpcErrorTransactionRejected},
		{name: "nonce too low", code: -32000, message: "nonce too low: next nonce 5, tx nonce 4", method: "eth_sendRawTransaction",
			wantCode: common.ErrCodeEndpointNonceException, wantNormalized: common.JsonRpcErrorTransactionRejected},

		// Transaction rejected / out of gas.
		{name: "-32003 code", code: -32003, message: "transaction underpriced", method: "eth_sendRawTransaction",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorTransactionRejected, wantRetryNw: true},
		{name: "gas too low on eth_call", code: -32000, message: "intrinsic gas too low", method: "eth_call",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorTransactionRejected},

		// Not found / disabled.
		{name: "method not found", code: -32601, message: "Method not found",
			wantCode: common.ErrCodeEndpointUnsupported, wantNormalized: common.JsonRpcErrorUnsupportedException, wantRetryNw: true},
		{name: "module disabled", code: -32000, message: "debug module is disabled",
			wantCode: common.ErrCodeEndpointUnsupported, wantNormalized: common.JsonRpcErrorUnsupportedException, wantRetryNw: true},
		{name: "block does not exist", code: -32000, message: "this block does not exist",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryNw: true},
		{name: "Block status not available", code: -32014, message: "Block status not available",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryNw: true},
		{name: "Transaction history not available", code: -32011, message: "Transaction history is not available from this node",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryNw: true},
		{name: "state not available", code: -32000, message: "state root not found",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryNw: true},
		{name: "tenderly bare not found", code: -32000, message: "not found",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorClientSideException, wantRetryNw: true},
		{name: "Project not found", code: -32000, message: "Project not found",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorClientSideException, wantRetryNw: true},
		{name: "data not available yet", code: -32000, message: "data not available yet",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorClientSideException, wantRetryNw: true},

		// Unsupported.
		{name: "Unsupported method", code: -32000, message: "Unsupported method: trace_block", method: "trace_block",
			wantCode: common.ErrCodeEndpointUnsupported, wantNormalized: common.JsonRpcErrorUnsupportedException, wantMessage: "The method trace_block does not exist/is not available", wantRetryNw: true},
		{name: "-32004 code", code: -32004, message: "nope", method: "eth_getProof",
			wantCode: common.ErrCodeEndpointUnsupported, wantNormalized: common.JsonRpcErrorUnsupportedException, wantMessage: "The method eth_getProof does not exist/is not available", wantRetryNw: true},
		{name: "plan restriction", code: -32000, message: "trace_filter is not included in your current plan", method: "trace_filter",
			wantCode: common.ErrCodeEndpointUnsupported, wantNormalized: common.JsonRpcErrorUnsupportedException, wantMessage: "The method trace_filter does not exist/is not available", wantRetryNw: true},
		{name: "transaction type not supported", code: -32000, message: "transaction type not supported", method: "eth_sendRawTransaction",
			wantCode: common.ErrCodeEndpointUnsupported, wantNormalized: common.JsonRpcErrorUnsupportedException, wantMessage: "The method eth_sendRawTransaction does not exist/is not available", wantRetryNw: true},

		// Malformed transactions.
		{name: "typed transaction too short", code: -32000, message: "typed transaction too short", method: "eth_sendRawTransaction",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorInvalidArgument},
		{name: "rlp expected input list", code: -32000, message: "rlp: expected input list for types.LegacyTx", method: "eth_sendRawTransaction",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorInvalidArgument},
		{name: "rlp value size exceeds", code: -32000, message: "rlp: value size exceeds available input length",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorInvalidArgument},
		{name: "invalid transaction", code: -32000, message: "Invalid transaction signature",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorInvalidArgument},

		// Retryable argument-shape errors.
		{name: "tx of type", code: -32000, message: "tx of type 4 is not allowed",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorUnsupportedException, wantRetryNw: true},
		{name: "envio block id map", code: -32000, message: "invalid type: map, expected BlockNumber, 'latest', or 'earliest'",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorUnsupportedException, wantRetryNw: true},
		{name: "generic -32602", code: -32602, message: "bad argument shape",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorInvalidArgument, wantRetryNw: true},

		// -32600 that is not a batch-validation cancellation falls through to the server-side fallback.
		{name: "-32600 with unrelated data map", code: -32600, message: "oops", data: map[string]interface{}{"message": "something else"},
			wantCode: common.ErrCodeEndpointServerSideException, wantNormalized: common.JsonRpcErrorClientSideException, wantRetryNw: true},
		{name: "-32600 with string data", code: -32600, message: "oops", data: "plain",
			wantCode: common.ErrCodeEndpointServerSideException, wantNormalized: common.JsonRpcErrorClientSideException, wantRetryNw: true},
		{name: "-32600 without inner message", code: -32600, message: "oops", data: map[string]interface{}{"other": "x"},
			wantCode: common.ErrCodeEndpointServerSideException, wantNormalized: common.JsonRpcErrorClientSideException, wantRetryNw: true},

		// Non-retryable user mistakes.
		{name: "param is required", code: -32000, message: "fromBlock param is required",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorInvalidArgument},
		{name: "invalid argument", code: -32000, message: "invalid argument 0: hex string has length 3, want 40 for common.Address",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorInvalidArgument},
		{name: "Bad request input parameters", code: -32000, message: "Bad request input parameters",
			wantCode: common.ErrCodeEndpointClientSideException, wantNormalized: common.JsonRpcErrorInvalidArgument},

		// Unauthorized.
		{name: "invalid api key", code: -32000, message: "invalid api key provided",
			wantCode: common.ErrCodeEndpointUnauthorized, wantNormalized: common.JsonRpcErrorUnauthorized, wantRetryNw: true},
		{name: "key is inactive", code: -32000, message: "API key is inactive",
			wantCode: common.ErrCodeEndpointUnauthorized, wantNormalized: common.JsonRpcErrorUnauthorized, wantRetryNw: true},
		{name: "403 status with jsonrpc error", status: 403, code: -32000, message: "forbidden",
			wantCode: common.ErrCodeEndpointUnauthorized, wantNormalized: common.JsonRpcErrorUnauthorized, wantRetryNw: true},

		// Fallback keeps the vendor code as the normalized code.
		{name: "unclassified error falls back to server-side", code: -32099, message: "something exploded",
			wantCode: common.ErrCodeEndpointServerSideException, wantNormalized: common.JsonRpcErrorNumber(-32099), wantRetryNw: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status := tc.status
			if status == 0 {
				status = 200
			}
			r := &http.Response{StatusCode: status, Header: http.Header{}}
			jrErr := common.NewErrJsonRpcExceptionExternal(tc.code, tc.message, "")
			if tc.data != nil {
				jrErr.Data = tc.data
			}
			jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

			err := ExtractJsonRpcError(r, covErrNormResponse(tc.method), jr, nil)
			if !common.HasErrorCode(err, tc.wantCode) {
				t.Fatalf("expected %s, got %T: %v", tc.wantCode, err, err)
			}
			jre := covErrNormInternal(t, err)
			if got := jre.NormalizedCode(); got != tc.wantNormalized {
				t.Fatalf("normalized code: got %d, want %d", got, tc.wantNormalized)
			}
			if got := jre.OriginalCode(); got != tc.code {
				t.Fatalf("original code: got %d, want %d", got, tc.code)
			}
			wantMsg := tc.wantMessage
			if wantMsg == "" {
				wantMsg = tc.message
			}
			if jre.Message != wantMsg {
				t.Fatalf("message: got %q, want %q", jre.Message, wantMsg)
			}
			if got := common.IsRetryableTowardNetwork(err); got != tc.wantRetryNw {
				t.Fatalf("IsRetryableTowardNetwork: got %v, want %v", got, tc.wantRetryNw)
			}
		})
	}
}

// Unsupported errors keep the raw vendor text for diagnostics.
func TestCovErrNorm_UnsupportedKeepsVendorMessage(t *testing.T) {
	t.Parallel()

	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, nil,
		common.NewErrJsonRpcExceptionExternal(-32000, "the method is not whitelisted", ""))
	err := ExtractJsonRpcError(r, covErrNormResponse("debug_traceBlockByNumber"), jr, nil)
	jre := covErrNormInternal(t, err)
	if got := jre.Details["vendorMessage"]; got != "the method is not whitelisted" {
		t.Fatalf("details.vendorMessage: got %v", got)
	}
}

// 200 OK responses whose result is an Error(string) ABI payload are reverts.
func TestCovErrNorm_RevertPayloadInResult(t *testing.T) {
	t.Parallel()

	const payload = "0x08c379a0000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000000046e6f706500000000000000000000000000000000000000000000000000000000"

	cases := []struct {
		name    string
		method  string
		result  string
		wantErr bool
	}{
		{name: "Error(string) selector", method: "eth_call", result: `"` + payload + `"`, wantErr: true},
		{name: "ordinary hex result", method: "eth_call", result: `"0x0000000000000000000000000000000000000000000000000000000000000001"`, wantErr: false},
		{name: "trace result without timeout", method: "trace_block", result: `[{"action":{}}]`, wantErr: false},
		{name: "short result", method: "eth_blockNumber", result: `"0x1"`, wantErr: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &http.Response{StatusCode: 200, Header: http.Header{}}
			jr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), []byte(tc.result), nil)
			err := ExtractJsonRpcError(r, covErrNormResponse(tc.method), jr, nil)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
				t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
			}
			jre := covErrNormInternal(t, err)
			if jre.NormalizedCode() != common.JsonRpcErrorEvmReverted {
				t.Fatalf("normalized code: got %d", jre.NormalizedCode())
			}
			if jre.Message != "transaction reverted" {
				t.Fatalf("message: got %q", jre.Message)
			}
			raw, ok := jre.Details["data"].(json.RawMessage)
			if !ok || string(raw) != tc.result {
				t.Fatalf("details.data: got %#v, want raw %s", jre.Details["data"], tc.result)
			}
		})
	}
}

func TestCovErrNorm_GrpcNilAndOk(t *testing.T) {
	t.Parallel()

	if err := ExtractGrpcError(nil, nil); err != nil {
		t.Fatalf("nil status must yield nil, got %v", err)
	}
	if err := ExtractGrpcError(status.New(codes.OK, "fine"), nil); err != nil {
		t.Fatalf("OK status must yield nil, got %v", err)
	}
}

// Plain gRPC status codes (no BDS details) map to erpc error kinds.
func TestCovErrNorm_GrpcStatusCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code           codes.Code
		wantCode       common.ErrorCode
		wantNormalized common.JsonRpcErrorNumber
		wantRetryNw    bool
	}{
		{codes.Unimplemented, common.ErrCodeEndpointUnsupported, common.JsonRpcErrorUnsupportedException, true},
		{codes.InvalidArgument, common.ErrCodeEndpointClientSideException, common.JsonRpcErrorInvalidArgument, false},
		{codes.ResourceExhausted, common.ErrCodeEndpointCapacityExceeded, common.JsonRpcErrorCapacityExceeded, true},
		{codes.DeadlineExceeded, common.ErrCodeEndpointServerSideException, common.JsonRpcErrorNodeTimeout, true},
		{codes.Unauthenticated, common.ErrCodeEndpointUnauthorized, common.JsonRpcErrorUnauthorized, true},
		{codes.PermissionDenied, common.ErrCodeEndpointUnauthorized, common.JsonRpcErrorUnauthorized, true},
		{codes.NotFound, common.ErrCodeEndpointMissingData, common.JsonRpcErrorMissingData, true},
		{codes.OutOfRange, common.ErrCodeEndpointMissingData, common.JsonRpcErrorMissingData, true},
		{codes.Internal, common.ErrCodeEndpointServerSideException, common.JsonRpcErrorServerSideException, true},
		{codes.Unknown, common.ErrCodeEndpointServerSideException, common.JsonRpcErrorServerSideException, true},
		{codes.Unavailable, common.ErrCodeEndpointServerSideException, common.JsonRpcErrorServerSideException, true},
		{codes.Aborted, common.ErrCodeEndpointServerSideException, common.JsonRpcErrorServerSideException, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.code.String(), func(t *testing.T) {
			t.Parallel()
			ups := common.NewFakeUpstream("rpc-grpc-1")
			msg := "bds says " + tc.code.String()
			err := ExtractGrpcError(status.New(tc.code, msg), ups)
			if !common.HasErrorCode(err, tc.wantCode) {
				t.Fatalf("expected %s, got %T: %v", tc.wantCode, err, err)
			}
			jre := covErrNormInternal(t, err)
			if got := jre.NormalizedCode(); got != tc.wantNormalized {
				t.Fatalf("normalized code: got %d, want %d", got, tc.wantNormalized)
			}
			if jre.Message != msg {
				t.Fatalf("message: got %q, want %q", jre.Message, msg)
			}
			if jre.Details["grpcCode"] != tc.code.String() || jre.Details["grpcMessage"] != msg {
				t.Fatalf("grpc details: got %v / %v", jre.Details["grpcCode"], jre.Details["grpcMessage"])
			}
			if jre.Details["upstreamId"] != "rpc-grpc-1" {
				t.Fatalf("upstreamId: got %v", jre.Details["upstreamId"])
			}
			if _, has := jre.Details["bdsErrorCode"]; has {
				t.Fatalf("plain gRPC status must not carry bdsErrorCode")
			}
			if got := common.IsRetryableTowardNetwork(err); got != tc.wantRetryNw {
				t.Fatalf("IsRetryableTowardNetwork: got %v, want %v", got, tc.wantRetryNw)
			}
		})
	}
}

// BDS error details take precedence over the gRPC status code.
func TestCovErrNorm_GrpcBdsErrorCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		bdsCode        bdscommon.ErrorCode
		wantCode       common.ErrorCode
		wantNormalized common.JsonRpcErrorNumber
		wantRetryNw    bool
	}{
		{"unsupported block tag", bdscommon.ErrorCode_UNSUPPORTED_BLOCK_TAG, common.ErrCodeEndpointUnsupported, common.JsonRpcErrorUnsupportedException, true},
		{"unsupported method", bdscommon.ErrorCode_UNSUPPORTED_METHOD, common.ErrCodeEndpointUnsupported, common.JsonRpcErrorUnsupportedException, true},
		{"range outside available", bdscommon.ErrorCode_RANGE_OUTSIDE_AVAILABLE, common.ErrCodeEndpointMissingData, common.JsonRpcErrorMissingData, true},
		{"invalid parameter", bdscommon.ErrorCode_INVALID_PARAMETER, common.ErrCodeEndpointClientSideException, common.JsonRpcErrorInvalidArgument, false},
		{"invalid request", bdscommon.ErrorCode_INVALID_REQUEST, common.ErrCodeEndpointClientSideException, common.JsonRpcErrorInvalidArgument, false},
		{"rate limited", bdscommon.ErrorCode_RATE_LIMITED, common.ErrCodeEndpointCapacityExceeded, common.JsonRpcErrorCapacityExceeded, true},
		{"timeout", bdscommon.ErrorCode_TIMEOUT_ERROR, common.ErrCodeEndpointServerSideException, common.JsonRpcErrorNodeTimeout, true},
		// RANGE_TOO_LARGE maps to gRPC ResourceExhausted; BDS detail must win over capacity-exceeded.
		{"range too large", bdscommon.ErrorCode_RANGE_TOO_LARGE, common.ErrCodeEndpointRequestTooLarge, common.JsonRpcErrorEvmLargeRange, true},
		{"internal", bdscommon.ErrorCode_INTERNAL_ERROR, common.ErrCodeEndpointServerSideException, common.JsonRpcErrorServerSideException, true},
		// DATA_NOT_FOUND has no BDS mapping and falls to gRPC Unknown.
		{"unmapped bds code falls back to grpc code", bdscommon.ErrorCode_DATA_NOT_FOUND, common.ErrCodeEndpointServerSideException, common.JsonRpcErrorServerSideException, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := bdscommon.NewError(tc.bdsCode, "bds failure").
				WithCause(errors.New("disk read failed")).
				WithDetail("fromBlock", "100").
				ToGRPCStatus()

			err := ExtractGrpcError(st, nil)
			if !common.HasErrorCode(err, tc.wantCode) {
				t.Fatalf("expected %s, got %T: %v", tc.wantCode, err, err)
			}
			jre := covErrNormInternal(t, err)
			if got := jre.NormalizedCode(); got != tc.wantNormalized {
				t.Fatalf("normalized code: got %d, want %d", got, tc.wantNormalized)
			}
			if jre.Message != "bds failure" {
				t.Fatalf("message: got %q", jre.Message)
			}
			if jre.Details["upstreamId"] != "n/a" {
				t.Fatalf("upstreamId without upstream: got %v", jre.Details["upstreamId"])
			}
			if jre.Details["bdsErrorCode"] != tc.bdsCode {
				t.Fatalf("bdsErrorCode: got %v, want %v", jre.Details["bdsErrorCode"], tc.bdsCode)
			}
			if jre.Details["cause"] != "disk read failed" {
				t.Fatalf("cause: got %v", jre.Details["cause"])
			}
			// "100" is JSON-decoded by the BDS layer into a number.
			if jre.Details["fromBlock"] != float64(100) {
				t.Fatalf("fromBlock detail: got %#v", jre.Details["fromBlock"])
			}
			if got := common.IsRetryableTowardNetwork(err); got != tc.wantRetryNw {
				t.Fatalf("IsRetryableTowardNetwork: got %v, want %v", got, tc.wantRetryNw)
			}
		})
	}
}

// A BDS error without cause or details still classifies and omits "cause".
func TestCovErrNorm_GrpcBdsWithoutCause(t *testing.T) {
	t.Parallel()

	st := (&bdscommon.BaseError{Code: bdscommon.ErrorCode_RATE_LIMITED, Message: "slow down"}).ToGRPCStatus()
	err := ExtractGrpcError(st, nil)
	if !common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
		t.Fatalf("expected ErrEndpointCapacityExceeded, got %T: %v", err, err)
	}
	jre := covErrNormInternal(t, err)
	if _, has := jre.Details["cause"]; has {
		t.Fatalf("cause must be absent, got %v", jre.Details["cause"])
	}
}
