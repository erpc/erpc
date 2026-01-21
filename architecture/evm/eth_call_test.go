package evm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// mockNetworkForEthCall implements common.Network for testing eth_call pre-forward hook
type mockNetworkForEthCall struct {
	networkId string
	projectId string
	cfg       *common.NetworkConfig
	forwardFn func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
	mu        sync.Mutex
	callCount int
}

func (m *mockNetworkForEthCall) Id() string        { return m.networkId }
func (m *mockNetworkForEthCall) Label() string     { return m.networkId }
func (m *mockNetworkForEthCall) ProjectId() string { return m.projectId }
func (m *mockNetworkForEthCall) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (m *mockNetworkForEthCall) Config() *common.NetworkConfig                        { return m.cfg }
func (m *mockNetworkForEthCall) Logger() *zerolog.Logger                              { return nil }
func (m *mockNetworkForEthCall) GetMethodMetrics(method string) common.TrackedMetrics { return nil }
func (m *mockNetworkForEthCall) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	m.mu.Lock()
	m.callCount++
	m.mu.Unlock()
	if m.forwardFn != nil {
		return m.forwardFn(ctx, req)
	}
	return nil, nil
}
func (m *mockNetworkForEthCall) GetFinality(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) common.DataFinalityState {
	return common.DataFinalityStateUnknown
}
func (m *mockNetworkForEthCall) EvmHighestLatestBlockNumber(ctx context.Context) int64    { return 0 }
func (m *mockNetworkForEthCall) EvmHighestFinalizedBlockNumber(ctx context.Context) int64 { return 0 }
func (m *mockNetworkForEthCall) EvmLeaderUpstream(ctx context.Context) common.Upstream    { return nil }
func (m *mockNetworkForEthCall) Cache() common.CacheDAL                                   { return nil }

func (m *mockNetworkForEthCall) GetCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

func TestProjectPreForward_eth_call_Batching(t *testing.T) {
	// Create valid multicall response for 2 calls
	// Each result: success=true with some return data
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0xde, 0xad, 0xbe, 0xef}},
		{Success: true, ReturnData: []byte{0xca, 0xfe, 0xba, 0xbe}},
	}
	encodedResult := encodeAggregate3Results(results)
	resultHex := "0x" + hexEncode(encodedResult)

	jrr, err := common.NewJsonRpcResponse(nil, resultHex, nil)
	require.NoError(t, err)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	cfg := &common.NetworkConfig{
		Evm: &common.EvmNetworkConfig{
			ChainId: 1,
			Multicall3Aggregation: &common.Multicall3AggregationConfig{
				Enabled:                true,
				WindowMs:               50,
				MinWaitMs:              5,
				MaxCalls:               20,
				MaxCalldataBytes:       64000,
				MaxQueueSize:           100,
				MaxPendingBatches:      20,
				AllowCrossUserBatching: util.BoolPtr(true),
				CachePerCall:           util.BoolPtr(false),
			},
		},
	}
	cfg.Evm.Multicall3Aggregation.SetDefaults()

	network := &mockNetworkForEthCall{
		networkId: "evm:1",
		projectId: "test-project",
		cfg:       cfg,
		forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return mockResp, nil
		},
	}

	ctx := context.Background()

	// Create two requests with different targets
	// Use only 1 param initially (no block param) - the pre-forward hook should add "latest"
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
	})
	jrq1.ID = "req1"
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)
	req1.SetNetwork(network)

	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x2222222222222222222222222222222222222222",
			"data": "0x05060708",
		},
	})
	jrq2.ID = "req2"
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)
	req2.SetNetwork(network)

	// Both should be batched into one call
	var resp1, resp2 *common.NormalizedResponse
	var err1, err2 error
	done := make(chan struct{}, 2)

	go func() {
		_, resp1, err1 = projectPreForward_eth_call(ctx, network, req1)
		done <- struct{}{}
	}()

	go func() {
		_, resp2, err2 = projectPreForward_eth_call(ctx, network, req2)
		done <- struct{}{}
	}()

	// Wait with timeout
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for batched requests")
		}
	}

	require.NoError(t, err1)
	require.NoError(t, err2)
	require.NotNil(t, resp1)
	require.NotNil(t, resp2)

	// Should have been batched into ONE call
	require.Equal(t, 1, network.GetCallCount(), "requests should be batched into one multicall")
}

