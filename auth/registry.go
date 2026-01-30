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

	// If no strategy matched or succeeded, consider the request unauthorized
	return nil, common.NewErrAuthUnauthorized("n/a", errors.Join(errs...).Error())
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

// FindDatabaseStrategy finds a database strategy by connector ID from the strategies
func (r *AuthRegistry) FindDatabaseStrategy(connectorId string) (*DatabaseStrategy, error) {
	for _, az := range r.strategies {
		if az.cfg.Database != nil {
			if az.cfg.Database.Connector != nil && az.cfg.Database.Connector.Id == connectorId {
				if dbStrategy, ok := az.strategy.(*DatabaseStrategy); ok {
					return dbStrategy, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("database strategy with connector ID '%s' not found", connectorId)
}
