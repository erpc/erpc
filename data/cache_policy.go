package data

import (
	"fmt"
	"strconv"
	"time"

	"github.com/erpc/erpc/common"
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

	str := fmt.Sprintf("network=%s method=%s finality=%s", cfg.Network, cfg.Method, cfg.Finality.String())
	if minSize != nil || maxSize != nil {
		str = fmt.Sprintf("%s minSize=%d maxSize=%d", str, minSize, maxSize)
	}
	if cfg.Params != nil {
		str = fmt.Sprintf("%s params=%v", str, cfg.Params != nil)
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
	match, err := common.WildcardMatch(p.config.Network, networkId)
	if err != nil {
		return false, err
	}
	if !match {
		return false, nil
	}

	match, err = common.WildcardMatch(p.config.Method, method)
	if err != nil {
		return false, err
	}
	if !match {
		return false, nil
	}

	match, err = p.matchParams(params)
	if err != nil {
		return false, err
	}
	if !match {
		return false, nil
	}

	if isEmptyish && p.config.Empty == common.CacheEmptyBehaviorIgnore {
		return false, nil
	}

	if !isEmptyish && p.config.Empty == common.CacheEmptyBehaviorOnly {
		return false, nil
	}

	// TODO do we need to make unknown superset of finalized/unfinalized?
	// TODO do we need to differentiate between 'unknown' (eth_trace*) and 'missing' (chain does not support finalized)?
	return p.config.Finality == finality, nil
}

func (p *CachePolicy) MatchesForGet(networkId, method string, params []interface{}, finality common.DataFinalityState) (bool, error) {
	match, err := common.WildcardMatch(p.config.Network, networkId)
	if err != nil {
		return false, err
	}
	if !match {
		return false, nil
	}

	match, err = common.WildcardMatch(p.config.Method, method)
	if err != nil {
		return false, err
	}
	if !match {
		return false, nil
	}

	match, err = p.matchParams(params)
	if err != nil {
		return false, err
	}
	if !match {
		return false, nil
	}

	// When finality is already known based on request params (e.g. eth_getBlockNumber(latest))
	// we can only match policies that have the same finality.
	if finality == common.DataFinalityStateFinalized {
		return p.config.Finality == common.DataFinalityStateFinalized ||
			// If request implies that data is finalized, it could be that originally written response was still unfinalized,
			// therefore we should still match unfinalized policies as well.
			p.config.Finality == common.DataFinalityStateUnfinalized, nil
	} else if finality == common.DataFinalityStateUnfinalized {
		return p.config.Finality == common.DataFinalityStateUnfinalized, nil
	} else if finality == common.DataFinalityStateRealtime {
		return p.config.Finality == common.DataFinalityStateRealtime, nil
	}

	// When fetching data for unknown finality we need to iterate over all policies as we don't know finality of the response when originally written.
	// We will iterate from first to last policy (matched on network/method) to see which one has the data.
	// Therefore it is recommended to put the fastest storage and most up-to-date policy first.
	return true, nil
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

func (p *CachePolicy) matchParams(params []interface{}) (bool, error) {
	if len(p.config.Params) == 0 {
		return true, nil
	}

	for i, pattern := range p.config.Params {
		var v interface{}
		if i < len(params) {
			v = params[i]
		}
		match, err := matchParam(pattern, v)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}

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
	return p.config.TTL.DurationPtr()
}
