package data

import (
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/matchers"
	"github.com/erpc/erpc/util"
)

type CachePolicy struct {
	config    *common.CachePolicyConfig
	connector Connector
	str       string
	minSize   *int
	maxSize   *int
}

// PolicyWithMatcher holds a policy along with the specific matcher that matched
type PolicyWithMatcher struct {
	Policy  *CachePolicy
	Matcher *common.MatcherConfig
}

// EmptyState returns the empty state behavior from the matched matcher
func (pm *PolicyWithMatcher) EmptyState() common.CacheEmptyBehavior {
	if pm.Matcher != nil {
		return pm.Matcher.Empty
	}
	// Fall back to policy's default behavior if no matcher
	return pm.Policy.EmptyState()
}

func NewCachePolicy(cfg *common.CachePolicyConfig, connector Connector) (*CachePolicy, error) {
	var minSize, maxSize *int

	if cfg.MinItemSize != nil {
		parsed, err := util.ParseByteSize(*cfg.MinItemSize)
		if err != nil {
			return nil, fmt.Errorf("invalid minItemSize: %w", err)
		}
		minSize = &parsed
	}

	if cfg.MaxItemSize != nil {
		parsed, err := util.ParseByteSize(*cfg.MaxItemSize)
		if err != nil {
			return nil, fmt.Errorf("invalid maxItemSize: %w", err)
		}
		maxSize = &parsed
	}

	// Build matcher details string
	var matcherStrs []string
	for _, matcher := range cfg.Matchers {
		matcherStr := fmt.Sprintf("method=%s", matcher.Method)
		if matcher.Network != "" {
			matcherStr += fmt.Sprintf(" network=%s", matcher.Network)
		}
		if len(matcher.Finality) > 0 {
			finalityStrs := make([]string, len(matcher.Finality))
			for i, f := range matcher.Finality {
				finalityStrs[i] = f.String()
			}
			matcherStr += fmt.Sprintf(" finality=[%s]", strings.Join(finalityStrs, ","))
		}
		if matcher.Params != nil && len(matcher.Params) > 0 {
			matcherStr += fmt.Sprintf(" params=%d", len(matcher.Params))
		}
		matcherStr += fmt.Sprintf(" action=%s", matcher.Action)
		matcherStrs = append(matcherStrs, fmt.Sprintf("{%s}", matcherStr))
	}
	str := fmt.Sprintf("matchers=[%s]", strings.Join(matcherStrs, " "))

	if minSize != nil || maxSize != nil {
		str = fmt.Sprintf("%s minSize=%d maxSize=%d", str, minSize, maxSize)
	}

	str = fmt.Sprintf("policy(%s)", str)

	return &CachePolicy{
		config:    cfg,
		connector: connector,
		str:       str,
		minSize:   minSize,
		maxSize:   maxSize,
	}, nil
}

func (p *CachePolicy) MarshalJSON() ([]byte, error) {
	return common.SonicCfg.Marshal(p.config)
}

func (p *CachePolicy) MatchesForSet(networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) (bool, error) {
	matched, _ := p.MatchesForSetWithMatcher(networkId, method, params, finality, isEmptyish)
	return matched, nil
}

// MatchesForSetWithMatcher returns both the match result and the specific matcher that matched
func (p *CachePolicy) MatchesForSetWithMatcher(networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) (bool, *common.MatcherConfig) {
	// Respect appliesTo directive for set
	if p.config.AppliesTo != "" && p.config.AppliesTo != common.CachePolicyAppliesToBoth && p.config.AppliesTo != common.CachePolicyAppliesToSet {
		return false, nil
	}

	// Check each matcher from last to first (last takes precedence)
	for i := len(p.config.Matchers) - 1; i >= 0; i-- {
		matcher := p.config.Matchers[i]
		if matchers.MatchConfig(matcher, networkId, method, params, finality, isEmptyish) {
			// For cache policies, we only cache if action is include
			return matcher.Action == common.MatcherInclude, matcher
		}
	}
	// No matcher matched
	return false, nil
}

