package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/golang-jwt/jwt/v4"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rsaJWKFromPrivateKey(t *testing.T, privateKey *rsa.PrivateKey, kid string) jwksKey {
	t.Helper()
	return jwksKey{
		Kty: "RSA",
		Kid: kid,
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(bigIntToBytes(privateKey.E)),
	}
}

func bigIntToBytes(v int) []byte {
	if v == 0 {
		return []byte{0}
	}
	var out []byte
	for x := v; x > 0; x >>= 8 {
		out = append([]byte{byte(x & 0xff)}, out...)
	}
	return out
}

func signTestRS256JWT(t *testing.T, privateKey *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(privateKey)
	require.NoError(t, err)
	return signed
}

func ecJWKFromPrivateKey(t *testing.T, privateKey *ecdsa.PrivateKey, kid string) jwksKey {
	t.Helper()
	size := (privateKey.Curve.Params().BitSize + 7) / 8
	return jwksKey{
		Kty: "EC",
		Kid: kid,
		Alg: "ES256",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(leftPad(privateKey.X.Bytes(), size)),
		Y:   base64.RawURLEncoding.EncodeToString(leftPad(privateKey.Y.Bytes(), size)),
	}
}

func leftPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

func signTestES256JWT(t *testing.T, privateKey *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(privateKey)
	require.NoError(t, err)
	return signed
}

func TestParseJWKS(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	doc := jwksDocument{Keys: []jwksKey{rsaJWKFromPrivateKey(t, privateKey, "issuer-kid-1")}}
	body, err := json.Marshal(doc)
	require.NoError(t, err)

	keys, err := parseJWKS(body, nil)
	require.NoError(t, err)
	require.Contains(t, keys, "issuer-kid-1")

	token := signTestRS256JWT(t, privateKey, "issuer-kid-1", jwt.MapClaims{
		"sub": "service-a",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	_, err = jwt.Parse(token, keys["issuer-kid-1"])
	require.NoError(t, err)
}

func TestJwtStrategyVerificationJwksUrl(t *testing.T) {
	defer gock.Off()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "jwks-test-kid"
	jwksURL := "https://auth.example.com/.well-known/jwks.json"

	doc := jwksDocument{Keys: []jwksKey{rsaJWKFromPrivateKey(t, privateKey, kid)}}
	body, err := json.Marshal(doc)
	require.NoError(t, err)

	gock.New("https://auth.example.com").
		Get("/.well-known/jwks.json").
		Reply(200).
		JSON(json.RawMessage(body))

	cfg := &common.JwtStrategyConfig{
		VerificationJwksUrl: jwksURL,
		AllowedAlgorithms:   []string{"RS256"},
		AllowedIssuers:      []string{"https://issuer.example.com"},
		ClaimMatchers:       map[string][]string{"roles": {"api:read"}},
	}

	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err)

	token := signTestRS256JWT(t, privateKey, kid, jwt.MapClaims{
		"sub":   "service-a",
		"iss":   "https://issuer.example.com",
		"roles": []interface{}{"api:read"},
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
		Type: common.AuthTypeJwt,
		Jwt:  &JwtPayload{Token: token},
	})
	require.NoError(t, err)
	assert.Equal(t, "service-a", user.Id)
	assert.True(t, gock.IsDone())
}

func TestFetchVerificationKeysFromJWKSRequiresKeys(t *testing.T) {
	defer gock.Off()

	jwksURL := "https://auth.example.com/empty-jwks"
	gock.New("https://auth.example.com").
		Get("/empty-jwks").
		Reply(200).
		JSON(map[string]any{"keys": []any{}})

	_, err := fetchVerificationKeysFromJWKS(context.Background(), http.DefaultClient, jwksURL, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no keys")
}

func TestJwtStrategyStaticKeysOverrideJwksKid(t *testing.T) {
	defer gock.Off()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	kid := "shared-kid"
	jwksURL := "https://auth.example.com/.well-known/jwks.json"

	doc := jwksDocument{Keys: []jwksKey{rsaJWKFromPrivateKey(t, privateKey, kid)}}
	body, err := json.Marshal(doc)
	require.NoError(t, err)

	gock.New("https://auth.example.com").
		Get("/.well-known/jwks.json").
		Reply(200).
		JSON(json.RawMessage(body))

	cfg := &common.JwtStrategyConfig{
		VerificationJwksUrl: jwksURL,
		VerificationKeys:  map[string]string{"shared-kid": testJwtHMACSecret},
		AllowedAlgorithms: []string{"HS256"},
	}

	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "override-user",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = kid
	signed, err := token.SignedString([]byte(testJwtHMACSecret))
	require.NoError(t, err)

	user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
		Type: common.AuthTypeJwt,
		Jwt:  &JwtPayload{Token: signed},
	})
	require.NoError(t, err)
	assert.Equal(t, "override-user", user.Id)
}

