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
// (callGasCap), as a node lowers it to its own cap. It runs before the
// multiplexer and the cache, so both key the lowered call.
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
// evm.callGasCap when set, else the highest cap that evm.callGasCapQuorum
// of the upstreams serving eth_call give (upstreamCallGasCap), among those
// the request's use-upstream selector matches; 0 while none is known or any
// is still measuring, as the caps known then are a part of the ballot and
// may name a cap below the network's. The largest cap alone would be a
// promise one node keeps, broken whenever it is down; the lowest would let
// one small node refuse every big call. The cap of a request with no
// selector is the network's gauge.
func (n *Network) callGasCap(ctx context.Context) uint64 {
	var c uint64
	sel := ""
	if n.cfg.Evm != nil && n.cfg.Evm.CallGasCap > 0 {
		c = n.cfg.Evm.CallGasCap
	} else if n.upstreamsRegistry != nil {
		sel = requestSelector(ctx)
		var buf [16]uint64
		caps := buf[:0]
		measuring := false
		for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
			if sel != "" {
				if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
					continue
				}
			}
			if !servesMethod(u, "eth_call") {
				continue
			}
			// Every upstream is read, so one call starts every probe.
			uc, m := upstreamCallGasCap(u)
			measuring = measuring || m
			if uc > 0 {
				caps = append(caps, uc)
			}
		}
		if len(caps) > 0 && !measuring {
			var q common.ServedTipQuorum
			if n.cfg.Evm != nil {
				q = n.cfg.Evm.CallGasCapQuorum
			}
			slices.Sort(caps)
			c = caps[len(caps)-q.Of(len(caps))]
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
// else as measured (EvmStatePoller.CallGasCap); 0 while unknown. measuring
// is true until u's first probe ends.
func upstreamCallGasCap(u common.Upstream) (gasCap uint64, measuring bool) {
	if cfg := u.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.CallGasCap > 0 {
		return cfg.Evm.CallGasCap, false
	}
	if eu, ok := u.(common.EvmUpstream); ok {
		if sp := eu.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
			return sp.CallGasCap()
		}
	}
	return 0, false
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
	c, _ := upstreamCallGasCap(u)
	return c > 0 && c < uint64(g)
}

// compare puts upstreams that give the gas before those short of it, these
// largest cap first; 0 between two that give it, so a stable sort keeps
// their order.
func (g gasFit) compare(a, b common.Upstream) int {
	sa, sb := g.short(a), g.short(b)
	switch {
	case sa && sb:
		ca, _ := upstreamCallGasCap(a)
		cb, _ := upstreamCallGasCap(b)
		return cmp.Compare(cb, ca)
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
