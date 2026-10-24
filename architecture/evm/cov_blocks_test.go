package evm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func covBlocksResp(t *testing.T, req *common.NormalizedRequest, result string) *common.NormalizedResponse {
	t.Helper()
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(result), nil),
	)
	if req != nil {
		resp.WithRequest(req)
	}
	return resp
}

func covBlocksReq(method string, params ...interface{}) *common.NormalizedRequest {
	if params == nil {
		params = []interface{}{}
	}
	return common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest(method, params))
}

// covBlocksChanIDReq carries an id that cannot be marshalled, so any attempt
// to synthesize a response for it fails.
func covBlocksChanIDReq(method string, params ...interface{}) *common.NormalizedRequest {
	if params == nil {
		params = []interface{}{}
	}
	jrq := common.NewJsonRpcRequest(method, params)
	jrq.ID = make(chan int)
	return common.NewNormalizedRequestFromJsonRpcRequest(jrq)
}

func covBlocksBadBodyReq() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":`))
}

// covBlocksForwardUpstream is a config upstream with a pluggable Forward.
type covBlocksForwardUpstream struct {
	queryTestConfigUpstream
	forwardFn func(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

func (u *covBlocksForwardUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion bool, isHedgeAttempt bool) (*common.NormalizedResponse, error) {
	return u.forwardFn(ctx, nq)
}

func TestCovBlocks_BuildGetBlockByNumberRequest(t *testing.T) {
	cases := []struct {
		name    string
		in      interface{}
		want    string
		wantErr bool
	}{
		{name: "latest_tag", in: "latest", want: "latest"},
		{name: "earliest_tag", in: "earliest", want: "earliest"},
		{name: "hex_string_kept_verbatim", in: "0x10", want: "0x10"},
		{name: "unknown_tag", in: "newest", wantErr: true},
		{name: "int", in: 16, want: "0x10"},
		{name: "int64", in: int64(255), want: "0xff"},
		{name: "float64_not_normalizable", in: float64(16), wantErr: true},
		{name: "unsupported_type", in: []int{1}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jrq, err := BuildGetBlockByNumberRequest(tc.in, true)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, jrq)
				assert.Contains(t, err.Error(), "invalid block number or tag for eth_getBlockByNumber")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "eth_getBlockByNumber", jrq.Method)
			assert.Equal(t, []interface{}{tc.want, true}, jrq.Params)
		})
	}
}

func TestCovBlocks_EnforceHighestBlock_Edges(t *testing.T) {
	ctx := context.Background()
	network := &testNetwork{highestLatest: 0x100}
	enforce := &common.RequestDirectives{EnforceHighestBlock: true}

	t.Run("unparsable_request_body_fails", func(t *testing.T) {
		req := covBlocksBadBodyReq()
		req.SetDirectives(enforce)
		out, err := enforceHighestBlock(ctx, network, req, covBlocksResp(t, nil, `{"number":"0x1"}`), nil)
		assert.Nil(t, out)
		assert.Error(t, err)
	})

	passThrough := []struct {
		name   string
		params []interface{}
	}{
		{name: "no_params", params: []interface{}{}},
		{name: "numeric_block_param", params: []interface{}{float64(5), false}},
		{name: "pending_tag", params: []interface{}{"pending"}},
	}
	for _, tc := range passThrough {
		t.Run(tc.name, func(t *testing.T) {
			req := covBlocksReq("eth_getBlockByNumber", tc.params...)
			req.SetDirectives(enforce)
			resp := covBlocksResp(t, req, `{"number":"0x1"}`)
			out, err := enforceHighestBlock(ctx, network, req, resp, nil)
			require.NoError(t, err)
			assert.Same(t, resp, out)
		})
	}

	for _, tag := range []string{"latest", "finalized"} {
		t.Run("malformed_block_number_in_"+tag+"_response_fails", func(t *testing.T) {
			req := covBlocksReq("eth_getBlockByNumber", tag, false)
			req.SetDirectives(enforce)
			resp := covBlocksResp(t, req, `{"number":"0xzz"}`)
			out, err := enforceHighestBlock(ctx, network, req, resp, nil)
			assert.Nil(t, out)
			require.Error(t, err)
		})
	}

	t.Run("upstream_error_short_circuits", func(t *testing.T) {
		req := covBlocksReq("eth_getBlockByNumber", "latest", false)
		req.SetDirectives(enforce)
		boom := errors.New("upstream down")
		out, err := networkPostForward_eth_getBlockByNumber(ctx, network, req, nil, boom)
		assert.Nil(t, out)
		assert.Same(t, boom, err)
	})
}

