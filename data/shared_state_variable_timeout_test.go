package data

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestTimeoutCalculationFix verifies that timeout calculations don't produce negative or zero values
func TestTimeoutCalculationFix(t *testing.T) {
	t.Run("small lockTtl scenario", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create config with very small lockTtl that would previously cause negative timeout
		cfg := &common.SharedStateConfig{
			ClusterKey:      "test",
			FallbackTimeout: common.Duration(3 * time.Second), // 3s
			LockTtl:         common.Duration(5 * time.Second), // 5s - smaller than operationBuffer
			Connector: &common.ConnectorConfig{
				Id:     "test-memory",
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{
					MaxItems:     100,
					MaxTotalSize: "10MB",
				},
			},
		}

		ssr, err := NewSharedStateRegistry(ctx, &log.Logger, cfg)
		require.NoError(t, err)

		counter1 := ssr.GetCounterInt64("test-timeout-1", 100)

		// Test TryUpdate - operationBuffer = fallbackTimeout * 2 = 6s
		// totalBuffer = 6s + 10s (DefaultOperationBuffer) = 16s
		// lockTtl (5s) < totalBuffer (16s), so should use MinOperationBuffer (5s)
		ctx1, cancel1 := context.WithTimeout(ctx, 30*time.Second)
		defer cancel1()

		value := counter1.TryUpdate(ctx1, 42)
		assert.Equal(t, int64(42), value, "TryUpdate should work with small lockTtl")

		// Test TryUpdateIfStale with a different counter to avoid staleness issues
		counter2 := ssr.GetCounterInt64("test-timeout-2", 100)

		// Test TryUpdateIfStale - operationBuffer = fallbackTimeout * 4 = 12s
		// totalBuffer = 12s + 10s (DefaultOperationBuffer) = 22s
		// lockTtl (5s) < totalBuffer (22s), so should use MinOperationBuffer (5s)
		ctx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
		defer cancel2()

		value2, err2 := counter2.TryUpdateIfStale(ctx2, 1*time.Millisecond, func(ctx context.Context) (int64, error) {
			return 123, nil
		})

		assert.NoError(t, err2)
		assert.Equal(t, int64(123), value2, "TryUpdateIfStale should work with small lockTtl")
	})

	t.Run("normal lockTtl scenario", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create config with normal lockTtl values
		cfg := &common.SharedStateConfig{
			ClusterKey:      "test",
			FallbackTimeout: common.Duration(3 * time.Second),  // 3s
			LockTtl:         common.Duration(30 * time.Second), // 30s - normal value
			Connector: &common.ConnectorConfig{
				Id:     "test-memory",
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{
					MaxItems:     100,
					MaxTotalSize: "10MB",
				},
			},
		}

		ssr, err := NewSharedStateRegistry(ctx, &log.Logger, cfg)
		require.NoError(t, err)

		counter1 := ssr.GetCounterInt64("test-normal-1", 100)

		// Test TryUpdate - operationBuffer = fallbackTimeout * 2 = 6s
		// totalBuffer = 6s + 10s (DefaultOperationBuffer) = 16s
		// lockTtl (30s) > totalBuffer (16s), so maxLockWaitTime = 30s - 16s = 14s
		ctx1, cancel1 := context.WithTimeout(ctx, 30*time.Second)
		defer cancel1()

		value := counter1.TryUpdate(ctx1, 456)
		assert.Equal(t, int64(456), value, "TryUpdate should work with normal lockTtl")

		// Test TryUpdateIfStale with a different counter to avoid staleness issues
		counter2 := ssr.GetCounterInt64("test-normal-2", 100)

		// Test TryUpdateIfStale - operationBuffer = fallbackTimeout * 4 = 12s
		// totalBuffer = 12s + 10s (DefaultOperationBuffer) = 22s
		// lockTtl (30s) > totalBuffer (22s), so maxLockWaitTime = 30s - 22s = 8s
		ctx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
		defer cancel2()

		value2, err2 := counter2.TryUpdateIfStale(ctx2, 1*time.Millisecond, func(ctx context.Context) (int64, error) {
			return 789, nil
		})

		assert.NoError(t, err2)
		assert.Equal(t, int64(789), value2, "TryUpdateIfStale should work with normal lockTtl")
	})

	t.Run("tight deadline scenario", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.SharedStateConfig{
			ClusterKey:      "test",
			FallbackTimeout: common.Duration(3 * time.Second),
			LockTtl:         common.Duration(30 * time.Second),
			Connector: &common.ConnectorConfig{
				Id:     "test-memory",
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{
					MaxItems:     100,
					MaxTotalSize: "10MB",
				},
			},
		}

		ssr, err := NewSharedStateRegistry(ctx, &log.Logger, cfg)
		require.NoError(t, err)

		counter := ssr.GetCounterInt64("test-deadline", 100)

		// Test with very tight deadline (2 seconds)
		ctx1, cancel1 := context.WithTimeout(ctx, 2*time.Second)
		defer cancel1()

		start := time.Now()
		value := counter.TryUpdate(ctx1, 999)
		elapsed := time.Since(start)

		assert.Equal(t, int64(999), value, "TryUpdate should work with tight deadline")
		assert.Less(t, elapsed, 2*time.Second, "Should not exceed deadline")
	})
}
