package matchers

import (
	"fmt"
	"strconv"

	"github.com/erpc/erpc/common"
)

// Matcher handles matching logic for requests and responses
type Matcher struct {
	configs []*common.MatcherConfig
}

// NewMatcher creates a new matcher with the given configs
func NewMatcher(configs []*common.MatcherConfig) *Matcher {
	return &Matcher{
		configs: configs,
	}
}

// MatchRequest evaluates if a request matches based on the configs
func (m *Matcher) MatchRequest(networkId, method string, params []interface{}, finality common.DataFinalityState) MatchResult {
	if len(m.configs) == 0 {
		return MatchResult{Matched: true, Action: common.MatcherInclude}
	}

	for _, config := range m.configs {
		if m.matchConfig(config, networkId, method, params, finality, false) {
			return MatchResult{Matched: true, Action: config.Action}
		}
	}

	// Default behavior if no matchers match
	return MatchResult{Matched: false, Action: common.MatcherExclude}
}

// MatchForCache evaluates if a response should be cached
func (m *Matcher) MatchForCache(networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) MatchResult {
	if len(m.configs) == 0 {
		return MatchResult{Matched: true, Action: common.MatcherInclude}
	}

	for _, config := range m.configs {
		if m.matchConfigWithEmpty(config, networkId, method, params, finality, isEmptyish) {
			return MatchResult{Matched: true, Action: config.Action}
		}
	}

	return MatchResult{Matched: false, Action: common.MatcherExclude}
}

// matchConfig checks if all fields in the config match
func (m *Matcher) matchConfig(config *common.MatcherConfig, networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) bool {
	// Match network
	if config.Network != "" {
		match, err := common.WildcardMatch(config.Network, networkId)
		if err != nil || !match {
			return false
		}
	}

	// Match method
	if config.Method != "" {
		match, err := common.WildcardMatch(config.Method, method)
		if err != nil || !match {
			return false
		}
	}

	// Match params
	if len(config.Params) > 0 {
		match, err := matchParams(config.Params, params)
		if err != nil || !match {
			return false
		}
	}

	// Match finality (only if specified)
	if config.Finality != 0 {
		if config.Finality != finality {
			return false
		}
	}

	return true
}

// matchConfigWithEmpty includes empty behavior matching for cache operations
func (m *Matcher) matchConfigWithEmpty(config *common.MatcherConfig, networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) bool {
	// First check basic matching
	if !m.matchConfig(config, networkId, method, params, finality, isEmptyish) {
		return false
	}

	// Then check empty behavior if specified
	if config.Empty != 0 {
		if isEmptyish && config.Empty == common.CacheEmptyBehaviorIgnore {
			return false
		}
		if !isEmptyish && config.Empty == common.CacheEmptyBehaviorOnly {
			return false
		}
	}

	return true
}

// matchParams checks if params match the pattern
func matchParams(pattern []interface{}, params []interface{}) (bool, error) {
	if len(pattern) == 0 {
		return true, nil
	}

	for i, p := range pattern {
		var v interface{}
		if i < len(params) {
			v = params[i]
		}
		match, err := matchParam(p, v)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}

// matchParam matches a single parameter (extracted from cache policy)
func matchParam(pattern interface{}, param interface{}) (bool, error) {
	switch p := pattern.(type) {
	case map[string]interface{}:
		// For objects, recursively match each field
		paramMap, ok := param.(map[string]interface{})
		if !ok {
			return false, nil
		}
		for k, v := range p {
			paramValue, exists := paramMap[k]
			match, err := matchParam(v, paramValue)
			if err != nil {
				return false, err
			}
			if !exists || !match {
				return false, nil
			}
		}
		return true, nil
	case []interface{}:
		// For arrays, match each element
		paramArray, ok := param.([]interface{})
		if !ok || len(p) != len(paramArray) {
			return false, nil
		}
		for i, v := range p {
			match, err := matchParam(v, paramArray[i])
			if err != nil {
				return false, err
			}
			if !match {
				return false, nil
			}
		}
		return true, nil
	case string:
		match, err := common.WildcardMatch(p, paramToString(param))
		if err != nil {
			return false, err
		}
		return match, nil
	default:
		// For other types, convert both to strings and compare
		return paramToString(pattern) == paramToString(param), nil
	}
}

// paramToString converts a parameter to string for matching
func paramToString(param interface{}) string {
	if param == nil {
		return ""
	}

	switch v := param.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
