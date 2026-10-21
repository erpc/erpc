package evm

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// projectPreForward_net_listening answers net_listening from eRPC itself: the
// node clients reach is eRPC, which is listening while it answers. Many
// upstreams reject the method, so routing it made one vendor a single point of
// failure for a constant.
func projectPreForward_net_listening(_ context.Context, _ common.Network, r *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	jrqID := r.ID()
	if jrqID == nil {
		jrqID = util.RandomID()
	}
	jrr, err := common.NewJsonRpcResponse(jrqID, true, nil)
	if err != nil {
		return true, nil, err
	}
	return true, common.NewNormalizedResponse().WithRequest(r).WithJsonRpcResponse(jrr), nil
}
