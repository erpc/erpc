package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

var _ common.Network = (*mockNetwork)(nil)

type mockNetwork struct {
	mock.Mock
	common.Network
}

func (m *mockNetwork) Id() string {
	args := m.Called()
	return args.Get(0).(string)
}

func (m *mockNetwork) Forward(
	ctx context.Context,
	req *common.NormalizedRequest,
) (*common.NormalizedResponse, error) {
	args := m.Called(ctx, req)
	// Retrieve arguments from the mock and return them
	respFn, eFn := args.Get(0).(func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error))
	if respFn != nil {
		resp, err := respFn(ctx, req)
		if resp == nil && err == nil {
			panic("Forward() returned nil for both resp and error eFn: " + fmt.Sprintf("%T", eFn))
		}
		return resp, err
	}
	respRaw, eRs := args.Get(0).(*common.NormalizedResponse)
	err, eEr := args.Get(1).(error)
	if respRaw == nil && err == nil {
		panic("Forward() returned nil for both resp and error eFn: " + fmt.Sprintf("%T", eFn) + " eRs: " + fmt.Sprintf("%T", eRs) + " eEr: " + fmt.Sprintf("%T", eEr))
	}
	return respRaw, err
}

func (m *mockNetwork) Config() *common.NetworkConfig {
	args := m.Called()
	return args.Get(0).(*common.NetworkConfig)
}

func (m *mockNetwork) ProjectId() string {
	args := m.Called()
	return args.Get(0).(string)
}

var _ common.EvmUpstream = (*mockEvmUpstream)(nil)

type mockEvmUpstream struct {
	mock.Mock
	common.EvmUpstream
}

func (m *mockEvmUpstream) Logger() *zerolog.Logger {
	return &log.Logger
}

func (m *mockEvmUpstream) Config() *common.UpstreamConfig {
	args := m.Called()
	return args.Get(0).(*common.UpstreamConfig)
}

func (m *mockEvmUpstream) NetworkId() string {
	args := m.Called()
	return args.Get(0).(string)
}

func (m *mockEvmUpstream) Id() string {
	args := m.Called()
	return args.Get(0).(string)
}

func (m *mockEvmUpstream) VendorName() string {
	args := m.Called()
	return args.Get(0).(string)
}

func (m *mockEvmUpstream) EvmStatePoller() common.EvmStatePoller {
	args := m.Called()
	return args.Get(0).(common.EvmStatePoller)
}

func (m *mockEvmUpstream) EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence common.AvailbilityConfidence, forceRefreshIfStale bool, blockNumber int64) (bool, error) {
	args := m.Called(ctx, forMethod, confidence, forceRefreshIfStale, blockNumber)
	return args.Bool(0), args.Error(1)
}

var _ common.EvmStatePoller = (*mockStatePoller)(nil)

type mockStatePoller struct {
	mock.Mock
	common.EvmStatePoller
}

func (m *mockStatePoller) LatestBlock() int64 {
	args := m.Called()
	lb, ok := args.Get(0).(int64)
	if !ok {
		panic("LatestBlock() returned non-int64 value")
	}
	return lb
}

func (m *mockStatePoller) IsObjectNull() bool {
	return false
}

func TestSplitEthGetLogsRequest(t *testing.T) {
	tests := []struct {
		name          string
		request       *common.NormalizedRequest
		lastErr       error
		expectedSplit []ethGetLogsSubRequest
		expectError   bool
	}{
		{
			name: "split by block range",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x4",
				"address":   "0x123",
				"topics":    []interface{}{"0xabc"},
			}),
			expectedSplit: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 2, address: "0x123", topics: []interface{}{"0xabc"}},
				{fromBlock: 3, toBlock: 4, address: "0x123", topics: []interface{}{"0xabc"}},
			},
		},
		{
			name: "split by addresses",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x1",
				"address":   []interface{}{"0x123", "0x456"},
				"topics":    []interface{}{"0xabc"},
			}),
			expectedSplit: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 1, address: []interface{}{"0x123"}, topics: []interface{}{"0xabc"}},
				{fromBlock: 1, toBlock: 1, address: []interface{}{"0x456"}, topics: []interface{}{"0xabc"}},
			},
		},
		{
			name: "split by topics",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x1",
				"address":   "0x123",
				"topics":    []interface{}{"0xabc", "0xdef"},
			}),
			expectedSplit: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{"0xabc"}},
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{"0xdef"}},
			},
		},
		{
			name: "invalid fromBlock",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "invalid",
				"toBlock":   "0x1",
			}),
			expectError: true,
		},
		{
			name: "cannot split further",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x1",
				"address":   "0x123",
				"topics":    []interface{}{"0xabc"},
			}),
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := splitEthGetLogsRequest(tt.request)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedSplit, result)
		})
	}
}

