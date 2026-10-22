package common

import (
	"context"
	"fmt"
)

// ArchitectureHandler abstracts all architecture-specific pipeline hooks.
// Each architecture (evm, svm, …) registers exactly one handler via its package init().
// Pipeline files dispatch through the registry — they never need to change for a new chain.
type ArchitectureHandler interface {
	HandleProjectPreForward(ctx context.Context, network Network, req *NormalizedRequest) (handled bool, resp *NormalizedResponse, err error)
	HandleNetworkPreForward(ctx context.Context, network Network, upstreams []Upstream, req *NormalizedRequest) (handled bool, resp *NormalizedResponse, err error)
	HandleNetworkPostForward(ctx context.Context, network Network, req *NormalizedRequest, resp *NormalizedResponse, err error) (*NormalizedResponse, error)
	HandleUpstreamPreForward(ctx context.Context, network Network, upstream Upstream, req *NormalizedRequest, skipCacheRead bool) (handled bool, resp *NormalizedResponse, err error)
	HandleUpstreamPostForward(ctx context.Context, network Network, upstream Upstream, req *NormalizedRequest, resp *NormalizedResponse, err error, skipCacheRead bool) (*NormalizedResponse, error)
	NewJsonRpcErrorExtractor() JsonRpcErrorExtractor
}

// ArchitectureRegistry maps architecture names to their handlers.
// Populated by each architecture package's init() via RegisterArchitecture.
var ArchitectureRegistry = map[NetworkArchitecture]ArchitectureHandler{}

func RegisterArchitecture(name NetworkArchitecture, handler ArchitectureHandler) {
	ArchitectureRegistry[name] = handler
}

func GetArchitectureHandler(arch NetworkArchitecture) (ArchitectureHandler, error) {
	h, ok := ArchitectureRegistry[arch]
	if !ok {
		return nil, fmt.Errorf("no architecture handler registered for %q", arch)
	}
	return h, nil
}