func TestJwtStrategyForcedRefreshPicksUpRotatedKey(t *testing.T) {
	defer gock.Off()

	oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwksURL := "https://auth.example.com/.well-known/jwks.json"

	// Construction fetch: only the old key is published.
	oldDoc, err := json.Marshal(jwksDocument{Keys: []jwksKey{rsaJWKFromPrivateKey(t, oldKey, "kid-old")}})
	require.NoError(t, err)
	gock.New("https://auth.example.com").Get("/.well-known/jwks.json").Reply(200).JSON(json.RawMessage(oldDoc))

	cfg := &common.JwtStrategyConfig{
		VerificationJwksUrl: jwksURL,
		AllowedAlgorithms:   []string{"RS256"},
	}
	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err)

	// Upstream rotates: a new key is now published alongside the old one.
	newDoc, err := json.Marshal(jwksDocument{Keys: []jwksKey{
		rsaJWKFromPrivateKey(t, oldKey, "kid-old"),
		rsaJWKFromPrivateKey(t, newKey, "kid-new"),
	}})
	require.NoError(t, err)
	gock.New("https://auth.example.com").Get("/.well-known/jwks.json").Persist().Reply(200).JSON(json.RawMessage(newDoc))

	// Simulate time having passed beyond the debounce window so the on-demand
	// refresh inside Authenticate actually runs when it sees the unknown kid.
	s.nextRefresh = time.Time{}

	// A token signed by the new key (unknown kid) must trigger an on-demand
	// refresh and verify, rather than failing until the next scheduled cycle.
	token := signTestRS256JWT(t, newKey, "kid-new", jwt.MapClaims{
		"sub": "rotated-user",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
		Type: common.AuthTypeJwt,
		Jwt:  &JwtPayload{Token: token},
	})
	require.NoError(t, err)
	assert.Equal(t, "rotated-user", user.Id)
}

func TestJwtStrategyForcedRefreshDebounced(t *testing.T) {
	defer gock.Off()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwksURL := "https://auth.example.com/.well-known/jwks.json"

	doc, err := json.Marshal(jwksDocument{Keys: []jwksKey{rsaJWKFromPrivateKey(t, privateKey, "kid-1")}})
	require.NoError(t, err)
	gock.New("https://auth.example.com").Get("/.well-known/jwks.json").Persist().Reply(200).JSON(json.RawMessage(doc))

	cfg := &common.JwtStrategyConfig{VerificationJwksUrl: jwksURL, AllowedAlgorithms: []string{"RS256"}}
	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err)

	// First on-demand refresh runs; a second within the debounce window is skipped.
	s.nextRefresh = time.Time{}
	refreshed, err := s.refreshVerificationKeys(context.Background())
	require.NoError(t, err)
	assert.True(t, refreshed, "first forced refresh should run")

	refreshed, err = s.refreshVerificationKeys(context.Background())
	require.NoError(t, err)
	assert.False(t, refreshed, "second forced refresh within debounce window should be skipped")
}

func TestJwtStrategyForcedRefreshFailureAllowsRetrySooner(t *testing.T) {
	defer gock.Off()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwksURL := "https://auth.example.com/.well-known/jwks.json"

	// Construction succeeds with a valid key published.
	doc, err := json.Marshal(jwksDocument{Keys: []jwksKey{rsaJWKFromPrivateKey(t, privateKey, "kid-1")}})
	require.NoError(t, err)
	gock.New("https://auth.example.com").Get("/.well-known/jwks.json").Reply(200).JSON(json.RawMessage(doc))

	cfg := &common.JwtStrategyConfig{VerificationJwksUrl: jwksURL, AllowedAlgorithms: []string{"RS256"}}
	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err)

	// The endpoint now fails. Reset the debounce so the next call actually runs.
	s.nextRefresh = time.Time{}
	gock.New("https://auth.example.com").Get("/.well-known/jwks.json").Persist().Reply(500)

	refreshed, err := s.refreshVerificationKeys(context.Background())
	require.Error(t, err)
	require.False(t, refreshed, "a failed refresh should report that no refresh succeeded")

	// A failed refresh must not hold the full debounce window: the next attempt
	// is allowed after the shorter retry interval so rotation recovery isn't stalled.
	remaining := time.Until(s.nextRefresh)
	assert.LessOrEqual(t, remaining, refreshRetryAfter, "failed refresh should use the short retry window")
	assert.Less(t, remaining, refreshDebounce, "failed refresh must not hold the full debounce window")
}

