package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// covTraceNet returns a queryTestNetwork whose Forward answers every
// trace_filter sub-request with a one-element array naming the sub-range, so
// merged output reveals both the split boundaries and the merge order.
func covTraceNet(cfg *common.NetworkConfig, latest int64, rec *covTraceRecorder) *queryTestNetwork {
	return &queryTestNetwork{
		cfg:    cfg,
		latest: latest,
		forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, err := req.JsonRpcRequest(ctx)
			if err != nil {
				return nil, err
			}
			jrq.RLock()
			method := jrq.Method
			filter := jrq.Params[0].(map[string]interface{})
			fb, _ := filter["fromBlock"].(string)
			tb, _ := filter["toBlock"].(string)
			jrq.RUnlock()
			if rec != nil {
				rec.add(method, fb, tb, req)
			}
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`[{"from":"`+fb+`","to":"`+tb+`"}]`), nil),
			), nil
		},
	}
}

type covTraceSub struct {
	method, from, to, skipCache string
	parent                      interface{}
	fromAddress                 interface{}
}

type covTraceRecorder struct {
	mu   sync.Mutex
	subs []covTraceSub
}

func (r *covTraceRecorder) add(method, fb, tb string, req *common.NormalizedRequest) {
	jrq, _ := req.JsonRpcRequest()
	jrq.RLock()
	fa := jrq.Params[0].(map[string]interface{})["fromAddress"]
	jrq.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs = append(r.subs, covTraceSub{
		method:      method,
		from:        fb,
		to:          tb,
		skipCache:   req.Directives().SkipCacheRead,
		parent:      req.ParentRequestId(),
		fromAddress: fa,
	})
}

func (r *covTraceRecorder) ranges() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for _, s := range r.subs {
		out[s.from] = s.to
	}
	return out
}

