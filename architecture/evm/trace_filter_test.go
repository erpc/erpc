package evm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// createTestTraceFilterRequest builds a NormalizedRequest for either
// "trace_filter" or "arbtrace_filter" with the given filter map.
func createTestTraceFilterRequest(method string, filter map[string]interface{}) *common.NormalizedRequest {
	params := []interface{}{filter}
	jrq := common.NewJsonRpcRequest(method, params)
	return common.NewNormalizedRequestFromJsonRpcRequest(jrq)
}

func init() {
	util.ConfigureTestLogger()
}

func TestUpstreamPreForward_trace_filter(t *testing.T) {
	ctx := context.Background()

	makeNetwork := func() *mock.Mock {
		n := new(mockNetwork)
		n.On("Id").Return("net1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(true),
				},
			},
		}).Maybe()
		return &n.Mock
	}

	createTraceFilterRequest := func(filter map[string]interface{}) *common.NormalizedRequest {
		jrq := &common.JsonRpcRequest{
			Method: "trace_filter",
			Params: []interface{}{filter},
		}
		return common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	}

	t.Run("skips_when_integrity_check_disabled", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Id").Return("net1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(false),
				},
			},
		}).Maybe()

		u := new(mockEvmUpstream)
		r := createTraceFilterRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x10",
		})

		handled, resp, err := upstreamPreForward_trace_filter(ctx, n, u, r)
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})

	t.Run("passes_when_both_bounds_available", func(t *testing.T) {
		n := new(mockNetwork)
		makeNetwork()
		n.On("Id").Return("net1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(true),
				},
			},
		}).Maybe()

		u := new(mockEvmUpstream)
		poller := &fixedPoller{latest: 100, finalized: 100}
		u.On("Id").Return("u1").Maybe()
		u.On("EvmStatePoller").Return(poller).Maybe()
		// Both bounds are available
		u.On("EvmAssertBlockAvailability", mock.Anything, "trace_filter", common.AvailbilityConfidenceBlockHead, true, int64(16)).
			Return(true, nil).Once()
		u.On("EvmAssertBlockAvailability", mock.Anything, "trace_filter", common.AvailbilityConfidenceBlockHead, false, int64(1)).
			Return(true, nil).Once()

		r := createTraceFilterRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x10",
		})

		handled, resp, err := upstreamPreForward_trace_filter(ctx, n, u, r)
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})

	t.Run("fails_when_toBlock_unavailable", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Id").Return("net1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(true),
				},
			},
		}).Maybe()

		u := new(mockEvmUpstream)
		poller := &fixedPoller{latest: 10, finalized: 10}
		u.On("Id").Return("u1").Maybe()
		u.On("EvmStatePoller").Return(poller).Maybe()
		// toBlock=100 is not available
		u.On("EvmAssertBlockAvailability", mock.Anything, "trace_filter", common.AvailbilityConfidenceBlockHead, true, int64(100)).
			Return(false, nil).Once()

		r := createTraceFilterRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x64",
		})

		handled, resp, err := upstreamPreForward_trace_filter(ctx, n, u, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable))
	})

	t.Run("fails_when_fromBlock_unavailable", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Id").Return("net1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(true),
				},
			},
		}).Maybe()

		u := new(mockEvmUpstream)
		poller := &fixedPoller{latest: 1000, finalized: 950}
		u.On("Id").Return("u1").Maybe()
		u.On("EvmStatePoller").Return(poller).Maybe()
		// toBlock is available
		u.On("EvmAssertBlockAvailability", mock.Anything, "trace_filter", common.AvailbilityConfidenceBlockHead, true, int64(100)).
			Return(true, nil).Once()
		// fromBlock=1 is not available (pruned node)
		u.On("EvmAssertBlockAvailability", mock.Anything, "trace_filter", common.AvailbilityConfidenceBlockHead, false, int64(1)).
			Return(false, nil).Once()

		r := createTraceFilterRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x64",
		})

		handled, resp, err := upstreamPreForward_trace_filter(ctx, n, u, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable))
	})

	t.Run("fails_when_fromBlock_greater_than_toBlock", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Id").Return("net1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(true),
				},
			},
		}).Maybe()

		u := new(mockEvmUpstream)
		u.On("Id").Return("u1").Maybe()

		r := createTraceFilterRequest(map[string]interface{}{
			"fromBlock": "0x100",
			"toBlock":   "0x10",
		})

		handled, resp, err := upstreamPreForward_trace_filter(ctx, n, u, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeInvalidRequest))
	})
}

