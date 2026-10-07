package auth

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// fixedErrStrategy is a strategy that always fails with err.
type fixedErrStrategy struct{ err error }

func (s fixedErrStrategy) Supports(*AuthPayload) bool { return true }

func (s fixedErrStrategy) Authenticate(context.Context, *common.NormalizedRequest, *AuthPayload) (*common.User, error) {
	return nil, s.err
}

func registryOf(errs ...error) *AuthRegistry {
	logger := zerolog.Nop()
	r := &AuthRegistry{projectId: "test_project"}
	for i, err := range errs {
		r.strategies = append(r.strategies, &Authorizer{
			projectId: "test_project",
			logger:    &logger,
			cfg:       &common.AuthStrategyConfig{Type: common.AuthTypeSecret},
			strategy:  fixedErrStrategy{err: err},
			index:     i,
		})
	}
	return r
}

func authenticateWith(t *testing.T, r *AuthRegistry) error {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
	user, err := r.Authenticate(context.Background(), req, "eth_chainId", &AuthPayload{
		Method: "eth_chainId",
		Type:   common.AuthTypeSecret,
		Secret: &SecretPayload{Value: "token"},
	})
	require.Nil(t, user)
	require.Error(t, err)
	return err
}

// A genuine denial from any strategy is a verdict on the credentials, so a
// concurrent backend outage in another strategy must not soften the joined
// error into the Unavailable flavor (which keeps established WebSocket
// streams alive during re-auth).
func TestAuthRegistry_Authenticate_DenialWinsOverUnavailable(t *testing.T) {
	denied := common.NewErrAuthUnauthorized("secret", "invalid secret")
	unavailable := common.NewErrAuthUnavailable("database", "connector down", context.DeadlineExceeded)

	for _, order := range [][]error{{denied, unavailable}, {unavailable, denied}} {
		err := authenticateWith(t, registryOf(order...))
		require.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
		require.False(t, common.HasErrorCode(err, common.ErrCodeAuthUnavailable),
			"a denial from one strategy must not be reported as a backend outage: %v", err)
	}
}

// With no denial verdict at all, an outage stays distinguishable.
func TestAuthRegistry_Authenticate_OnlyUnavailableStaysUnavailable(t *testing.T) {
	a := common.NewErrAuthUnavailable("database", "connector down", context.DeadlineExceeded)
	b := common.NewErrAuthUnavailable("jwt", "jwks fetch failed", context.DeadlineExceeded)
	err := authenticateWith(t, registryOf(a, b))
	require.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized))
	require.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnavailable))
}
