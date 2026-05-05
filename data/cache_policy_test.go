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
					Empty:  common.EmptyBehaviorPtr(common.CacheEmptyBehaviorIgnore),
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
			Finality:  common.FinalityPtr(common.DataFinalityStateFinalized),
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
					Empty:  common.EmptyBehaviorPtr(common.CacheEmptyBehaviorIgnore),
					Action: common.MatcherExclude,
				},
				// Second matcher: include eth_call with empty=allow
				{
					Method: "eth_call",
					Empty:  common.EmptyBehaviorPtr(common.CacheEmptyBehaviorAllow),
					Action: common.MatcherInclude,
				},
				// Third matcher: include eth_getBalance with empty=only
				{
					Method: "eth_getBalance",
					Empty:  common.EmptyBehaviorPtr(common.CacheEmptyBehaviorOnly),
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
		assert.NotNil(t, matcher.Empty)
		assert.Equal(t, common.CacheEmptyBehaviorAllow, *matcher.Empty)
		assert.Equal(t, "eth_call", matcher.Method)

		matched, matcher = policy.MatchesForGetWithMatcher("1", "eth_getBalance", nil, common.DataFinalityStateFinalized)
		assert.True(t, matched)
		assert.NotNil(t, matcher)
		assert.NotNil(t, matcher.Empty)
		assert.Equal(t, common.CacheEmptyBehaviorOnly, *matcher.Empty)
		assert.Equal(t, "eth_getBalance", matcher.Method)

		// Test MatchesForSetWithMatcher
		matched, matcher = policy.MatchesForSetWithMatcher("1", "eth_call", nil, common.DataFinalityStateFinalized, false)
		assert.True(t, matched)
		assert.NotNil(t, matcher)
		assert.NotNil(t, matcher.Empty)
		assert.Equal(t, common.CacheEmptyBehaviorAllow, *matcher.Empty)

		// Test that PolicyWithMatcher correctly returns the matched matcher's empty state
		pwm := &PolicyWithMatcher{
			Policy: policy,
			Matcher: &common.MatcherConfig{
				Method: "eth_call",
				Empty:  common.EmptyBehaviorPtr(common.CacheEmptyBehaviorAllow),
				Action: common.MatcherInclude,
			},
		}
		assert.Equal(t, common.CacheEmptyBehaviorAllow, pwm.EmptyState())
	})

	t.Run("MatchesForGet ignores empty constraint by design (filter happens post-fetch)", func(t *testing.T) {
		// Pins the architectural intent flagged by cursor-bot MED in PR #388:
		// MatchesForGetWithMatcher does NOT evaluate the matcher's `empty:`
		// constraint, because at GET-policy-selection time the cached value
		// has not yet been retrieved from the connector. Empty-filtering is
		// deferred to the cache layer's post-fetch hook (see
		// architecture/evm/json_rpc_cache.go around line 336, which switches
		// on PolicyWithMatcher.EmptyState()).
		//
		// This regression test guards against a future refactor that
		// "tightens" the GET path and accidentally rejects all GETs for
		// matchers with empty:only — the SET-side filter has already
		// guaranteed only empty entries got persisted, so they must remain
		// reachable from GET.
		onlyEmptyPolicy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				{
					Method: "eth_getLogs",
					Empty:  common.EmptyBehaviorPtr(common.CacheEmptyBehaviorOnly),
					Action: common.MatcherInclude,
				},
			},
			Connector: "test",
		}, mockConnector)
		assert.NoError(t, err)

		// MatchesForGet must return true even though we have no notion of
		// emptiness here — empty-filtering is the cache layer's job once the
		// value is in hand.
		matched, matcher := onlyEmptyPolicy.MatchesForGetWithMatcher("1", "eth_getLogs", nil, common.DataFinalityStateFinalized)
		assert.True(t, matched, "GET-policy selection must succeed regardless of matcher.Empty")
		assert.NotNil(t, matcher)
		assert.NotNil(t, matcher.Empty)
		assert.Equal(t, common.CacheEmptyBehaviorOnly, *matcher.Empty,
			"the returned matcher must carry the original Empty constraint so the post-fetch filter can apply it")

		// Symmetric guard for empty:ignore — also must not block GET-selection.
		ignoreEmptyPolicy, err := NewCachePolicy(&common.CachePolicyConfig{
			Matchers: []*common.MatcherConfig{
				{
					Method: "eth_getLogs",
					Empty:  common.EmptyBehaviorPtr(common.CacheEmptyBehaviorIgnore),
					Action: common.MatcherInclude,
				},
			},
			Connector: "test",
		}, mockConnector)
		assert.NoError(t, err)

		matched, _ = ignoreEmptyPolicy.MatchesForGetWithMatcher("1", "eth_getLogs", nil, common.DataFinalityStateFinalized)
		assert.True(t, matched, "GET-policy selection for empty:ignore must also succeed (filter is post-fetch)")
	})

	t.Run("FailsafeConfig.Copy deep-copies matcher.Empty pointer", func(t *testing.T) {
		// PR #388 cursor-bot LOW: the pointer-types refactor introduced
		// *CacheEmptyBehavior, and the existing FailsafeConfig.Copy() shallow-
		// copied the pointer (both copies pointed at the same value). This
		// test pins the deep-copy fix.
		original := &common.FailsafeConfig{
			Matchers: []*common.MatcherConfig{
				{
					Method: "eth_call",
					Empty:  common.EmptyBehaviorPtr(common.CacheEmptyBehaviorAllow),
					Action: common.MatcherInclude,
				},
			},
		}

		clone := original.Copy()

		// Pointers must differ.
		assert.NotSame(t, original.Matchers[0].Empty, clone.Matchers[0].Empty,
			"clone must have its own *CacheEmptyBehavior pointer, not share the original's")

		// Mutating the clone's pointee must not corrupt the original.
		*clone.Matchers[0].Empty = common.CacheEmptyBehaviorOnly
		assert.Equal(t, common.CacheEmptyBehaviorAllow, *original.Matchers[0].Empty,
			"mutating clone.Empty must not affect original.Empty")
	})
}
