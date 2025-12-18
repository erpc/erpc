package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func init() {
	util.ConfigureTestLogger()
}

var _ common.Network = (*mockNetwork)(nil)

type mockNetwork struct {
	mock.Mock
	common.Network
}

// Provide a default Label implementation to avoid nil deref in tests when metric labels are used.
func (m *mockNetwork) Label() string {
	// If explicitly mocked, return mocked value
	for _, c := range m.ExpectedCalls {
		if c.Method == "Label" {
			args := m.Called()
			if s, ok := args.Get(0).(string); ok {
				return s
			}
		}
	}
	// Fallbacks: use Id if available, else a static test label
	for _, c := range m.ExpectedCalls {
		if c.Method == "Id" {
			return m.Id()
		}
	}
	return "test"
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

func (m *mockNetwork) Logger() *zerolog.Logger {
	return &log.Logger
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

func (m *mockEvmUpstream) NetworkLabel() string {
	// If explicitly mocked, use the mock value
	for _, c := range m.ExpectedCalls {
		if c.Method == "NetworkLabel" {
			args := m.Called()
			if s, ok := args.Get(0).(string); ok {
				return s
			}
		}
	}
	// Fallback to NetworkId
	return m.NetworkId()
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

func (m *mockStatePoller) FinalizedBlock() int64 {
	// If explicitly mocked, return mocked value
	for _, c := range m.ExpectedCalls {
		if c.Method == "FinalizedBlock" {
			args := m.Called()
			fb, ok := args.Get(0).(int64)
			if !ok {
				panic("FinalizedBlock() returned non-int64 value")
			}
			return fb
		}
	}

	// Fallback: if LatestBlock is mocked, reuse it; otherwise default to 0
	for _, c := range m.ExpectedCalls {
		if c.Method == "LatestBlock" {
			return m.LatestBlock()
		}
	}
	return 0
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
				"topics":    []interface{}{[]interface{}{"0xabc", "0xdef"}},
			}),
			expectedSplit: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{[]interface{}{"0xabc"}}},
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{[]interface{}{"0xdef"}}},
			},
		},
		{
			name: "split by topics - odd count",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x1",
				"address":   "0x123",
				// Use topic0 OR-list odd count
				"topics": []interface{}{[]interface{}{"0xabc", "0xdef", "0xghi"}},
			}),
			expectedSplit: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{[]interface{}{"0xabc"}}},
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{[]interface{}{"0xdef", "0xghi"}}},
			},
		},
		{
			name: "split by block range - odd count",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x5",
				"address":   "0x123",
				"topics":    []interface{}{"0xabc"},
			}),
			expectedSplit: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 2, address: "0x123", topics: []interface{}{"0xabc"}},
				{fromBlock: 3, toBlock: 5, address: "0x123", topics: []interface{}{"0xabc"}},
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
		{
			name: "split by topics0 OR list",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x1",
				"address":   "0x123",
				"topics":    []interface{}{[]interface{}{"0xabc", "0xdef"}},
			}),
			expectedSplit: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{[]interface{}{"0xabc"}}},
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{[]interface{}{"0xdef"}}},
			},
		},
		{
			name: "split by topics0 OR list - odd count",
			request: createTestRequest(map[string]interface{}{
				"fromBlock": "0x1",
				"toBlock":   "0x1",
				"address":   "0x123",
				"topics":    []interface{}{[]interface{}{"0xabc", "0xdef", "0xghi"}},
			}),
			expectedSplit: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{[]interface{}{"0xabc"}}},
				{fromBlock: 1, toBlock: 1, address: "0x123", topics: []interface{}{[]interface{}{"0xdef", "0xghi"}}},
			},
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
				n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitConcurrency: 200}}).Maybe()
				n.On("ProjectId").Return("test")
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`["log1"]`), nil),
					),
					nil,
				).Once()
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`["log2"]`), nil),
					),
					nil,
				).Once()
				u.On("Id").Return("rpc1").Maybe()
				u.On("NetworkId").Return("evm:123").Maybe()
				u.On("NetworkLabel").Return("evm:123").Maybe()
				u.On("VendorName").Return("test").Maybe()
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
				n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitConcurrency: 200}}).Maybe()
				n.On("ProjectId").Return("test")
				n.On("Forward", mock.Anything, mock.Anything).
					Return(
						func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
							return common.NewNormalizedResponse().WithJsonRpcResponse(
								common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`["log2"]`), nil),
							), nil
						},
						nil,
					).Times(1).
					Return(nil, errors.New("failed")).Times(3)
				u.On("Id").Return("rpc1").Maybe()
				u.On("NetworkId").Return("evm:123").Maybe()
				u.On("NetworkLabel").Return("evm:123").Maybe()
				u.On("VendorName").Return("test").Maybe()
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

			result, err := executeGetLogsSubRequests(ctx, mockNetwork, req, tt.subRequests, false)

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
				u.On("Id").Return("rpc1").Maybe()
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Mock the new EvmAssertBlockAvailability calls
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(5)).Return(true, nil)
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)

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
				u.On("Id").Return("rpc1").Maybe()
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Availability checks
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(21)).Return(true, nil)
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)

				return n, u, r
			},
			expectSplit: false,
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
				u.On("Id").Return("rpc1").Maybe()
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Availability checks
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(20)).Return(true, nil)
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)

				return n, u, r
			},
			expectSplit: false,
			expectError: false,
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
				u.On("Id").Return("rpc1").Maybe()
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Availability checks
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(2)).Return(true, nil)
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)

				return n, u, r
			},
			expectSplit: false,
			expectError: false,
		},
		{
			name: "topics_list_exceeds_max_allowed_topics_hard_limit",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x2",
					// Only topic0 is counted for limit; use OR-list in position 0
					"topics": []interface{}{[]interface{}{"0xAAA", "0xBBB", "0xCCC"}},
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
				u.On("Id").Return("rpc1").Maybe()
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))
				// Availability checks
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(2)).Return(true, nil)
				u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, false, int64(1)).Return(true, nil)

				return n, u, r
			},
			expectSplit: false,
			expectError: false,
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
				u.On("Id").Return("rpc1").Maybe()
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
			} else if err != nil {
				t.Fatalf("Unexpected error: %v", err)
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

func TestNetworkPostForward_eth_getLogs(t *testing.T) {
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

				n.On("Id").Return("evm:123").Maybe()
				u.On("Id").Return("rpc1").Maybe()

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
				n.On("Id").Return("evm:123").Maybe()
				n.On("ProjectId").Return("test")
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`["log1"]`), nil),
					),
					nil,
				).Times(1)
				n.On("Forward", mock.Anything, mock.Anything).Return(
					common.NewNormalizedResponse().WithJsonRpcResponse(
						common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`["log2"]`), nil),
					),
					nil,
				).Times(1)
				u.On("Id").Return("rpc1").Maybe()
				u.On("NetworkId").Return("evm:123").Maybe()
				u.On("NetworkLabel").Return("evm:123").Maybe()
				u.On("VendorName").Return("test").Maybe()
				n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitOnError: util.BoolPtr(true)}}).Maybe()

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
				n.On("Id").Return("evm:123").Maybe()
				u.On("Id").Return("rpc1").Maybe()
				n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitOnError: util.BoolPtr(false)}}).Maybe()

				return n, u, r
			},
			inputError:  common.NewErrEndpointRequestTooLarge(errors.New("too large"), common.EvmBlockRangeTooLarge),
			expectSplit: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, u, r := tt.setup()

			resp, err := networkPostForward_eth_getLogs(
				context.Background(),
				n,
				r,
				common.NewNormalizedResponse(),
				tt.inputError,
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
	nonEmpty := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"logIndex":1,"address":"0x123","topics":["0xabc"],"data":"0x1234567890"}]`), nil)
	emptyResp := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[]`), nil)
	nullResp := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`null`), nil)

	t.Run("FirstFillLastEmpty", func(t *testing.T) {
		// Create the multi-response writer with both responses.
		ne1, _ := nonEmpty.Clone()
		er1, _ := emptyResp.Clone()
		writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{ne1, er1})

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
		er2, _ := emptyResp.Clone()
		ne2, _ := nonEmpty.Clone()
		writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{er2, ne2})

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
		er3, _ := emptyResp.Clone()
		ne3, _ := nonEmpty.Clone()
		writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{er3, ne3})

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
		nu1, _ := nullResp.Clone()
		ne4, _ := nonEmpty.Clone()
		er4, _ := emptyResp.Clone()
		nu2, _ := nullResp.Clone()
		er5, _ := emptyResp.Clone()
		ne5, _ := nonEmpty.Clone()
		writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{nu1, ne4, er4, nu2, er5, ne5})

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

