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
		require.NoError(t, err)

		if connector.initializer != nil {
			state := connector.initializer.State()
			require.Equal(t, util.StateReady, state, "connector should be in ready state")
		}

		err = connector.Set(ctx, "testPK", "testRK", []byte("hello-world"), nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, "", "testPK", "testRK", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("hello-world"), val)
	})

	t.Run("FailsOnFirstAttemptWithInvalidAddressButReturnsConnectorAnyway", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.PostgreSQLConnectorConfig{
			Table:         "test_table",
			ConnectionUri: "postgres://user:pass@127.0.0.1:9876/bogusdb?sslmode=disable",
			InitTimeout:   common.Duration(500 * time.Millisecond),
			GetTimeout:    common.Duration(500 * time.Millisecond),
			SetTimeout:    common.Duration(500 * time.Millisecond),
			MinConns:      1,
			MaxConns:      5,
		}

		connector, err := NewPostgreSQLConnector(ctx, &logger, "test-connector-invalid-addr", cfg)
		require.NoError(t, err)

		if connector.initializer != nil {
			require.NotEqual(t, util.StateReady, connector.initializer.State())
		}

		err = connector.Set(ctx, "testPK", "testRK", []byte("value"), nil)
		require.Error(t, err)

		_, err = connector.Get(ctx, "", "testPK", "testRK", nil)
		require.Error(t, err)
	})
}

func TestPostgreSQLDistributedLocking(t *testing.T) {
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
				return false
			}
			return connector.initializer.State() == util.StateReady
		}, 30*time.Second, 500*time.Millisecond, "connector did not become ready")

		cleanup := func() {
			cancel()
			postgresC.Terminate(context.Background())
		}

		return ctx, connector, cleanup
	}

	t.Run("SuccessfulImmediateLockAcquisition", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()

		lock, err := connector.Lock(ctx, "pg-lock-immediate", 5*time.Second)
		require.NoError(t, err)
		require.NotNil(t, lock)
		require.False(t, lock.IsNil())

		pgLock, ok := lock.(*postgresLock)
		require.True(t, ok)
		require.NotNil(t, pgLock.tx)

		err = lock.Unlock(ctx)
		require.NoError(t, err)
		require.Nil(t, pgLock.tx)
	})

	t.Run("LockFailsIfAlreadyHeld", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()

		lock1, err := connector.Lock(ctx, "pg-lock-already-held", 5*time.Second)
		require.NoError(t, err)
		require.NotNil(t, lock1)

		lock2, err := connector.Lock(ctx, "pg-lock-already-held", 5*time.Second)
		require.Error(t, err)
		require.Nil(t, lock2)
		require.Contains(t, err.Error(), "already locked")

		err = lock1.Unlock(ctx)
		require.NoError(t, err)
	})

	t.Run("LockSucceedsAfterBeingReleased", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()

		lock1, err := connector.Lock(ctx, "pg-lock-release-then-acquire", 5*time.Second)
		require.NoError(t, err)
		require.NotNil(t, lock1)
		err = lock1.Unlock(ctx)
		require.NoError(t, err)

		lock2, err := connector.Lock(ctx, "pg-lock-release-then-acquire", 5*time.Second)
		require.NoError(t, err)
		require.NotNil(t, lock2)
		err = lock2.Unlock(ctx)
		require.NoError(t, err)
	})

	t.Run("LockIsReleasedOnCommit", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()

		lock1, err := connector.Lock(ctx, "pg-lock-commit-release", 5*time.Second)
		require.NoError(t, err)
		pgLock1, ok := lock1.(*postgresLock)
		require.True(t, ok)

		_, err = connector.Lock(ctx, "pg-lock-commit-release", 1*time.Second)
		require.Error(t, err)

		err = pgLock1.Unlock(ctx)
		require.NoError(t, err)

		lock2, err := connector.Lock(ctx, "pg-lock-commit-release", 5*time.Second)
		require.NoError(t, err)
		require.NotNil(t, lock2)
		err = lock2.Unlock(ctx)
		require.NoError(t, err)
	})

	t.Run("LockIsReleasedOnRollback", func(t *testing.T) {
		ctx, connector, cleanup := setupConnector(t)
		defer cleanup()

		lock1, err := connector.Lock(ctx, "pg-lock-rollback-release", 5*time.Second)
		require.NoError(t, err)
		pgLock1, ok := lock1.(*postgresLock)
		require.True(t, ok)

		if pgLock1.tx != nil {
			err = pgLock1.tx.Rollback(ctx)
			require.NoError(t, err)
			pgLock1.tx = nil
		}

		lock2, err := connector.Lock(ctx, "pg-lock-rollback-release", 5*time.Second)
		require.NoError(t, err)
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
