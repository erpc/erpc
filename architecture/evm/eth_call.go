package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

func projectPreForward_eth_call(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
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

	// Some upstreams require the block number to be specified as a parameter.
	jrq.Lock()
	jrq.Params = []interface{}{
		jrq.Params[0],
		"latest",
	}
	jrq.Unlock()

	resp, err := network.Forward(ctx, nq)
	return true, resp, err
}
