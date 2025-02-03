package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func (m *mockNetwork) Forward(
	ctx context.Context,
	req *common.NormalizedRequest,
) (*common.NormalizedResponse, error) {
	args := m.Called(ctx, req)
	// Retrieve arguments from the mock and return them
	resp, _ := args.Get(0).(*common.NormalizedResponse)
	err, _ := args.Get(1).(error)
	return resp, err
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

func (m *mockEvmUpstream) EvmStatePoller() common.EvmStatePoller {
	args := m.Called()
	return args.Get(0).(common.EvmStatePoller)
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
			},
		},
		{
			name: "partial failure",
			subRequests: []ethGetLogsSubRequest{
				{fromBlock: 1, toBlock: 2},
				{fromBlock: 3, toBlock: 4},
			},
			mockSetup: func(n *mockNetwork, u *mockEvmUpstream) {
				n.On("Forward", mock.Anything, mock.Anything).
					Return(
						common.NewNormalizedResponse().WithJsonRpcResponse(
							&common.JsonRpcResponse{Result: []byte(`["log2"]`)},
						),
						nil,
					).Times(1).
					Return(nil, errors.New("failed")).Times(3)
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
			name: "range within limits",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x5",
				})
				n.On("Config").Return(&common.NetworkConfig{
					Evm: &common.EvmNetworkConfig{
						Integrity: &common.EvmIntegrityConfig{
							EnforceGetLogsBlockRange: util.BoolPtr(true),
							EnforceHighestBlock:      util.BoolPtr(true),
						},
					},
				})
				// n.On("ProjectId").Return("test")
				u.On("Config").Return(&common.UpstreamConfig{
					Evm: nil,
				})
				// u.On("NetworkId").Return("evm:123")
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))

				return n, u, r
			},
			expectSplit: false,
		},
		{
			name: "range exceeds limits",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x15",
				})

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
						GetLogsMaxBlockRange: 10,
					},
				})
				u.On("NetworkId").Return("evm:123")
				stp := new(mockStatePoller)
				u.On("EvmStatePoller").Return(stp)
				stp.On("LatestBlock").Return(int64(1000))

				return n, u, r
			},
			expectSplit: true,
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
			name: "no error",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(nil)

				return n, u, r
			},
			inputError:  nil,
			expectSplit: false,
		},
		{
			name: "request too large error",
			setup: func() (*mockNetwork, *mockEvmUpstream, *common.NormalizedRequest) {
				n := new(mockNetwork)
				u := new(mockEvmUpstream)
				r := createTestRequest(map[string]interface{}{
					"fromBlock": "0x1",
					"toBlock":   "0x2",
				})
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

				return n, u, r
			},
			inputError:  common.NewErrEndpointRequestTooLarge(errors.New("too large"), common.EvmBlockRangeTooLarge),
			expectSplit: true,
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
	nonEmpty := []byte(`[{"logIndex":1,"address":"0x123","topics":["0xabc"],"data":"0x1234567890"}]`)
	emptyResp := []byte(`[]`)
	nullResp := []byte(`null`)

	t.Run("FirstFillLastEmpty", func(t *testing.T) {
		// Create the multi-response writer with both responses.
		writer := NewGetLogsMultiResponseWriter([][]byte{nonEmpty, emptyResp})

		var buf bytes.Buffer
		writtenBytes, err := writer.WriteTo(&buf)
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
		writer := NewGetLogsMultiResponseWriter([][]byte{emptyResp, nonEmpty})

		var buf bytes.Buffer
		writtenBytes, err := writer.WriteTo(&buf)
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
		writer := NewGetLogsMultiResponseWriter([][]byte{emptyResp, nullResp, nonEmpty})

		var buf bytes.Buffer
		writtenBytes, err := writer.WriteTo(&buf)
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
		writer := NewGetLogsMultiResponseWriter([][]byte{nullResp, nonEmpty, emptyResp, nullResp, emptyResp, nonEmpty, emptyResp})

		var buf bytes.Buffer
		writtenBytes, err := writer.WriteTo(&buf)
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

func createTestRequest(filter interface{}) *common.NormalizedRequest {
	params := []interface{}{filter}
	jrq := common.NewJsonRpcRequest("eth_getLogs", params)
	return common.NewNormalizedRequestFromJsonRpcRequest(jrq)
}
