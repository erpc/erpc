package auth

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/golang-jwt/jwt/v4"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func authFailedCount(project, strategy, reason string) float64 {
	return testutil.ToFloat64(telemetry.MetricAuthFailedTotal.WithLabelValues(
		project, "n/a", strategy, reason, "unknown",
	))
}

func TestRecordAuthFailed_MetersOncePerClientDenial(t *testing.T) {
	registry := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "a", Value: "token-a"}},
			{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "b", Value: "token-b"}},
		},
	})

	t.Run("successful second key does not meter", func(t *testing.T) {
		before := authFailedCount("n/a", "secret", "invalid_secret")
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
		user, err := registry.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
			Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "token-b"},
		})
		require.NoError(t, err)
		require.Equal(t, "b", user.Id)
		assert.Equal(t, before, authFailedCount("n/a", "secret", "invalid_secret"))
	})

	t.Run("client denial meters once", func(t *testing.T) {
		beforeSecret := authFailedCount("proj-a", "secret", "invalid_secret")
		beforeJoined := authFailedCount("proj-a", "n/a", "unauthorized")
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
		_, err := registry.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
			Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "wrong"},
		})
		require.Error(t, err)
		assert.Equal(t, beforeSecret, authFailedCount("proj-a", "secret", "invalid_secret"))
		assert.Equal(t, beforeJoined, authFailedCount("proj-a", "n/a", "unauthorized"))
		RecordAuthFailed("proj-a", err)
		// Two secret strategies both reject with invalid_secret → keep those labels.
		assert.Equal(t, beforeSecret+1, authFailedCount("proj-a", "secret", "invalid_secret"))
		assert.Equal(t, beforeJoined, authFailedCount("proj-a", "n/a", "unauthorized"))
	})

	t.Run("single strategy denial keeps strategy reason labels", func(t *testing.T) {
		reg := newSecretAuthRegistry(t, &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{
				{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "a", Value: "token-a"}},
			},
		})
		before := authFailedCount("proj-b", "secret", "invalid_secret")
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
		_, err := reg.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
			Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "wrong"},
		})
		require.Error(t, err)
		RecordAuthFailed("proj-b", err)
		assert.Equal(t, before+1, authFailedCount("proj-b", "secret", "invalid_secret"))
	})
}

func TestNewErrAuthUnauthorized_AttachesReason(t *testing.T) {
	s := NewSecretStrategy(&common.SecretStrategyConfig{Id: "ops", Value: "correct"})
	_, err := s.Authenticate(context.Background(), nil, &AuthPayload{
		Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "wrong"},
	})
	require.Error(t, err)
	var authErr *common.ErrAuthUnauthorized
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, "invalid_secret", string(authErr.Reason()))
}

func TestRecordAuthFailed_IgnoresNonAuthErrors(t *testing.T) {
	before := authFailedCount("n/a", "n/a", "unauthorized")
	RecordAuthFailed("n/a", common.NewErrInvalidRequest(context.Canceled))
	assert.Equal(t, before, authFailedCount("n/a", "n/a", "unauthorized"))
}

func TestRecordAuthFailed_UsesProjectId(t *testing.T) {
	err := common.NewErrAuthUnauthorizedReason(
		"jwt",
		common.AuthUnauthorizedReasonTokenExpired,
	)
	before := authFailedCount("arbitrum-mainnet", "jwt", "token_expired")
	RecordAuthFailed("arbitrum-mainnet", err)
	assert.Equal(t, before+1, authFailedCount("arbitrum-mainnet", "jwt", "token_expired"))
}

func TestUnauthorized_PreservesAgreedJwtTokenExpired(t *testing.T) {
	reg := newSecretAuthRegistry(t, &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{
				Type: common.AuthTypeJwt,
				Jwt: &common.JwtStrategyConfig{
					VerificationKeys:  map[string]string{"a": testJwtHMACSecret},
					AllowedAlgorithms: []string{"HS256"},
				},
			},
			{
				Type: common.AuthTypeJwt,
				Jwt: &common.JwtStrategyConfig{
					VerificationKeys:  map[string]string{"b": testJwtHMACSecret},
					AllowedAlgorithms: []string{"HS256"},
				},
			},
		},
	})
	token := signTestJWT(t, jwt.MapClaims{
		"sub": "service-a",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	_, err := reg.Authenticate(context.Background(), nil, "eth_chainId", &AuthPayload{
		Type: common.AuthTypeJwt,
		Jwt:  &JwtPayload{Token: token},
	})
	require.Error(t, err)
	var authErr *common.ErrAuthUnauthorized
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, "jwt", authErr.Details["strategy"])
	assert.Equal(t, common.AuthUnauthorizedReasonTokenExpired, authErr.Reason())

	before := authFailedCount("proj-jwt", "jwt", "token_expired")
	RecordAuthFailed("proj-jwt", err)
	assert.Equal(t, before+1, authFailedCount("proj-jwt", "jwt", "token_expired"))
}

func TestUnauthorized_PrefersTokenExpiredAmongJwtAttempts(t *testing.T) {
	errs := []error{
		common.NewErrAuthUnauthorizedReason("jwt", common.AuthUnauthorizedReasonInvalidSignature),
		common.NewErrAuthUnauthorizedReason("jwt", common.AuthUnauthorizedReasonTokenExpired),
	}
	err := unauthorized(errs)
	var authErr *common.ErrAuthUnauthorized
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, "jwt", authErr.Details["strategy"])
	assert.Equal(t, common.AuthUnauthorizedReasonTokenExpired, authErr.Reason())
}

func TestUnauthorized_DivergentNonExpiryReasonsStayUnauthorized(t *testing.T) {
	errs := []error{
		common.NewErrAuthUnauthorizedReason("jwt", common.AuthUnauthorizedReasonInvalidSignature),
		common.NewErrAuthUnauthorized("secret", "invalid_secret", "invalid secret"),
	}
	err := unauthorized(errs)
	var authErr *common.ErrAuthUnauthorized
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, "n/a", authErr.Details["strategy"])
	assert.Equal(t, common.AuthUnauthorizedReason("unauthorized"), authErr.Reason())
}
