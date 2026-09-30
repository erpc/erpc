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

// authenticateRoles runs a token carrying `roles` through the real registry
// and returns the resolved caller.
func authenticateRoles(t *testing.T, strategies []*common.AuthStrategyConfig, roles ...string) *common.User {
	t.Helper()
	logger := zerolog.Nop()
	reg, err := NewAuthRegistry(context.Background(), &logger, "prj", &common.AuthConfig{Strategies: strategies}, nil)
	require.NoError(t, err)

	claimRoles := make([]interface{}, len(roles))
	for i, r := range roles {
		claimRoles[i] = r
	}
	token := signTestJWT(t, jwt.MapClaims{
		"sub":   "caller",
		"roles": claimRoles,
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	user, err := reg.Authenticate(context.Background(), nil, "eth_call", &AuthPayload{
		Type: common.AuthTypeJwt,
		Jwt:  &JwtPayload{Token: token},
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	return user
}

// oneStrategyForEveryRole is the shape an operator with a single identity
// provider actually writes: ONE strategy holding the JWKS/issuer config, with
// grades granted from the role claim they already use for access control.
func oneStrategyForEveryRole() []*common.AuthStrategyConfig {
	baseline := []string{"standard"}
	return []*common.AuthStrategyConfig{
		{
			Type:              common.AuthTypeJwt,
			ConsensusPolicies: &baseline,
			Jwt: &common.JwtStrategyConfig{
				VerificationKeys:  map[string]string{"default": testJwtHMACSecret},
				AllowedAlgorithms: []string{"HS256"},
				ClaimMatchers:     map[string][]string{"roles": {"erpc:all"}},
				ConsensusPoliciesByClaim: map[string]map[string][]string{
					"roles": {"erpc:consensus-fallback": {"degraded"}},
				},
			},
		},
	}
}

func TestConsensusPolicies_GrantedFromClaims(t *testing.T) {
	cfg := oneStrategyForEveryRole()

	t.Run("baseline role gets only the strict grade", func(t *testing.T) {
		user := authenticateRoles(t, cfg, "erpc:all")
		assert.True(t, user.MayBeServedConsensusPolicy("standard"))
		assert.False(t, user.MayBeServedConsensusPolicy("degraded"),
			"a caller without the fallback role must not be served the relaxed grade")
	})

	t.Run("fallback role adds the relaxed grade on top of the baseline", func(t *testing.T) {
		user := authenticateRoles(t, cfg, "erpc:all", "erpc:consensus-fallback")
		assert.True(t, user.MayBeServedConsensusPolicy("standard"), "grants are additive, not replacing")
		assert.True(t, user.MayBeServedConsensusPolicy("degraded"))
	})

	// The reason this exists at all: with one strategy per role, a token
	// holding several roles resolved to whichever strategy was listed first,
	// so the grade depended on config order. Claim grants are a union, so
	// role order in the token — and strategy order in the config — cannot
	// change the outcome.
	t.Run("multi-role grant is order-independent", func(t *testing.T) {
		a := authenticateRoles(t, cfg, "erpc:all", "erpc:consensus-fallback")
		b := authenticateRoles(t, cfg, "erpc:consensus-fallback", "erpc:all")
		assert.Equal(t, *a.ConsensusPolicies, *b.ConsensusPolicies,
			"the same roles in a different order must grant the same grades")
	})

	t.Run("unknown role grants nothing beyond the baseline", func(t *testing.T) {
		user := authenticateRoles(t, cfg, "erpc:all", "erpc:some-unrelated-role")
		assert.True(t, user.MayBeServedConsensusPolicy("standard"))
		assert.False(t, user.MayBeServedConsensusPolicy("degraded"))
	})

	t.Run("no claim mapping configured leaves the caller unrestricted", func(t *testing.T) {
		// Existing configs must be unaffected: without either field the
		// caller may be served any grade, exactly as before.
		plain := []*common.AuthStrategyConfig{{
			Type: common.AuthTypeJwt,
			Jwt: &common.JwtStrategyConfig{
				VerificationKeys:  map[string]string{"default": testJwtHMACSecret},
				AllowedAlgorithms: []string{"HS256"},
			},
		}}
		user := authenticateRoles(t, plain, "erpc:all")
		assert.Nil(t, user.ConsensusPolicies)
		assert.True(t, user.MayBeServedConsensusPolicy("anything"))
	})

	t.Run("claim mapping configured but no role matches permits nothing", func(t *testing.T) {
		// Distinct from "unset": the operator DID scope grades by role, so a
		// caller whose roles grant none must not fall through to unrestricted.
		scoped := []*common.AuthStrategyConfig{{
			Type: common.AuthTypeJwt,
			Jwt: &common.JwtStrategyConfig{
				VerificationKeys:  map[string]string{"default": testJwtHMACSecret},
				AllowedAlgorithms: []string{"HS256"},
				ConsensusPoliciesByClaim: map[string]map[string][]string{
					"roles": {"erpc:consensus-fallback": {"degraded"}},
				},
			},
		}}
		user := authenticateRoles(t, scoped, "erpc:all")
		require.NotNil(t, user.ConsensusPolicies)
		assert.Empty(t, *user.ConsensusPolicies)
		assert.False(t, user.MayBeServedConsensusPolicy("standard"))
	})
}
