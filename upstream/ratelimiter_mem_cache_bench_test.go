package upstream

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"testing"
	"time"

	pb_struct "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/envoyproxy/ratelimit/src/config"
	"github.com/envoyproxy/ratelimit/src/settings"
	"github.com/envoyproxy/ratelimit/src/stats"
	"github.com/envoyproxy/ratelimit/src/utils"
	gostats "github.com/lyft/gostats"

	"github.com/erpc/erpc/util"
)

func init() { util.ConfigureTestLogger() }

// buildBenchCache constructs a memory rate limit cache and a high-capacity
// per-second limit to avoid over-limit paths during benchmarks.
func buildBenchCache() (cache interface {
	DoLimit(context.Context, *pb.RateLimitRequest, []*config.RateLimit) []*pb.RateLimitResponse_DescriptorStatus
}, limit *config.RateLimit) {
	store := gostats.NewStore(gostats.NewNullSink(), false)
	mgr := stats.NewStatManager(store, settings.NewSettings())

	// Use deterministic RNG and no expiration jitter
	cacheImpl := NewMemoryRateLimitCache(
		utils.NewTimeSourceImpl(),
		rand.New(rand.NewSource(1)),
		0,
		0.8,
		"erpc_rl_",
		mgr,
	)

	// Very large max to stay within limit paths
	limit = config.NewRateLimit(
		1_000_000_000,
		pb.RateLimitResponse_RateLimit_SECOND,
		mgr.NewStats("bench"),
		false,
		false,
		"",
		nil,
		false,
	)

	return cacheImpl, limit
}

func makeReq(method, user string) *pb.RateLimitRequest {
	entries := []*pb_struct.RateLimitDescriptor_Entry{{Key: "method", Value: method}}
	if user != "" {
		entries = append(entries, &pb_struct.RateLimitDescriptor_Entry{Key: "user", Value: user})
	}
	return &pb.RateLimitRequest{
		Domain:      "bench",
		Descriptors: []*pb_struct.RateLimitDescriptor{{Entries: entries}},
		HitsAddend:  1,
	}
}

// makeReqUserIP creates a descriptor including method, user, and ip entries.
func makeReqUserIP(method, user, ip string) *pb.RateLimitRequest {
	entries := []*pb_struct.RateLimitDescriptor_Entry{
		{Key: "method", Value: method},
		{Key: "user", Value: user},
		{Key: "ip", Value: ip},
	}
	return &pb.RateLimitRequest{
		Domain:      "bench",
		Descriptors: []*pb_struct.RateLimitDescriptor{{Entries: entries}},
		HitsAddend:  1,
	}
}

func BenchmarkMemoryRateLimitCache_DoLimit_Sequential_SingleKey(b *testing.B) {
	cache, limit := buildBenchCache()
	req := makeReq("eth_getBalance", "u1")
	limits := []*config.RateLimit{limit}

	b.ReportAllocs()
	b.ResetTimer()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		_ = cache.DoLimit(ctx, req, limits)
	}
}

func BenchmarkMemoryRateLimitCache_DoLimit_Parallel_SingleKey(b *testing.B) {
	cache, limit := buildBenchCache()
	req := makeReq("eth_getBalance", "u1")
	limits := []*config.RateLimit{limit}

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			_ = cache.DoLimit(ctx, req, limits)
		}
	})
}

func BenchmarkMemoryRateLimitCache_DoLimit_Sequential_ManyKeys(b *testing.B) {
	for _, keys := range []int{1, 8, 64, 512, 4096} {
		b.Run(fmt.Sprintf("keys_%d", keys), func(b *testing.B) {
			cache, limit := buildBenchCache()
			limits := []*config.RateLimit{limit}

			// Pre-create a pool of requests that map to many cache keys
			reqs := make([]*pb.RateLimitRequest, keys)
			for i := 0; i < keys; i++ {
				reqs[i] = makeReq("eth_call", fmt.Sprintf("u%d", i))
			}

			b.ReportAllocs()
			b.ResetTimer()
			ctx := context.Background()
			idx := 0
			for i := 0; i < b.N; i++ {
				_ = cache.DoLimit(ctx, reqs[idx], limits)
				idx++
				if idx == keys {
					idx = 0
				}
			}
		})
	}
}

func BenchmarkMemoryRateLimitCache_DoLimit_Parallel_ManyKeys(b *testing.B) {
	for _, keys := range []int{1, 8, 64, 512, 4096} {
		b.Run(fmt.Sprintf("keys_%d", keys), func(b *testing.B) {
			cache, limit := buildBenchCache()
			limits := []*config.RateLimit{limit}

			reqs := make([]*pb.RateLimitRequest, keys)
			for i := 0; i < keys; i++ {
				reqs[i] = makeReq("eth_getLogs", fmt.Sprintf("u%d", i))
			}

			b.ReportAllocs()
			b.SetParallelism(runtime.GOMAXPROCS(0))
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				ctx := context.Background()
				idx := rand.Intn(keys)
				for pb.Next() {
					_ = cache.DoLimit(ctx, reqs[idx], limits)
					idx++
					if idx == keys {
						idx = 0
					}
				}
			})
		})
	}
}

