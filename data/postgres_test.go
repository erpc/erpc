package data

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestPostgreConnectorInitialization(t *testing.T) {
	t.Run("SucceedsValidConfigRealContainer", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req := testcontainers.ContainerRequest{
			Image:        "postgres:15-alpine",
			Env:          map[string]string{"POSTGRES_PASSWORD": "password"},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor:   wait.ForListeningPort("5432/tcp"),
		}
		postgresC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		require.NoError(t, err, "failed to start postgres container")
		defer postgresC.Terminate(ctx)

		host, err := postgresC.Host(ctx)
		require.NoError(t, err)
		port, err := postgresC.MappedPort(ctx, "5432")
		require.NoError(t, err)

		connURI := fmt.Sprintf("postgres://postgres:password@%s:%s/postgres?sslmode=disable", host, port.Port())

		cfg := &common.PostgreSQLConnectorConfig{
			Table:         "test_table",
			ConnectionUri: connURI,
			InitTimeout:   common.Duration(2 * time.Second),
			GetTimeout:    common.Duration(2 * time.Second),
			SetTimeout:    common.Duration(2 * time.Second),
			MinConns:      1,
			MaxConns:      5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-connector", cfg)
		// We expect no error, because it should succeed on the first attempt.
		require.NoError(t, err)

		// Ensure the connector reports StateReady via its initializer
		if connector.initializer != nil {
			state := connector.initializer.State()
			require.Equal(t, util.StateReady, state, "connector should be in ready state")
		}

		// Try a simple SET/GET to verify readiness.
		err = connector.Set(ctx, "testPK", "testRK", []byte("hello-world"), nil)
		require.NoError(t, err, "Set should succeed after successful initialization")

		val, err := connector.Get(ctx, "", "testPK", "testRK", nil)
		require.NoError(t, err, "Get should succeed for existing key")
		require.Equal(t, []byte("hello-world"), val)
	})

	t.Run("FailsOnFirstAttemptWithInvalidAddressButReturnsConnectorAnyway", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Intentionally invalid address
		cfg := &common.PostgreSQLConnectorConfig{
			Table:         "test_table",
			ConnectionUri: "postgres://user:pass@127.0.0.1:9876/bogusdb?sslmode=disable", // no server
			InitTimeout:   common.Duration(500 * time.Millisecond),
			GetTimeout:    common.Duration(500 * time.Millisecond),
			SetTimeout:    common.Duration(500 * time.Millisecond),
			MinConns:      1,
			MaxConns:      5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-connector-invalid-addr", cfg)
		// The constructor does not necessarily return an error if the first attempt fails;
		// it can return a connector with a not-ready state. So we expect no error here.
		require.NoError(t, err)

		if connector.initializer != nil {
			require.NotEqual(t, util.StateReady, connector.initializer.State(),
				"connector should not be in ready state if it failed to connect")
		}

		// Attempting to call Set or Get here should result in an error.
		err = connector.Set(ctx, "testPK", "testRK", []byte("value"), nil)
		require.Error(t, err, "should fail because Postgres is not connected")

		_, err = connector.Get(ctx, "", "testPK", "testRK", nil)
		require.Error(t, err, "should fail because Postgres is not connected")
	})
}

