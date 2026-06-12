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
		return projectPreForward_getGenesisHash(ctx, network, req)
	}
	return false, nil, nil
}

func (h *SvmArchitectureHandler) HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	// Per-method validation gates that can short-circuit the request.
	method, err := req.Method()
	if err != nil {
		return false, nil, err
	}
	if method == "getSignaturesForAddress" {
		if handled, resp, verr := networkPreForward_validateSignaturesForAddress(ctx, network, req); handled || verr != nil {
			return handled, resp, verr
		}
	}

	// Non-short-circuiting mutation: stamp commitment onto the body.
	return networkPreForward_injectCommitment(ctx, network, req)
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
