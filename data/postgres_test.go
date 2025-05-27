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

		val, err := connector.Get(ctx, "", "testPK", "testRK")
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

		_, err = connector.Get(ctx, "", "testPK", "testRK")
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
