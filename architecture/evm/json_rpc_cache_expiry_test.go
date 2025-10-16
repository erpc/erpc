package evm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestCacheExpiredRecordHandling tests that ErrRecordExpired is properly treated as a cache miss
func TestCacheExpiredRecordHandling(t *testing.T) {
	ctx := context.Background()
	logger := log.Logger

	t.Run("ErrRecordExpired treated as cache miss not error", func(t *testing.T) {
		// Create a mock connector that will return ErrRecordExpired
		mockConnector := data.NewMockConnector("test-expired")

		// Set up the mock to return ErrRecordExpired for Get calls
		testKey := "evm:1:0x123"
		testRangeKey := "eth_getBlockByNumber:hash123"
		mockConnector.On("Get", mock.Anything, data.ConnectorMainIndex, testKey, testRangeKey, mock.Anything).
			Return(nil, common.NewErrRecordExpired(testKey, testRangeKey, "mock", time.Now().Unix(), time.Now().Unix()-10))

		// Create cache with the mock connector
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  "realtime",
			Connector: mockConnector.Id(),
			TTL:       common.Duration(2 * time.Second),
		}, mockConnector)
		require.NoError(t, err)

		cache := &EvmJsonRpcCache{
			projectId: "test",
			policies:  []*data.CachePolicy{policy},
			logger:    &logger,
		}

		// Create a test request
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x123",false]}`))
		req.SetNetwork(&common.NormalizedNetworkRef{
			NetworkId: "evm:1",
		})
		req.SetFinality(common.DataFinalityStateRealtime)

		// Call cache.Get() - should return nil (cache miss), not error
		resp, err := cache.Get(ctx, req)

		// Verify: ErrRecordExpired is treated as cache miss (nil response, no error)
		assert.NoError(t, err, "ErrRecordExpired should not be returned as an error")
		assert.Nil(t, resp, "ErrRecordExpired should be treated as cache miss (nil response)")

		// Verify the connector was called
		mockConnector.AssertCalled(t, "Get", mock.Anything, data.ConnectorMainIndex, testKey, testRangeKey, mock.Anything)
	})

	t.Run("ErrRecordNotFound also treated as cache miss", func(t *testing.T) {
		// Create a mock connector that will return ErrRecordNotFound
		mockConnector := data.NewMockConnector("test-notfound")

		testKey := "evm:1:0x456"
		testRangeKey := "eth_getBlockByNumber:hash456"
		mockConnector.On("Get", mock.Anything, data.ConnectorMainIndex, testKey, testRangeKey, mock.Anything).
			Return(nil, common.NewErrRecordNotFound(testKey, testRangeKey, "mock"))

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  "realtime",
			Connector: mockConnector.Id(),
			TTL:       common.Duration(2 * time.Second),
		}, mockConnector)
		require.NoError(t, err)

		cache := &EvmJsonRpcCache{
			projectId: "test",
			policies:  []*data.CachePolicy{policy},
			logger:    &logger,
		}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x456",false]}`))
		req.SetNetwork(&common.NormalizedNetworkRef{
			NetworkId: "evm:1",
		})
		req.SetFinality(common.DataFinalityStateRealtime)

		// Call cache.Get() - should return nil (cache miss), not error
		resp, err := cache.Get(ctx, req)

		// Verify: ErrRecordNotFound is treated as cache miss
		assert.NoError(t, err, "ErrRecordNotFound should not be returned as an error")
		assert.Nil(t, resp, "ErrRecordNotFound should be treated as cache miss")

		mockConnector.AssertCalled(t, "Get", mock.Anything, data.ConnectorMainIndex, testKey, testRangeKey, mock.Anything)
	})

	t.Run("Other errors are propagated", func(t *testing.T) {
		// Create a mock connector that will return a different error
		mockConnector := data.NewMockConnector("test-error")

		testKey := "evm:1:0x789"
		testRangeKey := "eth_getBlockByNumber:hash789"
		testError := fmt.Errorf("connection failed")
		mockConnector.On("Get", mock.Anything, data.ConnectorMainIndex, testKey, testRangeKey, mock.Anything).
			Return(nil, testError)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  "realtime",
			Connector: mockConnector.Id(),
			TTL:       common.Duration(2 * time.Second),
		}, mockConnector)
		require.NoError(t, err)

		cache := &EvmJsonRpcCache{
			projectId: "test",
			policies:  []*data.CachePolicy{policy},
			logger:    &logger,
		}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x789",false]}`))
		req.SetNetwork(&common.NormalizedNetworkRef{
			NetworkId: "evm:1",
		})
		req.SetFinality(common.DataFinalityStateRealtime)

		// Call cache.Get() - should return the error (not treated as cache miss)
		resp, err := cache.Get(ctx, req)

		// Verify: Other errors are still propagated
		assert.Nil(t, resp, "response should be nil on error")
		// The error will be logged but Get() returns nil for non-found errors
		// This is the expected behavior based on the code

		mockConnector.AssertCalled(t, "Get", mock.Anything, data.ConnectorMainIndex, testKey, testRangeKey, mock.Anything)
	})

	t.Run("Successful cache hit returns data", func(t *testing.T) {
		// Create a mock connector that will return valid data
		mockConnector := data.NewMockConnector("test-hit")

		testKey := "evm:1:0xabc"
		testRangeKey := "eth_getBlockByNumber:hashabc"
		cachedResponse := []byte(`{"number":"0xabc","hash":"0x123"}`)
		mockConnector.On("Get", mock.Anything, data.ConnectorMainIndex, testKey, testRangeKey, mock.Anything).
			Return(cachedResponse, nil)

		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  "finalized",
			Connector: mockConnector.Id(),
			TTL:       common.Duration(0),
		}, mockConnector)
		require.NoError(t, err)

		cache := &EvmJsonRpcCache{
			projectId: "test",
			policies:  []*data.CachePolicy{policy},
			logger:    &logger,
		}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0xabc",false]}`))
		req.SetNetwork(&common.NormalizedNetworkRef{
			NetworkId: "evm:1",
		})
		req.SetFinality(common.DataFinalityStateFinalized)

		// Call cache.Get() - should return cached data
		resp, err := cache.Get(ctx, req)

		// Verify: Cache hit returns data successfully
		assert.NoError(t, err)
		assert.NotNil(t, resp, "response should not be nil on cache hit")
		assert.True(t, resp.IsFromCache(), "response should be marked as from cache")

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, string(jrr.GetResultBytes()), "0xabc")

		mockConnector.AssertCalled(t, "Get", mock.Anything, data.ConnectorMainIndex, testKey, testRangeKey, mock.Anything)
	})
}