func TestJwtStrategyVerificationJwksUrlES256(t *testing.T) {
	defer gock.Off()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	kid := "ec-test-kid"
	jwksURL := "https://auth.example.com/.well-known/jwks.json"

	doc, err := json.Marshal(jwksDocument{Keys: []jwksKey{ecJWKFromPrivateKey(t, privateKey, kid)}})
	require.NoError(t, err)
	gock.New("https://auth.example.com").Get("/.well-known/jwks.json").Reply(200).JSON(json.RawMessage(doc))

	cfg := &common.JwtStrategyConfig{
		VerificationJwksUrl: jwksURL,
		AllowedAlgorithms:   []string{"ES256"},
	}
	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err)

	token := signTestES256JWT(t, privateKey, kid, jwt.MapClaims{
		"sub": "ec-user",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
		Type: common.AuthTypeJwt,
		Jwt:  &JwtPayload{Token: token},
	})
	require.NoError(t, err)
	assert.Equal(t, "ec-user", user.Id)
}

func TestParseJWKSMixedRSAandEC(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	doc := jwksDocument{Keys: []jwksKey{
		rsaJWKFromPrivateKey(t, rsaKey, "rsa-kid"),
		ecJWKFromPrivateKey(t, ecKey, "ec-kid"),
	}}
	body, err := json.Marshal(doc)
	require.NoError(t, err)

	keys, err := parseJWKS(body, nil)
	require.NoError(t, err)
	require.Contains(t, keys, "rsa-kid")
	require.Contains(t, keys, "ec-kid")
}

func TestJwtStrategyStartupSurvivesJwksFailureWhenStaticKeysPresent(t *testing.T) {
	defer gock.Off()

	jwksURL := "https://auth.example.com/.well-known/jwks.json"
	gock.New("https://auth.example.com").Get("/.well-known/jwks.json").Reply(500)

	cfg := &common.JwtStrategyConfig{
		VerificationJwksUrl: jwksURL,
		VerificationKeys:    map[string]string{"default": testJwtHMACSecret},
		AllowedAlgorithms:   []string{"HS256"},
	}
	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err, "startup must not fail when static keys are available even if JWKS is unreachable")

	token := signTestJWT(t, jwt.MapClaims{
		"sub": "static-user",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
		Type: common.AuthTypeJwt,
		Jwt:  &JwtPayload{Token: token},
	})
	require.NoError(t, err)
	assert.Equal(t, "static-user", user.Id)
}

func TestJwtStrategyRejectsAlgorithmConfusion(t *testing.T) {
	defer gock.Off()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwksURL := "https://auth.example.com/.well-known/jwks.json"

	// JWKS publishes only an EC key, but the attacker presents an RS256 token.
	doc, err := json.Marshal(jwksDocument{Keys: []jwksKey{ecJWKFromPrivateKey(t, ecKey, "ec-kid")}})
	require.NoError(t, err)
	gock.New("https://auth.example.com").Get("/.well-known/jwks.json").Persist().Reply(200).JSON(json.RawMessage(doc))

	// Allow both algorithms so the token clears the upfront algorithm gate and the
	// test actually exercises the key-type compatibility guard in findVerificationKey.
	cfg := &common.JwtStrategyConfig{
		VerificationJwksUrl: jwksURL,
		AllowedAlgorithms:   []string{"RS256", "ES256"},
	}
	logger := zerolog.Nop()
	s, err := NewJwtStrategy(context.Background(), &logger, cfg)
	require.NoError(t, err)

	// RS256 token (different kid) must not be matched against the EC key.
	token := signTestRS256JWT(t, rsaKey, "rsa-kid", jwt.MapClaims{
		"sub": "attacker",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	_, err = s.Authenticate(context.Background(), nil, &AuthPayload{
		Type: common.AuthTypeJwt,
		Jwt:  &JwtPayload{Token: token},
	})
	require.Error(t, err)
}
