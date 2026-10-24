package erpc

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// headRefreshInterval is the longest a lane's Head goes without being
// computed (spec §6.4, #1273): the guard's TTL, the bench's clocks and the
// gauges move with time alone.
var headRefreshInterval = time.Second

// unknownHead is the Head of a lane before its first computation.
var unknownHead = &common.EvmHead{}

// EvmHead is the Head req serves (spec §7): the one req took from its lane
// before the multiplexer and the cache, taken now when it took none. With
// no req, the request bound to ctx; with neither, the network-wide lane's.
func (n *Network) EvmHead(ctx context.Context, req *common.NormalizedRequest) common.EvmHead {
	if req == nil {
		req, _ = ctx.Value(common.RequestContextKey).(*common.NormalizedRequest)
	}
	if req == nil {
		return *n.servedTipAll.published()
	}
	return *n.requestHead(req)
}

// requestHead is the Head req took once from its lane; every later reader
// of req gets that value, so retries, hedges, the cache key and
// interpolation all see the same Latest.
func (n *Network) requestHead(req *common.NormalizedRequest) *common.EvmHead {
	if h := req.EvmHead(); h != nil {
		return h
	}
	// The lane comes from the directives Forward applies; a reader before
	// Forward (the project's finality) must pick the same lane.
	if n.cfg != nil {
		req.ApplyDirectiveDefaults(n.cfg.DirectiveDefaults)
	}
	sel := ""
	if d := req.Directives(); d != nil {
		sel = d.UseUpstream
	}
	return req.TakeEvmHead(n.headFor(sel))
}

// headFor is the Head of selector sel: its lane's published one, or, for a
// selector that forms no lane, the stateless pick by the same rules.
func (n *Network) headFor(sel string) *common.EvmHead {
	if l := n.laneFor(sel); l != nil {
		return l.published()
	}
	h := n.computeHead(nil, sel)
	return &h
}

// EvmLaneHead is the published Head of the lane keyed groupKey ("" is the
// network-wide lane), materialized from selector on first use: what a
// lane's tracker sends up to (spec §6.5).
func (n *Network) EvmLaneHead(groupKey, selector string) common.EvmHead {
	if groupKey == "" {
		return *n.servedTipAll.published()
	}
	if l, ok := n.servedTipLanes.Load(groupKey); ok {
		return *l.(*servedTipLane).published()
	}
	return *n.headFor(selector)
}

func (l *servedTipLane) published() *common.EvmHead {
	if h := l.head.Load(); h != nil {
		return h
	}
	return unknownHead
}

// latestMoved returns a channel closed at the next change of l's Latest.
func (l *servedTipLane) latestMoved() <-chan struct{} {
	for {
		if c := l.moved.Load(); c != nil {
			return *c
		}
		c := make(chan struct{})
		if l.moved.CompareAndSwap(nil, &c) {
			return c
		}
	}
}

// headsComputed returns a channel closed after the next pass over every
// lane, for a reader whose selector forms no lane.
func (n *Network) headsComputed() <-chan struct{} {
	for {
		if c := n.headPass.Load(); c != nil {
			return *c
		}
		c := make(chan struct{})
		if n.headPass.CompareAndSwap(nil, &c) {
			return c
		}
	}
}

// headsChanged asks for every lane's Head to be computed again: an upstream
// head counter moved (local or remote), a bench changed, or the policy's
// eligible set changed. It never blocks.
func (n *Network) headsChanged() {
	select {
	case n.headWake <- struct{}{}:
	default:
	}
}

// startHeads computes every lane's Head on each headsChanged and every
// headRefreshInterval until the app stops. Once per network: its bootstrap
// can run again after it succeeded.
func (n *Network) startHeads() {
	if n.cfg == nil || n.cfg.Architecture != common.ArchitectureEvm || n.appCtx == nil {
		return
	}
	n.headLoopOnce.Do(func() {
		n.bg.Go(func() {
			// The loop's own: a poller's callbacks cannot be removed, so an
			// upstream that leaves and comes back is watched once.
			watched := map[*evm.EvmStatePoller]struct{}{}
			ticker := time.NewTicker(headRefreshInterval)
			defer ticker.Stop()
			for {
				n.refreshHeadsRecovered(watched)
				select {
				case <-n.appCtx.Done():
					return
				case <-ticker.C:
				case <-n.headWake:
				}
			}
		})
	})
}

func (n *Network) refreshHeadsRecovered(watched map[*evm.EvmStatePoller]struct{}) {
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"network-head",
				fmt.Sprintf("project:%s network:%s", n.projectId, n.networkId),
				common.ErrorFingerprint(rec),
			).Inc()
			n.logger.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).
				Msg("unexpected panic while computing the network's heads")
		}
	}()
	n.watchHeadCounters(watched)
	n.refreshHeads()
}

// watchHeadCounters makes every move of each upstream's head counters, by a
// poll, a push or another instance, wake the head loop (spec §6.4).
func (n *Network) watchHeadCounters(watched map[*evm.EvmStatePoller]struct{}) {
	if n.upstreamsRegistry == nil {
		return
	}
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		p, ok := u.EvmStatePoller().(*evm.EvmStatePoller)
		if !ok || p.IsObjectNull() {
			continue
		}
		if _, seen := watched[p]; seen {
			continue
		}
		watched[p] = struct{}{}
		p.OnLatestBlock(func(int64) { n.headsChanged() })
		p.OnFinalizedBlock(func(int64) { n.headsChanged() })
	}
}