func TestPostgreSQLDistributedLocking(t *testing.T) {
	// Common setup for PostgreSQL connector for locking tests
	setupConnector := func(t *testing.T) (context.Context, *PostgreSQLConnector, func()) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())

		req := testcontainers.ContainerRequest{
			Image:        "postgres:15-alpine",
			Env:          map[string]string{"POSTGRES_PASSWORD": "password", "POSTGRES_DB": "testdb"},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor:   wait.ForLog("database system is ready to accept connections").WithStartupTimeout(2 * time.Minute),
		}
		postgresC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		require.NoError(t, err, "failed to start postgres container")

		host, err := postgresC.Host(ctx)
		require.NoError(t, err)
		port, err := postgresC.MappedPort(ctx, "5432")
		require.NoError(t, err)

		connURI := fmt.Sprintf("postgres://postgres:password@%s:%s/testdb?sslmode=disable", host, port.Port())

		cfg := &common.PostgreSQLConnectorConfig{
			Table:         "locking_test_table",
			ConnectionUri: connURI,
			InitTimeout:   common.Duration(5 * time.Second),
			GetTimeout:    common.Duration(2 * time.Second),
			SetTimeout:    common.Duration(2 * time.Second),
			MinConns:      1,
			MaxConns:      5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-lock-connector", cfg)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			if connector.initializer == nil {
				t.Log("Initializer is nil")
				return false
			}
			state := connector.initializer.State()
			if state != util.StateReady {
				t.Logf("Connector not ready, current state: %s, errors: %v", state, connector.initializer.Errors())
			}
			return state == util.StateReady
		}, 30*time.Second, 500*time.Millisecond, "connector did not become ready")

		cleanup := func() {
			cancel()
			postgresC.Terminate(context.Background()) // Use a background context for termination
		}

		return ctx, connector, cleanup
	}

	t.Run("SuccessfulImmediateLockAcquisition", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()

		lockKey := "pg-lock-immediate"
		// TTL is not strictly used by pg_advisory_xact_lock, but pass a value for API compliance
		lock, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.NoError(t, err, "should acquire lock without issues")
		require.NotNil(t, lock, "lock should not be nil")
		require.False(t, lock.IsNil(), "lock.IsNil should be false")

		// Check if underlying transaction is present
		pgLock, ok := lock.(*postgresLock)
		require.True(t, ok, "lock should be of type *postgresLock")
		require.NotNil(t, pgLock.tx, "transaction should be present in acquired lock")

		err = lock.Unlock(ctx) // This commits the transaction
		require.NoError(t, err, "unlock should succeed")
		require.Nil(t, pgLock.tx, "transaction should be nil after unlock")
	})

	t.Run("LockFailsIfAlreadyHeld", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()
		lockKey := "pg-lock-already-held"

		// Goroutine 1 (main test goroutine) acquires the lock
		lock1, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.NoError(t, err, "lock1: should acquire lock")
		require.NotNil(t, lock1)

		// Goroutine 2 attempts to acquire the same lock
		lock2, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.Error(t, err, "lock2: should fail to acquire lock as it is already held")
		require.Nil(t, lock2, "lock2: should be nil as acquisition failed")
		require.Contains(t, err.Error(), "already locked", "error message should indicate lock is already held")

		// Goroutine 1 releases the lock
		err = lock1.Unlock(ctx)
		require.NoError(t, err, "lock1: unlock should succeed")
	})

	t.Run("LockSucceedsAfterBeingReleased", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()
		lockKey := "pg-lock-release-then-acquire"

		// Acquire and release the lock first
		lock1, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.NoError(t, err, "lock1: initial acquisition should succeed")
		require.NotNil(t, lock1)
		err = lock1.Unlock(ctx)
		require.NoError(t, err, "lock1: unlock should succeed")

		// Attempt to acquire the lock again
		lock2, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.NoError(t, err, "lock2: acquisition should succeed after release")
		require.NotNil(t, lock2)
		err = lock2.Unlock(ctx)
		require.NoError(t, err, "lock2: unlock should succeed")
	})

	t.Run("LockIsReleasedOnCommit", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()
		lockKey := "pg-lock-commit-release"

		// Acquire lock (this starts a transaction)
		lock1, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.NoError(t, err)
		pgLock1, ok := lock1.(*postgresLock)
		require.True(t, ok)

		// Try to acquire again (should fail as lock1's transaction holds it)
		_, err = connector.Lock(ctx, lockKey, 1*time.Second)
		require.Error(t, err, "second lock attempt should fail while first is active")

		// Commit the transaction for lock1
		err = pgLock1.Unlock(ctx) // Unlock is a commit
		require.NoError(t, err)

		// Now, acquiring the lock should succeed
		lock2, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.NoError(t, err, "should acquire lock after first transaction committed")
		require.NotNil(t, lock2)
		err = lock2.Unlock(ctx)
		require.NoError(t, err)
	})

	t.Run("LockIsReleasedOnRollback", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()
		lockKey := "pg-lock-rollback-release"

		// Acquire lock (starts a transaction)
		lock1, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.NoError(t, err)
		pgLock1, ok := lock1.(*postgresLock)
		require.True(t, ok)

		// Manually rollback the transaction instead of calling Unlock()
		// Note: This is for testing the rollback scenario. Normally, Unlock() handles commit.
		if pgLock1.tx != nil {
			err = pgLock1.tx.Rollback(ctx)
			require.NoError(t, err, "manual rollback should succeed")
			pgLock1.tx = nil // Simulate that unlock would do this too on rollback path (though current Unlock always commits)
		}

		// Now, acquiring the lock should succeed
		lock2, err := connector.Lock(ctx, lockKey, 5*time.Second)
		require.NoError(t, err, "should acquire lock after first transaction rolled back")
		require.NotNil(t, lock2)
		err = lock2.Unlock(ctx)
		require.NoError(t, err)
	})
}