func TestCovBlocks_PickHighestBlock(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("re-fetch failed")
	type pick int
	const (
		pickNil pick = iota
		pickX
		pickY
	)
	cases := []struct {
		name    string
		x, y    string // "" means nil response
		err     error
		want    pick
		wantErr error
	}{
		{name: "both_empty_with_error", x: `null`, y: "", err: boom, want: pickNil, wantErr: boom},
		{name: "both_nil_without_error", want: pickNil},
		{name: "y_missing_keeps_x", x: `{"number":"0x5"}`, want: pickX},
		{name: "x_without_number_keeps_y", x: `{"hash":"0x1"}`, y: `{"number":"0x5"}`, want: pickY},
		{name: "y_without_number_keeps_x", x: `{"number":"0x5"}`, y: `{"hash":"0x1"}`, want: pickX},
		{name: "x_unparsable_number_keeps_y", x: `{"number":"0xzz"}`, y: `{"number":"0x5"}`, want: pickY},
		{name: "y_unparsable_number_keeps_x", x: `{"number":"0x5"}`, y: `{"number":"0xzz"}`, want: pickX},
		{name: "x_higher_wins", x: `{"number":"0x6"}`, y: `{"number":"0x5"}`, want: pickX},
		{name: "y_higher_wins", x: `{"number":"0x5"}`, y: `{"number":"0x6"}`, want: pickY},
		{name: "tie_prefers_y", x: `{"number":"0x5"}`, y: `{"number":"0x5"}`, want: pickY},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var x, y *common.NormalizedResponse
			if tc.x != "" {
				x = covBlocksResp(t, nil, tc.x)
			}
			if tc.y != "" {
				y = covBlocksResp(t, nil, tc.y)
			}
			out, err := pickHighestBlock(ctx, x, y, tc.err)
			if tc.wantErr != nil {
				assert.Same(t, tc.wantErr, err)
			} else {
				assert.NoError(t, err)
			}
			switch tc.want {
			case pickNil:
				assert.Nil(t, out)
			case pickX:
				assert.Same(t, x, out)
			case pickY:
				assert.Same(t, y, out)
			}
		})
	}
}

func TestCovBlocks_ExtractBlockReference_Errors(t *testing.T) {
	ctx := context.Background()

	t.Run("nil_request_and_response", func(t *testing.T) {
		ref, bn, err := ExtractBlockReferenceFromRequest(ctx, nil)
		assert.Equal(t, "", ref)
		assert.Zero(t, bn)
		assert.NoError(t, err)

		ref, bn, err = ExtractBlockReferenceFromResponse(ctx, nil)
		assert.Equal(t, "", ref)
		assert.Zero(t, bn)
		assert.NoError(t, err)

		ts, err := ExtractBlockTimestampFromResponse(ctx, nil)
		assert.Zero(t, ts)
		assert.NoError(t, err)
	})

	t.Run("unparsable_request_body", func(t *testing.T) {
		_, _, err := ExtractBlockReferenceFromRequest(ctx, covBlocksBadBodyReq())
		assert.Error(t, err)

		_, _, err = ResolveCacheBlockRef(ctx, covBlocksBadBodyReq(), nil)
		assert.Error(t, err)

		resp := covBlocksResp(t, covBlocksBadBodyReq(), `{"number":"0x1"}`)
		_, _, err = ExtractBlockReferenceFromResponse(ctx, resp)
		assert.Error(t, err)
	})

	t.Run("malformed_last_valid_response_surfaces_error_keeping_request_ref", func(t *testing.T) {
		req := covBlocksReq("eth_getTransactionReceipt", "0xabc")
		lvr := covBlocksResp(t, req, `{"blockNumber":"0xzz"}`)
		require.True(t, req.SetLastValidResponse(ctx, lvr))

		ref, bn, err := ExtractBlockReferenceFromRequest(ctx, req)
		require.Error(t, err)
		assert.Equal(t, "*", ref)
		assert.Zero(t, bn)
	})

	t.Run("nil_json_rpc_inputs", func(t *testing.T) {
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`{"number":"0x1"}`), nil)
		_, _, err := extractRefFromJsonRpcResponse(ctx, nil, nil, jrr)
		assert.EqualError(t, err, "cannot extract block reference when json-rpc request is nil")

		jrq := common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"0x1", false})
		_, _, err = extractRefFromJsonRpcResponse(ctx, nil, jrq, nil)
		assert.EqualError(t, err, "cannot extract block reference when json-rpc response is nil")
	})
}

