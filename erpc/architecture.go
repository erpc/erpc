package erpc

import (
	"context"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
)

// archBehavior is the single seam through which the per-request path reaches
// architecture-specific logic. Every operation listed here was previously an
// `if/switch n.cfg.Architecture == common.ArchitectureEvm` (or, worse, an
// ungated `evm.*` call) scattered across networks.go / projects.go.
//
// EVM is the only architecture this build supports, and this seam does not
// pretend otherwise: it is NOT a plugin framework and there is no registration
// API. It exists so the EVM commitment is made in ONE place and applied
// UNIFORMLY, instead of being restated (and half-applied) at a dozen call
// sites. The member set is derived strictly from the call sites that exist
// today — nothing speculative is declared here.
//
// The interface (rather than a struct of function fields) is deliberate: the
// compiler then guarantees an architecture either implements every operation
// the request path performs, or does not resolve at all. There is no
// "partially implemented architecture" state to nil-check per member.
type archBehavior interface {
	// prepareRequest normalizes an incoming request before dispatch.
	// Called from Network.prepareRequest.
	prepareRequest(ctx context.Context, nr *common.NormalizedRequest) error

	// applySafeBlockSource routes "safe"-tagged requests, before multiplexing
	// and cache lookup. Called from Network.Forward.
	applySafeBlockSource(ctx context.Context, n *Network, req *common.NormalizedRequest) error

	// projectPreForward is the early, project-level (cache-affecting,
	// upstream-agnostic) pre-forward hook. Called from PreparedProject.doForward.
	projectPreForward(ctx context.Context, n *Network, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error)

	// networkPreForward is the network-level pre-forward hook, executed after
	// upstream selection so it can see the candidate list. Called from Network.Forward.
	networkPreForward(ctx context.Context, n *Network, upstreams []common.Upstream, nq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error)

	// networkPostForward is the network-level post-forward hook. Called from
	// PreparedProject.doForward on every path (short-circuited and forwarded alike).
	networkPostForward(ctx context.Context, n *Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error)

	// upstreamPreForward / upstreamPostForward wrap a single upstream attempt.
	// Called from Network.doForward.
	upstreamPreForward(ctx context.Context, n *Network, u common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (handled bool, resp *common.NormalizedResponse, err error)
	upstreamPostForward(ctx context.Context, n *Network, u common.Upstream, req *common.NormalizedRequest, resp *common.NormalizedResponse, re error, skipCacheRead bool) (*common.NormalizedResponse, error)

	// requestBlockNumber resolves the concrete block a request targets, or 0
	// when it carries none (tags, hashes, unparseable params).
	requestBlockNumber(ctx context.Context, req *common.NormalizedRequest) int64

	// responseBlockNumber resolves the concrete block a response pertains to,
	// or 0 when it carries none.
	responseBlockNumber(ctx context.Context, resp *common.NormalizedResponse) int64

	// checkUpstreamBlockAvailability gates one upstream attempt on the
	// upstream's configured block-availability bounds.
	checkUpstreamBlockAvailability(ctx context.Context, n *Network, u common.Upstream, req *common.NormalizedRequest, method string) (error, bool)

	// eligibleUpstreamIDsForBoundary derives the block-availability lane for a
	// single-block request, feeding the selection policy's per-boundary axis.
	eligibleUpstreamIDsForBoundary(ctx context.Context, n *Network, method string, req *common.NormalizedRequest) []string

	// enrichStatePoller feeds head observations from a served response back
	// into the responding upstream's state poller.
	enrichStatePoller(ctx context.Context, n *Network, method string, req *common.NormalizedRequest, resp *common.NormalizedResponse)
}

// evmBehavior implements archBehavior for `evm` networks. Most members are
// straight delegations to architecture/evm; the three that need Network
// internals (block-availability gating, boundary lanes, state-poller
// enrichment) keep their bodies in networks.go next to the code they
// collaborate with, and are wired in from here by receiver type.
type evmBehavior struct{}

// archBehaviorFor maps an architecture to its behavior, or nil when this build
// has no implementation for it. Unsupported architectures are already rejected
// at the construction edge (NetworksRegistry.prepareNetwork), so a nil here
// means "a Network was assembled outside that edge" — every call site treats it
// as "run no architecture-specific logic" rather than assuming EVM.
func archBehaviorFor(architecture common.NetworkArchitecture) archBehavior {
	switch architecture {
	case common.ArchitectureEvm:
		return evmBehavior{}
	default:
		return nil
	}
}

// arch resolves this network's architecture behavior. Resolution is a two-case
// switch returning a stateless zero-size value, so it is deliberately NOT
// cached on the Network: a cached field would add construction-order coupling
// (and a nil-field hazard for the package-internal tests that assemble
// `&Network{...}` directly) without making any input resolve differently.
//
// Network.Architecture() is the single resolution rule — including its
// "empty architecture with an evm block means evm" default — so the seam and
// the network's own answer to "what architecture am I" can never disagree.
func (n *Network) arch() archBehavior {
	if n == nil || n.cfg == nil {
		return nil
	}
	return archBehaviorFor(n.Architecture())
}

func (evmBehavior) prepareRequest(ctx context.Context, nr *common.NormalizedRequest) error {
	jsonRpcReq, err := nr.JsonRpcRequest(ctx)
	if err != nil {
		return common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorParseException,
			"failed to unmarshal json-rpc request",
			err,
			nil,
		)
	}
	evm.NormalizeHttpJsonRpc(ctx, nr, jsonRpcReq)
	return nil
}

