package jsonrpc

import (
	"github.com/erpc/erpc/common"
)

// IsReplaySafe reports whether req may be dispatched a second time (retry,
// hedge, failover). It is false in exactly one case: the request is bound to
// a jsonrpc network and its method is not declared in that network's
// generic.replaySafeMethods. Non-jsonrpc networks always return true (their
// own write-method sets govern), and so does a request with no network yet —
// upstream-level probes and unit-test forwards — because the generic policy
// has nothing to say about them and must not change EVM/SVM behavior.
//
// On a jsonrpc network eRPC knows nothing about a method's side effects, so
// the default is the conservative one: a method is replayable only when the
// operator said so. Everything else is dispatched exactly once — no hedge, no
// retry, no failover — and whatever the single attempt produced is surfaced.
func IsReplaySafe(req *common.NormalizedRequest) bool {
	if req == nil {
		return true
	}
	network := req.Network()
	if network == nil {
		return true
	}
	cfg := network.Config()
	if cfg == nil || cfg.Architecture != common.ArchitectureJsonRpc {
		return true
	}
	if cfg.Generic == nil {
		return false
	}
	method, err := req.Method()
	if err != nil || method == "" {
		return false
	}
	for _, pattern := range cfg.Generic.ReplaySafeMethods {
		if ok, err := common.WildcardMatch(pattern, method); err == nil && ok {
			return true
		}
	}
	return false
}

// upstreamPostForward_replayGuard marks errors from undeclared methods as
// non-retryable across upstreams. The method set is the complement of
// generic.replaySafeMethods: for an undeclared method, a second physical call
// after an error — against the same upstream or a different one — may repeat a
// side effect whose first attempt already took hold (the classic
// "createItem recorded, response lost" case). SVM has the analogous guard for
// sendTransaction/requestAirdrop and EVM for eth_sendRawTransaction; here the
// operator's declaration replaces the hardcoded method list.
//
// We always wrap as ClientSideException (not just flip retryableTowardNetwork on
// the original error) because the network-level upstream loop bails out on
// common.IsClientError — that check looks specifically for
// ErrCodeEndpointClientSideException rather than walking retryableTowardNetwork
// details. Without the wrap, a ServerSideException from the primary would be
// silently failed over to the secondary. The original error (and its
// ErrCodeEndpointServerSideException, HTTP status, JSON-RPC code/message/data)
// stays reachable as the cause, so the caller sees an upstream failure, not a
// "rejected before execution".
//
// The wrap applies ONLY to errors from an attempt that may have reached the
// wire. See common.PreDispatchErrorCodes.
func upstreamPostForward_replayGuard(req *common.NormalizedRequest, rs *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	if re == nil || IsReplaySafe(req) {
		return rs, re
	}
	// A call that was never transmitted cannot have taken effect, so there is
	// nothing to protect against a second dispatch — suppressing failover here
	// turns a routine "this upstream is unavailable" into a hard client error
	// while healthy upstreams sit unused.
	if common.HasErrorCode(re, common.PreDispatchErrorCodes...) {
		return rs, re
	}
	return rs, common.NewErrEndpointClientSideException(re).WithRetryableTowardNetwork(false)
}
