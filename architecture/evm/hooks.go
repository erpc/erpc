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
	case "eth_chainid":
		return networkPreForward_eth_chainId(ctx, network, upstreams, nq)
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
	case "eth_chainid":
		return upstreamPreForward_eth_chainId(ctx, n, u, r)
	case "trace_filter", "arbtrace_filter":
		return upstreamPreForward_trace_filter(ctx, n, u, r)
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

	methodLower := strings.ToLower(method)

	// Special case: eth_sendRawTransaction needs to handle errors for idempotency
	if methodLower == "eth_sendrawtransaction" {
		return upstreamPostForward_eth_sendRawTransaction(ctx, n, u, rq, rs, re)
	}

	// For all other methods, skip validation if there's already an error
	if re != nil {
		return rs, re
	}

	var validationErr error

	// Check if this method should have empty results marked as errors
	shouldMarkEmpty := isMethodInMarkEmptyList(n, methodLower)

	// Method-specific post-forward hooks with directive-based validation
	switch methodLower {
	case "eth_getlogs":
		rs, validationErr = upstreamPostForward_eth_getLogs(ctx, n, u, rq, rs, re)

	case "eth_getblockreceipts":
		// First check for unexpected empty (if enabled for this method)
		if shouldMarkEmpty {
			rs, validationErr = upstreamPostForward_markUnexpectedEmpty(ctx, u, rq, rs, re)
			if validationErr != nil {
				break
			}
		}
		// Then apply directive-based validation
		rs, validationErr = upstreamPostForward_eth_getBlockReceipts(ctx, n, u, rq, rs, re)

	case "eth_getblockbynumber", "eth_getblockbyhash":
		// First check for unexpected empty (if enabled for this method)
		if shouldMarkEmpty {
			rs, validationErr = upstreamPostForward_markUnexpectedEmpty(ctx, u, rq, rs, re)
			if validationErr != nil {
				break
			}
		}
		// Then apply directive-based validation
		rs, validationErr = upstreamPostForward_eth_getBlockByNumber(ctx, n, u, rq, rs, re)

	case "eth_gettransactionreceipt":
		rs, validationErr = upstreamPostForward_eth_getTransactionReceipt(ctx, n, u, rq, rs, re)

	default:
		// For other methods, only apply the mark empty check if configured
		if shouldMarkEmpty {
			rs, validationErr = upstreamPostForward_markUnexpectedEmpty(ctx, u, rq, rs, re)
		}
	}

	// If validation failed due to content validation error (e.g. bloom inconsistency),
	// clear the lastValidResponse so retry/consensus doesn't mistakenly use an invalid response.
	if validationErr != nil && common.HasErrorCode(validationErr, common.ErrCodeEndpointContentValidation) {
		rq.ClearLastValidResponse()
	}

	if validationErr != nil {
		return rs, validationErr
	}

	return rs, re
}

// isMethodInMarkEmptyList checks if the given method is in the network's configured
// list of methods that should have empty results marked as errors.
func isMethodInMarkEmptyList(n common.Network, methodLower string) bool {
	if n == nil || n.Config() == nil || n.Config().Evm == nil {
		return false
	}

	methods := n.Config().Evm.MarkEmptyAsErrorMethods
	if methods == nil {
		return false
	}

	for _, m := range methods {
		if strings.ToLower(m) == methodLower {
			return true
		}
	}
	return false
}
