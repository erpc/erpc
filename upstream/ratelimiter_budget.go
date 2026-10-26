package upstream

import (
	"context"
	"fmt"
	"math"
	"sync"
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
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type RateLimiterBudget struct {
	logger     *zerolog.Logger
	Id         string
	Rules      []*RateLimitRule
	registry   *RateLimitersRegistry
	rulesMu    sync.RWMutex
	maxTimeout time.Duration

	// creditUnits is read-only after construction.
	creditUnits map[string]int64
	// hasRuleCountMode lets TryAcquirePermit keep its whole-budget zero-cost
	// fast path: without a rule-level override every rule inherits the caller.
	hasRuleCountMode bool

	// admission is a buffered semaphore that bounds the number of concurrent
	// in-flight remote (Redis) DoLimit calls per budget; see doLimit. When it
	// is full the request is a store failure ("admission_full") and no
	// goroutine is spawned. Nil when no remote cache is in use.
	admission chan struct{}

	// onFailure decides requests the remote store could not (see doLimit).
	onFailure common.RateLimitStoreFailureMode
	// local counts every request in-process at maxCount × localFactor, so
	// onFailure: local decides with a warm counter. Nil unless that mode.
	local       limiter.RateLimitCache
	localFactor float64

	// inflight gauge, kept here for fast hot-path access without a labels
	// lookup on every call. Refreshed at registration time.
	inflightGauge     prometheus.Gauge
	admissionShedded  prometheus.Counter
	durationFailopen  prometheus.Observer
	durationOK        prometheus.Observer
	durationOverlimit prometheus.Observer
}

type RateLimitRule struct {
	Config *common.RateLimitRuleConfig

	// key isolates this rule's counter from other rules in the budget. The envoy
	// cache key excludes the limit value, so overlapping rules would otherwise
	// share one counter and each increment it.
	key string
}

// ruleKeyFor returns a stable identity for a rule. Two byte-identical rules map
// to the same key, which is correct: they are the same limit. Resolved once at
// construction so an autotuned MaxCount does not move the key mid-window.
func ruleKeyFor(c *common.RateLimitRuleConfig) string {
	scope := c.ScopeString()
	if scope == "" {
		scope = "global"
	}
	mode := c.CountMode
	if mode == "" {
		mode = common.RateLimitCountModeRequest
	}
	return fmt.Sprintf("method:%s scope:%s maxCount:%d period:%s countMode:%s", c.Method, scope, c.MaxCount, c.Period, mode)
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
	telemetry.MetricRateLimiterBudgetMaxCount.WithLabelValues(b.Id, rule.Config.Method, rule.Config.ScopeString(), rule.Config.CountModeString()).Set(float64(newMaxCount))
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
	telemetry.MetricRateLimiterBudgetMaxCount.WithLabelValues(b.Id, rule.Config.Method, rule.Config.ScopeString(), rule.Config.CountModeString()).Set(float64(next))
	return prev, next, true
}

// getCache returns the current cache from the registry (thread-safe)
func (b *RateLimiterBudget) getCache() limiter.RateLimitCache {
	return b.registry.GetCache()
}

// PermitCost is what one acquisition consumes from a budget.
type PermitCost struct {
	// Hits is what a credit-counting rule consumes. Zero means exempt.
	Hits uint32
	// Mode is the unit Hits is in; empty means request counting. A rule may
	// override it with its own countMode.
	Mode common.RateLimitCountMode
}

// requestPermit is the default cost: one request, request counting.
var requestPermit = PermitCost{Hits: 1, Mode: common.RateLimitCountModeRequest}

// TryAcquirePermit evaluates every rule matching method in one cache call, so a
// request costs one Redis pipeline and one admission slot however many rules it
// meets. Returns true if allowed, false if rate limited.
//
// cost is optional (only the first value is read; default one request hit). In
// credit mode it carries the request's estimated vendor credit-unit cost, and
// the matching rules are enforced as one pool shared by every method. What a
// rule actually charges is resolved per rule in resolveCost.
func (b *RateLimiterBudget) TryAcquirePermit(ctx context.Context, projectId string, req *common.NormalizedRequest, method string, vendor string, upstreamId string, authLabel string, origin string, cost ...PermitCost) (bool, error) {
	permit := requestPermit
	if len(cost) > 0 {
		permit = cost[0]
		if permit.Mode == "" {
			permit.Mode = common.RateLimitCountModeRequest
		}
	}
	// Whole-budget shortcut, valid only when no rule overrides the mode;
	// otherwise a request-counting rule would be skipped by a 0-credit caller.
	// buildLimitRequest handles that case per rule.
	if permit.Hits == 0 && !b.hasRuleCountMode {
		return true, nil
	}

	rules, err := b.GetRulesByMethod(method)
	if err != nil {
		return false, err
	}
	if len(rules) == 0 {
		return true, nil
	}

	var userLabel, agentName, networkLabel, finality, clientIP string
	if req != nil {
		userLabel = req.UserId()
		agentName = req.AgentName()
		networkLabel = req.NetworkId()
		finality = req.Finality(ctx).String()
		clientIP = req.ClientIP()
	}
	for _, rule := range rules {
		if (rule.Config.PerIP || rule.Config.PerUser || rule.Config.PerNetwork) && req == nil {
			return false, fmt.Errorf("request cannot be nil when ratelimiter rule has perIP/perUser/perNetwork")
		}
	}

	ctx, span := common.StartDetailSpan(ctx, "RateLimiter.TryAcquirePermit",
		trace.WithAttributes(
			attribute.String("budget", b.Id),
			attribute.String("method", method),
		),
	)
	defer span.End()

	rlReq, limits, charged := b.buildLimitRequest(rules, method, clientIP, userLabel, networkLabel, permit)
	if len(charged) == 0 {
		return true, nil
	}

	// The local limiter counts every request, so it is warm when the store fails.
	var localStatuses []*pb.RateLimitResponse_DescriptorStatus
	if b.local != nil {
		localStatuses = b.local.DoLimit(ctx, rlReq, b.scaledLimits(rlReq, limits))
	}

	statuses, failure := b.doLimit(ctx, rlReq, limits)
	if failure != "" {
		statuses = b.onStoreFailure(failure, localStatuses, method, userLabel, networkLabel)
	}

	blocking := overLimitIndex(statuses)
	if blocking < 0 {
		return true, nil
	}
	scope := ""
	if blocking < len(charged) {
		scope = charged[blocking].Config.ScopeString()
	}
	telemetry.CounterHandle(
		telemetry.MetricRateLimitsTotal,
		projectId, networkLabel, vendor, upstreamId, method, finality,
		userLabel, agentName, b.Id, scope, authLabel, origin,
	).Inc()
	return false, nil
}

// buildLimitRequest turns the matching rules into one envoy request with a
// descriptor and a limit per charged rule. charged[i] is the rule behind
// descriptor i; a credit rule pricing the call at 0 is left out.
func (b *RateLimiterBudget) buildLimitRequest(rules []*RateLimitRule, method, clientIP, userLabel, networkLabel string, permit PermitCost) (*pb.RateLimitRequest, []*config.RateLimit, []*RateLimitRule) {
	descriptors := make([]*pb_struct.RateLimitDescriptor, 0, len(rules))
	limits := make([]*config.RateLimit, 0, len(rules))
	charged := make([]*RateLimitRule, 0, len(rules))

	b.rulesMu.RLock()
	defer b.rulesMu.RUnlock()
	for _, rule := range rules {
		mode, hits := b.resolveCost(rule, method, permit)
		if mode == common.RateLimitCountModeCredit && hits == 0 {
			continue
		}
		// "rule" gives each rule its own counter; without it rules resolving to
		// the same {method, scope} at the same period collide.
		entries := []*pb_struct.RateLimitDescriptor_Entry{{Key: "rule", Value: rule.key}}
		// Request counting is per method. Credit counting is not: the cost is
		// already carried by the addend, so every method draws from one pool and
		// the budget behaves as the provider wallet it is meant to model.
		if mode != common.RateLimitCountModeCredit {
			entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "method", Value: method})
		}
		if rule.Config.PerIP && clientIP != "" && clientIP != "n/a" {
			entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "ip", Value: clientIP})
		}
		if rule.Config.PerUser && userLabel != "" && userLabel != "n/a" {
			entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "user", Value: userLabel})
		}
		if rule.Config.PerNetwork && networkLabel != "" && networkLabel != "n/a" {
			entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "network", Value: networkLabel})
		}
		descriptors = append(descriptors, &pb_struct.RateLimitDescriptor{
			Entries:    entries,
			HitsAddend: wrapperspb.UInt64(uint64(hits)),
		})

		rlStats := b.registry.statsManager.NewStats(b.Id + ".method_" + method + rule.statsKeySuffix())
		limits = append(limits, config.NewRateLimit(rule.Config.MaxCount, rule.Config.Period.Unit(), rlStats, false, false, "", nil, false))
		charged = append(charged, rule)
	}
	return &pb.RateLimitRequest{Domain: b.Id, Descriptors: descriptors}, limits, charged
}

