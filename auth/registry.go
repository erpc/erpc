package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

type AuthRegistry struct {
	projectId            string
	rateLimitersRegistry *upstream.RateLimitersRegistry
	strategies           []*Authorizer
}

// NewAuthRegistry creates a new group of authorizers for a project
func NewAuthRegistry(logger *zerolog.Logger, projectId string, cfg *common.AuthConfig, rateLimitersRegistry *upstream.RateLimitersRegistry) (*AuthRegistry, error) {
	if cfg == nil {
		return nil, common.NewErrInvalidConfig("auth config is nil")
	}

	r := &AuthRegistry{
		projectId:            projectId,
		strategies:           make([]*Authorizer, 0, len(cfg.Strategies)),
		rateLimitersRegistry: rateLimitersRegistry,
	}

	for _, strategy := range cfg.Strategies {
		lg := logger.With().Str("strategy", string(strategy.Type)).Logger()
		az, err := NewAuthorizer(&lg, projectId, strategy, rateLimitersRegistry)
		if err != nil {
			return nil, common.NewErrInvalidConfig(fmt.Sprintf("failed to create authorizer for project %s with strategy %s: %v", projectId, strategy.Type, err))
		}
		r.strategies = append(r.strategies, az)
	}

	return r, nil
}

// Authenticate checks the authentication payload against all registered strategies
func (r *AuthRegistry) Authenticate(ctx context.Context, method string, ap *AuthPayload) (context.Context, error) {
	if ap == nil {
		return ctx, common.NewErrAuthUnauthorized("", "auth payload is nil")
	}

	if len(r.strategies) == 0 {
		// If no strategies are configured, allow all requests
		return ctx, nil
	}

	var errs []error

	for _, az := range r.strategies {
		if !az.shouldApplyToMethod(method) {
			continue
		}

		if !az.strategy.Supports(ap) {
			continue
		}

		if err := az.strategy.Authenticate(ctx, ap); err != nil {
			errs = append(errs, err)
			continue
		}

		// If authentication is passed then apply and consume the rate limit
		if err := az.acquireRateLimitPermit(method); err != nil {
			return ctx, err
		}

		// Extract API key ID if available and store in context
		if apiKeyID := az.getAPIKeyID(ap); apiKeyID != "" {
			ctx = context.WithValue(ctx, common.ApiKeyIDContextKey, apiKeyID)
		}

		// If a strategy succeeds, we consider the request authenticated
		return ctx, nil
	}

	if len(errs) == 1 {
		return ctx, errs[0]
	}

	if len(errs) == 0 {
		return ctx, common.NewErrAuthUnauthorized("", "no auth strategy matched make sure correct headers or query strings are provided")
	}

	// If no strategy matched or succeeded, consider the request unauthorized
	return ctx, common.NewErrAuthUnauthorized("", errors.Join(errs...).Error())
}
