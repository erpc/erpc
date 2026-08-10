package erpc

import (
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func emptyishRequestForMethod(t *testing.T, method string, retryEmpty bool) *common.NormalizedRequest {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":[]}`, method)
	req := common.NewNormalizedRequest([]byte(body))
	req.ApplyDirectiveDefaults(&common.DirectiveDefaultsConfig{
		RetryEmpty: util.BoolPtr(retryEmpty),
	})
	return req
}

func emptyishResponse(t *testing.T, result interface{}) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(1, result, nil)
	require.NoError(t, err)
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
	require.True(t, resp.IsResultEmptyish(), "fixture must be emptyish")
	return resp
}

// emptyResultRetryExecutor builds a network executor whose retry policy leaves the
// empty-result decision entirely to the method-semantics resolution (no cap hit on
// the first attempt), with the default accept list.
func emptyResultRetryExecutor(t *testing.T) *networkExecutor {
	t.Helper()
	cfg := &common.NetworkFailsafeConfig{
		Retry: &common.RetryPolicyConfig{
			MaxAttempts:            3,
			EmptyResultMaxAttempts: common.DefaultEmptyResultMaxAttempts,
			EmptyResultAccept:      common.DefaultEmptyResultAccept(),
		},
	}
	e, err := NewNetworkExecutor(cfg, &log.Logger, nil, nil)
	require.NoError(t, err)
	return e
}

// TestNetworkExecutor_EmptyResultSemantics pins the network-scope empty-result
// decision for all three method classes — accept-list, mark-empty-as-error list,
// and the FALLTHROUGH (a method in neither list, e.g. a rollup-specific or
// future-EIP method). The fallthrough row is the load-bearing one: it documents
// that today an unknown method's legitimate zero answer IS retried across
// upstreams as `empty_result` whenever the retryEmpty directive is on.
//
// This test must pass identically before and after the empty-result semantics
// move into per-method metadata — it is the behavior-preservation pin.
func TestNetworkExecutor_EmptyResultSemantics(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		result     interface{}
		retryEmpty bool
		wantReason string
	}{
		// (a) accept-list methods: empty is the final answer, never retried.
		{"AcceptList_getLogs_retryEmptyOn", "eth_getLogs", []interface{}{}, true, ""},
		{"AcceptList_call_retryEmptyOn", "eth_call", "0x", true, ""},
		{"AcceptList_getBalance_retryEmptyOn", "eth_getBalance", "0x0", true, ""},
		{"AcceptList_getLogs_retryEmptyOff", "eth_getLogs", []interface{}{}, false, ""},

		// (b) mark-empty-as-error methods. NOTE: at the network-executor layer
		// these are indistinguishable from (c) — the mark-as-error list acts one
		// layer earlier (the per-upstream post-forward hook) and converts the
		// emptyish response into ErrEndpointMissingData, which lands on the
		// `missing_data` branch instead. When that hook does not fire (list
		// unset, non-EVM network, beyond-confidence block) the raw emptyish
		// response reaches here and is retried as `empty_result`.
		{"MarkAsError_getBlockByNumber_retryEmptyOn", "eth_getBlockByNumber", nil, true, "empty_result"},
		{"MarkAsError_getBlockByNumber_retryEmptyOff", "eth_getBlockByNumber", nil, false, ""},

		// (c) FALLTHROUGH — in neither list. Retried today when retryEmpty is on.
		{"Fallthrough_customMethod_retryEmptyOn", "myrollup_getSomething", nil, true, "empty_result"},
		{"Fallthrough_customMethod_emptyArray_retryEmptyOn", "myrollup_getSomething", []interface{}{}, true, "empty_result"},
		{"Fallthrough_customMethod_zeroHex_retryEmptyOn", "myrollup_getSomething", "0x0", true, "empty_result"},
		{"Fallthrough_customMethod_retryEmptyOff", "myrollup_getSomething", nil, false, ""},
		{"Fallthrough_getBlockReceipts_retryEmptyOn", "eth_getBlockReceipts", []interface{}{}, true, "empty_result"},
		{"Fallthrough_getTransactionReceipt_retryEmptyOn", "eth_getTransactionReceipt", nil, true, "empty_result"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := emptyResultRetryExecutor(t)
			req := emptyishRequestForMethod(t, tc.method, tc.retryEmpty)
			resp := emptyishResponse(t, tc.result)

			got := e.shouldRetryWithReason(req, resp, nil, 0)
			assert.Equal(t, tc.wantReason, got,
				"method=%s retryEmpty=%v", tc.method, tc.retryEmpty)
		})
	}
}

