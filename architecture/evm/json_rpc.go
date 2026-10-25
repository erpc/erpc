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

// TagBlockNumber is the block number tag interpolates to for nrq (spec §7):
// the request Head's Latest for "latest", its Finalized for "finalized". 0
// when the tag reaches upstreams as is: another tag, the method's
// translateLatestTag/translateFinalizedTag is off, the request skips
// interpolation, or the Head is unknown. "safe" and "pending" are never
// translated: eRPC tracks neither state.
func TagBlockNumber(ctx context.Context, nrq *common.NormalizedRequest, tag string) int64 {
	method, _ := nrq.Method()
	return tagBlockNumber(ctx, nrq, getMethodConfig(method, nrq.Network()), tag)
}

func tagBlockNumber(ctx context.Context, nrq *common.NormalizedRequest, cfg *common.CacheMethodConfig, tag string) int64 {
	network := nrq.Network()
	if network == nil || cfg == nil {
		return 0
	}
	if d := nrq.Directives(); d != nil && d.SkipInterpolation {
		return 0
	}
	switch tag {
	case "latest":
		if keepsLatestTag(cfg) {
			return 0
		}
		return common.EvmHeadOf(network, ctx, nrq).Latest
	case "finalized":
		if cfg.TranslateFinalizedTag != nil && !*cfg.TranslateFinalizedTag {
			return 0
		}
		return common.EvmHeadOf(network, ctx, nrq).Finalized
	}
	return 0
}

// keepsLatestTag reports whether a method's config sends "latest" to
// upstreams as the tag: translateLatestTag off.
func keepsLatestTag(cfg *common.CacheMethodConfig) bool {
	return cfg != nil && cfg.TranslateLatestTag != nil && !*cfg.TranslateLatestTag
}

// stateBlockParam is the block selector's position for the §7 state-at-a-block
// methods whose nodes read an absent or null selector as "latest" (geth's
// blockNrOrHashOrLatest, erigon's trace_call): absent, it is interpolated
// like "latest". eth_callMany is not here: its selector is a required
// object, whose blockNumber leaf is rewritten like any EIP-1898 one (see
// stateSelectorIndex). Nor are arbtrace_*: Arbitrum classic nodes serve
// them, and their latest is not the network's, so an absent param stays
// absent.
var stateBlockParam = map[string]int{
	"eth_call":                1,
	"eth_estimateGas":         1,
	"eth_getBalance":          1,
	"eth_getCode":             1,
	"eth_getStorageAt":        2,
	"eth_getTransactionCount": 1,
	"eth_getProof":            2,
	"eth_getAccount":          1,
	"eth_createAccessList":    1,
	"eth_simulateV1":          1,
	"debug_traceCall":         1,
	"trace_call":              2,
}

// stateBlockParamAbsent reports whether params end right before idx or hold
// null there.
func stateBlockParamAbsent(params []interface{}, idx int) bool {
	return len(params) == idx || (len(params) > idx && params[idx] == nil)
}

// stateSelectorIndex is the block selector's position of a §7 family-3
// (state at a block) method: stateBlockParam's, or eth_callMany's state
// context object: the one family-3 map pinLatestTag, the shadow pin, the
// tag-state gate and the state prober read.
func stateSelectorIndex(method string) (int, bool) {
	if idx, ok := stateBlockParam[method]; ok {
		return idx, true
	}
	if method == "eth_callMany" {
		return 1, true
	}
	return 0, false
}

// latestStateSelector is the path in params of a family-3 request's block
// selector when it means "latest", in every form interpolation rewrites: the
// tag, an EIP-1898 object's blockNumber leaf, or an absent or null selector
// of a stateBlockParam method. false for any other method or selector.
func latestStateSelector(method string, params []interface{}) ([]interface{}, bool) {
	idx, ok := stateSelectorIndex(method)
	if !ok {
		return nil, false
	}
	if _, absentIsLatest := stateBlockParam[method]; absentIsLatest && stateBlockParamAbsent(params, idx) {
		return []interface{}{idx}, true
	}
	if len(params) <= idx {
		return nil, false
	}
	leaf, ok := selectsLatest(params[idx])
	switch {
	case !ok:
		return nil, false
	case leaf:
		return []interface{}{idx, "blockNumber"}, true
	}
	return []interface{}{idx}, true
}

// selectsLatest reports whether a present block selector means "latest":
// the tag, or an EIP-1898 object whose blockNumber leaf is the tag (leaf).
func selectsLatest(v interface{}) (leaf, ok bool) {
	switch v := v.(type) {
	case string:
		return false, v == "latest"
	case map[string]interface{}:
		s, _ := v["blockNumber"].(string)
		return true, s == "latest" && v["blockHash"] == nil
	}
	return false, false
}

