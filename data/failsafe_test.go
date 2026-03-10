package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestCacheFailsafe_CreatePolicies_RejectsConsensus(t *testing.T) {
	logger := zerolog.New(io.Discard)
	cfg := &common.FailsafeConfig{
		Consensus: &common.ConsensusPolicyConfig{},
	}
	_, err := CreateCacheFailsafePolicies(&logger, "test-conn", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consensus is not supported")
}

func TestCacheFailsafe_CreatePolicies_RejectsHedgeQuantile(t *testing.T) {
	logger := zerolog.New(io.Discard)
	cfg := &common.FailsafeConfig{
		Hedge: &common.HedgePolicyConfig{
			Quantile: 0.95,
			MaxCount: 1,
		},
	}
	_, err := CreateCacheFailsafePolicies(&logger, "test-conn", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hedge quantile is not supported")
}

func TestCacheFailsafe_CreatePolicies_AcceptsValid(t *testing.T) {
	logger := zerolog.New(io.Discard)
	cfg := &common.FailsafeConfig{
		Timeout: &common.TimeoutPolicyConfig{
			Duration: common.Duration(2 * time.Second),
		},
		Retry: &common.RetryPolicyConfig{
			MaxAttempts: 3,
			Delay:       common.Duration(100 * time.Millisecond),
		},
		CircuitBreaker: &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    5,
			FailureThresholdCapacity: 10,
			HalfOpenAfter:            common.Duration(30 * time.Second),
		},
		Hedge: &common.HedgePolicyConfig{
			Delay:    common.Duration(200 * time.Millisecond),
			MaxCount: 1,
		},
	}
	policies, err := CreateCacheFailsafePolicies(&logger, "test-conn", cfg)
	require.NoError(t, err)
	assert.Len(t, policies, 4)
	assert.Contains(t, policies, "timeout")
	assert.Contains(t, policies, "retry")
	assert.Contains(t, policies, "circuitBreaker")
	assert.Contains(t, policies, "hedge")
}

func TestCacheFailsafe_RetryPolicy_DoesNotRetryRecordNotFound(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	notFoundErr := common.NewErrRecordNotFound("pk", "rk", "memory")
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, notFoundErr)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(10 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	// Should only be called once — no retry
	mc.AssertNumberOfCalls(t, "Get", 1)
}

func TestCacheFailsafe_RetryPolicy_DoesNotRetryContextCanceled(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, context.Canceled)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(10 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	mc.AssertNumberOfCalls(t, "Get", 1)
}

func TestCacheFailsafe_RetryPolicy_RetriesConnectionError(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	connErr := errors.New("connection refused")
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, connErr).Times(2)
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("data"), nil).Once()

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 5,
				Delay:       common.Duration(10 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("data"), result)
	mc.AssertNumberOfCalls(t, "Get", 3)
}

func TestCacheFailsafe_RetryPolicy_RespectsMaxAttempts(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	connErr := errors.New("connection refused")
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, connErr)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(10 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeRetryExceeded))
	mc.AssertNumberOfCalls(t, "Get", 3)
}

func TestCacheFailsafe_CircuitBreaker_DoesNotCountCacheMiss(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	notFoundErr := common.NewErrRecordNotFound("pk", "rk", "memory")
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, notFoundErr)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount: 2,
				HalfOpenAfter:         common.Duration(1 * time.Minute),
			},
		},
	}, nil)
	require.NoError(t, err)

	// Call many times — circuit should never open because misses don't count
	for i := 0; i < 10; i++ {
		_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	}
	mc.AssertNumberOfCalls(t, "Get", 10)
}

func TestCacheFailsafe_CircuitBreaker_OpensOnConnectionErrors(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	connErr := errors.New("connection refused")
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, connErr)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount: 2,
				HalfOpenAfter:         common.Duration(1 * time.Minute),
			},
		},
	}, nil)
	require.NoError(t, err)

	// First 2 calls should go through (reaching threshold)
	for i := 0; i < 2; i++ {
		_, _ = fc.Get(context.Background(), "", "pk", "rk", nil)
	}

	// Third call should get circuit breaker open error
	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen))
}

func TestCacheFailsafe_Timeout_Exceeded(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			ctx := args.Get(0).(context.Context)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}).
		Return(nil, context.DeadlineExceeded)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Timeout: &common.TimeoutPolicyConfig{
				Duration: common.Duration(50 * time.Millisecond),
			},
		},
	}, nil)
	require.NoError(t, err)

	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded))
}

