//go:build integration

package data

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
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

// =============================================================================
// Read Replica Tests - Integration Tests
// =============================================================================

func TestPostgreSQLReadReplicaIntegration(t *testing.T) {
	// Helper to start a postgres container
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
		require.NoError(t, err, "failed to start postgres container")

		host, err := container.Host(ctx)
		require.NoError(t, err)
		port, err := container.MappedPort(ctx, "5432")
		require.NoError(t, err)

		// Give PostgreSQL a moment to fully initialize
		time.Sleep(500 * time.Millisecond)

		connURI := fmt.Sprintf("postgres://postgres:password@%s:%s/%s?sslmode=disable", host, port.Port(), dbName)
		return container, connURI
	}

	t.Run("InitializesWithSingleReadReplica", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start primary
		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		// Start replica (in real scenario this would be a physical replica, but for testing we use another instance)
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

		// Verify replica was initialized
		assert.Len(t, connector.readReplicas, 1, "should have 1 read replica")
		assert.NotNil(t, connector.readReplicas[0], "read replica pool should not be nil")
	})

	t.Run("InitializesWithMultipleReadReplicas", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start primary
		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		// Start multiple replicas
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

		// Verify replicas were initialized
		assert.Len(t, connector.readReplicas, 2, "should have 2 read replicas")
		assert.NotNil(t, connector.readReplicas[0], "read replica 1 pool should not be nil")
		assert.NotNil(t, connector.readReplicas[1], "read replica 2 pool should not be nil")
	})

	t.Run("SkipsFailedReplicaDuringInitialization", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start primary
		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		// Start one valid replica
		replicaC, replicaURI := startPostgresContainer(t, ctx, "replica_db")
		defer replicaC.Terminate(ctx)

		// Invalid replica URI
		invalidReplicaURI := "postgres://postgres:password@127.0.0.1:59999/invalid_db?sslmode=disable"

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_partial_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{invalidReplicaURI, replicaURI}, // First fails, second succeeds
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

		// Only the valid replica should be initialized
		assert.Len(t, connector.readReplicas, 1, "should have 1 read replica (invalid one skipped)")
		assert.NotNil(t, connector.readReplicas[0], "valid read replica pool should not be nil")
	})

	t.Run("WorksWithNoReplicasConfigured", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start primary only
		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_no_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: nil, // No replicas
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

		assert.Empty(t, connector.readReplicas, "should have no read replicas")

		// Operations should still work
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

		// Start primary only
		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		// All invalid replica URIs
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

		// No replicas should be initialized
		assert.Empty(t, connector.readReplicas, "should have no read replicas when all fail")

		// Operations should still work via primary
		err = connector.Set(ctx, "testPK", "testRK", []byte("test-value"), nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, "", "testPK", "testRK", nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("test-value"), val)
	})

	t.Run("WriteOperationsAlwaysGoToPrimary", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start primary and replica
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

		// Write data
		err = connector.Set(ctx, "writePK", "writeRK", []byte("write-value"), nil)
		require.NoError(t, err, "Set should succeed on primary")

		// Verify data exists on primary by reading directly
		// (reads might go to replica which won't have the data in this test setup)
		connector.connMu.RLock()
		var value []byte
		err = connector.conn.QueryRow(ctx, fmt.Sprintf(
			"SELECT value FROM %s WHERE partition_key = $1 AND range_key = $2", connector.table),
			"writePK", "writeRK").Scan(&value)
		connector.connMu.RUnlock()

		require.NoError(t, err, "data should exist on primary")
		assert.Equal(t, []byte("write-value"), value)
	})

	t.Run("DeleteOperationsAlwaysGoToPrimary", func(t *testing.T) {
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

		// Write then delete
		err = connector.Set(ctx, "deletePK", "deleteRK", []byte("to-be-deleted"), nil)
		require.NoError(t, err)

		err = connector.Delete(ctx, "deletePK", "deleteRK")
		require.NoError(t, err, "Delete should succeed on primary")

		// Verify deleted on primary
		connector.connMu.RLock()
		var count int
		err = connector.conn.QueryRow(ctx, fmt.Sprintf(
			"SELECT COUNT(*) FROM %s WHERE partition_key = $1 AND range_key = $2", connector.table),
			"deletePK", "deleteRK").Scan(&count)
		connector.connMu.RUnlock()

		require.NoError(t, err)
		assert.Equal(t, 0, count, "data should be deleted from primary")
	})

	t.Run("LockOperationsAlwaysGoToPrimary", func(t *testing.T) {
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

		// Lock should work (uses primary's advisory locks)
		lock, err := connector.Lock(ctx, "test-lock-key", 5*time.Second)
		require.NoError(t, err, "Lock should succeed on primary")
		require.NotNil(t, lock)

		err = lock.Unlock(ctx)
		require.NoError(t, err, "Unlock should succeed")
	})

	t.Run("ListOperationsUseReplicas", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// For this test, we use the same database for primary and replica
		// to simulate data being available on both
		primaryC, primaryURI := startPostgresContainer(t, ctx, "shared_db")
		defer primaryC.Terminate(ctx)

		cfg := &common.PostgreSQLConnectorConfig{
			Table:           "test_list_replica_table",
			ConnectionUri:   primaryURI,
			ReadReplicaUris: []string{primaryURI}, // Same URI for testing (data available on both)
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

		// Insert test data
		for i := 0; i < 5; i++ {
			err = connector.Set(ctx, fmt.Sprintf("listPK%d", i), "listRK", []byte(fmt.Sprintf("value%d", i)), nil)
			require.NoError(t, err)
		}

		// List should work and use replica
		results, nextToken, err := connector.List(ctx, ConnectorMainIndex, 10, "")
		require.NoError(t, err, "List should succeed")
		assert.Len(t, results, 5, "should return 5 items")
		assert.Empty(t, nextToken, "should have no more pages")
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
			ReadReplicaUris: []string{primaryURI}, // Same URI for testing
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

		// Insert test data with pattern
		err = connector.Set(ctx, "wildcardPK", "pattern_001", []byte("value1"), nil)
		require.NoError(t, err)
		err = connector.Set(ctx, "wildcardPK", "pattern_002", []byte("value2"), nil)
		require.NoError(t, err)

		// Get with wildcard should work
		val, err := connector.Get(ctx, ConnectorMainIndex, "wildcardPK", "pattern_*", nil)
		require.NoError(t, err, "Get with wildcard should succeed")
		assert.NotEmpty(t, val, "should return a value")
	})
}

