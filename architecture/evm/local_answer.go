package evm

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// projectPreForward_localAnswer answers a method eRPC can answer itself (see
// common.LocalAnswer) before cache and upstream selection. A request that
// skips the cache read asks for an upstream's answer, so it is routed.
func projectPreForward_localAnswer(ctx context.Context, n common.Network, r *common.NormalizedRequest, method string) (handled bool, resp *common.NormalizedResponse, err error) {
	if r.ShouldSkipCacheRead("") {
		return false, nil, nil
	}
	result, ok := common.LocalAnswer(ctx, n, method)
	if !ok {
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
