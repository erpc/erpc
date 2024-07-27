package upstream

import (
	"fmt"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/ratelimiter"
	"github.com/flair-sdk/erpc/common"
	"github.com/rs/zerolog"
)

type RateLimitersRegistry struct {
	logger          *zerolog.Logger
	cfg             *common.RateLimiterConfig
	budgetsLimiters sync.Map
}

type RateLimiterBudget struct {
	Id       string
	Rules    []*RateLimitRule
	registry *RateLimitersRegistry
	rulesMu  sync.RWMutex
}

type RateLimitRule struct {
	Config  *common.RateLimitRuleConfig
	Limiter ratelimiter.RateLimiter[interface{}]
}

func NewRateLimitersRegistry(cfg *common.RateLimiterConfig, logger *zerolog.Logger) (*RateLimitersRegistry, error) {
	r := &RateLimitersRegistry{
		cfg:    cfg,
		logger: logger,
	}
	err := r.bootstrap()
	return r, err
}

func (r *RateLimitersRegistry) bootstrap() error {
	if r.cfg == nil {
		r.logger.Warn().Msg("no rate limiters defined which means all capacity of both local cpu/memory and remote upstreams will be used")
		return nil
	}

	for _, budgetCfg := range r.cfg.Budgets {
		r.logger.Debug().Msgf("bootstrapping rate limiter budget: %s", budgetCfg.Id)
		budget := &RateLimiterBudget{
			Id:       budgetCfg.Id,
			Rules:    make([]*RateLimitRule, 0),
			registry: r,
		}

		for _, rule := range budgetCfg.Rules {
			r.logger.Debug().Msgf("preparing rate limiter rule: %v", rule)

			limiter, err := r.createRateLimiter(rule)
			if err != nil {
				return err
			}

			budget.rulesMu.Lock()
			budget.Rules = append(budget.Rules, &RateLimitRule{
				Config:  rule,
				Limiter: limiter,
			})
			budget.rulesMu.Unlock()
		}

		r.budgetsLimiters.Store(budgetCfg.Id, budget)
	}

	return nil
}

func (r *RateLimitersRegistry) createRateLimiter(rule *common.RateLimitRuleConfig) (ratelimiter.RateLimiter[interface{}], error) {
	duration, err := time.ParseDuration(rule.Period)
	if err != nil {
		return nil, common.NewErrRateLimitInvalidConfig(fmt.Errorf("failed to parse duration for limit %v: %w", rule, err))
	}

	builder := ratelimiter.BurstyBuilder[interface{}](uint(rule.MaxCount), duration)

	if rule.WaitTime != "" {
		waitTime, err := time.ParseDuration(rule.WaitTime)
		if err != nil {
			return nil, common.NewErrRateLimitInvalidConfig(fmt.Errorf("failed to parse wait time for limit %v: %w", rule, err))
		}
		builder = builder.WithMaxWaitTime(waitTime)
	}

	builder.OnRateLimitExceeded(func(e failsafe.ExecutionEvent[any]) {
		r.logger.Warn().Msgf("rate limit exceeded for rule '%v'", rule)
	})

	limiter := builder.Build()
	r.logger.Debug().Msgf("rate limiter rule prepared: %v with max: %d duration: %d", limiter, rule.MaxCount, duration)
	return limiter, nil
}

func (r *RateLimitersRegistry) GetBudget(budgetId string) (*RateLimiterBudget, error) {
	if budgetId == "" {
		return nil, nil
	}

	r.logger.Debug().Msgf("getting rate limiter budget: %s", budgetId)

	if budget, ok := r.budgetsLimiters.Load(budgetId); ok {
		return budget.(*RateLimiterBudget), nil
	}

	return nil, common.NewErrRateLimitBudgetNotFound(budgetId)
}

func (b *RateLimiterBudget) GetRulesByMethod(method string) []*RateLimitRule {
	b.rulesMu.RLock()
	defer b.rulesMu.RUnlock()

	rules := make([]*RateLimitRule, 0)

	for _, rule := range b.Rules {
		if rule.Config.Method == method || common.WildcardMatch(rule.Config.Method, method) {
			rules = append(rules, rule)
		}
	}

	return rules
}