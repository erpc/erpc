package data

import (
	"context"
	"io"
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
