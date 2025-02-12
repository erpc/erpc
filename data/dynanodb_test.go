package data

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestDynamoDBConnectorInitialization(t *testing.T) {
	t.Run("succeeds immediately with valid config (real container)", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req := testcontainers.ContainerRequest{
			Image:        "amazon/dynamodb-local",
			ExposedPorts: []string{"8000/tcp"},
			WaitingFor:   wait.ForListeningPort("8000/tcp"),
		}
		ddbC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		require.NoError(t, err, "failed to start DynamoDB Local container")
		defer ddbC.Terminate(ctx)

		host, err := ddbC.Host(ctx)
		require.NoError(t, err)
		port, err := ddbC.MappedPort(ctx, "8000")
		require.NoError(t, err)

		endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

		cfg := &common.DynamoDBConnectorConfig{
			Endpoint:         endpoint,
			Region:           "us-west-2",
			Table:            "test_table",
			PartitionKeyName: "pk",
			RangeKeyName:     "rk",
			ReverseIndexName: "rk-pk-index",
			TTLAttributeName: "ttl",
			InitTimeout:      2 * time.Second,
			GetTimeout:       2 * time.Second,
			SetTimeout:       2 * time.Second,
			Auth: &common.AwsAuthConfig{
				Mode:            "secret",
				AccessKeyID:     "fakeKey",
				SecretAccessKey: "fakeSecret",
			},
		}

		// Creating the connector should succeed immediately
		connector, err := NewDynamoDBConnector(ctx, &logger, "test-dynamo-connector", cfg)
		require.NoError(t, err, "expected no error from NewDynamoDBConnector with a valid local container")

		if connector.initializer != nil {
			state := connector.initializer.State()
			require.Equal(t, common.StateReady, state, "connector should be in ready state")
		}

		// Try a simple SET/GET to confirm real operation
		err = connector.Set(ctx, "testPK", "testRK", "hello-dynamo", nil)
		require.NoError(t, err, "Set should succeed after successful initialization")

		val, err := connector.Get(ctx, "", "testPK", "testRK")
		require.NoError(t, err, "Get should succeed for existing key")
		require.Equal(t, "hello-dynamo", val)
	})

	t.Run("fails on first attempt with invalid endpoint, but returns connector anyway", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Intentionally invalid endpoint/port
		cfg := &common.DynamoDBConnectorConfig{
			Endpoint:         "http://127.0.0.1:9876", // no server listening
			Region:           "us-west-2",
			Table:            "test_table",
			PartitionKeyName: "pk",
			RangeKeyName:     "rk",
			ReverseIndexName: "rk-pk-index",
			TTLAttributeName: "ttl",
			InitTimeout:      500 * time.Millisecond,
			GetTimeout:       500 * time.Millisecond,
			SetTimeout:       500 * time.Millisecond,
			Auth: &common.AwsAuthConfig{
				Mode:            "secret",
				AccessKeyID:     "fakeKey",
				SecretAccessKey: "fakeSecret",
			},
		}

		// We expect the connector constructor to return with no fatal error,
		//    but the connector won't be ready.
		connector, err := NewDynamoDBConnector(ctx, &logger, "test-dynamo-invalid-endpoint", cfg)
		require.NoError(t, err, "Constructor does not necessarily return an error even if the first attempt fails")

		if connector.initializer != nil {
			require.NotEqual(t, common.StateReady, connector.initializer.State(),
				"connector should not be in READY state if it failed to connect")
		}

		// 4) Verify that calls to Set/Get fail because the DynamoDB client is not connected
		err = connector.Set(ctx, "testPK", "testRK", "value", nil)
		require.Error(t, err, "Set should fail because DynamoDB is not connected (invalid endpoint)")

		_, err = connector.Get(ctx, "", "testPK", "testRK")
		require.Error(t, err, "Get should fail because DynamoDB is not connected (invalid endpoint)")
	})
}
