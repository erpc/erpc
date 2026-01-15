package evm

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

func TestBatchingKey(t *testing.T) {
	key1 := BatchingKey{
		ProjectId:     "proj1",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: "use-upstream=alchemy",
		UserId:        "",
	}
	key2 := BatchingKey{
		ProjectId:     "proj1",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: "use-upstream=alchemy",
		UserId:        "",
	}
	key3 := BatchingKey{
		ProjectId:     "proj1",
		NetworkId:     "evm:1",
		BlockRef:      "12345",
		DirectivesKey: "use-upstream=alchemy",
		UserId:        "",
	}

	require.Equal(t, key1.String(), key2.String())
	require.NotEqual(t, key1.String(), key3.String())
}

func TestDirectivesKeyDerivation(t *testing.T) {
	dirs := &common.RequestDirectives{}
	dirs.UseUpstream = "alchemy"
	dirs.SkipCacheRead = true
	dirs.RetryEmpty = true

	key := DeriveDirectivesKey(dirs)
	require.Contains(t, key, "use-upstream=alchemy")
	require.Contains(t, key, "skip-cache-read=true")
	require.Contains(t, key, "retry-empty=true")
}

func TestCallKeyDerivation(t *testing.T) {
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1234567890123456789012345678901234567890",
			"data": "0xabcdef",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	key, err := DeriveCallKey(req)
	require.NoError(t, err)
	require.NotEmpty(t, key)

	// Same request should produce same key
	key2, err := DeriveCallKey(req)
	require.NoError(t, err)
	require.Equal(t, key, key2)
}

func TestDirectivesKeyDerivation_Nil(t *testing.T) {
	key := DeriveDirectivesKey(nil)
	require.Equal(t, "v1:", key)
}

func TestDirectivesKeyDerivation_VersionPrefix(t *testing.T) {
	dirs := &common.RequestDirectives{}
	dirs.UseUpstream = "alchemy"

	key := DeriveDirectivesKey(dirs)
	require.True(t, strings.HasPrefix(key, "v1:"), "key should have version prefix")
}

func TestCallKeyDerivation_NilRequest(t *testing.T) {
	key, err := DeriveCallKey(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "request is nil")
	require.Empty(t, key)
}

func TestIsEligibleForBatching(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                 true,
		AllowPendingTagBatching: false,
	}
	cfg.SetDefaults()

	tests := []struct {
		name     string
		method   string
		params   []interface{}
		eligible bool
		reason   string
	}{
		{
			name:   "eligible basic eth_call",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"latest",
			},
			eligible: true,
		},
		{
			name:   "eligible with finalized tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"finalized",
			},
			eligible: true,
		},
		{
			name:   "ineligible - pending tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"pending",
			},
			eligible: false,
			reason:   "pending tag not allowed",
		},
		{
			name:   "ineligible - has from field",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{
					"to":   "0x1234567890123456789012345678901234567890",
					"data": "0xabcd",
					"from": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
				"latest",
			},
			eligible: false,
			reason:   "has from field",
		},
		{
			name:   "ineligible - has value field",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{
					"to":    "0x1234567890123456789012345678901234567890",
					"data":  "0xabcd",
					"value": "0x1",
				},
				"latest",
			},
			eligible: false,
			reason:   "has value field",
		},
		{
			name:   "ineligible - has state override (3rd param)",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"latest",
				map[string]interface{}{}, // state override
			},
			eligible: false,
			reason:   "has state override",
		},
		{
			name:     "ineligible - not eth_call",
			method:   "eth_getBalance",
			params:   []interface{}{"0x1234567890123456789012345678901234567890", "latest"},
			eligible: false,
			reason:   "not eth_call",
		},
		{
			name:   "ineligible - already multicall (recursion guard)",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{
					"to":   "0xcA11bde05977b3631167028862bE2a173976CA11", // multicall3 address
					"data": "0x82ad56cb",                                 // aggregate3 selector
				},
				"latest",
			},
			eligible: false,
			reason:   "already multicall",
		},
		{
			name:   "eligible with safe tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"safe",
			},
			eligible: true,
		},
		{
			name:   "eligible with earliest tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"earliest",
			},
			eligible: true,
		},
		{
			name:   "eligible with numeric block number (hex)",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"0x1234",
			},
			eligible: true,
		},
		{
			name:   "eligible with block hash",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef", // 32-byte hash
			},
			eligible: true,
		},
		{
			name:   "ineligible - unknown block tag",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				"unknown_tag",
			},
			eligible: false,
			reason:   "block tag not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jrq := common.NewJsonRpcRequest(tt.method, tt.params)
			req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

			eligible, reason := IsEligibleForBatching(req, cfg)
			require.Equal(t, tt.eligible, eligible, "reason: %s", reason)
			if !tt.eligible {
				require.Contains(t, reason, tt.reason)
			}
		})
	}
}