func TestPostgreSQLReadReplicaGetReadPool(t *testing.T) {
	t.Run("ReturnsMainConnWhenNoReplicas", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         &pgxpool.Pool{},
			readReplicas: nil,
			replicaIndex: 0,
		}

		pool := connector.getReadPool()
		assert.Equal(t, connector.conn, pool)
	})

	t.Run("ReturnsMainConnWhenReplicasEmpty", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         &pgxpool.Pool{},
			readReplicas: []*pgxpool.Pool{},
			replicaIndex: 0,
		}

		pool := connector.getReadPool()
		assert.Equal(t, connector.conn, pool)
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

		for i := 0; i < 10; i++ {
			pool := connector.getReadPool()
			assert.Equal(t, replica1, pool)
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

		counts := make(map[*pgxpool.Pool]int)
		for i := 0; i < 9; i++ {
			pool := connector.getReadPool()
			counts[pool]++
		}

		assert.Equal(t, 3, counts[replica1])
		assert.Equal(t, 3, counts[replica2])
		assert.Equal(t, 3, counts[replica3])
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
		for _, replica := range replicas {
			assert.Equal(t, expectedCount, counts[replica])
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
		assert.Equal(t, totalCalls/2, count1)
		assert.Equal(t, totalCalls/2, count2)
	})

	t.Run("FallsBackToPrimaryWhenReplicaIsNil", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		primary := &pgxpool.Pool{}
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         primary,
			readReplicas: []*pgxpool.Pool{nil},
			replicaIndex: 0,
		}

		pool := connector.getReadPool()
		assert.Equal(t, primary, pool)
	})

	t.Run("FallsBackToPrimaryForNilReplicaInMixedList", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		primary := &pgxpool.Pool{}
		replica1 := &pgxpool.Pool{}
		connector := &PostgreSQLConnector{
			id:           "test",
			logger:       &logger,
			conn:         primary,
			readReplicas: []*pgxpool.Pool{replica1, nil},
			replicaIndex: 0,
		}

		pool1 := connector.getReadPool()
		assert.Equal(t, replica1, pool1)

		pool2 := connector.getReadPool()
		assert.Equal(t, primary, pool2)
	})
}

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
		assert.NotContains(t, string(jsonBytes), "secretpass")
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