func TestGetLogsMultiResponseWriter_ReleaseIdempotentSizeAfterRelease(t *testing.T) {
	r1 := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[1]`), nil)
	r2 := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[2,3]`), nil)
	writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{r1, r2})
	sz1, err := writer.Size()
	assert.NoError(t, err)
	assert.Greater(t, sz1, 0)
	writer.Release()
	writer.Release() // idempotent
	sz2, err := writer.Size()
	assert.NoError(t, err)
	assert.Equal(t, 0, sz2)
}

// race-prone scenario: concurrently writing a merged result while subresponses are freed.
// This emulates HTTP server writing to client while cleanup kicks in.
func TestGetLogsMultiResponseWriter_ConcurrentWriteAndFree_NoParseBodyPanic(t *testing.T) {
	// Prepare two non-empty JsonRpcResponses
	r1 := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[1]`), nil)
	r2 := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[2]`), nil)
	writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{r1, r2})

	// Wrap into a parent response to exercise WriteTo path that uses writer.WriteTo
	parent := &common.JsonRpcResponse{}
	_ = parent.SetID("0xparent")
	parent.SetResultWriter(writer)

	// Concurrency: one goroutine writes the full response repeatedly; another frees aggressively
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			var buf bytes.Buffer
			// should never error due to missing body
			_, err := parent.WriteTo(&buf)
			if err != nil {
				// surface error for visibility without failing flaky runs immediately
				return
			}
		}
	}()

	// Meanwhile, repeatedly call Release on writer and on child responses to simulate cleanup
	for i := 0; i < 100; i++ {
		writer.Release()
		r1.Free()
		r2.Free()
		// Re-set resultWriter to keep WriteTo path active even after Release
		parent.SetResultWriter(writer)
	}

	<-done
	// Finally, attempt one more write to ensure stability post-concurrency
	var buf bytes.Buffer
	_, _ = parent.WriteTo(&buf)
}

