package data

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

	// Create the test bucket using v2 SDK
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", ""),
		),
	)
	require.NoError(t, err)

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	_, err = s3Client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	require.NoError(t, err, "failed to create test bucket")

	cleanup := func() {
		container.Terminate(ctx) //nolint:errcheck
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

		err = connector.Set(ctx, "pk", "rk", []byte("value"), nil, nil)
		require.Error(t, err, "Set should fail because S3 is not connected")

		_, err = connector.Get(ctx, "", "pk", "rk", nil)
		require.Error(t, err, "Get should fail because S3 is not connected")
	})
}

// TestS3ConnectorBlockNumberMethods verifies block-number-based methods (finalized data).
// These methods have the block number in the partition key on both SET and GET.
// e.g. eth_getBlockByNumber, eth_call, eth_getBalance, eth_getCode, eth_getStorageAt
func TestS3ConnectorBlockNumberMethods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-blocknum-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-blocknum-bucket")

	t.Run("eth_getBlockByNumber round trip", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:18000000", "eth_getBlockByNumber:abc123", []byte(`{"blockNumber":"0x112A880"}`), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:18000000", "eth_getBlockByNumber:abc123", nil)
		require.NoError(t, err)
		require.Equal(t, `{"blockNumber":"0x112A880"}`, string(val))
	})

	t.Run("eth_call with specific block", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:18000000", "eth_call:def456", []byte(`{"result":"0x1234"}`), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:18000000", "eth_call:def456", nil)
		require.NoError(t, err)
		require.Equal(t, `{"result":"0x1234"}`, string(val))
	})

	t.Run("different block numbers dont collide", func(t *testing.T) {
		// Different block numbers produce different rangeKeys (because the block param differs in the hash)
		err := connector.Set(ctx, "evm:1:18000000", "eth_getBlockByNumber:hash_block_18m", []byte("block-18m"), nil, nil)
		require.NoError(t, err)

		err = connector.Set(ctx, "evm:1:18000001", "eth_getBlockByNumber:hash_block_18m1", []byte("block-18m+1"), nil, nil)
		require.NoError(t, err)

		val1, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:18000000", "eth_getBlockByNumber:hash_block_18m", nil)
		require.NoError(t, err)
		require.Equal(t, "block-18m", string(val1))

		val2, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:18000001", "eth_getBlockByNumber:hash_block_18m1", nil)
		require.NoError(t, err)
		require.Equal(t, "block-18m+1", string(val2))
	})
}

// TestS3ConnectorBlockHashMethods verifies block-hash-based methods.
// e.g. eth_getBlockByHash, eth_getTransactionByBlockHashAndIndex
func TestS3ConnectorBlockHashMethods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-blockhash-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-blockhash-bucket")

	t.Run("eth_getBlockByHash with hex hash partition key", func(t *testing.T) {
		// Block hash in partition key — extractNetworkId must correctly strip the hash
		err := connector.Set(ctx, "evm:1:0xabcdef1234567890abcdef1234567890", "eth_getBlockByHash:hash111", []byte("block-by-hash-result"), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:0xabcdef1234567890abcdef1234567890", "eth_getBlockByHash:hash111", nil)
		require.NoError(t, err)
		require.Equal(t, "block-by-hash-result", string(val))
	})

	t.Run("different hashes same network dont collide", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:0xaaa", "eth_getBlockByHash:hash_aaa", []byte("result-aaa"), nil, nil)
		require.NoError(t, err)

		err = connector.Set(ctx, "evm:1:0xbbb", "eth_getBlockByHash:hash_bbb", []byte("result-bbb"), nil, nil)
		require.NoError(t, err)

		val1, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:0xaaa", "eth_getBlockByHash:hash_aaa", nil)
		require.NoError(t, err)
		require.Equal(t, "result-aaa", string(val1))

		val2, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:0xbbb", "eth_getBlockByHash:hash_bbb", nil)
		require.NoError(t, err)
		require.Equal(t, "result-bbb", string(val2))
	})
}

