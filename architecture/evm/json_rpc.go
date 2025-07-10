package evm

import (
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

// normalizeParam attempts to normalize a parameter value.
// Returns the normalized value and whether normalization occurred.
func normalizeParam(param interface{}) (interface{}, bool) {
	switch v := param.(type) {
	case string:
		if strings.HasPrefix(v, "0x") {
			if normalized, err := common.NormalizeHex(v); err == nil && normalized != v {
				return normalized, true
			}
		}
		return v, false
	case float64:
		str := fmt.Sprintf("%f", v)
		if normalized, err := common.NormalizeHex(str); err == nil {
			return normalized, true
		}
		return v, false
	case map[string]interface{}:
		if blockNumber, ok := v["blockNumber"]; ok {
			if normalized, err := common.NormalizeHex(blockNumber); err == nil && normalized != blockNumber {
				// Create a new map to avoid modifying shared data
				newMap := make(map[string]interface{}, len(v))
				for k, val := range v {
					newMap[k] = val
				}
				newMap["blockNumber"] = normalized
				return newMap, true
			}
		}
		return v, false
	default:
		return v, false
	}
}

// replaceParam creates a new params slice with the value at the given index replaced.
func replaceParam(params []interface{}, index int, newValue interface{}) []interface{} {
	newParams := make([]interface{}, len(params))
	copy(newParams, params)
	newParams[index] = newValue
	return newParams
}

func NormalizeHttpJsonRpc(nrq *common.NormalizedRequest, jrq *common.JsonRpcRequest) {
	// First pass: check if we need to modify anything (with read lock)
	jrq.RLock()
	var needsUpdate bool
	var newParams []interface{}

	switch jrq.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		if len(jrq.Params) > 0 {
			if normalized, changed := normalizeParam(jrq.Params[0]); changed {
				needsUpdate = true
				newParams = replaceParam(jrq.Params, 0, normalized)
			}
		}

	case "eth_getBalance",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		if len(jrq.Params) > 1 {
			if normalized, changed := normalizeParam(jrq.Params[1]); changed {
				needsUpdate = true
				newParams = replaceParam(jrq.Params, 1, normalized)
			}
		}

	case "eth_getStorageAt":
		if len(jrq.Params) > 2 {
			if normalized, changed := normalizeParam(jrq.Params[2]); changed {
				needsUpdate = true
				newParams = replaceParam(jrq.Params, 2, normalized)
			}
		}

	case "eth_getLogs":
		if len(jrq.Params) > 0 {
			if paramsMap, ok := jrq.Params[0].(map[string]interface{}); ok {
				// Check if we need to normalize anything
				fromBlock, hasFromBlock := paramsMap["fromBlock"]
				toBlock, hasToBlock := paramsMap["toBlock"]

				fromNormalized := false
				toNormalized := false
				var normalizedFrom, normalizedTo string

				if hasFromBlock {
					if b, err := common.NormalizeHex(fromBlock); err == nil && b != fromBlock {
						normalizedFrom = b
						fromNormalized = true
					}
				}

				if hasToBlock {
					if b, err := common.NormalizeHex(toBlock); err == nil && b != toBlock {
						normalizedTo = b
						toNormalized = true
					}
				}

				// Only create new slices/maps if we actually normalized something
				if fromNormalized || toNormalized {
					newMap := make(map[string]interface{}, len(paramsMap))
					for k, v := range paramsMap {
						newMap[k] = v
					}

					if fromNormalized {
						newMap["fromBlock"] = normalizedFrom
					}
					if toNormalized {
						newMap["toBlock"] = normalizedTo
					}

					needsUpdate = true
					newParams = replaceParam(jrq.Params, 0, newMap)
				}
			}
		}
	}

	jrq.RUnlock()

	// Only acquire write lock if we need to update
	if needsUpdate {
		jrq.Lock()
		jrq.Params = newParams
		jrq.Unlock()
	}
}
