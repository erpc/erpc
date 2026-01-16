package evm

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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
			name:   "ineligible - unsupported call field",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{
					"to":         "0x1234567890123456789012345678901234567890",
					"data":       "0xabcd",
					"accessList": []interface{}{},
				},
				"latest",
			},
			eligible: false,
			reason:   "unsupported call field",
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
		{
			name:   "ineligible - EIP-1898 block param with requireCanonical:false",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				map[string]interface{}{
					"blockHash":        "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
					"requireCanonical": false,
				},
			},
			eligible: false,
			reason:   "has requireCanonical:false",
		},
		{
			name:   "eligible - EIP-1898 block param with requireCanonical:true",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				map[string]interface{}{
					"blockHash":        "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
					"requireCanonical": true,
				},
			},
			eligible: true,
		},
		{
			name:   "eligible - EIP-1898 block param without requireCanonical (default true)",
			method: "eth_call",
			params: []interface{}{
				map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0xabcd"},
				map[string]interface{}{
					"blockHash": "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
				},
			},
			eligible: true,
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
	forwarder := &mockForwarder{} // Not used in this test but required
	batcher := NewBatcher(cfg, forwarder, nil)

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
	forwarder := &mockForwarder{} // Not used in this test but required
	batcher := NewBatcher(cfg, forwarder, nil)

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
	forwarder := &mockForwarder{} // Not used in this test but required
	batcher := NewBatcher(cfg, forwarder, nil)

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
	response    *common.NormalizedResponse
	err         error
	called      int
	cacheWrites int
	mu          sync.Mutex
}

func (m *mockForwarder) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called++
	return m.response, m.err
}

func (m *mockForwarder) SetCache(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheWrites++
	return nil
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

	batcher := NewBatcher(cfg, forwarder, nil)

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

func TestBatcherFlush_UsesHexBlockParam(t *testing.T) {
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0x01}},
	}
	encodedResult := encodeAggregate3Results(results)
	resultHex := "0x" + hex.EncodeToString(encodedResult)

	blockParamCh := make(chan interface{}, 1)
	forwarder := &mockForwarderFunc{
		forwardFunc: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			jrq, err := req.JsonRpcRequest()
			if err == nil {
				jrq.RLock()
				params := jrq.Params
				jrq.RUnlock()
				if len(params) > 1 {
					blockParamCh <- params[1]
				}
			}

			jrr, err := common.NewJsonRpcResponse(nil, resultHex, nil)
			if err != nil {
				return nil, err
			}
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

	batcher := NewBatcher(cfg, forwarder, nil)

	ctx := context.Background()
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01",
		},
		"0x10",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	_, _, blockRef, err := ExtractCallInfo(req)
	require.NoError(t, err)

	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      blockRef,
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	entry, _, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)

	result := <-entry.ResultCh
	require.NoError(t, result.Error)

	select {
	case blockParam := <-blockParamCh:
		paramStr, ok := blockParam.(string)
		require.True(t, ok)
		require.Equal(t, "0x10", paramStr)
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for block param")
	}

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

	batcher := NewBatcher(cfg, forwarder, nil)

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

	batcher := NewBatcher(cfg, forwarder, nil)

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

	batcher := NewBatcher(cfg, forwarder, nil)

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

func (m *mockForwarderFunc) SetCache(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	return nil
}

func TestBatcherCancellation(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               100,
		MinWaitMs:              50,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	forwarder := &mockForwarder{} // Not used in this test but required
	batcher := NewBatcher(cfg, forwarder, nil)

	ctx, cancel := context.WithCancel(context.Background())
	key := BatchingKey{
		ProjectId: "test",
		NetworkId: "evm:1",
		BlockRef:  "latest",
	}

	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0x01"},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	entry, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.False(t, bypass)
	require.NotNil(t, entry)

	// Cancel before flush
	cancel()

	// Batcher should shutdown gracefully
	batcher.Shutdown()
}