// Ensure WriteResultTo handles a writer that is released mid-stream without panic.
func TestGetLogsMultiResponseWriter_SubResponseFreedDuringWrite_NoPanic(t *testing.T) {
	// Use real JsonRpcResponses only
	ne := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[9]`), nil)
	fr := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x2"`), []byte(`[1,2,3]`), nil)
	writer := NewGetLogsMultiResponseWriter([]*common.JsonRpcResponse{ne, fr})

	done := make(chan struct{})
	go func() {
		defer close(done)
		var buf bytes.Buffer
		// Attempt to write while another goroutine may free sub-responses
		_, _ = writer.WriteTo(&buf, false)
	}()

	// Concurrently free the second sub-response multiple times
	for i := 0; i < 100; i++ {
		fr.Free()
	}
	// Optionally release the writer to trigger internal frees
	writer.Release()
	<-done
}

func TestNetworkPostForward_NoSplitOnNonTooLargeError(t *testing.T) {
	n := new(mockNetwork)
	r := createTestRequest(map[string]interface{}{
		"fromBlock": "0x1",
		"toBlock":   "0x2",
	})
	n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitOnError: util.BoolPtr(true)}}).Maybe()
	resp, err := networkPostForward_eth_getLogs(context.Background(), n, r, nil, common.NewErrEndpointMissingData(errors.New("missing"), nil))
	assert.Nil(t, resp)
	assert.Error(t, err)
}

