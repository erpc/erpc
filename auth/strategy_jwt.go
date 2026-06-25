package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/golang-jwt/jwt/v4"
	"github.com/rs/zerolog"
)

type JwtStrategy struct {
	cfg        *common.JwtStrategyConfig
	logger     *zerolog.Logger
	httpClient *http.Client
	parser     *jwt.Parser
	keysMu     sync.RWMutex
	keys       map[string]jwt.Keyfunc

	// refreshMu serializes refreshVerificationKeys so concurrent callers never
	// fetch or swap keys concurrently. It also guards nextRefresh, the debounce gate.
	refreshMu   sync.Mutex
	nextRefresh time.Time
}

var _ AuthStrategy = &JwtStrategy{}

func NewJwtStrategy(appCtx context.Context, logger *zerolog.Logger, cfg *common.JwtStrategyConfig) (*JwtStrategy, error) {
	s := &JwtStrategy{
		cfg:        cfg,
		logger:     logger,
		httpClient: newJwksHTTPClient(cfg),
		parser:     jwt.NewParser(jwt.WithoutClaimsValidation()),
	}

	ctx, cancel := context.WithTimeout(appCtx, defaultJwksHTTPTimeout)
	defer cancel()
	if _, err := s.refreshVerificationKeys(ctx); err != nil {
		// JWKS unavailable at startup — fall back to static keys if configured,
		// so a transient IdP outage doesn't block startup.
		staticKeys, staticErr := s.loadStaticVerificationKeys()
		if staticErr != nil || len(staticKeys) == 0 {
			return nil, err
		}
		s.keysMu.Lock()
		s.keys = staticKeys
		s.keysMu.Unlock()
		if s.logger != nil {
			s.logger.Warn().Err(err).Msg("JWKS fetch failed at startup, serving static keys only")
		}
	}

	if cfg.VerificationJwksUrl != "" {
		s.startJwksRefreshLoop(appCtx, time.Duration(cfg.VerificationJwksRefreshInterval))
	}

	return s, nil
}

func newJwksHTTPClient(cfg *common.JwtStrategyConfig) *http.Client {
	if !cfg.VerificationJwksTlsInsecureSkipVerify {
		return &http.Client{Timeout: defaultJwksHTTPTimeout}
	}
	return &http.Client{
		Timeout: defaultJwksHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402
		},
	}
}

func (s *JwtStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeJwt
}

func (s *JwtStrategy) Authenticate(ctx context.Context, req *common.NormalizedRequest, ap *AuthPayload) (*common.User, error) {
	token, _, err := s.parser.ParseUnverified(ap.Jwt.Token, jwt.MapClaims{})
	if err != nil {
		return nil, common.NewErrAuthUnauthorized("jwt", "failed to parse JWT")
	}

	if len(s.cfg.AllowedAlgorithms) > 0 {
		if !contains(s.cfg.AllowedAlgorithms, token.Method.Alg()) {
			return nil, common.NewErrAuthUnauthorized("jwt", "unexpected signing method")
		}
	}

	key, err := s.findVerificationKey(ctx, token)
	if err != nil {
		return nil, common.NewErrAuthUnauthorized("jwt", err.Error())
	}

	// Verify the signature
	token, err = jwt.Parse(ap.Jwt.Token, key)
	if err != nil {
		return nil, common.NewErrAuthUnauthorized("jwt", "invalid signature")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, common.NewErrAuthUnauthorized("jwt", "invalid JWT claims")
	}

	if err := s.validateClaims(claims); err != nil {
		return nil, common.NewErrAuthUnauthorized("jwt", err.Error())
	}

	id, ok := claims["sub"].(string)
	if !ok {
		return nil, common.NewErrAuthUnauthorized("jwt", "missing 'sub' claim to be used as user id")
	}

	user := &common.User{Id: id}
	// Optional rate limit budget override via claim
	claimName := s.cfg.RateLimitBudgetClaimName
	if claimName == "" {
		claimName = "rlm"
	}
	if v, exists := claims[claimName]; exists {
		if s, ok := v.(string); ok && s != "" {
			user.RateLimitBudget = s
		}
	}

	return user, nil
}

func (s *JwtStrategy) findVerificationKey(ctx context.Context, token *jwt.Token) (jwt.Keyfunc, error) {
	keys := s.getVerificationKeys()

	kid, _ := token.Header["kid"].(string)
	if kid != "" {
		if key, exists := keys[kid]; exists {
			return key, nil
		}

		// The token names a kid we don't have. The upstream JWKS may have rotated
		// since the last scheduled refresh, so attempt a debounced on-demand
		// refresh and retry the kid lookup. This must happen before the
		// compatible-key-type fallback below, which would otherwise return a stale
		// key of the right type and fail signature verification without ever
		// refreshing. refreshing.
		if s.cfg.VerificationJwksUrl != "" {
			refreshCtx, cancel := context.WithTimeout(ctx, defaultJwksHTTPTimeout)
			refreshed, err := s.refreshVerificationKeys(refreshCtx)
			cancel()
			if err != nil && s.logger != nil {
				s.logger.Warn().Err(err).Msg("on-demand JWKS refresh failed, serving stale keys")
			}
			if refreshed {
				keys = s.getVerificationKeys()
				if key, exists := keys[kid]; exists {
					return key, nil
				}
			}
		}
	}

	// If no kid is provided or the kid doesn't match, try all keys
	for _, key := range keys {
		if isCompatibleKeyType(key, token.Method) {
			return key, nil
		}
	}

	return nil, fmt.Errorf("no suitable verification key found")
}

