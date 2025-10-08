package upstream

import (
	"context"
	"fmt"
	"sync"

	pb_struct "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/limiter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

type RateLimiterBudget struct {
	logger   *zerolog.Logger
	Id       string
	Rules    []*RateLimitRule
	registry *RateLimitersRegistry
	rulesMu  sync.RWMutex
	cache    limiter.RateLimitCache
}

type RateLimitRule struct {
	Config *common.RateLimitRuleConfig
}

func (b *RateLimiterBudget) GetRulesByMethod(method string) ([]*RateLimitRule, error) {
	b.rulesMu.RLock()
	defer b.rulesMu.RUnlock()

	rules := make([]*RateLimitRule, 0)

	for _, rule := range b.Rules {
		match, err := common.WildcardMatch(rule.Config.Method, method)
		if err != nil {
			return nil, err
		}
		if rule.Config.Method == method || match {
			rules = append(rules, rule)
		}
	}

	return rules, nil
}

// AdjustBudget updates the MaxCount for the provided rule and refreshes telemetry.
func (b *RateLimiterBudget) AdjustBudget(rule *RateLimitRule, newMaxCount uint32) error {
	if rule == nil || rule.Config == nil {
		return nil
	}
	b.rulesMu.Lock()
	defer b.rulesMu.Unlock()

	prev := rule.Config.MaxCount
	if prev == newMaxCount {
		return nil
	}
	b.logger.Warn().Str("method", rule.Config.Method).Msgf("adjusting rate limiter budget from: %d to: %d", prev, newMaxCount)
	rule.Config.MaxCount = newMaxCount
	telemetry.MetricRateLimiterBudgetMaxCount.WithLabelValues(b.Id, rule.Config.Method).Set(float64(newMaxCount))
	return nil
}

// TryAcquirePermit evaluates all matching rules for the given method using Envoy's DoLimit
// and returns whether the request is allowed.
func (b *RateLimiterBudget) TryAcquirePermit(ctx context.Context, req *common.NormalizedRequest, method string) (bool, error) {
	if b.cache == nil {
		return true, nil
	}

	rules, err := b.GetRulesByMethod(method)
	if err != nil {
		return false, err
	}
	if len(rules) == 0 {
		return true, nil
	}

	for _, rule := range rules {
		// Build descriptor entries
		entries := []*pb_struct.RateLimitDescriptor_Entry{{Key: "method", Value: method}}
		if rule.Config.PerIP {
			if req == nil {
				return false, fmt.Errorf("request cannot be nil when ratelimiter rule has perIP")
			}
			if ip := req.ClientIP(); ip != "" && ip != "n/a" {
				entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "ip", Value: ip})
			}
		}
		if rule.Config.PerUser {
			if req == nil {
				return false, fmt.Errorf("request cannot be nil when ratelimiter rule has perUser")
			}
			if uid := req.UserId(); uid != "" && uid != "n/a" {
				entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "user", Value: uid})
			}
		}
		if rule.Config.PerNetwork {
			if req == nil {
				return false, fmt.Errorf("request cannot be nil when ratelimiter rule has perNetwork")
			}
			if nid := req.NetworkId(); nid != "" && nid != "n/a" {
				entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "network", Value: nid})
			}
		}

		descriptor := &pb_struct.RateLimitDescriptor{Entries: entries}
		rlReq := &pb.RateLimitRequest{Domain: b.Id, Descriptors: []*pb_struct.RateLimitDescriptor{descriptor}, HitsAddend: 1}

		// Map enum period to unit (supports second..year)
		unit := rule.Config.Period.Unit()
		limit := &config.RateLimit{Limit: &pb.RateLimitResponse_RateLimit{RequestsPerUnit: rule.Config.MaxCount, Unit: unit}}

		statuses := b.cache.DoLimit(ctx, rlReq, []*config.RateLimit{limit})
		if len(statuses) > 0 && statuses[0].Code == pb.RateLimitResponse_OVER_LIMIT {
			// Telemetry: over limit per rule
			telemetry.MetricAuthRequestSelfRateLimited.WithLabelValues(b.Id, "<rule>", method).Inc()
			return false, nil
		}
	}
	return true, nil
}