func TestExecuteGetLogsSubRequests(t *testing.T) {
	tests := []struct {
		name        string
		subRequests []ethGetLogsSubRequest
		mockSetup   func(*mockNetwork, *mockEvmUpstream)
		expectError bool
	}{
		{
			name: "successful execution",
			subRequests: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 2},
				{fromBlock: 3, toBlock: 4},
			},
			mockSetup: func(n *mockNetwork, u *mockEvmUpstream) {
				n.On("ProjectId").Return("test")
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						&common.JsonRpcResponse{Result: []byte(`["log1"]`)},
					),
					nil,
				).Once()
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						&common.JsonRpcResponse{Result: []byte(`["log2"]`)},
					),
					nil,
				).Once()
				u.On("Id").Return("rpc1")
				u.On("NetworkId").Return("evm:123")
				u.On("VendorName").Return("test")
			},
		},
		{
			name: "partial failure",
			subRequests: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 2},
				{fromBlock: 3, toBlock: 4},
			},
			mockSetup: func(n *mockNetwork, u *mockEvmUpstream) {
				n.On("Id").Return("evm:123")
				n.On("ProjectId").Return("test")
				n.On("Forward", mock.Anything, mock.Anything).
					Return(
						common.NewNormalizedResponse().WithJsonRpcResponse(
							&common.JsonRpcResponse{Result: []byte(`["log2"]`)},
						),
						nil,
					).Times(1).
					Return(nil, errors.New("failed")).Times(3)
				u.On("Id").Return("rpc1")
				u.On("NetworkId").Return("evm:123")
				u.On("VendorName").Return("test")
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockNetwork := new(mockNetwork)
			mockUpstream := new(mockEvmUpstream)

			if tt.mockSetup != nil {
				tt.mockSetup(mockNetwork, mockUpstream)
			}

			ctx := context.Background()
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2","address":"0x123","topics":["0xabc"]}],"id":1}`))

			result, err := executeGetLogsSubRequests(ctx, mockNetwork, mockUpstream, req, tt.subRequests, false)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.NotNil(t, result)

			mockNetwork.AssertExpectations(t)
			mockUpstream.AssertExpectations(t)
		})
	}
}