// TestS3ConnectorTransactionHashMethods verifies tx-hash-based methods (the critical scenario).
// SET stores with an actual block number, GET uses wildcard "*".
// With block-agnostic keys, both map to the same S3 key: {networkId}/{rangeKey}.
// Methods: eth_getTransactionReceipt, eth_getTransactionByHash, debug_traceTransaction, etc.
func TestS3ConnectorTransactionHashMethods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-txhash-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-txhash-bucket")

	txMethods := []struct {
		name     string
		method   string
		rangeKey string
	}{
		{"eth_getTransactionReceipt", "eth_getTransactionReceipt", "eth_getTransactionReceipt:txhash_abc"},
		{"eth_getTransactionByHash", "eth_getTransactionByHash", "eth_getTransactionByHash:txhash_def"},
		{"debug_traceTransaction", "debug_traceTransaction", "debug_traceTransaction:txhash_ghi"},
		{"trace_transaction", "trace_transaction", "trace_transaction:txhash_jkl"},
		{"trace_replayTransaction", "trace_replayTransaction", "trace_replayTransaction:txhash_mno"},
		{"arbtrace_replayTransaction", "arbtrace_replayTransaction", "arbtrace_replayTransaction:txhash_pqr"},
	}

	for _, tc := range txMethods {
		tc := tc
		t.Run(tc.name+"_SET_with_block_GET_with_wildcard", func(t *testing.T) {
			value := []byte(fmt.Sprintf("%s-result", tc.method))

			// SET with specific block number in partition key (as happens during response caching)
			err := connector.Set(ctx, "evm:1:18000000", tc.rangeKey, value, nil, nil)
			require.NoError(t, err)

			// GET with wildcard "*" (as happens during request lookup — block unknown)
			val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:*", tc.rangeKey, nil)
			require.NoError(t, err)
			require.Equal(t, value, val, "GET with wildcard should find value stored with specific block")
		})

		t.Run(tc.name+"_GET_via_reverse_index", func(t *testing.T) {
			value := []byte(fmt.Sprintf("%s-rvi-result", tc.method))
			rk := tc.rangeKey + "_rvi"

			// SET with specific block number
			err := connector.Set(ctx, "evm:1:18000000", rk, value, nil, nil)
			require.NoError(t, err)

			// GET via ConnectorReverseIndex (cache layer sends this for blockRef="*" methods)
			// With block-agnostic keys, both indexes resolve identically
			val, err := connector.Get(ctx, ConnectorReverseIndex, "evm:1:*", rk, nil)
			require.NoError(t, err)
			require.Equal(t, value, val, "GET via reverse index should find value")
		})
	}
}

// TestS3ConnectorStaticMethods verifies methods that never change (blockRef="*").
// e.g. eth_chainId, net_version
func TestS3ConnectorStaticMethods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-static-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-static-bucket")

	t.Run("eth_chainId", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:*", "eth_chainId:aaa111", []byte(`"0x1"`), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:*", "eth_chainId:aaa111", nil)
		require.NoError(t, err)
		require.Equal(t, `"0x1"`, string(val))
	})

	t.Run("net_version persists without TTL", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:*", "net_version:bbb222", []byte(`"1"`), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:*", "net_version:bbb222", nil)
		require.NoError(t, err)
		require.Equal(t, `"1"`, string(val))
	})
}

