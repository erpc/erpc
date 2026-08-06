package evm

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

var specialSafeBlockRefs = map[string][][]interface{}{
	"eth_createaccesslist": {{1}},
	"eth_newfilter":        {{0, "fromBlock"}, {0, "toBlock"}},
}

// ApplySafeBlockSource routes requests carrying the `safe` block tag to the
// upstreams selected by evm.safeBlockSource. The tag stays verbatim so the
// selected node remains the authority for both block height and hash.
//
// Operator routing overrides a client use-upstream directive, and cache reads
// are skipped: a cached response may have been produced before the source was
// configured (or by a previous source) and cannot prove the current source
// still considers that block safe.
func ApplySafeBlockSource(ctx context.Context, network common.Network, req *common.NormalizedRequest) error {
	if network == nil || req == nil {
		return nil
	}
	cfg := network.Config()
	if cfg == nil || cfg.Evm == nil || cfg.Evm.SafeBlockSource == "" {
		return nil
	}

	method, err := req.Method()
	if err != nil {
		return err
	}
	methodCfg := getMethodConfig(method, network)
	var refs [][]interface{}
	if methodCfg != nil {
		refs = methodCfg.ReqRefs
	}
	if len(refs) == 0 {
		refs = specialSafeBlockRefs[strings.ToLower(method)]
	}
	if len(refs) == 0 {
		return nil
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return err
	}

	jrq.RLock()
	hasSafe := false
	for _, ref := range refs {
		value, err := jrq.PeekByPath(ref...)
		if err != nil {
			continue
		}
		blockRef, _, err := parseCompositeBlockParam(value)
		if err == nil && strings.EqualFold(blockRef, "safe") {
			hasSafe = true
			break
		}
	}
	jrq.RUnlock()
	if !hasSafe {
		return nil
	}

	directives := req.Directives().Clone()
	directives.UseUpstream = cfg.Evm.SafeBlockSource
	directives.SkipCacheRead = "true"
	req.SetDirectives(directives)
	return nil
}
