package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
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

		// Origin check runs after credential validation. On failure we return
		// immediately instead of continuing to the next strategy (unlike credential
		// failures which append to errs and continue). This is intentional: the
		// credential already identified the user, so the origin constraint applies
		// to that specific user. Trying another strategy with the same credential
		// but a different origin config would be semantically incorrect.
		if err := enforceOriginAllowlist(az.logger, req, user, string(az.cfg.Type)); err != nil {
			return nil, err
		}

		// Attach user to the request only after full auth success (credential + origin)
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

func enforceOriginAllowlist(logger *zerolog.Logger, req *common.NormalizedRequest, user *common.User, strategyType string) error {
	if user == nil || len(user.AllowedOrigins) == 0 {
		return nil
	}

	origin := ""
	if req != nil {
		origin = strings.ToLower(req.RequestOrigin())
	}
	if origin == "" {
		recordOriginFailureMetric(req, strategyType, "missing_origin")
		return common.NewErrAuthUnauthorized("origin", "missing request origin")
	}

	for _, allowedOrigin := range user.AllowedOrigins {
		lowered := strings.ToLower(allowedOrigin)
		if !strings.ContainsAny(lowered, "*?!|&() ") {
			if lowered == origin {
				return nil
			}
			continue
		}
		match, err := common.WildcardMatch(lowered, origin)
		if err != nil {
			if logger != nil {
				logger.Warn().
					Str("userId", user.Id).
					Str("pattern", allowedOrigin).
					Str("origin", origin).
					Err(err).
					Msg("invalid allowedOrigins pattern, skipping")
			}
			continue
		}
		if match {
			return nil
		}
	}

	recordOriginFailureMetric(req, strategyType, "origin_not_allowed")
	return common.NewErrAuthUnauthorized("origin", "request origin is not allowed")
}

func recordOriginFailureMetric(req *common.NormalizedRequest, strategy string, reason string) {
	project := "n/a"
	network := "n/a"
	agent := "unknown"
	if req != nil {
		if n := req.Network(); n != nil {
			project = n.ProjectId()
			network = req.NetworkLabel()
		} else {
			network = req.NetworkId()
		}
		agent = req.AgentName()
	}
	telemetry.MetricAuthFailedTotal.WithLabelValues(
		project,
		network,
		strategy,
		reason,
		agent,
	).Inc()
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
