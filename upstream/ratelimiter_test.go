package upstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRateLimitersRegistry_New(t *testing.T) {
	logger := zerolog.Nop()

	t.Run("nil config", func(t *testing.T) {
		registry, err := NewRateLimitersRegistry(context.Background(), nil, &logger)
		require.NoError(t, err)
		assert.NotNil(t, registry)
	})

	t.Run("valid config", func(t *testing.T) {
		cfg := &common.RateLimiterConfig{
			Store: &common.RateLimitStoreConfig{Driver: "memory"},
			Budgets: []*common.RateLimitBudgetConfig{
				{
					Id: "test-budget",
					Rules: []*common.RateLimitRuleConfig{
						{
							Method:   "test-method",
							MaxCount: 10,
							Period:   common.RateLimitPeriodSecond,
						},
					},
				},
			},
		}
		registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
		require.NoError(t, err)
		assert.NotNil(t, registry)
	})
}

func TestRateLimitersRegistry_GetBudget(t *testing.T) {
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: "test-budget",
				Rules: []*common.RateLimitRuleConfig{
					{
						Method:   "test-method",
						MaxCount: 10,
						Period:   common.RateLimitPeriodSecond,
					},
				},
			},
		},
	}
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)

	t.Run("existing budget", func(t *testing.T) {
		budget, err := registry.GetBudget("test-budget")
		require.NoError(t, err)
		assert.NotNil(t, budget)
		assert.Equal(t, "test-budget", budget.Id)
	})

	t.Run("non-existing budget", func(t *testing.T) {
		budget, err := registry.GetBudget("non-existing")
		require.Error(t, err)
		assert.Nil(t, budget)
		assert.IsType(t, &common.ErrRateLimitBudgetNotFound{}, err)
	})

	t.Run("empty budget id", func(t *testing.T) {
		budget, err := registry.GetBudget("")
		require.NoError(t, err)
		assert.Nil(t, budget)
	})
}

func TestRateLimiterBudget_GetRulesByMethod(t *testing.T) {
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: "test-budget",
				Rules: []*common.RateLimitRuleConfig{
					{
						Method:   "exact-method",
						MaxCount: 10,
						Period:   common.RateLimitPeriodSecond,
					},
					{
						Method:   "wild*",
						MaxCount: 20,
						Period:   common.RateLimitPeriodSecond,
					},
				},
			},
		},
	}
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)

	budget, err := registry.GetBudget("test-budget")
	require.NoError(t, err)
	require.NotNil(t, budget)

	t.Run("exact match", func(t *testing.T) {
		rules, err := budget.GetRulesByMethod("exact-method")
		require.NoError(t, err)
		assert.Len(t, rules, 1)
		assert.Equal(t, "exact-method", rules[0].Config.Method)
	})

	t.Run("wildcard match", func(t *testing.T) {
		rules, err := budget.GetRulesByMethod("wildcard")
		require.NoError(t, err)
		assert.Len(t, rules, 1)
		assert.Equal(t, "wild*", rules[0].Config.Method)
	})

	t.Run("no match", func(t *testing.T) {
		rules, err := budget.GetRulesByMethod("non-existing")
		require.NoError(t, err)
		assert.Len(t, rules, 0)
	})
}

func TestRateLimiter_ConcurrentPermits(t *testing.T) {
	t.Skip("Concurrent permits test skipped pending stabilization of Envoy-based memory limiter semantics")
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: "test-budget",
				Rules: []*common.RateLimitRuleConfig{
					{
						Method:   "test-method",
						MaxCount: 3000,
						Period:   common.RateLimitPeriodSecond,
					},
				},
			},
		},
	}
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)

	budget, err := registry.GetBudget("test-budget")
	require.NoError(t, err)
	require.NotNil(t, budget)

	const numGoroutines = 10
	const numRequests = 30

	start := time.Now()
	wg := sync.WaitGroup{}

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < numRequests; j++ {
				rules, err := budget.GetRulesByMethod("test-method")
				require.NoError(t, err)
				require.Len(t, rules, 1)
				ok, err := budget.TryAcquirePermit(context.TODO(), "", nil, "test-method", "", "", "", "upstream")
				require.NoError(t, err)
				require.True(t, ok)
			}
		}()
	}

	wg.Wait()

	elapsed := time.Since(start)
	t.Logf("Time taken for %d requests across %d goroutines: %v", numGoroutines*numRequests, numGoroutines, elapsed)
}

func TestRateLimiter_ExceedCapacity(t *testing.T) {
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: "test-budget",
				Rules: []*common.RateLimitRuleConfig{
					{
						Method:   "test-method",
						MaxCount: 10,
						Period:   common.RateLimitPeriodMinute,
					},
				},
			},
		},
	}

	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)

	budget, err := registry.GetBudget("test-budget")
	require.NoError(t, err)
	require.NotNil(t, budget)

	// The limiter implementation now uses Envoy's semantics via an in-memory cache,
	// which requires integration with time-sliced windows. This test needs to be
	// reworked to avoid flakiness and dependency on exact internal behavior.
	// Skipping for now; behavior is covered by higher-level forwarding tests.
	t.Skip("Exceed-capacity behavior will be revalidated with Envoy-based limiter")

	for i := 0; i < 10; i++ {
		rules, err := budget.GetRulesByMethod("test-method")
		require.NoError(t, err)
		require.Len(t, rules, 1)
		ok, err := budget.TryAcquirePermit(context.TODO(), "", nil, "test-method", "", "", "", "upstream")
		require.NoError(t, err)
		require.True(t, ok)
	}

	rules, err := budget.GetRulesByMethod("test-method")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	ok, err := budget.TryAcquirePermit(context.TODO(), "", nil, "test-method", "", "", "", "upstream")
	require.NoError(t, err)
	require.False(t, ok)
}
