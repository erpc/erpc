package data

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// =============================================================================
// Read Replica Tests - Unit Tests
// =============================================================================

func TestPostgreSQLReadReplicaGetReadPool(t *testing.T) {
	t.Run("ReturnsMainConnWhenNoReplicas", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         &pgxpool.Pool{}, // Mock primary pool
			readReplicas: nil,             // No replicas
			replicaIndex: 0,
		}

		pool := connector.getReadPool()
		assert.Equal(t, connector.conn, pool, "should return primary connection when no replicas configured")
	})

	t.Run("ReturnsMainConnWhenReplicasEmpty", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         &pgxpool.Pool{},
			readReplicas: []*pgxpool.Pool{}, // Empty slice
			replicaIndex: 0,
		}

		pool := connector.getReadPool()
		assert.Equal(t, connector.conn, pool, "should return primary connection when replicas slice is empty")
	})

	t.Run("RoundRobinWithSingleReplica", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		replica1 := &pgxpool.Pool{}
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         &pgxpool.Pool{},
			readReplicas: []*pgxpool.Pool{replica1},
			replicaIndex: 0,
		}

		// All calls should return the same replica
		for i := 0; i < 10; i++ {
			pool := connector.getReadPool()
			assert.Equal(t, replica1, pool, "should always return the single replica")
		}
	})

	t.Run("RoundRobinWithMultipleReplicas", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		replica1 := &pgxpool.Pool{}
		replica2 := &pgxpool.Pool{}
		replica3 := &pgxpool.Pool{}
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         &pgxpool.Pool{},
			readReplicas: []*pgxpool.Pool{replica1, replica2, replica3},
			replicaIndex: 0,
		}

		// Track which replicas are returned
		counts := make(map[*pgxpool.Pool]int)

		// Call getReadPool multiple times
		for i := 0; i < 9; i++ {
			pool := connector.getReadPool()
			counts[pool]++
		}

		// Each replica should be called 3 times (9 calls / 3 replicas)
		assert.Equal(t, 3, counts[replica1], "replica1 should be selected 3 times")
		assert.Equal(t, 3, counts[replica2], "replica2 should be selected 3 times")
		assert.Equal(t, 3, counts[replica3], "replica3 should be selected 3 times")
	})

	t.Run("RoundRobinDistributionIsEven", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		numReplicas := 5
		replicas := make([]*pgxpool.Pool, numReplicas)
		for i := 0; i < numReplicas; i++ {
			replicas[i] = &pgxpool.Pool{}
		}

		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         &pgxpool.Pool{},
			readReplicas: replicas,
			replicaIndex: 0,
		}

		counts := make(map[*pgxpool.Pool]int)
		numCalls := 1000

		for i := 0; i < numCalls; i++ {
			pool := connector.getReadPool()
			counts[pool]++
		}

		expectedCount := numCalls / numReplicas
		for i, replica := range replicas {
			assert.Equal(t, expectedCount, counts[replica],
				"replica %d should be selected %d times", i, expectedCount)
		}
	})

	t.Run("RoundRobinConcurrentAccess", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		replica1 := &pgxpool.Pool{}
		replica2 := &pgxpool.Pool{}
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         &pgxpool.Pool{},
			readReplicas: []*pgxpool.Pool{replica1, replica2},
			replicaIndex: 0,
		}

		var count1, count2 int64

		var wg sync.WaitGroup
		numGoroutines := 100
		callsPerGoroutine := 100

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < callsPerGoroutine; j++ {
					pool := connector.getReadPool()
					if pool == replica1 {
						atomic.AddInt64(&count1, 1)
					} else if pool == replica2 {
						atomic.AddInt64(&count2, 1)
					}
				}
			}()
		}

		wg.Wait()

		totalCalls := int64(numGoroutines * callsPerGoroutine)

		// Each should get roughly half
		assert.Equal(t, totalCalls/2, count1, "replica1 should get half the calls")
		assert.Equal(t, totalCalls/2, count2, "replica2 should get half the calls")
	})

	t.Run("FallsBackToPrimaryWhenReplicaIsNil", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		primary := &pgxpool.Pool{}
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         primary,
			readReplicas: []*pgxpool.Pool{nil}, // Nil replica (disconnected)
			replicaIndex: 0,
		}

		pool := connector.getReadPool()
		assert.Equal(t, primary, pool, "should fall back to primary when replica is nil")
	})

	t.Run("FallsBackToPrimaryForNilReplicaInMixedList", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		primary := &pgxpool.Pool{}
		replica1 := &pgxpool.Pool{}
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         primary,
			readReplicas: []*pgxpool.Pool{replica1, nil}, // Second replica is nil
			replicaIndex: 0,
		}

		// First call should return replica1 (index 0)
		pool1 := connector.getReadPool()
		assert.Equal(t, replica1, pool1, "first call should return replica1")

		// Second call should fall back to primary (index 1 is nil)
		pool2 := connector.getReadPool()
		assert.Equal(t, primary, pool2, "second call should fall back to primary for nil replica")
	})
}