func TestNetworkPreForward_eth_getLogs_blockHash_with_filters(t *testing.T) {
	n := new(mockNetwork)
	r := createTestRequest(map[string]interface{}{
		"fromBlock": "0x1",
		"toBlock":   "0x5",
		"blockHash": "0xabc",
		"address":   []interface{}{"0xA"},
		"topics":    []interface{}{[]interface{}{"0xT"}},
	})
	handled, resp, err := networkPreForward_eth_getLogs(context.Background(), n, nil, r)
	assert.NoError(t, err)
	assert.False(t, handled)
	assert.Nil(t, resp)
}

func TestExecuteGetLogsSubRequests_DeterministicOrder(t *testing.T) {
	mockNetwork := new(mockNetwork)
	mockUpstream := new(mockEvmUpstream)
	mockNetwork.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitConcurrency: 10}}).Maybe()
	mockNetwork.On("ProjectId").Return("test")
	// Return sub-responses with artificial delays to scramble completion order
	mockNetwork.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jreq, _ := r.JsonRpcRequest()
			filter := jreq.Params[0].(map[string]interface{})
			fb := filter["fromBlock"].(string)
			if fb == "0x2" {
				time.Sleep(30 * time.Millisecond)
			} else if fb == "0x3" {
				time.Sleep(10 * time.Millisecond)
			}
			body := []byte(fmt.Sprintf(`["log-%s"]`, fb))
			return common.NewNormalizedResponse().WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), body, nil)), nil
		},
		nil,
	).Times(3)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x3"}],"id":1}`))
	subs := []ethGetLogsSubRequest{
		{fromBlock: 0x1, toBlock: 0x1},
		{fromBlock: 0x2, toBlock: 0x2},
		{fromBlock: 0x3, toBlock: 0x3},
	}
	jrr, err := executeGetLogsSubRequests(context.Background(), mockNetwork, req, subs, false)
	assert.NoError(t, err)
	var buf bytes.Buffer
	_, _ = jrr.WriteTo(&buf)
	var out map[string]interface{}
	_ = json.Unmarshal(buf.Bytes(), &out)
	arr := out["result"].([]interface{})
	assert.Equal(t, []interface{}{"log-0x1", "log-0x2", "log-0x3"}, arr)

	mockNetwork.AssertExpectations(t)
	mockUpstream.AssertExpectations(t)
}