func TestBatcherDeadlineAwareness(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               100,
		MinWaitMs:              10,
		SafetyMarginMs:         5,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	forwarder := &mockForwarder{} // Not used in this test but required
	batcher := NewBatcher(cfg, forwarder, nil)

	key := BatchingKey{
		ProjectId: "test",
		NetworkId: "evm:1",
		BlockRef:  "latest",
	}

	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{"to": "0x1234567890123456789012345678901234567890", "data": "0x01"},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	// Context with tight deadline - should bypass
	tightCtx, cancel1 := context.WithDeadline(context.Background(), time.Now().Add(5*time.Millisecond))
	defer cancel1()

	_, bypass, err := batcher.Enqueue(tightCtx, key, req)
	require.NoError(t, err)
	require.True(t, bypass, "should bypass with tight deadline")

	// Context with reasonable deadline - should batch
	normalCtx, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(200*time.Millisecond))
	defer cancel2()

	_, bypass, err = batcher.Enqueue(normalCtx, key, req)
	require.NoError(t, err)
	require.False(t, bypass, "should batch with normal deadline")

	batcher.Shutdown()
}

func TestBatcherConcurrentFlush(t *testing.T) {
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

	// Mock forwarder that returns success for all batches
	var callCount int
	var mu sync.Mutex
	forwarder := &mockForwarderFunc{
		forwardFunc: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			mu.Lock()
			callCount++
			mu.Unlock()

			// Return multicall3 results with 3 successful calls
			results := []Multicall3Result{
				{Success: true, ReturnData: []byte{0xaa}},
				{Success: true, ReturnData: []byte{0xbb}},
				{Success: true, ReturnData: []byte{0xcc}},
			}
			encodedResult := encodeAggregate3Results(results)
			resultHex := "0x" + hex.EncodeToString(encodedResult)
			jrr, _ := common.NewJsonRpcResponse(nil, resultHex, nil)
			return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
		},
	}

	batcher := NewBatcher(cfg, forwarder, nil)

	ctx := context.Background()
	key := BatchingKey{
		ProjectId: "test",
		NetworkId: "evm:1",
		BlockRef:  "latest",
	}

	// Add first batch
	for i := 0; i < 3; i++ {
		jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
			map[string]interface{}{"to": fmt.Sprintf("0x%040d", i), "data": "0x01"},
			"latest",
		})
		req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
		batcher.Enqueue(ctx, key, req)
	}

	// Wait for first batch to start flushing
	time.Sleep(15 * time.Millisecond)

	// Add more requests - should go to new batch
	for i := 3; i < 6; i++ {
		jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
			map[string]interface{}{"to": fmt.Sprintf("0x%040d", i), "data": "0x01"},
			"latest",
		})
		req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
		_, bypass, _ := batcher.Enqueue(ctx, key, req)
		_ = bypass // May bypass or create new batch - either is acceptable
	}

	// Wait for all flushes to complete
	time.Sleep(20 * time.Millisecond)

	batcher.Shutdown()

	// Verify at least one batch was processed
	mu.Lock()
	require.GreaterOrEqual(t, callCount, 1)
	mu.Unlock()
}

func TestNewBatcher_NilConfig(t *testing.T) {
	forwarder := &mockForwarder{}

	// Test with nil config
	batcher := NewBatcher(nil, forwarder, nil)
	require.Nil(t, batcher, "NewBatcher should return nil for nil config")
}

func TestNewBatcher_DisabledConfig(t *testing.T) {
	forwarder := &mockForwarder{}

	// Test with disabled config
	cfg := &common.Multicall3AggregationConfig{
		Enabled: false,
	}
	batcher := NewBatcher(cfg, forwarder, nil)
	require.Nil(t, batcher, "NewBatcher should return nil for disabled config")
}

func TestNewBatcher_EnabledConfig(t *testing.T) {
	forwarder := &mockForwarder{}

	// Test with enabled config
	cfg := &common.Multicall3AggregationConfig{
		Enabled:           true,
		WindowMs:          50,
		MinWaitMs:         5,
		MaxCalls:          10,
		MaxCalldataBytes:  64000,
		MaxQueueSize:      100,
		MaxPendingBatches: 20,
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher, "NewBatcher should return non-nil for enabled config")
	batcher.Shutdown()
}

func TestNewBatcher_NilForwarder_Panics(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:           true,
		WindowMs:          50,
		MinWaitMs:         5,
		MaxCalls:          10,
		MaxCalldataBytes:  64000,
		MaxQueueSize:      100,
		MaxPendingBatches: 20,
	}
	cfg.SetDefaults()

	// Test that nil forwarder causes panic
	require.Panics(t, func() {
		NewBatcher(cfg, nil, nil)
	}, "NewBatcher should panic when forwarder is nil")
}