// =============================================================================
// Read Replica Configuration Tests
// =============================================================================

func TestPostgreSQLReadReplicaConfig(t *testing.T) {
	t.Run("ConfigWithReadReplicaUris", func(t *testing.T) {
		cfg := &common.PostgreSQLConnectorConfig{
			ConnectionUri: "postgres://user:pass@primary:5432/db",
			ReadReplicaUris: []string{
				"postgres://user:pass@replica1:5432/db",
				"postgres://user:pass@replica2:5432/db",
			},
			Table:       "test_table",
			MinConns:    1,
			MaxConns:    10,
			InitTimeout: common.Duration(5 * time.Second),
			GetTimeout:  common.Duration(2 * time.Second),
			SetTimeout:  common.Duration(2 * time.Second),
		}

		assert.Equal(t, "postgres://user:pass@primary:5432/db", cfg.ConnectionUri)
		assert.Len(t, cfg.ReadReplicaUris, 2)
		assert.Equal(t, "postgres://user:pass@replica1:5432/db", cfg.ReadReplicaUris[0])
		assert.Equal(t, "postgres://user:pass@replica2:5432/db", cfg.ReadReplicaUris[1])
	})

	t.Run("ConfigMarshalJSONRedactsCredentials", func(t *testing.T) {
		cfg := &common.PostgreSQLConnectorConfig{
			ConnectionUri: "postgres://user:secretpass@primary:5432/db",
			ReadReplicaUris: []string{
				"postgres://user:secretpass@replica1:5432/db",
				"postgres://user:secretpass@replica2:5432/db",
			},
			Table:       "test_table",
			MinConns:    1,
			MaxConns:    10,
			InitTimeout: common.Duration(5 * time.Second),
			GetTimeout:  common.Duration(2 * time.Second),
			SetTimeout:  common.Duration(2 * time.Second),
		}

		jsonBytes, err := cfg.MarshalJSON()
		assert.NoError(t, err)

		jsonStr := string(jsonBytes)
		assert.NotContains(t, jsonStr, "secretpass", "password should be redacted")
	})

	t.Run("ConfigWithEmptyReadReplicaUris", func(t *testing.T) {
		cfg := &common.PostgreSQLConnectorConfig{
			ConnectionUri:   "postgres://user:pass@primary:5432/db",
			ReadReplicaUris: []string{},
			Table:           "test_table",
			MinConns:        1,
			MaxConns:        10,
			InitTimeout:     common.Duration(5 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
		}

		assert.Empty(t, cfg.ReadReplicaUris)
	})

	t.Run("ConfigWithNilReadReplicaUris", func(t *testing.T) {
		cfg := &common.PostgreSQLConnectorConfig{
			ConnectionUri:   "postgres://user:pass@primary:5432/db",
			ReadReplicaUris: nil,
			Table:           "test_table",
			MinConns:        1,
			MaxConns:        10,
			InitTimeout:     common.Duration(5 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
		}

		assert.Nil(t, cfg.ReadReplicaUris)
	})
}
