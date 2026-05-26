package evm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// EIP-1559 signed transaction copied from networks_sendrawtx_test.go fixtures.
// Hash is deterministic from the signed bytes.
const sendRawTxFixture = "0x02f873010a8459682f008506fc23ac0082520894d8da6bf26964af9d7eed9e03e53415d37aa9604588016345785d8a000080c080a0a3d5fd825e582675933b2b6aea774b0454633edb49e94699d6f88d197cd26589a06295b0b43a9e93a3390b308272a65bb063d9f18deb4cb7db5ecf352bf9ba9fe7"
const sendRawTxFixtureHash = "0xb9f61197f9c6c63a6981ba69fb22308469d03a4e013b10bcd69315745110acf7"

// makeSendRawTxRequest builds a NormalizedRequest carrying the canonical fixture.
func makeSendRawTxRequest(t *testing.T) *common.NormalizedRequest {
	t.Helper()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sendRawTxFixture + `"]}`)
	return common.NewNormalizedRequest(body)
}

// makeExhaustedError builds an ErrUpstreamsExhausted with at least one upstream cause,
// matching what the failsafe loop surfaces when every upstream attempt has failed.
func makeExhaustedError() error {
	causes := &sync.Map{}
	causes.Store("u1", errors.New("upstream u1: HTTP 500"))
	causes.Store("u2", errors.New("upstream u2: connection refused"))
	return common.NewErrUpstreamsExhausted(
		nil, // *NormalizedRequest only used for diagnostics
		causes,
		"test-project",
		"evm:8453",
		"eth_sendRawTransaction",
		0, // duration
		6, // attempts
		6, // retries
		0, // hedges
		2, // upstreams
	)
}

