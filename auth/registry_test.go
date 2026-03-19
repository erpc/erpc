package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthRegistryAuthenticate_AllowsMatchingOriginForSecretStrategy(t *testing.T) {
	t.Parallel()

	registry := newTestAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{{
			Type: common.AuthTypeSecret,
			Secret: &common.SecretStrategyConfig{
				Id:             "secret-user",
				Value:          "secret-value",
				AllowedOrigins: []string{"https://app.example.com"},
			},
		}},
	}, nil)

	req := newOriginAwareRequest(http.Header{
		"Origin": []string{"https://app.example.com"},
	})

	user, err := registry.Authenticate(context.Background(), req, "eth_call", &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: "secret-value"},
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "secret-user", user.Id)
	assert.Equal(t, []string{"https://app.example.com"}, user.AllowedOrigins)
	assert.Equal(t, user, req.User())
}

func TestAuthRegistryAuthenticate_RejectsDisallowedOriginBeforeRateLimit(t *testing.T) {
	t.Parallel()

	logger := zerolog.New(io.Discard)
	rateLimiters, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{
			Driver: "memory",
		},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id: "auth-budget",
			Rules: []*common.RateLimitRuleConfig{{
				Method:   "*",
				MaxCount: 1,
				Period:   common.RateLimitPeriodMinute,
				PerUser:  true,
			}},
		}},
	}, &logger)
	require.NoError(t, err)

	registry := newTestAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{{
			Type: common.AuthTypeSecret,
			Secret: &common.SecretStrategyConfig{
				Id:             "secret-user",
				Value:          "secret-value",
				AllowedOrigins: []string{"https://app.example.com"},
			},
			RateLimitBudget: "auth-budget",
		}},
	}, rateLimiters)

	firstReq := newOriginAwareRequest(http.Header{
		"Origin": []string{"https://app.example.com"},
	})
	_, err = registry.Authenticate(context.Background(), firstReq, "eth_call", &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: "secret-value"},
	})
	require.NoError(t, err)

	secondReq := newOriginAwareRequest(http.Header{
		"Origin": []string{"https://evil.example.com"},
	})
	user, err := registry.Authenticate(context.Background(), secondReq, "eth_call", &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: "secret-value"},
	})
	require.Error(t, err)
	assert.NotNil(t, user)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
	assert.False(t, common.HasErrorCode(err, common.ErrCodeAuthRateLimitRuleExceeded))
}

func TestAuthRegistryAuthenticate_UsesRefererFallback(t *testing.T) {
	t.Parallel()

	registry := newTestAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{{
			Type: common.AuthTypeSecret,
			Secret: &common.SecretStrategyConfig{
				Id:             "secret-user",
				Value:          "secret-value",
				AllowedOrigins: []string{"https://app.example.com"},
			},
		}},
	}, nil)

	req := newOriginAwareRequest(http.Header{
		"Referer": []string{"https://app.example.com/dashboard?tab=rpc"},
	})

	user, err := registry.Authenticate(context.Background(), req, "eth_call", &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: "secret-value"},
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, "https://app.example.com", req.RequestOrigin())
}

func TestAuthRegistryAuthenticate_EmptyAllowedOriginsRemainUnrestricted(t *testing.T) {
	t.Parallel()

	registry := newTestAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{{
			Type: common.AuthTypeSecret,
			Secret: &common.SecretStrategyConfig{
				Id:    "secret-user",
				Value: "secret-value",
			},
		}},
	}, nil)

	req := newOriginAwareRequest(http.Header{
		"Origin": []string{"https://anywhere.example.com"},
	})

	user, err := registry.Authenticate(context.Background(), req, "eth_call", &AuthPayload{
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: "secret-value"},
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Empty(t, user.AllowedOrigins)
}

func TestDatabaseStrategyAuthenticate_LoadsAllowedOriginsFromRecordAndCacheInvalidation(t *testing.T) {
	t.Parallel()

	registry := newTestAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{{
			Type: common.AuthTypeDatabase,
			Database: &common.DatabaseStrategyConfig{
				Connector: &common.ConnectorConfig{
					Id:     "auth-db",
					Driver: common.DriverMemory,
					Memory: &common.MemoryConnectorConfig{
						MaxItems:     100,
						MaxTotalSize: "1MB",
					},
				},
				Cache: &common.DatabaseStrategyCacheConfig{
					TTL: durationPtr(time.Minute),
				},
			},
		}},
	}, nil)

	connector, err := registry.FindDatabaseConnector("auth-db")
	require.NoError(t, err)

	writeDatabaseUserRecord(t, connector, "db-secret", map[string]interface{}{
		"userId":          "db-user",
		"enabled":         true,
		"allowedOrigins":  []string{"https://app.example.com"},
		"rateLimitBudget": "db-budget",
	})

	allowedReq := newOriginAwareRequest(http.Header{
		"Origin": []string{"https://app.example.com"},
	})
	user, err := registry.Authenticate(context.Background(), allowedReq, "eth_call", &AuthPayload{
		Type:   common.AuthTypeDatabase,
		Secret: &SecretPayload{Value: "db-secret"},
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, []string{"https://app.example.com"}, user.AllowedOrigins)
	assert.Equal(t, "db-budget", user.RateLimitBudget)

	writeDatabaseUserRecord(t, connector, "db-secret", map[string]interface{}{
		"userId":         "db-user",
		"enabled":        true,
		"allowedOrigins": []string{"https://new.example.com"},
	})

	disallowedReq := newOriginAwareRequest(http.Header{
		"Origin": []string{"https://new.example.com"},
	})
	_, err = registry.Authenticate(context.Background(), disallowedReq, "eth_call", &AuthPayload{
		Type:   common.AuthTypeDatabase,
		Secret: &SecretPayload{Value: "db-secret"},
	})
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))

	authorizer := registry.strategies[0]
	dbStrategy, ok := authorizer.strategy.(*DatabaseStrategy)
	require.True(t, ok)
	dbStrategy.InvalidateCache("db-secret")

	refreshedReq := newOriginAwareRequest(http.Header{
		"Origin": []string{"https://new.example.com"},
	})
	user, err = registry.Authenticate(context.Background(), refreshedReq, "eth_call", &AuthPayload{
		Type:   common.AuthTypeDatabase,
		Secret: &SecretPayload{Value: "db-secret"},
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, []string{"https://new.example.com"}, user.AllowedOrigins)
}

func newTestAuthRegistry(t *testing.T, cfg *common.AuthConfig, rateLimiters *upstream.RateLimitersRegistry) *AuthRegistry {
	t.Helper()

	for _, strategy := range cfg.Strategies {
		require.NoError(t, strategy.SetDefaults())
	}

	logger := zerolog.New(io.Discard)
	registry, err := NewAuthRegistry(context.Background(), &logger, "test-project", cfg, rateLimiters)
	require.NoError(t, err)
	return registry
}

func newOriginAwareRequest(headers http.Header) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_call","params":[],"id":1}`))
	req.EnrichFromHttp(headers, nil, common.UserAgentTrackingModeSimplified)
	return req
}

func writeDatabaseUserRecord(t *testing.T, connector data.Connector, apiKey string, payload map[string]interface{}) {
	t.Helper()

	value, err := json.Marshal(payload)
	require.NoError(t, err)

	err = connector.Set(context.Background(), apiKey, "*", value, nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, getErr := connector.Get(context.Background(), data.ConnectorMainIndex, apiKey, "*", nil)
		return getErr == nil
	}, time.Second, 10*time.Millisecond)
}

func durationPtr(d time.Duration) *time.Duration {
	return &d
}