// refreshHeads computes and publishes every lane's Head.
func (n *Network) refreshHeads() {
	n.refreshLane(&n.servedTipAll)
	n.servedTipLanes.Range(func(_, v any) bool {
		n.refreshLane(v.(*servedTipLane))
		return true
	})
	if c := n.headPass.Swap(nil); c != nil {
		close(*c)
	}
}

// refreshLane computes l's Head and publishes it in one step. A change of
// Latest wakes the lane's tracker and its taps, which send no block above
// it (spec §6.5).
func (n *Network) refreshLane(l *servedTipLane) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.published()
	h := n.computeHead(l, l.selector)
	if h.Ceiling == old.Ceiling && h.Ceiling > 0 {
		h.CeilingAt = old.CeilingAt
	}
	if h == *old {
		return
	}
	l.head.Store(&h)
	if h.Latest != old.Latest {
		if c := l.moved.Swap(nil); c != nil {
			close(*c)
		}
		evm.LatestChanged(n, l.key)
	}
}

// computeHead computes the Head of selector sel (spec §6.4); l is its lane,
// nil for the stateless pick of a selector that forms none: no guard, no
// gauges, no anchor.
func (n *Network) computeHead(l *servedTipLane, sel string) common.EvmHead {
	h := common.EvmHead{
		Latest:    n.laneTip(l, sel, false),
		Finalized: n.laneTip(l, sel, true),
	}
	h.Leader = n.laneLeader(sel, h.Latest)
	h.Ceiling = n.laneCeiling(sel)
	if h.Ceiling > 0 {
		h.CeilingAt = time.Now()
	}
	return h
}

// laneTip is one axis of the Head: the freshest block at least the
// configured quorum of the lane's eligible voters already has (evmTipBallot,
// common.PickServedTip), guarded against a regression far below the live
// heads, clamped to every configured guarantee, then exported on the lane.
//
// The guard is in-process, bounded and one-directional (it can only hold the
// tip, and only briefly), and it cannot produce a block that no live upstream
// reports in the very same evaluation. Nothing is persisted and nothing is
// predicted, so no inherited or rogue value can pin, freeze or poison the
// result (networks_served_tip_invariants_test.go pins those outcomes against
// the frozen-tip regression).
func (n *Network) laneTip(l *servedTipLane, sel string, finalized bool) int64 {
	axis := "latest"
	if finalized {
		axis = "finalized"
	}
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	pick, live := n.evmTipBallot(sel, finalized, q, nil)
	tip := pick.Tip

	// Regression guard: "latest" cannot drop far below the corroborated live
	// head in one evaluation. Runs BEFORE the guaranteed-method floor, which is
	// a deliberate operator-configured clamp the guard must not fight.
	var anchor *servedTipAnchor
	if l != nil {
		anchor = l.anchor(finalized)
		tip = n.guardServedTipRegression(axis, l.label, anchor, pick, live)
	}

	// Capability guarantee (#855): the served tip must never exceed what any
	// configured guarantee SUBSET can serve — a method's supporting upstreams
	// (guaranteedMethods) or an operator-named group (guaranteedFor) — so a
	// request on such a method (e.g. trace_*), or one consensus requires that
	// group to answer, never resolves "latest" to a block only upstreams
	// outside the subset have.
	if tip > 0 {
		if floor := n.guaranteedFloor(sel, finalized); floor > 0 && floor < tip {
			tip = floor
		}
	}

	// Watchdog anchor: when did this process last see the served value change.
	if l != nil {
		if tip > 0 {
			anchor.observe(tip)
		}
		n.observeServedTipMetrics(axis, l.label, tip, pick.Freshest, anchor.age())
	}
	return tip
}

// laneLeader is the highest voter of the lane's ballot whose head is within
// latest + maxRetryableBlockDistance (#1154): a head beyond reach of the
// served one never leads. nil while latest is unknown.
func (n *Network) laneLeader(sel string, latest int64) common.Upstream {
	if latest <= 0 {
		return nil
	}
	reach := latest + n.maxRetryableBlockDistance()
	var leader common.Upstream
	var top int64
	best := -1
	for u := range n.tipCandidates(sel) {
		if n.isForkDemoted(u) {
			continue
		}
		head, rank, ok := tipVoterRank(u, false, n.metricsTracker)
		if !ok || (best >= 0 && rank > best) {
			continue
		}
		if best < 0 || rank < best {
			best, leader, top = rank, nil, 0
		}
		if head <= reach && head > top {
			leader, top = u, head
		}
	}
	return leader
}

// laneCeiling is the highest effective latest head across the lane's
// registered upstreams, regardless of selection policy, tier or syncing
// state, since any of them can end up serving a request (policy order, the
// fallback escape, syncing upstreams without skipWhenSyncing), minus those
// cordoned for answering for another chain. It uses latest heads whatever
// emptyResultConfidence says: that setting decides how an empty answer is
// treated, not whether an upstream can have the block. 0 (unknown) when the
// lane has no upstream or any upstream's head is unknown.
func (n *Network) laneCeiling(sel string) int64 {
	if n.upstreamsRegistry == nil {
		return 0
	}
	var head int64
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		if sel != "" {
			if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
				continue
			}
		}
		sp := u.EvmStatePoller()
		if sp == nil {
			return 0
		}
		if c, ok := sp.(interface{ ChainIdCordoned() bool }); ok && c.ChainIdCordoned() {
			continue
		}
		blk, _ := evmTipObservation(u, false)
		if blk <= 0 {
			return 0
		}
		head = max(head, blk)
	}
	return head
}