func TestUpstreamPreForward_eth_getLogs(t *testing.T) {
	tests := []struct {
		name        string
		setup       func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest)
		expectSplit bool
		expectError bool
	}{
		{
			name: "range_within_limits",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x5",
				})
				n.On("Id").Return("evm:123")
				n.On("Config").Return(&common.NetworkConfig{
					Evm: &common.EvmNetworkConfig{
						Integrity: &common.EvmIntegrityConfig{
							EnforceGetLogsBlockRange: util.BoolPtr(true),
							EnforceHighestBlock:      util.BoolPtr(true),
						},
					},
				})
				u.On("Id").Return("rpc1")
				u.On("Config").Return(&common.UpstreamConfig{
					Evm: nil,
				})
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Mock the new EvmAssertBlockAvailability calls
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(5)).Return(true, nil)

				return n, u, r
			},
			expectSplit: false,
			expectError: false,
		},
		{
			name: "range_exceeds_auto_splitting_range_threshold",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x15", // 21 blocks
				})
				n.On("Id").Return("evm:123")
				n.On("Config").Return(&common.NetworkConfig{
					Evm: &common.EvmNetworkConfig{
						Integrity: &common.EvmIntegrityConfig{
							EnforceGetLogsBlockRange: util.BoolPtr(true),
							EnforceHighestBlock:      util.BoolPtr(true),
						},
					},
				})
				n.On("ProjectId").Return("test")
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						&common.JsonRpcResponse{Result: []byte(`["log1"]`)},
					),
					nil,
				).Times(3)
				u.On("Config").Return(&common.UpstreamConfig{
					Evm: &common.EvmUpstreamConfig{
						GetLogsAutoSplittingRangeThreshold: 10,
					},
				})
				u.On("Id").Return("rpc1")
				u.On("NetworkId").Return("evm:123")
				u.On("VendorName").Return("test")
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Mock the new EvmAssertBlockAvailability calls
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(21)).Return(true, nil)

				return n, u, r
			},
			expectSplit: true,
			expectError: false,
		},
		{
			name: "range_exceeds_max_allowed_range_hard_limit",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x14", // 20 blocks
				})
				n.On("Id").Return("evm:123")
				n.On("Config").Return(&common.NetworkConfig{
					Evm: &common.EvmNetworkConfig{
						Integrity: &common.EvmIntegrityConfig{
							EnforceGetLogsBlockRange: util.BoolPtr(true),
							EnforceHighestBlock:      util.BoolPtr(true),
						},
					},
				})
				n.On("ProjectId").Return("test")
				// We do NOT expect "Forward" to be called at all, because we won't split
				// (the range is above the allowed limit => immediate error)
				u.On("Config").Return(&common.UpstreamConfig{
					Evm: &common.EvmUpstreamConfig{
						GetLogsMaxAllowedRange: 10,
					},
				})
				u.On("Id").Return("rpc1")
				u.On("NetworkId").Return("evm:123")
				u.On("VendorName").Return("test")
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Mock the new EvmAssertBlockAvailability calls - this should fail on the first call
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)

				return n, u, r
			},
			expectSplit: false,
			expectError: true,
		},
		{
			name: "address_list_exceeds_max_allowed_addresses_hard_limit",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)

				// Suppose we have 3 addresses while limit is 2
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x2",
					"address":   []interface{}{"0xABC", "0xDEF", "0x123"},
				})
				n.On("Id").Return("evm:123")
				n.On("Config").Return(&common.NetworkConfig{
					Evm: &common.EvmNetworkConfig{
						Integrity: &common.EvmIntegrityConfig{
							EnforceGetLogsBlockRange: util.BoolPtr(true),
							EnforceHighestBlock:      util.BoolPtr(true),
						},
					},
				})
				// We don't expect any forward calls if we fail on addresses limit
				n.On("ProjectId").Return("test").Maybe()

				u.On("Config").Return(&common.UpstreamConfig{
					Evm: &common.EvmUpstreamConfig{
						GetLogsMaxAllowedAddresses: 2,
					},
				})
				u.On("Id").Return("rpc1")
				u.On("NetworkId").Return("evm:123")
				u.On("VendorName").Return("test")
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Mock the new EvmAssertBlockAvailability calls - this should fail on the first call
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)

				return n, u, r
			},
			expectSplit: false,
			expectError: true,
		},
		{
			name: "topics_list_exceeds_max_allowed_topics_hard_limit",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x2",
					"topics":    []interface{}{"0xAAA", "0xBBB", "0xCCC"},
				})

				n.On("Id").Return("evm:123")
				n.On("Config").Return(&common.NetworkConfig{
					Evm: &common.EvmNetworkConfig{
						Integrity: &common.EvmIntegrityConfig{
							EnforceGetLogsBlockRange: util.BoolPtr(true),
							EnforceHighestBlock:      util.BoolPtr(true),
						},
					},
				})
				n.On("ProjectId").Return("test").Maybe()

				u.On("Config").Return(&common.UpstreamConfig{
					Evm: &common.EvmUpstreamConfig{
						GetLogsMaxAllowedTopics: 2,
					},
				})
				u.On("Id").Return("rpc1")
				u.On("NetworkId").Return("evm:123")
				u.On("VendorName").Return("test")
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Mock the new EvmAssertBlockAvailability calls - this should fail on the first call
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)

				return n, u, r
			},
			expectSplit: false,
			expectError: true,
		},
		{
			name: "blockHash_is_present",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x5",
					"blockHash": "0x123",
				})
				n.On("Id").Return("evm:123")
				n.On("Config").Return(&common.NetworkConfig{
					Evm: &common.EvmNetworkConfig{
						Integrity: &common.EvmIntegrityConfig{
							EnforceGetLogsBlockRange: util.BoolPtr(true),
							EnforceHighestBlock:      util.BoolPtr(true),
						},
					},
				})
				u.On("Id").Return("rpc1")
				return n, u, r
			},
			expectSplit: false,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, u, r := tt.setup()

			handled, resp, err := upstreamPreForward_eth_getLogs(context.Background(), n, u, r)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.Equal(t, tt.expectSplit, handled)
			if handled {
				assert.NotNil(t, resp)
			}

			n.AssertExpectations(t)
			u.AssertExpectations(t)
		})
	}
}

