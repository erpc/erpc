package data

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func init() {
	util.ConfigureTestLogger()
}

// createMinIOContainer starts a MinIO container and creates the test bucket.
func createMinIOContainer(t *testing.T, ctx context.Context, bucketName string) (string, func()) {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:        "minio/minio",
		ExposedPorts: []string{"9000/tcp"},
		Cmd:          []string{"server", "/data"},
		Env: map[string]string{
			"MINIO_ROOT_USER":     "minioadmin",
			"MINIO_ROOT_PASSWORD": "minioadmin",
		},
		WaitingFor: wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp"),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "failed to start MinIO container")

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "9000")
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	// Create the test bucket
	sess, err := session.NewSession(&aws.Config{
		Endpoint:         aws.String(endpoint),
		Region:           aws.String("us-east-1"),
		Credentials:      credentials.NewStaticCredentials("minioadmin", "minioadmin", ""),
		S3ForcePathStyle: aws.Bool(true),
	})
	require.NoError(t, err)

	s3Client := s3.New(sess)
	_, err = s3Client.CreateBucketWithContext(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	require.NoError(t, err, "failed to create test bucket")

	cleanup := func() {
		container.Terminate(ctx)
	}

	return endpoint, cleanup
}

func createTestS3Connector(t *testing.T, ctx context.Context, endpoint, bucketName string) *S3Connector {
	t.Helper()

	cfg := &common.S3ConnectorConfig{
		Endpoint:    endpoint,
		Region:      "us-east-1",
		Bucket:      bucketName,
		InitTimeout: common.Duration(5 * time.Second),
		GetTimeout:  common.Duration(5 * time.Second),
		SetTimeout:  common.Duration(5 * time.Second),
		MaxRetries:  3,
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
		},
	}

	connector, err := NewS3Connector(ctx, &log.Logger, "test-s3-connector", cfg)
	require.NoError(t, err, "failed to create S3 connector")

	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 10*time.Second, 100*time.Millisecond, "connector should be in ready state")

	return connector
}

func TestS3ConnectorInitialization(t *testing.T) {
	t.Run("succeeds with valid config", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		endpoint, cleanup := createMinIOContainer(t, ctx, "test-init-bucket")
		defer cleanup()

		connector := createTestS3Connector(t, ctx, endpoint, "test-init-bucket")
		require.NotNil(t, connector)
		require.Equal(t, "test-s3-connector", connector.Id())
	})

	t.Run("fails on first attempt with invalid endpoint but returns connector", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.S3ConnectorConfig{
			Endpoint:    "http://127.0.0.1:19876",
			Region:      "us-east-1",
			Bucket:      "nonexistent",
			InitTimeout: common.Duration(500 * time.Millisecond),
			GetTimeout:  common.Duration(500 * time.Millisecond),
			SetTimeout:  common.Duration(500 * time.Millisecond),
			MaxRetries:  0,
			Auth: &common.AwsAuthConfig{
				Mode:            "secret",
				AccessKeyID:     "fake",
				SecretAccessKey: "fake",
			},
		}

		connector, err := NewS3Connector(ctx, &log.Logger, "test-s3-invalid", cfg)
		require.NoError(t, err, "constructor should not return error even if first attempt fails")

		if connector.initializer != nil {
			require.NotEqual(t, util.StateReady, connector.initializer.State())
		}

		err = connector.Set(ctx, "pk", "rk", []byte("value"), nil)
		require.Error(t, err, "Set should fail because S3 is not connected")

		_, err = connector.Get(ctx, "", "pk", "rk", nil)
		require.Error(t, err, "Get should fail because S3 is not connected")
	})
}

func TestS3ConnectorSetGet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-setget-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-setget-bucket")

	t.Run("set and get round trip", func(t *testing.T) {
		err := connector.Set(ctx, "ethereum:0x123", "eth_call:abc123", []byte("response-data"), nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "ethereum:0x123", "eth_call:abc123", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("response-data"), val)
	})

	t.Run("get returns ErrRecordNotFound for missing key", func(t *testing.T) {
		_, err := connector.Get(ctx, ConnectorMainIndex, "nonexistent", "nonexistent", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "record not found")
	})

	t.Run("set with TTL and get before expiry succeeds", func(t *testing.T) {
		ttl := 10 * time.Second
		err := connector.Set(ctx, "ttl-pk", "ttl-rk", []byte("ttl-value"), &ttl)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "ttl-pk", "ttl-rk", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("ttl-value"), val)
	})

	t.Run("expired TTL returns error", func(t *testing.T) {
		// Set with a very short TTL
		ttl := 1 * time.Second
		err := connector.Set(ctx, "expired-pk", "expired-rk", []byte("expired-value"), &ttl)
		require.NoError(t, err)

		// Wait for expiry
		time.Sleep(2 * time.Second)

		_, err = connector.Get(ctx, ConnectorMainIndex, "expired-pk", "expired-rk", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "record expired")
	})

	t.Run("overwrite existing key", func(t *testing.T) {
		err := connector.Set(ctx, "overwrite-pk", "overwrite-rk", []byte("value-1"), nil)
		require.NoError(t, err)

		err = connector.Set(ctx, "overwrite-pk", "overwrite-rk", []byte("value-2"), nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "overwrite-pk", "overwrite-rk", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("value-2"), val)
	})
}

