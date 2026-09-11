package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func creditBudget(t *testing.T, cfg *common.RateLimiterConfig, id string) *RateLimiterBudget {
	t.Helper()
	logger := zerolog.Nop()
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	budget, err := registry.GetBudget(id)
	require.NoError(t, err)
	return budget
}

// A credit rule outside the upstream scope prices each method from the budget's
// creditUnits table and spends it from one shared pool.
func TestCreditRule_PricesFromBudgetTableAndPools(t *testing.T) {
	budget := creditBudget(t, &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:          "wallet",
			CreditUnits: map[string]int64{"*": 20, "eth_getLogs": 60},
			Rules: []*common.RateLimitRuleConfig{
				{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeCredit},
			},
		}},
	}, "wallet")

	ctx := context.Background()

	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_getLogs", "", "", "", "project")
	require.NoError(t, err)
	require.True(t, ok, "eth_getLogs is priced 60 by its exact entry, which fits in 100")

	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "project")
	require.NoError(t, err)
	require.True(t, ok, "eth_call falls to \"*\" at 20, so 60+20 still fits")

	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_getBalance", "", "", "", "project")
	require.NoError(t, err)
	require.True(t, ok, "60+20+20 reaches exactly 100")

	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_blockNumber", "", "", "", "project")
	require.NoError(t, err)
	require.False(t, ok, "the pool is spent regardless of which method asks")
}

// A method priced 0 is exempt from the credit rule but must still be subject to
// any flat rule covering it. This is the mixed-budget pattern: keep cheap
// methods out of the wallet without leaving them uncapped.
func TestCreditRule_ZeroCostExemptButFlatRuleStillApplies(t *testing.T) {
	const free = "eth_chainId"
	budget := creditBudget(t, &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:          "mixed",
			CreditUnits: map[string]int64{"*": 20, free: 0},
			Rules: []*common.RateLimitRuleConfig{
				{Method: "*", MaxCount: 40, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeCredit},
				{Method: free, MaxCount: 3, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeRequest},
			},
		}},
	}, "mixed")

	ctx := context.Background()

	// Drain the wallet with two 20-credit calls.
	for i := 1; i <= 2; i++ {
		ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "project")
		require.NoError(t, err)
		require.True(t, ok, "credit call %d fits in the 40-credit wallet", i)
	}
	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "project")
	require.NoError(t, err)
	require.False(t, ok, "the wallet is spent")

	// The exempt method is unaffected by the drained wallet, but its own flat
	// rule still counts one hit per call and caps it at 3.
	for i := 1; i <= 3; i++ {
		ok, err := budget.TryAcquirePermit(ctx, "", nil, free, "", "", "", "project")
		require.NoError(t, err)
		require.True(t, ok, "exempt call %d is within the flat ceiling of 3", i)
	}
	ok, err = budget.TryAcquirePermit(ctx, "", nil, free, "", "", "", "project")
	require.NoError(t, err)
	require.False(t, ok, "the exempt method is still bounded by its flat rule")
}

// A rule stating countMode: request must stay flat and per-method even when the
// caller is an upstream counting credits. Without per-rule mode resolution the
// caller's mode would leak into every rule and silently pool them.
func TestCreditRule_ExplicitRequestModeSurvivesCreditCaller(t *testing.T) {
	budget := creditBudget(t, &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id: "mixed",
			Rules: []*common.RateLimitRuleConfig{
				{Method: "*", MaxCount: 2, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeRequest},
			},
		}},
	}, "mixed")

	ctx := context.Background()
	credit := PermitCost{Hits: 50, Mode: common.RateLimitCountModeCredit}

	// The rule counts requests, so the 50-credit weight is ignored and each call
	// is worth exactly 1.
	for i := 1; i <= 2; i++ {
		ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "upstream", credit)
		require.NoError(t, err)
		require.True(t, ok, "request %d is within the flat ceiling of 2", i)
	}
	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "upstream", credit)
	require.NoError(t, err)
	require.False(t, ok, "eth_call is exhausted")

	// And it is still partitioned per method.
	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_getLogs", "", "", "", "upstream", credit)
	require.NoError(t, err)
	require.True(t, ok, "an explicit request-mode rule keeps a counter per method")
}

// countMode is part of a rule's identity. Two rules differing only in it cannot
// collide today anyway, since a credit rule drops the {method} entry a request
// rule keeps, but that holds only while method-omission is the sole descriptor
// difference between the modes. Keying on countMode does not rely on it.
func TestRuleKeyFor_CountModeIsPartOfIdentity(t *testing.T) {
	base := func(mode common.RateLimitCountMode) *common.RateLimitRuleConfig {
		return &common.RateLimitRuleConfig{
			Method: "*", MaxCount: 4, Period: common.RateLimitPeriodMinute, CountMode: mode,
		}
	}

	require.NotEqual(t,
		ruleKeyFor(base(common.RateLimitCountModeCredit)),
		ruleKeyFor(base(common.RateLimitCountModeRequest)),
		"rules differing only in countMode must not share a key")

	// An unset mode means request, so it must not split the key from an
	// explicit request rule that is otherwise identical.
	require.Equal(t,
		ruleKeyFor(base("")),
		ruleKeyFor(base(common.RateLimitCountModeRequest)),
		"an unset countMode is request and must key identically")
}