func TestUpstreamPreForward_arbtrace_filter(t *testing.T) {
	ctx := context.Background()

	createArbtraceFilterRequest := func(filter map[string]interface{}) *common.NormalizedRequest {
		jrq := &common.JsonRpcRequest{
			Method: "arbtrace_filter",
			Params: []interface{}{filter},
		}
		return common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	}

	t.Run("passes_when_both_bounds_available", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Id").Return("net1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm: &common.EvmNetworkConfig{
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(true),
				},
			},
		}).Maybe()

		u := new(mockEvmUpstream)
		poller := &fixedPoller{latest: 100, finalized: 100}
		u.On("Id").Return("u1").Maybe()
		u.On("EvmStatePoller").Return(poller).Maybe()
		// Both bounds are available
		u.On("EvmAssertBlockAvailability", mock.Anything, "arbtrace_filter", common.AvailbilityConfidenceBlockHead, true, int64(16)).
			Return(true, nil).Once()
		u.On("EvmAssertBlockAvailability", mock.Anything, "arbtrace_filter", common.AvailbilityConfidenceBlockHead, false, int64(1)).
			Return(true, nil).Once()

		r := createArbtraceFilterRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x10",
		})

		handled, resp, err := upstreamPreForward_trace_filter(ctx, n, u, r)
		assert.False(t, handled)
		assert.Nil(t, resp)
		assert.NoError(t, err)
	})
}

func TestIsTraceFilterMethod(t *testing.T) {
	assert.True(t, isTraceFilterMethod("trace_filter"))
	assert.True(t, isTraceFilterMethod("arbtrace_filter"))
	assert.True(t, isTraceFilterMethod("TRACE_FILTER"))
	assert.False(t, isTraceFilterMethod("eth_getLogs"))
	assert.False(t, isTraceFilterMethod("trace_block"))
	assert.False(t, isTraceFilterMethod(""))
}

func TestBuildTraceFilterRequest(t *testing.T) {
	t.Run("trace_filter_with_addresses", func(t *testing.T) {
		jrq, err := BuildTraceFilterRequest("trace_filter", 1, 16,
			[]interface{}{"0xaaa"},
			[]interface{}{"0xbbb"})
		assert.NoError(t, err)
		assert.Equal(t, "trace_filter", jrq.Method)
		filter := jrq.Params[0].(map[string]interface{})
		assert.Equal(t, "0x1", filter["fromBlock"])
		assert.Equal(t, "0x10", filter["toBlock"])
		assert.Equal(t, []interface{}{"0xaaa"}, filter["fromAddress"])
		assert.Equal(t, []interface{}{"0xbbb"}, filter["toAddress"])
	})

	t.Run("arbtrace_filter_without_addresses", func(t *testing.T) {
		jrq, err := BuildTraceFilterRequest("arbtrace_filter", 0, 1, nil, nil)
		assert.NoError(t, err)
		assert.Equal(t, "arbtrace_filter", jrq.Method)
		filter := jrq.Params[0].(map[string]interface{})
		_, hasFrom := filter["fromAddress"]
		_, hasTo := filter["toAddress"]
		assert.False(t, hasFrom)
		assert.False(t, hasTo)
	})
}

