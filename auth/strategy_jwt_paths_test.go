package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/golang-jwt/jwt/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pemPublicKey(t *testing.T, blockType string, pub interface{}) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}))
}

func authenticateJwt(s *JwtStrategy, token string) (*common.User, error) {
	return s.Authenticate(context.Background(), nil, &AuthPayload{Type: common.AuthTypeJwt, Jwt: &JwtPayload{Token: token}})
}

func TestJwtStrategy_ClaimValidation(t *testing.T) {
	cfg := &common.JwtStrategyConfig{
		VerificationKeys:         map[string]string{"default": testJwtHMACSecret},
		AllowedAlgorithms:        []string{"HS256"},
		AllowedIssuers:           []string{"https://issuer.example"},
		AllowedAudiences:         []string{"erpc"},
		RequiredClaims:           []string{"tenant"},
		RateLimitBudgetClaimName: "tier",
	}
	s := newTestJwtStrategy(t, cfg)
	assert.True(t, s.Supports(&AuthPayload{Type: common.AuthTypeJwt}))
	assert.False(t, s.Supports(&AuthPayload{Type: common.AuthTypeSecret}))

	base := func() jwt.MapClaims {
		return jwt.MapClaims{
			"sub":    "svc",
			"iss":    "https://issuer.example",
			"aud":    "erpc",
			"tenant": "t1",
			"tier":   "gold",
			"exp":    time.Now().Add(time.Hour).Unix(),
		}
	}

	t.Run("valid token carries budget from custom claim", func(t *testing.T) {
		user, err := authenticateJwt(s, signTestJWT(t, base()))
		require.NoError(t, err)
		assert.Equal(t, "svc", user.Id)
		assert.Equal(t, "gold", user.RateLimitBudget)
	})

	t.Run("empty budget claim is ignored", func(t *testing.T) {
		c := base()
		c["tier"] = ""
		user, err := authenticateJwt(s, signTestJWT(t, c))
		require.NoError(t, err)
		assert.Empty(t, user.RateLimitBudget)
	})

	rejects := []struct {
		name    string
		mutate  func(jwt.MapClaims)
		reason  string
		message string
	}{
		{name: "missing issuer", mutate: func(c jwt.MapClaims) { delete(c, "iss") }, reason: "claims_invalid", message: "'iss' issuer claim is missing"},
		{name: "foreign issuer", mutate: func(c jwt.MapClaims) { c["iss"] = "https://evil" }, reason: "claims_invalid", message: "the 'https://evil' issuer is not allowed"},
		{name: "missing audience", mutate: func(c jwt.MapClaims) { delete(c, "aud") }, reason: "claims_invalid", message: "'aud' audience claim is missing"},
		{name: "foreign audience", mutate: func(c jwt.MapClaims) { c["aud"] = "other" }, reason: "claims_invalid", message: "the 'other' audience is not allowed"},
		{name: "missing required claim", mutate: func(c jwt.MapClaims) { delete(c, "tenant") }, reason: "claims_invalid", message: "missing required claim: tenant"},
		{name: "non-string subject", mutate: func(c jwt.MapClaims) { c["sub"] = 42 }, reason: "missing_sub"},
		{name: "not-before in the future", mutate: func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() }, reason: "invalid_token"},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			_, err := authenticateJwt(s, signTestJWT(t, c))
			requireAuthReason(t, err, "jwt", tc.reason)
			if tc.message != "" {
				assert.Contains(t, err.Error(), tc.message)
			}
		})
	}

	t.Run("garbage token fails parsing", func(t *testing.T) {
		_, err := authenticateJwt(s, "not.a.jwt")
		requireAuthReason(t, err, "jwt", "parse_failed")
	})

	t.Run("algorithm outside allow list", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS512, base())
		signed, err := tok.SignedString([]byte(testJwtHMACSecret))
		require.NoError(t, err)
		_, err = authenticateJwt(s, signed)
		requireAuthReason(t, err, "jwt", "unexpected_algorithm")
	})

	t.Run("claims.Valid failure inside validateClaims", func(t *testing.T) {
		// jwt.Parse already rejects bad standard claims; validateClaims re-checks
		// them for direct callers.
		err := s.validateClaims(jwt.MapClaims{"exp": time.Now().Add(-time.Hour).Unix()})
		require.ErrorContains(t, err, "invalid standard claims")
	})

	t.Run("default budget claim name is rlm", func(t *testing.T) {
		plain := newTestJwtStrategy(t, &common.JwtStrategyConfig{VerificationKeys: map[string]string{"k": testJwtHMACSecret}})
		user, err := authenticateJwt(plain, signTestJWT(t, jwt.MapClaims{"sub": "a", "rlm": "silver"}))
		require.NoError(t, err)
		assert.Equal(t, "silver", user.RateLimitBudget)
	})
}