func TestUpstreamPostForward_eth_getLogs(t *testing.T) {
	tests := []struct {
		name        string
		setup       func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest)
		inputError  error
		expectSplit bool
		expectError bool
	}{
		{
			name: "no_error",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(nil)

				n.On("Id").Return("evm:123")
				u.On("Id").Return("rpc1")

				return n, u, r
			},
			inputError:  nil,
			expectSplit: false,
		},
		{
			name: "auto_split_request_too_large_error",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x2",
				})
				n.On("Id").Return("evm:123")
				n.On("ProjectId").Return("test")
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						&common.JsonRpcResponse{Result: []byte(`["log1"]`)},
					),
					nil,
				).Times(1)
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						&common.JsonRpcResponse{Result: []byte(`["log2"]`)},
					),
					nil,
				).Times(1)
				u.On("Id").Return("rpc1")
				u.On("NetworkId").Return("evm:123")
				u.On("VendorName").Return("test")
				u.On("Config").Return(&common.UpstreamConfig{
					Evm: &common.EvmUpstreamConfig{
						GetLogsMaxBlockRange: 10,
						GetLogsSplitOnError:  util.BoolPtr(true),
					},
				})

				return n, u, r
			},
			inputError:  common.NewErrEndpointRequestTooLarge(errors.New("too large"), common.EvmBlockRangeTooLarge),
			expectSplit: true,
		},
		{
			name: "not_splitting_request_too_large_error",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x2",
				})
				n.On("Id").Return("evm:123")
				u.On("Id").Return("rpc1")
				u.On("Config").Return(&common.UpstreamConfig{
					Evm: &common.EvmUpstreamConfig{
						GetLogsMaxBlockRange: 10,
						GetLogsSplitOnError:  util.BoolPtr(false),
					},
				})

				return n, u, r
			},
			inputError:  common.NewErrEndpointRequestTooLarge(errors.New("too large"), common.EvmBlockRangeTooLarge),
			expectSplit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, u, r := tt.setup()

			resp, err := upstreamPostForward_eth_getLogs(
				context.Background(),
				n,
				u,
				r,
				common.NewNormalizedResponse(),
				tt.inputError,
				false,
			)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			if tt.expectSplit {
				assert.NotNil(t, resp)
			}

			n.AssertExpectations(t)
			u.AssertExpectations(t)
		})
	}
}