// scaledLimits returns limits at maxCount × localFactor, raised to the
// request's own hits so a credit rule never scales below one call's cost, and
// capped at maxCount.
func (b *RateLimiterBudget) scaledLimits(rlReq *pb.RateLimitRequest, limits []*config.RateLimit) []*config.RateLimit {
	scaled := make([]*config.RateLimit, len(limits))
	for i, l := range limits {
		maxCount := l.Limit.RequestsPerUnit
		n := uint32(math.Ceil(float64(maxCount) * b.localFactor))
		n = min(max(n, uint32(rlReq.Descriptors[i].GetHitsAddend().GetValue()), 1), maxCount)
		scaled[i] = config.NewRateLimit(n, l.Limit.Unit, l.Stats, false, false, "", nil, false)
	}
	return scaled
}

func overLimitIndex(statuses []*pb.RateLimitResponse_DescriptorStatus) int {
	for i, s := range statuses {
		if s != nil && s.Code == pb.RateLimitResponse_OVER_LIMIT {
			return i
		}
	}
	return -1
}

// onStoreFailure returns the statuses the budget's onFailure mode decides for a
// request the store could not answer; no OVER_LIMIT status admits it.
func (b *RateLimiterBudget) onStoreFailure(reason string, localStatuses []*pb.RateLimitResponse_DescriptorStatus, method, userLabel, networkLabel string) []*pb.RateLimitResponse_DescriptorStatus {
	var statuses []*pb.RateLimitResponse_DescriptorStatus
	decision := "allow"
	switch b.onFailure {
	case common.RateLimitStoreFailureDeny:
		statuses, decision = deniedStatus, "deny"
	case common.RateLimitStoreFailureLocal:
		statuses = localStatuses
		if overLimitIndex(localStatuses) >= 0 {
			decision = "local_deny"
		} else {
			decision = "local_allow"
		}
	default:
		telemetry.MetricRateLimiterFailopenTotal.WithLabelValues(
			"", networkLabel, userLabel, "",
			b.Id, method, reason,
		).Inc()
	}
	telemetry.MetricRateLimiterStoreFailureTotal.WithLabelValues(b.Id, reason, decision).Inc()
	return statuses
}

