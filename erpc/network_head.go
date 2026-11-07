package erpc

import (
	"context"
	"encoding/hex"
	"fmt"
	"iter"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
)

// headRefreshInterval is the longest a lane's number facts go without
// being computed (spec §6.4, #1273): the guard's TTL, the bench's clocks
// and the gauges move with time alone. Network.headRefresh overrides it.
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
		return *n.headFor(tipScope{})
	}
	return *n.requestHead(ctx, req)
}

// requestHead is the Head req took once from its scope; every later reader
// of req gets that value, so retries, hedges, the cache key and
// interpolation all see the same Latest. A request naming the finalized
// tag starts the lane's finalized refresh and takes it at once (spec §6.4
// Finality). A read of latest
// (evm.ReadsLatest) is the block clock's demand (§6.8); the first after W
// without demand waits for the view to reach the idle edge's fetch,
// before the cache: a cache hit is pinned to that Head too. A head number
// waits only on a view that stood for W. A network-wide client request's
// Head is anchored to the cache's head (anchorHead); an internal one
// (UpstreamPolicies other than Configured: tracker fetches, their cache
// seeds, probes) takes the tracker's own Head, so it neither starts the
// anchor's hold nor counts as a Latest served.
func (n *Network) requestHead(ctx context.Context, req *common.NormalizedRequest) *common.EvmHead {
	if h := req.EvmHead(); h != nil {
		return h
	}
	// The lane comes from the directives Forward applies; a reader before
	// Forward (the project's finality) must pick the same lane.
	if n.cfg != nil {
		req.ApplyDirectiveDefaults(n.cfg.DirectiveDefaults)
	}
	s := n.requestScope(ctx, req)
	if evm.NamesFinalizedTag(ctx, req) {
		n.refreshFinalized(ctx, s.sel)
	} else if reads, number := evm.ReadsLatest(ctx, n, req); reads {
		if t := n.laneTracker(s.sel); t != nil {
			t.LatestRead(ctx, number)
		}
	}
	h := n.headFor(s)
	if n.cacheHead == nil || s.sel != "" || len(s.excluded) > 0 || req.UpstreamPolicies() != common.UpstreamPoliciesConfigured {
		return req.TakeEvmHead(h)
	}
	h = req.TakeEvmHead(n.anchorHead(h))
	if h.Latest > 0 {
		n.servedLatest.Store(h.Latest)
	}
	return h
}

// anchorHead is h, a network-wide read's Head, with Latest lowered to the
// cache's fresh head (evm.servedTip.followCacheHead, evm.CacheHeadAnchor
// .Clamp against the latest last served): Latest takes the view's pin at
// that number, its hash, and Finalized and Firm stay at or below it, as
// viewHead does for a limit. Only the read Head: headFor, the view, pins,
// intake, hash votes, the block clock, the refusal ceiling and every
// stream stay on the tracker's own head.
func (n *Network) anchorHead(h *common.EvmHead) *common.EvmHead {
	latest := n.cacheHead.Clamp(h.Latest, n.servedLatest.Load())
	if latest >= h.Latest {
		return h
	}
	a := *h
	c, pinned := indexer.Cursor{}, false
	if t := n.laneTracker(""); t != nil {
		c, pinned = t.View().PinAt(uint64(latest))
	}
	if !pinned {
		c = indexer.Cursor{Number: uint64(latest)}
	}
	a.Latest, a.LatestHash = latest, c.Hash
	a.Firm = min(a.Firm, latest)
	a.Finalized = min(a.Finalized, latest)
	return &a
}

// requestScope is whose heads req's Head counts: the upstreams its
// use-upstream selector matches that serve its method (spec §6.2).
func (n *Network) requestScope(ctx context.Context, req *common.NormalizedRequest) tipScope {
	var s tipScope
	if d := req.Directives(); d != nil {
		s.sel = d.UseUpstream
	}
	method, _ := req.Method()
	s.excluded = n.nonServers(ctx, method)
	return s
}

// headFor is the Head of scope s, read once: its selector lane's number
// facts and its tracker's view (spec §6.4, §6.5). A selector that forms
// no lane, or a method only some of its upstreams serve, takes the
// stateless pick of the facts and the selector lane's view, its Head
// capped at the block q of the scope's upstreams hold.
func (n *Network) headFor(s tipScope) *common.EvmHead {
	var h common.EvmHead
	var limit int64
	if l := n.laneFor(s.sel); l != nil && len(s.excluded) == 0 {
		h = *l.published()
	} else {
		h = n.computeHead(nil, s)
		limit, _ = n.laneHolders(s)
	}
	if t := n.laneTracker(s.sel); t != nil {
		viewHead(&h, t.View(), limit)
	}
	return &h
}

