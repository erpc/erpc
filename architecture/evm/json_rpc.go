package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// deepCopyParams creates a deep copy of a params slice to avoid race conditions.
// It recursively copies slices and maps to ensure complete isolation.
func deepCopyParams(params []interface{}) []interface{} {
	if params == nil {
		return nil
	}

	result := make([]interface{}, len(params))
	for i, param := range params {
		result[i] = deepCopyValue(param)
	}
	return result
}

// deepCopyValue recursively copies a value, handling slices and maps.
func deepCopyValue(val interface{}) interface{} {
	if val == nil {
		return nil
	}

	switch v := val.(type) {
	case map[string]interface{}:
		// Deep copy map
		newMap := make(map[string]interface{}, len(v))
		for key, value := range v {
			newMap[key] = deepCopyValue(value)
		}
		return newMap
	case []interface{}:
		// Deep copy slice
		newSlice := make([]interface{}, len(v))
		for i, item := range v {
			newSlice[i] = deepCopyValue(item)
		}
		return newSlice
	case string, bool, float64, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32:
		// Primitive types are safe to copy by value
		return v
	default:
		// For any other types (which shouldn't occur in JSON-RPC params),
		// return as-is. This could be custom types that are immutable.
		return v
	}
}

// resolveBlockTagToHex resolves well-known tags to concrete hex numbers using highest known state.
// IMPORTANT: Only translates tags we can accurately represent. "safe" and "pending" are passed
// through unchanged as we don't have the necessary state information for accurate translation.
func resolveBlockTagToHex(ctx context.Context, network common.Network, tag string) (string, bool) {
	if network == nil {
		return "", false
	}
	switch tag {
	case "latest":
		// Translate "latest" to the highest known latest block
		if bn := network.EvmHighestLatestBlockNumber(ctx); bn > 0 {
			if hx, err := common.NormalizeHex(bn); err == nil {
				return hx, true
			}
		}
	case "finalized":
		// Translate "finalized" to the highest known finalized block
		if bn := network.EvmHighestFinalizedBlockNumber(ctx); bn > 0 {
			if hx, err := common.NormalizeHex(bn); err == nil {
				return hx, true
			}
		}
		// "safe" and "pending" are intentionally NOT translated here:
		// - "safe": Represents a specific consensus state between finalized and latest that we don't track
		// - "pending": Represents state including mempool transactions, which we don't have visibility into
		// These tags should be passed through to the upstream unchanged
	}
	return "", false
}