func TestGetLogsMultiResponseWriter_WithEmptySubResponse(t *testing.T) {
	nonEmpty := &common.JsonRpcResponse{Result: []byte(`[{"logIndex":1,"address":"0x123","topics":["0xabc"],"data":"0x1234567890"}]`)}
	emptyResp := &common.JsonRpcResponse{Result: []byte(`[]`)}
	nullResp := &common.JsonRpcResponse{Result: []byte(`null`)}

	t.Run("FirstFillLastEmpty", func(t *testing.T) {
		// Create the multi-response writer with both responses.
		writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
			nonEmpty,
			emptyResp,
		})

		var buf bytes.Buffer
		writtenBytes, err := writer.WriteTo(&buf, false)
		assert.NoError(t, err, "WriteTo should not return an error")
		assert.Greater(t, writtenBytes, int64(0), "WriteTo should write some bytes")

		mergedJSON := buf.Bytes()
		// Validate that the merged JSON output is valid.
		assert.True(t, json.Valid(mergedJSON), "Merged JSON should be valid")

		// Unmarshal the merged JSON to verify its structure.
		var logs []map[string]interface{}
		err = json.Unmarshal(mergedJSON, &logs)
		assert.NoError(t, err, "Unmarshal of merged JSON should succeed")

		assert.Equal(t, 1, len(logs), "Expected only one log from non-empty sub-response")
		if len(logs) > 0 {
			assert.Equal(t, "0x1234567890", logs[0]["data"], "Expected the log to be '0x1234567890'")
		} else {
			assert.Fail(t, "Expected at least one log from non-empty sub-response")
		}
	})

	t.Run("FirstEmptyLastFill", func(t *testing.T) {
		// Create the multi-response writer with both responses.
		writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
			emptyResp,
			nonEmpty,
		})

		var buf bytes.Buffer
		writtenBytes, err := writer.WriteTo(&buf, false)
		assert.NoError(t, err, "WriteTo should not return an error")
		assert.Greater(t, writtenBytes, int64(0), "WriteTo should write some bytes")

		mergedJSON := buf.Bytes()
		// Validate that the merged JSON output is valid.
		assert.True(t, json.Valid(mergedJSON), "Merged JSON should be valid")

		// Unmarshal the merged JSON to verify its structure.
		var logs []map[string]interface{}
		err = json.Unmarshal(mergedJSON, &logs)
		assert.NoError(t, err, "Unmarshal of merged JSON should succeed")

		assert.Equal(t, 1, len(logs), "Expected only one log from non-empty sub-response")
		if len(logs) > 0 {
			assert.Equal(t, "0x1234567890", logs[0]["data"], "Expected the log to be '0x1234567890'")
		} else {
			assert.Fail(t, "Expected at least one log from non-empty sub-response")
		}
	})

	t.Run("FirstFillMidNullLastEmpty", func(t *testing.T) {
		// Create the multi-response writer with both responses.
		writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
			emptyResp,
			nonEmpty,
		})

		var buf bytes.Buffer
		writtenBytes, err := writer.WriteTo(&buf, false)
		assert.NoError(t, err, "WriteTo should not return an error")
		assert.Greater(t, writtenBytes, int64(0), "WriteTo should write some bytes")

		mergedJSON := buf.Bytes()
		// Validate that the merged JSON output is valid.
		assert.True(t, json.Valid(mergedJSON), "Merged JSON should be valid")

		// Unmarshal the merged JSON to verify its structure.
		var logs []map[string]interface{}
		err = json.Unmarshal(mergedJSON, &logs)
		assert.NoError(t, err, "Unmarshal of merged JSON should succeed")

		assert.Equal(t, 1, len(logs), "Expected only one log from non-empty sub-response")
		if len(logs) > 0 {
			assert.Equal(t, "0x1234567890", logs[0]["data"], "Expected the log to be '0x1234567890'")
		} else {
			assert.Fail(t, "Expected at least one log from non-empty sub-response")
		}
	})

	t.Run("FirstNullMidMixLastEmpty", func(t *testing.T) {
		// Create the multi-response writer with both responses.
		writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{
			nullResp,
			nonEmpty,
			emptyResp,
			nullResp,
			emptyResp,
			nonEmpty,
		})

		var buf bytes.Buffer
		writtenBytes, err := writer.WriteTo(&buf, false)
		assert.NoError(t, err, "WriteTo should not return an error")
		assert.Greater(t, writtenBytes, int64(0), "WriteTo should write some bytes")

		mergedJSON := buf.Bytes()
		// Validate that the merged JSON output is valid.
		assert.True(t, json.Valid(mergedJSON), "Merged JSON should be valid")

		// Unmarshal the merged JSON to verify its structure.
		var logs []map[string]interface{}
		err = json.Unmarshal(mergedJSON, &logs)
		assert.NoError(t, err, "Unmarshal of merged JSON should succeed")

		assert.Equal(t, 2, len(logs), "Expected only one log from non-empty sub-response")
		if len(logs) > 0 {
			assert.Equal(t, "0x1234567890", logs[0]["data"], "Expected the 1st log to be '0x1234567890'")
			assert.Equal(t, "0x1234567890", logs[1]["data"], "Expected the 2nd log to be '0x1234567890'")
		} else {
			assert.Fail(t, "Expected at least one log from non-empty sub-response")
		}
	})
}

