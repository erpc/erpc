package common

import (
	"context"
	"sync"
)

// ArchitectureHandler is the per-architecture surface erpc dispatches to
// during the request lifecycle. Each architecture (EVM, Solana, future)
// implements it once and registers itself via RegisterArchitectureHandler
// from its package init(). Call sites in erpc/ then look up the handler for
// a network's architecture and invoke methods on it — no per-architecture
// switches needed at every dispatch point.
type ArchitectureHandler interface {
	// HandleProjectPreForward runs before any cache lookup or upstream
	// selection, at the project boundary. Returning handled=true short-
	// circuits the rest of the forward path.
	HandleProjectPreForward(ctx context.Context, network Network, req *NormalizedRequest) (handled bool, resp *NormalizedResponse, err error)

	// HandleNetworkPreForward runs after upstream selection, at network scope.
	// Returning handled=true short-circuits the upstream loop.
	HandleNetworkPreForward(ctx context.Context, network Network, upstreams []Upstream, req *NormalizedRequest) (handled bool, resp *NormalizedResponse, err error)

	// HandleNetworkPostForward runs after the upstream loop completes, at
	// network scope. May enrich, fix, or reject the response.
	HandleNetworkPostForward(ctx context.Context, network Network, req *NormalizedRequest, resp *NormalizedResponse, err error) (*NormalizedResponse, error)

	// HandleUpstreamPreForward runs before a request is sent to a specific
	// upstream. Returning handled=true means the hook produced a response
	// directly (e.g. served from local state) and the upstream HTTP call
	// should be skipped.
	HandleUpstreamPreForward(ctx context.Context, network Network, upstream Upstream, req *NormalizedRequest, skipCacheRead bool) (handled bool, resp *NormalizedResponse, err error)

	// HandleUpstreamPostForward runs after a request returns from an upstream.
	// May translate errors, decorate responses, or trigger side effects.
	HandleUpstreamPostForward(ctx context.Context, network Network, upstream Upstream, req *NormalizedRequest, resp *NormalizedResponse, err error, skipCacheRead bool) (*NormalizedResponse, error)

	// NormalizeHttpJsonRpc applies architecture-specific normalization to an
	// inbound JSON-RPC request (canonical method name, parameter shape, etc.).
	NormalizeHttpJsonRpc(ctx context.Context, req *NormalizedRequest, jsonRpcReq *JsonRpcRequest)
}

var (
	architectureHandlers   = map[NetworkArchitecture]ArchitectureHandler{}
	architectureHandlersMu sync.RWMutex
)

// RegisterArchitectureHandler associates an architecture with its handler.
// Intended for use from each architecture package's init() function. Calling
// this twice for the same architecture overwrites the previous registration
// — useful in tests, surprising in production.
func RegisterArchitectureHandler(arch NetworkArchitecture, h ArchitectureHandler) {
	architectureHandlersMu.Lock()
	defer architectureHandlersMu.Unlock()
	architectureHandlers[arch] = h
}

// GetArchitectureHandler returns the registered handler for the given
// architecture, or nil if none is registered. Callers should treat nil as
// "unknown architecture" and surface an error rather than panic.
func GetArchitectureHandler(arch NetworkArchitecture) ArchitectureHandler {
	architectureHandlersMu.RLock()
	defer architectureHandlersMu.RUnlock()
	return architectureHandlers[arch]
}
