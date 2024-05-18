package resiliency

import (
	"fmt"
	"strings"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/ratelimiter"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/rs/zerolog/log"
)

type RateLimitersRegistry struct {
	cfg             *config.RateLimiterConfig
	bucketsLimiters map[string]*RateLimiterBucket
}

type RateLimiterBucket struct {
	Id    string
	Rules []*RateLimitRule

	registry *RateLimitersRegistry
}

type RateLimitRule struct {
	Config  *config.RateLimitRuleConfig
	Limiter *ratelimiter.RateLimiter[interface{}]
}

var rulesByBucketAndMethod map[string]map[string][]*RateLimitRule = make(map[string]map[string][]*RateLimitRule)

func NewRateLimitersRegistry(cfg *config.RateLimiterConfig) (*RateLimitersRegistry, error) {
	r := &RateLimitersRegistry{
		cfg:             cfg,
		bucketsLimiters: make(map[string]*RateLimiterBucket),
	}
	err := r.bootstrap()

	return r, err
}

func (r *RateLimitersRegistry) bootstrap() error {
	if r.cfg == nil {
		log.Warn().Msg("no rate limiters defined which means all capacity of both local cpu/memory and remote upstreams will be used")
		return nil
	}

	for _, bucketCfg := range r.cfg.Buckets {
		log.Debug().Msgf("bootstrapping rate limiter bucket: %s", bucketCfg.Id)
		for _, rule := range bucketCfg.Rules {
			log.Debug().Msgf("preparing rate limiter rule: %v", rule)

			if _, ok := r.bucketsLimiters[bucketCfg.Id]; !ok {
				r.bucketsLimiters[bucketCfg.Id] = &RateLimiterBucket{
					Id:       bucketCfg.Id,
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
			r.bucketsLimiters[bucketCfg.Id].Rules = append(r.bucketsLimiters[bucketCfg.Id].Rules, &RateLimitRule{
				Config:  rule,
				Limiter: &limiter,
			})
		}
	}

	return nil
}

func (r *RateLimitersRegistry) GetBucket(bucketId string) (*RateLimiterBucket, error) {
	if bucketId == "" {
		return nil, nil
	}

	log.Debug().Msgf("getting rate limiter bucket: %s", bucketId)

	if bucket, ok := r.bucketsLimiters[bucketId]; ok {
		return bucket, nil
	}

	return nil, common.NewErrRateLimitBucketNotFound(bucketId)
}

func (b *RateLimiterBucket) GetRulesByMethod(method string) []*RateLimitRule {
	if _, ok := rulesByBucketAndMethod[b.Id]; !ok {
		rulesByBucketAndMethod[b.Id] = make(map[string][]*RateLimitRule)
	}

	if rules, ok := rulesByBucketAndMethod[b.Id][method]; ok {
		return rules
	}

	rules := make([]*RateLimitRule, 0)

	for _, rule := range b.Rules {
		if rule.Config.Method == method ||
			wildcardMatch(rule.Config.Method, method) {
			rules = append(rules, rule)
		}
	}

	rulesByBucketAndMethod[b.Id][method] = rules

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
