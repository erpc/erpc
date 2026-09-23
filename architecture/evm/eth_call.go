package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// projectPreForward_defaultLatestBlock pins a call simulation sent without a
// block parameter to "latest". Forwarded bare, each node applies its own
// default block, so the answer depends on which upstream serves it.
func projectPreForward_defaultLatestBlock(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	jrq, err := nq.JsonRpcRequest()
	if err != nil {
		return false, nil, nil
	}

	jrq.RLock()
	if len(jrq.Params) != 1 {
		jrq.RUnlock()
		return false, nil, nil
	}
	jrq.RUnlock()

	jrq.Lock()
	jrq.Params = []interface{}{
		jrq.Params[0],
		"latest",
	}
	jrq.Unlock()

	resp, err := network.Forward(ctx, nq)
	return true, resp, err
}
