package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

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

