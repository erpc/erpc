package data

import (
	"context"
	"fmt"
	"strings"
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

func init() {
	util.ConfigureTestLogger()
}

func TestDynamoDBConnectorInitialization(t *testing.T) {
	t.Run("succeeds immediately with valid config (real container)", func(t *testing.T) {
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
		connector, err := NewDynamoDBConnector(ctx, &log.Logger, "test-dynamo-connector", cfg)
		require.NoError(t, err, "expected no error from NewDynamoDBConnector with a valid local container")

		// Wait for connector to be fully initialized
		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 10*time.Second, 100*time.Millisecond, "connector should be in ready state")

		// Try a simple SET/GET to confirm real operation
		err = connector.Set(ctx, "testPK", "testRK", []byte("hello-dynamo"), nil)
		require.NoError(t, err, "Set should succeed after successful initialization")

		val, err := connector.Get(ctx, "", "testPK", "testRK")
		require.NoError(t, err, "Get should succeed for existing key")
		require.Equal(t, []byte("hello-dynamo"), val)
	})

	t.Run("fails on first attempt with invalid endpoint, but returns connector anyway", func(t *testing.T) {
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
		connector, err := NewDynamoDBConnector(ctx, &log.Logger, "test-dynamo-invalid-endpoint", cfg)
		require.NoError(t, err, "Constructor does not necessarily return an error even if the first attempt fails")

		if connector.initializer != nil {
			require.NotEqual(t, util.StateReady, connector.initializer.State(),
				"connector should not be in READY state if it failed to connect")
		}

		// 4) Verify that calls to Set/Get fail because the DynamoDB client is not connected
		err = connector.Set(ctx, "testPK", "testRK", []byte("value"), nil)
		require.Error(t, err, "Set should fail because DynamoDB is not connected (invalid endpoint)")

		_, err = connector.Get(ctx, "", "testPK", "testRK")
		require.Error(t, err, "Get should fail because DynamoDB is not connected (invalid endpoint)")
	})
}

func TestDynamoDBConnectorReverseIndex(t *testing.T) {
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

	connector, err := NewDynamoDBConnector(ctx, &log.Logger, "test-reverse-index", cfg)
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
		err = connector.Set(ctx, data.pk, data.rk, []byte(data.value), nil)
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
				require.Equal(t, []byte(tc.expectedValue), value, "unexpected result value")
			}
		})
	}
}