func TestJwtStrategy_PemKeysAndKeyTypes(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	dir := t.TempDir()
	rsaPath := filepath.Join(dir, "rsa.pem")
	require.NoError(t, os.WriteFile(rsaPath, []byte(pemPublicKey(t, "PUBLIC KEY", &rsaKey.PublicKey)), 0o600))

	s := newTestJwtStrategy(t, &common.JwtStrategyConfig{
		VerificationKeys: map[string]string{
			"rsa": "file://" + rsaPath,
			"ec":  pemPublicKey(t, "EC PUBLIC KEY", &ecKey.PublicKey),
		},
	})
	claims := func() jwt.MapClaims { return jwt.MapClaims{"sub": "k", "exp": time.Now().Add(time.Hour).Unix()} }

	t.Run("rsa key loaded from file verifies RS256 by kid", func(t *testing.T) {
		user, err := authenticateJwt(s, signTestRS256JWT(t, rsaKey, "rsa", claims()))
		require.NoError(t, err)
		assert.Equal(t, "k", user.Id)
	})
	t.Run("ec pem key verifies ES256 without kid via type fallback", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims())
		signed, err := tok.SignedString(ecKey)
		require.NoError(t, err)
		user, err := authenticateJwt(s, signed)
		require.NoError(t, err)
		assert.Equal(t, "k", user.Id)
	})
	t.Run("HMAC token finds no compatible asymmetric key", func(t *testing.T) {
		_, err := authenticateJwt(s, signTestJWT(t, claims()))
		requireAuthReason(t, err, "jwt", "no_verification_key")
	})
	t.Run("unsupported signing method finds no key", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims())
		signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		require.NoError(t, err)
		_, err = authenticateJwt(s, signed)
		requireAuthReason(t, err, "jwt", "no_verification_key")
	})

	t.Run("isCompatibleKeyType rejects erroring keyfunc", func(t *testing.T) {
		failing := func(*jwt.Token) (interface{}, error) { return nil, assert.AnError }
		assert.False(t, isCompatibleKeyType(failing, jwt.SigningMethodHS256))
	})

	t.Run("parseKey rejects non-string and missing file", func(t *testing.T) {
		_, err := parseKey(123)
		require.ErrorContains(t, err, "unsupported key type: int")
		_, err = parseKey("file://" + filepath.Join(dir, "missing.pem"))
		require.ErrorContains(t, err, "failed to read key file")
	})
}

func TestJwtStrategy_StartupFailsWithoutAnyKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	logger := zerolog.Nop()
	_, err := NewJwtStrategy(context.Background(), &logger, &common.JwtStrategyConfig{VerificationJwksUrl: srv.URL})
	require.ErrorContains(t, err, "JWKS endpoint returned status 503")
}

func TestNewJwksHTTPClient_InsecureSkipVerify(t *testing.T) {
	secure := newJwksHTTPClient(&common.JwtStrategyConfig{})
	assert.Nil(t, secure.Transport)

	insecure := newJwksHTTPClient(&common.JwtStrategyConfig{VerificationJwksTlsInsecureSkipVerify: true})
	tr, ok := insecure.Transport.(*http.Transport)
	require.True(t, ok)
	assert.True(t, tr.TLSClientConfig.InsecureSkipVerify)
	assert.Equal(t, defaultJwksHTTPTimeout, insecure.Timeout)
}

