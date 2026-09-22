package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func creditCost(hits uint32) PermitCost {
	return PermitCost{Hits: hits, Mode: common.RateLimitCountModeCredit}
}

// A credit budget models a provider plan ("300M CU/month"), so every method
// must draw from one shared pool. Spending on one method has to reduce the
// headroom left for every other method.
func TestRateLimiterBudget_CreditPoolIsSharedAcrossMethods(t *testing.T) {
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:    "wallet",
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodMinute}},
		}},
	}
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	budget, err := registry.GetBudget("wallet")
	require.NoError(t, err)

	ctx := context.Background()

	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_getLogs", "", "", "", "upstream", creditCost(60))
	require.NoError(t, err)
	require.True(t, ok, "60 of 100 credits fits")

	// A different method, but the same wallet: 60+60 exceeds 100.
	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "upstream", creditCost(60))
	require.NoError(t, err)
	require.False(t, ok, "eth_call must draw from the pool eth_getLogs already spent from")

	// Still rejected for a third method: the pool is exhausted, not the method.
	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_getBalance", "", "", "", "upstream", creditCost(60))
	require.NoError(t, err)
	require.False(t, ok, "the whole pool is spent regardless of method")
}

// Request counting stays per method: exhausting one method's limit must not
// affect any other method.
func TestRateLimiterBudget_RequestModeStaysPerMethod(t *testing.T) {
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:    "per-method",
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 2, Period: common.RateLimitPeriodMinute}},
		}},
	}
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	budget, err := registry.GetBudget("per-method")
	require.NoError(t, err)

	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "upstream")
		require.NoError(t, err)
		require.True(t, ok, "eth_call request %d is within its own ceiling of 2", i)
	}
	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "upstream")
	require.NoError(t, err)
	require.False(t, ok, "eth_call is exhausted")

	// eth_getLogs has its own counter and is untouched.
	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_getLogs", "", "", "", "upstream")
	require.NoError(t, err)
	require.True(t, ok, "request mode keeps a separate counter per method")
}

// The pool is still per scope: two clients under a perIP credit rule get one
// wallet each, and they must not spend each other's credits.
func TestRateLimiterBudget_CreditPoolIsPerScope(t *testing.T) {
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:    "wallet",
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodMinute, PerIP: true}},
		}},
	}
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	budget, err := registry.GetBudget("wallet")
	require.NoError(t, err)

	ctx := context.Background()
	reqA := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	reqA.SetClientIP("1.1.1.1")
	reqB := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	reqB.SetClientIP("2.2.2.2")

	ok, err := budget.TryAcquirePermit(ctx, "", reqA, "eth_call", "", "", "", "upstream", creditCost(60))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = budget.TryAcquirePermit(ctx, "", reqA, "eth_getLogs", "", "", "", "upstream", creditCost(60))
	require.NoError(t, err)
	require.False(t, ok, "same IP shares one pool across methods")

	ok, err = budget.TryAcquirePermit(ctx, "", reqB, "eth_getLogs", "", "", "", "upstream", creditCost(60))
	require.NoError(t, err)
	require.True(t, ok, "a different IP has its own pool")
}
