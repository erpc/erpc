package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
)

// Block-selector parameter position of the state methods whose "latest" may be
// pinned to a concrete height: the shadow mirror (shadow.pinBlockTag) and
// upstreams with evm.pinLatestTag. Methods absent here are never pinned: for
// eth_getBlockByNumber and friends, pinning the tag would change the QUESTION,
// not just the evaluation height.
var latestTagParamIndex = map[string]int{
	"eth_call":                1,
	"eth_getBalance":          1,
	"eth_getCode":             1,
	"eth_getTransactionCount": 1,
	"eth_getStorageAt":        2,
}

// LatestTagParamIndex returns the block-selector position of a pinnable method.
func LatestTagParamIndex(method string) (int, bool) {
	idx, ok := latestTagParamIndex[method]
	return idx, ok
}

// selectsLatest reports whether the block selector at idx means latest: the
// string "latest", or absent because params end right before it.
func selectsLatest(params []interface{}, idx int) bool {
	if len(params) == idx {
		return true
	}
	if len(params) < idx {
		return false
	}
	tag, ok := params[idx].(string)
	return ok && tag == "latest"
}

// PinLatestParams returns a copy of params whose block selector at idx is
// hex(height), when that selector means latest (see selectsLatest). Otherwise
// it returns params unchanged and false. Elements are shared, not copied.
func PinLatestParams(params []interface{}, idx int, height int64) ([]interface{}, bool) {
	if !selectsLatest(params, idx) {
		return params, false
	}
	out := make([]interface{}, max(len(params), idx+1))
	copy(out, params)
	out[idx] = fmt.Sprintf("0x%x", height)
	return out, true
}

// PinBlockTagInBody rewrites params[idx] from "latest" to the given concrete
// height in a JSON-RPC request body. Returns the rewritten body and whether a
// rewrite happened; any parse trouble, an absent selector or a non-"latest"
// selector returns the body unchanged (fail-open, like every other
// shadow-path guard).
func PinBlockTagInBody(body []byte, idx int, height int64) ([]byte, bool) {
	if len(body) == 0 || height <= 0 {
		return body, false
	}
	var req map[string]interface{}
	if err := common.SonicCfg.Unmarshal(body, &req); err != nil {
		return body, false
	}
	params, ok := req["params"].([]interface{})
	if !ok || len(params) <= idx {
		return body, false
	}
	pinned, ok := PinLatestParams(params, idx, height)
	if !ok {
		return body, false
	}
	req["params"] = pinned
	out, err := common.SonicCfg.Marshal(req)
	if err != nil {
		return body, false
	}
	return out, true
}

// PinLatestTag resolves what upstream u must receive for r when u sets
// evm.pinLatestTag and r asks a pinnable method for latest: r's params with
// the block selector set to height, r's Head.Latest, the number every attempt
// and hedge of r uses (spec §7). pinned is false when u does not pin r.
// pinned with height 0 means the head is unknown, and u must not be sent
// "latest" at all.
func PinLatestTag(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest) (params []interface{}, height int64, pinned bool) {
	cfg := u.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.PinLatestTag == nil || !*cfg.Evm.PinLatestTag {
		return nil, 0, false
	}
	jrq, err := r.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return nil, 0, false
	}
	jrq.RLock()
	idx, ok := latestTagParamIndex[jrq.Method]
	ok = ok && selectsLatest(jrq.Params, idx)
	jrq.RUnlock()
	if !ok {
		return nil, 0, false
	}
	height = common.EvmHeadOf(n, ctx, r).Latest
	if height <= 0 {
		return nil, 0, true
	}
	jrq.RLock()
	params, ok = PinLatestParams(jrq.Params, idx, height)
	jrq.RUnlock()
	if !ok {
		return nil, 0, false
	}
	return params, height, true
}
