package matchers

import (
	"github.com/erpc/erpc/common"
)

// ConvertLegacyFailsafeConfig converts old MatchMethod and MatchFinality to new Matchers format
func ConvertLegacyFailsafeConfig(cfg *common.FailsafeConfig) {
	if cfg == nil {
		return
	}

	// If already has matchers, don't convert
	if len(cfg.Matchers) > 0 {
		return
	}

	// If has legacy fields, convert them
	if cfg.MatchMethod != "" || len(cfg.MatchFinality) > 0 {
		matcher := &common.MatcherConfig{
			Action: common.MatcherInclude,
		}

		// Set method if provided
		if cfg.MatchMethod != "" {
			matcher.Method = cfg.MatchMethod
		}

		// If has finality states, create separate matchers for each
		if len(cfg.MatchFinality) > 0 {
			cfg.Matchers = make([]*common.MatcherConfig, 0, len(cfg.MatchFinality))
			for _, finality := range cfg.MatchFinality {
				finalityMatcher := &common.MatcherConfig{
					Method:   cfg.MatchMethod,
					Finality: finality,
					Action:   common.MatcherInclude,
				}
				cfg.Matchers = append(cfg.Matchers, finalityMatcher)
			}
		} else {
			// Just method matcher
			cfg.Matchers = []*common.MatcherConfig{matcher}
		}
	}
}

// ShouldMatchFailsafe checks if a request matches the failsafe config
func ShouldMatchFailsafe(cfg *common.FailsafeConfig, method string, finality common.DataFinalityState) bool {
	// Convert legacy config if needed
	ConvertLegacyFailsafeConfig(cfg)

	// If no matchers, it matches everything (backward compatibility)
	if len(cfg.Matchers) == 0 {
		return true
	}

	// Create a matcher and check
	matcher := NewConfigMatcher(cfg.Matchers)
	result := matcher.MatchRequest("", method, nil, finality)

	return result.Matched && result.Action == common.MatcherInclude
}
