package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestDatabaseStrategy_CacheEnabled(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	// Create memory connector for testing
	memConnector, err := data.NewConnector(ctx, &logger, &common.ConnectorConfig{
		Driver: "memory",
		Memory: &common.MemoryConnectorConfig{},
	})
	require.NoError(t, err)

	// Create database strategy with cache enabled
	ttl := 30 * time.Second
	cfg := &common.DatabaseStrategyConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{},
		},
		Cache: &common.DatabaseStrategyCacheConfig{
			TTL:         &ttl,
			MaxSize:     &[]int64{100}[0],
			MaxCost:     &[]int64{1 << 20}[0], // 1MB
			NumCounters: &[]int64{1000}[0],
		},
	}

	strategy, err := NewDatabaseStrategy(ctx, &logger, cfg)
	require.NoError(t, err)
	require.NotNil(t, strategy.cache)
	defer strategy.Close()

	// Insert test data
	testAPIKey := "test-api-key-123"
	testUserData := map[string]interface{}{
		"userId":             "user-123",
		"perSecondRateLimit": int64(100),
		"enabled":            true,
	}
	testUserDataBytes, err := json.Marshal(testUserData)
	require.NoError(t, err)

	err = memConnector.Set(ctx, testAPIKey, "*", testUserDataBytes, nil)
	require.NoError(t, err)

	// First authentication should hit database and cache result
	payload := &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: testAPIKey},
	}

	user1, err := strategy.Authenticate(ctx, payload)
	require.NoError(t, err)
	require.NotNil(t, user1)
	assert.Equal(t, "user-123", user1.Id)
	assert.Equal(t, int64(100), user1.PerSecondRateLimit)

	// Second authentication should hit cache (verify by checking cache directly)
	cachedUser, found := strategy.cache.Get(testAPIKey)
	assert.True(t, found)
	assert.Equal(t, "user-123", cachedUser.Id)
	assert.Equal(t, int64(100), cachedUser.PerSecondRateLimit)

	// Third authentication should use cached data
	user2, err := strategy.Authenticate(ctx, payload)
	require.NoError(t, err)
	require.NotNil(t, user2)
	assert.Equal(t, user1.Id, user2.Id)
	assert.Equal(t, user1.PerSecondRateLimit, user2.PerSecondRateLimit)
}

func TestDatabaseStrategy_CacheDisabled(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	// Create database strategy with cache disabled (nil cache config)
	cfg := &common.DatabaseStrategyConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{},
		},
		Cache: nil, // Cache disabled
	}

	strategy, err := NewDatabaseStrategy(ctx, &logger, cfg)
	require.NoError(t, err)
	assert.Nil(t, strategy.cache)

	// Test that authentication still works without cache
	testAPIKey := "test-api-key-456"
	testUserData := map[string]interface{}{
		"userId":  "user-456",
		"enabled": true,
	}
	testUserDataBytes, err := json.Marshal(testUserData)
	require.NoError(t, err)

	err = strategy.GetConnector().Set(ctx, testAPIKey, "*", testUserDataBytes, nil)
	require.NoError(t, err)

	payload := &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: testAPIKey},
	}

	user, err := strategy.Authenticate(ctx, payload)
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "user-456", user.Id)
}

func TestDatabaseStrategy_CacheInvalidation(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	// Create database strategy with cache enabled
	ttl := 30 * time.Second
	cfg := &common.DatabaseStrategyConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{},
		},
		Cache: &common.DatabaseStrategyCacheConfig{
			TTL:         &ttl,
			MaxSize:     &[]int64{100}[0],
			MaxCost:     &[]int64{1 << 20}[0],
			NumCounters: &[]int64{1000}[0],
		},
	}

	strategy, err := NewDatabaseStrategy(ctx, &logger, cfg)
	require.NoError(t, err)
	defer strategy.Close()

	// Insert and authenticate to cache the result
	testAPIKey := "test-api-key-789"
	testUserData := map[string]interface{}{
		"userId":  "user-789",
		"enabled": true,
	}
	testUserDataBytes, err := json.Marshal(testUserData)
	require.NoError(t, err)

	err = strategy.GetConnector().Set(ctx, testAPIKey, "*", testUserDataBytes, nil)
	require.NoError(t, err)

	payload := &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: testAPIKey},
	}

	_, err = strategy.Authenticate(ctx, payload)
	require.NoError(t, err)

	// Verify it's cached
	_, found := strategy.cache.Get(testAPIKey)
	assert.True(t, found)

	// Invalidate cache entry
	strategy.InvalidateCache(testAPIKey)

	// Verify it's no longer cached
	_, found = strategy.cache.Get(testAPIKey)
	assert.False(t, found)

	// Test clear cache
	_, err = strategy.Authenticate(ctx, payload) // Cache it again
	require.NoError(t, err)

	strategy.ClearCache()
	_, found = strategy.cache.Get(testAPIKey)
	assert.False(t, found)
}

func TestDatabaseStrategy_CacheDisabledAPI(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	// Create database strategy with cache disabled
	cfg := &common.DatabaseStrategyConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{},
		},
		Cache: nil,
	}

	strategy, err := NewDatabaseStrategy(ctx, &logger, cfg)
	require.NoError(t, err)

	// Test that invalidation and clear methods work even when cache is disabled
	strategy.InvalidateCache("test-key") // Should not panic
	strategy.ClearCache()                // Should not panic
	strategy.Close()                     // Should not panic
}
