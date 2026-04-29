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
		return nil, common.NewErrAuthUnauthorized("n/a", "auth payload is nil")
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

	if len(errs) == 1 {
		return nil, errs[0]
	}

	if len(errs) == 0 {
		return nil, common.NewErrAuthUnauthorized("n/a", "no auth strategy matched make sure correct headers or query strings are provided")
	}

	// If multiple strategies returned ErrPaymentRequired (e.g. several x402
	// strategies advertising different chains/assets), merge their Accepts
	// arrays into a single 402 response so the challenge advertises every
	// accepted option. Otherwise SDK clients only ever see the first chain
	// in the response and signed payments for other chains never get tried.
	var payErrs []*common.ErrPaymentRequired
	for _, e := range errs {
		var payErr *common.ErrPaymentRequired
		if errors.As(e, &payErr) {
			payErrs = append(payErrs, payErr)
		}
	}
	if len(payErrs) > 0 {
		return nil, mergePaymentRequired(payErrs)
	}

	// If no strategy matched or succeeded, consider the request unauthorized
	return nil, common.NewErrAuthUnauthorized("n/a", errors.Join(errs...).Error())
}

// mergePaymentRequired combines multiple ErrPaymentRequired errors into a
// single error whose Accepts list is the concatenation of all inputs. The
// X402Version, Error, and Resource fields are taken from the first error
// since they don't vary across x402 strategies for a given request.
//
// If any payErr wraps a payload that isn't an X402PaymentRequirementsResponse
// (some future scheme), we fall back to the first error verbatim rather than
// dropping foreign entries silently.
func mergePaymentRequired(errs []*common.ErrPaymentRequired) error {
	if len(errs) == 1 {
		return errs[0]
	}
	base, ok := errs[0].PaymentRequirements.(X402PaymentRequirementsResponse)
	if !ok {
		return errs[0]
	}
	merged := append([]X402PaymentRequirement{}, base.Accepts...)
	for _, e := range errs[1:] {
		next, ok := e.PaymentRequirements.(X402PaymentRequirementsResponse)
		if !ok {
			return errs[0]
		}
		merged = append(merged, next.Accepts...)
	}
	base.Accepts = merged
	return common.NewErrPaymentRequired(base)
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