// viewHead sets h's view fields from v, its Head at most limit when that
// is set; Finalized never above the Head. A limit the pins do not hold
// (its upstreams' branch is not linked to them yet), or a guarantee cap
// below the pins (View.Capped), serves the number alone: no hash to check
// an answer against, and never a block above what the selector's or the
// guarantee's upstreams hold.
func viewHead(h *common.EvmHead, v evm.View, limit int64) {
	head, ok := v.Head()
	if !ok {
		n, capped := v.Capped()
		if !capped {
			h.Latest, h.LatestHash, h.Firm = 0, [32]byte{}, 0
			return
		}
		head = indexer.Cursor{Number: n}
	}
	if limit > 0 && uint64(limit) < head.Number {
		c, pinned := v.PinAt(uint64(limit))
		if !pinned {
			c = indexer.Cursor{Number: uint64(limit)}
		}
		head = c
	}
	h.Latest, h.LatestHash = int64(head.Number), head.Hash
	h.Firm = int64(min(v.Firm(), head.Number))
	if h.Finalized > h.Latest {
		h.Finalized = h.Latest
	}
}

// laneTracker is the chain tracker whose view selector sel's lane serves:
// its group's, or the network-wide one for "" and for a selector that
// forms no lane; nil on a network without trackers.
func (n *Network) laneTracker(sel string) *evm.ChainTracker {
	if sel == "" || n.chainTracker == nil {
		return n.chainTracker
	}
	if l := n.laneFor(sel); l != nil {
		return n.trackerOf(l)
	}
	return n.chainTracker
}

// trackerOf is lane l's chain tracker, made on first use.
func (n *Network) trackerOf(l *servedTipLane) *evm.ChainTracker {
	if l.key == "" || n.chainTracker == nil {
		return n.chainTracker
	}
	return evm.GroupTracker(n, l.key, l.selector, l.label)
}

// EvmLaneHead is the Head of the lane keyed groupKey ("" is the
// network-wide lane), materialized from selector on first use.
func (n *Network) EvmLaneHead(groupKey, selector string) common.EvmHead {
	if groupKey == "" {
		selector = ""
	}
	return *n.headFor(tipScope{sel: selector})
}

func (l *servedTipLane) published() *common.EvmHead {
	if h := l.head.Load(); h != nil {
		return h
	}
	return unknownHead
}

// headsComputed returns a channel closed after the head loop's next pass,
// for a reader whose selector forms no lane.
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

// headsChanged asks for every lane's Head to be computed again: a bench
// changed, the policy's eligible set changed, an upstream joined or its
// polls started or stopped failing. It never blocks.
func (n *Network) headsChanged() {
	n.markLanes(nil)
}

// headMoved asks for the Heads of the lanes u is in to be computed again:
// one of its head counters or its chain-identity cordon moved, which no
// other lane's ballot or ceiling counts. It never blocks.
func (n *Network) headMoved(u common.Upstream) {
	n.markLanes(u)
}

// markLanes marks every lane u is in (every lane for nil) dirty and wakes
// the head loop, whose next pass computes only the dirty lanes.
func (n *Network) markLanes(u common.Upstream) {
	n.servedTipAll.dirty.Store(true)
	n.servedTipLanes.Range(func(_, v any) bool {
		l := v.(*servedTipLane)
		if u == nil {
			l.dirty.Store(true)
		} else if m, _ := common.UpstreamMatchesSelector(l.selector, u); m {
			l.dirty.Store(true)
		}
		return true
	})
	select {
	case n.headWake <- struct{}{}:
	default:
	}
}

// startHeads computes every lane's Head now, so a network bootstrapped on
// upstreams with known heads serves its first request with a Head, then
// computes the dirty lanes on each wake (markLanes) and marks every lane
// each headRefresh until the app stops, and runs the cache-head anchor's
// poll (evm.CacheHeadAnchor.Run) as long. Once per network: its bootstrap
// can run again after it succeeded.
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
		if n.cacheHead != nil {
			n.bg.Go(func() { n.cacheHead.Run(n.appCtx) })
		}
		n.bg.Go(func() {
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			for {
				select {
				case <-n.appCtx.Done():
					return
				case <-ticker.C:
					n.headsChanged()
				case <-n.headWake:
					n.refreshDirtyHeads()
				}
			}
		})
	})
}

