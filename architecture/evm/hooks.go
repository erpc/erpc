package evm

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

// HandleProjectPreForward is the early pre-forward hook executed at project layer
// before cache and before upstream selection. Use this for transformations that
// affect cache hash or short-circuit results without upstream context.
func HandleProjectPreForward(ctx context.Context, network common.Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	ctx, span := common.StartDetailSpan(ctx, "Project.PreForwardHook")
	defer span.End()

	method, err := nq.Method()
	if err != nil {
		return false, nil, err
	}

	switch strings.ToLower(method) {
	case "eth_blocknumber":
		return projectPreForward_eth_blockNumber(ctx, network, nq)
	case "eth_call":
		return projectPreForward_eth_call(ctx, network, nq)
	case "eth_chainid":
		return projectPreForward_eth_chainId(ctx, network, nq)
	case "eth_getlogs":
		return projectPreForward_eth_getLogs(ctx, network, nq)
	default:
		return false, nil, nil
	}
}

// HandleNetworkPreForward is executed after upstream selection for upstream-aware logic.
// Pass the selected upstreams to allow computing effective thresholds, availability, etc.
func HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	ctx, span := common.StartDetailSpan(ctx, "Network.PreForwardHook")
	defer span.End()

	method, err := nq.Method()
	if err != nil {
		return false, nil, err
	}

	switch strings.ToLower(method) {
	case "eth_getlogs":
		return networkPreForward_eth_getLogs(ctx, network, upstreams, nq)
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

	switch strings.ToLower(method) {
	case "eth_getblockbynumber":
		return networkPostForward_eth_getBlockByNumber(ctx, network, nq, nr, re)
	case "eth_getlogs":
		return networkPostForward_eth_getLogs(ctx, network, nq, nr, re)
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

	switch strings.ToLower(method) {
	case "eth_getlogs":
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

	switch strings.ToLower(method) {
	case "eth_getlogs":
		return upstreamPostForward_eth_getLogs(ctx, n, u, rq, rs, re)
	case // Block lookups
		"eth_getblockbynumber",
		"eth_getblockbyhash",
		// Transaction lookups
		"eth_gettransactionbyhash",
		"eth_gettransactionreceipt",
		"eth_gettransactionbyblockhashandindex",
		"eth_gettransactionbyblocknumberandindex",
		// Uncle/ommers (legacy API)
		"eth_getunclebyblockhashandindex",
		"eth_getunclebyblocknumberandindex",
		// Traces (debug/trace/parity modules)
		"debug_tracetransaction",
		"trace_transaction",
		"trace_block",
		"trace_get":
		return upstreamPostForward_markUnexpectedEmpty(ctx, u, rq, rs, re)
	}

	return rs, re
}