func TestDynamoDBDistributedLocking(t *testing.T) {
	// Set up DynamoDB local container for testing
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

	// Create a DynamoDB connector for testing
	cfg := &common.DynamoDBConnectorConfig{
		Endpoint:          endpoint,
		Region:            "us-west-2",
		Table:             "test_locks",
		PartitionKeyName:  "pk",
		RangeKeyName:      "rk",
		TTLAttributeName:  "ttl",
		ReverseIndexName:  "rk-pk-index",
		InitTimeout:       common.Duration(5 * time.Second),
		GetTimeout:        common.Duration(5 * time.Second),
		SetTimeout:        common.Duration(5 * time.Second),
		LockRetryInterval: common.Duration(50 * time.Millisecond), // Use shorter interval for faster tests
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}

	connector, err := NewDynamoDBConnector(ctx, &log.Logger, "test-locks", cfg)
	require.NoError(t, err, "failed to create DynamoDB connector")

	// Ensure connector is ready
	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 10*time.Second, 100*time.Millisecond, "connector should be in ready state")

	t.Run("SuccessfulImmediateLockAcquisition", func(t *testing.T) {
		lockKey := "test-lock-1"
		lockTTL := 5 * time.Second

		// Lock should be acquired immediately
		lock, err := connector.Lock(ctx, lockKey, lockTTL)
		require.NoError(t, err, "should acquire lock without waiting")
		require.NotNil(t, lock, "lock should not be nil")

		// Clean up by unlocking
		err = lock.Unlock(ctx)
		require.NoError(t, err, "unlock should succeed")
	})

	t.Run("LockAcquisitionAfterWaitingHeldByAnotherProcess", func(t *testing.T) {
		lockKey := "test-lock-2"
		// We'll create a lock that will expire in 200ms
		lockExpiry := 200 * time.Millisecond

		// First create a lock record directly (simulate another process holding the lock)
		formattedLockKey := fmt.Sprintf("%s:lock", lockKey)
		lockExpiryTime := time.Now().Add(lockExpiry).Unix()

		_, err := connector.writeClient.PutItem(&dynamodb.PutItemInput{
			TableName: aws.String(connector.table),
			Item: map[string]*dynamodb.AttributeValue{
				connector.partitionKeyName: {S: aws.String(formattedLockKey)},
				connector.rangeKeyName:     {S: aws.String("lock")},
				"expiry":                   {N: aws.String(fmt.Sprintf("%d", lockExpiryTime))},
			},
		})
		require.NoError(t, err, "should create mock lock record")

		// Try to acquire the lock with a longer timeout
		// It should succeed after waiting for the existing lock to expire
		start := time.Now()
		lock, err := connector.Lock(ctx, lockKey, 5*time.Second)
		timeToAcquire := time.Since(start)

		require.NoError(t, err, "should eventually acquire lock")
		require.NotNil(t, lock, "lock should not be nil")
		assert.GreaterOrEqual(t, timeToAcquire.Milliseconds(), int64(150), "should have waited for existing lock to expire")

		// Clean up
		err = lock.Unlock(ctx)
		require.NoError(t, err, "unlock should succeed")
	})

	t.Run("LockAcquisitionTimesOut", func(t *testing.T) {
		lockKey := "test-lock-3"

		// First create a lock record with a long expiry
		formattedLockKey := fmt.Sprintf("%s:lock", lockKey)
		lockExpiryTime := time.Now().Add(100 * time.Second).Unix() // Won't expire during our test

		_, err := connector.writeClient.PutItem(&dynamodb.PutItemInput{
			TableName: aws.String(connector.table),
			Item: map[string]*dynamodb.AttributeValue{
				connector.partitionKeyName: {S: aws.String(formattedLockKey)},
				connector.rangeKeyName:     {S: aws.String("lock")},
				"expiry":                   {N: aws.String(fmt.Sprintf("%d", lockExpiryTime))},
			},
		})
		require.NoError(t, err, "should create mock lock record")

		// Try to acquire with a short timeout - should fail
		start := time.Now()
		lctx, lcancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer lcancel()
		lock, err := connector.Lock(lctx, lockKey, 100*time.Second)
		timeSpent := time.Since(start)

		require.Error(t, err, "lock acquisition should time out")
		require.Nil(t, lock, "lock should be nil")
		if !strings.Contains(err.Error(), "lock acquisition timed out") && !strings.Contains(err.Error(), "request context canceled") {
			t.Errorf("expected error to contain 'lock acquisition timed out' or 'request context canceled', got: %s", err.Error())
		}
		assert.InDelta(t, timeSpent.Milliseconds(), int64(300), 100, "should have waited for the full timeout")
	})

	t.Run("ContextCancellationDuringLockAcquisition", func(t *testing.T) {
		lockKey := "test-lock-4"

		// First create a lock record with a medium expiry
		formattedLockKey := fmt.Sprintf("%s:lock", lockKey)
		lockExpiryTime := time.Now().Add(5 * time.Second).Unix()

		_, err := connector.writeClient.PutItem(&dynamodb.PutItemInput{
			TableName: aws.String(connector.table),
			Item: map[string]*dynamodb.AttributeValue{
				connector.partitionKeyName: {S: aws.String(formattedLockKey)},
				connector.rangeKeyName:     {S: aws.String("lock")},
				"expiry":                   {N: aws.String(fmt.Sprintf("%d", lockExpiryTime))},
			},
		})
		require.NoError(t, err, "should create mock lock record")

		// Create context that will be cancelled during acquisition
		cancelCtx, cancelFn := context.WithCancel(ctx)

		// Start a goroutine to cancel the context after a short delay
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancelFn()
		}()

		// Try to lock with the cancellable context
		start := time.Now()
		lock, err := connector.Lock(cancelCtx, lockKey, 5*time.Second)
		timeSpent := time.Since(start)

		require.Error(t, err, "lock acquisition should be cancelled")
		require.Nil(t, lock, "lock should be nil")
		if !strings.Contains(err.Error(), "acquisition timed out") && !strings.Contains(err.Error(), "request context canceled") {
			t.Errorf("expected error to contain 'lock acquisition timed out' or 'request context canceled', got: %s", err.Error())
		}
		assert.GreaterOrEqual(t, timeSpent.Milliseconds(), int64(150), "should have waited some time before cancellation")
		assert.LessOrEqual(t, timeSpent.Milliseconds(), int64(400), "should not wait much longer after cancellation")
	})

	t.Run("LockAcquisitionWithExpiredLockInTable", func(t *testing.T) {
		lockKey := "test-lock-5"

		// Create an already-expired lock record
		formattedLockKey := fmt.Sprintf("%s:lock", lockKey)
		expiredLockTime := time.Now().Add(-60 * time.Second).Unix() // Expired 1 minute ago

		_, err := connector.writeClient.PutItem(&dynamodb.PutItemInput{
			TableName: aws.String(connector.table),
			Item: map[string]*dynamodb.AttributeValue{
				connector.partitionKeyName: {S: aws.String(formattedLockKey)},
				connector.rangeKeyName:     {S: aws.String("lock")},
				"expiry":                   {N: aws.String(fmt.Sprintf("%d", expiredLockTime))},
			},
		})
		require.NoError(t, err, "should create expired lock record")

		// Try to acquire the lock - should succeed immediately despite record existing
		start := time.Now()
		lock, err := connector.Lock(ctx, lockKey, 5*time.Second)
		timeSpent := time.Since(start)

		require.NoError(t, err, "should acquire lock immediately")
		require.NotNil(t, lock, "lock should not be nil")
		assert.LessOrEqual(t, timeSpent.Milliseconds(), int64(100), "should acquire expired lock quickly")

		// Clean up
		err = lock.Unlock(ctx)
		require.NoError(t, err, "unlock should succeed")
	})

	t.Run("MultipleLockAttemptsWithConcurrentUnlock", func(t *testing.T) {
		lockKey := "test-lock-6"

		// First acquire a lock with this process
		lock1, err := connector.Lock(ctx, lockKey, 10*time.Second)
		require.NoError(t, err, "should acquire first lock")
		require.NotNil(t, lock1, "lock1 should not be nil")

		// Start a goroutine that will unlock after a delay
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = lock1.Unlock(ctx)
		}()

		// Try to acquire the same lock - should succeed after the first one is unlocked
		start := time.Now()
		lock2, err := connector.Lock(ctx, lockKey, 5*time.Second)
		timeSpent := time.Since(start)

		require.NoError(t, err, "should acquire second lock after waiting")
		require.NotNil(t, lock2, "lock2 should not be nil")
		assert.InDelta(t, timeSpent.Milliseconds(), int64(300), 100, "should have waited for unlock")

		// Clean up
		err = lock2.Unlock(ctx)
		require.NoError(t, err, "unlock should succeed")
	})
}
