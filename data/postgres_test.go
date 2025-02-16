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
			InitTimeout:   2 * time.Second,
			GetTimeout:    2 * time.Second,
			SetTimeout:    2 * time.Second,
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
		err = connector.Set(ctx, "testPK", "testRK", "hello-world", nil)
		require.NoError(t, err, "Set should succeed after successful initialization")

		val, err := connector.Get(ctx, "", "testPK", "testRK")
		require.NoError(t, err, "Get should succeed for existing key")
		require.Equal(t, "hello-world", val)
	})

	t.Run("FailsOnFirstAttemptWithInvalidAddressButReturnsConnectorAnyway", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Intentionally invalid address
		cfg := &common.PostgreSQLConnectorConfig{
			Table:         "test_table",
			ConnectionUri: "postgres://user:pass@127.0.0.1:9876/bogusdb?sslmode=disable", // no server
			InitTimeout:   500 * time.Millisecond,
			GetTimeout:    500 * time.Millisecond,
			SetTimeout:    500 * time.Millisecond,
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
		err = connector.Set(ctx, "testPK", "testRK", "value", nil)
		require.Error(t, err, "should fail because Postgres is not connected")

		_, err = connector.Get(ctx, "", "testPK", "testRK")
		require.Error(t, err, "should fail because Postgres is not connected")
	})
}
