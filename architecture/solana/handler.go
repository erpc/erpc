package solana

import (
	"context"

	"github.com/erpc/erpc/common"
)

// architectureHandler bridges Solana's free-function lifecycle hooks to the
// common.ArchitectureHandler interface so erpc/ dispatch sites can look up
// the right handler instead of switching on architecture at every call site.
type architectureHandler struct{}

func (architectureHandler) HandleProjectPreForward(ctx context.Context, network common.Network, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return HandleProjectPreForward(ctx, network, req)
}

func (architectureHandler) HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return HandleNetworkPreForward(ctx, network, upstreams, req)
}

func (architectureHandler) HandleNetworkPostForward(ctx context.Context, network common.Network, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	return HandleNetworkPostForward(ctx, network, req, resp, err)
}

func (architectureHandler) HandleUpstreamPreForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (bool, *common.NormalizedResponse, error) {
	return HandleUpstreamPreForward(ctx, network, upstream, req, skipCacheRead)
}

func (architectureHandler) HandleUpstreamPostForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	return HandleUpstreamPostForward(ctx, network, upstream, req, resp, err, skipCacheRead)
}

func (architectureHandler) NormalizeHttpJsonRpc(ctx context.Context, req *common.NormalizedRequest, jsonRpcReq *common.JsonRpcRequest) {
	NormalizeHttpJsonRpc(ctx, req, jsonRpcReq)
}

func init() {
	common.RegisterArchitectureHandler(common.ArchitectureSolana, architectureHandler{})
}
