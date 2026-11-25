package erpc

import (
	"cmp"
	"context"
	"slices"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// lowerCallGas lowers the gas an eth_call names to the network's cap
// (callGasCap), as a node lowers it to its own cap: under the default cap,
// the lowest of the upstreams, the call runs alike on each of them. It runs
// before the multiplexer and the cache, so both key the lowered call.
func (n *Network) lowerCallGas(ctx context.Context, req *common.NormalizedRequest) {
	if method, _ := req.Method(); method != "eth_call" {
		return
	}
	gas := evm.CallGas(ctx, req)
	if gas == 0 {
		return
	}
	if c := n.callGasCap(ctx); c > 0 && gas > c {
		if jrq, err := req.JsonRpcRequest(ctx); err == nil {
			evm.SetCallGas(jrq, c)
		}
	}
}

// callGasCap is the most gas one eth_call may use on the network:
// evm.callGasCap when set, else the lowest cap of the upstreams that serve
// eth_call (upstreamCallGasCap), among those the request's use-upstream
// selector matches; 0 while none is known. A network that promised the
// largest cap would break its promise whenever the upstreams that give it
// are down. The cap of a request with no selector is the network's gauge.
func (n *Network) callGasCap(ctx context.Context) uint64 {
	var c uint64
	sel := ""
	if n.cfg.Evm != nil && n.cfg.Evm.CallGasCap > 0 {
		c = n.cfg.Evm.CallGasCap
	} else if n.upstreamsRegistry != nil {
		sel = requestSelector(ctx)
		for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
			if sel != "" {
				if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
					continue
				}
			}
			if uc := upstreamCallGasCap(u); uc > 0 && (c == 0 || uc < c) && servesMethod(u, "eth_call") {
				c = uc
			}
		}
	}
	if sel == "" && n.callGasCapShown.Swap(c) != c {
		if c == 0 {
			telemetry.MetricNetworkCallGasCap.DeleteLabelValues(n.projectId, n.Label())
		} else {
			telemetry.MetricNetworkCallGasCap.WithLabelValues(n.projectId, n.Label()).Set(float64(c))
		}
	}
	return c
}

// upstreamCallGasCap is u's eth_call gas cap: its evm.callGasCap when set,
// else as measured (EvmStatePoller.CallGasCap); 0 while unknown.
func upstreamCallGasCap(u common.Upstream) uint64 {
	if cfg := u.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.CallGasCap > 0 {
		return cfg.Evm.CallGasCap
	}
	if eu, ok := u.(common.EvmUpstream); ok {
		if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
			return sp.CallGasCap()
		}
	}
	return 0
}

// gasFit is the gas an eth_call names, which orders upstreams by their
// eth_call gas cap (upstreamCallGasCap): a node lowers a larger gas to its
// cap, and the out-of-gas revert that follows is an answer, not retried.
// Only a network cap above an upstream's leaves the upstream short of a
// call's gas (callGasCap). 0, for any other request, orders nothing.
type gasFit uint64

func callGasFit(ctx context.Context, req *common.NormalizedRequest) gasFit {
	if method, _ := req.Method(); method != "eth_call" {
		return 0
	}
	return gasFit(evm.CallGas(ctx, req))
}

// short reports whether u's cap is below the gas. An upstream with no known
// cap counts as giving it.
func (g gasFit) short(u common.Upstream) bool {
	if g == 0 {
		return false
	}
	c := upstreamCallGasCap(u)
	return c > 0 && c < uint64(g)
}

// compare puts upstreams that give the gas before those short of it, these
// largest cap first; 0 between two that give it, so a stable sort keeps
// their order.
func (g gasFit) compare(a, b common.Upstream) int {
	sa, sb := g.short(a), g.short(b)
	switch {
	case sa && sb:
		return cmp.Compare(upstreamCallGasCap(b), upstreamCallGasCap(a))
	case sa:
		return 1
	case sb:
		return -1
	}
	return 0
}

// waits reports whether u, short of the gas, waits for an upstream ahead of
// it in gas order (compare) that req holds in flight: u's out-of-gas revert
// would end a hedge race the other may still answer.
func (g gasFit) waits(req *common.NormalizedRequest, u common.Upstream) bool {
	if !g.short(u) {
		return false
	}
	for _, v := range req.Upstreams() {
		if v.Id() != u.Id() && g.compare(v, u) < 0 && req.Reserved(v) {
			return true
		}
	}
	return false
}

// gasShortLast moves the upstreams short of an eth_call's gas (gasFit) to
// the end of ups; ups itself when none is.
func gasShortLast(ctx context.Context, req *common.NormalizedRequest, ups []common.Upstream) []common.Upstream {
	g := callGasFit(ctx, req)
	if !slices.ContainsFunc(ups, g.short) {
		return ups
	}
	out := slices.Clone(ups)
	slices.SortStableFunc(out, g.compare)
	return out
}
