package data

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func TestCachePolicy_MatchersIntegration(t *testing.T) {
	mockConnector := &MockConnector{}

	t.Run("MatchesForSet with include matchers", func(t *testing.T) {
		policy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				{
					Network:  "1",
					Method:   "eth_call",
					Finality: []common.DataFinalityState{common.DataFinalityStateFinalized},
					Action:   common.MatcherInclude,
				},
			},
			Connector: "test",
		}, mockConnector)
		assert.NoError(t, err)

		// Should match
		match, err := policy.MatchesForSet("1", "eth_call", nil, common.DataFinalityStateFinalized, false)
		assert.NoError(t, err)
		assert.True(t, match)

		// Should not match different network
		match, err = policy.MatchesForSet("2", "eth_call", nil, common.DataFinalityStateFinalized, false)
		assert.NoError(t, err)
		assert.False(t, match)

		// Should not match different method
		match, err = policy.MatchesForSet("1", "eth_getBalance", nil, common.DataFinalityStateFinalized, false)
		assert.NoError(t, err)
		assert.False(t, match)
	})

	t.Run("MatchesForSet with exclude matchers", func(t *testing.T) {
		policy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				{
					Method: "*",
					Action: common.MatcherInclude,
				},
				{
					Method: "eth_getLogs",
					Action: common.MatcherExclude,
				},
			},
			Connector: "test",
		}, mockConnector)
		assert.NoError(t, err)

		// Should match most methods
		match, err := policy.MatchesForSet("1", "eth_call", nil, common.DataFinalityStateFinalized, false)
		assert.NoError(t, err)
		assert.True(t, match)

		// Should not match excluded method
		match, err = policy.MatchesForSet("1", "eth_getLogs", nil, common.DataFinalityStateFinalized, false)
		assert.NoError(t, err)
		assert.False(t, match)
	})

	t.Run("MatchesForSet with empty behavior", func(t *testing.T) {
		policy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				{
					Method: "*",
					Empty:  common.CacheEmptyBehaviorIgnore,
					Action: common.MatcherInclude,
				},
			},
			Connector: "test",
		}, mockConnector)
		assert.NoError(t, err)

		// Should not match empty responses
		match, err := policy.MatchesForSet("1", "eth_call", nil, common.DataFinalityStateFinalized, true)
		assert.NoError(t, err)
		assert.False(t, match)

		// Should match non-empty responses
		match, err = policy.MatchesForSet("1", "eth_call", nil, common.DataFinalityStateFinalized, false)
		assert.NoError(t, err)
		assert.True(t, match)
	})

	t.Run("MatchesForGet with special finality handling", func(t *testing.T) {
		policy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				{
					Method:   "*",
					Finality: []common.DataFinalityState{common.DataFinalityStateFinalized},
					Action:   common.MatcherInclude,
				},
			},
			Connector: "test",
		}, mockConnector)
		assert.NoError(t, err)

		// Should match exact finality
		match, err := policy.MatchesForGet("1", "eth_call", nil, common.DataFinalityStateFinalized)
		assert.NoError(t, err)
		assert.True(t, match)

		// Should match unknown finality (special handling)
		match, err = policy.MatchesForGet("1", "eth_call", nil, common.DataFinalityStateUnknown)
		assert.NoError(t, err)
		assert.True(t, match)

		// Should not match different finality
		match, err = policy.MatchesForGet("1", "eth_call", nil, common.DataFinalityStateRealtime)
		assert.NoError(t, err)
		assert.False(t, match)
	})

	t.Run("backward compatibility with legacy fields", func(t *testing.T) {
		// Create policy with legacy fields
		cfg := &common.CachePolicyConfig{
			Network:   "1",
			Method:    "eth_call",
			Finality:  common.DataFinalityStateFinalized,
			Connector: "test",
		}

		// Set defaults (includes legacy conversion)
		err := cfg.SetDefaults()
		assert.NoError(t, err)

		policy, err := NewCachePolicy(cfg, mockConnector)
		assert.NoError(t, err)

		// Should work exactly as before
		match, err := policy.MatchesForSet("1", "eth_call", nil, common.DataFinalityStateFinalized, false)
		assert.NoError(t, err)
		assert.True(t, match)

		match, err = policy.MatchesForSet("2", "eth_call", nil, common.DataFinalityStateFinalized, false)
		assert.NoError(t, err)
		assert.False(t, match)
	})

	t.Run("string representation with matchers", func(t *testing.T) {
		policy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				{Method: "*", Action: common.MatcherInclude},
			},
			Connector: "test",
		}, mockConnector)
		assert.NoError(t, err)
		assert.Contains(t, policy.String(), "matchers=[{method=* action=include}]")
	})

	t.Run("size limits work with matchers", func(t *testing.T) {
		minSize := "100B"
		maxSize := "1KB"
		policy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				{Method: "*", Action: common.MatcherInclude},
			},
			MinItemSize: &minSize,
			MaxItemSize: &maxSize,
			Connector:   "test",
		}, mockConnector)
		assert.NoError(t, err)

		// Too small
		assert.False(t, policy.MatchesSizeLimits(50))
		// Just right
		assert.True(t, policy.MatchesSizeLimits(500))
		// Too large
		assert.False(t, policy.MatchesSizeLimits(2000))
	})

	t.Run("matched matcher empty state behavior", func(t *testing.T) {
		// Test that empty state is correctly taken from the specific matcher that matched
		policy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				// First matcher: exclude everything with empty=ignore
				{
					Method: "*",
					Empty:  common.CacheEmptyBehaviorIgnore,
					Action: common.MatcherExclude,
				},
				// Second matcher: include eth_call with empty=allow
				{
					Method: "eth_call",
					Empty:  common.CacheEmptyBehaviorAllow,
					Action: common.MatcherInclude,
				},
				// Third matcher: include eth_getBalance with empty=only
				{
					Method: "eth_getBalance",
					Empty:  common.CacheEmptyBehaviorOnly,
					Action: common.MatcherInclude,
				},
			},
			Connector: "test",
		}, mockConnector)
		assert.NoError(t, err)

		// Test MatchesForGetWithMatcher returns the correct matcher
		matched, matcher := policy.MatchesForGetWithMatcher("1", "eth_call", nil, common.DataFinalityStateFinalized)
		assert.True(t, matched)
		assert.NotNil(t, matcher)
		assert.Equal(t, common.CacheEmptyBehaviorAllow, matcher.Empty)
		assert.Equal(t, "eth_call", matcher.Method)

		matched, matcher = policy.MatchesForGetWithMatcher("1", "eth_getBalance", nil, common.DataFinalityStateFinalized)
		assert.True(t, matched)
		assert.NotNil(t, matcher)
		assert.Equal(t, common.CacheEmptyBehaviorOnly, matcher.Empty)
		assert.Equal(t, "eth_getBalance", matcher.Method)

		// Test MatchesForSetWithMatcher
		matched, matcher = policy.MatchesForSetWithMatcher("1", "eth_call", nil, common.DataFinalityStateFinalized, false)
		assert.True(t, matched)
		assert.NotNil(t, matcher)
		assert.Equal(t, common.CacheEmptyBehaviorAllow, matcher.Empty)

		// Test that PolicyWithMatcher correctly returns the matched matcher's empty state
		pwm := &PolicyWithMatcher{
			Policy: policy,
			Matcher: &common.MatcherConfig{
				Method: "eth_call",
				Empty:  common.CacheEmptyBehaviorAllow,
				Action: common.MatcherInclude,
			},
		}
		assert.Equal(t, common.CacheEmptyBehaviorAllow, pwm.EmptyState())
	})
}
