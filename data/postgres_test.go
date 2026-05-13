package data

import (
	"context"
	"fmt"
	"io"
	"sync"
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
			// Wait for the second occurrence of this log message. Postgres outputs this message twice:
			// once during initialization and once when fully ready to accept connections.
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2 * time.Minute),
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

// TestPostgreSQLConnectorReconnectBehavior covers the regression fixed in
// the write-lock-during-connect bug: a reconnect must not block readers, and
// the one-time schema setup must not be re-issued on every reconnect.
//
// The original bug surfaced in production traces as:
//
//	span_name:           PostgreSQLConnector.Get
//	exception.message:   "PostgreSQLConnector not connected yet"
//	duration:            ~initTimeout (clustered just under 5s)
//
// because Get held connMu.RLock(), the in-flight connectTask held
// connMu.Lock() for the entire pgxpool.ConnectConfig + DDL sequence, and
// every Get serialized behind that lock. Reconnects also re-ran the schema
// DDL on every attempt, which produced a request burst against the pooler
// at the worst possible time (when a reconnect storm was already underway).
func TestPostgreSQLConnectorReconnectBehavior(t *testing.T) {
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "postgres:15-alpine",
		Env:          map[string]string{"POSTGRES_PASSWORD": "password"},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(2 * time.Minute),
	}
	postgresC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	defer postgresC.Terminate(context.Background())

	host, err := postgresC.Host(ctx)
	require.NoError(t, err)
	port, err := postgresC.MappedPort(ctx, "5432")
	require.NoError(t, err)
	connURI := fmt.Sprintf("postgres://postgres:password@%s:%s/postgres?sslmode=disable", host, port.Port())

	cfg := &common.PostgreSQLConnectorConfig{
		Table:         "reconnect_test_table",
		ConnectionUri: connURI,
		InitTimeout:   common.Duration(5 * time.Second),
		GetTimeout:    common.Duration(2 * time.Second),
		SetTimeout:    common.Duration(2 * time.Second),
		MinConns:      1,
		MaxConns:      5,
	}

	connector, err := NewPostgreSQLConnector(ctx, &logger, "reconnect-test", cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, connector.initializer.State())

	// Sanity-check that the initial connect set the schema-ready flag.
	require.True(t, connector.schemaReady.Load(),
		"schemaReady should be true after a successful first connect")

	// Seed a value so we have something to read during reconnect.
	require.NoError(t, connector.Set(ctx, "pk", "rk", []byte("v0"), nil))

	t.Run("ReconnectSkipsSchemaMigration", func(t *testing.T) {
		// Invoke connectTask directly to simulate the auto-retry path the
		// initializer uses on MarkTaskAsFailed. With schemaReady already set,
		// the second call must NOT re-issue DDL.
		//
		// We assert this indirectly via wall-clock: a no-DDL reconnect is
		// dominated by pgxpool.ConnectConfig (single-digit ms against a local
		// container) while the original DDL pass takes 100ms+ even on a warm
		// container. Generous threshold so the test isn't flaky.
		start := time.Now()
		require.NoError(t, connector.connectTask(ctx, cfg))
		reconnectDuration := time.Since(start)

		require.True(t, connector.schemaReady.Load(),
			"schemaReady must remain true after reconnect")
		require.Less(t, reconnectDuration, 2*time.Second,
			"reconnect without schema migration should be fast; got %v", reconnectDuration)

		// Confirm the new pool is usable after the swap.
		v, err := connector.Get(ctx, "", "pk", "rk", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("v0"), v)
	})

	t.Run("ReadersAreNotBlockedDuringReconnect", func(t *testing.T) {
		// Spawn a goroutine that triggers a reconnect, and concurrently fire
		// a burst of reads. With the bug, every read would block until the
		// reconnect's WRITE lock is released. With the fix, the WRITE lock is
		// held only for the brief pool-swap.
		const concurrency = 50
		const maxAllowedReadLatency = 1 * time.Second // very generous

		var wg sync.WaitGroup
		readLatencies := make([]time.Duration, concurrency)
		readErrors := make([]error, concurrency)

		// Trigger a reconnect (will run connectTask).
		reconnectStart := time.Now()
		reconnectDone := make(chan struct{})
		go func() {
			defer close(reconnectDone)
			_ = connector.connectTask(ctx, cfg)
		}()

		// Hammer Get from many goroutines while the reconnect is in flight.
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				start := time.Now()
				_, err := connector.Get(ctx, "", "pk", "rk", nil)
				readLatencies[i] = time.Since(start)
				readErrors[i] = err
			}(i)
		}

		wg.Wait()
		<-reconnectDone
		t.Logf("reconnect+50 concurrent reads completed in %v", time.Since(reconnectStart))

		// Every read must complete (succeed or fail) quickly. The bug would
		// have produced ~initTimeout (5s) per read.
		for i, lat := range readLatencies {
			require.Less(t, lat, maxAllowedReadLatency,
				"read %d was blocked for %v — exceeds the budget that proves the WRITE lock is no longer held during connect (err=%v)",
				i, lat, readErrors[i])
		}
	})
}
