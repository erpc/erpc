package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

// HandleNetworkPreForward checks if the request matches a known EVM method customization on network level,
// and returns a custom response if it applies. If it returns (false, nil, nil),
// then it's not a method we handle here. If it returns (true, resp, err),
// that means we've handled it. If any error is returned, it means handling failed.
func HandleNetworkPreForward(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PreForwardHook")
	defer span.End()

	method, err := nq.Method()
	if err != nil {
		return false, nil, err
	}

	switch method {
	case "eth_blockNumber":
		return networkPreForward_eth_blockNumber(ctx, network, nq)
	case "eth_call":
		return networkPreForward_eth_call(ctx, network, nq)
	default:
		return false, nil, nil
	}
}

// HandleNetworkPostForward checks if the request matches a known EVM method customization on network level,
// and returns a custom response if it applies. Otherwise returns the response/error as is.
func HandleNetworkPostForward(ctx context.Context, network common.Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PostForwardHook")
	defer span.End()

	method, err := nq.Method()
	if err != nil {
		return nr, err
	}

	switch method {
	case "eth_getBlockByNumber":
		return networkPostForward_eth_getBlockByNumber(ctx, network, nq, nr, re)
	default:
		return nr, re
	}
}

func HandleUpstreamPreForward(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest, skipCacheRead bool) (handled bool, resp *common.NormalizedResponse, err error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PreForwardHook")
	defer span.End()

	method, err := r.Method()
	if err != nil {
		return false, nil, err
	}

	switch method {
	case "eth_getLogs":
		return upstreamPreForward_eth_getLogs(ctx, n, u, r)
	default:
		return false, nil, nil
	}
}

func HandleUpstreamPostForward(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PostForwardHook")
	defer span.End()

	method, err := rq.Method()
	if err != nil {
		return rs, err
	}

	switch method {
	case "eth_getLogs":
		return upstreamPostForward_eth_getLogs(ctx, n, u, rq, rs, re, skipCacheRead)
	}

	return rs, re
}
