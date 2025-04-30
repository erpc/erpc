package data

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestMemoryConnector_TTL(t *testing.T) {
	// Setup
	logger := zerolog.New(io.Discard)
	ctx := context.Background()
	connector, err := NewMemoryConnector(ctx, &logger, "test", &common.MemoryConnectorConfig{
		MaxItems: 100,
	})
	require.NoError(t, err)

	// Test cases
	t.Run("item expires after TTL", func(t *testing.T) {
		// Set item with 100ms TTL
		ttl := 100 * time.Millisecond
		err := connector.Set(ctx, "pk1", "rk1", "value1", &ttl)
		require.NoError(t, err)

		// Verify item exists immediately
		val, err := connector.Get(ctx, "", "pk1", "rk1")
		require.NoError(t, err)
		require.Equal(t, "value1", val)

		// Wait for TTL to expire
		time.Sleep(150 * time.Millisecond)

		// Verify item is gone
		_, err = connector.Get(ctx, "", "pk1", "rk1")
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	})

	t.Run("cleanup removes expired items", func(t *testing.T) {
		// Set multiple items with different TTLs
		ttl1 := 50 * time.Millisecond
		ttl2 := 200 * time.Millisecond
		err := connector.Set(ctx, "pk2", "rk1", "value1", &ttl1)
		require.NoError(t, err)
		err = connector.Set(ctx, "pk2", "rk2", "value2", &ttl2)
		require.NoError(t, err)

		// Wait for first TTL to expire
		time.Sleep(100 * time.Millisecond)

		// Force cleanup
		connector.cleanupExpired()

		// Verify first item is gone but second remains
		_, err = connector.Get(ctx, "", "pk2", "rk1")
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))

		val, err := connector.Get(ctx, "", "pk2", "rk2")
		require.NoError(t, err)
		require.Equal(t, "value2", val)
	})

	t.Run("item without TTL doesn't expire", func(t *testing.T) {
		// Set item with no TTL
		err := connector.Set(ctx, "pk3", "rk1", "value1", nil)
		require.NoError(t, err)

		// Wait and force cleanup
		time.Sleep(100 * time.Millisecond)
		connector.cleanupExpired()

		// Verify item still exists
		val, err := connector.Get(ctx, "", "pk3", "rk1")
		require.NoError(t, err)
		require.Equal(t, "value1", val)
	})
}

func TestMemoryConnector_LockThunderingHerd(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ctx := context.Background()
	connector, err := NewMemoryConnector(ctx, &logger, "herd", &common.MemoryConnectorConfig{
		MaxItems: 10,
	})
	require.NoError(t, err)

	const goroutines = 100
	var acquired int32

	var wg sync.WaitGroup
	wg.Add(goroutines)

	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start

			lock, err := connector.Lock(ctx, "herd-key", 100*time.Millisecond)
			if err == nil {
				atomic.AddInt32(&acquired, 1)
				// Hold the lock briefly to give others a chance to collide
				time.Sleep(20 * time.Millisecond)
				_ = lock.Unlock(ctx)
			} else {
				require.True(t, common.HasErrorCode(err, common.ErrCodeLockAlreadyHeld))
			}
		}()
	}

	close(start)
	wg.Wait()

	// Exactly one goroutine should have acquired the lock at any moment,
	// but over the whole run we expect at least one successful acquire.
	require.Equal(t, int32(1), acquired, "expected exactly one successful lock acquisition")
}
