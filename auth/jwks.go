package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/rs/zerolog"
)

const defaultJwksHTTPTimeout = 10 * time.Second

// refreshDebounce is the window after a successful refresh during which further
// refreshes are skipped, bounding upstream JWKS fetch rate.
// refreshRetryAfter is the shorter window applied after a failed refresh so a
// transient IdP error doesn't block key-rotation recovery for the full window.
const (
	refreshDebounce   = 30 * time.Second
	refreshRetryAfter = 5 * time.Second
)

type jwksDocument struct {
	Keys []jwksKey `json:"keys"`
}

type jwksKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func fetchVerificationKeysFromJWKS(ctx context.Context, client *http.Client, url string, logger *zerolog.Logger) (map[string]jwt.Keyfunc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read JWKS response: %w", err)
	}

	return parseJWKS(body, logger)
}

func parseJWKS(body []byte, logger *zerolog.Logger) (map[string]jwt.Keyfunc, error) {
	var doc jwksDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse JWKS JSON: %w", err)
	}
	if len(doc.Keys) == 0 {
		return nil, fmt.Errorf("JWKS response contains no keys")
	}

	keys := make(map[string]jwt.Keyfunc)
	for _, key := range doc.Keys {
		if key.Kid == "" {
			continue
		}

		var publicKey interface{}
		var err error
		switch strings.ToUpper(key.Kty) {
		case "RSA":
			publicKey, err = parseRSAPublicKeyFromJWK(key.N, key.E)
		case "EC":
			publicKey, err = parseECPublicKeyFromJWK(key.Crv, key.X, key.Y)
		default:
			continue // skip unsupported key types (oct/OKP)
		}
		if err != nil {
			if logger != nil {
				logger.Warn().Err(err).Str("kid", key.Kid).Msg("skipping unparseable JWKS key")
			}
			continue
		}

		kid := key.Kid
		pub := publicKey
		alg := key.Alg
		keys[kid] = func(token *jwt.Token) (interface{}, error) {
			if alg != "" && token != nil && token.Method.Alg() != alg {
				return nil, fmt.Errorf("key %q requires alg %q, token uses %q", kid, alg, token.Method.Alg())
			}
			return pub, nil
		}
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("JWKS response contains no usable keys")
	}

	return keys, nil
}

func parseRSAPublicKeyFromJWK(nEncoded, eEncoded string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nEncoded)
	if err != nil {
		return nil, fmt.Errorf("invalid modulus: %w", err)
	}

	eBytes, err := base64.RawURLEncoding.DecodeString(eEncoded)
	if err != nil {
		return nil, fmt.Errorf("invalid exponent: %w", err)
	}

	if len(eBytes) > 8 {
		return nil, fmt.Errorf("RSA exponent too large (%d bytes)", len(eBytes))
	}
	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	if e == 0 {
		return nil, fmt.Errorf("invalid exponent")
	}

	return &rsa.PublicKey{N: n, E: e}, nil
}

func parseECPublicKeyFromJWK(crv, xEncoded, yEncoded string) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", crv)
	}

	xBytes, err := base64.RawURLEncoding.DecodeString(xEncoded)
	if err != nil {
		return nil, fmt.Errorf("invalid EC x coordinate: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(yEncoded)
	if err != nil {
		return nil, fmt.Errorf("invalid EC y coordinate: %w", err)
	}

	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	if !curve.IsOnCurve(x, y) { //nolint:staticcheck // SA1019: stdlib has no JWK parser; on-curve check is the right validation
		return nil, fmt.Errorf("EC point is not on curve %s", crv)
	}

	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

func (s *JwtStrategy) loadStaticVerificationKeys() (map[string]jwt.Keyfunc, error) {
	keys := make(map[string]jwt.Keyfunc)
	for kid, keyData := range s.cfg.VerificationKeys {
		parsedKey, err := parseKey(keyData)
		if err != nil {
			return nil, fmt.Errorf("failed to parse key %s: %w", kid, err)
		}
		kidCopy := kid
		keyCopy := parsedKey
		keys[kidCopy] = func(token *jwt.Token) (interface{}, error) {
			return keyCopy, nil
		}
	}
	return keys, nil
}

func (s *JwtStrategy) loadVerificationKeys(ctx context.Context) (map[string]jwt.Keyfunc, error) {
	keys, err := s.loadStaticVerificationKeys()
	if err != nil {
		return nil, err
	}
	if s.cfg.VerificationJwksUrl == "" {
		return keys, nil
	}

	jwksKeys, err := fetchVerificationKeysFromJWKS(ctx, s.httpClient, s.cfg.VerificationJwksUrl, s.logger)
	if err != nil {
		if len(keys) > 0 {
			// Static keys are available; JWKS is additive. Warn and continue so a
			// transient IdP outage doesn't prevent startup.
			if s.logger != nil {
				s.logger.Warn().Err(err).Msg("JWKS fetch failed at startup, serving static keys only")
			}
			return keys, nil
		}
		return nil, err
	}
	for kid, keyFn := range jwksKeys {
		if _, exists := keys[kid]; !exists {
			keys[kid] = keyFn
		}
	}
	return keys, nil
}

// refreshVerificationKeys reloads verification keys and swaps them in atomically.
// All refreshes are debounced via nextRefresh (shorter window on failure).
func (s *JwtStrategy) refreshVerificationKeys(ctx context.Context) (refreshed bool, err error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	if time.Now().Before(s.nextRefresh) {
		return false, nil
	}

	keys, err := s.loadVerificationKeys(ctx)
	if err != nil {
		s.nextRefresh = time.Now().Add(refreshRetryAfter)
		return false, err
	}

	s.keysMu.Lock()
	s.keys = keys
	s.keysMu.Unlock()
	s.nextRefresh = time.Now().Add(refreshDebounce)
	return true, nil
}

func (s *JwtStrategy) startJwksRefreshLoop(appCtx context.Context, interval time.Duration) {
	if s.cfg.VerificationJwksUrl == "" || interval <= 0 {
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-appCtx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(appCtx, defaultJwksHTTPTimeout)
				if _, err := s.refreshVerificationKeys(ctx); err != nil && s.logger != nil {
					// Keep serving with the last known keys when refresh fails.
					s.logger.Warn().Err(err).Msg("scheduled JWKS refresh failed, serving stale keys")
				}
				cancel()
			}
		}
	}()
}

func (s *JwtStrategy) getVerificationKeys() map[string]jwt.Keyfunc {
	s.keysMu.RLock()
	defer s.keysMu.RUnlock()
	return s.keys
}