func TestSplitTraceFilterRequest(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		filter      map[string]interface{}
		expected    []traceFilterSubRequest
		expectError bool
	}{
		{
			name:   "split_by_block_range_even",
			method: "trace_filter",
			filter: map[string]interface{}{
				"fromBlock":   "0x1",
				"toBlock":     "0x4",
				"fromAddress": []interface{}{"0xaaa"},
			},
			expected: []traceFilterSubRequest{
				{method: "trace_filter", fromBlock: 1, toBlock: 2, fromAddress: []interface{}{"0xaaa"}, toAddress: nil},
				{method: "trace_filter", fromBlock: 3, toBlock: 4, fromAddress: []interface{}{"0xaaa"}, toAddress: nil},
			},
		},
		{
			name:   "split_by_block_range_odd",
			method: "trace_filter",
			filter: map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x5",
			},
			expected: []traceFilterSubRequest{
				{method: "trace_filter", fromBlock: 1, toBlock: 2},
				{method: "trace_filter", fromBlock: 3, toBlock: 5},
			},
		},
		{
			name:   "single_block_split_by_fromAddress",
			method: "trace_filter",
			filter: map[string]interface{}{
				"fromBlock":   "0x10",
				"toBlock":     "0x10",
				"fromAddress": []interface{}{"0xaaa", "0xbbb"},
				"toAddress":   []interface{}{"0xccc"},
			},
			expected: []traceFilterSubRequest{
				{method: "trace_filter", fromBlock: 16, toBlock: 16, fromAddress: []interface{}{"0xaaa"}, toAddress: []interface{}{"0xccc"}},
				{method: "trace_filter", fromBlock: 16, toBlock: 16, fromAddress: []interface{}{"0xbbb"}, toAddress: []interface{}{"0xccc"}},
			},
		},
		{
			name:   "single_block_split_by_toAddress",
			method: "trace_filter",
			filter: map[string]interface{}{
				"fromBlock": "0x10",
				"toBlock":   "0x10",
				"toAddress": []interface{}{"0xaaa", "0xbbb", "0xccc"},
			},
			expected: []traceFilterSubRequest{
				{method: "trace_filter", fromBlock: 16, toBlock: 16, toAddress: []interface{}{"0xaaa"}},
				{method: "trace_filter", fromBlock: 16, toBlock: 16, toAddress: []interface{}{"0xbbb", "0xccc"}},
			},
		},
		{
			name:   "arbtrace_filter_same_behavior",
			method: "arbtrace_filter",
			filter: map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x4",
			},
			expected: []traceFilterSubRequest{
				{method: "arbtrace_filter", fromBlock: 1, toBlock: 2},
				{method: "arbtrace_filter", fromBlock: 3, toBlock: 4},
			},
		},
		{
			name:   "cannot_split_further_single_block_no_addresses",
			method: "trace_filter",
			filter: map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x1",
			},
			expectError: true,
		},
		{
			name:   "cannot_split_single_block_single_address",
			method: "trace_filter",
			filter: map[string]interface{}{
				"fromBlock":   "0x1",
				"toBlock":     "0x1",
				"fromAddress": []interface{}{"0xaaa"},
			},
			expectError: true,
		},
		{
			name:   "invalid_fromBlock",
			method: "trace_filter",
			filter: map[string]interface{}{
				"fromBlock": "garbage",
				"toBlock":   "0x1",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := createTestTraceFilterRequest(tt.method, tt.filter)
			got, err := splitTraceFilterRequest(req)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestExecuteTraceFilterSubRequests(t *testing.T) {
	t.Run("successful_concurrent_execution_preserves_order", func(t *testing.T) {
		n := new(mockNetwork)
		u := new(mockEvmUpstream)

		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitConcurrency: 4}}).Maybe()
		n.On("ProjectId").Return("test").Maybe()
		n.On("Id").Return("evm:1").Maybe()
		n.On("Forward", mock.Anything, mock.Anything).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"b":1}]`), nil),
			), nil,
		).Once()
		n.On("Forward", mock.Anything, mock.Anything).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"b":2}]`), nil),
			), nil,
		).Once()
		u.On("Id").Return("rpc1").Maybe()
		u.On("NetworkId").Return("evm:1").Maybe()
		u.On("NetworkLabel").Return("evm:1").Maybe()
		u.On("VendorName").Return("test").Maybe()

		ctx := context.Background()
		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x4",
		})
		subs := []traceFilterSubRequest{
			{method: "trace_filter", fromBlock: 1, toBlock: 2},
			{method: "trace_filter", fromBlock: 3, toBlock: 4},
		}

		merged, fromCache, err := executeTraceFilterSubRequests(ctx, n, req, subs, "")
		assert.NoError(t, err)
		assert.NotNil(t, merged)
		assert.False(t, fromCache)
	})

	t.Run("any_sub_failure_fails_whole", func(t *testing.T) {
		n := new(mockNetwork)
		u := new(mockEvmUpstream)

		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitConcurrency: 4}}).Maybe()
		n.On("ProjectId").Return("test").Maybe()
		n.On("Id").Return("evm:1").Maybe()
		n.On("Forward", mock.Anything, mock.Anything).Return(nil, errors.New("upstream failed")).Maybe()
		u.On("Id").Return("rpc1").Maybe()
		u.On("NetworkId").Return("evm:1").Maybe()
		u.On("NetworkLabel").Return("evm:1").Maybe()
		u.On("VendorName").Return("test").Maybe()

		ctx := context.Background()
		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x4",
		})
		subs := []traceFilterSubRequest{
			{method: "trace_filter", fromBlock: 1, toBlock: 2},
			{method: "trace_filter", fromBlock: 3, toBlock: 4},
		}

		_, _, err := executeTraceFilterSubRequests(ctx, n, req, subs, "")
		assert.Error(t, err)
	})
}

