package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func init() {
	_ = telemetry.SetHistogramBuckets("0.05,0.5,5,30")
	util.ConfigureTestLogger()
}

func TestEvmJsonRpcCache_BlockAgeValidation(t *testing.T) {
	ctx := context.Background()
	logger := log.Logger

	t.Run("AcceptsResultWithinTTL", func(t *testing.T) {
		// Create mock connector
		mockConnector := &data.MockConnector{}
		mockConnector.On("Id").Return("mock-connector")

		// Create a cached block response with a recent timestamp (5 seconds ago)
		currentTime := time.Now().Unix()
		blockTimestamp := fmt.Sprintf("0x%x", currentTime-5)
		cachedResponse := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number":    "0x1234",
				"timestamp": blockTimestamp,
				"hash":      "0xabcd",
			},
		}
		cachedBytes, _ := json.Marshal(cachedResponse)

		// Set up mock to return the cached response
		mockConnector.On("Get", mock.Anything, data.ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).
			Return(cachedBytes, nil)

		// Create cache policy with 1 minute TTL
		ttl := 1 * time.Minute
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "mock-connector",
			TTL:       common.Duration(ttl),
			Network:   "*",
			Method:    "eth_getBlockByNumber",
			Finality:  common.DataFinalityStateUnknown,
		}, mockConnector)
		require.NoError(t, err)

		// Create cache instance
		cache := &EvmJsonRpcCache{
			projectId: "test-project",
			logger:    &logger,
			policies:  []*data.CachePolicy{policy},
		}

		// Create request
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1234",true],"id":1}`))

		// Perform Get operation
		resp, err := cache.Get(ctx, req)

		// Should succeed as the block is only 5 seconds old (within 1 minute TTL)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.FromCache())
	})

	t.Run("RejectsResultExceedingTTL", func(t *testing.T) {
		// Create mock connector
		mockConnector := &data.MockConnector{}
		mockConnector.On("Id").Return("mock-connector")

		// Create a cached block response with an old timestamp (2 minutes ago)
		currentTime := time.Now().Unix()
		blockTimestamp := fmt.Sprintf("0x%x", currentTime-120)
		cachedResponse := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number":    "0x1234",
				"timestamp": blockTimestamp,
				"hash":      "0xabcd",
			},
		}
		cachedBytes, _ := json.Marshal(cachedResponse)

		// Set up mock to return the old cached response
		mockConnector.On("Get", mock.Anything, data.ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).
			Return(cachedBytes, nil)

		// Create cache policy with 1 minute TTL
		ttl := 1 * time.Minute
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "mock-connector",
			TTL:       common.Duration(ttl),
			Network:   "*",
			Method:    "eth_getBlockByNumber",
			Finality:  common.DataFinalityStateUnknown,
		}, mockConnector)
		require.NoError(t, err)

		// Create cache instance
		cache := &EvmJsonRpcCache{
			projectId: "test-project",
			logger:    &logger,
			policies:  []*data.CachePolicy{policy},
		}

		// Create request
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1234",true],"id":1}`))

		// Perform Get operation
		resp, err := cache.Get(ctx, req)

		// Should return nil as the block is 2 minutes old (exceeds 1 minute TTL)
		assert.NoError(t, err)
		assert.Nil(t, resp)
	})

	t.Run("AcceptsResultWithNoTimestamp", func(t *testing.T) {
		// Create mock connector
		mockConnector := &data.MockConnector{}
		mockConnector.On("Id").Return("mock-connector")

		// Create a cached response without block timestamp (e.g., eth_chainId)
		cachedResponse := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0x1",
		}
		cachedBytes, _ := json.Marshal(cachedResponse)

		// Set up mock to return the cached response
		mockConnector.On("Get", mock.Anything, data.ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).
			Return(cachedBytes, nil)

		// Create cache policy with 1 minute TTL
		ttl := 1 * time.Minute
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "mock-connector",
			TTL:       common.Duration(ttl),
			Network:   "*",
			Method:    "eth_chainId",
			Finality:  common.DataFinalityStateUnknown,
		}, mockConnector)
		require.NoError(t, err)

		// Create cache instance
		cache := &EvmJsonRpcCache{
			projectId: "test-project",
			logger:    &logger,
			policies:  []*data.CachePolicy{policy},
		}

		// Create request
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))

		// Perform Get operation
		resp, err := cache.Get(ctx, req)

		// Should succeed as we can't extract timestamp and thus can't validate age
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.FromCache())
	})

	t.Run("AcceptsResultWithNoTTL", func(t *testing.T) {
		// Create mock connector
		mockConnector := &data.MockConnector{}
		mockConnector.On("Id").Return("mock-connector")

		// Create a cached block response with an old timestamp (2 hours ago)
		currentTime := time.Now().Unix()
		blockTimestamp := fmt.Sprintf("0x%x", currentTime-7200)
		cachedResponse := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number":    "0x1234",
				"timestamp": blockTimestamp,
				"hash":      "0xabcd",
			},
		}
		cachedBytes, _ := json.Marshal(cachedResponse)

		// Set up mock to return the old cached response
		mockConnector.On("Get", mock.Anything, data.ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).
			Return(cachedBytes, nil)

		// Create cache policy with no TTL (nil)
		policy, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "mock-connector",
			Network:   "*",
			Method:    "eth_getBlockByNumber",
			Finality:  common.DataFinalityStateUnknown,
			// TTL is intentionally not set
		}, mockConnector)
		require.NoError(t, err)

		// Create cache instance
		cache := &EvmJsonRpcCache{
			projectId: "test-project",
			logger:    &logger,
			policies:  []*data.CachePolicy{policy},
		}

		// Create request
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["0x1234",true],"id":1}`))

		// Perform Get operation
		resp, err := cache.Get(ctx, req)

		// Should succeed as there's no TTL to validate against
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.FromCache())
	})

	t.Run("TriesNextPolicyAfterAgeRejection", func(t *testing.T) {
		// Create two mock connectors
		mockConnector1 := &data.MockConnector{}
		mockConnector1.On("Id").Return("mock-connector-1")

		mockConnector2 := &data.MockConnector{}
		mockConnector2.On("Id").Return("mock-connector-2")

		// Create an old cached response for connector 1 (2 minutes ago)
		currentTime := time.Now().Unix()
		oldBlockTimestamp := fmt.Sprintf("0x%x", currentTime-120)
		oldCachedResponse := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number":    "0x1234",
				"timestamp": oldBlockTimestamp,
				"hash":      "0xold",
			},
		}
		oldCachedBytes, _ := json.Marshal(oldCachedResponse)

		// Create a recent cached response for connector 2 (5 seconds ago)
		newBlockTimestamp := fmt.Sprintf("0x%x", currentTime-5)
		newCachedResponse := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"number":    "0x1235",
				"timestamp": newBlockTimestamp,
				"hash":      "0xnew",
			},
		}
		newCachedBytes, _ := json.Marshal(newCachedResponse)

		// Set up mocks
		mockConnector1.On("Get", mock.Anything, data.ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).
			Return(oldCachedBytes, nil)
		mockConnector2.On("Get", mock.Anything, data.ConnectorMainIndex, mock.Anything, mock.Anything, mock.Anything).
			Return(newCachedBytes, nil)

		// Create cache policies
		ttl := 1 * time.Minute
		policy1, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "mock-connector-1",
			TTL:       common.Duration(ttl),
			Network:   "*",
			Method:    "eth_getBlockByNumber",
			Finality:  common.DataFinalityStateUnknown,
		}, mockConnector1)
		require.NoError(t, err)

		policy2, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "mock-connector-2",
			TTL:       common.Duration(ttl),
			Network:   "*",
			Method:    "eth_getBlockByNumber",
			Finality:  common.DataFinalityStateUnknown,
		}, mockConnector2)
		require.NoError(t, err)

		// Create cache instance with both policies
		cache := &EvmJsonRpcCache{
			projectId: "test-project",
			logger:    &logger,
			policies:  []*data.CachePolicy{policy1, policy2},
		}

		// Create request
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_getBlockByNumber","params":["latest",true],"id":1}`))

		// Perform Get operation
		resp, err := cache.Get(ctx, req)

		// Should succeed with the second policy's result
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.True(t, resp.FromCache())

		// Verify it's the newer block from connector 2
		jrr, _ := resp.JsonRpcResponse(ctx)
		hash, _ := jrr.PeekStringByPath(ctx, "hash")
		assert.Equal(t, "0xnew", hash)
	})
}