// watchHeadCounters makes every move of ups's head counters, by a poll, a
// push or another instance, and every change of its chain-identity cordon,
// which moves it in or out of the ceiling and the voters, mark its lanes
// dirty (spec §6.4); a move of its latest counter also wakes its lanes'
// trackers, whose Firm reads it (§6.5), and a cordon change makes every
// tracker read its voters again. The registry calls it once per upstream
// as it joins the network, which is itself a change of the voters.
func (n *Network) watchHeadCounters(ups *upstream.Upstream) {
	if p, ok := ups.EvmStatePoller().(*evm.EvmStatePoller); ok && !p.IsObjectNull() {
		p.OnLatestBlock(func(int64) {
			n.headMoved(ups)
			evm.HoldersMoved(n, ups)
		})
		p.OnFinalizedBlock(func(int64) { n.headMoved(ups) })
		p.OnChainIdCordonChanged(func() {
			n.headMoved(ups)
			if n.intake != nil {
				n.intake.chainIdCordonChanged()
			}
		})
	}
	n.headsChanged()
}

// refreshHeads computes and publishes every lane's Head now, then ends the
// pass. A panic on one lane leaves the others computed.
func (n *Network) refreshHeads() {
	defer n.endHeadPass()
	n.refreshLaneRecovered(&n.servedTipAll)
	n.servedTipLanes.Range(func(_, v any) bool {
		n.refreshLaneRecovered(v.(*servedTipLane))
		return true
	})
}

// refreshDirtyHeads is refreshHeads over the dirty lanes only. A lane is
// clean before its computation, so a change during it marks it again.
func (n *Network) refreshDirtyHeads() {
	defer n.endHeadPass()
	if n.servedTipAll.dirty.Swap(false) {
		n.refreshLaneRecovered(&n.servedTipAll)
	}
	n.servedTipLanes.Range(func(_, v any) bool {
		if l := v.(*servedTipLane); l.dirty.Swap(false) {
			n.refreshLaneRecovered(l)
		}
		return true
	})
}

// endHeadPass wakes the readers waiting on the pass (headsComputed).
func (n *Network) endHeadPass() {
	if c := n.headPass.Swap(nil); c != nil {
		close(*c)
	}
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

// refreshLane computes l's number facts and publishes them in one step,
// learns the lane's finality step from an advance of its Finalized, and
// starts the next step's refresh once it is due (dueFinalizedStep).
func (n *Network) refreshLane(l *servedTipLane) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.published()
	h := n.computeHead(l, tipScope{sel: l.selector})
	// A fall through an unknown Ceiling counts at its first known value.
	h.CeilingPeak, h.CeilingFellAt = max(old.CeilingPeak, h.Ceiling), old.CeilingFellAt
	if h.Ceiling > 0 && h.Ceiling < old.CeilingPeak && (old.Ceiling == 0 || h.Ceiling < old.Ceiling) {
		h.CeilingFellAt = time.Now()
	}
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	pick, _ := n.evmTipBallot(tipScope{sel: l.selector}, false, q, nil)
	t := n.trackerOf(l)
	var view int64
	if t != nil {
		if head, ok := t.View().Head(); ok {
			view = int64(head.Number)
		}
	}
	// The latest axis's lag is main's: the ballot's Freshest less the
	// served Head. A lane without read demand stops its clock while the
	// polls move the ballot, and its next read fetches a fresh head, so
	// it lags by nothing a client reads.
	var freshest int64
	if t != nil && t.ClockRuns() {
		freshest = pick.Freshest
	}
	if l.gauges == nil {
		l.gauges = n.bindLaneGauges(l.label)
	}
	l.gauges.latest.observe(view, freshest, l.latest.age())
	// The step's timing reads the first voter's report of a block: a lane
	// whose clock fetches about one voter a block has its q-th report a
	// block or more after it, and the view later still, while each
	// voter's finalized moves with its own head. A report more than W of
	// blocks above the view counts at that bound, so a far-ahead head
	// cannot spend the step's polls before the chain gets there.
	if pick.Max > 0 {
		sight := pick.Max
		if view > 0 {
			ahead := int64(1)
			if bt := n.EvmBlockTime(); bt > 0 {
				ahead = max(ahead, int64(health.FreshnessWindow(bt)/bt))
			}
			sight = min(sight, view+ahead)
		}
		if old.Finalized > 0 && h.Finalized > old.Finalized && sight >= h.Finalized {
			l.learnFinalizedStep(h.Finalized-old.Finalized, sight-h.Finalized)
		}
		n.dueFinalizedStep(l, h.Finalized, sight)
	}
	if h == *old {
		return
	}
	// A copy, so only a changed Head reaches the heap.
	pub := h
	l.head.Store(&pub)
}

