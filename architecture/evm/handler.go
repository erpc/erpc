package evm

import (
	"context"

	"github.com/erpc/erpc/common"
)

func init() {
	common.RegisterArchitecture(common.ArchitectureEvm, &EvmArchitectureHandler{})
}

// EvmArchitectureHandler wraps the package-level EVM hook functions and registers
// them under ArchitectureEvm. No logic lives here — delegates to existing functions.
type EvmArchitectureHandler struct{}

func (h *EvmArchitectureHandler) HandleProjectPreForward(ctx context.Context, network common.Network, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return HandleProjectPreForward(ctx, network, req)
}

func (h *EvmArchitectureHandler) HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return HandleNetworkPreForward(ctx, network, upstreams, req)
}

func (h *EvmArchitectureHandler) HandleNetworkPostForward(ctx context.Context, network common.Network, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	return HandleNetworkPostForward(ctx, network, req, resp, err)
}

func (h *EvmArchitectureHandler) HandleUpstreamPreForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (bool, *common.NormalizedResponse, error) {
	return HandleUpstreamPreForward(ctx, network, upstream, req, skipCacheRead)
}

func (h *EvmArchitectureHandler) HandleUpstreamPostForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	return HandleUpstreamPostForward(ctx, network, upstream, req, resp, err, skipCacheRead)
}

func (h *EvmArchitectureHandler) NewJsonRpcErrorExtractor() common.JsonRpcErrorExtractor {
	return NewJsonRpcErrorExtractor()
}
