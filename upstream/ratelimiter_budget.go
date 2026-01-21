package upstream

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pb_struct "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/limiter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type RateLimiterBudget struct {
	logger     *zerolog.Logger
	Id         string
	Rules      []*RateLimitRule
	registry   *RateLimitersRegistry
	rulesMu    sync.RWMutex
	maxTimeout time.Duration
}

type RateLimitRule struct {
	Config *common.RateLimitRuleConfig
}

func (b *RateLimiterBudget) GetRulesByMethod(method string) ([]*RateLimitRule, error) {
	b.rulesMu.RLock()
	defer b.rulesMu.RUnlock()

	rules := make([]*RateLimitRule, 0, len(b.Rules))
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
	telemetry.MetricRateLimiterBudgetMaxCount.WithLabelValues(b.Id, rule.Config.Method, rule.Config.ScopeString()).Set(float64(newMaxCount))
	return nil
}

// ruleResult holds the result of evaluating a single rule.
type ruleResult struct {
	rule    *RateLimitRule
	allowed bool
}

// getCache returns the current cache from the registry (thread-safe)
func (b *RateLimiterBudget) getCache() limiter.RateLimitCache {
	return b.registry.GetCache()
}

// TryAcquirePermit evaluates all matching rules for the given method using Envoy's DoLimit.
// Rules are evaluated in parallel for lower latency. Returns true if allowed, false if rate limited.
func (b *RateLimiterBudget) TryAcquirePermit(ctx context.Context, projectId string, req *common.NormalizedRequest, method string, vendor string, upstreamId string, authLabel string, origin string) (bool, error) {
	cache := b.getCache()
	if cache == nil {
		return true, nil // Fail-open when no cache is available
	}

	ctx, span := common.StartDetailSpan(ctx, "RateLimiter.TryAcquirePermit",
		trace.WithAttributes(
			attribute.String("budget", b.Id),
			attribute.String("method", method),
		),
	)
	defer span.End()

	rules, err := b.GetRulesByMethod(method)
	if err != nil {
		return false, err
	}
	if len(rules) == 0 {
		return true, nil
	}

	// Extract request metadata once
	var userLabel, agentName, networkLabel, finality, clientIP string
	if req != nil {
		userLabel = req.UserId()
		agentName = req.AgentName()
		networkLabel = req.NetworkId()
		finality = req.Finality(ctx).String()
		clientIP = req.ClientIP()
	}

	// Validate request context upfront
	for _, rule := range rules {
		if (rule.Config.PerIP || rule.Config.PerUser || rule.Config.PerNetwork) && req == nil {
			return false, fmt.Errorf("request cannot be nil when ratelimiter rule has perIP/perUser/perNetwork")
		}
	}

	// Single rule: evaluate directly without goroutine overhead
	if len(rules) == 1 {
		allowed := b.evaluateRule(ctx, rules[0], method, clientIP, userLabel, networkLabel)
		if !allowed {
			telemetry.CounterHandle(
				telemetry.MetricRateLimitsTotal,
				projectId, networkLabel, vendor, upstreamId, method, finality,
				userLabel, agentName, b.Id, rules[0].Config.ScopeString(), authLabel, origin,
			).Inc()
		}
		return allowed, nil
	}

	// Multiple rules: evaluate in parallel
	resultCh := make(chan ruleResult, len(rules))
	var blocked atomic.Bool

	for _, rule := range rules {
		go func(r *RateLimitRule) {
			// Skip if already blocked
			if blocked.Load() {
				resultCh <- ruleResult{rule: r, allowed: true}
				return
			}
			allowed := b.evaluateRule(ctx, r, method, clientIP, userLabel, networkLabel)
			if !allowed {
				blocked.Store(true)
			}
			resultCh <- ruleResult{rule: r, allowed: allowed}
		}(rule)
	}

	// Collect results
	var blockingRule *RateLimitRule
	for i := 0; i < len(rules); i++ {
		result := <-resultCh
		if !result.allowed && blockingRule == nil {
			blockingRule = result.rule
		}
	}

	if blockingRule != nil {
		telemetry.CounterHandle(
			telemetry.MetricRateLimitsTotal,
			projectId, networkLabel, vendor, upstreamId, method, finality,
			userLabel, agentName, b.Id, blockingRule.Config.ScopeString(), authLabel, origin,
		).Inc()
		return false, nil
	}
	return true, nil
}

