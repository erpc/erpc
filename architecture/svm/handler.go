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
	return networkPreForward_injectCommitment(ctx, network, req)
}

func (h *SvmArchitectureHandler) HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	// Per-method validation gates that can short-circuit the request. Commitment
	// injection deliberately does NOT happen here — see HandleProjectPreForward.
	method, err := req.Method()
	if err != nil {
		return false, nil, err
	}
	if method == "getSignaturesForAddress" {
		return networkPreForward_validateSignaturesForAddress(ctx, network, req)
	}
	return false, nil, nil
}

func (h *SvmArchitectureHandler) HandleNetworkPostForward(ctx context.Context, network common.Network, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
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
	// Double-spend guard: a failing sendTransaction must not silently be retried
	// against another upstream, because the transaction may still propagate via
	// the original node. Strip retryability before returning.
	if strings.EqualFold(method, "sendTransaction") || strings.EqualFold(method, "sendRawTransaction") {
		return upstreamPostForward_sendTransaction(resp, err)
	}
	// Opportunistic slot tracking — uses response.context.slot to keep the
	// upstream's SvmStatePoller fresh between polling ticks. Silent on miss.
	if err == nil {
		upstreamPostForward_trackContextSlot(ctx, upstream, resp)
	}
	return resp, err
}

func (h *SvmArchitectureHandler) NewJsonRpcErrorExtractor() common.JsonRpcErrorExtractor {
	return NewJsonRpcErrorExtractor()
}
