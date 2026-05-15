package upstream

import (
	"context"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/limiter"
	"github.com/envoyproxy/ratelimit/src/settings"
	"github.com/envoyproxy/ratelimit/src/stats"
	"github.com/envoyproxy/ratelimit/src/utils"
	gostats "github.com/lyft/gostats"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// blockingCache simulates a Redis client that takes `delay` to respond and
// optionally panics like envoyproxy/ratelimit's checkError() does on failures.
// It also exposes counters so tests can observe in-flight depth.
type blockingCache struct {
	inner     limiter.RateLimitCache
	delay     time.Duration
	inflight  atomic.Int64
	maxSeen   atomic.Int64
	completed atomic.Int64
	panicRate float64 // 0..1 chance of panicking after the delay
}

func (b *blockingCache) DoLimit(ctx context.Context, req *pb.RateLimitRequest, limits []*config.RateLimit) []*pb.RateLimitResponse_DescriptorStatus {
	cur := b.inflight.Add(1)
	defer b.inflight.Add(-1)
	for {
		seen := b.maxSeen.Load()
		if cur <= seen || b.maxSeen.CompareAndSwap(seen, cur) {
			break
		}
	}
	time.Sleep(b.delay)
	if b.panicRate > 0 && rand.Float64() < b.panicRate {
		panic("simulated radix panic during DoLimit")
	}
	b.completed.Add(1)
	return b.inner.DoLimit(ctx, req, limits)
}

func (b *blockingCache) Flush() {}

func (b *blockingCache) IncreaseLimitByOne(ctx context.Context, key string, limits []*config.RateLimit) {
}

// buildLeakTestBudget creates a budget configured the same way prod is for the
// edge-prod-multi-region target: a Redis-style cache wrapped in maxTimeout, and
// admission cap derived from the connection pool size.
func buildLeakTestBudget(t testing.TB, redisDelay, maxTimeout time.Duration, admissionCap int, panicRate float64) (*RateLimiterBudget, *blockingCache) {
	t.Helper()

	// Initialise telemetry buckets so the histogram observers used by the
	// budget hot path are non-nil even in a single-test invocation.
	_ = telemetry.SetHistogramBuckets("0.05,0.5,5,30")

	store := gostats.NewStore(gostats.NewNullSink(), false)
	mgr := stats.NewStatManager(store, settings.NewSettings())

	mem := NewMemoryRateLimitCache(
		utils.NewTimeSourceImpl(),
		rand.New(rand.NewSource(1)),
		0,
		0.8,
		"erpc_rl_",
		mgr,
	)
	cache := &blockingCache{inner: mem, delay: redisDelay, panicRate: panicRate}

	rules := []*RateLimitRule{
		{
			Config: &common.RateLimitRuleConfig{
				Method:   "*",
				MaxCount: 1_000_000_000,
				Period:   common.RateLimitPeriodSecond,
			},
		},
	}

	logger := zerolog.Nop()
	registry := &RateLimitersRegistry{
		statsManager: mgr,
		envoyCache:   cache,
	}
	budget := &RateLimiterBudget{
		logger:     &logger,
		Id:         "leak-test",
		Rules:      rules,
		registry:   registry,
		maxTimeout: maxTimeout,
	}
	if admissionCap > 0 {
		budget.admission = make(chan struct{}, admissionCap)
	}
	// Pre-resolve metric handles so the hot path exercises the same code as
	// production. WithLabelValues lazily creates the child metric so this is
	// safe even when SetHistogramBuckets has not been called.
	budget.inflightGauge = telemetry.MetricRateLimiterRemoteInflight.WithLabelValues(budget.Id)
	budget.admissionShedded = telemetry.MetricRateLimiterRemoteAdmissionSheddedTotal.WithLabelValues(budget.Id)
	if telemetry.MetricRateLimiterRemoteDuration != nil {
		budget.durationOK = telemetry.MetricRateLimiterRemoteDuration.WithLabelValues(budget.Id, "ok")
		budget.durationOverlimit = telemetry.MetricRateLimiterRemoteDuration.WithLabelValues(budget.Id, "over_limit")
		budget.durationFailopen = telemetry.MetricRateLimiterRemoteDuration.WithLabelValues(budget.Id, "fail_open")
	}
	return budget, cache
}

// TestRateLimiterBudget_NoGoroutineLeakUnderRedisStall reproduces the
// 2026-05-07 cronos receipts incident: Redis answers slowly (5s) while a
// large burst of concurrent rate-limit checks arrive at the budget.
//
// Pre-fix behaviour was unbounded goroutine growth (one per check, lifetime
// ~= Redis answer time) which under sustained 70k+ checks/min/machine drove
// goroutine counts to 25k+ and triggered a CPU/GC death spiral.
//
// Post-fix expectation:
//
//   - In-flight Redis calls (and therefore Redis-bound goroutines) stay
//     bounded by the per-budget admission cap.
//   - Excess checks fail open via the "admission_full" reason instead of
//     queueing a new goroutine.
//   - After the burst completes, every Redis-bound goroutine drains and the
//     in-flight gauge returns to zero (no leak).
func TestRateLimiterBudget_NoGoroutineLeakUnderRedisStall(t *testing.T) {
	const (
		redisDelay   = 200 * time.Millisecond // slow but tractable for tests
		maxTimeout   = 50 * time.Millisecond  // mirrors original prod config
		admissionCap = 64                     // small cap to force shedding
		burstSize    = 5000                   // far exceeds the cap
	)

	budget, cache := buildLeakTestBudget(t, redisDelay, maxTimeout, admissionCap, 0)

	baseline := runtime.NumGoroutine()
	t.Logf("baseline goroutines=%d", baseline)

	allowed := atomic.Int64{}
	var wg sync.WaitGroup
	wg.Add(burstSize)
	startBurst := time.Now()
	for i := 0; i < burstSize; i++ {
		go func() {
			defer wg.Done()
			ok, err := budget.TryAcquirePermit(context.Background(), "proj", nil, "eth_call", "", "", "", "")
			require.NoError(t, err)
			if ok {
				allowed.Add(1)
			}
		}()
	}

	// Wait for every TryAcquirePermit caller to return. Each caller either
	// returns immediately (admission_full) or after the local maxTimeout
	// (50ms) — both are << redisDelay (200ms), so all callers return well
	// before the first Redis call finishes. After wg.Wait the only
	// goroutines added on top of baseline are the Redis-bound ones inside
	// cache.DoLimit. Counting them this way gives us a clean measurement
	// of the actual leak indicator (the runtime.NumGoroutine() during the
	// burst itself is polluted by the burstSize caller goroutines, none of
	// which are part of the leak).
	wg.Wait()
	burstFanInDur := time.Since(startBurst)
	postBurstGoroutines := runtime.NumGoroutine()
	maxInflightDuringBurst := cache.maxSeen.Load()
	currentInflight := cache.inflight.Load()
	t.Logf("after permits returned: goroutines=%d (delta from baseline=%d), allowed=%d, completedRedisCalls=%d, currentRedisInflight=%d, maxInflightInRedis=%d, burstFanIn=%v",
		postBurstGoroutines, postBurstGoroutines-baseline,
		allowed.Load(), cache.completed.Load(),
		currentInflight, maxInflightDuringBurst, burstFanInDur)

	// All TryAcquirePermit calls must have returned (every caller is fail-
	// opened: 100% allowed) — the leak fix MUST NOT change the user-visible
	// fail-open contract.
	assert.Equal(t, int64(burstSize), allowed.Load(),
		"every request must fail-open under stall: caller-visible behaviour unchanged")

	// Critical leak invariant #1: max concurrent Redis calls bounded by cap.
	// Pre-fix this would equal burstSize (one Redis-bound goroutine per
	// caller). Post-fix this must equal admissionCap.
	// We allow +1 fudge because the in-flight counter is bumped inside the
	// goroutine after the cap admits it — there's a tiny race window where
	// admissionCap calls have started but the (admissionCap+1)th hasn't
	// observed the full semaphore yet.
	assert.LessOrEqual(t, maxInflightDuringBurst, int64(admissionCap+1),
		"max concurrent Redis calls must be bounded by admission cap (saw %d, cap %d) — pre-fix this would equal burstSize=%d",
		maxInflightDuringBurst, admissionCap, burstSize)

	// Critical leak invariant #2: post-burst goroutine delta stays in
	// the same order of magnitude as admissionCap — NOT ~burstSize like
	// the pre-fix world (5000+). The admission cap means at most
	// `admissionCap` goroutines are still alive in cache.DoLimit;
	// everything else has already returned (fail-open path doesn't
	// spawn).
	//
	// The slack is loose-ish (2× admissionCap + small constant) to
	// absorb CI scheduler jitter: under heavy load the snapshot can
	// catch caller goroutines mid-cleanup or stragglers that haven't
	// returned-and-been-GCed yet. The check that actually matters is
	// the "not pre-fix levels" guarantee — even 2× cap is two orders
	// of magnitude under the pre-fix leak signature.
	maxAllowedDelta := 2*admissionCap + 16
	assert.LessOrEqual(t, postBurstGoroutines-baseline, maxAllowedDelta,
		"post-burst goroutines must stay within 2×admissionCap+16 (got delta %d, cap %d, slack %d, burstSize %d) — pre-fix would be ~burstSize",
		postBurstGoroutines-baseline, admissionCap, maxAllowedDelta, burstSize)

	// Now wait for all Redis-bound goroutines to drain. Each goroutine
	// lives for redisDelay (we don't kill them on timeout); the last one
	// to start was admitted near the end of the burst window.
	drainDeadline := time.Now().Add(2 * (redisDelay + maxTimeout))
	for time.Now().Before(drainDeadline) {
		if cache.inflight.Load() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Allow a few ms for the budget's deferred releases to settle.
	time.Sleep(20 * time.Millisecond)
	runtime.GC()
	finalGoroutines := runtime.NumGoroutine()

	t.Logf("after drain: goroutines=%d (delta from baseline=%d), inflight=%d",
		finalGoroutines, finalGoroutines-baseline, cache.inflight.Load())

	// Critical leak invariant #3: after the burst completes, every Redis-
	// bound goroutine MUST have drained. We allow a small fudge (tests
	// commonly leave a few goroutines around for runtime bookkeeping) but
	// not anywhere near admission cap or burst size.
	assert.Zero(t, cache.inflight.Load(), "all Redis-bound goroutines must have completed")
	assert.LessOrEqual(t, finalGoroutines-baseline, 8,
		"goroutine count must return to baseline (within slack), got delta %d",
		finalGoroutines-baseline)
}

// TestRateLimiterBudget_AdmissionPanicSafety verifies that a panicking
// remote (envoyproxy/ratelimit panics on Redis errors via checkError) does
// NOT leak admission slots or goroutines. Pre-fix the panic would have
// killed the whole process; we now recover and release the slot.
func TestRateLimiterBudget_AdmissionPanicSafety(t *testing.T) {
	const (
		redisDelay   = 50 * time.Millisecond
		maxTimeout   = 25 * time.Millisecond
		admissionCap = 16
		iterations   = 200
	)

	// 100% panic rate so every spawned goroutine exercises the recover path.
	budget, cache := buildLeakTestBudget(t, redisDelay, maxTimeout, admissionCap, 1.0)

	baseline := runtime.NumGoroutine()
	for i := 0; i < iterations; i++ {
		_, err := budget.TryAcquirePermit(context.Background(), "proj", nil, "eth_call", "", "", "", "")
		require.NoError(t, err)
	}

	// Wait for in-flight goroutines (panicking and recovering) to drain.
	deadline := time.Now().Add(2 * redisDelay)
	for time.Now().Before(deadline) && cache.inflight.Load() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	runtime.GC()

	finalGoroutines := runtime.NumGoroutine()
	t.Logf("after panic burst: goroutines=%d (delta=%d), inflight=%d",
		finalGoroutines, finalGoroutines-baseline, cache.inflight.Load())

	assert.Zero(t, cache.inflight.Load(),
		"recovered panic goroutines must release admission slots")
	assert.LessOrEqual(t, finalGoroutines-baseline, 8,
		"panic recovery must not leak goroutines (delta=%d)", finalGoroutines-baseline)

	// Sanity: admission semaphore must be empty so subsequent traffic flows.
	assert.Equal(t, 0, len(budget.admission),
		"admission semaphore must be drained after panicking goroutines complete")
}