// mockForwarderWithCacheError is a forwarder that returns errors from SetCache
type mockForwarderWithCacheError struct {
	response   *common.NormalizedResponse
	cacheError error
	mu         sync.Mutex
	called     int
}

func (m *mockForwarderWithCacheError) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called++
	return m.response, nil
}

func (m *mockForwarderWithCacheError) SetCache(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	return m.cacheError
}

func TestBatcher_CacheWriteError_DoesNotFailRequest(t *testing.T) {
	// Create multicall response for 1 call
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0xde, 0xad, 0xbe, 0xef}},
	}
	encodedResult := encodeAggregate3Results(results)
	resultHex := "0x" + hex.EncodeToString(encodedResult)

	jrr, err := common.NewJsonRpcResponse(nil, resultHex, nil)
	require.NoError(t, err)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	forwarder := &mockForwarderWithCacheError{
		response:   mockResp,
		cacheError: fmt.Errorf("cache write failed"),
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
		CachePerCall:           util.BoolPtr(true), // Enable per-call caching
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	entry, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.False(t, bypass)

	// Wait for result - should succeed despite cache write error
	select {
	case result := <-entry.ResultCh:
		require.NoError(t, result.Error, "request should succeed despite cache write error")
		require.NotNil(t, result.Response)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for batched request")
	}
}

func TestBatcher_ContextDeadlineError_WrappedWithBatchContext(t *testing.T) {
	// Create a forwarder that simulates internal timeout (not waiting on ctx)
	forwarder := &mockForwarderFunc{
		forwardFunc: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			// Simulate a slow response that triggers ctx deadline internally
			// But return the error immediately so entry context is still valid for delivery
			return nil, context.DeadlineExceeded
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
		SafetyMarginMs:         2,
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	// Create context with deadline longer than batch window so entry is still valid at delivery
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(500*time.Millisecond))
	defer cancel()

	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	entry, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.False(t, bypass)

	// Wait for result
	select {
	case result := <-entry.ResultCh:
		require.Error(t, result.Error)
		// The error wrapping happens when ctx.Err() != nil at forward time
		// Since we return DeadlineExceeded but ctx isn't actually expired yet,
		// the wrapping won't happen. This tests the error delivery path.
		require.Contains(t, result.Error.Error(), "deadline exceeded")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for batched request")
	}
}

func TestBatcher_MaxCalldataBytes_Bypass(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               100,
		MinWaitMs:              5,
		MaxCalls:               100, // High limit
		MaxCalldataBytes:       100, // Very low limit for testing
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	ctx := context.Background()
	forwarder := &mockForwarder{} // Not used in this test but required
	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// First request with small calldata - should be batched
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304", // 4 bytes
		},
		"latest",
	})
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)

	entry1, bypass1, err := batcher.Enqueue(ctx, key, req1)
	require.NoError(t, err)
	require.False(t, bypass1, "first request should be batched")
	require.NotNil(t, entry1)

	// Second request with large calldata - should bypass due to MaxCalldataBytes
	largeData := "0x" + strings.Repeat("aa", 200) // 200 bytes
	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x2222222222222222222222222222222222222222",
			"data": largeData,
		},
		"latest",
	})
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	_, bypass2, err := batcher.Enqueue(ctx, key, req2)
	require.NoError(t, err)
	require.True(t, bypass2, "second request should bypass due to MaxCalldataBytes")
}

func TestBatcher_OnlyIfPending_NoBatch(t *testing.T) {
	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               100,
		MinWaitMs:              5,
		MaxCalls:               100,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
		OnlyIfPending:          true, // Only batch if there's already a pending batch
	}
	cfg.SetDefaults()

	ctx := context.Background()
	forwarder := &mockForwarder{} // Not used in this test but required
	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// First request should bypass - no pending batch exists
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
		"latest",
	})
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)

	_, bypass1, err := batcher.Enqueue(ctx, key, req1)
	require.NoError(t, err)
	require.True(t, bypass1, "first request should bypass when OnlyIfPending is true and no batch exists")
}

