package auth

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
)

// Authorizer represents a single authentication strategy with its configuration
type Authorizer struct {
	projectId            string
	logger               *zerolog.Logger
	cfg                  *common.AuthStrategyConfig
	strategy             AuthStrategy
	rateLimitersRegistry *upstream.RateLimitersRegistry
	index                int
}

// NewAuthorizer creates a new Authorizer based on the provided configuration
func NewAuthorizer(appCtx context.Context, logger *zerolog.Logger, projectId string, cfg *common.AuthStrategyConfig, rateLimitersRegistry *upstream.RateLimitersRegistry, index int) (*Authorizer, error) {
	if cfg == nil {
		return nil, common.NewErrInvalidConfig("auth strategy config is nil")
	}

	var strategy AuthStrategy
	var err error

	switch cfg.Type {
	case common.AuthTypeSecret:
		if cfg.Secret == nil {
			return nil, common.NewErrInvalidConfig("secret strategy config is nil")
		}
		strategy = NewSecretStrategy(cfg.Secret)
	case common.AuthTypeJwt:
		if cfg.Jwt == nil {
			return nil, common.NewErrInvalidConfig("JWT strategy config is nil")
		}
		strategy, err = NewJwtStrategy(cfg.Jwt)
		if err != nil {
			return nil, err
		}
	case common.AuthTypeSiwe:
		if cfg.Siwe == nil {
			return nil, common.NewErrInvalidConfig("SIWE strategy config is nil")
		}
		strategy = NewSiweStrategy(cfg.Siwe)
	case common.AuthTypeNetwork:
		if cfg.Network == nil {
			return nil, common.NewErrInvalidConfig("network strategy config is nil")
		}
		strategy, err = NewNetworkStrategy(cfg.Network)
		if err != nil {
			return nil, err
		}
	case common.AuthTypeDatabase:
		if cfg.Database == nil {
			return nil, common.NewErrInvalidConfig("database strategy config is nil")
		}
		strategy, err = NewDatabaseStrategy(appCtx, logger, cfg.Database)
		if err != nil {
			return nil, err
		}
	default:
		return nil, common.NewErrInvalidConfig(fmt.Sprintf("unknown auth strategy type: %s", cfg.Type))
	}

	return &Authorizer{
		logger:               logger,
		cfg:                  cfg,
		strategy:             strategy,
		rateLimitersRegistry: rateLimitersRegistry,
		projectId:            projectId,
		index:                index,
	}, nil
}

// shouldApplyToMethod checks if the authorizer should be applied to the given method
// the allowedMethods takes precedence over ignoreMethods, meaning that explicitly allowed methods will override ignore methods
// for example if you want only eth_getLogs to be allowed, set ignoreMethods to ["*"] and allowMethods to ["eth_getLogs"]
func (a *Authorizer) shouldApplyToMethod(method string) bool {
	shouldApply := true

	if len(a.cfg.IgnoreMethods) > 0 {
		for _, ignoreMethod := range a.cfg.IgnoreMethods {
			match, err := common.WildcardMatch(ignoreMethod, method)
			if err != nil {
				a.logger.Error().Err(err).Msgf("error matching ignore method %s with method %s", ignoreMethod, method)
				continue
			}
			if match {
				shouldApply = false
				break
			}
		}
	}

	if len(a.cfg.AllowMethods) > 0 {
		for _, allowMethod := range a.cfg.AllowMethods {
			match, err := common.WildcardMatch(allowMethod, method)
			if err != nil {
				a.logger.Error().Err(err).Msgf("error matching allow method %s with method %s", allowMethod, method)
				continue
			}
			if match {
				shouldApply = true
				break
			}
		}
	}

	return shouldApply
}

func (a *Authorizer) acquireRateLimitPermit(ctx context.Context, req *common.NormalizedRequest, method string) error {
	// Determine effective budget
	effectiveBudget := a.cfg.RateLimitBudget
	if req != nil {
		if u := req.User(); u != nil && u.RateLimitBudget != "" {
			effectiveBudget = u.RateLimitBudget
		}
	}
	if effectiveBudget == "" {
		return nil
	}

	rlb, errNetLimit := a.rateLimitersRegistry.GetBudget(effectiveBudget)
	if errNetLimit != nil {
		return errNetLimit
	}
	if rlb == nil {
		return nil
	}

	allowed, err := rlb.TryAcquirePermit(ctx, a.projectId, req, method)
	if err != nil {
		return err
	}
	if !allowed {
		// Record unified rate limit event; attribute to auth origin.
		telemetry.CounterHandle(
			telemetry.MetricRateLimitsTotal,
			a.projectId,                // project
			req.NetworkId(),            // network
			"",                         // vendor
			"",                         // upstream
			method,                     // category
			req.Finality(ctx).String(), // finality
			req.UserId(),               // user
			req.AgentName(),            // agent_name
			effectiveBudget,            // budget (effective budget resolved earlier)
			"",                         // scope (budget rule scope unknown here)
			"blocked",                  // decision
			a.projectId,                // origin_project
			req.NetworkId(),            // origin_network
			fmt.Sprintf("%s:%d", string(a.cfg.Type), a.index), // origin_auth
		).Inc()
		return common.NewErrAuthRateLimitRuleExceeded(
			a.projectId,
			string(a.cfg.Type),
			effectiveBudget,
			fmt.Sprintf("method:%s", method),
		)
	}
	return nil
}
