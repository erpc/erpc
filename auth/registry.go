package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

// AuthRegistry holds the authentication strategies for a project
type AuthRegistry struct {
	projectId            string
	rateLimitersRegistry *upstream.RateLimitersRegistry
	strategies           []*Authorizer
}

// NewAuthRegistry creates a new group of authorizers for a project
func NewAuthRegistry(appCtx context.Context, logger *zerolog.Logger, projectId string, cfg *common.AuthConfig, rateLimitersRegistry *upstream.RateLimitersRegistry) (*AuthRegistry, error) {
	if cfg == nil {
		return nil, common.NewErrInvalidConfig("auth config is nil")
	}

	r := &AuthRegistry{
		projectId:            projectId,
		strategies:           make([]*Authorizer, 0, len(cfg.Strategies)),
		rateLimitersRegistry: rateLimitersRegistry,
	}

	for idx, strategy := range cfg.Strategies {
		lg := logger.With().Str("strategy", string(strategy.Type)).Int("index", idx).Logger()
		az, err := NewAuthorizer(appCtx, &lg, projectId, strategy, rateLimitersRegistry, idx)
		if err != nil {
			return nil, common.NewErrInvalidConfig(fmt.Sprintf("failed to create authorizer for project %s with strategy %s: %v", projectId, strategy.Type, err))
		}
		r.strategies = append(r.strategies, az)
	}

	return r, nil
}

// Authenticate checks the authentication payload against all registered strategies
func (r *AuthRegistry) Authenticate(ctx context.Context, req *common.NormalizedRequest, method string, ap *AuthPayload) (*common.User, error) {
	if ap == nil {
		return nil, common.NewErrAuthUnauthorized("n/a", "unauthorized", "auth payload is nil")
	}

	if len(r.strategies) == 0 {
		// If no strategies are configured, allow all requests
		return nil, nil
	}

	var errs []error

	for _, az := range r.strategies {
		if !az.shouldApplyToMethod(method) {
			continue
		}

		if !az.strategy.Supports(ap) {
			continue
		}

		user, err := az.strategy.Authenticate(ctx, req, ap)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		// Attach authorizer-level capabilities from the strategy that actually
		// authenticated this caller. Kept here (rather than in each strategy)
		// because it is configured on the outer AuthStrategyConfig — the same
		// place as allowMethods/ignoreMethods — so it applies uniformly to
		// every strategy type.
		//
		// Stamped onto a COPY, never in place: the database strategy caches one
		// *User per API key and hands that same pointer to every subsequent
		// request, so an in-place write would be an unsynchronized mutation of
		// shared state (racing the handler's read) and would also leak the
		// capability into the cache entry. The copy is allocated only when an
		// operator actually configured the capability.
		if user != nil && az.cfg.AllowClientDirectives != nil {
			stamped := *user
			stamped.AllowClientDirectives = az.cfg.AllowClientDirectives
			user = &stamped
		}

		// Attach user to the request early so downstream labels (user/agent) can be populated
		if user != nil && req != nil {
			req.SetUser(user)
		}
		// If authentication is passed then apply and consume the rate limit
		if err := az.acquireRateLimitPermit(ctx, req, method); err != nil {
			return user, err
		}

		// If a strategy succeeds, we consider the request authenticated
		return user, nil
	}

	return nil, unauthorized(errs)
}

// AcceptsPayload reports whether any strategy authenticates ap, whatever the
// method: nil when one does, or the error Authenticate would return. It
// applies no method scoping and takes no rate-limit permit, so it only
// rejects credentials no request could use; Authenticate still runs for
// every request.
func (r *AuthRegistry) AcceptsPayload(ctx context.Context, req *common.NormalizedRequest, ap *AuthPayload) error {
	if ap == nil {
		return common.NewErrAuthUnauthorized("n/a", "unauthorized", "auth payload is nil")
	}
	if len(r.strategies) == 0 {
		return nil
	}
	var errs []error
	for _, az := range r.strategies {
		if !az.strategy.Supports(ap) {
			continue
		}
		if _, err := az.strategy.Authenticate(ctx, req, ap); err != nil {
			errs = append(errs, err)
			continue
		}
		return nil
	}
	return unauthorized(errs)
}

// unauthorized is the error for a payload no strategy accepted.
func unauthorized(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}

	if len(errs) == 0 {
		return common.NewErrAuthUnauthorized("n/a", "unauthorized", "no auth strategy matched make sure correct headers or query strings are provided")
	}

	// Preserve strategy/reason when every attempt agrees. Prefer
	// token_expired when any attempt reports it: multiple JWT verifiers
	// often yield token_expired on the matching key and invalid_signature
	// on the others; expiry is the client-facing cause. Other divergent
	// labels stay n/a/unauthorized; the joined message still lists each.
	strategy, reason := agreedAuthFailureLabels(errs)
	return common.NewErrAuthUnauthorized(strategy, reason, errors.Join(errs...).Error())
}

// agreedAuthFailureLabels picks labels for a multi-strategy denial.
func agreedAuthFailureLabels(errs []error) (strategy, reason string) {
	if len(errs) == 0 {
		return "n/a", "unauthorized"
	}
	for _, err := range errs {
		s, r := authFailureLabels(err)
		if r == string(common.AuthUnauthorizedReasonTokenExpired) {
			return s, r
		}
	}
	strategy, reason = authFailureLabels(errs[0])
	for _, err := range errs[1:] {
		s, r := authFailureLabels(err)
		if s != strategy || r != reason {
			return "n/a", "unauthorized"
		}
	}
	return strategy, reason
}

// FindDatabaseConnector finds a database connector by ID from the strategies
func (r *AuthRegistry) FindDatabaseConnector(connectorId string) (data.Connector, error) {
	for _, az := range r.strategies {
		if az.cfg.Database != nil {
			if az.cfg.Database.Connector != nil && az.cfg.Database.Connector.Id == connectorId {
				// Access the connector from the DatabaseStrategy
				if dbStrategy, ok := az.strategy.(*DatabaseStrategy); ok {
					return dbStrategy.GetConnector(), nil
				}
			}
		}
	}
	return nil, fmt.Errorf("database connector with ID '%s' not found", connectorId)
}