func TestBatcher_OnlyIfPending_WithExistingBatch(t *testing.T) {
	// Create valid multicall3 result with 2 calls
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0xaa}},
		{Success: true, ReturnData: []byte{0xbb}},
	}
	encodedResult := encodeAggregate3Results(results)
	resultHex := "0x" + hex.EncodeToString(encodedResult)

	jrr, err := common.NewJsonRpcResponse(nil, resultHex, nil)
	require.NoError(t, err)
	mockResp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	forwarder := &mockForwarder{response: mockResp}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               100,
		MinWaitMs:              5,
		MaxCalls:               100,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
		OnlyIfPending:          false, // Start with false to create a batch
		CachePerCall:           util.BoolPtr(false),
	}
	cfg.SetDefaults()

	ctx := context.Background()
	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// First request creates a batch
	jrq1 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01",
		},
		"latest",
	})
	jrq1.ID = "req1"
	req1 := common.NewNormalizedRequestFromJsonRpcRequest(jrq1)

	entry1, bypass1, err := batcher.Enqueue(ctx, key, req1)
	require.NoError(t, err)
	require.False(t, bypass1, "first request should create batch")
	require.NotNil(t, entry1)

	// Second request should join the existing batch
	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x2222222222222222222222222222222222222222",
			"data": "0x02",
		},
		"latest",
	})
	jrq2.ID = "req2"
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	entry2, bypass2, err := batcher.Enqueue(ctx, key, req2)
	require.NoError(t, err)
	require.False(t, bypass2, "second request should join existing batch")
	require.NotNil(t, entry2)

	// Wait for results
	result1 := <-entry1.ResultCh
	result2 := <-entry2.ResultCh

	require.NoError(t, result1.Error)
	require.NoError(t, result2.Error)
}

func TestBatcher_DuplicateCallsShareResult(t *testing.T) {
	// Create result with 1 unique call (both requests have same target+data)
	results := []Multicall3Result{
		{Success: true, ReturnData: []byte{0xde, 0xad, 0xbe, 0xef}},
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
		MaxCalls:               100,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
		CachePerCall:           util.BoolPtr(false),
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Three identical requests - should all share the same result
	entries := make([]*BatchEntry, 3)
	for i := 0; i < 3; i++ {
		jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
			map[string]interface{}{
				"to":   "0x1111111111111111111111111111111111111111",
				"data": "0x12345678",
			},
			"latest",
		})
		jrq.ID = fmt.Sprintf("req%d", i)
		req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

		entry, bypass, err := batcher.Enqueue(ctx, key, req)
		require.NoError(t, err)
		require.False(t, bypass)
		entries[i] = entry
	}

	// All entries should have the same callKey (deduplication)
	require.Equal(t, entries[0].CallKey, entries[1].CallKey)
	require.Equal(t, entries[1].CallKey, entries[2].CallKey)

	// Wait for all results
	for i, entry := range entries {
		result := <-entry.ResultCh
		require.NoError(t, result.Error, "entry %d should succeed", i)
		require.NotNil(t, result.Response)

		jrrResult, err := result.Response.JsonRpcResponse()
		require.NoError(t, err)
		require.Equal(t, "\"0xdeadbeef\"", jrrResult.GetResultString())
	}

	// Forwarder should only be called once (all requests batched into single multicall)
	forwarder.mu.Lock()
	require.Equal(t, 1, forwarder.called)
	forwarder.mu.Unlock()
}

