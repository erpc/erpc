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

func TestJwtStrategyConfigValidateVerificationJwksUrl(t *testing.T) {
	t.Run("https URL is accepted", func(t *testing.T) {
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "https://auth.example.com/.well-known/jwks.json"}
		require.NoError(t, cfg.Validate())
	})

	t.Run("http URL is accepted", func(t *testing.T) {
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "http://auth.localhost/jwks"}
		require.NoError(t, cfg.Validate())
	})

	t.Run("URL with path query and port is accepted", func(t *testing.T) {
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "https://auth.example.com:8443/keys?format=jwks"}
		require.NoError(t, cfg.Validate())
	})

	t.Run("jwks URL alone satisfies key requirement", func(t *testing.T) {
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "https://auth.example.com/jwks"}
		require.NoError(t, cfg.Validate())
	})

	t.Run("scheme-less host is rejected", func(t *testing.T) {
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "auth.example.com/jwks"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a valid HTTP or HTTPS URL")
	})

	t.Run("unsupported scheme is rejected", func(t *testing.T) {
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "file:///etc/jwks.json"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a valid HTTP or HTTPS URL")
	})

	t.Run("missing host is rejected", func(t *testing.T) {
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "https:///jwks"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a valid HTTP or HTTPS URL")
	})

	t.Run("port-only host is rejected", func(t *testing.T) {
		// url.Parse sets Host=":443" but Hostname() is empty.
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "https://:443/jwks"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a valid HTTP or HTTPS URL")
	})

	t.Run("whitespace-only URL falls back to keys requirement", func(t *testing.T) {
		cfg := &JwtStrategyConfig{VerificationJwksUrl: "   "}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verificationKeys or auth.*.jwt.verificationJwksUrl is required")
	})

	t.Run("invalid URL with static keys is still rejected", func(t *testing.T) {
		cfg := &JwtStrategyConfig{
			VerificationKeys:    map[string]string{"default": "secret"},
			VerificationJwksUrl: "not-a-url",
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a valid HTTP or HTTPS URL")
	})
}