// covTraceResult decodes the "result" member of a merged JSON-RPC response.
func covTraceResult(t *testing.T, jrr *common.JsonRpcResponse) ([]map[string]string, interface{}) {
	t.Helper()
	var buf bytes.Buffer
	_, err := jrr.WriteTo(&buf)
	require.NoError(t, err)
	var out struct {
		ID     interface{}         `json:"id"`
		Result []map[string]string `json:"result"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out), buf.String())
	return out.Result, out.ID
}

func covTraceReq(t *testing.T, method string, id interface{}, filter map[string]interface{}) *common.NormalizedRequest {
	t.Helper()
	jrq := common.NewJsonRpcRequest(method, []interface{}{filter})
	if id != nil {
		require.NoError(t, jrq.SetID(id))
	}
	return common.NewNormalizedRequestFromJsonRpcRequest(jrq)
}

func covTraceHistogram(t *testing.T, method string) (uint64, float64) {
	t.Helper()
	obs := telemetry.MetricNetworkEvmTraceFilterRangeRequested.WithLabelValues(
		"test-project", "evm:1", method, "n/a", common.DataFinalityStateUnknown.String(),
	)
	m := &dto.Metric{}
	require.NoError(t, obs.(prometheus.Metric).Write(m))
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

func covTraceForcedSplits(method, dimension string) float64 {
	return testutil.ToFloat64(telemetry.MetricNetworkEvmTraceFilterForcedSplits.WithLabelValues(
		"test-project", "evm:1", method, dimension, "n/a", "unknown",
	))
}

func TestCovTrace_ProjectPreForward_RangeHistogram(t *testing.T) {
	ctx := context.Background()
	net := &queryTestNetwork{cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}, latest: 0x10, finalized: 0x8}

	cases := []struct {
		name      string
		method    string
		filter    interface{}
		wantCount uint64
		wantSum   float64
	}{
		{name: "hex_range_observed", method: "trace_filter", filter: map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"}, wantCount: 1, wantSum: 4},
		{name: "latest_tag_resolved_to_tip", method: "arbtrace_filter", filter: map[string]interface{}{"fromBlock": "0x1", "toBlock": "latest"}, wantCount: 1, wantSum: 16},
		{name: "finalized_tag_resolved", method: "trace_filter", filter: map[string]interface{}{"fromBlock": "finalized", "toBlock": "0x8"}, wantCount: 1, wantSum: 1},
		{name: "genesis_from_block_not_observed", method: "trace_filter", filter: map[string]interface{}{"fromBlock": "0x0", "toBlock": "0x4"}},
		{name: "inverted_range_not_observed", method: "trace_filter", filter: map[string]interface{}{"fromBlock": "0x5", "toBlock": "0x4"}},
		{name: "unresolvable_safe_tag_not_observed", method: "trace_filter", filter: map[string]interface{}{"fromBlock": "safe", "toBlock": "0x4"}},
		{name: "missing_bounds_observed_at_tip", method: "trace_filter", filter: map[string]interface{}{}, wantCount: 1, wantSum: 1},
		{name: "invalid_hex_not_observed", method: "trace_filter", filter: map[string]interface{}{"fromBlock": "0xzz", "toBlock": "0x4"}},
		{name: "non_object_filter_ignored", method: "trace_filter", filter: "0x1"},
		{name: "other_method_ignored", method: "eth_getLogs", filter: map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			beforeCount, beforeSum := covTraceHistogram(t, "trace_filter")
			beforeArbCount, beforeArbSum := covTraceHistogram(t, "arbtrace_filter")
			req := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest(tc.method, []interface{}{tc.filter}))

			handled, resp, err := projectPreForward_trace_filter(ctx, net, req)
			require.NoError(t, err)
			assert.False(t, handled)
			assert.Nil(t, resp)

			afterCount, afterSum := covTraceHistogram(t, "trace_filter")
			afterArbCount, afterArbSum := covTraceHistogram(t, "arbtrace_filter")
			gotCount := (afterCount - beforeCount) + (afterArbCount - beforeArbCount)
			gotSum := (afterSum - beforeSum) + (afterArbSum - beforeArbSum)
			assert.Equal(t, tc.wantCount, gotCount)
			assert.Equal(t, tc.wantSum, gotSum)
		})
	}

	t.Run("empty_params_and_nil_inputs_are_noops", func(t *testing.T) {
		req := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("trace_filter", []interface{}{}))
		handled, resp, err := projectPreForward_trace_filter(ctx, net, req)
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)

		handled, _, err = projectPreForward_trace_filter(ctx, nil, req)
		assert.False(t, handled)
		assert.NoError(t, err)
		handled, _, err = projectPreForward_trace_filter(ctx, net, nil)
		assert.False(t, handled)
		assert.NoError(t, err)
	})
}

func TestCovTrace_NetworkPreForward_PassThrough(t *testing.T) {
	ctx := context.Background()
	evmCfg := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}
	ups := []common.Upstream{&queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 1}}}}

	cases := []struct {
		name   string
		cfg    *common.NetworkConfig
		method string
		params []interface{}
	}{
		{name: "other_method", cfg: evmCfg, method: "eth_getLogs", params: []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x10"}}},
		{name: "empty_params", cfg: evmCfg, method: "trace_filter", params: []interface{}{}},
		{name: "non_object_filter", cfg: evmCfg, method: "trace_filter", params: []interface{}{"0x1"}},
		{name: "blockHash_filter", cfg: evmCfg, method: "trace_filter", params: []interface{}{map[string]interface{}{"blockHash": "0x01"}}},
		{name: "unresolvable_safe_tag", cfg: evmCfg, method: "trace_filter", params: []interface{}{map[string]interface{}{"fromBlock": "safe", "toBlock": "0x10"}}},
		{name: "unparsable_hex", cfg: evmCfg, method: "trace_filter", params: []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0xnothex"}}},
		{name: "network_without_evm_config", cfg: &common.NetworkConfig{}, method: "trace_filter", params: []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x10"}}},
		{name: "nil_network_config", cfg: nil, method: "trace_filter", params: []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x10"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net := &queryTestNetwork{cfg: tc.cfg, latest: 0x100, forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				t.Fatalf("no sub-request expected for %s", tc.name)
				return nil, nil
			}}
			req := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest(tc.method, tc.params))
			handled, resp, err := networkPreForward_trace_filter(ctx, net, ups, req)
			assert.False(t, handled)
			assert.Nil(t, resp)
			assert.NoError(t, err)
			assert.False(t, req.IsCompositeRequest())
		})
	}

	t.Run("nil_inputs", func(t *testing.T) {
		handled, _, err := networkPreForward_trace_filter(ctx, nil, ups, covTraceReq(t, "trace_filter", nil, map[string]interface{}{}))
		assert.False(t, handled)
		assert.NoError(t, err)
		handled, _, err = networkPreForward_trace_filter(ctx, &queryTestNetwork{cfg: evmCfg}, ups, nil)
		assert.False(t, handled)
		assert.NoError(t, err)
	})

	t.Run("already_composite_request_is_not_resplit", func(t *testing.T) {
		req := covTraceReq(t, "trace_filter", nil, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x10"})
		req.SetCompositeType(common.CompositeTypeTraceFilterSplitOnError)
		handled, _, err := networkPreForward_trace_filter(ctx, &queryTestNetwork{cfg: evmCfg}, ups, req)
		assert.False(t, handled)
		assert.NoError(t, err)
	})

	t.Run("upstreams_without_evm_config_disable_splitting", func(t *testing.T) {
		net := &queryTestNetwork{cfg: evmCfg, forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			t.Fatal("no sub-request expected")
			return nil, nil
		}}
		noEvm := []common.Upstream{
			nil,
			&queryTestConfigUpstream{cfg: nil},
			&queryTestConfigUpstream{cfg: &common.UpstreamConfig{}},
			&queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 0}}},
		}
		req := covTraceReq(t, "trace_filter", nil, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x10"})
		handled, resp, err := networkPreForward_trace_filter(ctx, net, noEvm, req)
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})

	t.Run("tag_resolved_inverted_range_is_invalid_request", func(t *testing.T) {
		net := &queryTestNetwork{cfg: evmCfg, latest: 0x5}
		req := covTraceReq(t, "trace_filter", nil, map[string]interface{}{"fromBlock": "0x10", "toBlock": "latest"})
		handled, resp, err := networkPreForward_trace_filter(ctx, net, ups, req)
		assert.True(t, handled)
		assert.Nil(t, resp)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))
		assert.Contains(t, err.Error(), "fromBlock (16) must be less than or equal to toBlock (5)")
	})
}

func TestCovTrace_NetworkPreForward_ProactiveSplitMerges(t *testing.T) {
	ctx := context.Background()
	rec := &covTraceRecorder{}
	net := covTraceNet(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitConcurrency: 2}}, 0x7, rec)
	ups := []common.Upstream{
		&queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 5}}},
		&queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 3}}},
	}
	// A tag bound reaches upstreams as is (translateLatestTag off, as for
	// arbtrace_*, or skip-interpolation): numbered sub-requests would change
	// what the client asked, so the range is not split.
	tagged := covTraceReq(t, "ARBTRACE_FILTER", 41, map[string]interface{}{"fromBlock": "0x1", "toBlock": "latest"})
	handled, resp, err := networkPreForward_trace_filter(ctx, net, ups, tagged)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Nil(t, resp)
	assert.Empty(t, rec.ranges(), "no sub-request for a range sent with a tag")

	req := covTraceReq(t, "ARBTRACE_FILTER", 42, map[string]interface{}{
		"fromBlock":   "0x1",
		"toBlock":     "0x7",
		"fromAddress": []interface{}{"0xaaa"},
	})
	req.SetDirectives(&common.RequestDirectives{SkipCacheRead: "*"})

	handled, resp, err = networkPreForward_trace_filter(ctx, net, ups, req)
	require.NoError(t, err)
	require.True(t, handled)
	require.NotNil(t, resp)
	assert.Equal(t, common.CompositeTypeTraceFilterSplitProactive, req.CompositeType())
	assert.False(t, resp.FromCache())
	assert.Same(t, resp, req.LastValidResponse())

	// Range 1..7 with min threshold 3 → [1,3], [4,6], [7,7].
	assert.Equal(t, map[string]string{"0x1": "0x3", "0x4": "0x6", "0x7": "0x7"}, rec.ranges())
	for _, s := range rec.subs {
		assert.Equal(t, "arbtrace_filter", s.method, "sub-requests use the lower-cased method")
		assert.Equal(t, "*", s.skipCache, "skip-cache-read directive propagates to sub-requests")
		assert.Equal(t, 42, s.parent)
		assert.Equal(t, []interface{}{"0xaaa"}, s.fromAddress)
	}

	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	result, id := covTraceResult(t, jrr)
	assert.EqualValues(t, 42, id)
	assert.Equal(t, []map[string]string{
		{"from": "0x1", "to": "0x3"},
		{"from": "0x4", "to": "0x6"},
		{"from": "0x7", "to": "0x7"},
	}, result, "sub-results are concatenated in request order")
}

func TestCovTrace_NetworkPreForward_SubRequestFailure(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("upstream exploded on second half")
	net := &queryTestNetwork{
		cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}},
		forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, _ := req.JsonRpcRequest()
			jrq.RLock()
			fb := jrq.Params[0].(map[string]interface{})["fromBlock"]
			jrq.RUnlock()
			if fb == "0x3" {
				return nil, boom
			}
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`[]`), nil),
			), nil
		},
	}
	ups := []common.Upstream{&queryTestConfigUpstream{cfg: &common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 2}}}}
	req := covTraceReq(t, "trace_filter", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})

	handled, resp, err := networkPreForward_trace_filter(ctx, net, ups, req)
	assert.True(t, handled)
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
}

func TestCovTrace_NetworkPostForward(t *testing.T) {
	ctx := context.Background()
	splitCfg := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitOnError: util.BoolPtr(true)}}
	largeRange := common.NewErrJsonRpcExceptionInternal(-32005, common.JsonRpcErrorEvmLargeRange, "query returned more than 10000 results", nil, nil)

	t.Run("normalized_large_range_code_triggers_split_and_releases_original", func(t *testing.T) {
		rec := &covTraceRecorder{}
		net := covTraceNet(splitCfg, 0, rec)
		req := covTraceReq(t, "trace_filter", 7, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})
		req.SetDirectives(&common.RequestDirectives{SkipCacheRead: "true"})
		orig := common.NewNormalizedResponse().WithJsonRpcResponse(
			common.MustNewJsonRpcResponseFromBytes([]byte(`7`), []byte(`null`), nil),
		)

		out, err := networkPostForward_trace_filter(ctx, net, req, orig, largeRange)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.NotSame(t, orig, out)
		assert.Equal(t, common.CompositeTypeTraceFilterSplitOnError, req.CompositeType())
		assert.Equal(t, map[string]string{"0x1": "0x2", "0x3": "0x4"}, rec.ranges())
		for _, s := range rec.subs {
			assert.Equal(t, "true", s.skipCache)
		}
		jrr, err := out.JsonRpcResponse(ctx)
		require.NoError(t, err)
		result, id := covTraceResult(t, jrr)
		assert.EqualValues(t, 7, id)
		assert.Equal(t, []map[string]string{{"from": "0x1", "to": "0x2"}, {"from": "0x3", "to": "0x4"}}, result)
	})

	t.Run("unsplittable_request_keeps_original_error", func(t *testing.T) {
		net := covTraceNet(splitCfg, 0, nil)
		req := covTraceReq(t, "trace_filter", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x1"})
		out, err := networkPostForward_trace_filter(ctx, net, req, nil, largeRange)
		assert.Nil(t, out)
		assert.Same(t, largeRange, err)
		assert.False(t, req.IsCompositeRequest())
	})

	t.Run("sub_request_failure_keeps_original_error", func(t *testing.T) {
		net := &queryTestNetwork{cfg: splitCfg, forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("still too large")
		}}
		req := covTraceReq(t, "trace_filter", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})
		orig := common.NewNormalizedResponse()
		tooLarge := common.NewErrEndpointRequestTooLarge(errors.New("range too large"), common.EvmBlockRangeTooLarge)
		out, err := networkPostForward_trace_filter(ctx, net, req, orig, tooLarge)
		assert.Same(t, orig, out)
		assert.Same(t, tooLarge, err)
	})

	t.Run("non_trace_method_keeps_original_error", func(t *testing.T) {
		net := covTraceNet(splitCfg, 0, nil)
		req := covTraceReq(t, "eth_getLogs", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})
		out, err := networkPostForward_trace_filter(ctx, net, req, nil, largeRange)
		assert.Nil(t, out)
		assert.Same(t, largeRange, err)
	})

	t.Run("other_internal_codes_are_not_split", func(t *testing.T) {
		net := covTraceNet(splitCfg, 0, nil)
		req := covTraceReq(t, "trace_filter", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})
		other := common.NewErrJsonRpcExceptionInternal(-32000, common.JsonRpcErrorServerSideException, "header not found", nil, nil)
		out, err := networkPostForward_trace_filter(ctx, net, req, nil, other)
		assert.Nil(t, out)
		assert.Same(t, other, err)
	})

	t.Run("split_disabled_when_flag_unset_or_config_missing", func(t *testing.T) {
		req := covTraceReq(t, "trace_filter", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"})
		for _, cfg := range []*common.NetworkConfig{nil, {}, {Evm: &common.EvmNetworkConfig{}}} {
			out, err := networkPostForward_trace_filter(ctx, &queryTestNetwork{cfg: cfg}, req, nil, largeRange)
			assert.Nil(t, out)
			assert.Same(t, largeRange, err)
		}
	})
}

func TestCovTrace_UpstreamPreForward_ParamValidation(t *testing.T) {
	ctx := context.Background()
	newNet := func() *mockNetwork {
		n := new(mockNetwork)
		n.On("Id").Return("net1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{
			Integrity: &common.EvmIntegrityConfig{EnforceGetLogsBlockRange: util.BoolPtr(true)},
		}}).Maybe()
		return n
	}
	newUp := func() *mockEvmUpstream {
		u := new(mockEvmUpstream)
		u.On("Id").Return("u1").Maybe()
		return u
	}

	t.Run("non_evm_upstream_is_skipped", func(t *testing.T) {
		n := newNet()
		req := covTraceReq(t, "trace_filter", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x2"})
		handled, resp, err := upstreamPreForward_trace_filter(ctx, n, &queryTestConfigUpstream{}, req)
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
		n.AssertNotCalled(t, "Config")
	})

	passThrough := []struct {
		name   string
		params []interface{}
	}{
		{name: "empty_params", params: []interface{}{}},
		{name: "non_object_filter", params: []interface{}{"0x1"}},
		{name: "tag_fromBlock", params: []interface{}{map[string]interface{}{"fromBlock": "latest", "toBlock": "0x2"}}},
		{name: "missing_fromBlock", params: []interface{}{map[string]interface{}{"toBlock": "0x2"}}},
		{name: "tag_toBlock", params: []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "finalized"}}},
		{name: "missing_toBlock", params: []interface{}{map[string]interface{}{"fromBlock": "0x1"}}},
	}
	for _, tc := range passThrough {
		t.Run(tc.name, func(t *testing.T) {
			u := newUp()
			req := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("trace_filter", tc.params))
			handled, resp, err := upstreamPreForward_trace_filter(ctx, newNet(), u, req)
			assert.False(t, handled)
			assert.Nil(t, resp)
			assert.NoError(t, err)
			u.AssertNotCalled(t, "EvmAssertBlockAvailability", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}

	malformed := []struct {
		name   string
		filter map[string]interface{}
	}{
		{name: "invalid_hex_fromBlock", filter: map[string]interface{}{"fromBlock": "0xzz", "toBlock": "0x2"}},
		{name: "invalid_hex_toBlock", filter: map[string]interface{}{"fromBlock": "0x1", "toBlock": "0xqq"}},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			u := newUp()
			req := covTraceReq(t, "arbtrace_filter", 1, tc.filter)
			handled, resp, err := upstreamPreForward_trace_filter(ctx, newNet(), u, req)
			assert.True(t, handled)
			assert.Nil(t, resp)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid syntax")
			u.AssertNotCalled(t, "EvmAssertBlockAvailability", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestCovTrace_SplitTraceFilterRequest_EdgesAndMetrics(t *testing.T) {
	net := &queryTestNetwork{cfg: &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}}

	t.Run("rejects_unsupported_method_and_bad_params", func(t *testing.T) {
		cases := []struct {
			name   string
			method string
			params []interface{}
			want   string
		}{
			{name: "method", method: "eth_getLogs", params: []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x4"}}, want: "unsupported method: eth_getLogs"},
			{name: "empty_params", method: "trace_filter", params: []interface{}{}, want: "invalid params length"},
			{name: "non_object", method: "trace_filter", params: []interface{}{"0x1"}, want: "invalid filter format"},
		}
		for _, tc := range cases {
			req := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest(tc.method, tc.params))
			subs, err := splitTraceFilterRequest(req)
			assert.Nil(t, subs, tc.name)
			assert.EqualError(t, err, tc.want, tc.name)
		}
	})

	cases := []struct {
		name      string
		filter    map[string]interface{}
		dimension string
		want      []traceFilterSubRequest
	}{
		{
			name:      "block_range",
			filter:    map[string]interface{}{"fromBlock": "0xa", "toBlock": "0xc", "toAddress": []interface{}{"0xbbb"}},
			dimension: "block_range",
			want: []traceFilterSubRequest{
				{method: "trace_filter", fromBlock: 10, toBlock: 10, toAddress: []interface{}{"0xbbb"}},
				{method: "trace_filter", fromBlock: 11, toBlock: 12, toAddress: []interface{}{"0xbbb"}},
			},
		},
		{
			name:      "from_address",
			filter:    map[string]interface{}{"fromBlock": "0x5", "toBlock": "0x5", "fromAddress": []interface{}{"0x1", "0x2", "0x3"}},
			dimension: "from_address",
			want: []traceFilterSubRequest{
				{method: "trace_filter", fromBlock: 5, toBlock: 5, fromAddress: []interface{}{"0x1"}},
				{method: "trace_filter", fromBlock: 5, toBlock: 5, fromAddress: []interface{}{"0x2", "0x3"}},
			},
		},
		{
			name:      "to_address_after_single_from_address",
			filter:    map[string]interface{}{"fromBlock": "0x5", "toBlock": "0x5", "fromAddress": []interface{}{"0x1"}, "toAddress": []interface{}{"0x8", "0x9"}},
			dimension: "to_address",
			want: []traceFilterSubRequest{
				{method: "trace_filter", fromBlock: 5, toBlock: 5, fromAddress: []interface{}{"0x1"}, toAddress: []interface{}{"0x8"}},
				{method: "trace_filter", fromBlock: 5, toBlock: 5, fromAddress: []interface{}{"0x1"}, toAddress: []interface{}{"0x9"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := covTraceReq(t, "TRACE_FILTER", 1, tc.filter)
			req.SetNetwork(net)
			before := covTraceForcedSplits("trace_filter", tc.dimension)

			got, err := splitTraceFilterRequest(req)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, before+1, covTraceForcedSplits("trace_filter", tc.dimension),
				"exactly one forced split recorded for dimension %s", tc.dimension)
		})
	}
}

// covTraceFailingWriter is a result writer whose materialization fails, used
// to exercise the clone failure path of the sub-request merger.
type covTraceFailingWriter struct{}

func (covTraceFailingWriter) WriteTo(w io.Writer, trimSides bool) (int64, error) {
	return 0, errors.New("result stream truncated")
}
func (covTraceFailingWriter) IsResultEmptyish() bool                   { return false }
func (covTraceFailingWriter) Size(ctx ...context.Context) (int, error) { return 0, nil }

func TestCovTrace_ExecuteSubRequests_FailureModes(t *testing.T) {
	ctx := context.Background()
	cfg := &common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}
	subs := []traceFilterSubRequest{{method: "trace_filter", fromBlock: 1, toBlock: 1}}

	failingClone := &common.JsonRpcResponse{}
	failingClone.SetResultWriter(covTraceFailingWriter{})

	cases := []struct {
		name    string
		resp    func() *common.NormalizedResponse
		wantErr string
	}{
		{
			name:    "nil_response_without_error",
			resp:    func() *common.NormalizedResponse { return nil },
			wantErr: "unexpected empty json-rpc response",
		},
		{
			name: "json_rpc_error_payload",
			resp: func() *common.NormalizedResponse {
				return common.NewNormalizedResponse().WithJsonRpcResponse(
					common.MustNewJsonRpcResponseFromBytes([]byte(`1`), nil, []byte(`{"code":-32000,"message":"trace_filter is not available"}`)),
				)
			},
			wantErr: "trace_filter is not available",
		},
		{
			name:    "result_clone_failure",
			resp:    func() *common.NormalizedResponse { return common.NewNormalizedResponse().WithJsonRpcResponse(failingClone) },
			wantErr: "result stream truncated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net := &queryTestNetwork{cfg: cfg, forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return tc.resp(), nil
			}}
			req := covTraceReq(t, "trace_filter", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x1"})
			merged, fromCache, err := executeTraceFilterSubRequests(ctx, net, req, subs, "")
			assert.Nil(t, merged)
			assert.False(t, fromCache)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("failure_metric_counts_each_failed_sub_request", func(t *testing.T) {
		net := &queryTestNetwork{cfg: cfg, forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("upstream timeout")
		}}
		req := covTraceReq(t, "trace_filter", 1, map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x3"})
		failures := telemetry.CounterHandle(telemetry.MetricNetworkEvmTraceFilterSplitFailure, "test-project", "evm:1", "arbtrace_filter", "n/a", "unknown")
		before := testutil.ToFloat64(failures)
		three := []traceFilterSubRequest{
			{method: "arbtrace_filter", fromBlock: 1, toBlock: 1},
			{method: "arbtrace_filter", fromBlock: 2, toBlock: 2},
			{method: "arbtrace_filter", fromBlock: 3, toBlock: 3},
		}
		_, _, err := executeTraceFilterSubRequests(ctx, net, req, three, "")
		require.Error(t, err)
		assert.Equal(t, before+3, testutil.ToFloat64(failures))
	})

	t.Run("all_sub_responses_from_cache_marks_merged_as_cached", func(t *testing.T) {
		net := &queryTestNetwork{cfg: cfg, forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return common.NewNormalizedResponse().WithFromCache(true).WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`[{"from":"c"}]`), nil),
			), nil
		}}
		req := covTraceReq(t, "trace_filter", "abc", map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x2"})
		two := []traceFilterSubRequest{{method: "trace_filter", fromBlock: 1, toBlock: 1}, {method: "trace_filter", fromBlock: 2, toBlock: 2}}
		merged, fromCache, err := executeTraceFilterSubRequests(ctx, net, req, two, "")
		require.NoError(t, err)
		assert.True(t, fromCache)
		result, id := covTraceResult(t, merged)
		assert.Equal(t, "abc", id)
		assert.Equal(t, []map[string]string{{"from": "c"}, {"from": "c"}}, result)
	})

	t.Run("unmarshalable_request_id_fails_merge", func(t *testing.T) {
		net := covTraceNet(cfg, 0, nil)
		jrq := common.NewJsonRpcRequest("trace_filter", []interface{}{map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x1"}})
		jrq.ID = make(chan int)
		req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
		merged, _, err := executeTraceFilterSubRequests(ctx, net, req, subs, "")
		assert.Nil(t, merged)
		assert.Error(t, err)
	})
}
