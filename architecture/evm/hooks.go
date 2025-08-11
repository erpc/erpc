package evm

import (
	"context"
	"strings"

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

	switch strings.ToLower(method) {
	case "eth_blocknumber":
		return networkPreForward_eth_blockNumber(ctx, network, nq)
	case "eth_call":
		return networkPreForward_eth_call(ctx, network, nq)
	case "eth_chainid":
		return networkPreForward_eth_chainId(ctx, network, nq)
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
		return upstreamPostForward_eth_getLogs(ctx, n, u, rq, rs, re, skipCacheRead)
	case "eth_getblockbynumber":
		return upstreamPostForward_pointLookupMissingData(ctx, n, u, rq, rs, re, "block", "number")
	case "eth_getblockbyhash":
		return upstreamPostForward_pointLookupMissingData(ctx, n, u, rq, rs, re, "block", "hash")
	}

	return rs, re
}

// upstreamPostForward_pointLookupMissingData classifies empty point-lookups (like getBlockByNumber/hash)
// as missing-data so that network-level retry can rotate to other upstreams.
func upstreamPostForward_pointLookupMissingData(
	ctx context.Context,
	n common.Network,
	u common.Upstream,
	rq *common.NormalizedRequest,
	rs *common.NormalizedResponse,
	re error,
	entity string, // e.g. "block"
	refKind string, // e.g. "number" or "hash"
) (*common.NormalizedResponse, error) {
	if re != nil || rs == nil || rs.IsObjectNull() || !rs.IsResultEmptyish() {
		return rs, re
	}

	// Build a method-specific message
	rqj, _ := rq.JsonRpcRequest(ctx)
	var ref string
	if rqj != nil && len(rqj.Params) > 0 {
		if s, ok := rqj.Params[0].(string); ok {
			ref = s
		}
	}
	msg := entity + " not found"
	if ref != "" {
		msg = entity + " not found with " + refKind + " " + ref
	}

	return rs, common.NewErrEndpointMissingData(
		common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorMissingData,
			msg,
			nil,
			map[string]interface{}{entity + refKind: ref},
		),
		u,
	)
}