// TestS3ConnectorRealtimeMethods verifies methods with short TTL (blockRef="*").
// e.g. eth_gasPrice, eth_blockNumber, eth_maxPriorityFeePerGas
func TestS3ConnectorRealtimeMethods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-realtime-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-realtime-bucket")

	t.Run("eth_gasPrice with TTL", func(t *testing.T) {
		ttl := 10 * time.Second
		err := connector.Set(ctx, "evm:1:*", "eth_gasPrice:ccc333", []byte(`"0x3B9ACA00"`), &ttl, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:*", "eth_gasPrice:ccc333", nil)
		require.NoError(t, err)
		require.Equal(t, `"0x3B9ACA00"`, string(val))
	})

	t.Run("eth_blockNumber expires after TTL", func(t *testing.T) {
		ttl := 1 * time.Second
		err := connector.Set(ctx, "evm:1:*", "eth_blockNumber:ddd444", []byte(`"0x112A880"`), &ttl, nil)
		require.NoError(t, err)

		// Read before expiry
		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:*", "eth_blockNumber:ddd444", nil)
		require.NoError(t, err)
		require.Equal(t, `"0x112A880"`, string(val))

		// Wait for expiry
		time.Sleep(2 * time.Second)

		_, err = connector.Get(ctx, ConnectorMainIndex, "evm:1:*", "eth_blockNumber:ddd444", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "record expired")
	})

	t.Run("overwrite with new value updates correctly", func(t *testing.T) {
		ttl := 10 * time.Second
		err := connector.Set(ctx, "evm:1:*", "eth_gasPrice:eee555", []byte("old-price"), &ttl, nil)
		require.NoError(t, err)

		err = connector.Set(ctx, "evm:1:*", "eth_gasPrice:eee555", []byte("new-price"), &ttl, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:*", "eth_gasPrice:eee555", nil)
		require.NoError(t, err)
		require.Equal(t, "new-price", string(val))
	})
}

// TestS3ConnectorMultiBlockRangeMethods verifies methods like eth_getLogs
// where blockRef becomes "*" due to fromBlock/toBlock range conflict.
func TestS3ConnectorMultiBlockRangeMethods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-multiblock-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-multiblock-bucket")

	t.Run("eth_getLogs with wildcard blockRef", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:*", "eth_getLogs:fff666", []byte(`[{"logIndex":"0x0"}]`), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:*", "eth_getLogs:fff666", nil)
		require.NoError(t, err)
		require.Equal(t, `[{"logIndex":"0x0"}]`, string(val))
	})
}

// TestS3ConnectorCrossNetworkIsolation verifies that the same rangeKey across
// different networks does NOT collide.
func TestS3ConnectorCrossNetworkIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-crossnet-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-crossnet-bucket")

	err := connector.Set(ctx, "evm:1:18000000", "eth_call:same_hash", []byte("mainnet-result"), nil, nil)
	require.NoError(t, err)

	err = connector.Set(ctx, "evm:137:18000000", "eth_call:same_hash", []byte("polygon-result"), nil, nil)
	require.NoError(t, err)

	val1, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:18000000", "eth_call:same_hash", nil)
	require.NoError(t, err)
	require.Equal(t, "mainnet-result", string(val1))

	val2, err := connector.Get(ctx, ConnectorMainIndex, "evm:137:18000000", "eth_call:same_hash", nil)
	require.NoError(t, err)
	require.Equal(t, "polygon-result", string(val2))
}