func TestPostgreSQLReadReplicaIntegration(t *testing.T) {
	startPostgresContainer := func(t *testing.T, ctx context.Context, dbName string) (testcontainers.Container, string) {
		req := testcontainers.ContainerRequest{
			Image:        "postgres:15-alpine",
			Env:          map[string]string{"POSTGRES_PASSWORD": "password", "POSTGRES_DB": dbName},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor: wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithStartupTimeout(2*time.Minute),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(2*time.Minute),
			),
		}
		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		require.NoError(t, err)

		host, err := container.Host(ctx)
		require.NoError(t, err)
		port, err := container.MappedPort(ctx, "5432")
		require.NoError(t, err)

		time.Sleep(500 * time.Millisecond)

		connURI := fmt.Sprintf("postgres://postgres:password@%s:%s/%s?sslmode=disable", host, port.Port(), dbName)
		return container, connURI
	}

	t.Run("InitializesWithSingleReadReplica", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		replicaC, replicaURI := startPostgresContainer(t, ctx, "replica_db")
		defer replicaC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{replicaURI},
			InitTimeout:     common.Duration(30 * time.Second),
			GetTimeout:      common.Duration(5 * time.Second),
			SetTimeout:      common.Duration(5 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-replica-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 60*time.Second, 500*time.Millisecond)

		assert.Len(t, connector.readReplicas, 1)
		assert.NotNil(t, connector.readReplicas[0])
	})

	t.Run("InitializesWithMultipleReadReplicas", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		replica1C, replica1URI := startPostgresContainer(t, ctx, "replica1_db")
		defer replica1C.Terminate(ctx)

		replica2C, replica2URI := startPostgresContainer(t, ctx, "replica2_db")
		defer replica2C.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_multi_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{replica1URI, replica2URI},
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-multi-replica-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		assert.Len(t, connector.readReplicas, 2)
		assert.NotNil(t, connector.readReplicas[0])
		assert.NotNil(t, connector.readReplicas[1])
	})

	t.Run("SkipsFailedReplicaDuringInitialization", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		replicaC, replicaURI := startPostgresContainer(t, ctx, "replica_db")
		defer replicaC.Terminate(ctx)

		invalidReplicaURI := "postgres://postgres:password@127.0.0.1:59999/invalid_db?sslmode=disable"

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_partial_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{invalidReplicaURI, replicaURI},
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-partial-replica-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		assert.Len(t, connector.readReplicas, 1)
		assert.NotNil(t, connector.readReplicas[0])
	})

	t.Run("WorksWithNoReplicasConfigured", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_no_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: nil,
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-no-replica-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		assert.Empty(t, connector.readReplicas)

		err = connector.Set(ctx, "testPK", "testRK", []byte("test-value"), nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, "", "testPK", "testRK", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("test-value"), val)
	})

	t.Run("AllReplicasFailStillWorkWithPrimaryOnly", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:         "test_all_fail_replica_table",
			ConnectionUri: primaryURI,
			ReadReplicaUris: []string{
				"postgres://postgres:password@127.0.0.1:59998/invalid1?sslmode=disable",
				"postgres://postgres:password@127.0.0.1:59999/invalid2?sslmode=disable",
			},
			InitTimeout: common.Duration(10 * time.Second),
			GetTimeout:  common.Duration(2 * time.Second),
			SetTimeout:  common.Duration(2 * time.Second),
			MinConns:    1,
			MaxConns:    5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-all-fail-replica-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		assert.Empty(t, connector.readReplicas)

		err = connector.Set(ctx, "testPK", "testRK", []byte("test-value"), nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, "", "testPK", "testRK", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("test-value"), val)
	})

	t.Run("WriteGoesToPrimary", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		replicaC, replicaURI := startPostgresContainer(t, ctx, "replica_db")
		defer replicaC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_write_primary_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{replicaURI},
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-write-primary-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		err = connector.Set(ctx, "writePK", "writeRK", []byte("write-value"), nil)
		require.NoError(t, err)

		connector.connMu.RLock()
		var value []byte
		err = connector.conn.QueryRow(ctx, fmt.Sprintf(
			"SELECT value FROM %s WHERE partition_key = $1 AND range_key = $2", connector.table),
			"writePK", "writeRK").Scan(&value)
		connector.connMu.RUnlock()

		require.NoError(t, err)
		assert.Equal(t, []byte("write-value"), value)
	})

	t.Run("DeleteGoesToPrimary", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		replicaC, replicaURI := startPostgresContainer(t, ctx, "replica_db")
		defer replicaC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_delete_primary_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{replicaURI},
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-delete-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		err = connector.Set(ctx, "deletePK", "deleteRK", []byte("to-be-deleted"), nil)
		require.NoError(t, err)

		err = connector.Delete(ctx, "deletePK", "deleteRK")
		require.NoError(t, err)

		connector.connMu.RLock()
		var count int
		err = connector.conn.QueryRow(ctx, fmt.Sprintf(
			"SELECT COUNT(*) FROM %s WHERE partition_key = $1 AND range_key = $2", connector.table),
			"deletePK", "deleteRK").Scan(&count)
		connector.connMu.RUnlock()

		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	t.Run("LockGoesToPrimary", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		replicaC, replicaURI := startPostgresContainer(t, ctx, "replica_db")
		defer replicaC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_lock_primary_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{replicaURI},
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-lock-primary-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		lock, err := connector.Lock(ctx, "test-lock-key", 5*time.Second)
		require.NoError(t, err)
		require.NotNil(t, lock)

		err = lock.Unlock(ctx)
		require.NoError(t, err)
	})

	t.Run("ListUsesReplicas", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "shared_db")
		defer primaryC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_list_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{primaryURI},
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-list-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		for i := 0; i < 5; i++ {
			err = connector.Set(ctx, fmt.Sprintf("listPK%d", i), "listRK", []byte(fmt.Sprintf("value%d", i)), nil)
			require.NoError(t, err)
		}

		results, nextToken, err := connector.List(ctx, ConnectorMainIndex, 10, "")
		require.NoError(t, err)
		assert.Len(t, results, 5)
		assert.Empty(t, nextToken)
	})

	t.Run("GetWithWildcardUsesReplicas", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "shared_db")
		defer primaryC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_wildcard_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{primaryURI},
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-wildcard-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		err = connector.Set(ctx, "wildcardPK", "pattern_001", []byte("value1"), nil)
		require.NoError(t, err)
		err = connector.Set(ctx, "wildcardPK", "pattern_002", []byte("value2"), nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "wildcardPK", "pattern_*", nil)
		require.NoError(t, err)
		assert.NotEmpty(t, val)
	})
}