// rangeBoundAbsent reports whether a range filter's bound key (fromBlock,
// toBlock) is absent or null with no blockHash: nodes read it as "latest"
// (spec §7 family 4).
func rangeBoundAbsent(filter map[string]interface{}, key string) bool {
	if filter["blockHash"] != nil {
		return false
	}
	v, ok := filter[key]
	return !ok || v == nil
}

// rangeBound is a range filter's bound key as the range checks read it:
// "latest" when absent (rangeBoundAbsent), else its string value.
func rangeBound(filter map[string]interface{}, key string) string {
	if rangeBoundAbsent(filter, key) {
		return "latest"
	}
	s, _ := filter[key].(string)
	return s
}

// rangeSentAsTag reports whether either bound of an interpolated range
// filter still reaches upstreams as a tag: absent, or "latest"/"finalized"
// left as the word (translate off, skip-interpolation). Such a range is
// never split proactively: its sub-requests would carry numbers (spec §7).
func rangeSentAsTag(filter map[string]interface{}) bool {
	for _, key := range [...]string{"fromBlock", "toBlock"} {
		if rangeBoundAbsent(filter, key) {
			return true
		}
		if s, _ := filter[key].(string); s == "latest" || s == "finalized" {
			return true
		}
	}
	return false
}

// absentRangeFilter is the filter of a range method whose absent bounds
// mean "latest" (spec §7 family 4; not arbtrace_filter, see
// stateBlockParam), else nil.
func absentRangeFilter(method string, params []interface{}) map[string]interface{} {
	if method != "eth_getLogs" && method != "trace_filter" || len(params) == 0 {
		return nil
	}
	filter, _ := params[0].(map[string]interface{})
	return filter
}

// hasAbsentSelector reports whether params hold a selector that is absent
// and means "latest" for method, without allocating: absentLatestPaths'
// pre-check before any ReqRef is read.
func hasAbsentSelector(method string, params []interface{}) bool {
	if idx, ok := stateBlockParam[method]; ok {
		return stateBlockParamAbsent(params, idx)
	}
	filter := absentRangeFilter(method, params)
	return filter != nil && (rangeBoundAbsent(filter, "fromBlock") || rangeBoundAbsent(filter, "toBlock"))
}

// absentLatestPaths are the block selectors of params that are absent and
// mean "latest" for method; selectorSeen: a ReqRef already holds one.
func absentLatestPaths(method string, params []interface{}, selectorSeen bool) [][]interface{} {
	if idx, ok := stateBlockParam[method]; ok {
		if !selectorSeen && stateBlockParamAbsent(params, idx) {
			return [][]interface{}{{idx}}
		}
		return nil
	}
	filter := absentRangeFilter(method, params)
	if filter == nil {
		return nil
	}
	var paths [][]interface{}
	for _, key := range [...]string{"fromBlock", "toBlock"} {
		if rangeBoundAbsent(filter, key) {
			paths = append(paths, []interface{}{0, key})
		}
	}
	return paths
}

