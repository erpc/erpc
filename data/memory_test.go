package data

import (
	"context"
	"fmt"
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
		err := connector.Set(ctx, "pk1", "rk1", []byte("value1"), &ttl)
		require.NoError(t, err)

		time.Sleep(30 * time.Millisecond)

		// Verify item exists immediately
		val, err := connector.Get(ctx, "", "pk1", "rk1")
		require.NoError(t, err)
		require.Equal(t, []byte("value1"), val)

		// Wait for TTL to expire
		time.Sleep(150 * time.Millisecond)

		// Verify item is gone
		_, err = connector.Get(ctx, "", "pk1", "rk1")
		require.Error(t, err)
		require.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
	})

	t.Run("item without TTL doesn't expire", func(t *testing.T) {
		// Set item with no TTL
		err := connector.Set(ctx, "pk3", "rk1", []byte("value1"), nil)
		require.NoError(t, err)

		// Wait a bit (less than typical eviction times for a non-full cache)
		time.Sleep(100 * time.Millisecond)

		// Verify item still exists
		val, err := connector.Get(ctx, "", "pk3", "rk1")
		require.NoError(t, err)
		require.Equal(t, []byte("value1"), val)
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
			MaxItems:     1000,
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
		err = connector.Set(ctx, "pk1", "rk1", []byte("value1"), nil)
		require.NoError(t, err)

		// Wait for Ristretto's eventual consistency
		connector.cache.Wait()

		val, err := connector.Get(ctx, "", "pk1", "rk1")
		require.NoError(t, err)
		require.Equal(t, []byte("value1"), val)

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
			MaxItems:     1000,
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

func TestMemoryConnector_ChainIsolation(t *testing.T) {
	// Setup Memory connector
	logger := zerolog.New(io.Discard)
	ctx := context.Background()

	connector, err := NewMemoryConnector(ctx, &logger, "test-chain-isolation", &common.MemoryConnectorConfig{
		MaxItems:     100_000,
		MaxTotalSize: "1GB",
	})
	require.NoError(t, err)
	defer connector.Close()

	// Define test data for two different chains
	chainA := "evm:1"   // Ethereum mainnet
	chainB := "evm:137" // Polygon
	method := "eth_blockNumber"
	blockNumberA := []byte("0x1234567")
	blockNumberB := []byte("0x7654321")

	// Store block number for chain A
	partitionKeyA := fmt.Sprintf("%s:%s", chainA, method)
	rangeKey := "latest"
	err = connector.Set(ctx, partitionKeyA, rangeKey, blockNumberA, nil)
	require.NoError(t, err, "failed to set block number for chain A")

	// Wait for Ristretto's eventual consistency
	connector.cache.Wait()

	// Store block number for chain B
	partitionKeyB := fmt.Sprintf("%s:%s", chainB, method)
	err = connector.Set(ctx, partitionKeyB, rangeKey, blockNumberB, nil)
	require.NoError(t, err, "failed to set block number for chain B")

	// Wait for Ristretto's eventual consistency
	connector.cache.Wait()

	// Verify chain A can read its own data
	valueA, err := connector.Get(ctx, ConnectorMainIndex, partitionKeyA, rangeKey)
	require.NoError(t, err, "failed to get block number for chain A")
	require.Equal(t, blockNumberA, valueA, "chain A should get its own block number")

	// Verify chain B can read its own data
	valueB, err := connector.Get(ctx, ConnectorMainIndex, partitionKeyB, rangeKey)
	require.NoError(t, err, "failed to get block number for chain B")
	require.Equal(t, blockNumberB, valueB, "chain B should get its own block number")

	// Verify chain A cannot read chain B's data by trying to get a non-existent key
	// The key format ensures isolation: each chain has its own partition key
	wrongKey := fmt.Sprintf("%s:%s", chainA, "wrong_method")
	_, err = connector.Get(ctx, ConnectorMainIndex, wrongKey, rangeKey)
	require.Error(t, err, "should not find data for non-existent key")
	require.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))

	// Test that the keys are truly different
	require.NotEqual(t, partitionKeyA, partitionKeyB, "partition keys for different chains should be different")

	// Test with wildcard partition key (reverse index)
	// This tests the reverse index functionality for chain isolation
	wildcardPartitionKeyA := fmt.Sprintf("%s:*", chainA)
	wildcardPartitionKeyB := fmt.Sprintf("%s:*", chainB)

	// Try to get data using wildcard for chain A
	valueWildcardA, err := connector.Get(ctx, ConnectorReverseIndex, wildcardPartitionKeyA, rangeKey)
	if err == nil {
		// If reverse index exists, it should return chain A's data
		require.Equal(t, blockNumberA, valueWildcardA, "wildcard lookup for chain A should return chain A's data")
	}

	// Try to get data using wildcard for chain B
	valueWildcardB, err := connector.Get(ctx, ConnectorReverseIndex, wildcardPartitionKeyB, rangeKey)
	if err == nil {
		// If reverse index exists, it should return chain B's data
		require.Equal(t, blockNumberB, valueWildcardB, "wildcard lookup for chain B should return chain B's data")
	}

	// Additional verification: Store data with same range key but different partition keys
	// to ensure they don't overwrite each other
	testRangeKey := "test-isolation"
	testValueA := []byte("value-for-chain-A")
	testValueB := []byte("value-for-chain-B")

	err = connector.Set(ctx, partitionKeyA, testRangeKey, testValueA, nil)
	require.NoError(t, err)
	connector.cache.Wait()

	err = connector.Set(ctx, partitionKeyB, testRangeKey, testValueB, nil)
	require.NoError(t, err)
	connector.cache.Wait()

	// Verify both values exist independently
	gotA, err := connector.Get(ctx, ConnectorMainIndex, partitionKeyA, testRangeKey)
	require.NoError(t, err)
	require.Equal(t, testValueA, gotA)

	gotB, err := connector.Get(ctx, ConnectorMainIndex, partitionKeyB, testRangeKey)
	require.NoError(t, err)
	require.Equal(t, testValueB, gotB)
}
