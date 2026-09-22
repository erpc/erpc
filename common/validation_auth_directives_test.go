package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func authStrategyWithDirectives(pattern *string) *AuthStrategyConfig {
	return &AuthStrategyConfig{
		Type:                  AuthTypeSecret,
		AllowClientDirectives: pattern,
		Secret:                &SecretStrategyConfig{Id: "ops", Value: "ops-token"},
	}
}

// TestAuthStrategyConfig_Validate_AllowClientDirectives pins that a
// syntactically broken per-strategy directive pattern is caught at config load
// — the alternative is a matcher that silently fails to compile and a caller
// who quietly loses (or gains) directive access at runtime.
func TestAuthStrategyConfig_Validate_AllowClientDirectives(t *testing.T) {
	t.Run("unclosed group is rejected and names the offending field", func(t *testing.T) {
		pattern := "(use-upstream | retry-*"
		err := authStrategyWithDirectives(&pattern).Validate()

		require.Error(t, err)
		assert.Contains(t, err.Error(), "auth.*.allowClientDirectives")
		assert.Contains(t, err.Error(), "closing parenthesis",
			"the parser's reason should survive the wrap so the operator can fix the pattern")
	})

	t.Run("the error surfaces through the enclosing auth config", func(t *testing.T) {
		pattern := "!(skip-cache-read"
		err := (&AuthConfig{Strategies: []*AuthStrategyConfig{
			authStrategyWithDirectives(nil),
			authStrategyWithDirectives(&pattern),
		}}).Validate()

		require.Error(t, err)
		assert.Contains(t, err.Error(), "auth.*.allowClientDirectives")
	})

	t.Run("empty pattern means deny-all and is never parsed", func(t *testing.T) {
		pattern := ""
		require.NoError(t, authStrategyWithDirectives(&pattern).Validate())
	})

	t.Run("parseable patterns are accepted", func(t *testing.T) {
		for _, pattern := range []string{"*", "retry-*", "!skip-cache-read & !use-upstream", "!(skip-cache-read | use-upstream)"} {
			p := pattern
			assert.NoErrorf(t, authStrategyWithDirectives(&p).Validate(), "pattern %q should be accepted", pattern)
		}
	})
}
