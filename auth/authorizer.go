package auth

import (
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
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
}

// NewAuthorizer creates a new Authorizer based on the provided configuration
func NewAuthorizer(logger *zerolog.Logger, projectId string, cfg *common.AuthStrategyConfig, rateLimitersRegistry *upstream.RateLimitersRegistry) (*Authorizer, error) {
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
	default:
		return nil, common.NewErrInvalidConfig(fmt.Sprintf("unknown auth strategy type: %s", cfg.Type))
	}

	return &Authorizer{
		logger:               logger,
		cfg:                  cfg,
		strategy:             strategy,
		rateLimitersRegistry: rateLimitersRegistry,
	}, nil
}

// shouldApplyToMethod checks if the authorizer should be applied to the given method
// the allowedMethods takes precedence over ignoreMethods, meaning that explicitly allowed methods will override ignore methods
// for example if you want only eth_getLogs to be allowed, set ignoreMethods to ["*"] and allowMethods to ["eth_getLogs"]
func (a *Authorizer) shouldApplyToMethod(method string) bool {
	shouldApply := true

	if len(a.cfg.IgnoreMethods) > 0 {
		for _, ignoreMethod := range a.cfg.IgnoreMethods {
			if common.WildcardMatch(ignoreMethod, method) {
				shouldApply = false
				break
			}
		}
	}

	if len(a.cfg.AllowMethods) > 0 {
		for _, allowMethod := range a.cfg.AllowMethods {
			if common.WildcardMatch(allowMethod, method) {
				shouldApply = true
				break
			}
		}
	}

	return shouldApply
}

func (a *Authorizer) acquireRateLimitPermit(req common.NormalizedRequest) error {
	if a.cfg.RateLimitBudget == "" {
		return nil
	}

	rlb, errNetLimit := a.rateLimitersRegistry.GetBudget(a.cfg.RateLimitBudget)
	if errNetLimit != nil {
		return errNetLimit
	}
	if rlb == nil {
		return nil
	}

	method, errMethod := req.Method()
	if errMethod != nil {
		return errMethod
	}
	lg := a.logger.With().Str("method", method).Logger()

	rules := rlb.GetRulesByMethod(method)
	lg.Debug().Msgf("found %d auth-level rate limiters", len(rules))

	if len(rules) > 0 {
		for _, rule := range rules {
			permit := rule.Limiter.TryAcquirePermit()
			if !permit {
				health.MetricAuthRequestSelfRateLimited.WithLabelValues(
					a.projectId,
					string(a.cfg.Type),
					method,
				).Inc()
				return common.NewErrAuthRateLimitRuleExceeded(
					a.projectId,
					string(a.cfg.Type),
					a.cfg.RateLimitBudget,
					fmt.Sprintf("%+v", rule.Config),
				)
			} else {
				lg.Debug().Object("rateLimitRule", rule.Config).Msgf("auth-level rate limit passed")
			}
		}
	}

	return nil
}