// =============================================================================
// Read Replica Fallback Tests
// =============================================================================

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

		// Give PostgreSQL a moment to fully initialize
		time.Sleep(500 * time.Millisecond)

		connURI := fmt.Sprintf("postgres://postgres:password@%s:%s/%s?sslmode=disable", host, port.Port(), dbName)
		return container, connURI
	}

	t.Run("FallsBackToPrimaryWhenReplicaStops", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start primary
		primaryC, primaryURI := startPostgresContainer(t, ctx, "primary_db")
		defer primaryC.Terminate(ctx)

		// Start replica
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

		// Insert data via primary
		err = connector.Set(ctx, "fallbackPK", "fallbackRK", []byte("fallback-value"), nil)
		require.NoError(t, err)

		// Stop the replica
		err = replicaC.Terminate(ctx)
		require.NoError(t, err, "should be able to stop replica")

		// Give it a moment to detect the connection is gone
		time.Sleep(1 * time.Second)

		// Read should still work (falls back to primary)
		// Note: This test verifies the fallback logic, though the exact behavior
		// depends on connection pool health checks
		val, err := connector.Get(ctx, "", "fallbackPK", "fallbackRK", nil)
		require.NoError(t, err, "Get should succeed via fallback to primary")
		assert.Equal(t, []byte("fallback-value"), val)
	})
}