func TestParseJWKS_RejectsBadDocuments(t *testing.T) {
	b64 := base64.RawURLEncoding.EncodeToString
	logger := zerolog.Nop()
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "invalid json", body: "{", wantErr: "failed to parse JWKS JSON"},
		{name: "no keys", body: `{"keys":[]}`, wantErr: "contains no keys"},
		{
			name: "only unusable keys",
			body: mustJSON(t, jwksDocument{Keys: []jwksKey{
				{Kty: "RSA", Kid: ""},                                          // no kid
				{Kty: "oct", Kid: "sym"},                                       // unsupported type
				{Kty: "RSA", Kid: "bad-n", N: "!!", E: "AQAB"},                 // bad modulus
				{Kty: "RSA", Kid: "bad-e", N: "AQAB", E: "!!"},                 // bad exponent
				{Kty: "RSA", Kid: "big-e", N: "AQAB", E: b64(make([]byte, 9))}, // exponent too large
				{Kty: "RSA", Kid: "zero-e", N: "AQAB", E: b64([]byte{0})},      // zero exponent
				{Kty: "EC", Kid: "bad-crv", Crv: "P-999"},
				{Kty: "EC", Kid: "bad-x", Crv: "P-256", X: "!!", Y: "AA"},
				{Kty: "EC", Kid: "bad-y", Crv: "P-384", X: "AA", Y: "!!"},
				{Kty: "EC", Kid: "off-curve", Crv: "P-521", X: b64([]byte{1}), Y: b64([]byte{1})},
			}}),
			wantErr: "contains no usable keys",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := parseJWKS([]byte(tc.body), &logger)
			require.ErrorContains(t, err, tc.wantErr)
			assert.Nil(t, keys)
		})
	}

	t.Run("key alg pin rejects tokens signed with another alg", func(t *testing.T) {
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		keys, err := parseJWKS([]byte(mustJSON(t, jwksDocument{Keys: []jwksKey{rsaJWKFromPrivateKey(t, rsaKey, "pinned")}})), nil)
		require.NoError(t, err)
		_, err = keys["pinned"](&jwt.Token{Method: jwt.SigningMethodRS512})
		require.ErrorContains(t, err, `requires alg "RS256"`)
	})
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestFetchVerificationKeysFromJWKS_TransportErrors(t *testing.T) {
	_, err := fetchVerificationKeysFromJWKS(context.Background(), http.DefaultClient, "://bad-url", nil)
	require.ErrorContains(t, err, "failed to create JWKS request")

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	_, err = fetchVerificationKeysFromJWKS(context.Background(), http.DefaultClient, url, nil)
	require.ErrorContains(t, err, "failed to fetch JWKS")
}

func TestJwtStrategy_ScheduledRefreshKeepsStaleKeysOnFailure(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	doc := mustJSON(t, jwksDocument{Keys: []jwksKey{rsaJWKFromPrivateKey(t, rsaKey, "kid-1")}})

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			_, _ = w.Write([]byte(doc))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := zerolog.Nop()
	s, err := NewJwtStrategy(ctx, &logger, &common.JwtStrategyConfig{
		VerificationJwksUrl:             srv.URL,
		VerificationJwksRefreshInterval: common.Duration(5 * time.Millisecond),
	})
	require.NoError(t, err)

	// Lift the post-startup debounce so the ticker performs a real (failing) fetch.
	s.refreshMu.Lock()
	s.nextRefresh = time.Time{}
	s.refreshMu.Unlock()

	require.Eventually(t, func() bool { return hits.Load() >= 2 }, 5*time.Second, 5*time.Millisecond)

	user, err := authenticateJwt(s, signTestRS256JWT(t, rsaKey, "kid-1", jwt.MapClaims{"sub": "still-ok"}))
	require.NoError(t, err)
	assert.Equal(t, "still-ok", user.Id)
}