// Second-window rollover cleanup cost: pre-populate many expired entries and
// measure the cost of a DoLimit call that performs the opportunistic cleanup.
func BenchmarkMemoryRateLimitCache_DoLimit_RolloverCleanup_Sequential(b *testing.B) {
	for _, keys := range []int{4096, 32768, 131072, 524288} {
		b.Run(fmt.Sprintf("expired_%d", keys), func(b *testing.B) {
			cache, limit := buildBenchCache()
			limits := []*config.RateLimit{limit}
			ctx := context.Background()
			req := makeReq("eth_call", "")
			mc := cache.(*memoryRateLimitCache)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Pre-populate expired buckets across shards to simulate previous-second window.
				now := mc.timeSrc.UnixNow()
				// Distribute keys evenly across shards
				perShard := (keys + len(mc.shards) - 1) / len(mc.shards)
				for si := range mc.shards {
					sh := &mc.shards[si]
					sh.mu.Lock()
					ex := now - 1
					m := make(map[string]uint64, perShard)
					for j := 0; j < perShard; j++ {
						m[fmt.Sprintf("roll-%d-%d", si, j)] = 1
					}
					sh.buckets[ex] = m
					sh.mu.Unlock()
				}

				_ = cache.DoLimit(ctx, req, limits)
			}
		})
	}
}

func BenchmarkMemoryRateLimitCache_DoLimit_RolloverCleanup_Parallel(b *testing.B) {
	for _, keys := range []int{4096, 32768, 131072, 524288} {
		b.Run(fmt.Sprintf("expired_%d", keys), func(b *testing.B) {
			cache, limit := buildBenchCache()
			limits := []*config.RateLimit{limit}
			ctx := context.Background()
			req := makeReq("eth_call", "")
			mc := cache.(*memoryRateLimitCache)

			// Pre-populate once before timing to simulate second rollover for the first batch.
			now := mc.timeSrc.UnixNow()
			perShard := (keys + len(mc.shards) - 1) / len(mc.shards)
			for si := range mc.shards {
				sh := &mc.shards[si]
				sh.mu.Lock()
				ex := now - 1
				m := make(map[string]uint64, perShard)
				for j := 0; j < perShard; j++ {
					m[fmt.Sprintf("roll-%d-%d", si, j)] = 1
				}
				sh.buckets[ex] = m
				sh.mu.Unlock()
			}

			b.ReportAllocs()
			b.SetParallelism(runtime.GOMAXPROCS(0))
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_ = cache.DoLimit(ctx, req, limits)
				}
			})
		})
	}
}

// Mixed workload: mostly hot calls with periodic second-boundary rollover cleanup.
func BenchmarkMemoryRateLimitCache_DoLimit_RolloverMixed_Sequential(b *testing.B) {
	for _, keys := range []int{8, 64, 512, 4096, 32768} {
		b.Run(fmt.Sprintf("mixed_%d", keys), func(b *testing.B) {
			cache, limit := buildBenchCache()
			limits := []*config.RateLimit{limit}
			ctx := context.Background()
			req := makeReq("eth_call", "")
			mc := cache.(*memoryRateLimitCache)

			cleanupEvery := 10 // simulate 1 cleanup per 10 ops
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%cleanupEvery == 0 {
					b.StopTimer()
					now := mc.timeSrc.UnixNow()
					perShard := (keys + len(mc.shards) - 1) / len(mc.shards)
					for si := range mc.shards {
						sh := &mc.shards[si]
						sh.mu.Lock()
						ex := now - 1
						m := make(map[string]uint64, perShard)
						for j := 0; j < perShard; j++ {
							m[fmt.Sprintf("mix-%d-%d", si, j)] = 1
						}
						sh.buckets[ex] = m
						sh.mu.Unlock()
					}
					b.StartTimer()
				}
				_ = cache.DoLimit(ctx, req, limits)
			}
		})
	}
}

func BenchmarkMemoryRateLimitCache_DoLimit_RolloverMixed_Parallel(b *testing.B) {
	for _, keys := range []int{8, 64, 512, 4096, 32768} {
		b.Run(fmt.Sprintf("mixed_%d", keys), func(b *testing.B) {
			cache, limit := buildBenchCache()
			limits := []*config.RateLimit{limit}
			ctx := context.Background()
			req := makeReq("eth_call", "")
			mc := cache.(*memoryRateLimitCache)

			b.ReportAllocs()
			b.SetParallelism(runtime.GOMAXPROCS(0))
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				local := rand.New(rand.NewSource(time.Now().UnixNano()))
				// Each worker performs occasional cleanups
				counter := 0
				for pb.Next() {
					if counter%10 == 0 && local.Intn(10) == 0 {
						now := mc.timeSrc.UnixNow()
						perShard := (keys + len(mc.shards) - 1) / len(mc.shards)
						for si := range mc.shards {
							sh := &mc.shards[si]
							sh.mu.Lock()
							ex := now - 1
							m := make(map[string]uint64, perShard)
							for j := 0; j < perShard; j++ {
								m[fmt.Sprintf("mix-%d-%d", si, j)] = 1
							}
							sh.buckets[ex] = m
							sh.mu.Unlock()
						}
					}
					_ = cache.DoLimit(ctx, req, limits)
					counter++
				}
			})
		})
	}
}

