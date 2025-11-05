package data

import (
	"context"
	"sync"
	"sync/atomic"
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

// TestIntegrationTimeoutFlow tests the complete timeout flow from state poller to Redis operations
func TestIntegrationTimeoutFlow(t *testing.T) {
	t.Run("complete flow with working Redis", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create shared state with memory connector (simulates working Redis)
		ssr, err := NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			ClusterKey:      "test",
			FallbackTimeout: common.Duration(1 * time.Second),
			LockTtl:         common.Duration(30 * time.Second),
			// In this scenario we expect the first update to complete in-foreground.
			// The refresh function sleeps 100ms, so allow a foreground wait > 100ms.
			UpdateMaxWait: common.Duration(200 * time.Millisecond),
			Connector: &common.ConnectorConfig{
				Id:     "test-memory",
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{
					MaxItems:     100,
					MaxTotalSize: "10MB",
				},
			},
		})
		require.NoError(t, err)

		counter := ssr.GetCounterInt64("test-flow", 100)

		// Simulate state poller context (45s timeout based on lockTtl + 15s)
		pollTimeout := ssr.GetLockTtl() + 15*time.Second
		pollCtx, pollCancel := context.WithTimeout(ctx, pollTimeout)
		defer pollCancel()

		start := time.Now()

		// First update - should be fast with no contention
		value, err := counter.TryUpdateIfStale(pollCtx, 5*time.Second, func(ctx context.Context) (int64, error) {
			// Simulate block fetch
			time.Sleep(100 * time.Millisecond)
			return 42, nil
		})

		elapsed := time.Since(start)

		assert.NoError(t, err)
		assert.Equal(t, int64(42), value)
		assert.Less(t, elapsed, 500*time.Millisecond, "First update should be fast")

		// Second update within debounce - should return cached
		start2 := time.Now()
		value2, err2 := counter.TryUpdateIfStale(pollCtx, 5*time.Second, func(ctx context.Context) (int64, error) {
			t.Fatal("Should not fetch when value is fresh")
			return 0, nil
		})
		elapsed2 := time.Since(start2)

		assert.NoError(t, err2)
		assert.Equal(t, int64(42), value2)
		assert.Less(t, elapsed2, 10*time.Millisecond, "Cached read should be instant")
	})

	t.Run("complete flow with lock contention", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ssr, err := NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
			ClusterKey:      "test",
			FallbackTimeout: common.Duration(1 * time.Second),
			LockTtl:         common.Duration(5 * time.Second), // Shorter for test
			// Simulate waiting for the lock (held ~1s by another instance)
			LockMaxWait: common.Duration(1200 * time.Millisecond),
			// Ensure we wait long enough for refresh work once lock is acquired
			UpdateMaxWait: common.Duration(500 * time.Millisecond),
			Connector: &common.ConnectorConfig{
				Id:     "test-memory",
				Driver: common.DriverMemory,
				Memory: &common.MemoryConnectorConfig{
					MaxItems:     100,
					MaxTotalSize: "10MB",
				},
			},
		})
		require.NoError(t, err)

		counter := ssr.GetCounterInt64("test-contention", 100)

		// Acquire lock from another "instance"
		lockCtx, lockCancel := context.WithTimeout(ctx, 10*time.Second)
		defer lockCancel()

		registry := ssr.(*sharedStateRegistry)
		lock, err := registry.connector.Lock(lockCtx, "test/test-contention", 3*time.Second)
		require.NoError(t, err)

		// Try to update while lock is held
		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			defer wg.Done()
			time.Sleep(1 * time.Second)
			_ = lock.Unlock(context.Background())
		}()

		pollTimeout := ssr.GetLockTtl() + 15*time.Second
		pollCtx, pollCancel := context.WithTimeout(ctx, pollTimeout)
		defer pollCancel()

		start := time.Now()
		value, err := counter.TryUpdateIfStale(pollCtx, 100*time.Millisecond, func(ctx context.Context) (int64, error) {
			return 123, nil
		})
		elapsed := time.Since(start)

		wg.Wait()

		assert.NoError(t, err)
		assert.Equal(t, int64(123), value)
		assert.Greater(t, elapsed, 900*time.Millisecond, "Should wait for lock")
		assert.Less(t, elapsed, 2*time.Second, "Should not wait too long")
	})

	t.Run("timeout budget validation", func(t *testing.T) {
		// Verify our timeout calculations leave enough time for operations
		lockTtl := 30 * time.Second
		pollTimeout := lockTtl + 15*time.Second // 45s

		// Worst case: wait full lockTtl for lock
		remainingAfterLock := pollTimeout - lockTtl // 15s

		// Operations that need to fit:
		getRemoteTime := 1 * time.Second
		fetchBlockTime := 10 * time.Second // Conservative estimate
		updateRemoteTime := 1 * time.Second

		totalOperationTime := getRemoteTime + fetchBlockTime + updateRemoteTime // 12s

		assert.Greater(t, remainingAfterLock, totalOperationTime,
			"Poll timeout should leave enough time for operations after max lock wait")
	})
}

// TestConcurrentPollers simulates multiple state pollers running simultaneously
func TestConcurrentPollers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ssr, err := NewSharedStateRegistry(ctx, &log.Logger, &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(1 * time.Second),
		LockTtl:         common.Duration(2 * time.Second),        // Short for test
		UpdateMaxWait:   common.Duration(300 * time.Millisecond), // > fetch duration (200ms)
		Connector: &common.ConnectorConfig{
			Id:     "test-memory",
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100,
				MaxTotalSize: "10MB",
			},
		},
	})
	require.NoError(t, err)

	counter := ssr.GetCounterInt64("test-concurrent", 100)

	// Track how many times the fetch function is called
	fetchCount := atomic.Int32{}
	currentValue := atomic.Int64{}

	// Simulate 5 concurrent pollers
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// Each poller tries to update
			pollTimeout := ssr.GetLockTtl() + 5*time.Second
			pollCtx, pollCancel := context.WithTimeout(ctx, pollTimeout)
			defer pollCancel()

			value, err := counter.TryUpdateIfStale(pollCtx, 100*time.Millisecond, func(ctx context.Context) (int64, error) {
				// Only one should actually fetch
				count := fetchCount.Add(1)
				newValue := currentValue.Add(1)
				t.Logf("Poller %d fetching, count=%d, value=%d", id, count, newValue)
				time.Sleep(200 * time.Millisecond) // Simulate fetch time
				return newValue, nil
			})

			assert.NoError(t, err)
			t.Logf("Poller %d got value: %d", id, value)
		}(i)
	}

	wg.Wait()

	// Only one poller should have fetched
	assert.Equal(t, int32(1), fetchCount.Load(), "Only one poller should fetch")
	assert.Equal(t, int64(1), counter.GetValue(), "Counter should be 1")
}