// A per-method `emptyResult` override under networks[].methods.definitions wins
// over BOTH shipped lists and over the unknown-method fallthrough — the point of
// the override being that an operator can speak about a method (rollup-specific,
// future EIP) that no list has heard of.
func TestNetworkExecutor_EmptyResultOverrideWinsOverDefaults(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		override   common.EmptyResultBehavior
		wantReason string
	}{
		{"UnknownMethod_acceptOverride_stopsRetry", "myrollup_getSomething", common.EmptyResultBehaviorAccept, ""},
		{"UnknownMethod_errorOverride_keepsRetry", "myrollup_getSomething", common.EmptyResultBehaviorError, "empty_result"},
		{"AcceptListMethod_errorOverride_startsRetry", "eth_getLogs", common.EmptyResultBehaviorError, "empty_result"},
		{"MarkAsErrorMethod_acceptOverride_stopsRetry", "eth_getBlockByNumber", common.EmptyResultBehaviorAccept, ""},
		{"AcceptListMethod_explicitDefault_defersToList", "eth_getLogs", common.EmptyResultBehaviorDefault, ""},
		{"UnknownMethod_explicitDefault_defersToFallthrough", "myrollup_getSomething", common.EmptyResultBehaviorDefault, "empty_result"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nw := &Network{cfg: &common.NetworkConfig{
				Architecture: common.ArchitectureEvm,
				Evm: &common.EvmNetworkConfig{
					ChainId:                 123,
					MarkEmptyAsErrorMethods: common.DefaultMarkEmptyAsErrorMethods(),
				},
				Methods: &common.MethodsConfig{
					Definitions: map[string]*common.CacheMethodConfig{
						tc.method: {EmptyResult: tc.override},
					},
				},
			}}

			e := emptyResultRetryExecutor(t)
			req := emptyishRequestForMethod(t, tc.method, true)
			req.SetNetwork(nw)

			got := e.shouldRetryWithReason(req, emptyishResponse(t, nil), nil, 0)
			assert.Equal(t, tc.wantReason, got,
				"method=%s override=%s", tc.method, tc.override)
		})
	}
}

// Without an override, a network that carries the shipped markEmptyAsErrorMethods
// list still behaves exactly as before: the empty result is retried.
func TestNetworkExecutor_EmptyResultDefaultsUnchangedWithNetworkConfig(t *testing.T) {
	nw := &Network{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                 123,
			MarkEmptyAsErrorMethods: common.DefaultMarkEmptyAsErrorMethods(),
		},
	}}
	e := emptyResultRetryExecutor(t)

	for method, want := range map[string]string{
		"eth_getLogs":               "",             // accept list
		"eth_getBlockByNumber":      "empty_result", // mark-as-error list
		"eth_getTransactionReceipt": "empty_result", // in neither list
		"myrollup_getSomething":     "empty_result", // unknown method
	} {
		req := emptyishRequestForMethod(t, method, true)
		req.SetNetwork(nw)
		assert.Equal(t, want, e.shouldRetryWithReason(req, emptyishResponse(t, nil), nil, 0),
			"method=%s", method)
	}
}

// The shared data-unavailable cap bounds the fallthrough path too: an unknown
// method's emptyish result is retried at most EmptyResultMaxAttempts times, not
// MaxAttempts times.
func TestNetworkExecutor_EmptyResultFallthroughRespectsCap(t *testing.T) {
	e := emptyResultRetryExecutor(t)
	req := emptyishRequestForMethod(t, "myrollup_getSomething", true)

	assert.Equal(t, "empty_result", e.shouldRetryWithReason(req, emptyishResponse(t, nil), nil, 0),
		"first attempt retries the unknown method's empty result")
	assert.Equal(t, "", e.shouldRetryWithReason(req, emptyishResponse(t, nil), nil, 1),
		"EmptyResultMaxAttempts=2 stops the second retry even though MaxAttempts=3")
}
