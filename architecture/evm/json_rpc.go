package evm

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

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
	case "latest", "pending":
		// Both "latest" and "pending" resolve to the highest known latest block
		// since we don't have pending transaction pool visibility
		if bn := network.EvmHighestLatestBlockNumber(ctx); bn > 0 {
			if hx, err := common.NormalizeHex(bn); err == nil {
				return hx, true
			}
		}
	case "finalized", "safe":
		// Both "finalized" and "safe" resolve to the highest known finalized block
		// "safe" is typically between finalized and latest, but we use finalized as conservative choice
		if bn := network.EvmHighestFinalizedBlockNumber(ctx); bn > 0 {
			if hx, err := common.NormalizeHex(bn); err == nil {
				return hx, true
			}
		}
	}
	return "", false
}

func NormalizeHttpJsonRpc(ctx context.Context, nrq *common.NormalizedRequest, jrq *common.JsonRpcRequest) {
	// Generic, config-driven pass: iterate method ReqRefs and translate/normalize values in-place.
	jrq.RLock()
	var needsUpdate bool
	var workingParams []interface{}

	// Get method config (network overrides or defaults)
	methodCfg := getMethodConfig(jrq.Method, nrq)
	if methodCfg != nil && len(methodCfg.ReqRefs) > 0 {
		translateLatest := methodCfg.TranslateLatestTag == nil || *methodCfg.TranslateLatestTag
		translateFinalized := methodCfg.TranslateFinalizedTag == nil || *methodCfg.TranslateFinalizedTag

		for _, ref := range methodCfg.ReqRefs {
			val, err := jrq.PeekByPath(ref...)
			if err != nil {
				continue
			}

			var newVal interface{}
			changed := false

			switch v := val.(type) {
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
					switch v {
					case "latest", "pending":
						if translateLatest {
							if hx, ok := resolveBlockTagToHex(ctx, nrq, v); ok {
								newVal = hx
								changed = true
							}
						}
					case "finalized", "safe":
						if translateFinalized {
							if hx, ok := resolveBlockTagToHex(ctx, nrq, v); ok {
								newVal = hx
								changed = true
							}
						}
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
				// Initialize working params on first change
				if !needsUpdate {
					workingParams = jrq.Params
					needsUpdate = true
				}
				if np, ok := replaceParamAtPath(workingParams, ref, newVal); ok {
					workingParams = np
				}
			}
		}
	}
	jrq.RUnlock()

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