func TestExecuteGetLogsSubRequests_WithNestedSplits(t *testing.T) {
	// Setup mocks
	mockNetwork := new(mockNetwork)
	mockUpstream := new(mockEvmUpstream)

	// Configure upstream
	mockUpstream.On("Config").Return(&common.UpstreamConfig{
		Evm: &common.EvmUpstreamConfig{
			GetLogsAutoSplittingRangeThreshold: 1,
		},
	})
	mockUpstream.On("Id").Return("rpc1").Maybe()
	mockUpstream.On("NetworkId").Return("evm:123").Maybe()
	mockUpstream.On("NetworkLabel").Return("evm:123").Maybe()
	mockUpstream.On("VendorName").Return("test").Maybe()
	mockUpstream.On("Config").Return(&common.UpstreamConfig{
		Evm: &common.EvmUpstreamConfig{
			GetLogsAutoSplittingRangeThreshold: 1,
		},
	})

	// Setup request that will trigger nested splits
	req := createTestRequest(map[string]interface{}{
		"fromBlock": "0x1",
		"toBlock":   "0x4",
		"address":   []interface{}{"0x123", "0x456"}, // Will split by address
		"topics":    []interface{}{"0xabc", "0xdef"}, // Will split by topics
	})
	mockNetwork.On("ProjectId").Return("test")
	mockNetwork.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{GetLogsSplitConcurrency: 200}}).Maybe()

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
				subJrr, err = executeGetLogsSubRequests(ctx, mockNetwork, req, []ethGetLogsSubRequest{
					{fromBlock: 0x1, toBlock: 0x2, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
					{fromBlock: 0x3, toBlock: 0x4, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
				}, false)
				if err != nil {
					return nil, err
				}
			case fromBlock == "0x5" && toBlock == "0x8":
				subJrr, err = executeGetLogsSubRequests(ctx, mockNetwork, req, []ethGetLogsSubRequest{
					{fromBlock: 0x5, toBlock: 0x6, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
					{fromBlock: 0x7, toBlock: 0x8, address: []interface{}{"0x123", "0x456"}, topics: []interface{}{"0xabc", "0xdef"}},
				}, false)
				if err != nil {
					return nil, err
				}

			case fromBlock == "0x1" && toBlock == "0x2":
				subJrr = common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"logIndex":"0x1","address":"0x123","topics":["0xabc"],"data":"0x1"}]`), nil)
			case fromBlock == "0x3" && toBlock == "0x4":
				subJrr = common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"logIndex":"0x2","address":"0x123","topics":["0xdef"],"data":"0x2"}]`), nil)

			case fromBlock == "0x5" && toBlock == "0x6":
				subJrr = common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"logIndex":"0x3","address":"0x456","topics":["0xabc"],"data":"0x3"}]`), nil)
			case fromBlock == "0x7" && toBlock == "0x8":
				subJrr = common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"logIndex":"0x4","address":"0x456","topics":["0xdef"],"data":"0x4"}]`), nil)
			}

			return common.NewNormalizedResponse().WithJsonRpcResponse(subJrr), nil
		},
		nil,
	).Times(6)

	// Execute the request
	jrr, err := executeGetLogsSubRequests(context.Background(), mockNetwork, req, []ethGetLogsSubRequest{
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

func TestNetworkPreForward_eth_getLogs(t *testing.T) {
	ctx := context.Background()

	t.Run("short_circuit_on_blockHash", func(t *testing.T) {
		n := new(mockNetwork)
		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x5",
			"blockHash": "0xabc",
		})

		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, nil, r)
		assert.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})

	t.Run("range_validation_error_when_from_gt_to", func(t *testing.T) {
		n := new(mockNetwork)
		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x5",
			"toBlock":   "0x1",
		})

		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, nil, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
	})

	t.Run("enforce_max_allowed_range_hard_limit", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{GetLogsMaxAllowedRange: 3},
		})
		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x5", // 5 blocks > 3
		})

		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, nil, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeGetLogsExceededMaxAllowedRange))
		n.AssertExpectations(t)
	})

	t.Run("enforce_max_allowed_addresses_hard_limit", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{GetLogsMaxAllowedAddresses: 2},
		})
		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x2",
			"address":   []interface{}{"0xA", "0xB", "0xC"},
		})

		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, nil, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeGetLogsExceededMaxAllowedAddresses))
		n.AssertExpectations(t)
	})

	t.Run("enforce_max_allowed_topics_hard_limit", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{GetLogsMaxAllowedTopics: 2},
		})
		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x2",
			"topics":    []interface{}{[]interface{}{"0xAAA", "0xBBB", "0xCCC"}},
		})

		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, nil, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeGetLogsExceededMaxAllowedTopics))
		n.AssertExpectations(t)
	})

	t.Run("proactive_split_by_block_range_and_merge", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("ProjectId").Return("test")
		n.On("Config").Return(&common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{},
		})

		// Upstreams with thresholds: min is 2
		u1 := new(mockEvmUpstream)
		u2 := new(mockEvmUpstream)
		u1.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{GetLogsAutoSplittingRangeThreshold: 2}})
		u2.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{GetLogsAutoSplittingRangeThreshold: 4}})

		// Expect 3 sub-requests for range 1..5 with threshold 2: [1-2],[3-4],[5-5]
		n.On("Forward", mock.Anything, mock.Anything).Return(
			func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				jrq, err := r.JsonRpcRequest()
				assert.NoError(t, err)
				filter := jrq.Params[0].(map[string]interface{})
				fb := filter["fromBlock"].(string)
				tb := filter["toBlock"].(string)
				body := []byte(fmt.Sprintf(`[{"data":"%s-%s"}]`, fb, tb))
				subJrr := common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), body, nil)
				return common.NewNormalizedResponse().WithJsonRpcResponse(subJrr), nil
			},
			nil,
		).Times(3)

		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x5",
		})

		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, []common.Upstream{u1, u2}, r)
		assert.True(t, handled)
		assert.NoError(t, err)
		assert.NotNil(t, resp)

		jrr, err := resp.JsonRpcResponse(ctx)
		assert.NoError(t, err)
		var buf bytes.Buffer
		_, _ = jrr.WriteTo(&buf)
		var out map[string]interface{}
		err = json.Unmarshal(buf.Bytes(), &out)
		assert.NoError(t, err)
		res, ok := out["result"].([]interface{})
		assert.True(t, ok)
		assert.Equal(t, 3, len(res))

		n.AssertExpectations(t)
		u1.AssertExpectations(t)
		u2.AssertExpectations(t)
	})

	t.Run("no_split_when_request_range_below_threshold", func(t *testing.T) {
		n := new(mockNetwork)
		n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}})
		u := new(mockEvmUpstream)
		u.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{GetLogsAutoSplittingRangeThreshold: 10}})

		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x1",
			"toBlock":   "0x5",
		})

		handled, resp, err := networkPreForward_eth_getLogs(ctx, n, []common.Upstream{u}, r)
		assert.False(t, handled)
		assert.NoError(t, err)
		assert.Nil(t, resp)

		n.AssertExpectations(t)
		u.AssertExpectations(t)
	})
}

