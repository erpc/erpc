package erpc

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
)

// headRefreshInterval is the longest a lane's Head goes without being
// computed (spec §6.4, #1273): the guard's TTL, the bench's clocks and the
// gauges move with time alone. Network.headRefresh overrides it.
const headRefreshInterval = time.Second

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

// startHeads computes every lane's Head now, so a network bootstrapped on
// upstreams with known heads serves its first request with a Head, then on
// each headsChanged and every headRefresh until the app stops. Once per
// network: its bootstrap can run again after it succeeded.
func (n *Network) startHeads() {
	if n.cfg == nil || n.cfg.Architecture != common.ArchitectureEvm || n.appCtx == nil {
		return
	}
	every := n.headRefresh
	if every <= 0 {
		every = headRefreshInterval
	}
	n.headLoopOnce.Do(func() {
		n.refreshHeads()
		n.bg.Go(func() {
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			for {
				select {
				case <-n.appCtx.Done():
					return
				case <-ticker.C:
				case <-n.headWake:
				}
				n.refreshHeads()
			}
		})
	})
}

// watchHeadCounters makes every move of ups's head counters, by a poll, a
// push or another instance, and every change of its chain-identity cordon,
// which moves it in or out of the ceiling, wake the head loop (spec §6.4).
// The registry calls it once per upstream as it joins the network, which
// is itself a change of the voters.
func (n *Network) watchHeadCounters(ups *upstream.Upstream) {
	if p, ok := ups.EvmStatePoller().(*evm.EvmStatePoller); ok && !p.IsObjectNull() {
		p.OnLatestBlock(func(int64) { n.headsChanged() })
		p.OnFinalizedBlock(func(int64) { n.headsChanged() })
		p.OnChainIdCordonChanged(n.headsChanged)
	}
	n.headsChanged()
}

// refreshHeads computes and publishes every lane's Head. A panic on one
// lane leaves the others computed, and the pass ends for its waiters.
func (n *Network) refreshHeads() {
	defer func() {
		if c := n.headPass.Swap(nil); c != nil {
			close(*c)
		}
	}()
	n.refreshLaneRecovered(&n.servedTipAll)
	n.servedTipLanes.Range(func(_, v any) bool {
		n.refreshLaneRecovered(v.(*servedTipLane))
		return true
	})
}

func (n *Network) refreshLaneRecovered(l *servedTipLane) {
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"network-head",
				fmt.Sprintf("project:%s network:%s lane:%s", n.projectId, n.networkId, l.label),
				common.ErrorFingerprint(rec),
			).Inc()
			n.logger.Error().Interface("panic", rec).Str("lane", l.label).Str("stack", string(debug.Stack())).
				Msg("unexpected panic while computing a lane's head")
		}
	}()
	n.refreshLane(l)
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
	ups := n.ceilingUpstreams(sel)
	var head int64
	for _, u := range ups {
		if sp := u.EvmStatePoller(); sp == nil {
			return 0
		}
		blk, _ := evmTipObservation(u, false)
		if blk <= 0 {
			return 0
		}
		head = max(head, blk)
	}
	return head
}

// ceilingUpstreams are the upstreams selector sel's lane ceiling counts:
// every registered one within sel, minus those cordoned for answering for
// another chain.
func (n *Network) ceilingUpstreams(sel string) []*upstream.Upstream {
	if n.upstreamsRegistry == nil {
		return nil
	}
	var out []*upstream.Upstream
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		if sel != "" {
			if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
				continue
			}
		}
		if c, ok := u.EvmStatePoller().(interface{ ChainIdCordoned() bool }); ok && c.ChainIdCordoned() {
			continue
		}
		out = append(out, u)
	}
	return out
}