// computeHead computes the number facts of scope s (spec §6.4):
// Finalized, Leader and the ceiling, against the lane's view's Head; l is
// its lane, nil for the stateless pick of a scope that forms none: no
// guard, no gauges, no anchor.
func (n *Network) computeHead(l *servedTipLane, s tipScope) common.EvmHead {
	var latest int64
	t := n.chainTracker
	if l != nil {
		// By the lane itself: a lane being made is not yet laneFor's.
		t = n.trackerOf(l)
	} else {
		t = n.laneTracker(s.sel)
	}
	if t != nil {
		if head, ok := t.View().Head(); ok {
			latest = int64(head.Number)
		}
	}
	h := common.EvmHead{Finalized: n.laneFinalized(l, s, latest)}
	h.Leader = n.laneLeader(s, latest)
	h.Ceiling, h.OldestReportAt = n.laneCeiling(s.sel)
	if l != nil && latest > 0 {
		l.latest.observe(latest)
	}
	return h
}

// laneFinalized is the Head's Finalized: the freshest finalized block at
// least the configured quorum of the lane's eligible voters already has
// (evmTipBallot, common.PickServedTip), guarded against a regression far
// below the live heads, clamped to every configured guarantee, held at the
// lane's finalized floor and, network-wide, at the finalized other
// instances served (holdFinalized, shareFinalized), then exported on the
// lane. latest is the lane's view's Head, which caps a held Finalized.
//
// The guard is in-process, bounded and one-directional (it can only hold the
// tip, and only briefly), and it cannot produce a block that no live upstream
// reports in the very same evaluation. The finalized floors hold a block only
// while a voter of the same evaluation reports it, the shared one included.
// Nothing else is persisted and nothing is predicted, so no inherited or
// rogue value can pin, freeze or poison the result
// (networks_served_tip_invariants_test.go pins those outcomes against
// the frozen-tip regression).
func (n *Network) laneFinalized(l *servedTipLane, s tipScope, latest int64) int64 {
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	pick, live := n.evmTipBallot(s, true, q, nil)
	tip := pick.Tip

	// Regression guard: "finalized" cannot drop far below the corroborated
	// live head in one evaluation. Runs BEFORE the guaranteed-method floor,
	// which is a deliberate operator-configured clamp the guard must not
	// fight.
	if l != nil {
		tip = n.guardServedTipRegression("finalized", l.label, &l.finalized, pick, live)
	}

	// Capability guarantee (#855): the served finalized must never exceed
	// what any configured guarantee SUBSET can serve — a method's
	// supporting upstreams (guaranteedMethods) or an operator-named group
	// (guaranteedFor).
	backing := pick.Max
	if tip > 0 {
		floor, subsets := n.guaranteedFloor(s.sel, true)
		if floor > 0 && floor < tip {
			tip = floor
		}
		if subsets > 0 {
			backing = min(backing, subsets)
		}
	}

	// Finalized floor (#1353): a change of voters, in the lane's ballot or a
	// guarantee subset's, cannot pull "finalized" below a block this lane,
	// or for the network-wide lane any instance, served while a voter of
	// each still reports it. A floor holds only drops within the regression
	// tolerance, so a far-ahead rogue head cannot pin it, and a held value
	// never exceeds Latest: per voter finalized <= latest, so the pick keeps
	// that order and the floor must too. laneFinalized of a lane runs only
	// under l.mu (refreshLane), so the floor and this evaluation's backing
	// are read together.
	if l != nil {
		computed := tip
		tolerance := n.finalizedFloorTolerance()
		held := l.holdFinalized(tip, backing, tolerance)
		if l == &n.servedTipAll {
			held = n.shareFinalized(held, tip, backing, tolerance)
		}
		if held > tip && latest > 0 && held > latest {
			held = max(tip, latest)
		}
		tip = held
		if prev := l.published().Finalized; prev > 0 && tip > prev && pick.Inputs < q.Of(n.voters(s, voterQuery{kind: numberVote}).count()) {
			telemetry.MetricFinalizedUnderQuorumTotal.WithLabelValues(n.projectId, n.Label()).Inc()
		}
		if l == &n.servedTipAll {
			n.publishFinalized(computed)
		}
		// Watchdog anchor: when did this process last see the served value
		// change.
		if tip > 0 {
			l.finalized.observe(tip)
		}
		if l.gauges == nil {
			l.gauges = n.bindLaneGauges(l.label)
		}
		l.gauges.finalized.observe(tip, pick.Freshest, l.finalized.age())
	}
	return tip
}

