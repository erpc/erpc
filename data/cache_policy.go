package data

import (
	"fmt"
	"strconv"
	"time"

	"github.com/erpc/erpc/common"
)

type CachePolicy struct {
	config    *common.CachePolicyConfig
	connector Connector
	str       string
}

func NewCachePolicy(cfg *common.CachePolicyConfig, connector Connector) (*CachePolicy, error) {
	return &CachePolicy{
		config:    cfg,
		connector: connector,
		str:       fmt.Sprintf("network=%s method=%s finality=%s params=%v", cfg.Network, cfg.Method, cfg.Finality.String(), cfg.Params != nil),
	}, nil
}

func (p *CachePolicy) MarshalJSON() ([]byte, error) {
	return common.SonicCfg.Marshal(p.config)
}

func (p *CachePolicy) MatchesForSet(networkId, method string, params []interface{}, finality common.DataFinalityState) bool {
	if !common.WildcardMatch(p.config.Network, networkId) {
		return false
	}

	if !common.WildcardMatch(p.config.Method, method) {
		return false
	}

	if !p.matchParams(params) {
		return false
	}

	// TODO do we need to make unknown superset of finalized/unfinalized?
	// TODO do we need to differentiate between 'unknown' (eth_trace*) and 'missing' (chain does not support finalized)?
	return p.config.Finality == finality
}

func (p *CachePolicy) MatchesForGet(networkId, method string, params []interface{}) bool {
	if !common.WildcardMatch(p.config.Network, networkId) {
		return false
	}

	if !common.WildcardMatch(p.config.Method, method) {
		return false
	}

	if !p.matchParams(params) {
		return false
	}

	// When fetching data we need to iterate over all policies as we don't know finality of the data when originally written
	// We will iterate from first to last policy (matched on network/method) to see which one has the data
	// Therefore it is recommended to put the fastest most up-to-date policy first
	return true
}

func (p *CachePolicy) EmptyState() common.CacheEmptyBehavior {
	return p.config.Empty
}

func (p *CachePolicy) matchParams(params []interface{}) bool {
	if len(p.config.Params) == 0 {
		return true
	}

	for i, pattern := range p.config.Params {
		var v interface{}
		if i < len(params) {
			v = params[i]
		}
		if !matchParam(pattern, v) {
			return false
		}
	}
	return true
}

func matchParam(pattern interface{}, param interface{}) bool {
	switch p := pattern.(type) {
	case map[string]interface{}:
		// For objects, recursively match each field
		paramMap, ok := param.(map[string]interface{})
		if !ok {
			return false
		}
		for k, v := range p {
			paramValue, exists := paramMap[k]
			if !exists || !matchParam(v, paramValue) {
				return false
			}
		}
		return true
	case []interface{}:
		// For arrays, match each element
		paramArray, ok := param.([]interface{})
		if !ok || len(p) != len(paramArray) {
			return false
		}
		for i, v := range p {
			if !matchParam(v, paramArray[i]) {
				return false
			}
		}
		return true
	case string:
		return common.WildcardMatch(p, paramToString(param))
	default:
		// For other types, convert both to strings and compare
		return paramToString(pattern) == paramToString(param)
	}
}

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

func (p *CachePolicy) GetConnector() Connector {
	return p.connector
}

func (p *CachePolicy) String() string {
	return p.str
}

func (p *CachePolicy) GetTTL() *time.Duration {
	return &p.config.TTL
}