// NormalizeHttpJsonRpc normalizes and translates block parameters in JSON-RPC requests.
// It converts supported block tags (latest, finalized) to concrete hex block numbers
// and normalizes hex values, in every form (spec §7): a plain tag, an EIP-1898
// object's blockNumber leaf, and an absent selector that means "latest"
// (absentLatestPaths). Tags like "safe" and "pending" are passed through unchanged.
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
		anyAbsent       bool
	)

	jrq.RLock()
	method = jrq.Method
	reqId = jrq.ID
	network := nrq.Network()
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
	anyAbsent = hasAbsentSelector(method, jrq.Params)
	jrq.RUnlock()

	if len(paramsToProcess) == 0 && !anyAbsent {
		return
	}

	skipInterpolation := nrq.Directives() != nil && nrq.Directives().SkipInterpolation

	// Aggregators
	var (
		needsUpdate   bool
		seenLatest    bool
		seenFinalized bool
		selectorSeen  bool
	)

	// Helper: cache numeric block number when safe.
	// The cached value is metadata used by cache lookups, gRPC routing, tracing, and
	// the network-level block-availability check. It does not by itself drive routing
	// decisions — that is gated independently by EnforceBlockAvailability at the
	// consumer call sites. Always caching when we can extract a number keeps the
	// metadata complete regardless of per-method enforcement defaults.
	cacheBlockNumber := func(n int64) {
		// Ordering of ReqRefs should ensure higher bounds (e.g., toBlock) appear later,
		// so the final cached value represents the upper bound when ranges are present.
		if n > 0 {
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
		// An EIP-1898 object selects its block by its blockNumber leaf.
		if m, ok := p.value.(map[string]interface{}); ok && len(p.path) == 1 && m["blockHash"] == nil {
			if leaf, ok := m["blockNumber"]; ok {
				p.path = []interface{}{p.path[0], "blockNumber"}
				p.value = leaf
			}
		}
		// Avoid replacing entire object params; prefer leaf updates when refs include nested keys.
		isTopLevelParamOfMap := false
		if len(p.path) == 1 {
			if _, ok := p.path[0].(int); ok {
				if _, ok := p.value.(map[string]interface{}); ok {
					isTopLevelParamOfMap = true
				}
			}
		}

		// Use composite parser to distinguish block numbers, tags, and hashes.
		blockRef, blockNum, err := parseCompositeBlockParam(p.value)
		if err == nil && (blockRef != "" || blockNum > 0) {
			selectorSeen = true
		}
		if err == nil {
			// Cache numeric block number (for single-ref methods)
			if blockNum > 0 {
				cacheBlockNumber(blockNum)
				// Normalize numeric representations to canonical hex
				if normalized, nerr := common.NormalizeHex(blockNum); nerr == nil {
					// Only mark as changed when representation actually differs
					if sv, ok := p.value.(string); ok {
						if normalized != sv {
							// Only replace when not a top-level object param
							if !isTopLevelParamOfMap {
								newVal = normalized
								changed = true
							}
						}
					} else {
						// Only replace when not a top-level object param
						if !isTopLevelParamOfMap {
							newVal = normalized
							changed = true
						}
					}
				}
			} else if blockRef != "" {
				// Handle tags; do not mutate when blockRef is a hash.
				switch blockRef {
				case "latest", "finalized":
					if blockRef == "latest" {
						seenLatest = true
					} else {
						seenFinalized = true
					}
					n := tagBlockNumber(ctx, nrq, methodCfg, blockRef)
					if hx, herr := common.NormalizeHex(n); n > 0 && herr == nil {
						// Only replace when not a top-level object param
						if !isTopLevelParamOfMap {
							newVal = hx
							changed = true
						}
						cacheBlockNumber(n)
						if network.Logger() != nil {
							network.Logger().Debug().
								Str("component", "evm").
								Str("method", method).
								Interface("requestId", reqId).
								Str("tag", blockRef).
								Str("resolvedHex", hx).
								Int64("resolvedNumber", n).
								Interface("path", p.path).
								Str("networkId", network.Id()).
								Str("networkLabel", network.Label()).
								Msg("interpolated block tag to concrete block number")
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

	var absent [][]interface{}
	if anyAbsent {
		jrq.RLock()
		absent = absentLatestPaths(method, jrq.Params, selectorSeen)
		jrq.RUnlock()
	}
	// An absent selector is "latest" spelled out: interpolated when the tag
	// is, else sent as the word, so no node applies its own default block (a
	// vendor answered a bare eth_estimateGas from state far behind its head).
	for _, path := range absent {
		seenLatest = true
		var val interface{} = "latest"
		n := tagBlockNumber(ctx, nrq, methodCfg, "latest")
		if hx, herr := common.NormalizeHex(n); n > 0 && herr == nil {
			val = hx
			cacheBlockNumber(n)
		}
		changes = append(changes, change{path: path, newVal: val})
		needsUpdate = true
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
// Supports param arrays (top-level) and map[string]interface{} nesting. A missing
// leaf is created (an absent selector, absentLatestPaths): a key of an existing
// map, or the element right after an array's last; containers never are.
func setByPath(current interface{}, path []interface{}, newValue interface{}) (interface{}, bool) {
	if len(path) == 0 {
		return newValue, true
	}
	switch key := path[0].(type) {
	case int:
		arr, ok := current.([]interface{})
		if !ok || key < 0 || key > len(arr) || (key == len(arr) && len(path) > 1) {
			return current, false
		}
		var child interface{}
		if key < len(arr) {
			child = arr[key]
		}
		updatedChild, changed := setByPath(child, path[1:], newValue)
		if !changed {
			return current, false
		}
		// Clone slice and set updated child
		newArr := make([]interface{}, max(len(arr), key+1))
		copy(newArr, arr)
		newArr[key] = updatedChild
		return newArr, true
	case string:
		m, ok := current.(map[string]interface{})
		if !ok {
			return current, false
		}
		child, exists := m[key]
		if !exists && len(path) > 1 {
			// Never create intermediate structure
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
