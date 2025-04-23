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
	"github.com/rs/zerolog/log"
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
			InitTimeout:      common.Duration(2 * time.Second),
			GetTimeout:       common.Duration(2 * time.Second),
			SetTimeout:       common.Duration(2 * time.Second),
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
			require.Equal(t, util.StateReady, state, "connector should be in ready state")
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
			InitTimeout:      common.Duration(500 * time.Millisecond),
			GetTimeout:       common.Duration(500 * time.Millisecond),
			SetTimeout:       common.Duration(500 * time.Millisecond),
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
			require.NotEqual(t, util.StateReady, connector.initializer.State(),
				"connector should not be in READY state if it failed to connect")
		}

		// 4) Verify that calls to Set/Get fail because the DynamoDB client is not connected
		err = connector.Set(ctx, "testPK", "testRK", "value", nil)
		require.Error(t, err, "Set should fail because DynamoDB is not connected (invalid endpoint)")

		_, err = connector.Get(ctx, "", "testPK", "testRK")
		require.Error(t, err, "Get should fail because DynamoDB is not connected (invalid endpoint)")
	})
}

func TestDynamoDBConnectorReverseIndex(t *testing.T) {
	logger := log.Logger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a DynamoDB local container
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

	// Create a DynamoDB connector with a table and reverse index
	cfg := &common.DynamoDBConnectorConfig{
		Endpoint:         endpoint,
		Region:           "us-west-2",
		Table:            "test_reverse_index",
		PartitionKeyName: "pk",
		RangeKeyName:     "rk",
		ReverseIndexName: "rk-pk-index",
		TTLAttributeName: "ttl",
		InitTimeout:      common.Duration(2 * time.Second),
		GetTimeout:       common.Duration(2 * time.Second),
		SetTimeout:       common.Duration(2 * time.Second),
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}

	connector, err := NewDynamoDBConnector(ctx, &logger, "test-reverse-index", cfg)
	require.NoError(t, err, "failed to create DynamoDB connector")

	// Wait for connector to be fully initialized
	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 5*time.Second, 100*time.Millisecond, "connector should be in ready state")

	// Insert test data
	testData := []struct {
		pk    string
		rk    string
		value string
	}{
		{"user:1", "profile", "user1-profile"},
		{"user:2", "profile", "user2-profile"},
		{"user:1", "settings", "user1-settings"},
		{"user:2", "settings", "user2-settings"},
		{"event:1", "data", "event1-data"},
		{"event:2", "data", "event2-data"},
	}

	for _, data := range testData {
		err = connector.Set(ctx, data.pk, data.rk, data.value, nil)
		require.NoError(t, err, "failed to insert test data")
	}

	// Test cases that use the reverse index (which was fixed)
	testCases := []struct {
		name          string
		rangeKey      string // This is the partition key in the reverse index
		partitionKey  string // This is the sort key in the reverse index
		expectedValue string
		expectedError bool
		errorContains string
	}{
		{
			name:          "exact match on both keys",
			rangeKey:      "profile",
			partitionKey:  "user:1",
			expectedValue: "user1-profile",
		},
		{
			name:          "match on range key with prefix on partition key",
			rangeKey:      "profile",
			partitionKey:  "user:*",
			expectedValue: "user1-profile", // It should return the first matching item
		},
		{
			name:          "exact match on range key only",
			rangeKey:      "settings",
			partitionKey:  "",
			expectedValue: "user1-settings", // It should return the first matching item
		},
		{
			name:          "non-existent range key",
			rangeKey:      "nonexistent",
			partitionKey:  "",
			expectedError: true,
			errorContains: "record not found",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			value, err := connector.Get(ctx, ConnectorReverseIndex, tc.partitionKey, tc.rangeKey)

			if tc.expectedError {
				require.Error(t, err, "expected error but got none")
				if tc.errorContains != "" {
					require.Contains(t, err.Error(), tc.errorContains, "error does not contain expected text")
				}
			} else {
				require.NoError(t, err, "unexpected error")
				require.Equal(t, tc.expectedValue, value, "unexpected result value")
			}
		})
	}
}