func TestCovBlocks_ExtractBlockTimestamp(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		result  string
		want    int64
		wantErr bool
	}{
		{name: "decimal_string", result: `{"timestamp":"1700000000"}`, want: 1700000000},
		{name: "hex_string", result: `{"timestamp":"0x6553f100"}`, want: 0x6553f100},
		{name: "null_result", result: `null`, want: 0},
		{name: "invalid_hex", result: `{"timestamp":"0xzz"}`, wantErr: true},
		{name: "invalid_decimal", result: `{"timestamp":"soon"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := ExtractBlockTimestampFromResponse(ctx, covBlocksResp(t, nil, tc.result))
			if tc.wantErr {
				assert.Error(t, err)
				assert.Zero(t, ts)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, ts)
		})
	}
}

// Plan H2 deletes "the cache key on the word `latest`": a translated tag
// keys on the number its Head gives it, an untranslated one gets no GET key.
func TestCovBlocks_ResolveCacheBlockRef_TipFallbacks(t *testing.T) {
	ctx := context.Background()

	t.Run("finalized_tag_resolves_to_network_finalized_tip", func(t *testing.T) {
		req := covBlocksReq("eth_getBalance", "0xabc", "finalized")
		req.SetNetwork(&queryTestNetwork{latest: 0x40, finalized: 0x30})
		ref, bn, err := ResolveCacheBlockRef(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, "48", ref)
		assert.Equal(t, int64(0x30), bn)
	})

	t.Run("untranslated_tag_gets_no_get_key", func(t *testing.T) {
		// eth_getBlockByNumber keeps translateLatestTag off: its tag reaches
		// upstreams as is, so no Head number names the answer.
		req := covBlocksReq("eth_getBlockByNumber", "latest", false)
		req.SetNetwork(&queryTestNetwork{latest: 0x40, finalized: 0x30})
		ref, bn, err := ResolveCacheBlockRef(ctx, req, nil)
		require.NoError(t, err)
		assert.Equal(t, "", ref)
		assert.Equal(t, int64(0), bn)
	})
}

func TestCovBlocks_BlockNumberHooks(t *testing.T) {
	ctx := context.Background()
	enforce := &common.RequestDirectives{EnforceHighestBlock: true}
	staleCounter := func(upstreamID string) float64 {
		return testutil.ToFloat64(telemetry.MetricUpstreamStaleLatestBlock.WithLabelValues(
			"test-project", "", "test", upstreamID, "eth_blockNumber",
		))
	}

	run := func(t *testing.T, tip int64, respHex string, ups common.Upstream) (*common.NormalizedResponse, error) {
		t.Helper()
		req := covBlocksReq("eth_blockNumber")
		req.SetDirectives(enforce)
		jrr, err := common.NewJsonRpcResponse(1, respHex, nil)
		require.NoError(t, err)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		if ups != nil {
			resp.SetUpstream(ups)
		}
		return networkPostForward_eth_blockNumber(ctx, &testNetwork{highestLatest: tip}, req, resp, nil)
	}

	t.Run("stale_upstream_answer_is_raised_and_counted", func(t *testing.T) {
		ups := common.NewFakeUpstream("covblocks-stale")
		before := staleCounter("covblocks-stale")
		out, err := run(t, 0x1000, "0x800", ups)
		require.NoError(t, err)
		_, bn, err := ExtractBlockReferenceFromResponse(ctx, out)
		require.NoError(t, err)
		assert.Equal(t, int64(0x1000), bn)
		assert.Same(t, ups, out.Upstream(), "corrected response keeps upstream attribution")
		assert.False(t, out.FromCache())
		assert.Equal(t, before+1, staleCounter("covblocks-stale"))
	})

	t.Run("ahead_upstream_answer_is_pinned_without_stale_metric", func(t *testing.T) {
		ups := common.NewFakeUpstream("covblocks-ahead")
		before := staleCounter("covblocks-ahead")
		out, err := run(t, 0x1000, "0x1002", ups)
		require.NoError(t, err)
		_, bn, err := ExtractBlockReferenceFromResponse(ctx, out)
		require.NoError(t, err)
		assert.Equal(t, int64(0x1000), bn)
		assert.Same(t, ups, out.Upstream())
		assert.Equal(t, before, staleCounter("covblocks-ahead"))
	})

	t.Run("malformed_block_number_fails", func(t *testing.T) {
		out, err := run(t, 0x1000, "0xzz", nil)
		assert.Nil(t, out)
		assert.Error(t, err)
	})

	t.Run("upstream_error_passes_through", func(t *testing.T) {
		boom := errors.New("timeout")
		req := covBlocksReq("eth_blockNumber")
		req.SetDirectives(enforce)
		out, err := networkPostForward_eth_blockNumber(ctx, &testNetwork{highestLatest: 1}, req, nil, boom)
		assert.Nil(t, out)
		assert.Same(t, boom, err)
	})

	t.Run("unmarshalable_request_id_fails_synthesis", func(t *testing.T) {
		req := covBlocksChanIDReq("eth_blockNumber")
		req.SetDirectives(enforce)
		jrr, err := common.NewJsonRpcResponse(1, "0x1", nil)
		require.NoError(t, err)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		out, err := networkPostForward_eth_blockNumber(ctx, &testNetwork{highestLatest: 0x10}, req, resp, nil)
		assert.Nil(t, out)
		assert.Error(t, err)

		handled, pre, err := networkPreForward_eth_blockNumber(ctx, &testNetwork{highestLatest: 0x10}, req)
		assert.True(t, handled)
		assert.Nil(t, pre)
		assert.Error(t, err)
	})
}

func TestCovBlocks_SendRawTransaction_UpstreamHookPassThrough(t *testing.T) {
	ctx := context.Background()
	net := &queryTestNetwork{cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}}
	ups := &covBlocksForwardUpstream{forwardFn: func(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		t.Fatal("no verification probe expected")
		return nil, nil
	}}
	alreadyKnown := common.NewErrEndpointNonceException(errors.New("already known"), common.NonceExceptionReasonAlreadyKnown)

	cases := []struct {
		name string
		req  *common.NormalizedRequest
		re   error
	}{
		{name: "nonce_code_without_nonce_exception_type", req: makeSendRawTxRequest(t), re: &common.BaseError{Code: common.ErrCodeEndpointNonceException, Message: "nonce"}},
		{name: "unknown_nonce_reason", req: makeSendRawTxRequest(t), re: common.NewErrEndpointNonceException(errors.New("replacement"), common.NonceExceptionReason("replacement_underpriced"))},
		{name: "unparsable_request_body", req: covBlocksBadBodyReq(), re: alreadyKnown},
		{name: "no_params", req: covBlocksReq("eth_sendRawTransaction"), re: alreadyKnown},
		{name: "non_string_param", req: covBlocksReq("eth_sendRawTransaction", float64(1)), re: alreadyKnown},
		{name: "non_hex_param", req: covBlocksReq("eth_sendRawTransaction", "0xnothex"), re: alreadyKnown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := common.NewNormalizedResponse()
			out, err := upstreamPostForward_eth_sendRawTransaction(ctx, net, ups, tc.req, rs, tc.re)
			assert.Same(t, rs, out)
			assert.Same(t, tc.re, err)
		})
	}

	t.Run("already_known_with_unmarshalable_id_fails_synthesis", func(t *testing.T) {
		req := covBlocksChanIDReq("eth_sendRawTransaction", sendRawTxFixture)
		out, err := upstreamPostForward_eth_sendRawTransaction(ctx, net, ups, req, nil, alreadyKnown)
		assert.Nil(t, out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create JSON-RPC response")
	})
}

func TestCovBlocks_SendRawTransaction_NonceTooLowVerification(t *testing.T) {
	ctx := context.Background()
	net := &queryTestNetwork{cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}}
	vendorMsg := "nonce too low: next nonce 7, tx nonce 3"
	nonceTooLow := common.NewErrEndpointNonceException(
		common.NewErrJsonRpcExceptionInternal(-32000, common.JsonRpcErrorServerSideException, vendorMsg, nil, nil),
		common.NonceExceptionReasonNonceTooLow,
	)
	probed := 0
	ups := &covBlocksForwardUpstream{forwardFn: func(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		probed++
		m, _ := nq.Method()
		assert.Equal(t, "eth_getTransactionByHash", m)
		return common.NewNormalizedResponse().WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes(
			[]byte(`1`),
			[]byte(`{"hash":"`+sendRawTxFixtureHash+`"}`),
			[]byte(`{"code":-32000,"message":"internal error"}`),
		)), nil
	}}

	out, err := upstreamPostForward_eth_sendRawTransaction(ctx, net, ups, makeSendRawTxRequest(t), nil, nonceTooLow)
	assert.Nil(t, out)
	require.Error(t, err)
	assert.Equal(t, 1, probed)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException))
	var jre *common.ErrJsonRpcExceptionInternal
	require.True(t, errors.As(err, &jre))
	assert.Equal(t, common.JsonRpcErrorTransactionRejected, jre.NormalizedCode())
	assert.Equal(t, vendorMsg, jre.Message, "the upstream wording is preserved")
}

func TestCovBlocks_SendRawTransaction_NetworkHookPassThrough(t *testing.T) {
	ctx := context.Background()
	cfg := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}

	t.Run("unparsable_tx_keeps_exhausted_error", func(t *testing.T) {
		net := &queryTestNetwork{cfg: cfg, forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			t.Fatal("no probe expected")
			return nil, nil
		}}
		exhausted := makeExhaustedError()
		rs := common.NewNormalizedResponse()
		out, err := networkPostForward_eth_sendRawTransaction(ctx, net, covBlocksReq("eth_sendRawTransaction", "0x02"), rs, exhausted)
		assert.Same(t, rs, out)
		assert.Same(t, exhausted, err)
	})

	t.Run("probe_response_with_error_payload_keeps_exhausted_error", func(t *testing.T) {
		net := &queryTestNetwork{cfg: cfg, forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return common.NewNormalizedResponse().WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes(
				[]byte(`1`),
				[]byte(`{"hash":"`+sendRawTxFixtureHash+`"}`),
				[]byte(`{"code":-32000,"message":"internal error"}`),
			)), nil
		}}
		exhausted := makeExhaustedError()
		out, err := networkPostForward_eth_sendRawTransaction(ctx, net, makeSendRawTxRequest(t), nil, exhausted)
		assert.Nil(t, out)
		assert.Same(t, exhausted, err)
	})
}

func TestCovBlocks_CreateNormalizedNonceTooLowError(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want string
	}{
		{name: "plain_cause_message", in: common.NewErrEndpointNonceException(errors.New("nonce too low: address 0xabc, tx: 3 state: 7"), common.NonceExceptionReasonNonceTooLow), want: "nonce too low: address 0xabc, tx: 3 state: 7"},
		{name: "no_cause_uses_exception_message", in: common.NewErrEndpointNonceException(nil, common.NonceExceptionReasonNonceTooLow), want: "transaction with this nonce already exists"},
		{name: "foreign_error_uses_generic_message", in: errors.New("connection reset"), want: "nonce too low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := createNormalizedNonceTooLowError(tc.in)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException))
			var jre *common.ErrJsonRpcExceptionInternal
			require.True(t, errors.As(err, &jre))
			assert.Equal(t, common.JsonRpcErrorTransactionRejected, jre.NormalizedCode())
			assert.Equal(t, tc.want, jre.Message)
		})
	}
}

// TestCovBlocks_DetailedTracing toggles the process-wide detailed-tracing flag,
// so it must stay sequential (no t.Parallel), like integrity_tracing_test.go.
func TestCovBlocks_DetailedTracing(t *testing.T) {
	prev := common.IsTracingDetailed
	common.IsTracingDetailed = true
	t.Cleanup(func() { common.IsTracingDetailed = prev })
	ctx := context.Background()

	t.Run("latest_block_records_timestamp_distance", func(t *testing.T) {
		network := &testNetwork{highestLatest: 0x20}
		req := covBlocksReq("eth_getBlockByNumber", "latest", false)
		ts := time.Now().Unix() - 100
		resp := covBlocksResp(t, req, fmt.Sprintf(`{"number":"0x10","hash":"0x01","timestamp":"0x%x"}`, ts))

		out, err := networkPostForward_eth_getBlockByNumber(ctx, network, req, resp, nil)
		require.NoError(t, err)
		assert.Same(t, resp, out)
		gauge := testutil.ToFloat64(telemetry.MetricNetworkLatestBlockTimestampDistance.WithLabelValues("test-project", "test", "network_response"))
		assert.GreaterOrEqual(t, gauge, float64(100))
		assert.Less(t, gauge, float64(160))
	})

	t.Run("block_refs_resolve_the_same_under_detailed_tracing", func(t *testing.T) {
		cached := covBlocksReq("eth_getBlockByNumber", "0x10", false)
		cached.SetEvmBlockRef("cached-ref")
		cached.SetEvmBlockNumber(int64(99))
		ref, bn, err := ExtractBlockReferenceFromRequest(ctx, cached)
		require.NoError(t, err)
		assert.Equal(t, "cached-ref", ref)
		assert.Equal(t, int64(99), bn)

		req := covBlocksReq("eth_getBlockByNumber", "0x10", false)
		ref, bn, err = ExtractBlockReferenceFromRequest(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, "16", ref)
		assert.Equal(t, int64(16), bn)

		ref, bn, err = ExtractBlockReferenceFromResponse(ctx, covBlocksResp(t, req, `{"number":"0x11"}`))
		require.NoError(t, err)
		assert.Equal(t, "17", ref)
		assert.Equal(t, int64(17), bn)

		tsv, err := ExtractBlockTimestampFromResponse(ctx, covBlocksResp(t, nil, `{"timestamp":"0x20"}`))
		require.NoError(t, err)
		assert.Equal(t, int64(32), tsv)
	})

	t.Run("block_number_ahead_of_tip_is_pinned", func(t *testing.T) {
		req := covBlocksReq("eth_blockNumber")
		req.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: true})
		jrr, err := common.NewJsonRpcResponse(1, "0x30", nil)
		require.NoError(t, err)
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		out, err := networkPostForward_eth_blockNumber(ctx, &testNetwork{highestLatest: 0x20}, req, resp, nil)
		require.NoError(t, err)
		_, bn, err := ExtractBlockReferenceFromResponse(ctx, out)
		require.NoError(t, err)
		assert.Equal(t, int64(0x20), bn)
	})
}
