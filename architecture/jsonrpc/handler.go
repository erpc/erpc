package jsonrpc

import (
	"context"

	"github.com/erpc/erpc/common"
)

func init() {
	common.RegisterArchitecture(common.ArchitectureJsonRpc, &JsonRpcArchitectureHandler{})
}

// JsonRpcArchitectureHandler is the protocol-agnostic architecture: every
// pre/post-forward hook is a no-op except the upstream post-forward replay
// guard, because a generic JSON-RPC endpoint exposes no block tracking,
// finality or consensus semantics eRPC could act on. Load balancing,
// failsafe, rate limiting, singleflight and metrics all come from the shared
// pipeline for free.
type JsonRpcArchitectureHandler struct{}

func (h *JsonRpcArchitectureHandler) HandleProjectPreForward(ctx context.Context, network common.Network, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return false, nil, nil
}

func (h *JsonRpcArchitectureHandler) HandleNetworkPreForward(ctx context.Context, network common.Network, upstreams []common.Upstream, req *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return false, nil, nil
}

func (h *JsonRpcArchitectureHandler) HandleNetworkPostForward(ctx context.Context, network common.Network, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error) (*common.NormalizedResponse, error) {
	return resp, err
}

func (h *JsonRpcArchitectureHandler) HandleUpstreamPreForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (bool, *common.NormalizedResponse, error) {
	return false, nil, nil
}

func (h *JsonRpcArchitectureHandler) HandleUpstreamPostForward(ctx context.Context, network common.Network, upstream common.Upstream, req *common.NormalizedRequest, resp *common.NormalizedResponse, err error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	return upstreamPostForward_replayGuard(req, resp, err)
}

func (h *JsonRpcArchitectureHandler) NewJsonRpcErrorExtractor() common.JsonRpcErrorExtractor {
	return NewJsonRpcErrorExtractor()
}