// TestS3ConnectorFinalityStorageClass verifies per-finality storage class via context.
func TestS3ConnectorFinalityStorageClass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-finality-bucket")
	defer cleanup()

	cfg := &common.S3ConnectorConfig{
		Endpoint:    endpoint,
		Region:      "us-east-1",
		Bucket:      "test-finality-bucket",
		InitTimeout: common.Duration(5 * time.Second),
		GetTimeout:  common.Duration(5 * time.Second),
		SetTimeout:  common.Duration(5 * time.Second),
		MaxRetries:  3,
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
		},
		EvmFinalityS3Metadata: &common.EvmFinalityS3Metadata{
			Finalized:   &common.S3ObjectMetadata{StorageClass: "STANDARD_IA"},
			Unfinalized: &common.S3ObjectMetadata{StorageClass: "STANDARD"},
			Realtime:    &common.S3ObjectMetadata{StorageClass: "STANDARD"},
		},
	}

	connector, err := NewS3Connector(ctx, &log.Logger, "test-s3-finality", cfg)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 10*time.Second, 100*time.Millisecond)

	t.Run("finalized storage class applied", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:18000000", "eth_getBlockByNumber:fin1", []byte("finalized-data"), nil, common.DataFinalityStateFinalized)
		require.NoError(t, err)

		// Verify data is readable
		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:18000000", "eth_getBlockByNumber:fin1", nil)
		require.NoError(t, err)
		require.Equal(t, "finalized-data", string(val))
	})

	t.Run("unfinalized storage class applied", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:18000100", "eth_getBlockByNumber:unfin1", []byte("unfinalized-data"), nil, common.DataFinalityStateUnfinalized)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:18000100", "eth_getBlockByNumber:unfin1", nil)
		require.NoError(t, err)
		require.Equal(t, "unfinalized-data", string(val))
	})

	t.Run("no finality metadata still works", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:18000200", "eth_getBlockByNumber:nofin1", []byte("no-finality-data"), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:18000200", "eth_getBlockByNumber:nofin1", nil)
		require.NoError(t, err)
		require.Equal(t, "no-finality-data", string(val))
	})
}

// TestS3ConnectorTTLBehavior covers various TTL scenarios.
func TestS3ConnectorTTLBehavior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-ttl-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-ttl-bucket")

	t.Run("set with TTL and get before expiry succeeds", func(t *testing.T) {
		ttl := 10 * time.Second
		err := connector.Set(ctx, "ttl-pk:block", "ttl-rk", []byte("ttl-value"), &ttl, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "ttl-pk:block", "ttl-rk", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("ttl-value"), val)
	})

	t.Run("expired TTL returns error", func(t *testing.T) {
		ttl := 1 * time.Second
		err := connector.Set(ctx, "exp-pk:block", "exp-rk", []byte("expired-value"), &ttl, nil)
		require.NoError(t, err)

		time.Sleep(2 * time.Second)

		_, err = connector.Get(ctx, ConnectorMainIndex, "exp-pk:block", "exp-rk", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "record expired")
	})

	t.Run("set without TTL never expires", func(t *testing.T) {
		err := connector.Set(ctx, "nottl-pk:block", "nottl-rk", []byte("no-ttl-value"), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "nottl-pk:block", "nottl-rk", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("no-ttl-value"), val)
	})

	t.Run("overwrite with TTL then without TTL removes expiry", func(t *testing.T) {
		ttl := 1 * time.Second
		err := connector.Set(ctx, "ovttl-pk:block", "ovttl-rk", []byte("with-ttl"), &ttl, nil)
		require.NoError(t, err)

		// Overwrite without TTL
		err = connector.Set(ctx, "ovttl-pk:block", "ovttl-rk", []byte("without-ttl"), nil, nil)
		require.NoError(t, err)

		// Wait past original TTL
		time.Sleep(2 * time.Second)

		// Should still be readable (no TTL)
		val, err := connector.Get(ctx, ConnectorMainIndex, "ovttl-pk:block", "ovttl-rk", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("without-ttl"), val)
	})
}

// TestS3ConnectorDeleteBehavior covers delete scenarios.
func TestS3ConnectorDeleteBehavior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-delbeh-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-delbeh-bucket")

	t.Run("set then delete then get returns not found", func(t *testing.T) {
		err := connector.Set(ctx, "del-pk:block", "del-rk", []byte("to-delete"), nil, nil)
		require.NoError(t, err)

		err = connector.Delete(ctx, "del-pk:block", "del-rk")
		require.NoError(t, err)

		_, err = connector.Get(ctx, ConnectorMainIndex, "del-pk:block", "del-rk", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "record not found")
	})

	t.Run("delete non-existent key does not error", func(t *testing.T) {
		err := connector.Delete(ctx, "nonexistent-pk:block", "nonexistent-rk")
		require.NoError(t, err)
	})

	t.Run("delete with different blockRef still deletes same S3 key", func(t *testing.T) {
		// SET with block 18000000
		err := connector.Set(ctx, "evm:1:18000000", "del-test-rk", []byte("value"), nil, nil)
		require.NoError(t, err)

		// DELETE with wildcard blockRef — both map to same S3 key
		err = connector.Delete(ctx, "evm:1:*", "del-test-rk")
		require.NoError(t, err)

		// Should be gone
		_, err = connector.Get(ctx, ConnectorMainIndex, "evm:1:18000000", "del-test-rk", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "record not found")
	})
}