// askLane Asks, with a number need, each upstream selector sel's lane
// ceiling counts whose head is below block, voters first (spec §6.4,
// §6.8): a poll debounced by this instance's last fetch of it, on the
// network's context bounded by W, so no caller's end fails it for the
// others. One poll per upstream is in flight at a time; a caller finding
// one joins it.
func (n *Network) askLane(sel string, block int64) {
	parent := n.appCtx
	if parent == nil {
		parent = context.Background()
	}
	ups := n.ceilingUpstreams(sel)
	for _, voters := range [2]bool{true, false} {
		for _, u := range ups {
			if _, _, ok := tipVoterRank(u, false, n.metricsTracker); ok != voters {
				continue
			}
			sp := u.EvmStatePoller()
			if sp == nil || sp.IsObjectNull() {
				continue
			}
			if head, _ := evmTipObservation(u, false); head >= block {
				continue
			}
			if _, inFlight := n.asks.LoadOrStore(u.Id(), struct{}{}); inFlight {
				continue
			}
			go func() {
				defer n.asks.Delete(u.Id())
				defer n.recoverAskPanic(sel)
				ctx, cancel := context.WithTimeout(parent, health.FreshnessWindow(n.EvmBlockTime()))
				defer cancel()
				_, _ = sp.PollLatestBlockNumber(ctx)
			}()
		}
	}
}

func (n *Network) recoverAskPanic(sel string) {
	if rec := recover(); rec != nil {
		telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
			"network-ask",
			fmt.Sprintf("project:%s network:%s selector:%s", n.projectId, n.networkId, sel),
			common.ErrorFingerprint(rec),
		).Inc()
		n.logger.Error().Interface("panic", rec).Str("selector", sel).Str("stack", string(debug.Stack())).
			Msg("unexpected panic while asking a lane's heads")
	}
}

// EvmAskCeiling Asks selector sel's lane for block (askLane) and waits
// until its ceiling reaches block, 1 bt passes or ctx ends, re-reading it
// at each head pass, then returns the lane's ceiling: a stale ceiling is
// Asked before any "future" answer, and the band's retry orders upstreams
// on the Ask's heads (spec §6.4).
func (n *Network) EvmAskCeiling(ctx context.Context, sel string, block int64) int64 {
	n.askLane(sel, block)
	bt := n.EvmBlockTime()
	if bt <= 0 {
		bt = health.ColdBlockTime
	}
	timer := time.NewTimer(bt)
	defer timer.Stop()
	for {
		pass := n.headsComputed()
		if c := n.laneCeiling(sel); c >= block {
			return c
		}
		select {
		case <-pass:
		case <-timer.C:
			return n.laneCeiling(sel)
		case <-ctx.Done():
			return n.laneCeiling(sel)
		}
	}
}

// holdByHash returns the answer to send for resp, an answer to req, which
// names a block or transaction by hash (spec §6.4): from a block above its
// lane's Latest, it waits up to 1 bt while the voters below that block
// refresh, then is sent if Latest includes the block, else it is null.
// held: resp was replaced by null.
func (n *Network) holdByHash(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) (out *common.NormalizedResponse, held bool) {
	if resp == nil || resp.IsObjectNull(ctx) || resp.IsResultEmptyish(ctx) || !namesHash(ctx, req) {
		return resp, false
	}
	_, bn, err := evm.ExtractBlockReferenceFromResponse(ctx, resp)
	if err != nil || bn <= 0 {
		return resp, false
	}
	sel := ""
	if d := req.Directives(); d != nil {
		sel = d.UseUpstream
	}
	latest := func() (int64, <-chan struct{}) {
		if l := n.laneFor(sel); l != nil {
			moved := l.latestMoved()
			return l.published().Latest, moved
		}
		computed := n.headsComputed()
		return n.headFor(sel).Latest, computed
	}
	at, moved := latest()
	if at <= 0 || bn <= at {
		return resp, false
	}
	n.EvmRefreshHeadsBelow(sel, bn)
	bt := n.EvmBlockTime()
	if bt <= 0 {
		bt = health.ColdBlockTime
	}
	timer := time.NewTimer(bt)
	defer timer.Stop()
	for at < bn {
		select {
		case <-moved:
		case <-timer.C:
			jrr, err := common.NewJsonRpcResponse(req.ID(), nil, nil)
			if err != nil {
				return resp, false
			}
			return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), true
		case <-ctx.Done():
			return resp, false
		}
		at, moved = latest()
	}
	return resp, false
}

// namesHash reports whether req's first param is a 32-byte hash: a block
// or transaction named by hash.
func namesHash(ctx context.Context, req *common.NormalizedRequest) bool {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return false
	}
	jrq.RLock()
	defer jrq.RUnlock()
	if len(jrq.Params) == 0 {
		return false
	}
	h, ok := jrq.Params[0].(string)
	return ok && len(h) == 66 && strings.HasPrefix(h, "0x")
}