func TestCacheFailsafe_Set_RetriesOnError(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	connErr := errors.New("connection refused")
	mc.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(connErr).Once()
	mc.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()

	fc, err := NewFailsafeConnector(&logger, mc, nil, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(10 * time.Millisecond),
			},
		},
	})
	require.NoError(t, err)

	ttl := 1 * time.Minute
	err = fc.Set(context.Background(), "pk", "rk", []byte("val"), &ttl)
	require.NoError(t, err)
	mc.AssertNumberOfCalls(t, "Set", 2)
}

func TestCacheFailsafe_Delete_RetriesOnError(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	connErr := errors.New("connection refused")
	mc.On("Delete", mock.Anything, mock.Anything, mock.Anything).
		Return(connErr).Once()
	mc.On("Delete", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Once()

	fc, err := NewFailsafeConnector(&logger, mc, nil, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(10 * time.Millisecond),
			},
		},
	})
	require.NoError(t, err)

	err = fc.Delete(context.Background(), "pk", "rk")
	require.NoError(t, err)
	mc.AssertNumberOfCalls(t, "Delete", 2)
}

func TestCacheFailsafe_Passthrough_List(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	expected := []KeyValuePair{{PartitionKey: "pk", RangeKey: "rk", Value: []byte("v")}}
	mc.On("List", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(expected, "next", nil)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{Retry: &common.RetryPolicyConfig{MaxAttempts: 3}},
	}, nil)
	require.NoError(t, err)

	result, token, err := fc.List(context.Background(), "idx", 10, "")
	require.NoError(t, err)
	assert.Equal(t, expected, result)
	assert.Equal(t, "next", token)
}

func TestCacheFailsafe_Get_UsesGetExecutors_Set_UsesSetExecutors(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	var getAttempts atomic.Int32
	var setAttempts atomic.Int32

	connErr := errors.New("connection refused")

	// Get always fails
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { getAttempts.Add(1) }).
		Return(nil, connErr)

	// Set always fails
	mc.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { setAttempts.Add(1) }).
		Return(connErr)

	// Get: 2 max attempts, Set: 4 max attempts — proves they use separate executors
	fc, err := NewFailsafeConnector(&logger, mc,
		[]*common.FailsafeConfig{{Retry: &common.RetryPolicyConfig{MaxAttempts: 2, Delay: common.Duration(10 * time.Millisecond)}}},
		[]*common.FailsafeConfig{{Retry: &common.RetryPolicyConfig{MaxAttempts: 4, Delay: common.Duration(10 * time.Millisecond)}}},
	)
	require.NoError(t, err)

	_, _ = fc.Get(context.Background(), "", "pk", "rk", nil)
	ttl := time.Minute
	_ = fc.Set(context.Background(), "pk", "rk", []byte("v"), &ttl)

	assert.Equal(t, int32(2), getAttempts.Load(), "Get should use getExecutors with maxAttempts=2")
	assert.Equal(t, int32(4), setAttempts.Load(), "Set should use setExecutors with maxAttempts=4")
}

func TestCacheFailsafe_ExecutorSelection_ByMethodFromContext(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	var callCount atomic.Int32
	connErr := errors.New("connection refused")

	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { callCount.Add(1) }).
		Return(nil, connErr)

	// Specific config for eth_getLogs: 2 attempts
	// Default config: 4 attempts
	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			MatchMethod: "eth_getLogs",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 2, Delay: common.Duration(10 * time.Millisecond)},
		},
		{
			MatchMethod: "*",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 4, Delay: common.Duration(10 * time.Millisecond)},
		},
	}, nil)
	require.NoError(t, err)

	// Create context with a request that has method "eth_getLogs"
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getLogs","params":[],"id":1}`))
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	callCount.Store(0)
	_, _ = fc.Get(ctx, "", "pk", "rk", nil)
	assert.Equal(t, int32(2), callCount.Load(), "eth_getLogs should match specific config with maxAttempts=2")

	// Now test with a different method
	req2 := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":[],"id":1}`))
	ctx2 := context.WithValue(context.Background(), common.RequestContextKey, req2)

	callCount.Store(0)
	_, _ = fc.Get(ctx2, "", "pk", "rk", nil)
	assert.Equal(t, int32(4), callCount.Load(), "eth_getBlockByNumber should match default config with maxAttempts=4")
}

func TestCacheFailsafe_TranslateError_RetryExceeded(t *testing.T) {
	err := common.NewErrFailsafeRetryExceeded(scopeConnector, errors.New("cause"), nil)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeRetryExceeded))
}

func TestCacheFailsafe_TranslateError_TimeoutExceeded(t *testing.T) {
	err := common.NewErrFailsafeTimeoutExceeded(scopeConnector, errors.New("cause"), nil)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeTimeoutExceeded))
}

