package upstream

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	pb_struct "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/limiter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
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

	// admission is a buffered semaphore that bounds the number of concurrent
	// in-flight remote (Redis) DoLimit calls per budget. It exists because the
	// underlying envoyproxy/ratelimit + radix client does not honor context
	// cancellation: a goroutine spawned in doLimitWithTimeout that has timed
	// out will continue to live until Redis finally answers (which can be
	// seconds when the connection pool is contended).
	//
	// Without this cap we observed 25k+ leaked goroutines per machine during
	// a Redis-rate-limiter contention spike (root-caused 2026-05-07 cronos
	// receipts incident), which then drove CPU/GC into a death spiral.
	//
	// When admission is full, doLimitWithTimeout fail-opens immediately
	// without spawning a goroutine — increments
	// MetricRateLimiterRemoteAdmissionSheddedTotal so we can alert on it.
	//
	// Nil when no remote cache is in use (e.g. memory cache); only allocated
	// when maxTimeout > 0.
	admission chan struct{}

	// inflight gauge, kept here for fast hot-path access without a labels
	// lookup on every call. Refreshed at registration time.
	inflightGauge      prometheus.Gauge
	admissionShedded   prometheus.Counter
	durationFailopen   prometheus.Observer
	durationOK         prometheus.Observer
	durationOverlimit  prometheus.Observer
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

// AdjustBudgetByFactor reads currentMax, multiplies by factor, clamps to
// [minBudget, maxBudget], and writes the result -- all under rulesMu.
// Returns (prev, next, changed) so callers can log without holding the lock.
func (b *RateLimiterBudget) AdjustBudgetByFactor(rule *RateLimitRule, factor float64, minBudget, maxBudget int) (prev, next uint32, changed bool) {
	if rule == nil || rule.Config == nil {
		return 0, 0, false
	}
	b.rulesMu.Lock()
	defer b.rulesMu.Unlock()

	prev = rule.Config.MaxCount
	raw := math.Ceil(float64(prev) * factor)
	// Clamp to a valid uint32 range before the narrowing cast.  A large
	// increaseFactor can push the float64 product above math.MaxUint32,
	// which causes Go's float64→uint32 conversion to wrap to an arbitrary
	// small value -- the opposite of an increase.
	if raw > math.MaxUint32 {
		raw = math.MaxUint32
	} else if raw < 0 {
		raw = 0
	}
	next = uint32(raw)
	if minBudget > 0 {
		minClamped := uint32(max(0, minBudget))
		if next < minClamped {
			next = minClamped
		}
	}
	if maxBudget > 0 {
		maxClamped := uint32(max(0, maxBudget))
		if next > maxClamped {
			next = maxClamped
		}
	}

	if next == prev {
		return prev, next, false
	}

	rule.Config.MaxCount = next
	telemetry.MetricRateLimiterBudgetMaxCount.WithLabelValues(b.Id, rule.Config.Method, rule.Config.ScopeString()).Set(float64(next))
	return prev, next, true
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
	var failOpenReason string
	if b.maxTimeout > 0 {
		statuses, timedOut, failOpenReason = b.doLimitWithTimeout(ctx, cache, rlReq, limits, method, userLabel, networkLabel)
	} else {
		statuses = cache.DoLimit(ctx, rlReq, limits)
	}

	if timedOut {
		doSpan.SetAttributes(
			attribute.String("result", "fail_open"),
			attribute.String("reason", failOpenReason),
		)
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

// doLimitWithTimeout executes DoLimit with a timeout AND a per-budget admission cap.
//
// Returns (statuses, failOpen, reason).
//
// The admission semaphore is the critical resilience primitive: the underlying
// envoyproxy/ratelimit cache + radix client do not honor context cancellation
// for the actual Redis I/O. Without a cap, every timed-out call leaks a
// goroutine that lives until Redis answers — which under contention spirals
// into runaway goroutine/FD growth and CPU saturation. With the cap in place,
// the in-flight count is bounded and the worst case is a brief load-shed
// burst.
//
// We DO continue to spawn a goroutine for the actual DoLimit (so the response
// can still complete and update Redis state correctly even after we've
// fail-opened), but only when we have an admission slot available — and we
// release the slot from inside that goroutine, so a slow Redis holds the slot
// for as long as the call truly takes, not just the local timeout.
func (b *RateLimiterBudget) doLimitWithTimeout(
	ctx context.Context,
	cache limiter.RateLimitCache,
	rlReq *pb.RateLimitRequest,
	limits []*config.RateLimit,
	method, userLabel, networkLabel string,
) ([]*pb.RateLimitResponse_DescriptorStatus, bool, string) {
	if b.admission != nil {
		select {
		case b.admission <- struct{}{}:
			// Got a slot — proceed.
		default:
			// Admission semaphore is full: too many in-flight Redis calls.
			// Fail-open immediately without spawning a goroutine. This is
			// the load-shedding path that prevents goroutine accumulation.
			if b.admissionShedded != nil {
				b.admissionShedded.Inc()
			}
			telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
				"", networkLabel, userLabel, "",
				b.Id, method, "admission_full",
			).Inc()
			return nil, true, "admission_full"
		}
	}

	if b.inflightGauge != nil {
		b.inflightGauge.Inc()
	}

	start := time.Now()
	resultCh := make(chan []*pb.RateLimitResponse_DescriptorStatus, 1)
	go func() {
		// Always release the admission slot and decrement the in-flight gauge,
		// even if cache.DoLimit panics — checkError() in envoy/ratelimit panics
		// on Redis errors, so this is a real concern.
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"ratelimiter-redis-dolimit",
					"budget:"+b.Id,
					common.ErrorFingerprint(rec),
				).Inc()
				b.logger.Error().
					Interface("panic", rec).
					Str("method", method).
					Msg("panic recovered during Redis DoLimit (rate limiting fails open for this request)")
				// Closed-but-non-nil channel: signal that we got nothing back.
				select {
				case resultCh <- nil:
				default:
				}
			}
			if b.inflightGauge != nil {
				b.inflightGauge.Dec()
			}
			if b.admission != nil {
				select {
				case <-b.admission:
				default:
				}
			}
		}()
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
		// Observe duration only for non-timeout cases — timeouts are tracked
		// by the timeout branch below to keep buckets clean.
		dur := time.Since(start).Seconds()
		if statuses != nil {
			isOverLimit := len(statuses) > 0 && statuses[0].Code == pb.RateLimitResponse_OVER_LIMIT
			if isOverLimit && b.durationOverlimit != nil {
				b.durationOverlimit.Observe(dur)
			} else if !isOverLimit && b.durationOK != nil {
				b.durationOK.Observe(dur)
			}
		} else if b.durationFailopen != nil {
			// Panic path — recovered above, count as fail-open.
			b.durationFailopen.Observe(dur)
		}
		return statuses, false, ""

	case <-timer.C:
		if b.durationFailopen != nil {
			b.durationFailopen.Observe(time.Since(start).Seconds())
		}
		// Sample the warn log; under sustained pressure this fires hundreds of
		// times per second and dwarfs the rest of the log volume.
		if b.logger.GetLevel() <= zerolog.DebugLevel {
			b.logger.Debug().
				Str("budget", b.Id).
				Str("method", method).
				Dur("timeout", b.maxTimeout).
				Msg("rate limiter timeout exceeded, failing open")
		}

		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			"", networkLabel, userLabel, "",
			b.Id, method, "limit_timeout",
		).Inc()

		return nil, true, "limit_timeout"
	}
}
