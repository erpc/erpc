package evm

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// projectPreForward_localAnswer answers a method eRPC can answer itself (see
// common.LocalAnswer) before cache and upstream selection. A request that
// skips the cache read asks for an upstream's answer, so it is routed. A
// matching network staticResponse is explicit config and wins.
//
// Bind r into ctx before answering: net_peerCount and eth_syncing walk
// EvmRoutableUpstreams, which narrows to the request's use-upstream selector
// via RequestContextKey. That key is otherwise set only inside Network.Forward,
// which this hook runs before.
func projectPreForward_localAnswer(ctx context.Context, n common.Network, r *common.NormalizedRequest, method string) (handled bool, resp *common.NormalizedResponse, err error) {
	if r.ShouldSkipCacheRead("") {
		return false, nil, nil
	}
	ctx = context.WithValue(ctx, common.RequestContextKey, r)
	result, ok := common.LocalAnswer(ctx, n, method)
	if !ok || hasStaticResponse(ctx, n, r, method) {
		return false, nil, nil
	}
	id := r.ID()
	if id == nil {
		id = util.RandomID()
	}
	jrr, err := common.NewJsonRpcResponse(id, result, nil)
	if err != nil {
		return true, nil, err
	}
	return true, common.NewNormalizedResponse().WithRequest(r).WithJsonRpcResponse(jrr), nil
}

func hasStaticResponse(ctx context.Context, n common.Network, r *common.NormalizedRequest, method string) bool {
	cfg := n.Config()
	if cfg == nil || len(cfg.StaticResponses) == 0 {
		return false
	}
	jrq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return false
	}
	jrq.RLock()
	params := jrq.Params
	jrq.RUnlock()
	return common.FindStaticResponseMatch(cfg.StaticResponses, method, params) != nil
}
