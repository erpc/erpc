package evm

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
)

// pinLatestParams returns a copy of a family-3 request's params whose block
// selector, when it means latest (latestStateSelector), is hex(height),
// rewritten as interpolation rewrites it: the tag, an EIP-1898 blockNumber
// leaf, or an absent selector. Otherwise it returns params unchanged and
// false. Methods outside family 3 are never pinned: for eth_getBlockByNumber
// and friends, pinning the tag would change the QUESTION, not just the
// evaluation height.
func pinLatestParams(method string, params []interface{}, height int64) ([]interface{}, bool) {
	path, ok := latestStateSelector(method, params)
	if !ok {
		return params, false
	}
	return replaceParamAtPath(params, path, fmt.Sprintf("0x%x", height))
}

// PinBlockTagInBody pins the "latest" block selector of a family-3 request
// body to height (pinLatestParams). Returns the rewritten body and whether a
// rewrite happened; any parse trouble or a selector that does not mean
// latest returns the body unchanged (fail-open, like every other shadow-path
// guard).
func PinBlockTagInBody(body []byte, height int64) ([]byte, bool) {
	if len(body) == 0 || height <= 0 {
		return body, false
	}
	var req map[string]interface{}
	if err := common.SonicCfg.Unmarshal(body, &req); err != nil {
		return body, false
	}
	method, _ := req["method"].(string)
	params, _ := req["params"].([]interface{})
	pinned, ok := pinLatestParams(method, params, height)
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

// pinsLatestTag reports whether u sets evm.pinLatestTag: it is sent the
// request's Head.Latest for a family-3 "latest", never the tag.
func pinsLatestTag(u common.Upstream) bool {
	cfg := u.Config()
	return cfg != nil && cfg.Evm != nil && cfg.Evm.PinLatestTag != nil && *cfg.Evm.PinLatestTag
}

// PinLatestTag resolves what upstream u must receive for r when u sets
// evm.pinLatestTag and r asks a family-3 method for latest: r's params with
// the block selector set to height, r's Head.Latest, the number every attempt
// and hedge of r uses (spec §7). pinned is false when u does not pin r.
// pinned with height 0 means the head is unknown, and u must not be sent
// "latest" at all.
func PinLatestTag(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest) (params []interface{}, height int64, pinned bool) {
	if !pinsLatestTag(u) {
		return nil, 0, false
	}
	jrq, err := r.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return nil, 0, false
	}
	jrq.RLock()
	_, ok := latestStateSelector(jrq.Method, jrq.Params)
	jrq.RUnlock()
	if !ok {
		return nil, 0, false
	}
	height = common.EvmHeadOf(n, ctx, r).Latest
	if height <= 0 {
		return nil, 0, true
	}
	jrq.RLock()
	params, ok = pinLatestParams(jrq.Method, jrq.Params, height)
	jrq.RUnlock()
	if !ok {
		return nil, 0, false
	}
	return params, height, true
}
