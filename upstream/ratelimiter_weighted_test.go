package upstream

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRateLimiterBudget_costFor(t *testing.T) {
	b := &RateLimiterBudget{methodCosts: map[string]uint32{
		"eth_getLogs":     50,
		"eth_blockNumber": 0,
		"*":               10,
	}}
	assert.Equal(t, uint32(50), b.costFor("eth_getLogs"))    // exact match wins
	assert.Equal(t, uint32(0), b.costFor("eth_blockNumber")) // explicit exemption
	assert.Equal(t, uint32(10), b.costFor("eth_call"))       // "*" default for unlisted

	// No table at all → fallback of 1.
	assert.Equal(t, uint32(1), (&RateLimiterBudget{}).costFor("anything"))

	// Table without "*" and without an exact hit → fallback of 1.
	assert.Equal(t, uint32(1), (&RateLimiterBudget{
		methodCosts: map[string]uint32{"x": 5},
	}).costFor("y"))
}

func newWeightedBudget(t *testing.T, maxCount uint32, costs map[string]uint32) *RateLimiterBudget {
	t.Helper()
	logger := zerolog.Nop()
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{Driver: "memory"},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id:          "wb",
			MethodCosts: costs,
			Rules: []*common.RateLimitRuleConfig{{
				Method:   "*",
				MaxCount: maxCount,
				Period:   common.RateLimitPeriodMinute,
				Weighted: true,
			}},
		}},
	}
	reg, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	b, err := reg.GetBudget("wb")
	require.NoError(t, err)
	require.NotNil(t, b)
	return b
}

// A cost-0 method is exempt: the weighted rule is skipped for it, so it is never
// blocked regardless of how far past MaxCount it is called. This path never
// touches the time window, so it is fully deterministic.
func TestRateLimiterBudget_WeightedExemption(t *testing.T) {
	b := newWeightedBudget(t, 5, map[string]uint32{"*": 10, "exempt": 0})
	for i := 0; i < 25; i++ {
		ok, err := b.TryAcquirePermit(context.Background(), "proj", nil, "exempt", "", "", "", "test")
		require.NoError(t, err)
		require.True(t, ok, "exempt (cost 0) method must never be blocked by the weighted budget (iter %d)", i)
	}
}

// A weighted rule accumulates each request's cost against MaxCount. With cost 6
// and a ceiling of 10, the first request (6) fits and the second (12) exceeds.
func TestRateLimiterBudget_WeightedEnforcement(t *testing.T) {
	b := newWeightedBudget(t, 10, map[string]uint32{"*": 6})

	ok, err := b.TryAcquirePermit(context.Background(), "proj", nil, "costly", "", "", "", "test")
	require.NoError(t, err)
	require.True(t, ok, "first weighted request (cost 6 of 10) should be allowed")

	ok, err = b.TryAcquirePermit(context.Background(), "proj", nil, "costly", "", "", "", "test")
	require.NoError(t, err)
	require.False(t, ok, "second weighted request (would reach 12 of 10) should be denied")
}

// A weighted rule with maxCount 0 is a misconfiguration (it would reject every
// non-exempt request) and must be rejected at validation time.
func TestRateLimitRuleConfig_WeightedRequiresMaxCount(t *testing.T) {
	err := (&common.RateLimitRuleConfig{
		Method:   "*",
		MaxCount: 0,
		Period:   common.RateLimitPeriodSecond,
		Weighted: true,
	}).Validate()
	require.Error(t, err)

	// A non-weighted rule with maxCount 0 stays valid (unchanged behavior).
	require.NoError(t, (&common.RateLimitRuleConfig{
		Method:   "*",
		MaxCount: 0,
		Period:   common.RateLimitPeriodSecond,
	}).Validate())
}
