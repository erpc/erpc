package upstream

import (
	"testing"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRateLimitersRegistry(t *testing.T) {
	logger := zerolog.Nop()

	t.Run("nil config", func(t *testing.T) {
		registry, err := NewRateLimitersRegistry(nil, &logger)
		require.NoError(t, err)
		assert.NotNil(t, registry)
	})

	t.Run("valid config", func(t *testing.T) {
		cfg := &common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{
				{
					Id: "test-budget",
					Rules: []*common.RateLimitRuleConfig{
						{
							Method:   "test-method",
							MaxCount: 10,
							Period:   "1s",
						},
					},
				},
			},
		}
		registry, err := NewRateLimitersRegistry(cfg, &logger)
		require.NoError(t, err)
		assert.NotNil(t, registry)
	})

	t.Run("invalid duration", func(t *testing.T) {
		cfg := &common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{
				{
					Id: "test-budget",
					Rules: []*common.RateLimitRuleConfig{
						{
							Method:   "test-method",
							MaxCount: 10,
							Period:   "invalid",
						},
					},
				},
			},
		}
		_, err := NewRateLimitersRegistry(cfg, &logger)
		require.Error(t, err)
		assert.IsType(t, &common.ErrRateLimitInvalidConfig{}, err)
	})

	t.Run("invalid wait time", func(t *testing.T) {
		cfg := &common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{
				{
					Id: "test-budget",
					Rules: []*common.RateLimitRuleConfig{
						{
							Method:   "test-method",
							MaxCount: 10,
							Period:   "1s",
							WaitTime: "invalid",
						},
					},
				},
			},
		}
		_, err := NewRateLimitersRegistry(cfg, &logger)
		require.Error(t, err)
		assert.IsType(t, &common.ErrRateLimitInvalidConfig{}, err)
	})
}

func TestRateLimitersRegistry_GetBudget(t *testing.T) {
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: "test-budget",
				Rules: []*common.RateLimitRuleConfig{
					{
						Method:   "test-method",
						MaxCount: 10,
						Period:   "1s",
					},
				},
			},
		},
	}
	registry, err := NewRateLimitersRegistry(cfg, &logger)
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
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: "test-budget",
				Rules: []*common.RateLimitRuleConfig{
					{
						Method:   "exact-method",
						MaxCount: 10,
						Period:   "1s",
					},
					{
						Method:   "wild*",
						MaxCount: 20,
						Period:   "1s",
					},
				},
			},
		},
	}
	registry, err := NewRateLimitersRegistry(cfg, &logger)
	require.NoError(t, err)

	budget, err := registry.GetBudget("test-budget")
	require.NoError(t, err)
	require.NotNil(t, budget)

	t.Run("exact match", func(t *testing.T) {
		rules := budget.GetRulesByMethod("exact-method")
		assert.Len(t, rules, 1)
		assert.Equal(t, "exact-method", rules[0].Config.Method)
	})

	t.Run("wildcard match", func(t *testing.T) {
		rules := budget.GetRulesByMethod("wildcard")
		assert.Len(t, rules, 1)
		assert.Equal(t, "wild*", rules[0].Config.Method)
	})

	t.Run("no match", func(t *testing.T) {
		rules := budget.GetRulesByMethod("non-existing")
		assert.Len(t, rules, 0)
	})
}

func TestRateLimiterConcurrency(t *testing.T) {
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{
			{
				Id: "test-budget",
				Rules: []*common.RateLimitRuleConfig{
					{
						Method:   "test-method",
						MaxCount: 100,
						Period:   "1s",
					},
				},
			},
		},
	}
	registry, err := NewRateLimitersRegistry(cfg, &logger)
	require.NoError(t, err)

	budget, err := registry.GetBudget("test-budget")
	require.NoError(t, err)
	require.NotNil(t, budget)

	const numGoroutines = 100
	const numRequests = 1000

	start := time.Now()
	done := make(chan bool)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			for j := 0; j < numRequests; j++ {
				rules := budget.GetRulesByMethod("test-method")
				require.Len(t, rules, 1)
				ok := rules[0].Limiter.TryAcquirePermit()
				require.True(t, ok)
			}
			done <- true
		}()
	}

	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	elapsed := time.Since(start)
	t.Logf("Time taken for %d requests across %d goroutines: %v", numGoroutines*numRequests, numGoroutines, elapsed)

	// Ensure that the rate limiting worked
	assert.True(t, elapsed >= 10*time.Second, "Rate limiting should have slowed down the requests")
}