func (s *JwtStrategy) validateClaims(claims jwt.MapClaims) error {
	if err := claims.Valid(); err != nil {
		return fmt.Errorf("invalid standard claims: %w", err)
	}

	if len(s.cfg.AllowedIssuers) > 0 {
		iss, ok := claims["iss"].(string)
		if !ok {
			return fmt.Errorf("the 'iss' issuer claim is missing")
		}
		if !contains(s.cfg.AllowedIssuers, iss) {
			return fmt.Errorf("the '%s' issuer is not allowed", iss)
		}
	}

	if len(s.cfg.AllowedAudiences) > 0 {
		aud, ok := claims["aud"].(string)
		if !ok {
			return fmt.Errorf("the 'aud' audience claim is missing")
		}
		if !contains(s.cfg.AllowedAudiences, aud) {
			return fmt.Errorf("the '%s' audience is not allowed", aud)
		}
	}

	for _, requiredClaim := range s.cfg.RequiredClaims {
		if _, ok := claims[requiredClaim]; !ok {
			return fmt.Errorf("missing required claim: %s", requiredClaim)
		}
	}

	if err := validateClaimMatchers(claims, s.cfg.ClaimMatchers); err != nil {
		return err
	}

	return nil
}

// validateClaimMatchers enforces the configured claim matchers. Every key is an
// AND condition; within a key's value list any match passes (OR). An empty/omitted
// matcher set skips the check entirely.
func validateClaimMatchers(claims jwt.MapClaims, matchers map[string][]string) error {
	for claim, allowed := range matchers {
		if len(allowed) == 0 {
			continue
		}
		raw, ok := claims[claim]
		if !ok {
			return fmt.Errorf("claim %q is missing", claim)
		}
		tokenValues, err := normalizeClaimToStrings(raw)
		if err != nil {
			return fmt.Errorf("invalid %q claim: %w", claim, err)
		}
		if len(tokenValues) == 0 {
			return fmt.Errorf("claim %q is empty", claim)
		}
		matched := false
		for _, v := range tokenValues {
			if contains(allowed, v) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("claim %q: none of the token values are allowed", claim)
		}
	}
	return nil
}

// normalizeClaimToStrings flattens a JWT claim value to []string. It accepts a
// bare string, a string array, or a SCIM-style array of objects (extracting "value").
func normalizeClaimToStrings(raw interface{}) ([]string, error) {
	switch t := raw.(type) {
	case string:
		return []string{t}, nil
	case []string:
		return t, nil
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			switch v := item.(type) {
			case string:
				out = append(out, v)
			case map[string]interface{}: // SCIM: {"value": "...", ...}
				val, ok := v["value"].(string)
				if !ok {
					return nil, fmt.Errorf("object element missing string \"value\"")
				}
				out = append(out, val)
			default:
				return nil, fmt.Errorf("unsupported element type %T", item)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported claim type %T", raw)
	}
}

func parseKey(keyData interface{}) (interface{}, error) {
	switch k := keyData.(type) {
	case string:
		// Check if it's a file path
		if strings.HasPrefix(k, "file://") {
			content, err := os.ReadFile(strings.TrimPrefix(k, "file://"))
			if err != nil {
				return nil, fmt.Errorf("failed to read key file: %w", err)
			}
			k = string(content)
		}

		// Try to parse as PEM
		block, _ := pem.Decode([]byte(k))
		if block != nil {
			switch block.Type {
			case "RSA PUBLIC KEY", "PUBLIC KEY":
				return jwt.ParseRSAPublicKeyFromPEM([]byte(k))
			case "EC PUBLIC KEY":
				return jwt.ParseECPublicKeyFromPEM([]byte(k))
			}
		}

		// If not PEM, treat as HMAC secret
		return []byte(k), nil
	default:
		return nil, fmt.Errorf("unsupported key type: %T", keyData)
	}
}

func isCompatibleKeyType(keyFn jwt.Keyfunc, method jwt.SigningMethod) bool {
	key, err := keyFn(nil)
	if err != nil {
		return false
	}
	switch method.(type) {
	case *jwt.SigningMethodRSA:
		_, ok := key.(*rsa.PublicKey)
		return ok
	case *jwt.SigningMethodECDSA:
		_, ok := key.(*ecdsa.PublicKey)
		return ok
	case *jwt.SigningMethodHMAC:
		_, ok := key.([]byte)
		return ok
	default:
		return false
	}
}

func contains(slice []string, item interface{}) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