// laneHolders is what Firm and the Head read of scope s (spec §6.5):
// holders, the q-th highest block its number voters hold, by their head
// counters (any instance, any path) or by this instance's by-number
// answers from them (heldAtLeast); and guarantee, the cap the Head never
// passes, 0 while none constrains it: the lowest majority of the guarantee
// subsets and, on a scope whose voters are all static caps, the highest
// cap (no voter serves a block above it, rule 4).
func (n *Network) laneHolders(s tipScope) (holders, guarantee int64) {
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	var buf [16]int64
	heads := buf[:0]
	vs := n.voters(s, voterQuery{kind: numberVote})
	vs.each(func(u common.Upstream, head int64) bool {
		heads = append(heads, max(head, n.heldAtLeast(u.Id())))
		return true
	})
	guarantee, _ = n.guaranteedFloor(s.sel, false)
	if len(heads) > 0 && !vs.live() {
		if c := slices.Max(heads); guarantee == 0 || c < guarantee {
			guarantee = c
		}
	}
	return common.PickServedTip(heads, q).Tip, guarantee
}

// guaranteeLaggers are the lane's number voters in a guarantee subset
// whose head is at or below floor (guaranteedFloor): only their newer
// heads raise it.
func (n *Network) guaranteeLaggers(sel string, floor int64) []string {
	if floor <= 0 || n.cfg == nil || n.cfg.Evm == nil || n.cfg.Evm.ServedTip == nil {
		return nil
	}
	st := n.cfg.Evm.ServedTip
	member := func(u common.Upstream) bool {
		for _, g := range st.GuaranteedFor {
			if m, _ := common.UpstreamMatchesSelector(g, u); m {
				return true
			}
		}
		for _, m := range st.GuaranteedMethods {
			if handle, _ := u.ShouldHandleMethod(m); handle {
				return true
			}
		}
		return false
	}
	var ids []string
	n.voters(tipScope{sel: sel}, voterQuery{kind: numberVote}).each(func(u common.Upstream, head int64) bool {
		if max(head, n.heldAtLeast(u.Id())) <= floor && member(u) {
			ids = append(ids, u.Id())
		}
		return true
	})
	return ids
}

// laneLeader is the highest number voter of scope s (voterSet) whose head
// is within latest + maxRetryableBlockDistance (#1154): a head beyond reach
// of the served one never leads. nil while latest is unknown.
func (n *Network) laneLeader(s tipScope, latest int64) common.Upstream {
	if latest <= 0 {
		return nil
	}
	reach := latest + n.maxRetryableBlockDistance()
	var leader common.Upstream
	var top int64
	vs := n.voters(s, voterQuery{kind: numberVote})
	vs.each(func(u common.Upstream, head int64) bool {
		if head <= reach && head > top {
			leader, top = u, head
		}
		return true
	})
	return leader
}

// laneCeiling is the highest effective latest head across the lane's
// counted upstreams (countedUpstreams), regardless of selection policy,
// tier or syncing state, since any of them can end up serving a request
// (policy order, the fallback escape, syncing upstreams without
// skipWhenSyncing). It uses latest heads whatever emptyResultConfidence
// says: that setting decides how an empty answer is treated, not whether
// an upstream can have the block. 0 (unknown) when the lane has no counted
// upstream or any one's head is unknown. oldest is when the oldest counted
// report's evidence was taken (LatestReportAt), zero when one has none.
func (n *Network) laneCeiling(sel string) (ceiling int64, oldest time.Time) {
	first := true
	for u := range n.countedUpstreams(sel) {
		sp := u.EvmStatePoller()
		if sp == nil {
			return 0, time.Time{}
		}
		blk, _ := evmTipObservation(u, false)
		if blk <= 0 {
			return 0, time.Time{}
		}
		ceiling = max(ceiling, blk)
		var at time.Time
		if !sp.IsObjectNull() {
			at = sp.LatestReportAt()
		}
		if first || at.Before(oldest) {
			oldest, first = at, false
		}
	}
	return ceiling, oldest
}