// TestS3ConnectorKeyPrefix verifies key prefix isolation.
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
		err := connector.Set(ctx, "prefix-pk:block", "prefix-rk", []byte("prefixed-value"), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "prefix-pk:block", "prefix-rk", nil)
		require.NoError(t, err)
		require.Equal(t, []byte("prefixed-value"), val)
	})
}

// TestS3ConnectorListUnsupported verifies that List returns an error.
func TestS3ConnectorListUnsupported(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-listuns-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-listuns-bucket")

	_, _, err := connector.List(ctx, ConnectorMainIndex, 100, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not support List")
}

// TestS3ConnectorUnsupportedOperations verifies Lock, WatchCounterInt64, PublishCounterInt64.
func TestS3ConnectorUnsupportedOperations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-unsup-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-unsup-bucket")

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

// TestS3ConnectorEdgeCases covers edge cases.
func TestS3ConnectorEdgeCases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint, cleanup := createMinIOContainer(t, ctx, "test-edge-bucket")
	defer cleanup()

	connector := createTestS3Connector(t, ctx, endpoint, "test-edge-bucket")

	t.Run("empty value can be stored and retrieved", func(t *testing.T) {
		err := connector.Set(ctx, "evm:1:100", "empty-rk", []byte{}, nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:100", "empty-rk", nil)
		require.NoError(t, err)
		require.Empty(t, val)
	})

	t.Run("large value 1MB works", func(t *testing.T) {
		largeVal := make([]byte, 1024*1024)
		for i := range largeVal {
			largeVal[i] = byte(i % 256)
		}

		err := connector.Set(ctx, "evm:1:100", "large-rk", largeVal, nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:100", "large-rk", nil)
		require.NoError(t, err)
		require.Equal(t, largeVal, val)
	})

	t.Run("special characters in rangeKey", func(t *testing.T) {
		rk := "eth_call:0xabcdef+/=&?#"
		err := connector.Set(ctx, "evm:1:100", rk, []byte("special-chars"), nil, nil)
		require.NoError(t, err)

		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:100", rk, nil)
		require.NoError(t, err)
		require.Equal(t, "special-chars", string(val))
	})

	t.Run("concurrent set and get to same key no corruption", func(t *testing.T) {
		const concurrency = 10
		var wg sync.WaitGroup
		errors := make([]error, concurrency)

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				val := []byte(fmt.Sprintf("concurrent-value-%d", idx))
				errors[idx] = connector.Set(ctx, "evm:1:100", "concurrent-rk", val, nil, nil)
			}(i)
		}
		wg.Wait()

		for i, err := range errors {
			require.NoError(t, err, "Set %d should not error", i)
		}

		// Final read should succeed with some value
		val, err := connector.Get(ctx, ConnectorMainIndex, "evm:1:100", "concurrent-rk", nil)
		require.NoError(t, err)
		require.NotEmpty(t, val)
	})
}

// TestExtractNetworkId unit tests for the key extraction helper.
func TestExtractNetworkId(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"evm:1:18000000", "evm:1"},
		{"evm:137:*", "evm:137"},
		{"evm:1:0xabcdef", "evm:1"},
		{"evm:1:latest", "evm:1"},
		{"evm:56:18000000", "evm:56"},
		{"evm:8453:*", "evm:8453"},
		{"singlevalue", "singlevalue"}, // no colon — return as-is
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := extractNetworkId(tc.input)
			require.Equal(t, tc.expected, result)
		})
	}
}
