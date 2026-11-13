package upstream

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func newRedisBudget(t *testing.T, addr string, store func(*common.RateLimitStoreConfig), rules ...*common.RateLimitRuleConfig) *RateLimiterBudget {
	t.Helper()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{
			Driver: "redis",
			Redis: &common.RedisConnectorConfig{
				Addr:         addr,
				ConnPoolSize: 4,
				GetTimeout:   common.Duration(50 * time.Millisecond),
			},
			CacheKeyPrefix: t.Name() + "_",
		},
		Budgets: []*common.RateLimitBudgetConfig{{Id: "b", Rules: rules}},
	}
	if store != nil {
		store(cfg.Store)
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, cfg.Validate())
	logger := zerolog.Nop()
	reg, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	require.NotNil(t, reg.GetCache(), "redis limiter must connect")
	budget, err := reg.GetBudget("b")
	require.NoError(t, err)
	return budget
}

func admitted(t *testing.T, b *RateLimiterBudget, n int, concurrent bool) int64 {
	t.Helper()
	var ok atomic.Int64
	var wg sync.WaitGroup
	for range n {
		run := func() {
			allowed, err := b.TryAcquirePermit(context.Background(), "p", nil, "eth_call", "", "", "", "upstream")
			require.NoError(t, err)
			if allowed {
				ok.Add(1)
			}
		}
		if concurrent {
			wg.Add(1)
			go func() { defer wg.Done(); run() }()
		} else {
			run()
		}
	}
	wg.Wait()
	return ok.Load()
}

func perHour(max uint32) *common.RateLimitRuleConfig {
	return &common.RateLimitRuleConfig{Method: "*", MaxCount: max, Period: common.RateLimitPeriodHour}
}

// A redis budget walked through a healthy, slow, saturated and dead store,
// under each onFailure mode. 5 requests land while healthy. The remote limit
// (1000/hour) stays out of reach, so the over-limit cache never decides here;
// local mode counts at factor 0.01, i.e. 10/hour.
func TestRateLimiterRedis_StoreFailureFollowsOnFailure(t *testing.T) {
	cases := []struct {
		mode common.RateLimitStoreFailureMode
		// admitted on top of the 5 healthy ones, per failure phase
		slow, saturated, down func(got int64) bool
	}{
		{
			mode:      common.RateLimitStoreFailureAllow,
			slow:      func(got int64) bool { return got == 20 },
			saturated: func(got int64) bool { return got > 1000 },
			down:      func(got int64) bool { return got == 20 },
		},
		{
			mode:      common.RateLimitStoreFailureDeny,
			slow:      func(got int64) bool { return got == 0 },
			saturated: func(got int64) bool { return got == 0 },
			down:      func(got int64) bool { return got == 0 },
		},
		{
			// The local counter saw the 5 healthy requests, so 5 remain in
			// the window across every failure phase together.
			mode:      common.RateLimitStoreFailureLocal,
			slow:      func(got int64) bool { return got == 5 },
			saturated: func(got int64) bool { return got == 0 },
			down:      func(got int64) bool { return got == 0 },
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			m := miniredis.RunT(t)
			proxy := util.NewSlowTCPProxy(t, m.Addr())
			budget := newRedisBudget(t, proxy.Addr(), func(s *common.RateLimitStoreConfig) {
				s.OnFailure = tc.mode
				if tc.mode == common.RateLimitStoreFailureLocal {
					s.LocalLimitFactor = 0.01
				}
			}, perHour(1000))

			require.Equal(t, int64(5), admitted(t, budget, 5, false), "healthy store admits within the limit")

			proxy.SetDelay(200 * time.Millisecond) // > getTimeout 50ms
			got := admitted(t, budget, 20, false)
			require.True(t, tc.slow(got), "slow store: admitted %d", got)

			// Wait for the timed-out calls to drain, then saturate admission.
			require.Eventually(t, func() bool { return len(budget.admission) == 0 }, 10*time.Second, 10*time.Millisecond)
			got = admitted(t, budget, 2000, true)
			require.True(t, tc.saturated(got), "saturated store: admitted %d", got)

			require.Eventually(t, func() bool { return len(budget.admission) == 0 }, 30*time.Second, 10*time.Millisecond)
			proxy.SetDelay(0)
			m.Close()
			got = admitted(t, budget, 20, false)
			require.True(t, tc.down(got), "dead store: admitted %d", got)
		})
	}
}