// TestBatcher_ShutdownDuringActiveFlush verifies that shutdown during an active
// flush delivers shutdown errors to pending entries and cleans up properly.
func TestBatcher_ShutdownDuringActiveFlush(t *testing.T) {
	// Create a forwarder that blocks to simulate a long-running flush
	flushStarted := make(chan struct{})
	flushBlock := make(chan struct{})

	forwarder := &mockForwarderFunc{
		forwardFunc: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			close(flushStarted) // Signal that flush has started
			<-flushBlock        // Block until test unblocks
			return nil, fmt.Errorf("should not reach here")
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
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Enqueue a request
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	entry, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.False(t, bypass)

	// Wait for flush to start (forwarder called)
	select {
	case <-flushStarted:
		// Good - flush has started
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for flush to start")
	}

	// Now enqueue another request for a DIFFERENT batch key
	// This creates a new batch that hasn't started flushing yet
	key2 := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "0x12345", // Different block ref = different batch
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	jrq2 := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x2222222222222222222222222222222222222222",
			"data": "0x05060708",
		},
		"0x12345",
	})
	req2 := common.NewNormalizedRequestFromJsonRpcRequest(jrq2)

	entry2, bypass2, err2 := batcher.Enqueue(ctx, key2, req2)
	require.NoError(t, err2)
	require.False(t, bypass2)

	// Call shutdown while first flush is blocked
	// This should trigger flushWithShutdownError for the second batch
	go func() {
		time.Sleep(10 * time.Millisecond) // Give shutdown a head start
		close(flushBlock)                 // Unblock the first flush
	}()

	batcher.Shutdown()

	// The second entry should receive a shutdown error
	select {
	case result := <-entry2.ResultCh:
		require.Error(t, result.Error)
		require.Contains(t, result.Error.Error(), "shutting down")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for shutdown error on entry2")
	}

	// First entry gets error because forwarder returns error after unblock
	select {
	case result := <-entry.ResultCh:
		// Either an error from forwarder or from shutdown is acceptable
		require.Error(t, result.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for result on entry1")
	}
}

// TestBatcher_DoubleFlushPrevention verifies that concurrent flush calls
// on the same batch don't result in double-processing (race condition test).
func TestBatcher_DoubleFlushPrevention(t *testing.T) {
	var forwardCallCount int64
	var mu sync.Mutex

	// Create a forwarder that counts calls
	forwarder := &mockForwarderFunc{
		forwardFunc: func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			mu.Lock()
			forwardCallCount++
			mu.Unlock()

			// Return multicall3 results with 1 successful call
			results := []Multicall3Result{
				{Success: true, ReturnData: []byte{0xaa, 0xbb}},
			}
			encodedResult := encodeAggregate3Results(results)
			resultHex := "0x" + hex.EncodeToString(encodedResult)
			jrr, _ := common.NewJsonRpcResponse(nil, resultHex, nil)
			return common.NewNormalizedResponse().WithJsonRpcResponse(jrr), nil
		},
	}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               1000, // Long window so we control when flush happens
		MinWaitMs:              500,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Enqueue a request
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	entry, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.False(t, bypass)

	// Get the batch directly from the batcher's internal map
	keyStr := key.String()
	batcher.mu.Lock()
	batch := batcher.batches[keyStr]
	batcher.mu.Unlock()
	require.NotNil(t, batch, "batch should exist")

	// Simulate concurrent flush calls (race condition scenario)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batcher.flush(keyStr, batch)
		}()
	}
	wg.Wait()

	// Verify forwarder was only called once (double-flush prevented)
	mu.Lock()
	finalCallCount := forwardCallCount
	mu.Unlock()
	require.Equal(t, int64(1), finalCallCount, "forwarder should only be called once despite concurrent flush attempts")

	// Entry should receive exactly one result
	select {
	case result := <-entry.ResultCh:
		require.NoError(t, result.Error)
		require.NotNil(t, result.Response)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

// mockPanicForwarder panics when Forward is called to test panic recovery
type mockPanicForwarder struct {
	panicMessage string
}

func (m *mockPanicForwarder) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	panic(m.panicMessage)
}

func (m *mockPanicForwarder) SetCache(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	return nil
}

func TestBatcher_ScheduleFlush_PanicRecovery(t *testing.T) {
	forwarder := &mockPanicForwarder{panicMessage: "test panic in forwarder"}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               10,
		MinWaitMs:              5,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Enqueue a request
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	entry, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.False(t, bypass)

	// Wait for the batch to flush (will panic and recover)
	select {
	case result := <-entry.ResultCh:
		// Should receive an error due to the panic
		require.Error(t, result.Error)
		require.Contains(t, result.Error.Error(), "panic")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for result - panic recovery may have failed")
	}
}

// mockFallbackThenPanicForwarder returns an error triggering fallback on first call,
// then panics on subsequent (individual) calls to test fallback panic recovery
type mockFallbackThenPanicForwarder struct {
	callCount    int
	panicMessage string
	mu           sync.Mutex
}

func (m *mockFallbackThenPanicForwarder) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	m.mu.Lock()
	m.callCount++
	count := m.callCount
	m.mu.Unlock()

	if count == 1 {
		// First call is the multicall - return error that triggers fallback
		return nil, common.NewErrEndpointExecutionException(
			fmt.Errorf("contract not found"),
		)
	}
	// Subsequent calls (individual fallback) - panic
	panic(m.panicMessage)
}

