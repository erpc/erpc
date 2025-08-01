package matchers

import (
	"fmt"
	"strconv"

	"github.com/erpc/erpc/common"
)

// matchConfig checks if all fields in the config match
func matchConfig(config *common.MatcherConfig, networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool) bool {
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

	// Check empty behavior if specified (only matters when we have a response)
	if config.Empty != 0 && isEmptyish {
		if config.Empty == common.CacheEmptyBehaviorIgnore {
			return false
		}
	} else if config.Empty == common.CacheEmptyBehaviorOnly && !isEmptyish {
		return false
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

// matchParam matches a single parameter
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

// Match provides a simplified interface for request/response matching
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

	// Determine if response is empty
	isEmptyish := false
	if resp != nil {
		isEmptyish = resp.IsObjectNull() || resp.IsResultEmptyish()
	}

	// Check each config from last to first (last takes precedence)
	for i := len(configs) - 1; i >= 0; i-- {
		config := configs[i]
		if matchConfig(config, networkId, method, params, finality, isEmptyish) {
			return config.Action == common.MatcherInclude
		}
	}

	// Default to false if no match
	return false
}
