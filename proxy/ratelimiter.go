package proxy

import (
	"fmt"
	"strings"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/ratelimiter"
	"github.com/flair-sdk/erpc/config"
	"github.com/rs/zerolog/log"
)

type RateLimitersHub struct {
	cfg             *config.RateLimiterConfig
	bucketsLimiters map[string]*RateLimiterBucket
}

type RateLimiterBucket struct {
	Id    string
	Rules []*RateLimitRule
}

type RateLimitRule struct {
	Config  *config.RateLimitRuleConfig
	Limiter *ratelimiter.RateLimiter[interface{}]
}

var rulesByMethod map[string][]*RateLimitRule = make(map[string][]*RateLimitRule)

func NewRateLimitersHub(cfg *config.RateLimiterConfig) *RateLimitersHub {
	return &RateLimitersHub{
		cfg:             cfg,
		bucketsLimiters: make(map[string]*RateLimiterBucket),
	}
}

func (r *RateLimitersHub) Bootstrap() error {
	for _, bucketCfg := range r.cfg.Buckets {
		log.Debug().Msgf("bootstrapping rate limiter bucket: %s", bucketCfg.Id)
		for _, rule := range bucketCfg.Rules {
			log.Debug().Msgf("preparing rate limiter rule: %v", rule)

			if _, ok := r.bucketsLimiters[bucketCfg.Id]; !ok {
				r.bucketsLimiters[bucketCfg.Id] = &RateLimiterBucket{
					Id:    bucketCfg.Id,
					Rules: make([]*RateLimitRule, 0),
				}
			}

			var builder ratelimiter.RateLimiterBuilder[interface{}]

			duration, err := time.ParseDuration(rule.Period)
			if err != nil {
				return fmt.Errorf("failed to parse duration for limit %v: %w", rule, err)
			}

			if rule.Mode == "smooth" {
				builder = ratelimiter.SmoothBuilder[interface{}](uint(rule.MaxCount), duration)
			} else {
				builder = ratelimiter.BurstyBuilder[interface{}](uint(rule.MaxCount), duration)
			}

			if rule.WaitTime != "" {
				waitTime, err := time.ParseDuration(rule.WaitTime)
				if err != nil {
					return fmt.Errorf("failed to parse wait time for limit %v: %w", rule, err)
				}
				builder = builder.WithMaxWaitTime(waitTime)
			}

			builder.OnRateLimitExceeded(func(e failsafe.ExecutionEvent[any]) {
				log.Warn().Msgf("rate limit '%v' exceeded", rule)
			})

			limiter := builder.Build()
			log.Debug().Msgf("rate limiter rule prepared: %v with max: %d duration: %d", limiter, rule.MaxCount, duration)
			r.bucketsLimiters[bucketCfg.Id].Rules = append(r.bucketsLimiters[bucketCfg.Id].Rules, &RateLimitRule{
				Config:  rule,
				Limiter: &limiter,
			})
		}
	}

	return nil
}

func (r *RateLimitersHub) GetBucket(bucketId string) (*RateLimiterBucket, error) {
	if bucketId == "" {
		return nil, nil
	}

	if bucket, ok := r.bucketsLimiters[bucketId]; ok {
		return bucket, nil
	}

	return nil, fmt.Errorf("bucket not found: %s", bucketId)
}

func (b *RateLimiterBucket) GetRulesByMethod(method string) []*RateLimitRule {
	if limiters, ok := rulesByMethod[method]; ok {
		return limiters
	}

	rules := make([]*RateLimitRule, 0)

	for _, rule := range b.Rules {
		if rule.Config.Method == method ||
			wildcardMatch(rule.Config.Method, method) {
			rules = append(rules, rule)
		}
	}

	rulesByMethod[method] = rules
	
	return rules
}

func wildcardMatch(pattern, value string) bool {
	if pattern == "*" {
		return true
	}

	// if pattern includes a * anywhere in the string then check the match
	if i := strings.Index(pattern, "*"); i != -1 {
		return strings.HasPrefix(value, pattern[:i]) && strings.HasSuffix(value, pattern[i+1:])
	}

	return false
}
