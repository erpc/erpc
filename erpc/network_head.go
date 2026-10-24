package erpc

import (
	"context"
	"fmt"
	"runtime/debug"
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
	h.Ceiling, h.OldestReportAt = n.laneCeiling(sel)
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
// counted upstreams (countedUpstreams), regardless of selection policy,
// tier or syncing state, since any of them can end up serving a request
// (policy order, the fallback escape, syncing upstreams without
// skipWhenSyncing). It uses latest heads whatever emptyResultConfidence
// says: that setting decides how an empty answer is treated, not whether
// an upstream can have the block. 0 (unknown) when the lane has no counted
// upstream or any one's head is unknown. oldest is the local receive time
// of the oldest counted report, zero when one has none.
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
			_, at, _ = sp.LatestReport()
		}
		if i == 0 || at.Before(oldest) {
			oldest = at
		}
	}
	return ceiling, oldest
}

// countedUpstreams are the upstreams selector sel's lane ceiling counts:
// every registered one within sel, minus those cordoned for answering for
// another chain and those whose newest poll on this instance failed
// (spec §6.1, §6.4).
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
		sp := u.EvmStatePoller()
		if c, ok := sp.(interface{ ChainIdCordoned() bool }); ok && c.ChainIdCordoned() {
			continue
		}
		if sp != nil && !sp.IsObjectNull() {
			if _, _, live := sp.LatestReport(); !live {
				continue
			}
		}
		out = append(out, u)
	}
	return out
}

// EvmAskStale Asks each of selector's counted upstreams whose report is
// 1 bt old or older, waits for those polls, at most 1 bt or until ctx
// ends, and returns the lane's ceiling and oldest report after them (spec
// §6.4: no future on a stale report).
func (n *Network) EvmAskStale(ctx context.Context, selector string) (ceiling int64, oldestReportAt time.Time) {
	bt := n.blockTimeOrCold()
	now := time.Now()
	var stale []common.EvmUpstream
	for _, u := range n.countedUpstreams(selector) {
		if sp := u.EvmStatePoller(); sp != nil && !sp.IsObjectNull() {
			if _, at, _ := sp.LatestReport(); now.Sub(at) >= bt {
				stale = append(stale, u)
			}
		}
	}
	n.awaitPolls(ctx, n.ask(selector, stale))
	return n.laneCeiling(selector)
}

// EvmAskBand Asks, in the background, with a number need, each upstream
// selector's lane ceiling counts whose head is below block, voters first
// (spec §6.4 band): a syncing upstream votes for no Latest yet may hold
// the block, and the band's retry orders upstreams by these heads.
func (n *Network) EvmAskBand(selector string, block int64) {
	n.askBand(selector, block)
}

// askBand is EvmAskBand, returning a channel per asked upstream, closed
// when its poll ends.
func (n *Network) askBand(sel string, block int64) []<-chan struct{} {
	ups := n.countedUpstreams(sel)
	targets := make([]common.EvmUpstream, 0, len(ups))
	for _, voters := range [2]bool{true, false} {
		for _, u := range ups {
			if _, _, ok := tipVoterRank(u, false, n.metricsTracker); ok == voters {
				targets = append(targets, u)
			}
		}
	}
	return n.askBelow(sel, block, targets)
}

// askBelow Asks each of targets whose head is below block, in order (ask).
func (n *Network) askBelow(sel string, block int64, targets []common.EvmUpstream) []<-chan struct{} {
	below := targets[:0:0]
	for _, u := range targets {
		if head, _ := evmTipObservation(u, false); head < block {
			below = append(below, u)
		}
	}
	return n.ask(sel, below)
}

// ask polls each of targets, in order: a poll debounced by this
// instance's last fetch of it, on the network's context bounded by W, so
// no caller's end fails it for the others. One poll per upstream is in
// flight at a time; a caller finding one joins it. It returns a channel
// per asked upstream, closed when its poll ends.
func (n *Network) ask(sel string, targets []common.EvmUpstream) []<-chan struct{} {
	parent := n.appCtx
	if parent == nil {
		parent = context.Background()
	}
	var polls []<-chan struct{}
	for _, u := range targets {
		sp := u.EvmStatePoller()
		if sp == nil || sp.IsObjectNull() {
			continue
		}
		done := make(chan struct{})
		if cur, inFlight := n.asks.LoadOrStore(u.Id(), (<-chan struct{})(done)); inFlight {
			polls = append(polls, cur.(<-chan struct{}))
			continue
		}
		polls = append(polls, done)
		go func() {
			defer close(done)
			defer n.asks.Delete(u.Id())
			defer n.recoverAskPanic(sel)
			ctx, cancel := context.WithTimeout(parent, health.FreshnessWindow(n.EvmBlockTime()))
			defer cancel()
			_, _ = sp.PollLatestBlockNumber(ctx)
		}()
	}
	return polls
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

// awaitBandAsk joins the band's Ask of selector sel's lane for block
// (askBand) and waits for its polls (awaitPolls), so the band's retry
// orders upstreams on the heads it brings (spec §6.4).
func (n *Network) awaitBandAsk(ctx context.Context, sel string, block int64) {
	n.awaitPolls(ctx, n.askBand(sel, block))
}

// awaitPolls waits for polls, at most 1 bt or until ctx ends.
func (n *Network) awaitPolls(ctx context.Context, polls []<-chan struct{}) {
	if len(polls) == 0 {
		return
	}
	timer := time.NewTimer(n.blockTimeOrCold())
	defer timer.Stop()
	for _, p := range polls {
		select {
		case <-p:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
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
// above its lane's Latest waits up to 1 bt while the voters below that
// block refresh, then is sent if Latest includes the block, else it is
// null, as it is when ctx ends first. held: resp was replaced by null.
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
			return nullAnswer(req, resp)
		case <-ctx.Done():
			return nullAnswer(req, resp)
		}
		at, moved = latest()
	}
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
