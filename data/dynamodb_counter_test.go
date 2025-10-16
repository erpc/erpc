package data

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestDynamoDBCounterInt64 tests the counter functionality end-to-end
func TestDynamoDBCounterInt64(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start DynamoDB local container
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
		Endpoint:          endpoint,
		Region:            "us-west-2",
		Table:             "test_counters",
		PartitionKeyName:  "pk",
		RangeKeyName:      "rk",
		TTLAttributeName:  "ttl",
		ReverseIndexName:  "rk-pk-index",
		InitTimeout:       common.Duration(5 * time.Second),
		GetTimeout:        common.Duration(5 * time.Second),
		SetTimeout:        common.Duration(5 * time.Second),
		StatePollInterval: common.Duration(100 * time.Millisecond),
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}

	connector, err := NewDynamoDBConnector(ctx, &log.Logger, "test-counters", cfg)
	require.NoError(t, err, "failed to create DynamoDB connector")

	// Wait for connector to be ready
	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 10*time.Second, 100*time.Millisecond, "connector should be in ready state")

	t.Run("PublishAndRetrieveCounterValue", func(t *testing.T) {
		counterKey := "test-cluster/latestBlock/upstream-1/hash-abc123"

		// Publish a counter value
		err := connector.PublishCounterInt64(ctx, counterKey, 12345)
		require.NoError(t, err, "PublishCounterInt64 should succeed")

		// Retrieve the value using GetSimpleValue
		value, err := connector.GetSimpleValue(ctx, counterKey)
		require.NoError(t, err, "getSimpleValue should succeed")
		assert.Equal(t, int64(12345), value, "retrieved value should match published value")
	})

	t.Run("GetSimpleValueReturnsZeroForNonExistentCounter", func(t *testing.T) {
		counterKey := "test-cluster/nonexistent/counter/key"

		// Try to get a counter that doesn't exist
		value, err := connector.GetSimpleValue(ctx, counterKey)
		require.NoError(t, err, "getSimpleValue should not error for non-existent counter")
		assert.Equal(t, int64(0), value, "non-existent counter should return 0")
	})

	t.Run("PublishOnlyUpdatesIfHigher", func(t *testing.T) {
		counterKey := "test-cluster/latestBlock/upstream-2/hash-def456"

		// Publish initial value
		err := connector.PublishCounterInt64(ctx, counterKey, 100)
		require.NoError(t, err, "initial PublishCounterInt64 should succeed")

		// Try to publish a lower value (should be ignored)
		err = connector.PublishCounterInt64(ctx, counterKey, 50)
		require.NoError(t, err, "PublishCounterInt64 with lower value should not error")

		// Verify value is still 100
		value, err := connector.GetSimpleValue(ctx, counterKey)
		require.NoError(t, err, "getSimpleValue should succeed")
		assert.Equal(t, int64(100), value, "counter should not decrease")

		// Publish a higher value
		err = connector.PublishCounterInt64(ctx, counterKey, 200)
		require.NoError(t, err, "PublishCounterInt64 with higher value should succeed")

		// Verify value is now 200
		value, err = connector.GetSimpleValue(ctx, counterKey)
		require.NoError(t, err, "getSimpleValue should succeed")
		assert.Equal(t, int64(200), value, "counter should increase to new value")
	})

	t.Run("WatchCounterInt64ReceivesUpdates", func(t *testing.T) {
		counterKey := "test-cluster/latestBlock/upstream-3/hash-ghi789"

		// Start watching the counter
		updates, cleanup, err := connector.WatchCounterInt64(ctx, counterKey)
		require.NoError(t, err, "WatchCounterInt64 should succeed")
		defer cleanup()

		// Initial value should be 0 (counter doesn't exist yet)
		select {
		case val := <-updates:
			assert.Equal(t, int64(0), val, "initial value should be 0")
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for initial value")
		}

		// Publish a value
		err = connector.PublishCounterInt64(ctx, counterKey, 5000)
		require.NoError(t, err, "PublishCounterInt64 should succeed")

		// Should receive the updated value within the poll interval
		select {
		case val := <-updates:
			assert.Equal(t, int64(5000), val, "should receive published value")
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for published value update")
		}

		// Publish another higher value
		err = connector.PublishCounterInt64(ctx, counterKey, 10000)
		require.NoError(t, err, "PublishCounterInt64 should succeed")

		// Should receive the second update
		select {
		case val := <-updates:
			assert.Equal(t, int64(10000), val, "should receive second published value")
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for second value update")
		}
	})

	t.Run("GetSimpleValueHandlesNumberAndStringTypes", func(t *testing.T) {
		// Test with Number type (standard format from PublishCounterInt64)
		counterKeyNumber := "test-cluster/counter/number-type"
		err := connector.PublishCounterInt64(ctx, counterKeyNumber, 777)
		require.NoError(t, err, "PublishCounterInt64 should succeed")

		value, err := connector.GetSimpleValue(ctx, counterKeyNumber)
		require.NoError(t, err, "getSimpleValue should handle Number type")
		assert.Equal(t, int64(777), value, "should parse Number type correctly")

		// Test with String type (backward compatibility)
		counterKeyString := "test-cluster/counter/string-type"
		_, err = connector.writeClient.PutItem(&dynamodb.PutItemInput{
			TableName: aws.String(connector.table),
			Item: map[string]*dynamodb.AttributeValue{
				connector.partitionKeyName: {S: aws.String(counterKeyString)},
				connector.rangeKeyName:     {S: aws.String("value")},
				"value":                    {S: aws.String("888")}, // String type
			},
		})
		require.NoError(t, err, "direct PutItem should succeed")

		value, err = connector.GetSimpleValue(ctx, counterKeyString)
		require.NoError(t, err, "getSimpleValue should handle String type")
		assert.Equal(t, int64(888), value, "should parse String type correctly")
	})

	t.Run("GetSimpleValueHandlesInvalidFormats", func(t *testing.T) {
		// Test with neither N nor S field
		counterKeyInvalid := "test-cluster/counter/invalid-type"
		_, err := connector.writeClient.PutItem(&dynamodb.PutItemInput{
			TableName: aws.String(connector.table),
			Item: map[string]*dynamodb.AttributeValue{
				connector.partitionKeyName: {S: aws.String(counterKeyInvalid)},
				connector.rangeKeyName:     {S: aws.String("value")},
				"value":                    {BOOL: aws.Bool(true)}, // Invalid type
			},
		})
		require.NoError(t, err, "direct PutItem should succeed")

		_, err = connector.GetSimpleValue(ctx, counterKeyInvalid)
		require.Error(t, err, "getSimpleValue should error on invalid attribute type")
		assert.Contains(t, err.Error(), "neither Number (N) nor String (S)", "error should mention missing N/S fields")
	})

	t.Run("GetSimpleValueHandlesNilAttribute", func(t *testing.T) {
		// Test with missing value attribute
		counterKeyMissing := "test-cluster/counter/missing-value"
		_, err := connector.writeClient.PutItem(&dynamodb.PutItemInput{
			TableName: aws.String(connector.table),
			Item: map[string]*dynamodb.AttributeValue{
				connector.partitionKeyName: {S: aws.String(counterKeyMissing)},
				connector.rangeKeyName:     {S: aws.String("value")},
				// No "value" attribute
			},
		})
		require.NoError(t, err, "direct PutItem should succeed")

		value, err := connector.GetSimpleValue(ctx, counterKeyMissing)
		require.NoError(t, err, "getSimpleValue should not error on missing value attribute")
		assert.Equal(t, int64(0), value, "missing value attribute should return 0")
	})

	t.Run("MultipleConcurrentPublishes", func(t *testing.T) {
		counterKey := "test-cluster/latestBlock/concurrent/test"

		// Publish multiple values concurrently
		done := make(chan bool, 10)
		for i := 0; i < 10; i++ {
			go func(val int64) {
				err := connector.PublishCounterInt64(ctx, counterKey, val)
				assert.NoError(t, err, "concurrent PublishCounterInt64 should succeed")
				done <- true
			}(int64(i * 1000))
		}

		// Wait for all goroutines to complete
		for i := 0; i < 10; i++ {
			<-done
		}

		// Final value should be the maximum (9000)
		value, err := connector.GetSimpleValue(ctx, counterKey)
		require.NoError(t, err, "getSimpleValue should succeed")
		assert.Equal(t, int64(9000), value, "counter should have highest value")
	})
}
