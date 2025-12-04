package upstream

import (
	"context"
	"fmt"
	"sync"
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
	cache      limiter.RateLimitCache
	maxTimeout time.Duration
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
	telemetry.MetricRateLimiterBudgetMaxCount.WithLabelValues(b.Id, rule.Config.Method, rule.Config.ScopeString()).Set(float64(newMaxCount))
	return nil
}

// TryAcquirePermit evaluates all matching rules for the given method using Envoy's DoLimit
// and returns whether the request is allowed.
func (b *RateLimiterBudget) TryAcquirePermit(ctx context.Context, projectId string, req *common.NormalizedRequest, method string, vendor string, upstreamId string, authLabel string, origin string) (bool, error) {
	if b.cache == nil {
		return true, nil
	}

	// Detailed span for internal evaluation
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

		// Map enum period to unit (supports second..year) and construct full RateLimit
		unit := rule.Config.Period.Unit()
		// Build stats key similar to Envoy: domain + '.' + keys (avoid high-cardinality values)
		statsKey := b.Id + ".method_" + method
		if rule.Config.PerIP {
			statsKey += ".ip"
		}
		if rule.Config.PerUser {
			statsKey += ".user"
		}
		if rule.Config.PerNetwork {
			statsKey += ".network"
		}
		rlStats := b.registry.statsManager.NewStats(statsKey)
		limit := config.NewRateLimit(rule.Config.MaxCount, unit, rlStats, false, false, "", nil, false)

		// Emit consolidated budget decision metrics only (avoid redundant per-call trio counters)
		userLabel := ""
		agentName := ""
		networkLabel := ""
		finality := ""
		if req != nil {
			userLabel = req.UserId()
			agentName = req.AgentName()
			networkLabel = req.NetworkId()
			finality = req.Finality(ctx).String()
		}

		// External span around cache.DoLimit to capture total time spent
		_, doSpan := common.StartSpan(ctx, "RateLimiter.DoLimit",
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("budget", b.Id),
				attribute.String("method", method),
				attribute.String("scope", rule.Config.ScopeString()),
			),
		)

		// Execute DoLimit with timeout (fail-open if timeout exceeded)
		var statuses []*pb.RateLimitResponse_DescriptorStatus
		limits := []*config.RateLimit{limit}

		if b.maxTimeout > 0 {
			// Run DoLimit with timeout; fail-open if exceeded.
			// Use buffered channel so goroutine can always send and exit cleanly,
			// even if we've timed out and moved on.
			resultCh := make(chan []*pb.RateLimitResponse_DescriptorStatus, 1)
			go func() {
				resultCh <- b.cache.DoLimit(ctx, rlReq, limits)
			}()

			// Use time.NewTimer instead of time.After to avoid timer leak.
			// time.After creates a timer that won't be GC'd until it fires,
			// causing memory accumulation in high-throughput scenarios.
			timer := time.NewTimer(b.maxTimeout)
			select {
			case statuses = <-resultCh:
				// DoLimit completed in time - stop the timer to release resources
				if !timer.Stop() {
					// Timer already fired, drain the channel to prevent leak
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
				// Timeout exceeded - fail open (allow request).
				// The goroutine will eventually complete and send to the buffered channel,
				// then exit normally (no goroutine leak, just delayed cleanup).
				b.logger.Warn().
					Str("budget", b.Id).
					Str("method", method).
					Dur("timeout", b.maxTimeout).
					Msg("rate limiter timeout exceeded, failing open (allowing request)")
				doSpan.SetAttributes(attribute.String("result", "timeout_fail_open"))
				doSpan.End()
				telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
					projectId,
					networkLabel,
					userLabel,
					agentName,
					b.Id,
					method,
					"limit_timeout",
				).Inc()
				continue // Skip this rule, allow request
			}
		} else {
			// No timeout configured, call synchronously
			statuses = b.cache.DoLimit(ctx, rlReq, limits)
		}

		if len(statuses) > 0 && statuses[0].Code == pb.RateLimitResponse_OVER_LIMIT {
			doSpan.SetAttributes(attribute.String("result", "over_limit"))
			doSpan.End()
			// Record blocked event centrally here to avoid double counting at call sites
			telemetry.CounterHandle(
				telemetry.MetricRateLimitsTotal,
				projectId,                 // project
				networkLabel,              // network
				vendor,                    // vendor
				upstreamId,                // upstream
				method,                    // category
				finality,                  // finality
				userLabel,                 // user
				agentName,                 // agent_name
				b.Id,                      // budget
				rule.Config.ScopeString(), // scope
				authLabel,                 // auth
				origin,                    // origin
			).Inc()
			return false, nil
		}
		doSpan.SetAttributes(attribute.String("result", "ok"))
		doSpan.End()
	}
	return true, nil
}