// countedUpstreams are the upstreams selector sel's lane ceiling counts:
// every registered one within sel, minus those cordoned for answering for
// another chain and those whose polls are failing (spec §6.1, §6.4;
// networkIntake.pollsFailing): one failed poll, a 429 included, keeps an
// upstream counted, so a block it reports is never called future.
func (n *Network) countedUpstreams(sel string) iter.Seq[*upstream.Upstream] {
	// Kept this small so it inlines, as tipCandidates.
	return func(yield func(*upstream.Upstream) bool) { n.walkCounted(sel, yield) }
}

func (n *Network) walkCounted(sel string, yield func(*upstream.Upstream) bool) {
	if n.upstreamsRegistry == nil {
		return
	}
	for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), n.networkId) {
		if sel != "" {
			if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
				continue
			}
		}
		if chainIdCordoned(u) {
			continue
		}
		if n.intake != nil && n.intake.pollsFailing(u.Id()) {
			continue
		}
		if !yield(u) {
			return
		}
	}
}

// EvmAskStale Asks, with a number need at 1 bt (spec §6.8), each of
// selector's counted upstreams whose report is 1 bt old or older, every
// one before the first block time sample, waits for their answers, at most
// 1 bt or until ctx ends, and returns the lane's ceiling and oldest report
// after them (spec §6.4: no future on a stale report). The age filter
// saves the request path an Ask per fresh upstream; the Ask paces fetches.
func (n *Network) EvmAskStale(ctx context.Context, selector string) (ceiling int64, oldestReportAt time.Time) {
	bt, cold := n.blockTimeOrCold(), n.EvmBlockTime() <= 0
	now := time.Now()
	var stale []common.EvmStatePoller
	for u := range n.countedUpstreams(selector) {
		if sp := u.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
			if cold || now.Sub(sp.LatestReportAt()) >= bt {
				stale = append(stale, sp)
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, bt)
	defer cancel()
	askAll(ctx, stale, bt, common.AskRefusal)
	return n.laneCeiling(selector)
}

// EvmAwaitCeiling waits for selector sel's lane ceiling to reach block, at
// most W or until ctx ends, Asking the lane's number voters below block
// at once and every bt after (spec §6.4 near future), each after the
// answer it last took from that voter: a tick never takes the fetch the
// one before took, so it sees each new head within a bt. The Ask is
// single-flight at pace bt, so many waiting requests fetch each voter
// about once per bt. It returns the ceiling and oldest report it last saw.
func (n *Network) EvmAwaitCeiling(ctx context.Context, sel string, block int64) (ceiling int64, oldestReportAt time.Time) {
	bt := n.blockTimeOrCold()
	wait, cancel := context.WithTimeout(ctx, health.FreshnessWindow(n.EvmBlockTime()))
	defer cancel()
	taken := map[string]time.Time{}
	ask := func() {
		n.voters(tipScope{sel: sel}, voterQuery{kind: numberVote}).each(func(u common.Upstream, head int64) bool {
			if head < block {
				if sp := u.(common.EvmUpstream).EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
					if at := sp.AskBackground(common.HeadNeedNumber, bt, taken[u.Id()], common.AskRefusal).At; !at.IsZero() {
						taken[u.Id()] = at
					}
				}
			}
			return true
		})
	}
	ask()
	tick := time.NewTicker(bt)
	defer tick.Stop()
	for {
		computed := n.headsComputed()
		h := n.headFor(tipScope{sel: sel})
		if h.Ceiling >= block {
			return h.Ceiling, h.OldestReportAt
		}
		select {
		case <-computed:
		case <-tick.C:
			ask()
		case <-wait.Done():
			h = n.headFor(tipScope{sel: sel})
			return h.Ceiling, h.OldestReportAt
		}
	}
}

// EvmAskBand Asks, in the background, with a number need at 1 bt, each
// upstream selector's lane ceiling counts whose head is below block,
// voters first (spec §6.4 band): a syncing upstream votes for no Latest
// yet may hold the block, and the band's retry orders upstreams by these
// heads.
func (n *Network) EvmAskBand(selector string, block int64) {
	bt := n.blockTimeOrCold()
	for _, sp := range n.bandPollers(selector, block) {
		sp.AskBackground(common.HeadNeedNumber, bt, time.Time{}, common.AskRefusal)
	}
}