// A request matching several rules holds one admission slot, not one per rule.
func TestRateLimiterRedis_ManyRulesShareOneAdmissionSlot(t *testing.T) {
	m := miniredis.RunT(t)
	proxy := util.NewSlowTCPProxy(t, m.Addr())
	budget := newRedisBudget(t, proxy.Addr(), func(s *common.RateLimitStoreConfig) {
		s.Redis.GetTimeout = common.Duration(5 * time.Second)
	},
		perHour(100),
		&common.RateLimitRuleConfig{Method: "eth_*", MaxCount: 50, Period: common.RateLimitPeriodHour},
		&common.RateLimitRuleConfig{Method: "eth_call", MaxCount: 2, Period: common.RateLimitPeriodHour},
	)

	proxy.SetDelay(300 * time.Millisecond)
	done := make(chan int64)
	go func() { done <- admitted(t, budget, 1, false) }()
	require.Eventually(t, func() bool { return len(budget.admission) > 0 }, time.Second, time.Millisecond)
	require.Equal(t, 1, len(budget.admission), "one request across 3 rules must hold 1 slot")
	require.Equal(t, int64(1), <-done)

	// The tightest rule still decides: eth_call allows 2 per hour.
	proxy.SetDelay(0)
	require.Equal(t, int64(1), admitted(t, budget, 2, false), "eth_call 2/hour rule enforced in the batch")
}

// A key Redis reported over its limit is denied from the local cache until
// its window ends: no Redis call, so it holds even with Redis dead under the
// default onFailure allow.
func TestRateLimiterRedis_OverLimitKeyDeniesWithoutRedis(t *testing.T) {
	m := miniredis.RunT(t)
	budget := newRedisBudget(t, m.Addr(), nil, perHour(2))

	require.Equal(t, int64(2), admitted(t, budget, 3, false), "third request is over the limit")
	m.Close()
	require.Equal(t, int64(0), admitted(t, budget, 5, false), "cached over-limit key denies without Redis")
}

// Raising maxCount in the same window must clear the over-limit freecache.
// Cache keys exclude the limit value, so without a clear the prior denial
// would keep rejecting until the window ended.
func TestRateLimiterRedis_AdjustBudgetIncreaseClearsOverLimitCache(t *testing.T) {
	m := miniredis.RunT(t)
	budget := newRedisBudget(t, m.Addr(), nil, perHour(2))
	require.Equal(t, int64(2), admitted(t, budget, 3, false), "third request is over the limit")

	rules, err := budget.GetRulesByMethod("eth_call")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.NoError(t, budget.AdjustBudget(rules[0], 10))

	require.Equal(t, int64(5), admitted(t, budget, 5, false), "raised limit admits within the new maxCount")
}

// onFailure: local never scales a credit rule below one call's cost: 600
// credits × 0.1 is 60, under a 200-credit call, so the local limit is 200.
func TestRateLimiterRedis_LocalCreditLimitFitsOneCall(t *testing.T) {
	m := miniredis.RunT(t)
	budget := newRedisBudget(t, m.Addr(), func(s *common.RateLimitStoreConfig) {
		s.OnFailure = common.RateLimitStoreFailureLocal
		s.LocalLimitFactor = 0.1
	}, &common.RateLimitRuleConfig{Method: "*", MaxCount: 600, Period: common.RateLimitPeriodHour, CountMode: common.RateLimitCountModeCredit})
	m.Close()

	call := PermitCost{Hits: 200, Mode: common.RateLimitCountModeCredit}
	got := 0
	for range 3 {
		ok, err := budget.TryAcquirePermit(context.Background(), "p", nil, "eth_call", "", "", "", "upstream", call)
		require.NoError(t, err)
		if ok {
			got++
		}
	}
	require.Equal(t, 1, got, "local limit fits exactly one 200-credit call")
}
