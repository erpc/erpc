package upstream

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// Mock upstream for testing
type mockUpstreamForRetry struct {
	mock.Mock
	common.Upstream
}

func (m *mockUpstreamForRetry) Config() *common.UpstreamConfig {
	args := m.Called()
	return args.Get(0).(*common.UpstreamConfig)
}

func (m *mockUpstreamForRetry) EvmSyncingState() common.EvmSyncingState {
	args := m.Called()
	return args.Get(0).(common.EvmSyncingState)
}

func (m *mockUpstreamForRetry) EvmAssertBlockAvailability(ctx context.Context, forMethod string, confidence common.AvailbilityConfidence, forceRefreshIfStale bool, blockNumber int64) (bool, error) {
	args := m.Called(ctx, forMethod, confidence, forceRefreshIfStale, blockNumber)
	return args.Bool(0), args.Error(1)
}

// Add other required methods to satisfy the interface
func (m *mockUpstreamForRetry) Id() string {
	return "mock-upstream"
}

func (m *mockUpstreamForRetry) NetworkId() string {
	return "evm:123"
}

func (m *mockUpstreamForRetry) VendorName() string {
	return "mock"
}

func (m *mockUpstreamForRetry) Logger() *zerolog.Logger {
	logger := zerolog.New(nil)
	return &logger
}

func (m *mockUpstreamForRetry) EvmStatePoller() common.EvmStatePoller {
	return nil
}

func (m *mockUpstreamForRetry) EvmIsBlockFinalized(ctx context.Context, blockNumber int64, forceFreshIfStale bool) (bool, error) {
	return false, nil
}

func (m *mockUpstreamForRetry) EvmLatestBlock() (int64, error) {
	return 0, nil
}

func (m *mockUpstreamForRetry) EvmFinalizedBlock() (int64, error) {
	return 0, nil
}

func (m *mockUpstreamForRetry) EvmGetChainId(ctx context.Context) (string, error) {
	return "1", nil
}

// Test helper to execute retry policy
func executeRetryPolicy(t *testing.T, cfg *common.RetryPolicyConfig, scope common.Scope, response *common.NormalizedResponse, err error) (attempts int, finalErr error) {
	policy, policyErr := createRetryPolicy(scope, cfg)
	assert.NoError(t, policyErr)

	executor := failsafe.NewExecutor(policy)
	attempts = 0

	_, finalErr = executor.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts = exec.Attempts()
		// Always return the same response/error to test if retry logic is triggered
		return response, err
	})

	return attempts, finalErr
}

