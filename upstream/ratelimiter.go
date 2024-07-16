package upstream

import (
	"fmt"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/ratelimiter"
	"github.com/flair-sdk/erpc/common"
	"github.com/rs/zerolog/log"
)

type RateLimitersRegistry struct {
	cfg             *common.RateLimiterConfig
	budgetsLimiters map[string]*RateLimiterBudget
}

type RateLimiterBudget struct {
	Id    string
	Rules []*RateLimitRule

	registry *RateLimitersRegistry
}

type RateLimitRule struct {
	Config  *common.RateLimitRuleConfig
	Limiter *ratelimiter.RateLimiter[interface{}]
}

var rulesByBudgetAndMethod map[string]map[string][]*RateLimitRule = make(map[string]map[string][]*RateLimitRule)

func NewRateLimitersRegistry(cfg *common.RateLimiterConfig) (*RateLimitersRegistry, error) {
	r := &RateLimitersRegistry{
		cfg:             cfg,
		budgetsLimiters: make(map[string]*RateLimiterBudget),
	}
	err := r.bootstrap()

	return r, err
}

func (r *RateLimitersRegistry) bootstrap() error {
	if r.cfg == nil {
		log.Warn().Msg("no rate limiters defined which means all capacity of both local cpu/memory and remote upstreams will be used")
		return nil
	}

	for _, budgetCfg := range r.cfg.Budgets {
		log.Debug().Msgf("bootstrapping rate limiter budget: %s", budgetCfg.Id)
		for _, rule := range budgetCfg.Rules {
			log.Debug().Msgf("preparing rate limiter rule: %v", rule)

			if _, ok := r.budgetsLimiters[budgetCfg.Id]; !ok {
				r.budgetsLimiters[budgetCfg.Id] = &RateLimiterBudget{
					Id:       budgetCfg.Id,
					Rules:    make([]*RateLimitRule, 0),
					registry: r,
				}
			}

			var builder ratelimiter.RateLimiterBuilder[interface{}]

			duration, err := time.ParseDuration(rule.Period)
			if err != nil {
				return common.NewErrRateLimitInvalidConfig(fmt.Errorf("failed to parse duration for limit %v: %w", rule, err))
			}

			builder = ratelimiter.BurstyBuilder[interface{}](uint(rule.MaxCount), duration)

			if rule.WaitTime != "" {
				waitTime, err := time.ParseDuration(rule.WaitTime)
				if err != nil {
					return common.NewErrRateLimitInvalidConfig(fmt.Errorf("failed to parse wait time for limit %v: %w", rule, err))
				}
				builder = builder.WithMaxWaitTime(waitTime)
			}

			builder.OnRateLimitExceeded(func(e failsafe.ExecutionEvent[any]) {
				log.Warn().Msgf("rate limit exceeded for rule '%v'", rule)
			})

			limiter := builder.Build()
			log.Debug().Msgf("rate limiter rule prepared: %v with max: %d duration: %d", limiter, rule.MaxCount, duration)
			r.budgetsLimiters[budgetCfg.Id].Rules = append(r.budgetsLimiters[budgetCfg.Id].Rules, &RateLimitRule{
				Config:  rule,
				Limiter: &limiter,
			})
		}
	}

	return nil
}

func (r *RateLimitersRegistry) GetBudget(budgetId string) (*RateLimiterBudget, error) {
	if budgetId == "" {
		return nil, nil
	}

	log.Debug().Msgf("getting rate limiter budget: %s", budgetId)

	if budget, ok := r.budgetsLimiters[budgetId]; ok {
		return budget, nil
	}

	return nil, common.NewErrRateLimitBudgetNotFound(budgetId)
}

func (b *RateLimiterBudget) GetRulesByMethod(method string) []*RateLimitRule {
	if _, ok := rulesByBudgetAndMethod[b.Id]; !ok {
		rulesByBudgetAndMethod[b.Id] = make(map[string][]*RateLimitRule)
	}

	if rules, ok := rulesByBudgetAndMethod[b.Id][method]; ok {
		return rules
	}

	rules := make([]*RateLimitRule, 0)

	for _, rule := range b.Rules {
		if rule.Config.Method == method ||
			common.WildcardMatch(rule.Config.Method, method) {
			rules = append(rules, rule)
		}
	}

	rulesByBudgetAndMethod[b.Id][method] = rules

	return rules
}