func createTestRequest(filter interface{}) *common.NormalizedRequest {
	params := []interface{}{filter}
	jrq := common.NewJsonRpcRequest("eth_getLogs", params)
	return common.NewNormalizedRequestFromJsonRpcRequest(jrq)
}

func TestUpstreamPreForward_eth_getLogs_BlockHeadTolerance(t *testing.T) {
	ctx := context.Background()
	i64ptr := func(v int64) *int64 { return &v }

	makeNetwork := func(maxRetryable *int64) *mockNetwork {
		n := new(mockNetwork)
		n.On("Id").Return("evm:123").Maybe()
		n.On("ProjectId").Return("test").Maybe()
		n.On("Config").Return(&common.NetworkConfig{
			Evm: &common.EvmNetworkConfig{
				Integrity: &common.EvmIntegrityConfig{
					EnforceGetLogsBlockRange: util.BoolPtr(true),
				},
				MaxRetryableBlockDistance: maxRetryable,
			},
		}).Maybe()
		return n
	}

	t.Run("hard_fails_when_toBlock_ahead_by_1", func(t *testing.T) {
		n := makeNetwork(i64ptr(128))
		u := new(mockEvmUpstream)
		poller := &fixedPoller{latest: 100, finalized: 100}

		u.On("Id").Return("u1").Maybe()
		u.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}).Maybe()
		u.On("EvmStatePoller").Return(poller).Maybe()
		// toBlock=101, upstream says not available
		u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(101)).
			Return(false, nil).
			Once()

		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x65", // 101
			"toBlock":   "0x65",
		})

		handled, resp, err := upstreamPreForward_eth_getLogs(ctx, n, u, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	})

	t.Run("still_hard_fails_when_toBlock_ahead_beyond_maxRetryableDistance", func(t *testing.T) {
		n := makeNetwork(i64ptr(128))
		u := new(mockEvmUpstream)
		poller := &fixedPoller{latest: 100, finalized: 100}

		u.On("Id").Return("u1").Maybe()
		u.On("Config").Return(&common.UpstreamConfig{Evm: &common.EvmUpstreamConfig{}}).Maybe()
		u.On("EvmStatePoller").Return(poller).Maybe()
		// toBlock=301 (0x12d), upstream says not available
		u.On("EvmAssertBlockAvailability", mock.Anything, "eth_getLogs", common.AvailbilityConfidenceBlockHead, true, int64(301)).
			Return(false, nil).
			Once()

		r := createTestRequest(map[string]interface{}{
			"fromBlock": "0x12d",
			"toBlock":   "0x12d",
		})

		handled, resp, err := upstreamPreForward_eth_getLogs(ctx, n, u, r)
		assert.True(t, handled)
		assert.Nil(t, resp)
		assert.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointMissingData))
	})
}

type fixedPoller struct {
	common.EvmStatePoller
	latest    int64
	finalized int64
}

func (p *fixedPoller) LatestBlock() int64 { return p.latest }

func (p *fixedPoller) FinalizedBlock() int64 { return p.finalized }

func (p *fixedPoller) IsObjectNull() bool { return false }