func TestCacheFailsafe_TranslateError_CircuitBreakerOpen(t *testing.T) {
	err := common.NewErrFailsafeCircuitBreakerOpen(scopeConnector, errors.New("cause"), nil)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFailsafeCircuitBreakerOpen))
}

func TestCacheFailsafe_NoConfig_PassesThrough(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("data"), nil)

	// Even with empty configs, no-op fallback executor ensures normal operation
	fc, err := NewFailsafeConnector(&logger, mc, nil, nil)
	require.NoError(t, err)

	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("data"), result)
	mc.AssertNumberOfCalls(t, "Get", 1)
}

func TestCacheFailsafe_Validation_RejectsConsensus(t *testing.T) {
	cfg := &common.ConnectorConfig{
		Id:     "test",
		Driver: common.DriverMemory,
		Memory: &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"},
		FailsafeForGets: []*common.FailsafeConfig{
			{Consensus: &common.ConsensusPolicyConfig{}},
		},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consensus is not supported")
}

func TestCacheFailsafe_Validation_RejectsHedgeQuantile(t *testing.T) {
	cfg := &common.ConnectorConfig{
		Id:     "test",
		Driver: common.DriverMemory,
		Memory: &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"},
		FailsafeForSets: []*common.FailsafeConfig{
			{Hedge: &common.HedgePolicyConfig{Quantile: 0.9, MaxCount: 1}},
		},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hedge quantile is not supported")
}

func TestCacheFailsafe_Validation_AcceptsValidConfig(t *testing.T) {
	cfg := &common.ConnectorConfig{
		Id:     "test",
		Driver: common.DriverMemory,
		Memory: &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"},
		FailsafeForGets: []*common.FailsafeConfig{
			{
				Timeout: &common.TimeoutPolicyConfig{Duration: common.Duration(2 * time.Second)},
				Retry:   &common.RetryPolicyConfig{MaxAttempts: 3},
			},
		},
	}
	err := cfg.Validate()
	require.NoError(t, err)
}

func TestCacheFailsafe_RetryPolicy_DoesNotRetryRecordExpired(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	expiredErr := common.NewErrRecordExpired("pk", "rk", "memory", 0, 0)
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, expiredErr)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{Retry: &common.RetryPolicyConfig{MaxAttempts: 3, Delay: common.Duration(10 * time.Millisecond)}},
	}, nil)
	require.NoError(t, err)

	_, err = fc.Get(context.Background(), "", "pk", "rk", nil)
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordExpired))
	mc.AssertNumberOfCalls(t, "Get", 1)
}

func TestCacheFailsafe_Get_Success(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("hello"), nil)

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Timeout: &common.TimeoutPolicyConfig{Duration: common.Duration(1 * time.Second)},
			Retry:   &common.RetryPolicyConfig{MaxAttempts: 3},
		},
	}, nil)
	require.NoError(t, err)

	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), result)
}

func TestCacheFailsafe_Set_Success(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	mc.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil)

	fc, err := NewFailsafeConnector(&logger, mc, nil, []*common.FailsafeConfig{
		{
			Timeout: &common.TimeoutPolicyConfig{Duration: common.Duration(1 * time.Second)},
		},
	})
	require.NoError(t, err)

	ttl := time.Minute
	err = fc.Set(context.Background(), "pk", "rk", []byte("val"), &ttl)
	require.NoError(t, err)
}

func TestCacheFailsafe_RetryWithBackoff(t *testing.T) {
	logger := zerolog.New(io.Discard)
	mc := NewMockConnector("test")

	connErr := errors.New("connection refused")
	// First 2 calls fail, third succeeds
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, connErr).Times(2)
	mc.On("Get", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]byte("data"), nil).Once()

	fc, err := NewFailsafeConnector(&logger, mc, []*common.FailsafeConfig{
		{
			Retry: &common.RetryPolicyConfig{
				MaxAttempts:     5,
				Delay:           common.Duration(10 * time.Millisecond),
				BackoffMaxDelay: common.Duration(100 * time.Millisecond),
				BackoffFactor:   2,
			},
		},
	}, nil)
	require.NoError(t, err)

	start := time.Now()
	result, err := fc.Get(context.Background(), "", "pk", "rk", nil)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Equal(t, []byte("data"), result)
	mc.AssertNumberOfCalls(t, "Get", 3)
	// With backoff: 10ms + 20ms = 30ms minimum for 2 retries before success
	assert.True(t, elapsed >= 20*time.Millisecond, fmt.Sprintf("expected backoff delay, got %v", elapsed))
}
