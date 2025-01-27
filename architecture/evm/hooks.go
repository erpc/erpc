package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// HandlePreForward checks if the request matches a known EVM method customization
// and returns a custom response if it applies. If it returns (false, nil, nil),
// then it's not a method we handle here. If it returns (true, resp, err),
// that means we've handled it. If any error is returned, it means handling failed.
func HandlePreForward(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	method, err := nq.Method()
	if err != nil {
		return false, nil, err
	}

	switch method {
	case "eth_blockNumber":
		return preForward_eth_blockNumber(ctx, network, nq)
	default:
		return false, nil, nil
	}
}

// HandlePostForward checks if the request matches a known EVM method customization,
// and returns a custom response if it applies. Otherwise returns the response/error as is.
func HandlePostForward(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse) (resp *common.NormalizedResponse, err error) {
	method, err := nq.Method()
	if err != nil {
		return resp, err
	}

	switch method {
	case "eth_getBlockByNumber":
		return postForward_eth_getBlockByNumber(ctx, network, nq, nr)
	default:
		return resp, err
	}
}