// evaluateRule checks a single rate limit rule against the cache.
// Returns true if allowed, false if over limit.
func (b *RateLimiterBudget) evaluateRule(ctx context.Context, rule *RateLimitRule, method, clientIP, userLabel, networkLabel string) bool {
	cache := b.getCache()
	if cache == nil {
		return true // Fail-open when no cache is available
	}

	// Build descriptor entries
	entries := []*pb_struct.RateLimitDescriptor_Entry{{Key: "method", Value: method}}
	if rule.Config.PerIP && clientIP != "" && clientIP != "n/a" {
		entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "ip", Value: clientIP})
	}
	if rule.Config.PerUser && userLabel != "" && userLabel != "n/a" {
		entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "user", Value: userLabel})
	}
	if rule.Config.PerNetwork && networkLabel != "" && networkLabel != "n/a" {
		entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "network", Value: networkLabel})
	}

	rlReq := &pb.RateLimitRequest{
		Domain:      b.Id,
		Descriptors: []*pb_struct.RateLimitDescriptor{{Entries: entries}},
		HitsAddend:  1,
	}

	// Build stats key
	statsKey := b.Id + ".method_" + method + rule.statsKeySuffix()

	rlStats := b.registry.statsManager.NewStats(statsKey)
	limit := config.NewRateLimit(rule.Config.MaxCount, rule.Config.Period.Unit(), rlStats, false, false, "", nil, false)
	limits := []*config.RateLimit{limit}

	_, doSpan := common.StartSpan(ctx, "RateLimiter.DoLimit",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("budget", b.Id),
			attribute.String("method", method),
			attribute.String("scope", rule.Config.ScopeString()),
		),
	)

	var statuses []*pb.RateLimitResponse_DescriptorStatus
	var timedOut bool
	if b.maxTimeout > 0 {
		statuses, timedOut = b.doLimitWithTimeout(ctx, cache, rlReq, limits, method, userLabel, networkLabel)
	} else {
		statuses = cache.DoLimit(ctx, rlReq, limits)
	}

	if timedOut {
		doSpan.SetAttributes(attribute.String("result", "timeout_fail_open"))
		doSpan.End()
		return true // fail-open
	}

	isOverLimit := len(statuses) > 0 && statuses[0].Code == pb.RateLimitResponse_OVER_LIMIT
	if isOverLimit {
		doSpan.SetAttributes(attribute.String("result", "over_limit"))
	} else {
		doSpan.SetAttributes(attribute.String("result", "ok"))
	}
	doSpan.End()

	return !isOverLimit
}

// statsKeySuffix returns the pre-computed suffix for stats key.
func (r *RateLimitRule) statsKeySuffix() string {
	suffix := ""
	if r.Config.PerIP {
		suffix += ".ip"
	}
	if r.Config.PerUser {
		suffix += ".user"
	}
	if r.Config.PerNetwork {
		suffix += ".network"
	}
	return suffix
}

// doLimitWithTimeout executes DoLimit with a timeout.
// Returns (statuses, timedOut). On timeout, returns (nil, true) and records fail-open metric.
func (b *RateLimiterBudget) doLimitWithTimeout(
	ctx context.Context,
	cache limiter.RateLimitCache,
	rlReq *pb.RateLimitRequest,
	limits []*config.RateLimit,
	method, userLabel, networkLabel string,
) ([]*pb.RateLimitResponse_DescriptorStatus, bool) {
	resultCh := make(chan []*pb.RateLimitResponse_DescriptorStatus, 1)
	go func() {
		resultCh <- cache.DoLimit(ctx, rlReq, limits)
	}()

	timer := time.NewTimer(b.maxTimeout)
	select {
	case statuses := <-resultCh:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return statuses, false

	case <-timer.C:
		b.logger.Warn().
			Str("budget", b.Id).
			Str("method", method).
			Dur("timeout", b.maxTimeout).
			Msg("rate limiter timeout exceeded, failing open")

		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			"", // projectId not available here
			networkLabel,
			userLabel,
			"", // agentName not available here
			b.Id,
			method,
			"limit_timeout",
		).Inc()

		return nil, true
	}
}
