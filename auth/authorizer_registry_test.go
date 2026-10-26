package auth

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRateLimiters(t *testing.T, budgets ...*common.RateLimitBudgetConfig) *upstream.RateLimitersRegistry {
	t.Helper()
	logger := zerolog.Nop()
	rlr, err := upstream.NewRateLimitersRegistry(context.Background(), &common.RateLimiterConfig{
		Store:   &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: budgets,
	}, &logger)
	require.NoError(t, err)
	return rlr
}

func onePerMinuteBudget(id string) *common.RateLimitBudgetConfig {
	return &common.RateLimitBudgetConfig{
		Id:    id,
		Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 1, Period: common.RateLimitPeriodMinute}},
	}
}

func newRequest(method string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"` + method + `","params":[],"id":1}`))
}

func TestNewAuthorizer_ConfigValidation(t *testing.T) {
	logger := zerolog.Nop()
	tests := []struct {
		name    string
		cfg     *common.AuthStrategyConfig
		wantErr string
	}{
		{name: "nil config", cfg: nil, wantErr: "auth strategy config is nil"},
		{name: "secret without block", cfg: &common.AuthStrategyConfig{Type: common.AuthTypeSecret}, wantErr: "secret strategy config is nil"},
		{name: "jwt without block", cfg: &common.AuthStrategyConfig{Type: common.AuthTypeJwt}, wantErr: "JWT strategy config is nil"},
		{name: "siwe without block", cfg: &common.AuthStrategyConfig{Type: common.AuthTypeSiwe}, wantErr: "SIWE strategy config is nil"},
		{name: "network without block", cfg: &common.AuthStrategyConfig{Type: common.AuthTypeNetwork}, wantErr: "network strategy config is nil"},
		{name: "database without block", cfg: &common.AuthStrategyConfig{Type: common.AuthTypeDatabase}, wantErr: "database strategy config is nil"},
		{name: "unknown type", cfg: &common.AuthStrategyConfig{Type: "magic"}, wantErr: "unknown auth strategy type: magic"},
		{
			name:    "network with invalid ip",
			cfg:     &common.AuthStrategyConfig{Type: common.AuthTypeNetwork, Network: &common.NetworkStrategyConfig{AllowedIPs: []string{"x"}}},
			wantErr: "invalid IP address",
		},
		{
			name:    "jwt with unusable key",
			cfg:     &common.AuthStrategyConfig{Type: common.AuthTypeJwt, Jwt: &common.JwtStrategyConfig{VerificationKeys: map[string]string{"k": "file:///nonexistent/erpc-test-key.pem"}}},
			wantErr: "failed to read key file",
		},
		{
			name:    "database without connector",
			cfg:     &common.AuthStrategyConfig{Type: common.AuthTypeDatabase, Database: &common.DatabaseStrategyConfig{}},
			wantErr: "connector config is nil",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			az, err := NewAuthorizer(context.Background(), &logger, "p", tc.cfg, nil, 0)
			require.ErrorContains(t, err, tc.wantErr)
			assert.Nil(t, az)
		})
	}

	t.Run("each supported type builds its strategy", func(t *testing.T) {
		cfgs := map[common.AuthType]*common.AuthStrategyConfig{
			common.AuthTypeSecret:  {Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "a", Value: "b"}},
			common.AuthTypeJwt:     {Type: common.AuthTypeJwt, Jwt: &common.JwtStrategyConfig{VerificationKeys: map[string]string{"k": "hmac"}}},
			common.AuthTypeSiwe:    {Type: common.AuthTypeSiwe, Siwe: &common.SiweStrategyConfig{}},
			common.AuthTypeNetwork: {Type: common.AuthTypeNetwork, Network: &common.NetworkStrategyConfig{AllowedCIDRs: []string{"10.0.0.0/8"}}},
			common.AuthTypeDatabase: {Type: common.AuthTypeDatabase, Database: &common.DatabaseStrategyConfig{
				Connector: &common.ConnectorConfig{Id: "mem", Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 10, MaxTotalSize: "1MB"}},
			}},
		}
		for typ, cfg := range cfgs {
			az, err := NewAuthorizer(context.Background(), &logger, "p", cfg, nil, 3)
			require.NoError(t, err, typ)
			assert.True(t, az.strategy.Supports(&AuthPayload{Type: typ}), typ)
			assert.Equal(t, 3, az.index)
		}
	})
}

func TestAuthorizer_ShouldApplyToMethod(t *testing.T) {
	logger := zerolog.Nop()
	tests := []struct {
		name   string
		ignore []string
		allow  []string
		method string
		want   bool
	}{
		{name: "no lists applies to all", method: "eth_call", want: true},
		{name: "ignored exactly", ignore: []string{"eth_call"}, method: "eth_call", want: false},
		{name: "ignored by wildcard", ignore: []string{"debug_*"}, method: "debug_traceCall", want: false},
		{name: "not ignored", ignore: []string{"debug_*"}, method: "eth_call", want: true},
		{name: "allow overrides ignore-all", ignore: []string{"*"}, allow: []string{"eth_getLogs"}, method: "eth_getLogs", want: true},
		{name: "ignore-all still blocks other methods", ignore: []string{"*"}, allow: []string{"eth_getLogs"}, method: "eth_call", want: false},
		{name: "allow list alone keeps default apply", allow: []string{"eth_getLogs"}, method: "eth_call", want: true},
		{name: "invalid ignore pattern is skipped", ignore: []string{"(eth_call", "eth_call"}, method: "eth_call", want: false},
		{name: "invalid allow pattern is skipped", ignore: []string{"*"}, allow: []string{"(eth_call", "eth_call"}, method: "eth_call", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			az := &Authorizer{logger: &logger, cfg: &common.AuthStrategyConfig{IgnoreMethods: tc.ignore, AllowMethods: tc.allow}}
			assert.Equal(t, tc.want, az.shouldApplyToMethod(tc.method))
		})
	}
}

func TestAuthRegistry_RateLimitBudgets(t *testing.T) {
	logger := zerolog.Nop()

	t.Run("strategy budget is enforced per authorizer", func(t *testing.T) {
		rlr := newTestRateLimiters(t, onePerMinuteBudget("strategy-budget"))
		reg, err := NewAuthRegistry(context.Background(), &logger, "proj", &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{{
				Type:            common.AuthTypeSecret,
				RateLimitBudget: "strategy-budget",
				Secret:          &common.SecretStrategyConfig{Id: "alice", Value: "s"},
			}},
		}, rlr)
		require.NoError(t, err)

		ap := &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "s"}}
		req := newRequest("eth_call")
		req.SetClientIP("1.2.3.4")
		user, err := reg.Authenticate(context.Background(), req, "eth_call", ap)
		require.NoError(t, err)
		assert.Equal(t, "alice", user.Id)
		assert.Equal(t, "alice", req.UserId(), "authenticated user is attached to the request")

		req2 := newRequest("eth_call")
		req2.SetClientIP("1.2.3.4")
		user, err = reg.Authenticate(context.Background(), req2, "eth_call", ap)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthRateLimitRuleExceeded))
		require.NotNil(t, user, "rate-limited caller is still identified")
		var rlErr *common.ErrAuthRateLimitRuleExceeded
		require.ErrorAs(t, err, &rlErr)
		assert.Equal(t, "strategy-budget", rlErr.Details["budget"])
		assert.Equal(t, "alice", rlErr.Details["userId"])
		assert.Equal(t, "1.2.3.4", rlErr.Details["clientIp"])
		assert.Equal(t, "method:eth_call", rlErr.Details["rule"])
	})

	t.Run("user budget overrides strategy budget", func(t *testing.T) {
		rlr := newTestRateLimiters(t, onePerMinuteBudget("tight"), &common.RateLimitBudgetConfig{
			Id:    "roomy",
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 1000, Period: common.RateLimitPeriodMinute}},
		})
		reg, err := NewAuthRegistry(context.Background(), &logger, "proj", &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{{
				Type:            common.AuthTypeSecret,
				RateLimitBudget: "tight",
				Secret:          &common.SecretStrategyConfig{Id: "vip", Value: "s", RateLimitBudget: "roomy"},
			}},
		}, rlr)
		require.NoError(t, err)
		ap := &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "s"}}
		for i := range 3 {
			user, err := reg.Authenticate(context.Background(), newRequest("eth_call"), "eth_call", ap)
			require.NoError(t, err, "attempt %d", i)
			assert.Equal(t, "roomy", user.RateLimitBudget)
		}
	})

	t.Run("unknown budget surfaces budget-not-found", func(t *testing.T) {
		rlr := newTestRateLimiters(t)
		reg, err := NewAuthRegistry(context.Background(), &logger, "proj", &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{{
				Type:            common.AuthTypeSecret,
				RateLimitBudget: "missing",
				Secret:          &common.SecretStrategyConfig{Id: "u", Value: "s"},
			}},
		}, rlr)
		require.NoError(t, err)
		_, err = reg.Authenticate(context.Background(), newRequest("eth_call"), "eth_call",
			&AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "s"}})
		assert.True(t, common.HasErrorCode(err, "ErrRateLimitBudgetNotFound"), "got %v", err)
	})

	t.Run("nil request uses strategy budget and n/a labels", func(t *testing.T) {
		rlr := newTestRateLimiters(t, onePerMinuteBudget("nilreq"))
		az, err := NewAuthorizer(context.Background(), &logger, "proj", &common.AuthStrategyConfig{
			Type:            common.AuthTypeSecret,
			RateLimitBudget: "nilreq",
			Secret:          &common.SecretStrategyConfig{Id: "u", Value: "s"},
		}, rlr, 0)
		require.NoError(t, err)
		require.NoError(t, az.acquireRateLimitPermit(context.Background(), nil, "eth_call"))
		err = az.acquireRateLimitPermit(context.Background(), nil, "eth_call")
		var rlErr *common.ErrAuthRateLimitRuleExceeded
		require.ErrorAs(t, err, &rlErr)
		assert.Equal(t, "n/a", rlErr.Details["userId"])
		assert.Equal(t, "n/a", rlErr.Details["clientIp"])
	})

	t.Run("no budget never consults the limiter", func(t *testing.T) {
		az := &Authorizer{cfg: &common.AuthStrategyConfig{Type: common.AuthTypeSecret}}
		assert.NoError(t, az.acquireRateLimitPermit(context.Background(), newRequest("eth_call"), "eth_call"))
	})
}

func TestAuthRegistry_Authenticate_Routing(t *testing.T) {
	logger := zerolog.Nop()

	t.Run("nil config is rejected", func(t *testing.T) {
		_, err := NewAuthRegistry(context.Background(), &logger, "p", nil, nil)
		require.ErrorContains(t, err, "auth config is nil")
	})

	t.Run("invalid strategy fails registry creation", func(t *testing.T) {
		_, err := NewAuthRegistry(context.Background(), &logger, "p", &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{{Type: common.AuthTypeSecret}},
		}, nil)
		require.ErrorContains(t, err, "failed to create authorizer for project p with strategy secret")
	})

	t.Run("nil payload is unauthorized", func(t *testing.T) {
		reg := newSecretAuthRegistry(t, &common.AuthConfig{})
		_, err := reg.Authenticate(context.Background(), nil, "eth_call", nil)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
		assert.True(t, common.HasErrorCode(reg.AcceptsPayload(context.Background(), nil, nil), common.ErrCodeAuthUnauthorized))
	})

	t.Run("no strategies allows everything", func(t *testing.T) {
		reg := newSecretAuthRegistry(t, &common.AuthConfig{})
		user, err := reg.Authenticate(context.Background(), nil, "eth_call", &AuthPayload{Type: common.AuthTypeNetwork})
		require.NoError(t, err)
		assert.Nil(t, user)
		assert.NoError(t, reg.AcceptsPayload(context.Background(), nil, &AuthPayload{Type: common.AuthTypeNetwork}))
	})

	reg := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{
				Type:          common.AuthTypeSecret,
				IgnoreMethods: []string{"*"},
				AllowMethods:  []string{"eth_getLogs"},
				Secret:        &common.SecretStrategyConfig{Id: "logs-only", Value: "logs"},
			},
			{
				Type:    common.AuthTypeNetwork,
				Network: &common.NetworkStrategyConfig{AllowedCIDRs: []string{"10.0.0.0/8"}, IPAsUser: true},
			},
		},
	})

	t.Run("method scoping skips a strategy", func(t *testing.T) {
		ap := &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "logs"}}
		user, err := reg.Authenticate(context.Background(), newRequest("eth_getLogs"), "eth_getLogs", ap)
		require.NoError(t, err)
		assert.Equal(t, "logs-only", user.Id)

		_, err = reg.Authenticate(context.Background(), newRequest("eth_call"), "eth_call", ap)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no auth strategy matched")
	})

	t.Run("AcceptsPayload ignores method scoping", func(t *testing.T) {
		ap := &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "logs"}}
		assert.NoError(t, reg.AcceptsPayload(context.Background(), nil, ap))

		err := reg.AcceptsPayload(context.Background(), nil, &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "nope"}})
		requireAuthReason(t, err, "secret", "invalid_secret")

		err = reg.AcceptsPayload(context.Background(), nil, &AuthPayload{Type: common.AuthTypeJwt, Jwt: &JwtPayload{Token: "x"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no auth strategy matched")
	})

	t.Run("network strategy authenticates by client ip", func(t *testing.T) {
		req := newRequest("eth_call")
		req.SetClientIP("10.9.8.7")
		user, err := reg.Authenticate(context.Background(), req, "eth_call", &AuthPayload{Type: common.AuthTypeNetwork})
		require.NoError(t, err)
		assert.Equal(t, "10.9.8.7", user.Id)
		assert.Equal(t, "10.9.8.7", req.UserId())
	})
}

func TestAuthRegistry_FindDatabaseConnector(t *testing.T) {
	logger := zerolog.Nop()
	reg, err := NewAuthRegistry(context.Background(), &logger, "p", &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "a", Value: "b"}},
			{Type: common.AuthTypeDatabase, Database: &common.DatabaseStrategyConfig{
				Connector: &common.ConnectorConfig{Id: "keys-db", Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 10, MaxTotalSize: "1MB"}},
			}},
		},
	}, nil)
	require.NoError(t, err)

	conn, err := reg.FindDatabaseConnector("keys-db")
	require.NoError(t, err)
	assert.Equal(t, "keys-db", conn.Id())

	_, err = reg.FindDatabaseConnector("other")
	require.EqualError(t, err, "database connector with ID 'other' not found")
}

func TestDatabaseStrategy_MemoryConnectorEndToEnd(t *testing.T) {
	logger := zerolog.Nop()
	ttl := time.Minute
	maxSize, maxCost, counters := int64(100), int64(1<<20), int64(1000)
	s, err := NewDatabaseStrategy(context.Background(), &logger, &common.DatabaseStrategyConfig{
		Connector: &common.ConnectorConfig{Id: "mem", Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"}},
		Cache:     &common.DatabaseStrategyCacheConfig{TTL: &ttl, MaxSize: &maxSize, MaxCost: &maxCost, NumCounters: &counters},
		MaxWait:   common.Duration(time.Second),
	})
	require.NoError(t, err)
	t.Cleanup(s.Close)

	conn := s.GetConnector()
	ctx := context.Background()
	put := func(key, value string) {
		require.NoError(t, conn.Set(ctx, key, "*", []byte(value), nil))
	}
	put("good", `{"userId":"u1","rateLimitBudget":"gold"}`)
	put("disabled", `{"userId":"u2","enabled":false}`)
	put("garbage", `not-json`)
	put("noid", `{"enabled":true}`)
	// Ristretto writes are async; Get must observe them.
	require.Eventually(t, func() bool {
		_, err := conn.Get(ctx, data.ConnectorMainIndex, "noid", "*", nil)
		return err == nil
	}, 5*time.Second, 5*time.Millisecond)

	auth := func(key string) (*common.User, error) {
		return s.Authenticate(ctx, nil, &AuthPayload{Type: common.AuthTypeDatabase, Secret: &SecretPayload{Value: key}})
	}

	assert.True(t, s.Supports(&AuthPayload{Type: common.AuthTypeSecret}))
	assert.True(t, s.Supports(&AuthPayload{Type: common.AuthTypeDatabase}))
	assert.False(t, s.Supports(&AuthPayload{Type: common.AuthTypeJwt}))

	_, err = s.Authenticate(ctx, nil, &AuthPayload{Type: common.AuthTypeDatabase})
	requireAuthReason(t, err, "database", "missing_secret")
	_, err = auth("")
	requireAuthReason(t, err, "database", "empty_secret")

	user, err := auth("good")
	require.NoError(t, err)
	assert.Equal(t, "u1", user.Id)
	assert.Equal(t, "gold", user.RateLimitBudget)

	_, err = auth("disabled")
	requireAuthReason(t, err, "database", "disabled_key")
	_, err = auth("garbage")
	requireAuthReason(t, err, "database", "db_record_parse_error")
	_, err = auth("noid")
	requireAuthReason(t, err, "database", "db_record_missing_user_id")
	_, err = auth("unknown")
	requireAuthReason(t, err, "database", "invalid_api_key")

	// Positive cache: once cached, the user survives deletion from the store
	// until invalidated.
	require.Eventually(t, func() bool {
		_, found := s.cache.Get("good")
		return found
	}, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, conn.Delete(ctx, "good", "*"))
	user, err = auth("good")
	require.NoError(t, err)
	assert.Equal(t, "u1", user.Id, "cached user served without hitting the store")

	s.InvalidateCache("good")
	s.cache.Wait()
	_, err = auth("good")
	requireAuthReason(t, err, "database", "invalid_api_key")

	// Negative cache: a key that was unknown keeps failing from cache even
	// after it is created, until the negative entry expires.
	require.Eventually(t, func() bool {
		_, found := s.negCache.Get("unknown")
		return found
	}, 5*time.Second, 5*time.Millisecond)
	put("unknown", `{"userId":"late"}`)
	_, err = auth("unknown")
	requireAuthReason(t, err, "database", "cached_unknown_api_key")

	s.ClearCache()
}

func TestNewDatabaseStrategy_Errors(t *testing.T) {
	logger := zerolog.Nop()
	_, err := NewDatabaseStrategy(context.Background(), &logger, nil)
	require.ErrorContains(t, err, "database strategy config is nil")
	_, err = NewDatabaseStrategy(context.Background(), &logger, &common.DatabaseStrategyConfig{})
	require.ErrorContains(t, err, "connector config is nil")
	_, err = NewDatabaseStrategy(context.Background(), &logger, &common.DatabaseStrategyConfig{
		Connector: &common.ConnectorConfig{Id: "m", Driver: common.DriverMemory, Memory: &common.MemoryConnectorConfig{MaxItems: 0}},
	})
	require.ErrorContains(t, err, "failed to create database connector")
}
