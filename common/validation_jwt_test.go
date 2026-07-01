package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJwtStrategyConfigValidateClaimMatchers(t *testing.T) {
	base := func(matchers map[string][]string) *JwtStrategyConfig {
		return &JwtStrategyConfig{
			VerificationKeys: map[string]string{"default": "secret"},
			ClaimMatchers:    matchers,
		}
	}

	t.Run("roles matcher is accepted", func(t *testing.T) {
		require.NoError(t, base(map[string][]string{"roles": {"api:read"}}).Validate())
	})

	t.Run("omitted claimMatchers is accepted", func(t *testing.T) {
		require.NoError(t, base(nil).Validate())
	})

	t.Run("arbitrary claim is accepted", func(t *testing.T) {
		require.NoError(t, base(map[string][]string{"foo": {"bar"}}).Validate())
	})

	t.Run("empty value list is a startup error", func(t *testing.T) {
		err := base(map[string][]string{"roles": {}}).Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be empty")
	})

	t.Run("blank value is a startup error", func(t *testing.T) {
		err := base(map[string][]string{"roles": {" "}}).Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty value not allowed")
	})
}