// Helper to create a mock response
func createMockResponse(isEmpty bool, request *common.NormalizedRequest, upstream common.Upstream) *common.NormalizedResponse {
	resp := common.NewNormalizedResponse().WithRequest(request)
	if upstream != nil {
		resp.SetUpstream(upstream)
	}

	if isEmpty {
		// Set empty result
		resp = resp.WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[]`), nil))
	} else {
		// Set non-empty result
		resp = resp.WithJsonRpcResponse(common.MustNewJsonRpcResponseFromBytes([]byte(`"0x1"`), []byte(`[{"data": "0x123"}]`), nil))
	}

	return resp
}

func TestRetryPolicy_EmptyResultWithConfidence(t *testing.T) {
	tests := []struct {
		name            string
		confidence      common.AvailbilityConfidence
		blockAvailable  bool
		expectedRetries int
		description     string
	}{
		{
			name:            "Finalized_BlockAvailable_NoRetry",
			confidence:      common.AvailbilityConfidenceFinalized,
			blockAvailable:  true,
			expectedRetries: 1, // No retry, only initial attempt
			description:     "When block is finalized and available, should not retry empty response",
		},
		{
			name:            "Finalized_BlockNotAvailable_Retry",
			confidence:      common.AvailbilityConfidenceFinalized,
			blockAvailable:  false,
			expectedRetries: 3, // Should retry up to max attempts
			description:     "When block is not finalized, should retry empty response up to max attempts",
		},
		{
			name:            "BlockHead_BlockAvailable_NoRetry",
			confidence:      common.AvailbilityConfidenceBlockHead,
			blockAvailable:  true,
			expectedRetries: 1, // No retry
			description:     "When block is available at head, should not retry empty response",
		},
		{
			name:            "BlockHead_BlockNotAvailable_Retry",
			confidence:      common.AvailbilityConfidenceBlockHead,
			blockAvailable:  false,
			expectedRetries: 3, // Should retry up to max attempts
			description:     "When block is not available at head, should retry empty response up to max attempts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create retry config with confidence
			cfg := &common.RetryPolicyConfig{
				MaxAttempts:           3,
				EmptyResultConfidence: tt.confidence,
			}

			// Create mock request
			req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
			req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

			// Create mock upstream
			mockUpstream := new(mockUpstreamForRetry)
			mockUpstream.On("Config").Return(&common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			}).Maybe()
			mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing).Maybe()
			mockUpstream.On("EvmAssertBlockAvailability", mock.Anything, "eth_getBlockByNumber", tt.confidence, false, int64(100)).Return(tt.blockAvailable, nil).Maybe()

			// Create mock response
			mockResp := createMockResponse(true, req, mockUpstream)

			// Execute retry policy
			attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)

			assert.Equal(t, tt.expectedRetries, attempts, tt.description)
			mockUpstream.AssertExpectations(t)
		})
	}
}

func TestRetryPolicy_EmptyResultWithIgnore(t *testing.T) {
	tests := []struct {
		name            string
		method          string
		ignoreList      []string
		expectedRetries int
		description     string
	}{
		{
			name:            "MethodInIgnoreList_ShouldNotRetry",
			method:          "eth_getBalance",
			ignoreList:      []string{"eth_getBalance", "eth_getCode"},
			expectedRetries: 1, // No retry
			description:     "When method is in ignore list, should NOT retry empty response",
		},
		{
			name:            "MethodNotInIgnoreList_CheckAvailability",
			method:          "eth_getTransactionReceipt",
			ignoreList:      []string{"eth_getBalance", "eth_getCode"},
			expectedRetries: 3, // Should retry up to max attempts (no block number in request)
			description:     "When method is not in ignore list and has no block number, should retry",
		},
		{
			name:            "EmptyIgnoreList_CheckAvailability",
			method:          "eth_getBalance",
			ignoreList:      []string{},
			expectedRetries: 1, // No retry
			description:     "When ignore list is empty, should check block availability",
		},
		{
			name:            "NilIgnoreList_CheckAvailability",
			method:          "eth_getBalance",
			ignoreList:      nil,
			expectedRetries: 1, // No retry
			description:     "When ignore list is nil, should check block availability",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create retry config with ignore list
			cfg := &common.RetryPolicyConfig{
				MaxAttempts:           3,
				EmptyResultIgnore:     tt.ignoreList,
				EmptyResultConfidence: common.AvailbilityConfidenceBlockHead,
			}

			// Create mock request
			req := common.NewNormalizedRequest([]byte(`{"method":"` + tt.method + `","params":["0x742d35Cc6634C0532925a3b844Bc9e7595f8fA49","0x64"],"id":1,"jsonrpc":"2.0"}`))
			req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

			// Create mock upstream
			mockUpstream := new(mockUpstreamForRetry)
			mockUpstream.On("Config").Return(&common.UpstreamConfig{
				Type: common.UpstreamTypeEvm,
				Evm:  &common.EvmUpstreamConfig{},
			}).Maybe()
			mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing).Maybe()

			// Only expect availability check if method is not in ignore list
			shouldCheckAvailability := true
			for _, ignore := range tt.ignoreList {
				if ignore == tt.method {
					shouldCheckAvailability = false
					break
				}
			}
			if shouldCheckAvailability {
				mockUpstream.On("EvmAssertBlockAvailability", mock.Anything, tt.method, common.AvailbilityConfidenceBlockHead, false, int64(100)).Return(true, nil).Maybe()
			}

			// Create mock response
			mockResp := createMockResponse(true, req, mockUpstream)

			// Execute retry policy
			attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)

			assert.Equal(t, tt.expectedRetries, attempts, tt.description)
			mockUpstream.AssertExpectations(t)
		})
	}
}

func TestRetryPolicy_EdgeCases(t *testing.T) {
	t.Run("NonEmptyResponse_NoRetry", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0x123"],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		mockResp := createMockResponse(false, req, nil) // Non-empty response

		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)
		assert.Equal(t, 1, attempts, "Non-empty response should not be retried")
	})

	t.Run("NoRetryEmptyDirective_NoRetry", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0x123"],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: false}) // RetryEmpty is false

		mockResp := createMockResponse(true, req, nil)

		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)
		assert.Equal(t, 1, attempts, "Empty response without RetryEmpty directive should not be retried")
	})

	t.Run("SyncingUpstream_AlwaysRetry", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		mockUpstream := new(mockUpstreamForRetry)
		mockUpstream.On("Config").Return(&common.UpstreamConfig{
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{},
		}).Maybe()
		mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateSyncing).Maybe() // Syncing state

		mockResp := createMockResponse(true, req, mockUpstream)

		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)
		assert.Equal(t, 3, attempts, "Empty response from syncing upstream should always be retried up to max attempts")
	})

	t.Run("NoBlockNumber_AlwaysRetry", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		}

		// Request without block number
		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0x123"],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		mockUpstream := new(mockUpstreamForRetry)
		mockUpstream.On("Config").Return(&common.UpstreamConfig{
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{},
		}).Maybe()
		mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing).Maybe()

		mockResp := createMockResponse(true, req, mockUpstream)

		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)
		assert.Equal(t, 3, attempts, "Empty response without block number should be retried up to max attempts")
	})

	t.Run("AvailabilityCheckError_Retry", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		mockUpstream := new(mockUpstreamForRetry)
		mockUpstream.On("Config").Return(&common.UpstreamConfig{
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{},
		}).Maybe()
		mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing).Maybe()
		mockUpstream.On("EvmAssertBlockAvailability", mock.Anything, "eth_getBlockByNumber", common.AvailbilityConfidenceFinalized, false, int64(100)).
			Return(false, errors.New("availability check failed")).Maybe()

		mockResp := createMockResponse(true, req, mockUpstream)

		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)
		assert.Equal(t, 3, attempts, "Empty response with availability check error should be retried up to max attempts")
	})

	t.Run("ErrorResponse_AlwaysRetry", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		}

		// Test with error instead of response
		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, nil, errors.New("some error"))
		assert.Equal(t, 3, attempts, "Error responses should be retried up to max attempts")
	})

	t.Run("WriteMethod_NoRetry", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_sendTransaction","params":[],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		mockResp := createMockResponse(true, req, nil)

		// Write methods should not be retried even with error
		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, errors.New("some error"))
		assert.Equal(t, 1, attempts, "Write methods should not be retried")
	})
}

func TestRetryPolicy_CombinedConfidenceAndIgnore(t *testing.T) {
	t.Run("MethodInIgnoreList_IgnoresConfidence", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
			EmptyResultIgnore:     []string{"eth_getBalance"},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0x742d35Cc6634C0532925a3b844Bc9e7595f8fA49","0x64"],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		mockUpstream := new(mockUpstreamForRetry)
		mockUpstream.On("Config").Return(&common.UpstreamConfig{
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{},
		}).Maybe()
		mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing).Maybe()
		// Should not call EvmAssertBlockAvailability because method is in ignore list

		mockResp := createMockResponse(true, req, mockUpstream)

		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)
		assert.Equal(t, 1, attempts, "Method in ignore list should NOT retry empty response")
		mockUpstream.AssertExpectations(t)
		mockUpstream.AssertNotCalled(t, "EvmAssertBlockAvailability", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("MethodNotInIgnoreList_UsesConfidence", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
			EmptyResultIgnore:     []string{"eth_getBalance"},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		mockUpstream := new(mockUpstreamForRetry)
		mockUpstream.On("Config").Return(&common.UpstreamConfig{
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{},
		}).Maybe()
		mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing).Maybe()
		mockUpstream.On("EvmAssertBlockAvailability", mock.Anything, "eth_getBlockByNumber", common.AvailbilityConfidenceFinalized, false, int64(100)).
			Return(true, nil).Maybe()

		mockResp := createMockResponse(true, req, mockUpstream)

		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)
		assert.Equal(t, 1, attempts, "Method not in ignore list should use confidence check")
		mockUpstream.AssertExpectations(t)
	})
}

func TestRetryPolicy_UpstreamScope(t *testing.T) {
	t.Run("UpstreamScope_NoEmptyRetryLogic", func(t *testing.T) {
		cfg := &common.RetryPolicyConfig{
			MaxAttempts:           3,
			EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
			EmptyResultIgnore:     []string{"eth_getBalance"},
		}

		req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0x742d35Cc6634C0532925a3b844Bc9e7595f8fA49","0x64"],"id":1,"jsonrpc":"2.0"}`))
		req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

		mockResp := createMockResponse(true, req, nil)

		// Upstream scope with error should retry
		attempts, _ := executeRetryPolicy(t, cfg, common.ScopeUpstream, mockResp, errors.New("some error"))
		assert.Equal(t, 3, attempts, "Upstream scope should retry on error up to max attempts")

		// Upstream scope without error should not retry empty responses
		attempts, _ = executeRetryPolicy(t, cfg, common.ScopeUpstream, mockResp, nil)
		assert.Equal(t, 1, attempts, "Upstream scope should not retry empty responses without error")
	})
}