// NormalizeHttpJsonRpc normalizes and translates block parameters in JSON-RPC requests.
// It converts supported block tags (latest, finalized) to concrete hex block numbers
// and normalizes hex values. Tags like "safe" and "pending" are passed through unchanged.
// The function minimizes lock contention by:
// 1. Briefly holding a read lock to extract parameter values
// 2. Releasing the lock before performing expensive operations (network state lookups)
// 3. Only acquiring a write lock if modifications are needed
func NormalizeHttpJsonRpc(ctx context.Context, nrq *common.NormalizedRequest, jrq *common.JsonRpcRequest) {
	// Collect parameter values/paths under read lock
	type paramRef struct {
		path  []interface{}
		value interface{}
	}
	var (
		paramsToProcess []paramRef
		method          string
		reqId           interface{}
	)

	jrq.RLock()
	method = jrq.Method
	reqId = jrq.ID
	var network common.Network
	if nrq != nil {
		network = nrq.Network()
	}
	methodCfg := getMethodConfig(method, network)
	if methodCfg == nil || len(methodCfg.ReqRefs) == 0 {
		jrq.RUnlock()
		return
	}
	for _, ref := range methodCfg.ReqRefs {
		val, err := jrq.PeekByPath(ref...)
		if err != nil {
			continue
		}
		paramsToProcess = append(paramsToProcess, paramRef{
			path:  ref,
			value: val,
		})
	}
	jrq.RUnlock()

	if len(paramsToProcess) == 0 {
		return
	}

	// Config flags
	translateLatest := methodCfg.TranslateLatestTag == nil || *methodCfg.TranslateLatestTag
	translateFinalized := methodCfg.TranslateFinalizedTag == nil || *methodCfg.TranslateFinalizedTag
	skipInterpolation := nrq != nil && nrq.Directives() != nil && nrq.Directives().SkipInterpolation

	// Aggregators
	var (
		needsUpdate   bool
		seenLatest    bool
		seenFinalized bool
	)

	// Helper: cache numeric block number when safe
	cacheBlockNumber := func(n int64) {
		// Best-effort: always cache the last seen numeric block number.
		// Ordering of ReqRefs should ensure higher bounds (e.g., toBlock) appear later,
		// so the final cached value represents the upper bound when ranges are present.
		if nrq != nil && n > 0 {
			nrq.SetEvmBlockNumber(n)
		}
	}

	// Prepare list of concrete param changes so we can avoid copying unless needed
	type change struct {
		path   []interface{}
		newVal interface{}
	}
	var changes []change

	for _, p := range paramsToProcess {
		var (
			newVal  interface{}
			changed bool
		)

		// Use composite parser to distinguish block numbers, tags, and hashes.
		blockRef, blockNum, err := parseCompositeBlockParam(p.value)
		if err == nil {
			// Cache numeric block number (for single-ref methods)
			if blockNum > 0 {
				cacheBlockNumber(blockNum)
				// Normalize numeric representations to canonical hex
				if normalized, nerr := common.NormalizeHex(blockNum); nerr == nil {
					// Only mark as changed when representation actually differs
					if sv, ok := p.value.(string); ok {
						if normalized != sv {
							newVal = normalized
							changed = true
						}
					} else {
						newVal = normalized
						changed = true
					}
				}
			} else if blockRef != "" {
				// Handle tags; do not mutate when blockRef is a hash.
				switch blockRef {
				case "latest":
					seenLatest = true
					if !skipInterpolation && translateLatest {
						if hx, ok := resolveBlockTagToHex(ctx, network, blockRef); ok {
							newVal = hx
							changed = true
							var resolvedNum int64
							if n, herr := common.HexToInt64(hx); herr == nil {
								resolvedNum = n
								cacheBlockNumber(n)
							}
							// Debug log interpolation
							if network != nil && network.Logger() != nil {
								lg := network.Logger()
								ev := lg.Debug().
									Str("component", "evm").
									Str("method", method).
									Interface("requestId", reqId).
									Str("tag", blockRef).
									Str("resolvedHex", hx).
									Int64("resolvedNumber", resolvedNum).
									Interface("path", p.path).
									Str("networkId", network.Id()).
									Str("networkLabel", network.Label()).
									Bool("translateLatest", translateLatest).
									Bool("translateFinalized", translateFinalized).
									Bool("skipInterpolation", skipInterpolation)
								ev.Msg("interpolated block tag to concrete block number")
							}
						}
					}
				case "finalized":
					seenFinalized = true
					if !skipInterpolation && translateFinalized {
						if hx, ok := resolveBlockTagToHex(ctx, network, blockRef); ok {
							newVal = hx
							changed = true
							var resolvedNum int64
							if n, herr := common.HexToInt64(hx); herr == nil {
								resolvedNum = n
								cacheBlockNumber(n)
							}
							// Debug log interpolation
							if network != nil && network.Logger() != nil {
								lg := network.Logger()
								ev := lg.Debug().
									Str("component", "evm").
									Str("method", method).
									Interface("requestId", reqId).
									Str("tag", blockRef).
									Str("resolvedHex", hx).
									Int64("resolvedNumber", resolvedNum).
									Interface("path", p.path).
									Str("networkId", network.Id()).
									Str("networkLabel", network.Label()).
									Bool("translateLatest", translateLatest).
									Bool("translateFinalized", translateFinalized).
									Bool("skipInterpolation", skipInterpolation)
								ev.Msg("interpolated block tag to concrete block number")
							}
						}
					}
				default:
					if network != nil && network.Logger() != nil {
						lg := network.Logger()
						lg.Trace().
							Str("component", "evm").
							Str("method", method).
							Interface("requestId", reqId).
							Str("blockRef", blockRef).
							Interface("path", p.path).
							Str("networkId", network.Id()).
							Str("networkLabel", network.Label()).
							Msg("passed through block tag")
					}
					// "safe", "pending", "earliest" and any other strings or a hash "0x..." are passed through unchanged.
				}
			}
		}

		if changed {
			changes = append(changes, change{path: p.path, newVal: newVal})
			needsUpdate = true
		}
	}

	// Set EvmBlockRef from observed tags: collapse only when both appear
	if seenLatest && seenFinalized {
		nrq.SetEvmBlockRef("*")
	} else if seenLatest {
		nrq.SetEvmBlockRef("latest")
	} else if seenFinalized {
		nrq.SetEvmBlockRef("finalized")
	}

	// Apply changes
	if needsUpdate && !skipInterpolation {
		// Deep copy snapshot only if we actually need to mutate
		jrq.RLock()
		workingParams := deepCopyParams(jrq.Params)
		jrq.RUnlock()
		for _, ch := range changes {
			if np, ok := replaceParamAtPath(workingParams, ch.path, ch.newVal); ok {
				workingParams = np
			}
		}
		jrq.Lock()
		jrq.Params = workingParams
		jrq.Unlock()
	}
}

// replaceParamAtPath returns a new params slice with the value at the given path replaced.
// The path format mirrors cache method refs: first element is the params index (int),
// followed by zero or more string keys into nested maps/objects.
func replaceParamAtPath(params []interface{}, path []interface{}, newValue interface{}) ([]interface{}, bool) {
	if len(path) == 0 {
		return params, false
	}
	// Build a virtual root container to reuse generic setter
	root := interface{}(params)
	updatedRoot, changed := setByPath(root, path, newValue)
	if !changed {
		return params, false
	}
	if out, ok := updatedRoot.([]interface{}); ok {
		return out, true
	}
	return params, false
}

// setByPath clones only the necessary containers along the path and sets the leaf to newValue.
// Supports param arrays (top-level) and map[string]interface{} nesting.
func setByPath(current interface{}, path []interface{}, newValue interface{}) (interface{}, bool) {
	if len(path) == 0 {
		return newValue, true
	}
	switch key := path[0].(type) {
	case int:
		arr, ok := current.([]interface{})
		if !ok || key < 0 || key >= len(arr) {
			return current, false
		}
		updatedChild, changed := setByPath(arr[key], path[1:], newValue)
		if !changed {
			return current, false
		}
		// Clone slice and set updated child
		newArr := make([]interface{}, len(arr))
		copy(newArr, arr)
		newArr[key] = updatedChild
		return newArr, true
	case string:
		m, ok := current.(map[string]interface{})
		if !ok {
			return current, false
		}
		child, exists := m[key]
		if !exists {
			// If path does not exist, do not create new structure
			return current, false
		}
		updatedChild, changed := setByPath(child, path[1:], newValue)
		if !changed {
			return current, false
		}
		// Clone map and set updated child
		newMap := make(map[string]interface{}, len(m))
		for k, v := range m {
			newMap[k] = v
		}
		newMap[key] = updatedChild
		return newMap, true
	default:
		// Unsupported path element type
		return current, false
	}
}