func (m *mockFallbackThenPanicForwarder) SetCache(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	return nil
}

func TestBatcher_FallbackIndividual_PanicRecovery(t *testing.T) {
	forwarder := &mockFallbackThenPanicForwarder{panicMessage: "test panic in fallback"}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               10,
		MinWaitMs:              5,
		MaxCalls:               10,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           100,
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Enqueue a request
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x1111111111111111111111111111111111111111",
			"data": "0x01020304",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

	entry, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.False(t, bypass)

	// Wait for the batch to flush (multicall fails with "contract not found",
	// triggers fallback, fallback panics and recovers)
	select {
	case result := <-entry.ResultCh:
		// Should receive an error due to the panic in fallback
		require.Error(t, result.Error)
		require.Contains(t, result.Error.Error(), "panic in fallback forward")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for result - fallback panic recovery may have failed")
	}
}

func TestBatcher_MaxQueueSize_Enforcement(t *testing.T) {
	forwarder := &mockForwarder{}

	cfg := &common.Multicall3AggregationConfig{
		Enabled:                true,
		WindowMs:               1000, // Long window to prevent auto-flush
		MinWaitMs:              5,
		MaxCalls:               100,
		MaxCalldataBytes:       64000,
		MaxQueueSize:           3, // Small queue for testing
		MaxPendingBatches:      20,
		AllowCrossUserBatching: util.BoolPtr(true),
	}
	cfg.SetDefaults()

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Fill up the queue
	for i := 0; i < 3; i++ {
		jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
			map[string]interface{}{
				"to":   fmt.Sprintf("0x%040d", i+1),
				"data": "0x01020304",
			},
			"latest",
		})
		req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
		_, bypass, err := batcher.Enqueue(ctx, key, req)
		require.NoError(t, err)
		require.False(t, bypass, "request %d should be enqueued", i)
	}

	// Next request should bypass due to full queue
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x0000000000000000000000000000000000000099",
			"data": "0x01020304",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	_, bypass, err := batcher.Enqueue(ctx, key, req)
	require.NoError(t, err)
	require.True(t, bypass, "4th request should bypass due to full queue")
}

func TestBatchingKey_Validate(t *testing.T) {
	tests := []struct {
		name    string
		key     BatchingKey
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid key",
			key: BatchingKey{
				ProjectId:     "proj1",
				NetworkId:     "evm:1",
				BlockRef:      "latest",
				DirectivesKey: "v1:",
			},
			wantErr: false,
		},
		{
			name: "missing project id",
			key: BatchingKey{
				NetworkId: "evm:1",
				BlockRef:  "latest",
			},
			wantErr: true,
			errMsg:  "ProjectId is required",
		},
		{
			name: "missing network id",
			key: BatchingKey{
				ProjectId: "proj1",
				BlockRef:  "latest",
			},
			wantErr: true,
			errMsg:  "NetworkId is required",
		},
		{
			name: "missing block ref",
			key: BatchingKey{
				ProjectId: "proj1",
				NetworkId: "evm:1",
			},
			wantErr: true,
			errMsg:  "BlockRef is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.key.Validate()
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestBatcher_InvalidTargetLength_Bypass(t *testing.T) {
	forwarder := &mockForwarder{}

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

	batcher := NewBatcher(cfg, forwarder, nil)
	require.NotNil(t, batcher)
	defer batcher.Shutdown()

	ctx := context.Background()
	key := BatchingKey{
		ProjectId:     "test-project",
		NetworkId:     "evm:1",
		BlockRef:      "latest",
		DirectivesKey: DeriveDirectivesKey(nil),
	}

	// Request with invalid target address (21 bytes instead of 20)
	// 42 hex chars = 21 bytes
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{
		map[string]interface{}{
			"to":   "0x00112233445566778899aabbccddeeff00112233ab", // 21 bytes (42 hex chars)
			"data": "0x01020304",
		},
		"latest",
	})
	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	_, bypass, err := batcher.Enqueue(ctx, key, req)
	require.Error(t, err)
	require.True(t, bypass, "request with invalid target should bypass")
	require.Contains(t, err.Error(), "invalid target address length")
}
