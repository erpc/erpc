package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func weightedBudget(costs map[string]uint32) *RateLimitBudgetConfig {
	return &RateLimitBudgetConfig{
		Id:          "b",
		MethodCosts: costs,
		Rules: []*RateLimitRuleConfig{
			{Method: "*", MaxCount: 100, Period: RateLimitPeriodSecond, Weighted: true},
		},
	}
}

// A partial MethodCosts override merges onto the built-in defaults: the
// overridden method takes the user value while every other method keeps its
// default. This is the anti-footgun — a one-method override must not wipe the
// rest of the table.
func TestRateLimitBudget_MethodCostsPartialOverride(t *testing.T) {
	b := weightedBudget(map[string]uint32{"eth_getLogs": 999})
	require.NoError(t, b.SetDefaults())

	assert.Equal(t, uint32(999), b.MethodCosts["eth_getLogs"], "explicit override wins")
	assert.Equal(t, DefaultRateLimitMethodCosts["eth_call"], b.MethodCosts["eth_call"], "unset method keeps default")
	assert.Equal(t, DefaultRateLimitMethodCosts["*"], b.MethodCosts["*"], "default fallback preserved")
	assert.Equal(t, uint32(0), b.MethodCosts["eth_chainId"], "trivial method stays free")
}

// The user may also override the "*" default.
func TestRateLimitBudget_MethodCostsOverrideDefault(t *testing.T) {
	b := weightedBudget(map[string]uint32{"*": 5})
	require.NoError(t, b.SetDefaults())
	assert.Equal(t, uint32(5), b.MethodCosts["*"], "user default overrides built-in *")
	assert.Equal(t, DefaultRateLimitMethodCosts["eth_call"], b.MethodCosts["eth_call"], "per-method defaults still applied")
}

// A weighted rule with no MethodCosts at all inherits the full default table.
func TestRateLimitBudget_MethodCostsAllDefaults(t *testing.T) {
	b := weightedBudget(nil)
	require.NoError(t, b.SetDefaults())
	assert.Equal(t, DefaultRateLimitMethodCosts["*"], b.MethodCosts["*"])
	assert.Equal(t, DefaultRateLimitMethodCosts["eth_getLogs"], b.MethodCosts["eth_getLogs"])
}

// Without any weighted rule, the default table is NOT injected — a non-weighted
// budget's MethodCosts is left untouched (defaults are inert for it).
func TestRateLimitBudget_NoWeightedRuleLeavesCostsUntouched(t *testing.T) {
	b := &RateLimitBudgetConfig{
		Id:          "b",
		MethodCosts: map[string]uint32{"eth_call": 7},
		Rules: []*RateLimitRuleConfig{
			{Method: "*", MaxCount: 100, Period: RateLimitPeriodSecond},
		},
	}
	require.NoError(t, b.SetDefaults())
	assert.Equal(t, map[string]uint32{"eth_call": 7}, b.MethodCosts, "no weighted rule → no default injection")
}
