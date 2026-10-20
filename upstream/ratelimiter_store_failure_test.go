package upstream

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/erpc/erpc/common"
)

// slowProxy forwards TCP to target and holds every client->server chunk for
// delay, so a real radix client sees a slow Redis.
type slowProxy struct {
	ln    net.Listener
	delay atomic.Int64
}

func newSlowProxy(t *testing.T, target string) *slowProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &slowProxy{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c, target)
		}
	}()
	return p
}

func (p *slowProxy) serve(c net.Conn, target string) {
	defer c.Close()
	s, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer s.Close()
	go func() { _, _ = io.Copy(c, s) }()
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if d := time.Duration(p.delay.Load()); d > 0 {
				time.Sleep(d)
			}
			if _, werr := s.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

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
// under each onFailure mode. Limit 10/hour; 5 requests land while healthy.
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
			proxy := newSlowProxy(t, m.Addr())
			budget := newRedisBudget(t, proxy.ln.Addr().String(), func(s *common.RateLimitStoreConfig) {
				s.OnFailure = tc.mode
				if tc.mode == common.RateLimitStoreFailureLocal {
					s.LocalLimitFactor = 1
				}
			}, perHour(10))

			require.Equal(t, int64(5), admitted(t, budget, 5, false), "healthy store admits within the limit")

			proxy.delay.Store(int64(200 * time.Millisecond)) // > getTimeout 50ms
			got := admitted(t, budget, 20, false)
			require.True(t, tc.slow(got), "slow store: admitted %d", got)

			// Wait for the timed-out calls to drain, then saturate admission.
			require.Eventually(t, func() bool { return len(budget.admission) == 0 }, 10*time.Second, 10*time.Millisecond)
			got = admitted(t, budget, 2000, true)
			require.True(t, tc.saturated(got), "saturated store: admitted %d", got)

			require.Eventually(t, func() bool { return len(budget.admission) == 0 }, 30*time.Second, 10*time.Millisecond)
			proxy.delay.Store(0)
			m.Close()
			got = admitted(t, budget, 20, false)
			require.True(t, tc.down(got), "dead store: admitted %d", got)
		})
	}
}

// A request matching several rules holds one admission slot, not one per rule.
func TestRateLimiterRedis_ManyRulesShareOneAdmissionSlot(t *testing.T) {
	m := miniredis.RunT(t)
	proxy := newSlowProxy(t, m.Addr())
	budget := newRedisBudget(t, proxy.ln.Addr().String(), func(s *common.RateLimitStoreConfig) {
		s.Redis.GetTimeout = common.Duration(5 * time.Second)
	},
		perHour(100),
		&common.RateLimitRuleConfig{Method: "eth_*", MaxCount: 50, Period: common.RateLimitPeriodHour},
		&common.RateLimitRuleConfig{Method: "eth_call", MaxCount: 2, Period: common.RateLimitPeriodHour},
	)

	proxy.delay.Store(int64(300 * time.Millisecond))
	done := make(chan int64)
	go func() { done <- admitted(t, budget, 1, false) }()
	require.Eventually(t, func() bool { return len(budget.admission) > 0 }, time.Second, time.Millisecond)
	require.Equal(t, 1, len(budget.admission), "one request across 3 rules must hold 1 slot")
	require.Equal(t, int64(1), <-done)

	// The tightest rule still decides: eth_call allows 2 per hour.
	proxy.delay.Store(0)
	require.Equal(t, int64(1), admitted(t, budget, 2, false), "eth_call 2/hour rule enforced in the batch")
}
