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
		MaxItems: 100_000, MaxTotalSize: "1GB",
	})
	require.NoError(t, err)

	// Test cases
	t.Run("item expires after TTL", func(t *testing.T) {
		// Set item with 100ms TTL
		ttl := 100 * time.Millisecond
		err := connector.Set(ctx, "pk1", "rk1", "value1", &ttl)
		require.NoError(t, err)

		time.Sleep(30 * time.Millisecond)

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

	t.Run("item without TTL doesn't expire", func(t *testing.T) {
		// Set item with no TTL
		err := connector.Set(ctx, "pk3", "rk1", "value1", nil)
		require.NoError(t, err)

		// Wait a bit (less than typical eviction times for a non-full cache)
		time.Sleep(100 * time.Millisecond)

		// Verify item still exists
		val, err := connector.Get(ctx, "", "pk3", "rk1")
		require.NoError(t, err)
		require.Equal(t, "value1", val)
	})
}