// awaitBandAsk joins the band's Ask of selector sel's lane for block
// (EvmAskBand, AskReaching) and waits for its answers, at most W (spec
// §6.4): a poll over a slow link takes longer than 1 bt, and the band's
// retry orders upstreams on the heads it brings. The first answer that
// reaches block ends the wait: the retry has a holder, and one slow
// upstream never holds it back (rule 2). The other fetches run on.
func (n *Network) awaitBandAsk(ctx context.Context, sel string, block int64) {
	ctx, cancel := context.WithTimeout(ctx, health.FreshnessWindow(n.EvmBlockTime()))
	defer cancel()
	bt := n.blockTimeOrCold()
	var wg sync.WaitGroup
	for _, sp := range n.bandPollers(sel, block) {
		wg.Go(func() {
			if a := sp.AskReaching(ctx, block, bt, common.AskRefusal); a.Err == nil && a.Block >= block {
				cancel()
			}
		})
	}
	wg.Wait()
}

// awaitViewOnto reports whether c, an upstream's block at req's Head
// number on another branch, is pinned at or below the Head of req's lane
// view now or within one bt (spec §6.5, §7 family 2). An upstream may show
// a reorg before this instance's tracker applied it; once the view moved,
// c is the chain's block there and no upstream serves the Head req took
// any more. A minority fork's block is never pinned: it stays a miss.
func (n *Network) awaitViewOnto(ctx context.Context, req *common.NormalizedRequest, c indexer.Cursor) bool {
	sel := ""
	if d := req.Directives(); d != nil {
		sel = d.UseUpstream
	}
	t := n.laneTracker(sel)
	if t == nil {
		return false
	}
	timer := time.NewTimer(n.blockTimeOrCold())
	defer timer.Stop()
	for {
		v := t.View()
		if head, ok := v.Head(); ok && c.Number <= head.Number && v.Pinned(c) {
			return true
		}
		select {
		case <-v.Changed():
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// bandPollers are the pollers of the upstreams sel's lane ceiling counts
// whose head is below block, number voters (voterSet) first.
func (n *Network) bandPollers(sel string, block int64) []common.EvmStatePoller {
	ups := slices.Collect(n.countedUpstreams(sel))
	s := n.voters(tipScope{sel: sel}, voterQuery{kind: numberVote})
	voters := s.list()
	out := make([]common.EvmStatePoller, 0, len(ups))
	for _, first := range [2]bool{true, false} {
		for _, u := range ups {
			if slices.ContainsFunc(voters, func(v common.Upstream) bool { return v.Id() == u.Id() }) != first {
				continue
			}
			if head, _ := evmTipObservation(u, false); head >= block {
				continue
			}
			if sp := u.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
				out = append(out, sp)
			}
		}
	}
	return out
}

// askAll Asks each of pollers with a number need at pace for reason, at
// once, and waits for every answer or ctx's end.
func askAll(ctx context.Context, pollers []common.EvmStatePoller, pace time.Duration, reason common.AskReason) {
	var wg sync.WaitGroup
	for _, sp := range pollers {
		wg.Go(func() { sp.Ask(ctx, common.HeadNeedNumber, pace, time.Time{}, reason) })
	}
	wg.Wait()
}

// blockTimeOrCold is the network's block time, or the cold-start one
// before its first sample (spec §6.7).
func (n *Network) blockTimeOrCold() time.Duration {
	if bt := n.EvmBlockTime(); bt > 0 {
		return bt
	}
	return health.ColdBlockTime
}

// byHashFamily is the §7 by-hash family, eth_getLogs by blockHash included
// (spec §6.4): each method's paths to the number and the hash of the block
// its answer names. debug_traceTransaction is not here: its answer names
// no block.
var byHashFamily = map[string]struct{ number, hash []interface{} }{
	"eth_getBlockByHash":        {[]interface{}{"number"}, []interface{}{"hash"}},
	"eth_getTransactionByHash":  {[]interface{}{"blockNumber"}, []interface{}{"blockHash"}},
	"eth_getTransactionReceipt": {[]interface{}{"blockNumber"}, []interface{}{"blockHash"}},
	"trace_transaction":         {[]interface{}{0, "blockNumber"}, []interface{}{0, "blockHash"}},
	"eth_getLogs":               {[]interface{}{0, "blockNumber"}, []interface{}{0, "blockHash"}},
}

// holdByHash returns the answer to send for resp, an answer to req, from
// an upstream or the cache (spec §6.4). A by-hash answer from a block
// above its lane's Latest, the view's Head, proves that block exists:
// the wait is the lane's demand, and the block clock takes the proof
// (ChainTracker.Proven), fetching the block's head at once as its probe
// for it, unless a clock fetch is out: from the upstream that answered,
// or for a cache answer the lane's first voter whose counter holds the
// block, when it is a free voter, else from the clock's fastest free
// voter. That fetch is the block's one: it sights the block, so the clock
// fetches the next one, and a header that serves only at q holders (a
// mismatch, §6.5) gets them from the clock's holder fetch (§6.8). The
// answer is sent once the view includes its block, and its upstream then
// counts as the block's holder (noteHeld); it is null once the view pins
// another block at that number, or when W passed, or ctx ended, with
// neither. Each wait counts its outcome. held: resp was replaced by null.
func (n *Network) holdByHash(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) (out *common.NormalizedResponse, held bool) {
	bn, bh := byHashBlock(ctx, req, resp)
	if bn <= 0 {
		return resp, false
	}
	sel := ""
	if d := req.Directives(); d != nil {
		sel = d.UseUpstream
	}
	t := n.laneTracker(sel)
	if t == nil {
		return resp, false
	}
	var hash [32]byte
	named := false
	if len(bh) == 66 {
		if b, err := hex.DecodeString(bh[2:]); err == nil {
			named = copy(hash[:], b) == len(hash)
		}
	}
	// includes is whether the view holds the block: Latest at or above it
	// and its pin there, if any, the answer's; off: another block is
	// pinned there.
	includes := func() (in, off bool, moved <-chan struct{}) {
		moved = t.Changed()
		if h := n.headFor(tipScope{sel: sel}); h.Latest < bn {
			return false, false, moved
		}
		if named {
			if c, pinned := t.View().PinAt(uint64(bn)); pinned && c.Hash != hash {
				return false, true, moved
			}
		}
		return true, false, moved
	}
	if h := n.headFor(tipScope{sel: sel}); h.Latest <= 0 || bn <= h.Latest {
		return resp, false
	}
	prover, _ := resp.Upstream().(common.EvmUpstream)
	fromUpstream := prover != nil && !resp.SentAt().IsZero()
	by := ""
	if fromUpstream {
		by = prover.Id()
	} else {
		by = n.firstHolder(sel, bn)
	}
	t.Proven(uint64(bn), by)
	asks, cancel := context.WithTimeout(ctx, health.FreshnessWindow(n.EvmBlockTime()))
	defer cancel()
	for {
		in, off, moved := includes()
		switch {
		case in:
			if fromUpstream {
				n.noteHeld(prover, bn)
			}
			n.countHeadRefusal("held_served")
			return resp, false
		case off:
			n.countHeadRefusal("held_null")
			return nullAnswer(req, resp)
		}
		select {
		case <-moved:
			continue
		case <-asks.Done():
		}
		// The clock's fetch returned once intake had its header; the
		// vote may still wait in the tracker's mailbox.
		t.Sync(ctx)
		if in, _, _ := includes(); in {
			continue
		}
		n.countHeadRefusal("held_null")
		return nullAnswer(req, resp)
	}
}

// nullAnswer is a null answer to req, held instead of resp; resp itself
// when the null cannot be built.
func nullAnswer(req *common.NormalizedRequest, resp *common.NormalizedResponse) (*common.NormalizedResponse, bool) {
	jrr, err := common.NewJsonRpcResponse(req.ID(), nil, nil)
	if err != nil {
		return resp, false
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), true
}

// byHashBlock is the number and hash of the block resp names when req is
// of the by-hash family (byHashFamily), else 0; the hash "" when resp
// names none.
func byHashBlock(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) (int64, string) {
	if resp == nil || resp.IsObjectNull(ctx) || resp.IsResultEmptyish(ctx) {
		return 0, ""
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return 0, ""
	}
	jrq.RLock()
	paths, ok := byHashFamily[jrq.Method]
	if ok && jrq.Method == "eth_getLogs" {
		var f map[string]interface{}
		if len(jrq.Params) > 0 {
			f, _ = jrq.Params[0].(map[string]interface{})
		}
		_, ok = f["blockHash"]
	}
	jrq.RUnlock()
	if !ok {
		return 0, ""
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return 0, ""
	}
	s, err := jrr.PeekStringByPath(ctx, paths.number...)
	if err != nil {
		return 0, ""
	}
	bn, err := common.HexToInt64(s)
	if err != nil {
		return 0, ""
	}
	bh, _ := jrr.PeekStringByPath(ctx, paths.hash...)
	return bn, bh
}