// Few budgets, no user/ip: simulate 4-5 budgets with only method dimension.
func BenchmarkMemoryRateLimitCache_DoLimit_FewBudgets_NoUser(b *testing.B) {
	cache, limit := buildBenchCache()
	limits := []*config.RateLimit{limit}

	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	reqs := make([]*pb.RateLimitRequest, len(methods))
	for i, m := range methods {
		reqs[i] = makeReq(m, "")
	}

	b.ReportAllocs()
	b.ResetTimer()
	ctx := context.Background()
	idx := 0
	for i := 0; i < b.N; i++ {
		_ = cache.DoLimit(ctx, reqs[idx], limits)
		idx++
		if idx == len(reqs) {
			idx = 0
		}
	}
}

func BenchmarkMemoryRateLimitCache_DoLimit_FewBudgets_NoUser_Parallel(b *testing.B) {
	cache, limit := buildBenchCache()
	limits := []*config.RateLimit{limit}

	methods := []string{"eth_getBalance", "eth_call", "eth_getLogs", "eth_getBlockByNumber", "eth_getTransactionReceipt"}
	reqs := make([]*pb.RateLimitRequest, len(methods))
	for i, m := range methods {
		reqs[i] = makeReq(m, "")
	}

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		idx := rand.Intn(len(reqs))
		for pb.Next() {
			_ = cache.DoLimit(ctx, reqs[idx], limits)
			idx++
			if idx == len(reqs) {
				idx = 0
			}
		}
	})
}

// High-cardinality: per-user only. We cycle through a large set of users.
func BenchmarkMemoryRateLimitCache_DoLimit_PerUser_Cardinality(b *testing.B) {
	cache, limit := buildBenchCache()
	limits := []*config.RateLimit{limit}

	const users = 131072 // 128K
	req := makeReq("eth_call", "u0")

	b.ReportAllocs()
	b.ResetTimer()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		// mutate the user in-place to avoid allocations
		u := i % users
		req.Descriptors[0].Entries[1].Value = fmt.Sprintf("u%d", u)
		_ = cache.DoLimit(ctx, req, limits)
	}
}

func BenchmarkMemoryRateLimitCache_DoLimit_PerUser_Cardinality_Parallel(b *testing.B) {
	cache, limit := buildBenchCache()
	limits := []*config.RateLimit{limit}

	const users = 131072 // 128K
	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(p *testing.PB) {
		// Each worker gets its own request to avoid races
		req := makeReq("eth_call", "u0")
		ctx := context.Background()
		local := rand.New(rand.NewSource(time.Now().UnixNano()))
		for p.Next() {
			u := local.Intn(users)
			req.Descriptors[0].Entries[1].Value = fmt.Sprintf("u%d", u)
			_ = cache.DoLimit(ctx, req, limits)
		}
	})
}

// High-cardinality: per-user + per-ip combinations.
func BenchmarkMemoryRateLimitCache_DoLimit_PerUserIP_Cardinality(b *testing.B) {
	cache, limit := buildBenchCache()
	limits := []*config.RateLimit{limit}

	const users = 65536 // 64K
	const ips = 4096    // 4K
	req := makeReqUserIP("eth_getLogs", "u0", "10.0.0.1")

	b.ReportAllocs()
	b.ResetTimer()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		u := i % users
		ip := i % ips
		req.Descriptors[0].Entries[1].Value = fmt.Sprintf("u%d", u)
		req.Descriptors[0].Entries[2].Value = fmt.Sprintf("10.0.%d.%d", ip/256, ip%256)
		_ = cache.DoLimit(ctx, req, limits)
	}
}

func BenchmarkMemoryRateLimitCache_DoLimit_PerUserIP_Cardinality_Parallel(b *testing.B) {
	cache, limit := buildBenchCache()
	limits := []*config.RateLimit{limit}

	const users = 65536 // 64K
	const ips = 4096    // 4K

	b.ReportAllocs()
	b.SetParallelism(runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(p *testing.PB) {
		req := makeReqUserIP("eth_getLogs", "u0", "10.0.0.1")
		ctx := context.Background()
		local := rand.New(rand.NewSource(time.Now().UnixNano()))
		for p.Next() {
			u := local.Intn(users)
			ip := local.Intn(ips)
			req.Descriptors[0].Entries[1].Value = fmt.Sprintf("u%d", u)
			req.Descriptors[0].Entries[2].Value = fmt.Sprintf("10.0.%d.%d", ip/256, ip%256)
			_ = cache.DoLimit(ctx, req, limits)
		}
	})
}