func TestNetworkPreForward_trace_filter(t *testing.T) {
	ctx := context.Background()

	t.Run("no_split_when_range_below_threshold", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()
		u := new(mockEvmUpstream)
		u.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 10}}).Maybe()

		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x5",
		})
		handled, resp, err := networkPreForward_trace_filter(ctx, n, []common.Upstream{u}, req)
		assert.False(t, handled)
		assert.NoError(t, err)
		assert.Nil(t, resp)
	})

	t.Run("no_split_when_threshold_unset", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()
		u := new(mockEvmUpstream)
		u.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}).Maybe()

		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0xffff",
		})
		handled, resp, err := networkPreForward_trace_filter(ctx, n, []common.Upstream{u}, req)
		assert.False(t, handled)
		assert.NoError(t, err)
		assert.Nil(t, resp)
	})

	t.Run("proactive_split_uses_min_threshold_across_upstreams", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("ProjectId").Return("test").Maybe()
		n.On("Id").Return("evm:1").Maybe()
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()
		// Effective threshold is 2 (min of 2 and 5) → range 1..5 splits into [1,2], [3,4], [5,5].
		n.On("Forward", mock.Anything, mock.Anything).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[]`), nil),
			), nil,
		).Times(3)

		u1 := new(mockEvmUpstream)
		u1.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 2}}).Maybe()
		u1.On("Id").Return("u1").Maybe()
		u1.On("NetworkId").Return("evm:1").Maybe()
		u1.On("NetworkLabel").Return("evm:1").Maybe()
		u1.On("VendorName").Return("test").Maybe()
		u2 := new(mockEvmUpstream)
		u2.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 5}}).Maybe()
		u2.On("Id").Return("u2").Maybe()
		u2.On("NetworkId").Return("evm:1").Maybe()
		u2.On("NetworkLabel").Return("evm:1").Maybe()
		u2.On("VendorName").Return("test").Maybe()

		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x5",
		})
		handled, resp, err := networkPreForward_trace_filter(ctx, n, []common.Upstream{u1, u2}, req)
		assert.True(t, handled)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		n.AssertExpectations(t)
	})

	t.Run("skips_for_sub_requests", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()
		u := new(mockEvmUpstream)
		u.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{TraceFilterAutoSplittingRangeThreshold: 1}}).Maybe()

		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x10",
		})
		req.SetParentRequestId("some-parent")

		handled, resp, err := networkPreForward_trace_filter(ctx, n, []common.Upstream{u}, req)
		assert.False(t, handled)
		assert.NoError(t, err)
		assert.Nil(t, resp)
	})

	t.Run("returns_error_when_fromBlock_greater_than_toBlock", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()

		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x10",
			"toBlock":   "0x1",
		})
		handled, _, err := networkPreForward_trace_filter(ctx, n, nil, req)
		assert.True(t, handled)
		assert.Error(t, err)
	})
}

func TestNetworkPostForward_trace_filter(t *testing.T) {
	t.Run("no_error_passes_through", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitOnError: util.BoolPtr(true)}}).Maybe()
		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1", "toBlock": "0x2",
		})
		rs, re := networkPostForward_trace_filter(context.Background(), n, req, nil, nil)
		assert.Nil(t, rs)
		assert.NoError(t, re)
	})

	t.Run("disabled_when_flag_off", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitOnError: util.BoolPtr(false)}}).Maybe()
		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1", "toBlock": "0x4",
		})
		tooLarge := common.NewErrEndpointRequestTooLarge(errors.New("too large"), common.EvmBlockRangeTooLarge)
		_, re := networkPostForward_trace_filter(context.Background(), n, req, nil, tooLarge)
		assert.ErrorIs(t, re, tooLarge)
	})

	t.Run("ignores_non_too_large_errors", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitOnError: util.BoolPtr(true)}}).Maybe()
		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1", "toBlock": "0x4",
		})
		other := errors.New("connection refused")
		_, re := networkPostForward_trace_filter(context.Background(), n, req, nil, other)
		assert.ErrorIs(t, re, other)
	})

	t.Run("splits_on_too_large", func(t *testing.T) {
		n := new(mockNetwork)
		u := new(mockEvmUpstream)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitOnError: util.BoolPtr(true), TraceFilterSplitConcurrency: 4}}).Maybe()
		n.On("ProjectId").Return("test").Maybe()
		n.On("Id").Return("evm:1").Maybe()
		n.On("Forward", mock.Anything, mock.Anything).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"b":1}]`), nil),
			), nil,
		).Once()
		n.On("Forward", mock.Anything, mock.Anything).Return(
			common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"b":2}]`), nil),
			), nil,
		).Once()
		u.On("Id").Return("rpc1").Maybe()
		u.On("NetworkId").Return("evm:1").Maybe()
		u.On("NetworkLabel").Return("evm:1").Maybe()
		u.On("VendorName").Return("test").Maybe()

		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1", "toBlock": "0x4",
		})
		tooLarge := common.NewErrEndpointRequestTooLarge(errors.New("too many results"), common.EvmBlockRangeTooLarge)
		rs, re := networkPostForward_trace_filter(context.Background(), n, req, nil, tooLarge)
		assert.NoError(t, re)
		assert.NotNil(t, rs)
	})

	t.Run("skips_for_sub_requests", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{TraceFilterSplitOnError: util.BoolPtr(true)}}).Maybe()
		req := createTestTraceFilterRequest("trace_filter", map[string]interface{}{
			"fromBlock": "0x1", "toBlock": "0x4",
		})
		req.SetParentRequestId("some-parent-id")
		tooLarge := common.NewErrEndpointRequestTooLarge(errors.New("too large"), common.EvmBlockRangeTooLarge)
		_, re := networkPostForward_trace_filter(context.Background(), n, req, nil, tooLarge)
		assert.ErrorIs(t, re, tooLarge)
	})
}