var deniedStatus = []*pb.RateLimitResponse_DescriptorStatus{{Code: pb.RateLimitResponse_OVER_LIMIT}}

// Reasons the store could not decide a request.
const (
	storeUnavailable = "store_unavailable"
	storeError       = "store_error"
	admissionFull    = "admission_full"
	limitTimeout     = "limit_timeout"
)

// resolveCost returns the mode and hits this rule charges for one call. The
// rule's countMode wins and an unset one inherits the caller's, so a rule that
// states its mode is honored even under a credit caller. A credit rule prices
// from the caller's vendor estimate if there is one, else from creditUnits.
func (b *RateLimiterBudget) resolveCost(rule *RateLimitRule, method string, permit PermitCost) (common.RateLimitCountMode, uint32) {
	mode := rule.Config.CountMode
	if mode == "" {
		mode = permit.Mode
	}
	if mode != common.RateLimitCountModeCredit {
		return common.RateLimitCountModeRequest, 1
	}
	if permit.Mode == common.RateLimitCountModeCredit {
		return mode, permit.Hits
	}
	units := common.ResolveCreditUnits(nil, b.creditUnits, method)
	if units <= 0 {
		return mode, 0
	}
	if units > math.MaxUint32 {
		return mode, math.MaxUint32
	}
	return mode, uint32(units)
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

// doLimit asks the cache for one request's statuses. failure is empty when the
// cache answered, else the reason it did not; onStoreFailure decides those.
//
// A remote (redis) cache runs under a timeout AND a per-budget admission cap.
// The envoyproxy/ratelimit cache + radix client do not honor context
// cancellation for the actual Redis I/O, so every timed-out call keeps a
// goroutine until Redis answers; uncapped, that spiralled into runaway
// goroutine growth (2026-05-07 cronos receipts incident). The goroutine still
// finishes the call so Redis state stays correct, and holds its admission slot
// for as long as the call truly takes, not just the local timeout.
func (b *RateLimiterBudget) doLimit(
	ctx context.Context,
	rlReq *pb.RateLimitRequest,
	limits []*config.RateLimit,
) (statuses []*pb.RateLimitResponse_DescriptorStatus, failure string) {
	cache := b.getCache()
	if cache == nil {
		return nil, storeUnavailable
	}
	if b.maxTimeout <= 0 {
		return cache.DoLimit(ctx, rlReq, limits), ""
	}

	_, span := common.StartSpan(ctx, "RateLimiter.DoLimit",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("budget", b.Id),
			attribute.Int("descriptors", len(rlReq.Descriptors)),
		),
	)
	defer func() {
		switch {
		case failure != "":
			span.SetAttributes(attribute.String("result", "store_failure"), attribute.String("reason", failure))
		case overLimitIndex(statuses) >= 0:
			span.SetAttributes(attribute.String("result", "over_limit"))
		default:
			span.SetAttributes(attribute.String("result", "ok"))
		}
		span.End()
	}()

	if b.admission != nil {
		select {
		case b.admission <- struct{}{}:
		default:
			// Too many in-flight Redis calls: shed without spawning a goroutine.
			if b.admissionShedded != nil {
				b.admissionShedded.Inc()
			}
			return nil, admissionFull
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
		// on Redis errors.
		defer func() {
			if rec := recover(); rec != nil {
				telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
					"ratelimiter-redis-dolimit",
					"budget:"+b.Id,
					common.ErrorFingerprint(rec),
				).Inc()
				if b.logger.GetLevel() <= zerolog.DebugLevel {
					b.logger.Debug().Interface("panic", rec).Msg("redis DoLimit failed")
				}
				resultCh <- nil
			}
			if b.inflightGauge != nil {
				b.inflightGauge.Dec()
			}
			if b.admission != nil {
				<-b.admission
			}
		}()
		resultCh <- cache.DoLimit(ctx, rlReq, limits)
	}()

	timer := time.NewTimer(b.maxTimeout)
	defer timer.Stop()
	select {
	case statuses = <-resultCh:
		dur := time.Since(start).Seconds()
		switch {
		case statuses == nil:
			if b.durationFailopen != nil {
				b.durationFailopen.Observe(dur)
			}
			return nil, storeError
		case overLimitIndex(statuses) >= 0:
			if b.durationOverlimit != nil {
				b.durationOverlimit.Observe(dur)
			}
		default:
			if b.durationOK != nil {
				b.durationOK.Observe(dur)
			}
		}
		return statuses, ""

	case <-timer.C:
		if b.durationFailopen != nil {
			b.durationFailopen.Observe(time.Since(start).Seconds())
		}
		return nil, limitTimeout
	}
}