func TestRetryPolicy_MaxAttemptsRespected(t *testing.T) {
	cfg := &common.RetryPolicyConfig{
		MaxAttempts:           2, // Only allow 2 attempts total
		EmptyResultConfidence: common.AvailbilityConfidenceBlockHead,
	}

	req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

	mockUpstream := new(mockUpstreamForRetry)
	mockUpstream.On("Config").Return(&common.UpstreamConfig{
		Type: common.UpstreamTypeEvm,
		Evm:  &common.EvmUpstreamConfig{},
	}).Maybe()
	mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing).Maybe()
	mockUpstream.On("EvmAssertBlockAvailability", mock.Anything, "eth_getBlockByNumber", common.AvailbilityConfidenceBlockHead, false, int64(100)).
		Return(false, nil).Maybe() // Block not available, should retry

	mockResp := createMockResponse(true, req, mockUpstream)

	policy, err := createRetryPolicy(common.ScopeNetwork, cfg)
	assert.NoError(t, err)

	executor := failsafe.NewExecutor(policy)
	attempts := 0

	_, _ = executor.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts = exec.Attempts()
		// Always return empty response to trigger retry
		return mockResp, nil
	})

	assert.Equal(t, 2, attempts, "Should respect MaxAttempts limit")
}

func TestRetryPolicy_Debug(t *testing.T) {
	// Create retry config with confidence
	cfg := &common.RetryPolicyConfig{
		MaxAttempts:           3,
		EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
	}

	// Create mock request
	req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

	// Test block extraction
	_, bn, err := evm.ExtractBlockReferenceFromRequest(context.TODO(), req)
	t.Logf("Block extraction: bn=%d, err=%v", bn, err)

	// Create mock upstream
	mockUpstream := new(mockUpstreamForRetry)
	mockUpstream.On("Config").Return(&common.UpstreamConfig{
		Type: common.UpstreamTypeEvm,
		Evm:  &common.EvmUpstreamConfig{},
	}).Maybe()
	mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing).Maybe()
	mockUpstream.On("EvmAssertBlockAvailability", mock.Anything, "eth_getBlockByNumber", common.AvailbilityConfidenceFinalized, false, int64(100)).Return(true, nil).Maybe()

	// Create mock response
	mockResp := createMockResponse(true, req, mockUpstream)

	// Execute retry policy
	attempts, _ := executeRetryPolicy(t, cfg, common.ScopeNetwork, mockResp, nil)

	t.Logf("Attempts: %d", attempts)
}

