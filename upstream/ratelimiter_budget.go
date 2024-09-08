package upstream

import (
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go/ratelimiter"
	"github.com/rs/zerolog"
)

type RateLimiterBudget struct {
	logger   *zerolog.Logger
	Id       string
	Rules    []*RateLimitRule
	registry *RateLimitersRegistry
	rulesMu  sync.RWMutex
}

type RateLimitRule struct {
	Config  *common.RateLimitRuleConfig
	Limiter ratelimiter.RateLimiter[interface{}]
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

func (b *RateLimiterBudget) AdjustBudget(rule *RateLimitRule, newMaxCount uint) error {
	b.rulesMu.Lock()
	defer b.rulesMu.Unlock()

	b.logger.Warn().Str("method", rule.Config.Method).Msgf("adjusting rate limiter budget from: %d to: %d ", rule.Config.MaxCount, newMaxCount)

	newCfg := &common.RateLimitRuleConfig{
		Method:   rule.Config.Method,
		Period:   rule.Config.Period,
		MaxCount: newMaxCount,
		WaitTime: rule.Config.WaitTime,
	}
	newLimiter, err := b.registry.createRateLimiter(newCfg)
	if err != nil {
		return err
	}
	rule.Config = newCfg
	rule.Limiter = newLimiter

	return nil
}
