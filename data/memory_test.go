package data

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
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

func TestMemoryConnector_Metrics(t *testing.T) {
	// Setup logger
	logger := zerolog.New(io.Discard)
	ctx := context.Background()
	
	// Initialize histogram buckets for telemetry
	err := telemetry.SetHistogramBuckets("0.05,0.5,5,30")
	require.NoError(t, err)

	t.Run("metrics disabled by default", func(t *testing.T) {
		connector, err := NewMemoryConnector(ctx, &logger, "test-no-metrics", &common.MemoryConnectorConfig{
			MaxItems: 1000, MaxTotalSize: "10MB",
		})
		require.NoError(t, err)
		defer connector.Close()

		// Verify metrics are not enabled
		require.False(t, connector.emitMetrics)
		require.Nil(t, connector.stopMetrics)
	})

	t.Run("metrics enabled when configured", func(t *testing.T) {
		emitMetrics := true
		connector, err := NewMemoryConnector(ctx, &logger, "test-with-metrics", &common.MemoryConnectorConfig{
			MaxItems:    1000,
			MaxTotalSize: "10MB",
			EmitMetrics:  &emitMetrics,
		})
		require.NoError(t, err)
		defer connector.Close()

		// Verify metrics are enabled
		require.True(t, connector.emitMetrics)
		require.NotNil(t, connector.stopMetrics)
		require.NotNil(t, connector.cache.Metrics)

		// Perform some cache operations to generate metrics
		err = connector.Set(ctx, "pk1", "rk1", "value1", nil)
		require.NoError(t, err)

		// Wait for Ristretto's eventual consistency
		connector.cache.Wait()

		val, err := connector.Get(ctx, "", "pk1", "rk1")
		require.NoError(t, err)
		require.Equal(t, "value1", val)

		// Try to get a non-existent key to generate a miss
		_, err = connector.Get(ctx, "", "pk1", "nonexistent")
		require.Error(t, err)

		// Force metrics collection
		connector.collectAndEmitMetrics()

		// Verify that Ristretto metrics are being tracked
		metrics := connector.cache.Metrics
		require.NotNil(t, metrics)
		
		// Verify we can collect metrics without errors
		connector.collectAndEmitMetrics()

		// Verify the metrics collection completes without error
		require.True(t, connector.emitMetrics)
	})

	t.Run("metrics collection handles nil cache gracefully", func(t *testing.T) {
		emitMetrics := true
		connector, err := NewMemoryConnector(ctx, &logger, "test-graceful", &common.MemoryConnectorConfig{
			MaxItems:    1000,
			MaxTotalSize: "10MB",
			EmitMetrics:  &emitMetrics,
		})
		require.NoError(t, err)

		// Close the cache to simulate a nil cache scenario
		connector.cache.Close()
		connector.cache = nil

		// This should not panic
		connector.collectAndEmitMetrics()

		// Cleanup
		connector.Close()
	})
}
