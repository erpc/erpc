package evm

import (
	"context"
	"strings"

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
	// First pass: collect parameter values and their paths while holding read lock
	type paramRef struct {
		path  []interface{}
		value interface{}
	}
	var paramsToProcess []paramRef
	var method string

	// Hold read lock only for quick data extraction
	jrq.RLock()
	method = jrq.Method
	methodCfg := getMethodConfig(method, nrq)
	if methodCfg != nil && len(methodCfg.ReqRefs) > 0 {
		for _, ref := range methodCfg.ReqRefs {
			val, err := jrq.PeekByPath(ref...)
			if err != nil {
				continue
			}
			// Store path and value for processing outside the lock
			// Note: val itself could be a reference type, but PeekByPath returns
			// the actual value from the params, not a copy. Since we only read
			// the value to determine if translation is needed, this is safe.
			paramsToProcess = append(paramsToProcess, paramRef{
				path:  ref,
				value: val,
			})
		}
	}
	// Deep copy params for working if we have refs to process
	// This prevents race conditions after we release the lock
	var workingParams []interface{}
	if len(paramsToProcess) > 0 {
		workingParams = deepCopyParams(jrq.Params)
	}
	jrq.RUnlock()

	// If no parameters to process, return early
	if len(paramsToProcess) == 0 {
		return
	}

	// Decide if we will interpolate any tags for this request and warm up original block ref if needed
	translateLatest := methodCfg.TranslateLatestTag == nil || *methodCfg.TranslateLatestTag
	translateFinalized := methodCfg.TranslateFinalizedTag == nil || *methodCfg.TranslateFinalizedTag
	willInterpolate := false
	for _, p := range paramsToProcess {
		if s, ok := p.value.(string); ok {
			if (s == "latest" && translateLatest) || (s == "finalized" && translateFinalized) {
				willInterpolate = true
				break
			}
		}
	}
	if willInterpolate && nrq.EvmBlockRef() == nil {
		// Preserve the original tag before we translate it to a hex number
		ExtractBlockReferenceFromRequest(ctx, nrq)
	}

	// If this request opted out of param interpolation, don't mutate params.
	// We still warmed the original block reference above to preserve semantics.
	if nrq != nil && nrq.Directives() != nil && nrq.Directives().SkipInterpolation {
		return
	}

	// Process parameters outside the lock (expensive operations)
	var needsUpdate bool

	for _, param := range paramsToProcess {
		var newVal interface{}
		changed := false

		switch v := param.value.(type) {
		case string:
			// Handle string values (hex or block tags)
			if strings.HasPrefix(v, "0x") {
				// Normalize hex string
				if normalized, err := common.NormalizeHex(v); err == nil && normalized != v {
					newVal = normalized
					changed = true
				}
			} else {
				// Check if it's a block tag that should be translated
				// This is the expensive operation that calls network methods
				switch v {
				case "latest":
					if translateLatest {
						// Preserve original tag semantics across multiple block params:
						// - If no ref yet, set to current tag
						// - If a different tag was already set, collapse to "*"
						if cur := nrq.EvmBlockRef(); cur == nil {
							nrq.SetEvmBlockRef("latest")
						} else if s, ok := cur.(string); ok && s != "latest" && s != "*" {
							nrq.SetEvmBlockRef("*")
						}
						if hx, ok := resolveBlockTagToHex(ctx, nrq, v); ok {
							newVal = hx
							changed = true
						}
					}
				case "finalized":
					if translateFinalized {
						// Preserve original tag semantics across multiple block params:
						// - If no ref yet, set to current tag
						// - If a different tag was already set, collapse to "*"
						if cur := nrq.EvmBlockRef(); cur == nil {
							nrq.SetEvmBlockRef("finalized")
						} else if s, ok := cur.(string); ok && s != "finalized" && s != "*" {
							nrq.SetEvmBlockRef("*")
						}
						if hx, ok := resolveBlockTagToHex(ctx, nrq, v); ok {
							newVal = hx
							changed = true
						}
					}
					// "safe" and "pending" are passed through unchanged
					// We don't have the necessary state information to accurately translate them
				}
			}
		case float64:
			// Handle numeric values from JSON (converts to hex)
			// JSON unmarshaling produces float64 for all numbers
			if normalized, err := common.NormalizeHex(int64(v)); err == nil {
				newVal = normalized
				changed = true
			}
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			// Handle other numeric types (shouldn't normally occur from JSON, but handle for completeness)
			if normalized, err := common.NormalizeHex(v); err == nil {
				newVal = normalized
				changed = true
			}
		default:
			// Skip unsupported types
			continue
		}

		if changed {
			// Apply changes to working copy
			if !needsUpdate {
				needsUpdate = true
			}
			if np, ok := replaceParamAtPath(workingParams, param.path, newVal); ok {
				workingParams = np
			}
		}
	}

	// Only acquire write lock if we need to update
	if needsUpdate {
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
