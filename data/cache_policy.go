package data

import (
	"fmt"
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

	var str string
	if len(cfg.Matchers) > 0 {
		str = fmt.Sprintf("matchers=%d", len(cfg.Matchers))
	} else {
		// Legacy format
		str = fmt.Sprintf("network=%s method=%s finality=%s", cfg.Network, cfg.Method, cfg.Finality.String())
		if cfg.Params != nil {
			str = fmt.Sprintf("%s params=%v", str, cfg.Params != nil)
		}
	}

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
	// Check each matcher from last to first (last takes precedence)
	for i := len(p.config.Matchers) - 1; i >= 0; i-- {
		matcher := p.config.Matchers[i]
		if matchers.MatchConfig(matcher, networkId, method, params, finality, isEmptyish) {
			// For cache policies, we only cache if action is include
			return matcher.Action == common.MatcherInclude, nil
		}
	}
	// No matcher matched
	return false, nil
}

func (p *CachePolicy) MatchesForGet(networkId, method string, params []interface{}, finality common.DataFinalityState) (bool, error) {
	// For GET operations, we need special handling of finality
	// to be more flexible in finding cached data

	// Check each matcher from last to first (last takes precedence)
	for i := len(p.config.Matchers) - 1; i >= 0; i-- {
		matcher := p.config.Matchers[i]

		// For GET, we don't have isEmptyish, so pass false
		if matchers.MatchConfig(matcher, networkId, method, params, finality, false) {
			if matcher.Action == common.MatcherInclude {
				return true, nil
			}
		}

		// Special finality handling for cache GET
		if finality == common.DataFinalityStateUnknown {
			// Unknown finality should match any policy regardless of finality settings
			if matchers.MatchConfig(matcher, networkId, method, params, common.DataFinalityStateFinalized, false) ||
				matchers.MatchConfig(matcher, networkId, method, params, common.DataFinalityStateUnfinalized, false) ||
				matchers.MatchConfig(matcher, networkId, method, params, common.DataFinalityStateRealtime, false) {
				if matcher.Action == common.MatcherInclude {
					return true, nil
				}
			}
		} else if finality == common.DataFinalityStateFinalized {
			// Finalized requests can match unfinalized policies too
			// (data might have been cached as unfinalized but is now finalized)
			if matchers.MatchConfig(matcher, networkId, method, params, common.DataFinalityStateUnfinalized, false) {
				if matcher.Action == common.MatcherInclude {
					return true, nil
				}
			}
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