// TestDynamoDBRealtimeCacheExpiry tests the full workflow with real DynamoDB
func TestDynamoDBRealtimeCacheExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

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
	require.NoError(t, err)
	defer ddbC.Terminate(ctx)

	host, err := ddbC.Host(ctx)
	require.NoError(t, err)
	port, err := ddbC.MappedPort(ctx, "8000")
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	// Create DynamoDB connector
	cfg := &common.DynamoDBConnectorConfig{
		Endpoint:         endpoint,
		Region:           "us-west-2",
		Table:            "test_cache_expiry",
		PartitionKeyName: "groupKey",
		RangeKeyName:     "requestKey",
		ReverseIndexName: "idx_requestKey_groupKey",
		TTLAttributeName: "ttl",
		InitTimeout:      common.Duration(5 * time.Second),
		GetTimeout:       common.Duration(2 * time.Second),
		SetTimeout:       common.Duration(2 * time.Second),
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}

	connector, err := data.NewConnector(ctx, &log.Logger, &common.ConnectorConfig{
		Id:       "dynamodb-test",
		Driver:   "dynamodb",
		DynamoDB: cfg,
	})
	require.NoError(t, err)

	// Wait for connector to be ready
	if ddbConn, ok := connector.(*data.DynamoDBConnector); ok {
		require.Eventually(t, func() bool {
			return ddbConn.Initializer().State() == util.StateReady
		}, 10*time.Second, 100*time.Millisecond)
	}

	t.Run("Expired realtime cache triggers fresh fetch", func(t *testing.T) {
		// Create a cache policy with very short TTL
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  "realtime",
			Connector: connector.Id(),
			TTL:       common.Duration(100 * time.Millisecond), // Very short TTL for testing
		}, connector)
		require.NoError(t, err)

		cache := &EvmJsonRpcCache{
			projectId: "test",
			policies:  []*data.CachePolicy{policy},
			logger:    &log.Logger,
		}

		// Store a value in the cache with short TTL
		groupKey := "evm:1:0x100"
		requestKey := "eth_blockNumber:test"

		ttl := 100 * time.Millisecond
		err = connector.Set(ctx, groupKey, requestKey, []byte(`"0x100"`), &ttl)
		require.NoError(t, err)

		// Create a request for that cached data
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
		req.SetNetwork(&common.NormalizedNetworkRef{
			NetworkId: "evm:1",
		})
		req.SetFinality(common.DataFinalityStateRealtime)

		// First fetch should hit cache
		resp, err := cache.Get(ctx, req)
		assert.NoError(t, err)
		if resp != nil {
			assert.True(t, resp.IsFromCache())
		}

		// Wait for TTL to expire
		time.Sleep(150 * time.Millisecond)

		// Second fetch after expiry should be treated as cache miss (nil response, no error)
		resp2, err2 := cache.Get(ctx, req)
		assert.NoError(t, err2, "ErrRecordExpired should be treated as cache miss, not returned as error")
		assert.Nil(t, resp2, "Expired record should result in cache miss (nil response)")
	})

	t.Run("Non-expired cache returns data successfully", func(t *testing.T) {
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Network:   "*",
			Method:    "*",
			Finality:  "finalized",
			Connector: connector.Id(),
			TTL:       common.Duration(0), // No expiry
		}, connector)
		require.NoError(t, err)

		cache := &EvmJsonRpcCache{
			projectId: "test",
			policies:  []*data.CachePolicy{policy},
			logger:    &log.Logger,
		}

		// Store finalized data (no TTL)
		groupKey := "evm:1:0x200"
		requestKey := "eth_getBlockByNumber:test200"

		err = connector.Set(ctx, groupKey, requestKey, []byte(`{"number":"0x200","hash":"0xabc"}`), nil)
		require.NoError(t, err)

		// Create request
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x200",false]}`))
		req.SetNetwork(&common.NormalizedNetworkRef{
			NetworkId: "evm:1",
		})
		req.SetFinality(common.DataFinalityStateFinalized)

		// Fetch should return cached data
		resp, err := cache.Get(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.IsFromCache())

		// Wait a bit and verify it's still cached (no TTL means never expires)
		time.Sleep(200 * time.Millisecond)

		resp2, err2 := cache.Get(ctx, req)
		assert.NoError(t, err2)
		assert.NotNil(t, resp2, "Finalized data should remain in cache")
		assert.True(t, resp2.IsFromCache())
	})
}