func TestExtractCallInfo(t *testing.T) {
	tests := []struct {
		name           string
		params         []interface{}
		expectedTarget string
		expectedData   string
		expectedBlock  string
		expectError    bool
	}{
		{
			name: "basic extraction with data field",
			params: []interface{}{
				map[string]interface{}{
					"to":   "0x1234567890123456789012345678901234567890",
					"data": "0xabcdef",
				},
				"latest",
			},
			expectedTarget: "0x1234567890123456789012345678901234567890",
			expectedData:   "0xabcdef",
			expectedBlock:  "latest",
		},
		{
			name: "extraction with input field instead of data",
			params: []interface{}{
				map[string]interface{}{
					"to":    "0x1234567890123456789012345678901234567890",
					"input": "0x12345678",
				},
				"finalized",
			},
			expectedTarget: "0x1234567890123456789012345678901234567890",
			expectedData:   "0x12345678",
			expectedBlock:  "finalized",
		},
		{
			name: "extraction with empty data",
			params: []interface{}{
				map[string]interface{}{
					"to": "0x1234567890123456789012345678901234567890",
				},
				"latest",
			},
			expectedTarget: "0x1234567890123456789012345678901234567890",
			expectedData:   "0x",
			expectedBlock:  "latest",
		},
		{
			name: "extraction with no block param (defaults to latest)",
			params: []interface{}{
				map[string]interface{}{
					"to":   "0x1234567890123456789012345678901234567890",
					"data": "0xaa",
				},
			},
			expectedTarget: "0x1234567890123456789012345678901234567890",
			expectedData:   "0xaa",
			expectedBlock:  "latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jrq := common.NewJsonRpcRequest("eth_call", tt.params)
			req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

			target, data, blockRef, err := ExtractCallInfo(req)
			if tt.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.expectedTarget, "0x"+hex.EncodeToString(target))
			require.Equal(t, tt.expectedData, "0x"+hex.EncodeToString(data))
			require.Equal(t, tt.expectedBlock, blockRef)
		})
	}
}

func TestIsEligibleForBatching_AllowPendingTag(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                 true,
		AllowPendingTagBatching: true,
	}
	cfg.SetDefaults()

	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1234567890123456789012345678901234567890",
			"data": "0xabcd",
		},
		"pending",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	eligible, reason := IsEligibleForBatching(req, cfg)
	require.True(t, eligible, "pending should be allowed when AllowPendingTagBatching is true: %s", reason)
}

func TestBatcherEnqueueAndFlush(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               50,
		MinWaitMs:              5,
		SafetyMarginMs:         2,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	ctx := context.Background()
	batcher := NewBatcher(cfg, nil) // nil forwarder for now

	// Create test requests
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1234567890123456789012345678901234567890",
			"data": "0xabcdef01",
		},
		"latest",
	})
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)

	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x2234567890123456789012345678901234567890",
			"data": "0xabcdef02",
		},
		"latest",
	})
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Enqueue first request
	entry1, bypass1, err := batcher.Enqueue(ctx, key, req1)
	require.NoError(t, err)
	require.False(t, bypass1)
	require.NotNil(t, entry1)

	// Enqueue second request
	entry2, bypass2, err := batcher.Enqueue(ctx, key, req2)
	require.NoError(t, err)
	require.False(t, bypass2)
	require.NotNil(t, entry2)

	// Check batch exists
	batcher.mu.RLock()
	batch, exists := batcher.batches[key.String()]
	batcher.mu.RUnlock()
	require.True(t, exists)
	require.Len(t, batch.Entries, 2)

	// Cleanup
	batcher.Shutdown()
}

func TestBatcherDeduplication(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               50,
		MinWaitMs:              5,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	ctx := context.Background()
	batcher := NewBatcher(cfg, nil)

	// Two identical requests - using the same jrq to ensure call key consistency
	// (JSON serialization of map[string]interface{} can have non-deterministic key order)
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1234567890123456789012345678901234567890",
			"data": "0xabcdef01",
		},
		"latest",
	})
	jrq2 := common.NewJsonRpcRequest("eth_call", jrq1.Params) // Use same params object
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	entry1, _, _ := batcher.Enqueue(ctx, key, req1)
	entry2, _, _ := batcher.Enqueue(ctx, key, req2)

	// Both should share the same callKey slot
	require.Equal(t, entry1.CallKey, entry2.CallKey)

	batcher.mu.RLock()
	batch := batcher.batches[key.String()]
	batcher.mu.RUnlock()

	// Two entries but deduplicated
	require.Len(t, batch.Entries, 2)
	require.Len(t, batch.CallKeys[entry1.CallKey], 2)

	batcher.Shutdown()
}

