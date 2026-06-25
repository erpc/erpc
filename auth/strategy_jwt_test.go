package auth

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/golang-jwt/jwt/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testJwtHMACSecret = "test-jwt-hmac-secret"

func newTestJwtStrategy(t *testing.T, cfg *common.JwtStrategyConfig) *JwtStrategy {
	t.Helper()
	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err)
	return s
}

func signTestJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(testJwtHMACSecret))
	require.NoError(t, err)
	return signed
}

func TestJwtStrategyClaimMatchersRoles(t *testing.T) {
	baseCfg := &common.JwtStrategyConfig{
		VerificationKeys:  map[string]string{"default": testJwtHMACSecret},
		AllowedAlgorithms: []string{"HS256"},
		ClaimMatchers:     map[string][]string{"roles": {"api:read"}},
	}

	t.Run("accepts token with matching role in array", func(t *testing.T) {
		s := newTestJwtStrategy(t, baseCfg)
		token := signTestJWT(t, jwt.MapClaims{
			"sub":   "service-a",
			"roles": []interface{}{"other:role", "api:read"},
			"exp":   time.Now().Add(time.Hour).Unix(),
		})

		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: token},
		})
		require.NoError(t, err)
		assert.Equal(t, "service-a", user.Id)
	})

	t.Run("accepts token with matching role as string", func(t *testing.T) {
		s := newTestJwtStrategy(t, baseCfg)
		token := signTestJWT(t, jwt.MapClaims{
			"sub":   "service-a",
			"roles": "api:read",
			"exp":   time.Now().Add(time.Hour).Unix(),
		})

		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: token},
		})
		require.NoError(t, err)
	})

	t.Run("accepts token with SCIM-style role objects", func(t *testing.T) {
		s := newTestJwtStrategy(t, baseCfg)
		token := signTestJWT(t, jwt.MapClaims{
			"sub":   "service-a",
			"roles": []interface{}{map[string]interface{}{"value": "api:read", "display": "Read"}},
			"exp":   time.Now().Add(time.Hour).Unix(),
		})

		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: token},
		})
		require.NoError(t, err)
	})

	t.Run("rejects token without roles claim", func(t *testing.T) {
		s := newTestJwtStrategy(t, baseCfg)
		token := signTestJWT(t, jwt.MapClaims{
			"sub": "service-a",
			"exp": time.Now().Add(time.Hour).Unix(),
		})

		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: token},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "roles")
	})

	t.Run("rejects token with unrelated roles", func(t *testing.T) {
		s := newTestJwtStrategy(t, baseCfg)
		token := signTestJWT(t, jwt.MapClaims{
			"sub":   "service-b",
			"roles": []interface{}{"risk:admin"},
			"exp":   time.Now().Add(time.Hour).Unix(),
		})

		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: token},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "none of the token values are allowed")
	})

	t.Run("rejects token with empty roles array", func(t *testing.T) {
		s := newTestJwtStrategy(t, baseCfg)
		token := signTestJWT(t, jwt.MapClaims{
			"sub":   "service-a",
			"roles": []interface{}{},
			"exp":   time.Now().Add(time.Hour).Unix(),
		})

		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: token},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("skips role check when claimMatchers is omitted", func(t *testing.T) {
		cfg := &common.JwtStrategyConfig{
			VerificationKeys:  map[string]string{"default": testJwtHMACSecret},
			AllowedAlgorithms: []string{"HS256"},
		}
		s := newTestJwtStrategy(t, cfg)
		token := signTestJWT(t, jwt.MapClaims{
			"sub": "service-a",
			"exp": time.Now().Add(time.Hour).Unix(),
		})

		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: token},
		})
		require.NoError(t, err)
	})

	t.Run("accepts when any configured role matches", func(t *testing.T) {
		cfg := &common.JwtStrategyConfig{
			VerificationKeys:  map[string]string{"default": testJwtHMACSecret},
			AllowedAlgorithms: []string{"HS256"},
			ClaimMatchers:     map[string][]string{"roles": {"api:read", "api:admin"}},
		}
		s := newTestJwtStrategy(t, cfg)
		token := signTestJWT(t, jwt.MapClaims{
			"sub":   "ops-tool",
			"roles": []interface{}{"api:admin"},
			"exp":   time.Now().Add(time.Hour).Unix(),
		})

		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: token},
		})
		require.NoError(t, err)
	})

	t.Run("AND: all matchers must match", func(t *testing.T) {
		cfg := &common.JwtStrategyConfig{
			VerificationKeys:  map[string]string{"default": testJwtHMACSecret},
			AllowedAlgorithms: []string{"HS256"},
			ClaimMatchers:     map[string][]string{"roles": {"api:read"}, "aud": {"erpc"}},
		}
		s := newTestJwtStrategy(t, cfg)

		tokenMissingAud := signTestJWT(t, jwt.MapClaims{
			"sub":   "service-a",
			"roles": []interface{}{"api:read"},
			"exp":   time.Now().Add(time.Hour).Unix(),
		})
		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: tokenMissingAud},
		})
		require.Error(t, err)

		tokenBothMatch := signTestJWT(t, jwt.MapClaims{
			"sub":   "service-a",
			"roles": []interface{}{"api:read"},
			"aud":   "erpc",
			"exp":   time.Now().Add(time.Hour).Unix(),
		})
		_, err = s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: tokenBothMatch},
		})
		require.NoError(t, err)
	})

	t.Run("arbitrary claim string match", func(t *testing.T) {
		cfg := &common.JwtStrategyConfig{
			VerificationKeys:  map[string]string{"default": testJwtHMACSecret},
			AllowedAlgorithms: []string{"HS256"},
			ClaimMatchers:     map[string][]string{"foo": {"bar"}},
		}
		s := newTestJwtStrategy(t, cfg)

		tokenMatch := signTestJWT(t, jwt.MapClaims{
			"sub": "service-a",
			"foo": "bar",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: tokenMatch},
		})
		require.NoError(t, err)

		tokenNoMatch := signTestJWT(t, jwt.MapClaims{
			"sub": "service-a",
			"foo": "baz",
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		_, err = s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeJwt,
			Jwt:  &JwtPayload{Token: tokenNoMatch},
		})
		require.Error(t, err)
	})
}

func TestNormalizeClaimToStrings(t *testing.T) {
	t.Run("array of strings via interface slice", func(t *testing.T) {
		got, err := normalizeClaimToStrings([]interface{}{"a", "b"})
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b"}, got)
	})

	t.Run("single string", func(t *testing.T) {
		got, err := normalizeClaimToStrings("solo")
		require.NoError(t, err)
		assert.Equal(t, []string{"solo"}, got)
	})

	t.Run("SCIM-style objects extract value", func(t *testing.T) {
		got, err := normalizeClaimToStrings([]interface{}{
			map[string]interface{}{"value": "api:read", "display": "Read"},
			map[string]interface{}{"value": "api:admin"},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"api:read", "api:admin"}, got)
	})

	t.Run("rejects non-string array element", func(t *testing.T) {
		_, err := normalizeClaimToStrings([]interface{}{"ok", 42})
		require.Error(t, err)
	})

	t.Run("rejects object without string value", func(t *testing.T) {
		_, err := normalizeClaimToStrings([]interface{}{map[string]interface{}{"display": "no value"}})
		require.Error(t, err)
	})
}
