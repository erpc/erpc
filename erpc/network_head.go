package erpc

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
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
	return *n.requestHead(ctx, req)
}

// requestHead is the Head req took once from its lane; every later reader
// of req gets that value, so retries, hedges, the cache key and
// interpolation all see the same Latest. A request naming the finalized
// tag takes it after the lane's finalized refresh (spec §6.4 Finality): a
// Finalized up to one full poll old would serve older blocks than the tag
// gets live. A request answering Latest (evm.AnswersLatest) takes it after
// the latest refresh (refreshLatest, §6.8), before the cache: a cache hit
// is pinned to that Head too. A request with X-ERPC-Min-Block first waits
// for its lane's Latest to reach it (awaitMinBlock, streams spec §8), so
// its tags, cache key and every retry read the waited Head; that wait and
// the refresh after it share one W.
func (n *Network) requestHead(ctx context.Context, req *common.NormalizedRequest) *common.EvmHead {
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
	if block := minBlockOf(ctx, req); block > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, health.FreshnessWindow(n.EvmBlockTime()))
		defer cancel()
		n.awaitMinBlock(ctx, sel, block)
	}
	switch {
	case evm.NamesFinalizedTag(ctx, req):
		n.awaitFinalizedRefresh(ctx, sel)
	case evm.AnswersLatest(ctx, n, req):
		n.refreshLatest(ctx, sel)
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
// lane leaves the others computed, and the pass ends for its waiters. The
// stateless heads of selectors that form no lane are picked again after
// each pass.
func (n *Network) refreshHeads() {
	defer func() {
		n.statelessMu.Lock()
		n.statelessGen.Add(1)
		n.statelessHeads.Clear()
		n.statelessMu.Unlock()
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
	// A fall through an unknown Ceiling counts at its first known value.
	h.CeilingPeak, h.CeilingFellAt = max(old.CeilingPeak, h.Ceiling), old.CeilingFellAt
	if h.Ceiling > 0 && h.Ceiling < old.CeilingPeak && (old.Ceiling == 0 || h.Ceiling < old.Ceiling) {
		h.CeilingFellAt = time.Now()
	}
	left, joined := ballotChange(l.voters, l.ballot)
	l.voters, l.ballot = l.ballot, l.voters
	if h == *old {
		return
	}
	// A Read that sees the new Latest finds when it reached each block.
	if h.Latest != old.Latest {
		l.reached.note(h.Latest, time.Now(), n.latestReachedKept())
	}
	l.head.Store(&h)
	if h.Latest != old.Latest {
		// Latest 0 is unknown, not a served number: eth_blockNumber then
		// errors. A fall through unknown counts at its first known value.
		sent := l.sent.Load()
		switch below := h.Latest > 0 && uint64(h.Latest) < sent; {
		case !below:
			if h.Latest > 0 {
				l.fallen = false
			}
		case l.fallen || old.Latest > 0 && h.Latest > old.Latest:
		case left != "" || joined != "":
			// A voter that left lowers the q-th highest number, and one
			// that joined raises q under a majority quorum, with no
			// upstream's head falling: the ballot changed, the chain did not
			// (spec §6.4, §17).
			common.TransitionLog(n.logger, zerolog.InfoLevel, n.Label(), "ballot").
				Str("lane", l.label).Str("left", left).Str("joined", joined).Int64("latest", h.Latest).Int64("previous", old.Latest).Uint64("sent", sent).
				Msg("lane's Latest fell below a block this instance sent as its ballot's voters changed")
		default:
			l.fallen = true
			common.ViolateInvariant(n.logger, n.projectId, n.Label(), common.InvariantLatestRegressed).
				Str("lane", l.label).Int64("latest", h.Latest).Int64("previous", old.Latest).Uint64("sent", sent).
				Msg("lane's Latest fell below a block this instance sent")
		}
		if c := l.moved.Swap(nil); c != nil {
			close(*c)
		}
		evm.LatestChanged(n, l.key)
	}
}

// ballotChange is the first voter of prev that cur no longer counts and
// the first of cur that prev did not, "" each when there is none.
func ballotChange(prev, cur []string) (left, joined string) {
	for _, id := range prev {
		if !slices.Contains(cur, id) {
			left = id
			break
		}
	}
	for _, id := range cur {
		if !slices.Contains(prev, id) {
			joined = id
			break
		}
	}
	return left, joined
}

// computeHead computes the Head of selector sel (spec §6.4); l is its lane,
// nil for the stateless pick of a selector that forms none: no guard, no
// gauges, no anchor.
func (n *Network) computeHead(l *servedTipLane, sel string) common.EvmHead {
	h := common.EvmHead{
		Latest: n.laneTip(l, sel, false, 0),
	}
	h.Finalized = n.laneTip(l, sel, true, h.Latest)
	h.Leader = n.laneLeader(sel, h.Latest)
	h.Ceiling, h.OldestReportAt = n.laneCeiling(sel)
	return h
}

// laneTip is one axis of the Head: the freshest block at least the
// configured quorum of the lane's eligible voters already has (evmTipBallot,
// common.PickServedTip), guarded against a regression far below the live
// heads, clamped to every configured guarantee, held at the lane's finalized
// floor and, network-wide, at the finalized other instances served
// (finalized only, holdFinalized, shareFinalized), then exported on the lane.
// latest is the lane's Latest of the same evaluation, which caps a held
// Finalized; unused on the latest axis.
//
// The guard is in-process, bounded and one-directional (it can only hold the
// tip, and only briefly), and it cannot produce a block that no live upstream
// reports in the very same evaluation. The finalized floors hold a block only
// while a voter of the same evaluation reports it, the shared one included.
// Nothing else is persisted and nothing is predicted, so no inherited or
// rogue value can pin, freeze or poison the result
// (networks_served_tip_invariants_test.go pins those outcomes against
// the frozen-tip regression).
func (n *Network) laneTip(l *servedTipLane, sel string, finalized bool, latest int64) int64 {
	axis := "latest"
	if finalized {
		axis = "finalized"
	}
	var q common.ServedTipQuorum
	if n.cfg != nil {
		q = n.cfg.Evm.ServedTipQuorum()
	}
	var voters *[]string
	if l != nil && !finalized {
		voters = &l.ballot
	}
	pick, live, ranks := n.evmTipBallot(sel, finalized, q, nil, voters)
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
	backing := pick.Max
	if tip > 0 {
		floor, subsets := n.guaranteedFloor(sel, finalized)
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
	// that order and the floor must too. laneTip of a lane runs only under
	// l.mu (refreshLane), so the floor and this evaluation's backing are
	// read together.
	if finalized && l != nil {
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
		if l == &n.servedTipAll {
			n.publishFinalized(computed)
		}
	}

	// Watchdog anchor: when did this process last see the served value change.
	if l != nil {
		if tip > 0 {
			anchor.observe(tip)
		}
		n.observeServedTipMetrics(axis, l.label, tip, pick.Freshest, anchor.age())
		if !finalized {
			for rank, voters := range ranks {
				telemetry.GaugeHandle(telemetry.MetricServedTipVoters, n.projectId, n.Label(), l.label, tipRankLabels[rank]).Set(float64(voters))
			}
			quorum := 0
			if pick.Inputs > 0 {
				quorum = q.Of(pick.Inputs)
			}
			telemetry.GaugeHandle(telemetry.MetricServedTipQuorum, n.projectId, n.Label(), l.label).Set(float64(quorum))
		}
	}
	return tip
}

// laneLeader is the highest number voter of the lane (voterSet) whose head
// is within latest + maxRetryableBlockDistance (#1154): a head beyond reach
// of the served one never leads. nil while latest is unknown.
func (n *Network) laneLeader(sel string, latest int64) common.Upstream {
	if latest <= 0 {
		return nil
	}
	reach := latest + n.maxRetryableBlockDistance()
	var leader common.Upstream
	var top int64
	s := n.voters(sel, voterQuery{kind: numberVote})
	s.each(func(u common.Upstream, head int64) bool {
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
	ups := n.countedUpstreams(sel)
	for i, u := range ups {
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
		if i == 0 || at.Before(oldest) {
			oldest = at
		}
	}
	return ceiling, oldest
}

// countedUpstreams are the upstreams selector sel's lane ceiling counts:
// every registered one within sel, minus those cordoned for answering for
// another chain and those whose polls are failing (spec §6.1, §6.4;
// networkIntake.pollsFailing): one failed poll, a 429 included, keeps an
// upstream counted, so a block it reports is never called future.
func (n *Network) countedUpstreams(sel string) []*upstream.Upstream {
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
		if n.intake != nil && n.intake.pollsFailing(u.Id()) {
			continue
		}
		out = append(out, u)
	}
	return out
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
	for _, u := range n.countedUpstreams(selector) {
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
// at once and every bt after (spec §6.4 near future). The Ask is
// single-flight at pace bt, so many waiting requests fetch each voter at
// most once per bt. It returns the ceiling and oldest report it last saw.
func (n *Network) EvmAwaitCeiling(ctx context.Context, sel string, block int64) (ceiling int64, oldestReportAt time.Time) {
	bt := n.blockTimeOrCold()
	wait, cancel := context.WithTimeout(ctx, health.FreshnessWindow(n.EvmBlockTime()))
	defer cancel()
	ask := func() {
		n.voters(sel, voterQuery{kind: numberVote}).each(func(u common.Upstream, head int64) bool {
			if head < block {
				if sp := u.(common.EvmUpstream).EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
					sp.AskBackground(common.HeadNeedNumber, bt, common.AskRefusal)
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
		h := n.headFor(sel)
		if h.Ceiling >= block {
			return h.Ceiling, h.OldestReportAt
		}
		select {
		case <-computed:
		case <-tick.C:
			ask()
		case <-wait.Done():
			h = n.headFor(sel)
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
		sp.AskBackground(common.HeadNeedNumber, bt, common.AskRefusal)
	}
}

// awaitBandAsk joins the band's Ask of selector sel's lane for block
// (EvmAskBand, AskReaching) and waits for its answers, at most W (spec
// §6.4): a poll over a slow link takes longer than 1 bt, and the band's
// retry orders upstreams on the heads it brings.
func (n *Network) awaitBandAsk(ctx context.Context, sel string, block int64) {
	ctx, cancel := context.WithTimeout(ctx, health.FreshnessWindow(n.EvmBlockTime()))
	defer cancel()
	bt := n.blockTimeOrCold()
	var wg sync.WaitGroup
	for _, sp := range n.bandPollers(sel, block) {
		wg.Go(func() { sp.AskReaching(ctx, block, bt, common.AskRefusal) })
	}
	wg.Wait()
}

// bandPollers are the pollers of the upstreams sel's lane ceiling counts
// whose head is below block, number voters (voterSet) first.
func (n *Network) bandPollers(sel string, block int64) []common.EvmStatePoller {
	ups := n.countedUpstreams(sel)
	s := n.voters(sel, voterQuery{kind: numberVote})
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
		wg.Go(func() { sp.Ask(ctx, common.HeadNeedNumber, pace, reason) })
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
// (spec §6.4): each method's path to the block number in its answer.
// debug_traceTransaction is not here: its answer names no block.
var byHashFamily = map[string][]interface{}{
	"eth_getBlockByHash":        {"number"},
	"eth_getTransactionByHash":  {"blockNumber"},
	"eth_getTransactionReceipt": {"blockNumber"},
	"trace_transaction":         {0, "blockNumber"},
	"eth_getLogs":               {0, "blockNumber"},
}

// holdByHash returns the answer to send for resp, an answer to req, from
// an upstream or the cache (spec §6.4): a by-hash answer from a block
// above its lane's Latest waits up to 1 bt while the voters Latest lacks
// are Asked for a head fetched after the wait began (AskReaching: a
// counter fresher than 1 bt is no evidence their nodes lack the block),
// then is sent if Latest includes the block, else it is null, as it is
// when ctx ends first. Each wait counts its outcome. held: resp was
// replaced by null.
func (n *Network) holdByHash(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) (out *common.NormalizedResponse, held bool) {
	bn := byHashBlock(ctx, req, resp)
	if bn <= 0 {
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
	bt := n.blockTimeOrCold()
	wait, cancel := context.WithTimeout(ctx, bt)
	defer cancel()
	for _, sp := range n.lackingVoters(sel, bn) {
		go sp.AskReaching(wait, bn, bt, common.AskRefusal)
	}
	for at < bn {
		select {
		case <-moved:
		case <-wait.Done():
			n.countHeadRefusal("held_null")
			return nullAnswer(req, resp)
		}
		at, moved = latest()
	}
	n.countHeadRefusal("held_served")
	return resp, false
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

// byHashBlock is the block number resp names when req is of the by-hash
// family (byHashFamily), else 0.
func byHashBlock(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) int64 {
	if resp == nil || resp.IsObjectNull(ctx) || resp.IsResultEmptyish(ctx) {
		return 0
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return 0
	}
	jrq.RLock()
	path, ok := byHashFamily[jrq.Method]
	if ok && jrq.Method == "eth_getLogs" {
		var f map[string]interface{}
		if len(jrq.Params) > 0 {
			f, _ = jrq.Params[0].(map[string]interface{})
		}
		_, ok = f["blockHash"]
	}
	jrq.RUnlock()
	if !ok {
		return 0
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return 0
	}
	s, err := jrr.PeekStringByPath(ctx, path...)
	if err != nil {
		return 0
	}
	bn, err := common.HexToInt64(s)
	if err != nil {
		return 0
	}
	return bn
}