func TestBatcherCapsEnforcement(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               50,
		MinWaitMs:              5,
		MaxCalls:               2, // Very low limit
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	ctx := context.Background()
	batcher := NewBatcher(cfg, nil)

	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Add requests up to cap
	for i := 0; i < 2; i++ {
		jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
			map[string]interface{}{
				"to":   fmt.Sprintf("0x%040d", i),
				"data": "0xabcdef",
			},
			"latest",
		})
		req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
		_, bypass, err := batcher.Enqueue(ctx, key, req)
		require.NoError(t, err)
		require.False(t, bypass)
	}

	// Next request should trigger bypass (caps reached)
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x9999999999999999999999999999999999999999",
			"data": "0xabcdef",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	_, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.True(t, bypass, "should bypass when caps reached")

	batcher.Shutdown()
}

// mockForwarder implements Forwarder for testing
type mockForwarder struct {
	response *common.NormalizedResponse
	err      error
	called   int
	mu       sync.Mutex
}

func (m *mockForwarder) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called++
	return m.response, m.err
}

func TestBatcherFlushAndResultMapping(t *testing.T) {
	// Create valid multicall3 result with 2 calls
	// Each call returns success=true with some data
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0xde, 0xad, 0xbe, 0xef}},
		{Success: true, ReturnData: []byte{0xca, 0xfe, 0xba, 0xbe}},
	}
	encodedResult := encodeAggregate3Results(results)
	resultHex := "0x" + hex.EncodeToString(encodedResult)

	jrr, err := common.NewJsonRpcResponse(nil, resultHex, nil)
	require.NoError(t, err)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	forwarder := &mockForwarder{response: mockResp}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               10, // Short window for test
		MinWaitMs:              1,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
		CachePerCall:           util.BoolPtr(false), // disable caching for test
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder)

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Add two requests
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x1111111111111111111111111111111111111111", "data": "0x01"},
		"latest",
	})
	jrq1.ID = "req1"
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)

	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x2222222222222222222222222222222222222222", "data": "0x02"},
		"latest",
	})
	jrq2.ID = "req2"
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	entry1, _, err := batcher.Enqueue(ctx, key, req1)
	require.NoError(t, err)
	entry2, _, err := batcher.Enqueue(ctx, key, req2)
	require.NoError(t, err)

	// Wait for results
	result1 := <-entry1.ResultCh
	result2 := <-entry2.ResultCh

	require.NoError(t, result1.Error)
	require.NoError(t, result2.Error)
	require.NotNil(t, result1.Response)
	require.NotNil(t, result2.Response)

	// Verify forwarder was called exactly once
	forwarder.mu.Lock()
	require.Equal(t, 1, forwarder.called)
	forwarder.mu.Unlock()

	// Verify the responses contain the expected data
	jrr1, err := result1.Response.JsonRpcResponse()
	require.NoError(t, err)
	require.Equal(t, "\"0xdeadbeef\"", jrr1.GetResultString())

	jrr2, err := result2.Response.JsonRpcResponse()
	require.NoError(t, err)
	require.Equal(t, "\"0xcafebabe\"", jrr2.GetResultString())

	batcher.Shutdown()
}

func TestBatcherFlushDeduplication(t *testing.T) {
	// Create result with 1 call (deduplication means only 1 unique call is made)
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0xab, 0xcd}},
	}
	encodedResult := encodeAggregate3Results(results)
	resultHex := "0x" + hex.EncodeToString(encodedResult)

	jrr, err := common.NewJsonRpcResponse(nil, resultHex, nil)
	require.NoError(t, err)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	forwarder := &mockForwarder{response: mockResp}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               10,
		MinWaitMs:              1,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
		CachePerCall:           util.BoolPtr(false),
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder)

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Add two IDENTICAL requests (same target and calldata)
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x1111111111111111111111111111111111111111", "data": "0x01"},
		"latest",
	})
	jrq1.ID = "req1"
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)

	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x1111111111111111111111111111111111111111", "data": "0x01"},
		"latest",
	})
	jrq2.ID = "req2"
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	entry1, _, err := batcher.Enqueue(ctx, key, req1)
	require.NoError(t, err)
	entry2, _, err := batcher.Enqueue(ctx, key, req2)
	require.NoError(t, err)

	// Both should share the same call key
	require.Equal(t, entry1.CallKey, entry2.CallKey)

	// Wait for results
	result1 := <-entry1.ResultCh
	result2 := <-entry2.ResultCh

	require.NoError(t, result1.Error)
	require.NoError(t, result2.Error)

	// Both should get the same result (fanned out)
	jrr1, err := result1.Response.JsonRpcResponse()
	require.NoError(t, err)
	jrr2, err := result2.Response.JsonRpcResponse()
	require.NoError(t, err)
	require.Equal(t, jrr1.GetResultString(), jrr2.GetResultString())

	// Forwarder should only be called once
	forwarder.mu.Lock()
	require.Equal(t, 1, forwarder.called)
	forwarder.mu.Unlock()

	batcher.Shutdown()
}

