package upstream

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
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

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func init() { util.ConfigureTestLogger() }

// delayedCache wraps a cache and adds artificial latency to simulate Redis
type delayedCache struct {
	inner limiter.RateLimitCache
	delay time.Duration
}

func (d *delayedCache) DoLimit(ctx context.Context, req *pb.RateLimitRequest, limits []*config.RateLimit) []*pb.RateLimitResponse_DescriptorStatus {
	time.Sleep(d.delay)
	return d.inner.DoLimit(ctx, req, limits)
}

func (d *delayedCache) Flush() {}

func (d *delayedCache) IncreaseLimitByOne(ctx context.Context, key string, limits []*config.RateLimit) {
}

func buildBenchBudget(numRules int, perUser, perIP, perNetwork bool) *RateLimiterBudget {
	store := gostats.NewStore(gostats.NewNullSink(), false)
	mgr := stats.NewStatManager(store, settings.NewSettings())

	cache := NewMemoryRateLimitCache(
		utils.NewTimeSourceImpl(),
		rand.New(rand.NewSource(1)),
		0,
		0.8,
		"erpc_rl_",
		mgr,
	)

	rules := make([]*RateLimitRule, numRules)
	for i := 0; i < numRules; i++ {
		rules[i] = &RateLimitRule{
			Config: &common.RateLimitRuleConfig{
				Method:     "eth_*", // Wildcard to match
				MaxCount:   1_000_000_000,
				Period:     common.RateLimitPeriodSecond,
				PerUser:    perUser,
				PerIP:      perIP,
				PerNetwork: perNetwork,
			},
		}
	}

	logger := zerolog.Nop()
	registry := &RateLimitersRegistry{
		statsManager: mgr,
		envoyCache:   cache,
	}
	return &RateLimiterBudget{
		logger:   &logger,
		Id:       "bench-budget",
		Rules:    rules,
		registry: registry,
	}
}

func makeBenchRequest() *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`))
	req.SetClientIP("10.0.0.1")
	req.SetUser(&common.User{Id: "user123"})
	return req
}

// BenchmarkTryAcquirePermit_SingleRule tests the most common case: 1 matching rule
func BenchmarkTryAcquirePermit_SingleRule(b *testing.B) {
	budget := buildBenchBudget(1, false, false, false)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = budget.TryAcquirePermit(ctx, "proj", nil, "eth_getBalance", "", "", "", "")
	}
}

// BenchmarkTryAcquirePermit_SingleRule_Parallel tests concurrent single-rule evaluation
func BenchmarkTryAcquirePermit_SingleRule_Parallel(b *testing.B) {
	budget := buildBenchBudget(1, false, false, false)

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			_, _ = budget.TryAcquirePermit(ctx, "proj", nil, "eth_getBalance", "", "", "", "")
		}
	})
}

// BenchmarkTryAcquirePermit_MultipleRules tests multiple matching rules (sequential eval)
func BenchmarkTryAcquirePermit_MultipleRules(b *testing.B) {
	for _, numRules := range []int{2, 4, 8} {
		b.Run(fmt.Sprintf("rules_%d", numRules), func(b *testing.B) {
			budget := buildBenchBudget(numRules, false, false, false)
			ctx := context.Background()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = budget.TryAcquirePermit(ctx, "proj", nil, "eth_getBalance", "", "", "", "")
			}
		})
	}
}

// BenchmarkTryAcquirePermit_WithRequest tests with full request context
func BenchmarkTryAcquirePermit_WithRequest(b *testing.B) {
	budget := buildBenchBudget(1, true, true, true)
	ctx := context.Background()
	req := makeBenchRequest()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = budget.TryAcquirePermit(ctx, "proj", req, "eth_getBalance", "", "", "", "")
	}
}

// BenchmarkTryAcquirePermit_WithRequest_Parallel tests concurrent with request context
func BenchmarkTryAcquirePermit_WithRequest_Parallel(b *testing.B) {
	budget := buildBenchBudget(1, true, true, true)

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		req := makeBenchRequest()
		for pb.Next() {
			_, _ = budget.TryAcquirePermit(ctx, "proj", req, "eth_getBalance", "", "", "", "")
		}
	})
}

// BenchmarkTryAcquirePermit_NoRulesMatch tests the fast path when no rules match
func BenchmarkTryAcquirePermit_NoRulesMatch(b *testing.B) {
	budget := buildBenchBudget(1, false, false, false)
	// Change rule to not match
	budget.Rules[0].Config.Method = "debug_*"
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = budget.TryAcquirePermit(ctx, "proj", nil, "eth_getBalance", "", "", "", "")
	}
}

// BenchmarkTryAcquirePermit_NilCache tests the nil cache fast path
func BenchmarkTryAcquirePermit_NilCache(b *testing.B) {
	budget := buildBenchBudget(1, false, false, false)
	budget.registry.envoyCache = nil // Simulate Redis not connected yet
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = budget.TryAcquirePermit(ctx, "proj", nil, "eth_getBalance", "", "", "", "")
	}
}

// buildBenchBudgetWithDelay creates a budget with a delayed cache (simulating Redis latency)
func buildBenchBudgetWithDelay(numRules int, delay time.Duration) *RateLimiterBudget {
	store := gostats.NewStore(gostats.NewNullSink(), false)
	mgr := stats.NewStatManager(store, settings.NewSettings())

	innerCache := NewMemoryRateLimitCache(
		utils.NewTimeSourceImpl(),
		rand.New(rand.NewSource(1)),
		0,
		0.8,
		"erpc_rl_",
		mgr,
	)

	rules := make([]*RateLimitRule, numRules)
	for i := 0; i < numRules; i++ {
		rules[i] = &RateLimitRule{
			Config: &common.RateLimitRuleConfig{
				Method:   "eth_*",
				MaxCount: 1_000_000_000,
				Period:   common.RateLimitPeriodSecond,
			},
		}
	}

	logger := zerolog.Nop()
	registry := &RateLimitersRegistry{
		statsManager: mgr,
		envoyCache:   &delayedCache{inner: innerCache, delay: delay},
	}
	return &RateLimiterBudget{
		logger:   &logger,
		Id:       "bench-budget",
		Rules:    rules,
		registry: registry,
	}
}

// BenchmarkTryAcquirePermit_WithRedisLatency simulates Redis with 5ms latency
func BenchmarkTryAcquirePermit_WithRedisLatency(b *testing.B) {
	delays := []time.Duration{1 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond}
	ruleCounts := []int{1, 2, 4, 8}

	for _, delay := range delays {
		for _, numRules := range ruleCounts {
			name := fmt.Sprintf("delay_%dms_rules_%d", delay.Milliseconds(), numRules)
			b.Run(name, func(b *testing.B) {
				budget := buildBenchBudgetWithDelay(numRules, delay)
				ctx := context.Background()

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, _ = budget.TryAcquirePermit(ctx, "proj", nil, "eth_getBalance", "", "", "", "")
				}
			})
		}
	}
}
