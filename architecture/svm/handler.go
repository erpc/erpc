package svm

import (
	"context"
	"strings"

	"github.com/erpc/erpc/common"
)

func init() {
	common.RegisterArchitecture(common.ArchitectureSvm, &SvmArchitectureHandler{})
}

// SvmArchitectureHandler is the registry hook that exposes SVM's pre/post-forward
// hooks and error extractor to the generic pipeline. Each method is a thin wrapper;
// real logic lives in the architecture/svm subpackage.
type SvmArchitectureHandler struct{}

func (h *SvmArchitectureHandler) HandleProjectPreForward(ctx context.Context, network common.Network, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	method, err := req.Method()
	if err != nil {
		return false, nil, err
	}
	switch method {
	case "getGenesisHash":
		if handled, resp, gerr := projectPreForward_getGenesisHash(ctx, network, req); handled || gerr != nil {
			return handled, resp, gerr
		}
	}

	// Commitment injection runs here — at the project layer, BEFORE the
	// network-layer cache read — rather than in HandleNetworkPreForward. It
	// mutates params and invalidates the memoized CacheHash, so running it after
	// the cache GET would key the read on pre-injection params and the write on
	// post-injection params: a permanent cache miss for commitment-defaulted
	// requests. It is upstream-agnostic (needs only network config), so the
	// project layer is the correct, earliest-safe place. Non-short-circuiting.
	if handled, resp, ierr := networkPreForward_injectCommitment(ctx, network, req); handled || ierr != nil {
		return handled, resp, ierr
	}
	// Write/effectful methods (sendTransaction, simulateTransaction,
	// requestAirdrop) carry commitment via their own config field rather than the
	// read-path "commitment" param, so normalize those too. The read and write
	// method sets are disjoint, so only one of these mutates any given request.
	return networkPreForward_injectWriteCommitment(ctx, network, req)
}

func (h *SvmArchitectureHandler) HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	// Per-method validation gates that can short-circuit the request. Commitment
	// injection deliberately does NOT happen here — see HandleProjectPreForward.
	method, err := req.Method()
	if err != nil {
		return false, nil, err
	}
	switch method {
	case "getBlock", "getConfirmedBlock":
		return networkPreForward_getBlock(ctx, network, req)
	}
	return false, nil, nil
}

func (h *SvmArchitectureHandler) HandleNetworkPostForward(ctx context.Context, network common.Network, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	if err != nil || resp == nil {
		return resp, err
	}
	method, mErr := req.Method()
	if mErr != nil {
		return resp, err
	}
	switch strings.ToLower(method) {
	case "getslot":
		// getBlockHeight is deliberately NOT routed here. Solana's block height
		// and slot number are different counters — block height trails the slot
		// number by the count of skipped slots (tens of millions on
		// mainnet-beta) — so applying the slot-tip floor to a getBlockHeight
		// response would replace it with a slot number. That breaks the
		// canonical transaction-expiry check (getBlockHeight vs
		// lastValidBlockHeight from getLatestBlockhash), which would then see
		// every transaction as permanently expired.
		return networkPostForward_getSlot(ctx, network, req, resp, err)
	}
	return resp, err
}

func (h *SvmArchitectureHandler) HandleUpstreamPreForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (bool, *common.NormalizedResponse, error) {
	return false, nil, nil
}

func (h *SvmArchitectureHandler) HandleUpstreamPostForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	method, mErr := req.Method()
	if mErr != nil {
		return resp, err
	}
	// Non-idempotent write guard: a failing sendTransaction must not silently be
	// retried against another upstream, because the transaction may still
	// propagate via the original node; and requestAirdrop MINTS per call, so a
	// failover after an effective first attempt mints twice. Both live in
	// IsNonRetryableWriteMethod — gate on that helper rather than re-listing
	// method names here, so the set cannot drift between call sites.
	if IsNonRetryableWriteMethod(method) {
		return upstreamPostForward_nonRetryableWrite(resp, err)
	}
	// Opportunistic slot tracking — uses response.context.slot to keep the
	// upstream's SvmStatePoller fresh between polling ticks (and to feed the
	// poller's traffic gate). The hook itself filters to the methods whose
	// result actually carries a context envelope (see contextSlotMethods), so
	// this never walks a multi-megabyte getBlock payload. Silent on miss.
	if err == nil {
		upstreamPostForward_trackContextSlot(ctx, network, upstream, req, resp)
	}
	return resp, err
}

func (h *SvmArchitectureHandler) NewJsonRpcErrorExtractor() common.JsonRpcErrorExtractor {
	return NewJsonRpcErrorExtractor()
}