func TestBatcherFlushRevertHandling(t *testing.T) {
	// Create result where second call reverts
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0xde, 0xad, 0xbe, 0xef}},
		{Success: false, ReturnData: []byte{0x08, 0xc3, 0x79, 0xa0}}, // Error(string) selector
	}
	encodedResult := encodeAggregate3Results(results)
	resultHex := "0x" + hex.EncodeToString(encodedResult)

	jrr, err := common.NewJsonRpcResponse(nil, resultHex, nil)
	require.NoError(t, err)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	forwarder := &mockForwarder{response: mockResp}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               10,
		MinWaitMs:              1,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
		CachePerCall:           util.BoolPtr(false),
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder)

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Add two requests
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x1111111111111111111111111111111111111111", "data": "0x01"},
		"latest",
	})
	jrq1.ID = "req1"
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)

	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x2222222222222222222222222222222222222222", "data": "0x02"},
		"latest",
	})
	jrq2.ID = "req2"
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	entry1, _, err := batcher.Enqueue(ctx, key, req1)
	require.NoError(t, err)
	entry2, _, err := batcher.Enqueue(ctx, key, req2)
	require.NoError(t, err)

	// Wait for results
	result1 := <-entry1.ResultCh
	result2 := <-entry2.ResultCh

	// First call should succeed
	require.NoError(t, result1.Error)
	require.NotNil(t, result1.Response)

	// Second call should fail with revert error
	require.Error(t, result2.Error)
	require.Contains(t, result2.Error.Error(), "execution reverted")

	batcher.Shutdown()
}

func TestBatcherFlushFallbackOnMulticall3Unavailable(t *testing.T) {
	// Track individual calls made during fallback
	var individualCalls []*common.NormalizedRequest
	var mu sync.Mutex

	forwarder := &mockForwarderFunc{
		forwardFunc: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			mu.Lock()
			defer mu.Unlock()

			// Check if this is a multicall3 request (to multicall3 address)
			jrq, _ := req.JsonRpcRequest()
			if jrq != nil && len(jrq.Params) > 0 {
				if callObj, ok := jrq.Params[0].(map[string]interface{}); ok {
					if toAddr, ok := callObj["to"].(string); ok {
						if strings.EqualFold(toAddr, "0xcA11bde05977b3631167028862bE2a173976CA11") {
							// This is a multicall3 request - return "contract not found" error
							return nil, common.NewErrEndpointExecutionException(fmt.Errorf("contract not found"))
						}
					}
				}
			}

			// Individual call - track and return success
			individualCalls = append(individualCalls, req)
			jrr, _ := common.NewJsonRpcResponse(req.ID(), "0xdeadbeef", nil)
			return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
		},
	}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               10,
		MinWaitMs:              1,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
		CachePerCall:           util.BoolPtr(false),
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder)

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Add two requests
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x1111111111111111111111111111111111111111", "data": "0x01"},
		"latest",
	})
	jrq1.ID = "req1"
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)

	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x2222222222222222222222222222222222222222", "data": "0x02"},
		"latest",
	})
	jrq2.ID = "req2"
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	entry1, _, err := batcher.Enqueue(ctx, key, req1)
	require.NoError(t, err)
	entry2, _, err := batcher.Enqueue(ctx, key, req2)
	require.NoError(t, err)

	// Wait for results
	result1 := <-entry1.ResultCh
	result2 := <-entry2.ResultCh

	// Both should succeed via fallback
	require.NoError(t, result1.Error)
	require.NoError(t, result2.Error)

	// Verify individual fallback calls were made
	mu.Lock()
	require.Equal(t, 2, len(individualCalls))
	mu.Unlock()

	batcher.Shutdown()
}

// mockForwarderFunc allows custom forward behavior for testing
type mockForwarderFunc struct {
	forwardFunc func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

func (m *mockForwarderFunc) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return m.forwardFunc(ctx, req)
}
