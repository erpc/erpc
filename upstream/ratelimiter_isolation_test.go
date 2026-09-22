package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Every rule in a budget must enforce its own limit against its own counter.
//
// The envoy cache key is derived from the domain, the descriptor entries and
// the period bucket -- the limit value is not part of it. Two rules that
// resolve to the same {method, scope} at the same period therefore produce an
// identical key, and because each rule is evaluated with its own DoLimit call,
// a single request increments that one shared counter once per overlapping
// rule. The tightest limit then trips after MaxCount/N requests instead of
// MaxCount.
//
// Here a catch-all rule and an exact-method rule both match eth_call, so each
// request charges the shared counter twice and the 10-request ceiling is hit
// after 5.
func TestRateLimiterBudget_OverlappingRulesDoNotShareCounter(t *testing.T) {
	logger := zerolog.Nop()
	const method = "eth_call"

	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id: "b",
			Rules: []*common.RateLimitRuleConfig{
				// Loose catch-all, far above anything this test sends.
				{Method: "*", MaxCount: 1000, Period: common.RateLimitPeriodMinute},
				// Tight per-method rule: this is the limit under test.
				{Method: method, MaxCount: 10, Period: common.RateLimitPeriodMinute},
			},
		}},
	}

	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	budget, err := registry.GetBudget("b")
	require.NoError(t, err)

	ctx := context.Background()
	for i := 1; i <= 10; i++ {
		ok, err := budget.TryAcquirePermit(ctx, "", nil, method, "", "", "", "upstream")
		require.NoError(t, err)
		require.True(t, ok, "request %d is within the per-method ceiling of 10 and must be allowed", i)
	}

	ok, err := budget.TryAcquirePermit(ctx, "", nil, method, "", "", "", "upstream")
	require.NoError(t, err)
	require.False(t, ok, "request 11 must exceed the per-method ceiling of 10")
}

// Two rules that differ only in MaxCount also collide on one counter: they
// share a {method, scope} descriptor at the same period, so each request is
// counted twice and the tighter of the two rules trips at half its ceiling.
// The effective limit must be the tighter rule's MaxCount, not half of it.
func TestRateLimiterBudget_SameMethodDifferentLimitsDoNotShareCounter(t *testing.T) {
	logger := zerolog.Nop()
	const method = "eth_getLogs"

	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id: "b",
			Rules: []*common.RateLimitRuleConfig{
				{Method: method, MaxCount: 6, Period: common.RateLimitPeriodMinute},
				{Method: method, MaxCount: 100, Period: common.RateLimitPeriodMinute},
			},
		}},
	}

	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	budget, err := registry.GetBudget("b")
	require.NoError(t, err)

	ctx := context.Background()
	for i := 1; i <= 6; i++ {
		ok, err := budget.TryAcquirePermit(ctx, "", nil, method, "", "", "", "upstream")
		require.NoError(t, err)
		require.True(t, ok, "request %d is within the tighter ceiling of 6 and must be allowed", i)
	}

	ok, err := budget.TryAcquirePermit(ctx, "", nil, method, "", "", "", "upstream")
	require.NoError(t, err)
	require.False(t, ok, "request 7 must exceed the tighter ceiling of 6")
}
