package evm

import (
	"context"
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

// resolveBlockTagToHex resolves well-known tags to concrete hex numbers using highest known state.
func resolveBlockTagToHex(ctx context.Context, nrq *common.NormalizedRequest, tag string) (string, bool) {
	if nrq == nil {
		return "", false
	}
	network := nrq.Network()
	if network == nil {
		return "", false
	}
	switch tag {
	case "latest":
		if bn := network.EvmHighestLatestBlockNumber(ctx); bn > 0 {
			if hx, err := common.NormalizeHex(bn); err == nil {
				return hx, true
			}
		}
	case "finalized":
		if bn := network.EvmHighestFinalizedBlockNumber(ctx); bn > 0 {
			if hx, err := common.NormalizeHex(bn); err == nil {
				return hx, true
			}
		}
	}
	return "", false
}

// normalizeOrTranslateParam normalizes hex representations or translates well-known tags to concrete hex.
func normalizeOrTranslateParam(ctx context.Context, nrq *common.NormalizedRequest, param interface{}) (interface{}, bool) {
	switch v := param.(type) {
	case string:
		// If already hex, normalize and return if changed
		if strings.HasPrefix(v, "0x") {
			if normalized, err := common.NormalizeHex(v); err == nil && normalized != v {
				return normalized, true
			}
			return v, false
		}
		// Translate well-known tags to concrete numbers
		if hx, ok := resolveBlockTagToHex(ctx, nrq, v); ok {
			return hx, true
		}
		return v, false
	case float64:
		str := fmt.Sprintf("%f", v)
		if normalized, err := common.NormalizeHex(str); err == nil {
			return normalized, true
		}
		return v, false
	case map[string]interface{}:
		// Copy-on-write only when needed
		updated := false
		newMap := v
		// blockNumber inside objects (e.g., trace/debug call specs)
		if bn, ok := v["blockNumber"]; ok {
			switch bsv := bn.(type) {
			case string:
				if strings.HasPrefix(bsv, "0x") {
					if normalized, err := common.NormalizeHex(bsv); err == nil && normalized != bsv {
						if !updated {
							newMap = make(map[string]interface{}, len(v))
							for k, val := range v {
								newMap[k] = val
							}
						}
						newMap["blockNumber"] = normalized
						updated = true
					}
				} else {
					if hx, ok := resolveBlockTagToHex(ctx, nrq, bsv); ok {
						if !updated {
							newMap = make(map[string]interface{}, len(v))
							for k, val := range v {
								newMap[k] = val
							}
						}
						newMap["blockNumber"] = hx
						updated = true
					}
				}
			}
		}
		if updated {
			return newMap, true
		}
		return v, false
	default:
		return v, false
	}
}

func NormalizeHttpJsonRpc(ctx context.Context, nrq *common.NormalizedRequest, jrq *common.JsonRpcRequest) {
	// First pass: check if we need to modify anything (with read lock)
	jrq.RLock()
	var needsUpdate bool
	var newParams []interface{}

	switch jrq.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber",
		"trace_block",
		"debug_traceBlockByNumber",
		"trace_replayBlockTransactions",
		"debug_storageRangeAt",
		"debug_getRawBlock",
		"debug_getRawHeader",
		"debug_getRawReceipts",
		"erigon_getHeaderByNumber",
		"arbtrace_block",
		"arbtrace_replayBlockTransactions":
		if len(jrq.Params) > 0 {
			if normalized, changed := normalizeOrTranslateParam(ctx, nrq, jrq.Params[0]); changed {
				needsUpdate = true
				newParams = replaceParam(jrq.Params, 0, normalized)
			}
		}

	case "eth_getBalance",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas",
		"eth_feeHistory",
		"eth_getAccount",
		"debug_traceCall",
		"eth_simulateV1",
		"erigon_getBlockByTimestamp",
		"arbtrace_callMany":
		if len(jrq.Params) > 1 {
			if normalized, changed := normalizeOrTranslateParam(ctx, nrq, jrq.Params[1]); changed {
				needsUpdate = true
				newParams = replaceParam(jrq.Params, 1, normalized)
			}
		}

	case "eth_getStorageAt":
		if len(jrq.Params) > 2 {
			if normalized, changed := normalizeOrTranslateParam(ctx, nrq, jrq.Params[2]); changed {
				needsUpdate = true
				newParams = replaceParam(jrq.Params, 2, normalized)
			}
		}

	case "eth_getBlockReceipts":
		if len(jrq.Params) > 0 {
			if paramsMap, ok := jrq.Params[0].(map[string]interface{}); ok {
				// Copy-on-write only when a change is required
				updated := false
				newMap := paramsMap
				if bn, ok := paramsMap["blockNumber"]; ok {
					if hx, changed := normalizeOrTranslateParam(ctx, nrq, bn); changed {
						newMap = make(map[string]interface{}, len(paramsMap))
						for k, v := range paramsMap {
							newMap[k] = v
						}
						newMap["blockNumber"] = hx
						updated = true
					}
				}
				if updated {
					needsUpdate = true
					newParams = replaceParam(jrq.Params, 0, newMap)
				}
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
					switch v := fromBlock.(type) {
					case string:
						if nv, changed := normalizeOrTranslateParam(ctx, nrq, v); changed {
							if s, ok := nv.(string); ok {
								normalizedFrom = s
								fromNormalized = true
							}
						}
					}
				}

				if hasToBlock {
					switch v := toBlock.(type) {
					case string:
						if nv, changed := normalizeOrTranslateParam(ctx, nrq, v); changed {
							if s, ok := nv.(string); ok {
								normalizedTo = s
								toNormalized = true
							}
						}
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