func TestRetryPolicy_SimpleEmpty(t *testing.T) {
	// Very simple test to verify the basic retry logic
	cfg := &common.RetryPolicyConfig{
		MaxAttempts:           2,
		EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
	}

	// Test with error - should retry
	policy, _ := createRetryPolicy(common.ScopeNetwork, cfg)
	executor := failsafe.NewExecutor(policy)
	attempts := 0

	_, _ = executor.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts = exec.Attempts()
		return nil, errors.New("some error")
	})

	assert.Equal(t, 2, attempts, "Should retry on error")

	// Test with non-empty response - should NOT retry
	req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0x123"],"id":1,"jsonrpc":"2.0"}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})
	nonEmptyResp := createMockResponse(false, req, nil)

	executor2 := failsafe.NewExecutor(policy)
	attempts2 := 0

	_, _ = executor2.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts2 = exec.Attempts()
		return nonEmptyResp, nil
	})

	assert.Equal(t, 1, attempts2, "Should not retry non-empty response")

	// Test with empty response but no RetryEmpty directive - should NOT retry
	req2 := common.NewNormalizedRequest([]byte(`{"method":"eth_getBalance","params":["0x123"],"id":1,"jsonrpc":"2.0"}`))
	req2.SetDirectives(&common.RequestDirectives{RetryEmpty: false})
	emptyResp := createMockResponse(true, req2, nil)

	executor3 := failsafe.NewExecutor(policy)
	attempts3 := 0

	_, _ = executor3.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts3 = exec.Attempts()
		return emptyResp, nil
	})

	assert.Equal(t, 1, attempts3, "Should not retry without RetryEmpty directive")
}