// A narrow credit rule and a catch-all credit rule form nested pools: the
// wildcard pattern is part of the rule identity, so each keeps its own counter.
func TestCreditRule_WildcardPoolsAreNested(t *testing.T) {
	budget := creditBudget(t, &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:          "nested",
			CreditUnits: map[string]int64{"*": 10},
			Rules: []*common.RateLimitRuleConfig{
				{Method: "eth_*", MaxCount: 20, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeCredit},
				{Method: "*", MaxCount: 1000, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeCredit},
			},
		}},
	}, "nested")

	ctx := context.Background()

	// The inner eth_ pool holds 2 calls at 10 credits each.
	for i := 1; i <= 2; i++ {
		ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "project")
		require.NoError(t, err)
		require.True(t, ok, "eth call %d fits the inner 20-credit pool", i)
	}
	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_getLogs", "", "", "", "project")
	require.NoError(t, err)
	require.False(t, ok, "the inner eth_ pool is spent")

	// A non-eth method only meets the outer pool, which has plenty left.
	ok, err = budget.TryAcquirePermit(ctx, "", nil, "net_version", "", "", "", "project")
	require.NoError(t, err)
	require.True(t, ok, "the outer pool is independent of the inner one")
}

// An upstream counting credits against a budget whose rules state no countMode
// must behave exactly as before this change: vendor weight, pooled methods.
func TestCreditRule_InheritsCallerModeWhenUnset(t *testing.T) {
	budget := creditBudget(t, &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:    "vendor",
			Rules: []*common.RateLimitRuleConfig{{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodMinute}},
		}},
	}, "vendor")

	ctx := context.Background()
	credit := func(h uint32) PermitCost {
		return PermitCost{Hits: h, Mode: common.RateLimitCountModeCredit}
	}

	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_getLogs", "", "", "", "upstream", credit(60))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "upstream", credit(60))
	require.NoError(t, err)
	require.False(t, ok, "the vendor weight is charged to one shared pool")
}

// The budget table only applies where the caller has no vendor estimate. An
// upstream in credit mode keeps pricing from its vendor.
func TestCreditRule_VendorWeightWinsOverBudgetTable(t *testing.T) {
	budget := creditBudget(t, &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:          "b",
			CreditUnits: map[string]int64{"*": 1},
			Rules: []*common.RateLimitRuleConfig{
				{Method: "*", MaxCount: 100, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeCredit},
			},
		}},
	}, "b")

	ctx := context.Background()
	// If the budget's "*": 1 were used, this would take 100 calls to exhaust.
	credit := PermitCost{Hits: 60, Mode: common.RateLimitCountModeCredit}
	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "upstream", credit)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "upstream", credit)
	require.NoError(t, err)
	require.False(t, ok, "the caller's vendor weight is what gets charged")
}

// With no creditUnits table at all, a credit rule is a total request cap shared
// across methods.
func TestCreditRule_NoTableIsCrossMethodRequestCap(t *testing.T) {
	budget := creditBudget(t, &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id: "cap",
			Rules: []*common.RateLimitRuleConfig{
				{Method: "*", MaxCount: 3, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeCredit},
			},
		}},
	}, "cap")

	ctx := context.Background()
	for i, method := range []string{"eth_call", "eth_getLogs", "net_version"} {
		ok, err := budget.TryAcquirePermit(ctx, "", nil, method, "", "", "", "project")
		require.NoError(t, err)
		require.True(t, ok, "call %d (%s) is within the shared cap of 3", i+1, method)
	}
	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_chainId", "", "", "", "project")
	require.NoError(t, err)
	require.False(t, ok, "the cap counts calls across all methods")
}

// "*": 0 exempts every unlisted method from the credit rule.
func TestCreditRule_StarZeroExemptsUnlistedMethods(t *testing.T) {
	budget := creditBudget(t, &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:          "b",
			CreditUnits: map[string]int64{"*": 0, "eth_getLogs": 60},
			Rules: []*common.RateLimitRuleConfig{
				{Method: "*", MaxCount: 60, Period: common.RateLimitPeriodMinute, CountMode: common.RateLimitCountModeCredit},
			},
		}},
	}, "b")

	ctx := context.Background()

	ok, err := budget.TryAcquirePermit(ctx, "", nil, "eth_getLogs", "", "", "", "project")
	require.NoError(t, err)
	require.True(t, ok, "the priced method spends the whole pool")
	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_getLogs", "", "", "", "project")
	require.NoError(t, err)
	require.False(t, ok, "the pool is spent")

	ok, err = budget.TryAcquirePermit(ctx, "", nil, "eth_call", "", "", "", "project")
	require.NoError(t, err)
	require.True(t, ok, "an unlisted method is exempt via \"*\": 0")
}