func TestS3ConnectorDelete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-delete-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-delete-bucket")

	t.Run("delete existing key", func(t *testing.T) {
		err := connector.Set(ctx, "del-pk", "del-rk", []byte("to-delete"), nil)
		require.NoError(t, err)

		err = connector.Delete(ctx, "del-pk", "del-rk")
		require.NoError(t, err)

		_, err = connector.Get(ctx, ConnectorMainIndex, "del-pk", "del-rk", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "record not found")
	})

	t.Run("delete non-existent key does not error", func(t *testing.T) {
		// S3 DeleteObject is idempotent
		err := connector.Delete(ctx, "nonexistent-pk", "nonexistent-rk")
		require.NoError(t, err)
	})
}

func TestS3ConnectorReverseIndex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-rvi-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-rvi-bucket")

	// Insert test data
	testData := []struct {
		pk    string
		rk    string
		value string
	}{
		{"user:1", "profile", "user1-profile"},
		{"user:2", "profile", "user2-profile"},
		{"user:1", "settings", "user1-settings"},
		{"event:1", "data", "event1-data"},
	}

	for _, d := range testData {
		err := connector.Set(ctx, d.pk, d.rk, []byte(d.value), nil)
		require.NoError(t, err, "failed to insert test data")
	}

	t.Run("exact match on both keys", func(t *testing.T) {
		val, err := connector.Get(ctx, ConnectorReverseIndex, "user:1", "profile", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("user1-profile"), val)
	})

	t.Run("match on range key with wildcard partition key", func(t *testing.T) {
		val, err := connector.Get(ctx, ConnectorReverseIndex, "*", "profile", nil)
		require.NoError(t, err)
		// Should return one of the profile values
		require.True(t, string(val) == "user1-profile" || string(val) == "user2-profile",
			"expected a profile value, got %s", string(val))
	})

	t.Run("non-existent range key returns not found", func(t *testing.T) {
		_, err := connector.Get(ctx, ConnectorReverseIndex, "*", "nonexistent", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "record not found")
	})

	t.Run("wildcard range key returns error", func(t *testing.T) {
		_, err := connector.Get(ctx, ConnectorReverseIndex, "*", "prof*", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not contain wildcards")
	})
}

func TestS3ConnectorList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-list-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-list-bucket")

	// Insert test data
	for i := 0; i < 3; i++ {
		pk := fmt.Sprintf("list-pk-%d", i)
		rk := fmt.Sprintf("list-rk-%d", i)
		err := connector.Set(ctx, pk, rk, []byte(fmt.Sprintf("value-%d", i)), nil)
		require.NoError(t, err)
	}

	t.Run("list returns all items", func(t *testing.T) {
		items, _, err := connector.List(ctx, ConnectorMainIndex, 100, "")
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(items), 3, "should list at least 3 items")
	})

	t.Run("list with limit", func(t *testing.T) {
		items, _, err := connector.List(ctx, ConnectorMainIndex, 1, "")
		require.NoError(t, err)
		require.Len(t, items, 1, "should return exactly 1 item")
	})
}

func TestS3ConnectorUnsupportedOperations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-unsupported-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-unsupported-bucket")

	t.Run("Lock returns error", func(t *testing.T) {
		_, err := connector.Lock(ctx, "some-key", 5*time.Second)
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not support distributed locking")
	})

	t.Run("WatchCounterInt64 returns error", func(t *testing.T) {
		_, _, err := connector.WatchCounterInt64(ctx, "some-key")
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not support counter watching")
	})

	t.Run("PublishCounterInt64 returns error", func(t *testing.T) {
		err := connector.PublishCounterInt64(ctx, "some-key", CounterInt64State{Value: 42})
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not support counter publishing")
	})
}

func TestS3ConnectorKeyPrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-prefix-bucket")
	defer cleanup()

	cfg := &common.S3ConnectorConfig{
		Endpoint:    endpoint,
		Region:      "us-east-1",
		Bucket:      "test-prefix-bucket",
		KeyPrefix:   "v1/",
		InitTimeout: common.Duration(5 * time.Second),
		GetTimeout:  common.Duration(5 * time.Second),
		SetTimeout:  common.Duration(5 * time.Second),
		MaxRetries:  3,
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
		},
	}

	connector, err := NewS3Connector(ctx, &log.Logger, "test-s3-prefix", cfg)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 10*time.Second, 100*time.Millisecond)

	t.Run("set and get with prefix", func(t *testing.T) {
		err := connector.Set(ctx, "prefix-pk", "prefix-rk", []byte("prefixed-value"), nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "prefix-pk", "prefix-rk", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("prefixed-value"), val)
	})
}