func (p *CachePolicy) MatchesForGet(networkId, method string, params []interface{}, finality common.DataFinalityState) (bool, error) {
	matched, _ := p.MatchesForGetWithMatcher(networkId, method, params, finality)
	return matched, nil
}

// MatchesForGetWithMatcher returns both the match result and the specific matcher that matched
func (p *CachePolicy) MatchesForGetWithMatcher(networkId, method string, params []interface{}, finality common.DataFinalityState) (bool, *common.MatcherConfig) {
	// Respect appliesTo directive for get
	if p.config.AppliesTo != "" && p.config.AppliesTo != common.CachePolicyAppliesToBoth && p.config.AppliesTo != common.CachePolicyAppliesToGet {
		return false, nil
	}

	// For GET operations, we need special handling of finality
	// to be more flexible in finding cached data

	// Check each matcher from last to first (last takes precedence)
	for i := len(p.config.Matchers) - 1; i >= 0; i-- {
		matcher := p.config.Matchers[i]

		// Check network match
		if matcher.Network != "" {
			match, err := common.WildcardMatch(matcher.Network, networkId)
			if err != nil || !match {
				continue
			}
		}

		// Check method match
		if matcher.Method != "" {
			match, err := common.WildcardMatch(matcher.Method, method)
			if err != nil || !match {
				continue
			}
		}

		// Check params match
		if len(matcher.Params) > 0 {
			match, err := matchers.MatchParams(matcher.Params, params)
			if err != nil || !match {
				continue
			}
		}

		// At this point, network/method/params match. Now handle finality with special GET semantics.
		finalityMatches := false

		if len(matcher.Finality) == 0 {
			// No finality constraint means it matches any finality
			finalityMatches = true
		} else if finality == common.DataFinalityStateUnknown {
			// Unknown finality matches any policy (we don't know what finality the cached data has)
			finalityMatches = true
		} else if finality == common.DataFinalityStateFinalized {
			// Finalized requests can match both finalized and unfinalized policies
			// (data might have been cached as unfinalized but is now finalized)
			for _, f := range matcher.Finality {
				if f == common.DataFinalityStateFinalized || f == common.DataFinalityStateUnfinalized {
					finalityMatches = true
					break
				}
			}
		} else {
			// For unfinalized or realtime, require exact match
			for _, f := range matcher.Finality {
				if f == finality {
					finalityMatches = true
					break
				}
			}
		}

		if finalityMatches {
			// This matcher fully matches, return based on action
			if matcher.Action == common.MatcherInclude {
				return true, matcher
			}
			return false, nil
		}
	}
	// No matcher matched
	return false, nil
}

func (p *CachePolicy) MatchesSizeLimits(size int) bool {
	if p.minSize != nil && size < *p.minSize {
		return false
	}
	if p.maxSize != nil && size > *p.maxSize {
		return false
	}
	return true
}

func (p *CachePolicy) EmptyState() common.CacheEmptyBehavior {
	// For legacy configs, use the legacy field
	if p.config.Empty != 0 || p.config.Network != "" || p.config.Method != "" {
		return p.config.Empty
	}

	// For new matcher configs, find the first INCLUDE matcher (skip auto-added excludes)
	if len(p.config.Matchers) > 0 {
		for _, matcher := range p.config.Matchers {
			if matcher != nil && matcher.Action == common.MatcherInclude {
				return matcher.Empty
			}
		}
		// If no include matchers, use the first one
		if p.config.Matchers[0] != nil {
			return p.config.Matchers[0].Empty
		}
	}

	// Fall back to legacy field
	return p.config.Empty
}

func (p *CachePolicy) GetConnector() Connector {
	return p.connector
}

func (p *CachePolicy) String() string {
	return p.str
}

func (p *CachePolicy) GetTTL() *time.Duration {
	return p.config.TTL.DurationPtr()
}