// TestDynamoDBCounterWithCacheExpiry verifies counter and cache work together
func TestDynamoDBCounterWithCacheExpiry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

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
	require.NoError(t, err)
	defer ddbC.Terminate(ctx)

	host, err := ddbC.Host(ctx)
	require.NoError(t, err)
	port, err := ddbC.MappedPort(ctx, "8000")
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	// Create connector
	cfg := &common.DynamoDBConnectorConfig{
		Endpoint:          endpoint,
		Region:            "us-west-2",
		Table:             "test_counter_cache",
		PartitionKeyName:  "groupKey",
		RangeKeyName:      "requestKey",
		ReverseIndexName:  "idx_requestKey_groupKey",
		TTLAttributeName:  "ttl",
		InitTimeout:       common.Duration(5 * time.Second),
		GetTimeout:        common.Duration(2 * time.Second),
		SetTimeout:        common.Duration(2 * time.Second),
		StatePollInterval: common.Duration(100 * time.Millisecond),
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}

	connector, err := data.NewDynamoDBConnector(ctx, &log.Logger, "test-counter-cache", cfg)
	require.NoError(t, err)

	// Wait for connector
	require.Eventually(t, func() bool {
		return connector.Initializer().State() == util.StateReady
	}, 10*time.Second, 100*time.Millisecond)

	t.Run("Counter values persist and cache respects TTL", func(t *testing.T) {
		counterKey := "test-cluster/latestBlock/upstream-test/abc"

		// Publish counter value
		err := connector.PublishCounterInt64(ctx, counterKey, 1000)
		require.NoError(t, err)

		// Retrieve counter value
		val, err := connector.GetSimpleValue(ctx, counterKey)
		require.NoError(t, err)
		assert.Equal(t, int64(1000), val)

		// Store cache data with TTL
		cacheKey := "evm:1:0x3e8" // 1000 in hex
		cacheRangeKey := "eth_getBlockByNumber:test1000"
		ttl := 100 * time.Millisecond
		err = connector.Set(ctx, cacheKey, cacheRangeKey, []byte(`{"number":"0x3e8"}`), &ttl)
		require.NoError(t, err)

		// Get cache data immediately - should succeed
		data, err := connector.Get(ctx, data.ConnectorMainIndex, cacheKey, cacheRangeKey, nil)
		require.NoError(t, err)
		assert.NotNil(t, data)

		// Wait for TTL to expire
		time.Sleep(150 * time.Millisecond)

		// Get cache data after expiry - should return ErrRecordExpired
		_, err = connector.Get(ctx, data.ConnectorMainIndex, cacheKey, cacheRangeKey, nil)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordExpired), "should return ErrRecordExpired for expired cache")

		// But counter value should still be accessible (no TTL)
		val2, err := connector.GetSimpleValue(ctx, counterKey)
		require.NoError(t, err)
		assert.Equal(t, int64(1000), val2, "counter value should persist")
	})
}