// TestNetworkPostForward_eth_sendRawTransaction covers the last-line idempotency
// safeguard: when the failsafe loop has exhausted all upstreams for a tx that
// has nevertheless landed in the network (mempool or chain), erpc must return a
// synthetic success with the tx hash instead of -32603 "all upstream attempts
// failed". This prevents misleading "send failed" errors for txs that actually
// went through but where upstreams returned mis-classified server errors.
func TestNetworkPostForward_eth_sendRawTransaction(t *testing.T) {
	t.Run("exhausted_but_tx_in_network_returns_success", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()

		// Mock the eth_getTransactionByHash verification call: tx IS in the network.
		txObject := []byte(`{"hash":"` + sendRawTxFixtureHash + `","blockNumber":"0x123","from":"0x0","to":"0x0"}`)
		n.On("Forward", mock.Anything, mock.MatchedBy(func(r *common.NormalizedRequest) bool {
			m, _ := r.Method()
			if m != "eth_getTransactionByHash" {
				return false
			}
			// Strengthen the matcher: the probe must carry the expected tx hash
			// in params. A regression in extractTxHashFromSendRawTransaction
			// (e.g. wrong type-N decode) would otherwise be invisible because
			// the mocked response would still fire on method name alone.
			jrpc, jerr := r.JsonRpcRequest()
			if jerr != nil || jrpc == nil || len(jrpc.Params) == 0 {
				return false
			}
			hash, ok := jrpc.Params[0].(string)
			return ok && strings.EqualFold(hash, sendRawTxFixtureHash)
		})).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), txObject, nil),
			),
			nil,
		).Once()

		req := makeSendRawTxRequest(t)
		resp, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, nil, makeExhaustedError(),
		)

		require.NoError(t, err, "exhausted-but-tx-found should yield synthetic success, not error")
		require.NotNil(t, resp)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), sendRawTxFixtureHash, "synthetic result should be the tx hash")
		n.AssertExpectations(t)
	})

	t.Run("exhausted_and_tx_not_in_network_returns_original_error", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()

		// Verification call returns null result (tx not found anywhere).
		n.On("Forward", mock.Anything, mock.MatchedBy(func(r *common.NormalizedRequest) bool {
			m, _ := r.Method()
			if m != "eth_getTransactionByHash" {
				return false
			}
			// Strengthen the matcher: the probe must carry the expected tx hash
			// in params. A regression in extractTxHashFromSendRawTransaction
			// (e.g. wrong type-N decode) would otherwise be invisible because
			// the mocked response would still fire on method name alone.
			jrpc, jerr := r.JsonRpcRequest()
			if jerr != nil || jrpc == nil || len(jrpc.Params) == 0 {
				return false
			}
			hash, ok := jrpc.Params[0].(string)
			return ok && strings.EqualFold(hash, sendRawTxFixtureHash)
		})).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`null`), nil),
			),
			nil,
		).Once()

		origErr := makeExhaustedError()
		req := makeSendRawTxRequest(t)
		resp, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, nil, origErr,
		)

		// When the tx genuinely isn't anywhere, we must not invent success.
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"original exhausted error must propagate when verification confirms absence")
		assert.Nil(t, resp)
		n.AssertExpectations(t)
	})

	t.Run("non_exhausted_error_passes_through_unchanged", func(t *testing.T) {
		n := new(mockNetwork)
		// No Forward expectation — we should not trigger verification for non-exhausted errors.

		// A clean client-side rejection (e.g. insufficient funds) must not be second-guessed.
		clientErr := common.NewErrEndpointExecutionException(
			common.NewErrJsonRpcExceptionInternal(
				int(common.JsonRpcErrorTransactionRejected),
				common.JsonRpcErrorTransactionRejected,
				"insufficient funds",
				nil,
				nil,
			),
		)
		req := makeSendRawTxRequest(t)
		_, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, nil, clientErr,
		)
		require.Error(t, err)
		assert.Equal(t, clientErr, err, "non-exhausted errors should pass through verbatim")
		n.AssertExpectations(t)
	})

	t.Run("no_error_passes_through", func(t *testing.T) {
		n := new(mockNetwork)
		req := makeSendRawTxRequest(t)
		okResp := common.NewNormalizedResponse().WithJsonRpcResponse(
			common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"`+sendRawTxFixtureHash+`"`), nil),
		)
		resp, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, okResp, nil,
		)
		require.NoError(t, err)
		require.NotNil(t, resp)
		// Forward must NOT be called when there's no error.
		n.AssertExpectations(t)
	})

	t.Run("idempotent_broadcast_disabled_skips_verification", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		disabled := false
		n.On("Config").Return(&common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{IdempotentTransactionBroadcast: &disabled},
		}).Maybe()
		// No Forward expectation — verification must be skipped.

		origErr := makeExhaustedError()
		req := makeSendRawTxRequest(t)
		_, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, nil, origErr,
		)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"original error must propagate untouched when idempotent broadcast is disabled")
		n.AssertExpectations(t)
	})

	// --- Coverage for the P1 review findings (gated_auto fixes applied) ---

	t.Run("failsafe_timeout_exceeded_triggers_verification", func(t *testing.T) {
		// Regression guard for review finding #1: ErrCodeFailsafeTimeoutExceeded
		// was missing from the exhausted-class gate. When the network-scope
		// timeout policy fires before retries exhaust, the broadcast may still
		// have reached an upstream's mempool. The same verification probe must
		// apply.
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()

		txObject := []byte(`{"hash":"` + sendRawTxFixtureHash + `","blockNumber":"0x123","from":"0x0","to":"0x0"}`)
		n.On("Forward", mock.Anything, mock.MatchedBy(func(r *common.NormalizedRequest) bool {
			m, _ := r.Method()
			return m == "eth_getTransactionByHash"
		})).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), txObject, nil),
			),
			nil,
		).Once()

		// Construct a network-scope failsafe timeout error wrapping a context cause.
		timeoutErr := common.NewErrFailsafeTimeoutExceeded(common.ScopeNetwork, context.DeadlineExceeded, nil)

		req := makeSendRawTxRequest(t)
		resp, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, nil, timeoutErr,
		)
		require.NoError(t, err, "FailsafeTimeoutExceeded should trigger verification just like exhausted retries")
		require.NotNil(t, resp)
		n.AssertExpectations(t)
	})

	t.Run("returned_hash_mismatch_refuses_synthetic_success", func(t *testing.T) {
		// Regression guard for review finding #3: a byzantine or buggy upstream
		// returning *any* non-null tx object for the queried hash would have
		// triggered false synthetic success. The hook now cross-checks the
		// returned hash field against the locally-derived hash.
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()

		// The upstream returns a non-null tx object but with a DIFFERENT hash.
		wrongHash := "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
		txObject := []byte(`{"hash":"` + wrongHash + `","blockNumber":"0x123","from":"0x0","to":"0x0"}`)
		n.On("Forward", mock.Anything, mock.MatchedBy(func(r *common.NormalizedRequest) bool {
			m, _ := r.Method()
			return m == "eth_getTransactionByHash"
		})).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), txObject, nil),
			),
			nil,
		).Once()

		origErr := makeExhaustedError()
		req := makeSendRawTxRequest(t)
		resp, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, nil, origErr,
		)
		require.Error(t, err, "hash mismatch must NOT yield synthetic success")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"original exhausted error must propagate on hash mismatch")
		assert.Nil(t, resp)
		n.AssertExpectations(t)
	})

	t.Run("missing_hash_field_in_response_refuses_synthetic_success", func(t *testing.T) {
		// Defense-in-depth: if the verification response is a non-null object
		// but the "hash" field is absent (malformed upstream), the cross-check
		// must fail safe and propagate the original error.
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()

		// Non-null object with no "hash" field at all.
		txObject := []byte(`{"blockNumber":"0x123","from":"0x0","to":"0x0"}`)
		n.On("Forward", mock.Anything, mock.MatchedBy(func(r *common.NormalizedRequest) bool {
			m, _ := r.Method()
			return m == "eth_getTransactionByHash"
		})).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), txObject, nil),
			),
			nil,
		).Once()

		origErr := makeExhaustedError()
		req := makeSendRawTxRequest(t)
		_, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, nil, origErr,
		)
		require.Error(t, err, "missing hash field must NOT yield synthetic success")
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted))
		n.AssertExpectations(t)
	})

	t.Run("verification_forward_error_returns_original_error", func(t *testing.T) {
		// Coverage for the verifyErr != nil branch (review testing gap T-1).
		// If the verification probe itself fails (transport error, all upstreams
		// 5xx the read, etc.), the hook must fall back to the original error.
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()

		n.On("Forward", mock.Anything, mock.MatchedBy(func(r *common.NormalizedRequest) bool {
			m, _ := r.Method()
			return m == "eth_getTransactionByHash"
		})).Return(
			(*common.NormalizedResponse)(nil),
			errors.New("verification transport failure"),
		).Once()

		origErr := makeExhaustedError()
		req := makeSendRawTxRequest(t)
		_, err := networkPostForward_eth_sendRawTransaction(
			context.Background(), n, req, nil, origErr,
		)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"original exhausted error must propagate when verification itself fails")
		n.AssertExpectations(t)
	})

	t.Run("parent_context_already_cancelled_still_runs_probe", func(t *testing.T) {
		// Critical: the independent verification timeout (context.WithoutCancel
		// + WithTimeout) must allow the probe to run even when the parent
		// context is already cancelled — otherwise the feature silently no-ops
		// in the most common production failure mode (deadline already consumed
		// by the failsafe retry loop).
		n := new(mockNetwork)
		n.On("Id").Return("evm:8453").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()

		txObject := []byte(`{"hash":"` + sendRawTxFixtureHash + `","blockNumber":"0x123","from":"0x0","to":"0x0"}`)
		n.On("Forward", mock.Anything, mock.MatchedBy(func(r *common.NormalizedRequest) bool {
			m, _ := r.Method()
			if m != "eth_getTransactionByHash" {
				return false
			}
			// Inspect the ctx the probe was called with. It must be a fresh
			// context with no Done channel triggered (because we used
			// WithoutCancel + WithTimeout).
			// We can't directly inspect the call's ctx from MatchedBy, but the
			// fact that Forward gets called at all (mock fires) proves the
			// probe wasn't short-circuited by the parent ctx.Err() check
			// inside our hook.
			return true
		})).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), txObject, nil),
			),
			nil,
		).Once()

		// Parent ctx is already cancelled when the hook is called.
		parentCtx, cancel := context.WithCancel(context.Background())
		cancel()

		origErr := makeExhaustedError()
		req := makeSendRawTxRequest(t)
		resp, err := networkPostForward_eth_sendRawTransaction(
			parentCtx, n, req, nil, origErr,
		)
		require.NoError(t, err, "independent verification budget must allow probe even with cancelled parent ctx")
		require.NotNil(t, resp)
		n.AssertExpectations(t)
	})
}