func TestRetryPolicy_EmptyWithUpstream(t *testing.T) {
	cfg := &common.RetryPolicyConfig{
		MaxAttempts:           3,
		EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
	}

	// Create mock request with block number
	req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

	// Test 1: Empty response with no upstream - should retry
	emptyResp := createMockResponse(true, req, nil)

	policy, _ := createRetryPolicy(common.ScopeNetwork, cfg)
	executor := failsafe.NewExecutor(policy)
	attempts := 0

	_, _ = executor.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts = exec.Attempts()
		return emptyResp, nil
	})

	assert.Equal(t, 3, attempts, "Should retry empty response with no upstream")

	// Test 2: Empty response with upstream but wrong type - should retry
	mockUpstream := new(mockUpstreamForRetry)
	mockUpstream.On("Config").Return(&common.UpstreamConfig{
		Type: "solana", // Not EVM
	})

	emptyResp2 := createMockResponse(true, req, mockUpstream)

	executor2 := failsafe.NewExecutor(policy)
	attempts2 := 0

	_, _ = executor2.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts2 = exec.Attempts()
		return emptyResp2, nil
	})

	assert.Equal(t, 3, attempts2, "Should retry empty response with non-EVM upstream")
}

func TestRetryPolicy_EmptyWithEvmUpstream(t *testing.T) {
	cfg := &common.RetryPolicyConfig{
		MaxAttempts:           3,
		EmptyResultConfidence: common.AvailbilityConfidenceFinalized,
		EmptyResultIgnore:     []string{},
	}

	// Create mock request with block number
	req := common.NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber","params":["0x64",true],"id":1,"jsonrpc":"2.0"}`))
	req.SetDirectives(&common.RequestDirectives{RetryEmpty: true})

	// Test with EVM upstream that is syncing - should retry
	mockUpstream := new(mockUpstreamForRetry)
	mockUpstream.On("Config").Return(&common.UpstreamConfig{
		Type: common.UpstreamTypeEvm,
		Evm:  &common.EvmUpstreamConfig{},
	})
	mockUpstream.On("EvmSyncingState").Return(common.EvmSyncingStateSyncing)

	emptyResp := createMockResponse(true, req, mockUpstream)

	policy, _ := createRetryPolicy(common.ScopeNetwork, cfg)
	executor := failsafe.NewExecutor(policy)
	attempts := 0

	_, _ = executor.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts = exec.Attempts()
		return emptyResp, nil
	})

	assert.Equal(t, 3, attempts, "Should retry empty response when upstream is syncing")
	mockUpstream.AssertNotCalled(t, "EvmAssertBlockAvailability", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)

	// Test with EVM upstream that is NOT syncing and block is available
	mockUpstream2 := new(mockUpstreamForRetry)
	mockUpstream2.On("Config").Return(&common.UpstreamConfig{
		Type: common.UpstreamTypeEvm,
		Evm:  &common.EvmUpstreamConfig{},
	})
	mockUpstream2.On("EvmSyncingState").Return(common.EvmSyncingStateNotSyncing)
	mockUpstream2.On("EvmAssertBlockAvailability", mock.Anything, "eth_getBlockByNumber", common.AvailbilityConfidenceFinalized, false, int64(100)).Return(true, nil)

	emptyResp2 := createMockResponse(true, req, mockUpstream2)

	executor2 := failsafe.NewExecutor(policy)
	attempts2 := 0
	callCount := 0

	_, _ = executor2.GetWithExecution(func(exec failsafe.Execution[*common.NormalizedResponse]) (*common.NormalizedResponse, error) {
		attempts2 = exec.Attempts()
		callCount++
		t.Logf("Attempt %d, call count %d", attempts2, callCount)
		return emptyResp2, nil
	})

	t.Logf("Final attempts: %d", attempts2)
	assert.Equal(t, 1, attempts2, "Should NOT retry empty response when block is available")
	mockUpstream2.AssertExpectations(t)
}

