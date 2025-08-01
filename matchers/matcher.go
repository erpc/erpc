package matchers

import (
	"fmt"
	"strconv"

	"github.com/erpc/erpc/common"
)

// ConfigMatcher handles matching logic for requests and responses using MatcherConfig
type ConfigMatcher struct {
	configs []*common.MatcherConfig
}

// NewConfigMatcher creates a new matcher with the given configs
func NewConfigMatcher(configs []*common.MatcherConfig) *ConfigMatcher {
	return &ConfigMatcher{
		configs: configs,
	}
}

// MatchRequest evaluates if a request matches based on the configs
func (m *ConfigMatcher) MatchRequest(networkId, method string, params []interface{}, finality common.DataFinalityState) common.MatchResult {
	if len(m.configs) == 0 {
		return common.MatchResult{Matched: true, Action: common.MatcherInclude}
	}

	for i := len(m.configs) - 1; i >= 0; i-- {
		config := m.configs[i]
		if m.matchConfig(config, networkId, method, params, finality, false) {
			return common.MatchResult{Matched: true, Action: config.Action}
		}
	}

	// Default behavior if no matchers match - this should not happen if catch-all rules are properly configured
	// We determine the default based on the intended behavior:
	// - If the first matcher (processed last) is an include, default to exclude
	// - If the first matcher (processed last) is an exclude, default to include
	// - Otherwise, default to exclude for safety (fail-closed)
	if len(m.configs) > 0 {
		firstMatcher := m.configs[0]
		if firstMatcher.Action == common.MatcherInclude {
			return common.MatchResult{Matched: false, Action: common.MatcherExclude}
		} else if firstMatcher.Action == common.MatcherExclude {
			return common.MatchResult{Matched: false, Action: common.MatcherInclude}
		}
	}

	// Fallback to exclude for safety
	return common.MatchResult{Matched: false, Action: common.MatcherExclude}
}

// MatchForCache evaluates if a response should be cached
func (m *ConfigMatcher) MatchForCache(networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) common.MatchResult {
	if len(m.configs) == 0 {
		return common.MatchResult{Matched: true, Action: common.MatcherInclude}
	}

	for i := len(m.configs) - 1; i >= 0; i-- {
		config := m.configs[i]
		if m.matchConfigWithEmpty(config, networkId, method, params, finality, isEmptyish) {
			return common.MatchResult{Matched: true, Action: config.Action}
		}
	}

	return common.MatchResult{Matched: false, Action: common.MatcherExclude}
}

// matchConfig checks if all fields in the config match
func (m *ConfigMatcher) matchConfig(config *common.MatcherConfig, networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) bool {
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
	if len(config.Finality) > 0 {
		matched := false
		for _, f := range config.Finality {
			if f == finality {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

// matchConfigWithEmpty includes empty behavior matching for cache operations
func (m *ConfigMatcher) matchConfigWithEmpty(config *common.MatcherConfig, networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) bool {
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

// Match provides a simplified interface for request matching
func Match(configs []*common.MatcherConfig, req *common.NormalizedRequest, resp *common.NormalizedResponse) bool {
	if len(configs) == 0 {
		return false
	}

	// Extract required fields from request
	method, _ := req.Method()
	finality := req.Finality(nil)
	networkId := ""
	if req.NetworkId() != "" {
		networkId = req.NetworkId()
	}

	var params []interface{}
	if jrpcReq, err := req.JsonRpcRequest(); err == nil && jrpcReq != nil {
		params = jrpcReq.Params
	}

	// Create matcher and check
	matcher := NewConfigMatcher(configs)
	result := matcher.MatchRequest(networkId, method, params, finality)

	return result.Matched && result.Action == common.MatcherInclude
}
