package auth

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func authFailedCount(strategy, reason string) float64 {
	return testutil.ToFloat64(telemetry.MetricAuthFailedTotal.WithLabelValues(
		"n/a", "n/a", strategy, reason, "unknown",
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
		before := authFailedCount("secret", "invalid_secret")
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
		user, err := registry.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
			Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "token-b"},
		})
		require.NoError(t, err)
		require.Equal(t, "b", user.Id)
		assert.Equal(t, before, authFailedCount("secret", "invalid_secret"))
	})

	t.Run("client denial meters once", func(t *testing.T) {
		beforeSecret := authFailedCount("secret", "invalid_secret")
		beforeJoined := authFailedCount("n/a", "unauthorized")
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
		_, err := registry.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
			Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "wrong"},
		})
		require.Error(t, err)
		assert.Equal(t, beforeSecret, authFailedCount("secret", "invalid_secret"))
		assert.Equal(t, beforeJoined, authFailedCount("n/a", "unauthorized"))
		RecordAuthFailed(req, err)
		// Two strategies failed → joined ErrAuthUnauthorized(strategy=n/a).
		assert.Equal(t, beforeJoined+1, authFailedCount("n/a", "unauthorized"))
		assert.Equal(t, beforeSecret, authFailedCount("secret", "invalid_secret"))
	})

	t.Run("single strategy denial keeps strategy reason labels", func(t *testing.T) {
		reg := newSecretAuthRegistry(t, &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{
				{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Id: "a", Value: "token-a"}},
			},
		})
		before := authFailedCount("secret", "invalid_secret")
		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
		_, err := reg.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
			Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "wrong"},
		})
		require.Error(t, err)
		RecordAuthFailed(req, err)
		assert.Equal(t, before+1, authFailedCount("secret", "invalid_secret"))
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
	before := authFailedCount("n/a", "unauthorized")
	RecordAuthFailed(nil, common.NewErrInvalidRequest(context.Canceled))
	assert.Equal(t, before, authFailedCount("n/a", "unauthorized"))
}