func (evmBehavior) applySafeBlockSource(ctx context.Context, n *Network, req *common.NormalizedRequest) error {
	return evm.ApplySafeBlockSource(ctx, n, req)
}

func (evmBehavior) projectPreForward(ctx context.Context, n *Network, nq *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return evm.HandleProjectPreForward(ctx, n, nq)
}

func (evmBehavior) networkPreForward(ctx context.Context, n *Network, upstreams []common.Upstream, nq *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	return evm.HandleNetworkPreForward(ctx, n, upstreams, nq)
}

func (evmBehavior) networkPostForward(ctx context.Context, n *Network, nq *common.NormalizedRequest, nr *common.NormalizedResponse, re error) (*common.NormalizedResponse, error) {
	return evm.HandleNetworkPostForward(ctx, n, nq, nr, re)
}

func (evmBehavior) upstreamPreForward(ctx context.Context, n *Network, u common.Upstream, req *common.NormalizedRequest, skipCacheRead bool) (bool, *common.NormalizedResponse, error) {
	return evm.HandleUpstreamPreForward(ctx, n, u, req, skipCacheRead)
}

func (evmBehavior) upstreamPostForward(ctx context.Context, n *Network, u common.Upstream, req *common.NormalizedRequest, resp *common.NormalizedResponse, re error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	return evm.HandleUpstreamPostForward(ctx, n, u, req, resp, re, skipCacheRead)
}

// requestBlockNumber prefers the block number cached during normalization and
// falls back to extracting it from the raw request (defensive: covers paths
// that bypass json_rpc.go normalization, and methods whose params have not
// been pre-cached yet).
func (evmBehavior) requestBlockNumber(ctx context.Context, req *common.NormalizedRequest) int64 {
	if v := req.EvmBlockNumber(); v != nil {
		if n64, ok := v.(int64); ok && n64 > 0 {
			return n64
		}
	}
	if _, bn, err := evm.ExtractBlockReferenceFromRequest(ctx, req); err == nil && bn > 0 {
		return bn
	}
	return 0
}

func (evmBehavior) responseBlockNumber(ctx context.Context, resp *common.NormalizedResponse) int64 {
	if _, bn, err := evm.ExtractBlockReferenceFromResponse(ctx, resp); err == nil && bn > 0 {
		return bn
	}
	return 0
}