func TestExecuteGetLogsSubRequests_WithNestedSplits(t *testing.T) {
	// Setup mocks
	mockNetwork := new(mockNetwork)
	mockUpstream := new(mockEvmUpstream)
	mockStatePoller := new(mockStatePoller)

	// Configure upstream
	mockUpstream.On("Config").Return(&common.UpstreamConfig{
		Evm: &common.EvmUpstreamConfig{
			GetLogsAutoSplittingRangeThreshold: 1,
		},
	})
	mockUpstream.On("Id").Return("rpc1")
	mockUpstream.On("NetworkId").Return("evm:1")
	mockUpstream.On("VendorName").Return("test")
	mockUpstream.On("EvmStatePoller").Return(mockStatePoller)
	mockStatePoller.On("LatestBlock").Return(int64(1000))

	// Setup request that will trigger nested splits
	req := createTestRequest(map[string]interface{}{
		"fromBlock": "0x1",
		"toBlock":   "0x4",
		"address":   []interface{}{"0x123", "0x456"}, // Will split by address
		"topics":    []interface{}{"0xabc", "0xdef"}, // Will split by topics
	})
	mockNetwork.On("ProjectId").Return("test")

	// Mock network responses for the nested splits
	// Each block range (0x1-0x2, 0x3-0x4) will be split by address (0x123, 0x456)
	// and then by topics (0xabc, 0xdef)
	mockNetwork.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			// Extract params to generate appropriate response
			jrr, err := r.JsonRpcRequest()
			assert.NoError(t, err)
			filter := jrr.Params[0].(map[string]interface{})

			fromBlock := filter["fromBlock"].(string)
			toBlock := filter["toBlock"].(string)

			// Generate response based on the combination
			var subJrr *common.JsonRpcResponse
			switch {
			case fromBlock == "0x1" && toBlock == "0x4":
				subJrr, err = executeGetLogsSubRequests(ctx, mockNetwork, mockUpstream, req, []ethGetLogsSubRequest{
					{fromBlock: 0x1, toBlock: 0x2, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
					{fromBlock: 0x3, toBlock: 0x4, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
				}, false)
				if err != nil {
					return nil, err
				}
			case fromBlock == "0x5" && toBlock == "0x8":
				subJrr, err = executeGetLogsSubRequests(ctx, mockNetwork, mockUpstream, req, []ethGetLogsSubRequest{
					{fromBlock: 0x5, toBlock: 0x6, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
					{fromBlock: 0x7, toBlock: 0x8, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
				}, false)
				if err != nil {
					return nil, err
				}

			case fromBlock == "0x1" && toBlock == "0x2":
				subJrr = &common.JsonRpcResponse{Result: []byte(`[{"logIndex":"0x1","address":"0x123","topics":["0xabc"],"data":"0x1"}]`)}
			case fromBlock == "0x3" && toBlock == "0x4":
				subJrr = &common.JsonRpcResponse{Result: []byte(`[{"logIndex":"0x2","address":"0x123","topics":["0xdef"],"data":"0x2"}]`)}

			case fromBlock == "0x5" && toBlock == "0x6":
				subJrr = &common.JsonRpcResponse{Result: []byte(`[{"logIndex":"0x3","address":"0x456","topics":["0xabc"],"data":"0x3"}]`)}
			case fromBlock == "0x7" && toBlock == "0x8":
				subJrr = &common.JsonRpcResponse{Result: []byte(`[{"logIndex":"0x4","address":"0x456","topics":["0xdef"],"data":"0x4"}]`)}
			}

			return common.NewNormalizedResponse().WithJsonRpcResponse(subJrr), nil
		},
		nil,
	).Times(6)

	// Execute the request
	jrr, err := executeGetLogsSubRequests(context.Background(), mockNetwork, mockUpstream, req, []ethGetLogsSubRequest{
		{fromBlock: 0x1, toBlock: 0x4, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
		{fromBlock: 0x5, toBlock: 0x8, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
	}, false)

	// Verify results
	assert.NoError(t, err)
	assert.NotNil(t, jrr)

	var buf bytes.Buffer
	jrr.WriteTo(&buf)

	var resp map[string]interface{}
	err = json.Unmarshal(buf.Bytes(), &resp)
	assert.NoError(t, err)
	result, ok := resp["result"].([]interface{})
	assert.True(t, ok)
	assert.Equal(t, 4, len(result), "Expected 4 logs instead of %d in the response: %s", len(result), buf.String())

	for _, log := range result {
		logMap, ok := log.(map[string]interface{})
		assert.True(t, ok)
		assert.Contains(t, logMap, "logIndex")
	}
}

func createTestRequest(filter interface{}) *common.NormalizedRequest {
	params := []interface{}{filter}
	jrq := common.NewJsonRpcRequest("eth_getLogs", params)
	return common.NewNormalizedRequestFromJsonRpcRequest(jrq)
}
