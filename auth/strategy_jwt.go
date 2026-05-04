package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/golang-jwt/jwt/v4"
)

type JwtStrategy struct {
	cfg    *common.JwtStrategyConfig
	parser *jwt.Parser
	keys   map[string]jwt.Keyfunc
}

var _ AuthStrategy = &JwtStrategy{}

func NewJwtStrategy(cfg *common.JwtStrategyConfig) (*JwtStrategy, error) {
	// Parse and store verification keys
	var keys map[string]jwt.Keyfunc = make(map[string]jwt.Keyfunc)
	for kid, keyData := range cfg.VerificationKeys {
		parsedKey, err := parseKey(keyData)
		if err != nil {
			return nil, fmt.Errorf("failed to parse key %s: %w", kid, err)
		}
		keys[kid] = func(token *jwt.Token) (interface{}, error) {
			return parsedKey, nil
		}
	}

	return &JwtStrategy{
		cfg:    cfg,
		parser: jwt.NewParser(jwt.WithoutClaimsValidation()),
		keys:   keys,
	}, nil
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

	key, err := s.findVerificationKey(token)
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

func (s *JwtStrategy) findVerificationKey(token *jwt.Token) (jwt.Keyfunc, error) {
	kid, ok := token.Header["kid"].(string)
	if ok {
		if key, exists := s.keys[kid]; exists {
			return key, nil
		}
	}

	// If no kid is provided or the kid doesn't match, try all keys
	for _, key := range s.keys {
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

	return nil
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
