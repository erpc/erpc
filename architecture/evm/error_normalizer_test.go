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
		{
			// Aurora's engine verdict for the mined tx; every Aurora node repeats it.
			name:          "debug_traceTransaction Aurora ERR_OUT_OF_FUND stays non-retried",
			method:        "debug_traceTransaction",
			message:       `Error: EVM tracing failed in transaction 0xe373b20df8aa743e52c77d8aae113810157af9e330c8f4165cce7f2f4f759a80 due to ContractError { message: "ERR_OUT_OF_FUND" }`,
			wantRetryable: false,
		},
		{
			name:          "debug_traceTransaction Aurora NEAR-native tx stays non-retried",
			method:        "debug_traceTransaction",
			message:       "Error: no EVM execution in transaction 0x37c6ca677fb7d9f6fe0d53a72288d89fb37b9483ff90950684de6e697e013a00",
			wantRetryable: false,
		},
		{
			// Aurora engine system call (to 0x4444…192b) the tracer has no EVM record of.
			name:          "debug_traceTransaction Aurora TransactionHashNotFound stays non-retried",
			method:        "debug_traceTransaction",
			message:       "StandaloneError(TransactionHashNotFound(0x1e5f1daaab9c1e10582a8defeb63f0716e8e1071c8a0327e973ff47732f50141))",
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

// TestExtractJsonRpcError_MonadReserveBalanceViolation verifies that Monad's
// `reserve balance violation` — its wording for a sender that cannot cover the
// call, returned as -32000 by eth_call and eth_fillTransaction (where it
// replaced "insufficient balance") — is an execution outcome, not an endpoint
// failure. As a server-side exception every upstream returns the same verdict
// and the network retry policy exhausts all of them for one simulation.
func TestExtractJsonRpcError_MonadReserveBalanceViolation(t *testing.T) {
	t.Parallel()

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`))
	nr := common.NewNormalizedResponse().WithRequest(req)

	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jrErr := common.NewErrJsonRpcExceptionExternal(-32000, "reserve balance violation", "")
	jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

	err := ExtractJsonRpcError(r, nr, jr, nil)
	if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
		t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
	}
	if common.IsRetryableTowardNetwork(err) {
		t.Fatalf("eth_call reserve balance violation must not be retried toward the network")
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

// HyperEVM embeds 10055 in the message ("Unexpected error (code=10055)"), not
// the JSON-RPC code field (which is typically -32100). Scoped to
// eth_sendRawTransaction only: normalize to nonce-too-low (on-chain verify)
// rather than already-known (blind success), because the same code also covers
// insufficient funds / underpriced. Other methods must keep failover.
func TestExtractJsonRpcError_HyperEVMCode10055(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		method  string
		code    int
		message string
		wantNTL bool
	}{
		{
			name:    "sendRawTransaction observed HyperEVM wire message",
			method:  "eth_sendRawTransaction",
			code:    -32100,
			message: "Unexpected error (code=10055)",
			wantNTL: true,
		},
		{
			name:    "sendRawTransaction code=10055 with other wrapper wording",
			method:  "eth_sendRawTransaction",
			code:    -32603,
			message: "rpc error: code=10055 msg=boundary",
			wantNTL: true,
		},
		{
			name:    "eth_call keeps server-side / failover behavior",
			method:  "eth_call",
			code:    -32100,
			message: "Unexpected error (code=10055)",
			wantNTL: false,
		},
		{
			name:    "nil request does not match",
			method:  "",
			code:    -32100,
			message: "Unexpected error (code=10055)",
			wantNTL: false,
		},
		{
			name:    "unrelated 10055 digit sequence does not match",
			method:  "eth_sendRawTransaction",
			code:    -32000,
			message: "internal error reference 10055",
			wantNTL: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var nr *common.NormalizedResponse
			if tc.method != "" {
				req := common.NewNormalizedRequest([]byte(
					`{"jsonrpc":"2.0","method":"` + tc.method + `","params":[],"id":1}`))
				nr = common.NewNormalizedResponse().WithRequest(req)
			}

			r := &http.Response{StatusCode: 200, Header: http.Header{}}
			jrErr := common.NewErrJsonRpcExceptionExternal(tc.code, tc.message, "")
			jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

			err := ExtractJsonRpcError(r, nr, jr, nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var ne *common.ErrEndpointNonceException
			isNTL := errors.As(err, &ne) &&
				ne.Details["nonceExceptionReason"] == string(common.NonceExceptionReasonNonceTooLow)
			if isNTL != tc.wantNTL {
				t.Fatalf("expected nonceTooLow=%v, got %T: %v", tc.wantNTL, err, err)
			}
			if tc.wantNTL && common.HasErrorCode(err, common.ErrCodeEndpointServerSideException) {
				t.Fatalf("10055 must not fall through to server-side exception: %v", err)
			}
		})
	}
}

// A TX_REPLAY_ATTACK rejection of an already-accepted transaction normalizes
// to an "already known" nonce exception; other "replay attack" wording does not.
func TestExtractJsonRpcError_ReplayAttackIdempotency(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		message      string
		alreadyKnown bool
	}{
		{
			name:         "observed errmsg token",
			message:      "errcode: 113, errmsg: TX_REPLAY_ATTACK",
			alreadyKnown: true,
		},
		{
			name:    "other replay attack wording",
			message: "replay attack detected",
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
			var ne *common.ErrEndpointNonceException
			isAlreadyKnown := errors.As(err, &ne) &&
				ne.Details["nonceExceptionReason"] == string(common.NonceExceptionReasonAlreadyKnown)
			if isAlreadyKnown != tc.alreadyKnown {
				t.Fatalf("expected alreadyKnown=%v, got %T: %v", tc.alreadyKnown, err, err)
			}
		})
	}
}

// TestExtractJsonRpcError_RethRevertError covers reth's Debug-formatted revert,
// returned as -32603 with the revert bytes only in the message (seen on HyperEVM
// nodes for eth_estimateGas pinned to a block number or "pending"). It is an
// execution outcome: code 3, revert bytes in data, never retried or scored as an
// upstream failure.
func TestExtractJsonRpcError_RethRevertError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		method   string
		message  string
		wantData interface{}
	}{
		{
			name:     "estimateGas custom error bytes",
			method:   "eth_estimateGas",
			message:  "Failed to estimate gas: InvalidTransaction(Revert(RevertError { output: Some(0x756688fe) }))",
			wantData: "0x756688fe",
		},
		{
			name:     "eth_call revert without output",
			method:   "eth_call",
			message:  "InvalidTransaction(Revert(RevertError { output: None }))",
			wantData: nil,
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
			jrErr.Data = nil
			jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

			err := ExtractJsonRpcError(r, nr, jr, nil)
			if !common.HasErrorCode(err, common.ErrCodeEndpointExecutionException) {
				t.Fatalf("expected ErrEndpointExecutionException, got %T: %v", err, err)
			}
			if common.IsRetryableTowardNetwork(err) {
				t.Fatalf("revert must not be retried toward the network")
			}
			jre := &common.ErrJsonRpcExceptionInternal{}
			if !errors.As(err, &jre) {
				t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T", err)
			}
			if got := jre.NormalizedCode(); got != common.JsonRpcErrorEvmReverted {
				t.Fatalf("normalized code: got %d, want %d", got, common.JsonRpcErrorEvmReverted)
			}
			if got := jre.Details["data"]; got != tc.wantData {
				t.Fatalf("data: got %v, want %v", got, tc.wantData)
			}
		})
	}
}

// hl-node's reply when a HyperEVM read precompile (0x…0800 onwards) runs
// against a block for which the node holds no HyperCore state. The trailing
// number is the node's RPC gas cap and differs per provider.
const hyperEvmPrecompileStateGap = "out of gas: gas exhausted during precompiled contract execution: 600000000"

// TestExtractJsonRpcError_HyperEvmPrecompileStateGap verifies that the
// precompile-state gap is classified as missing data, so the network sweep
// moves on to an upstream that holds the state. A generic "out of gas" stays a
// final execution result, and eth_sendRawTransaction keeps its existing
// transaction-rejected handling.
func TestExtractJsonRpcError_HyperEvmPrecompileStateGap(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		method          string // empty = no request attached to the response
		message         string
		wantCode        common.ErrorCode
		wantNormalized  common.JsonRpcErrorNumber
		wantRetryableNw bool
	}{
		{name: "eth_call", method: "eth_call", message: hyperEvmPrecompileStateGap,
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "eth_estimateGas", method: "eth_estimateGas", message: hyperEvmPrecompileStateGap,
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "eth_createAccessList", method: "eth_createAccessList", message: hyperEvmPrecompileStateGap,
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "eth_simulateV1", method: "eth_simulateV1", message: hyperEvmPrecompileStateGap,
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "debug_traceCall", method: "debug_traceCall", message: hyperEvmPrecompileStateGap,
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "unknown method", method: "hl_someFutureCall", message: hyperEvmPrecompileStateGap,
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "no request attached", method: "", message: hyperEvmPrecompileStateGap,
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "other gas cap", method: "eth_call", message: "out of gas: gas exhausted during precompiled contract execution: 50000000",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "plain out of gas stays a final execution result", method: "eth_call", message: "out of gas",
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorTransactionRejected, wantRetryableNw: false},
		{name: "eth_sendRawTransaction keeps transaction-rejected handling", method: "eth_sendRawTransaction", message: hyperEvmPrecompileStateGap,
			wantCode: common.ErrCodeEndpointExecutionException, wantNormalized: common.JsonRpcErrorTransactionRejected, wantRetryableNw: true},
		// Pruned-state answers from Alchemy (base eth_getProof) and Nirvana (base eth_getCode/eth_call):
		// another archive upstream can serve them, so fail over instead of retrying as a server fault.
		{name: "eth_getProof no state found", method: "eth_getProof", message: "no state found for block number 52938851",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
		{name: "eth_getCode state pruned", method: "eth_getCode", message: "state at block #51672341 is pruned",
			wantCode: common.ErrCodeEndpointMissingData, wantNormalized: common.JsonRpcErrorMissingData, wantRetryableNw: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var nr *common.NormalizedResponse
			if tc.method != "" {
				req := common.NewNormalizedRequest([]byte(
					`{"jsonrpc":"2.0","method":"` + tc.method + `","params":[],"id":1}`))
				nr = common.NewNormalizedResponse().WithRequest(req)
			}

			r := &http.Response{StatusCode: 200, Header: http.Header{}}
			jrErr := common.NewErrJsonRpcExceptionExternal(-32003, tc.message, "")
			jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

			err := ExtractJsonRpcError(r, nr, jr, nil)
			if !common.HasErrorCode(err, tc.wantCode) {
				t.Fatalf("expected %s, got %T: %v", tc.wantCode, err, err)
			}
			if got := common.IsRetryableTowardNetwork(err); got != tc.wantRetryableNw {
				t.Fatalf("IsRetryableTowardNetwork: got %v, want %v", got, tc.wantRetryableNw)
			}
			jre := &common.ErrJsonRpcExceptionInternal{}
			if !errors.As(err, &jre) {
				t.Fatalf("expected ErrJsonRpcExceptionInternal in chain, got %T", err)
			}
			if got := jre.NormalizedCode(); got != tc.wantNormalized {
				t.Fatalf("normalized code: got %d, want %d", got, tc.wantNormalized)
			}
			if got := jre.Message; got != tc.message {
				t.Fatalf("node message must reach the caller verbatim: got %q, want %q", got, tc.message)
			}
		})
	}
}

func TestExtractJsonRpcError_TooManySubscriptions_IsCapacityExceeded(t *testing.T) {
	t.Parallel()

	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jrErr := common.NewErrJsonRpcExceptionExternal(
		-32006,
		"Too many subscriptions on the connection",
		"Exceeded max limit of 32",
	)
	jr := common.MustNewJsonRpcResponse(1, nil, jrErr)

	err := ExtractJsonRpcError(r, nil, jr, nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded) {
		t.Fatalf("expected ErrEndpointCapacityExceeded, got %T: %v", err, err)
	}
}