func TestRetryPolicy_TypeAssertion(t *testing.T) {
	// Test type assertion directly
	mockUpstream := new(mockUpstreamForRetry)
	mockUpstream.On("Config").Return(&common.UpstreamConfig{
		Type: common.UpstreamTypeEvm,
		Evm:  &common.EvmUpstreamConfig{},
	})

	// Check if it implements common.Upstream
	var _ common.Upstream = mockUpstream

	// Check if it can be type asserted to EvmUpstream
	if evmUp, ok := interface{}(mockUpstream).(common.EvmUpstream); ok {
		t.Logf("Type assertion to EvmUpstream succeeded: %T", evmUp)
	} else {
		t.Errorf("Type assertion to EvmUpstream failed for %T", mockUpstream)
	}
}

func TestFailsafeMatcherFinality(t *testing.T) {
	// Test single finality matcher
	t.Run("SingleFinalityMatcher", func(t *testing.T) {
		cfg := &common.FailsafeConfig{
			Matchers: []*common.MatcherConfig{
				{
					Method:   "eth_getBlockByNumber",
					Finality: []common.DataFinalityState{common.DataFinalityStateFinalized},
					Action:   common.MatcherInclude,
				},
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
			},
		}

		// Test that the configuration is properly set up
		assert.Len(t, cfg.Matchers, 1)
		assert.Equal(t, "eth_getBlockByNumber", cfg.Matchers[0].Method)
		assert.Len(t, cfg.Matchers[0].Finality, 1)
		assert.Equal(t, common.DataFinalityStateFinalized, cfg.Matchers[0].Finality[0])
		assert.Equal(t, 3, cfg.Retry.MaxAttempts)
	})

	// Test array finality matcher
	t.Run("ArrayFinalityMatcher", func(t *testing.T) {
		cfg := &common.FailsafeConfig{
			Matchers: []*common.MatcherConfig{
				{
					Method:   "eth_*",
					Finality: []common.DataFinalityState{common.DataFinalityStateFinalized, common.DataFinalityStateUnfinalized},
					Action:   common.MatcherInclude,
				},
			},
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 5,
			},
		}

		// Test that the configuration is properly set up
		assert.Len(t, cfg.Matchers, 1)
		assert.Equal(t, "eth_*", cfg.Matchers[0].Method)
		assert.Len(t, cfg.Matchers[0].Finality, 2)
		assert.Contains(t, []common.DataFinalityState(cfg.Matchers[0].Finality), common.DataFinalityStateFinalized)
		assert.Contains(t, []common.DataFinalityState(cfg.Matchers[0].Finality), common.DataFinalityStateUnfinalized)
		assert.Equal(t, 5, cfg.Retry.MaxAttempts)
	})
}