func TestPostgreSQLReadReplicaFallback(t *testing.T) {
	startPostgresContainer := func(t *testing.T, ctx context.Context, dbName string) (testcontainers.Container, string) {
		req := testcontainers.ContainerRequest{
			Image:        "postgres:15-alpine",
			Env:          map[string]string{"POSTGRES_PASSWORD": "password", "POSTGRES_DB": dbName},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor: wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithStartupTimeout(2*time.Minute),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(2*time.Minute),
			),
		}
		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		require.NoError(t, err)

		host, err := container.Host(ctx)
		require.NoError(t, err)
		port, err := container.MappedPort(ctx, "5432")
		require.NoError(t, err)

		time.Sleep(500 * time.Millisecond)

		connURI := fmt.Sprintf("postgres://postgres:password@%s:%s/%s?sslmode=disable", host, port.Port(), dbName)
		return container, connURI
	}

	t.Run("FallsBackToPrimaryWhenReplicaStops", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		replicaC, replicaURI := startPostgresContainer(t, ctx, "replica_db")

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_fallback_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{replicaURI},
			InitTimeout:     common.Duration(10 * time.Second),
			GetTimeout:      common.Duration(2 * time.Second),
			SetTimeout:      common.Duration(2 * time.Second),
			MinConns:        1,
			MaxConns:        5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-fallback-connector", cfg)
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond)

		err = connector.Set(ctx, "fallbackPK", "fallbackRK", []byte("fallback-value"), nil)
		require.NoError(t, err)

		err = replicaC.Terminate(ctx)
		require.NoError(t, err)

		time.Sleep(1 * time.Second)

		val, err := connector.Get(ctx, "", "fallbackPK", "fallbackRK", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("fallback-value"), val)
	})
}