func TestProjectPreForward_eth_call_NoBatching_Disabled(t *testing.T) {
	// Test that requests are forwarded normally when batching is disabled
	jrr, _ := common.NewJsonRpcResponse(nil, "0xdeadbeef", nil)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	cfg := &common.NetworkConfig{
		Evm: &common.EvmNetworkConfig{
			ChainId: 1,
			// Explicitly disable batching (nil config uses default which has Enabled: true)
			Multicall3Aggregation: &common.Multicall3AggregationConfig{
				Enabled: false,
			},
		},
	}

	network := &mockNetworkForEthCall{
		networkId: "evm:1",
		projectId: "test-project",
		cfg:       cfg,
		forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return mockResp, nil
		},
	}

	ctx := context.Background()
	// Use only 1 param - the pre-forward hook will add "latest" and forward
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	req.SetNetwork(network)

	handled, resp, err := projectPreForward_eth_call(ctx, network, req)
	require.True(t, handled)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, 1, network.GetCallCount())
}

func TestProjectPreForward_eth_call_NoBatching_Ineligible(t *testing.T) {
	// Test that ineligible requests are forwarded normally (e.g., with "from" field)
	jrr, _ := common.NewJsonRpcResponse(nil, "0xdeadbeef", nil)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	cfg := &common.NetworkConfig{
		Evm: &common.EvmNetworkConfig{
			ChainId: 1,
			Multicall3Aggregation: &common.Multicall3AggregationConfig{
				Enabled:                true,
				WindowMs:               50,
				MinWaitMs:              5,
				MaxCalls:               20,
				MaxCalldataBytes:       64000,
				MaxQueueSize:           100,
				MaxPendingBatches:      20,
				AllowCrossUserBatching: util.BoolPtr(true),
			},
		},
	}
	cfg.Evm.Multicall3Aggregation.SetDefaults()

	network := &mockNetworkForEthCall{
		networkId: "evm:1",
		projectId: "test-project",
		cfg:       cfg,
		forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return mockResp, nil
		},
	}

	ctx := context.Background()
	// Request with "from" field is ineligible for batching
	// Use only 1 param so the pre-forward hook processes it
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
			"from": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	req.SetNetwork(network)

	handled, resp, err := projectPreForward_eth_call(ctx, network, req)
	require.True(t, handled)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, 1, network.GetCallCount())
}

func TestProjectPreForward_eth_call_AddsBlockParam(t *testing.T) {
	// Test that missing block param is added as "latest"
	jrr, _ := common.NewJsonRpcResponse(nil, "0xdeadbeef", nil)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	var capturedReq *common.NormalizedRequest
	cfg := &common.NetworkConfig{
		Evm: &common.EvmNetworkConfig{
			ChainId: 1,
			// Explicitly disable batching to test block param normalization
			// (nil config uses default which has Enabled: true)
			Multicall3Aggregation: &common.Multicall3AggregationConfig{
				Enabled: false,
			},
		},
	}

	network := &mockNetworkForEthCall{
		networkId: "evm:1",
		projectId: "test-project",
		cfg:       cfg,
		forwardFn: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			capturedReq = req
			return mockResp, nil
		},
	}

	ctx := context.Background()
	// Request with only 1 param (no block param)
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	req.SetNetwork(network)

	handled, _, err := projectPreForward_eth_call(ctx, network, req)
	require.True(t, handled)
	require.NoError(t, err)

	// Verify block param was added
	capturedJrq, err := capturedReq.JsonRpcRequest()
	require.NoError(t, err)
	require.Len(t, capturedJrq.Params, 2)
	require.Equal(t, "latest", capturedJrq.Params[1])
}

// hexEncode is a helper to encode bytes as hex string
func hexEncode(b []byte) string {
	const hexChars = "0123456789abcdef"
	dst := make([]byte, len(b)*2)
	for i, v := range b {
		dst[i*2] = hexChars[v>>4]
		dst[i*2+1] = hexChars[v&0x0f]
	}
	return string(dst)
}
