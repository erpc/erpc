package evm

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// clockFit is how many recent blocks the block clock's phase fits. The
// fit spaces them by the block time estimate, so its error moves the
// phase by about clockFit/2 of it: few blocks keep a cold estimate (whole
// second timestamps) from moving fires by a block.
const clockFit = 8

// clockUnknownRTT stands in for a voter's round trip before the clock
// measured one.
const clockUnknownRTT = 150 * time.Millisecond

// headClock is a lane's block clock (spec §6.8): when no push brought the
// next block, it fetches one voter's head once, at the 95th percentile of
// the block's expected first sight, retries once when that was early, and
// hedges to the next voter when the fetch is late. It runs while the lane
// has demand: a tap that needs newHeads, or a read of latest within W.
// Only the tracker's goroutine touches it.
type headClock struct {
	// sights are the first local sights of the newest numbers, ascending,
	// at most clockFit.
	sights []clockSight
	// fit is the sights the phase reads (phasePoint), ascending, at most
	// clockFit: when blocks become available, not when one first reached
	// this instance.
	fit []clockSight
	// pushLane: a push brought a first sight since the reset, so only
	// on-time pushes and fetch answers before the predicted sight move the
	// phase. missed counts the first sights in a row no on-time push
	// brought (pushFed).
	pushLane bool
	missed   int
	// off is where a fire is sent relative to the next block's predicted
	// sight, less the target's round trip, in block times, so it holds
	// while the block time estimate moves from its cold value: it moves
	// up by 32 steps after an early answer and down by 1 after a timely
	// one, so it settles where 3% of the fires come early (k ≈ 1.9 at any
	// spread): each early fire costs a retry, and catch-ups and hedges
	// share the 5% over one fetch per block the budget allows.
	off    float64
	offSet bool
	// out is the fetch in flight, hedge the second voter's while out is
	// late; nil while none is.
	out, hedge *clockFire
	// target is the number the clock fetches for: the top sight's next.
	// early counts the fires that answered below it; misses the fires in a
	// row that brought no new block, which double the next delay up to W.
	target          uint64
	early, misses   int
	retryAt, nextAt time.Time
	lastFire        time.Time
	// scheduled: a fire for target was due when a sight brought target,
	// which the next run counts as skipped_push.
	scheduled bool
	skips     int
	rr        int
	ups       map[string]*clockVoter
	// taken is, per voter, the At of the last answer the clock took from
	// it: the next fetch must be newer (Ask's after).
	taken map[string]time.Time
	// edge is the fetch that answers the first read after an idle W
	// (ChainTracker.LatestRead). Its answer sets edgeTo, the number it
	// brought: the edge ends once the view reaches it, or at edgeBy, W
	// after the answer, when it never does (a fork, a walk that fails);
	// edgeDone ends it at the next run.
	edge     *clockFire
	edgeTo   uint64
	edgeBy   time.Time
	edgeDone bool
	// seq names the clock's generation: a reset moves it on, so an answer
	// from before the reset is dropped.
	seq uint64
}

type clockSight struct {
	number uint64
	at     time.Time
	// pushed: a push, not a fetch, brought it first; onTime: a push no
	// later than its predicted sight plus the push allowance (pushLate).
	pushed, onTime bool
}

type clockFire struct {
	id   string
	sent time.Time
	// top is the clock's top sight when it was sent: an answer above it
	// is a new block.
	top    uint64
	hedged bool
	// kind is why it was sent, the kind label of erpc_head_clock_total:
	// probe (at the phase's fire time, fireAt), retry (a probe's after an
	// early answer), hedge, catch_up, guarantee, edge (the idle edge's),
	// cold (before any sight). Only a probe's or retry's answer says where
	// the next block's sight is, so only they move the offset, early count
	// and misses.
	kind string
}

// probe reports whether f was sent at the phase's fire time.
func (f *clockFire) probe() bool { return f.kind == "probe" || f.kind == "retry" }

// clockVoter is what the clock learned of one voter: its round trip's EMA
// and mean deviation, and late, an EMA of how far behind the first sight
// its answers were, so a lagging voter is not the target.
type clockVoter struct {
	rtt, dev, late time.Duration
	samples        int
}

func (c *headClock) reset() {
	seq := c.seq + 1
	*c = headClock{seq: seq, ups: make(map[string]*clockVoter), taken: make(map[string]time.Time)}
}

// saw records the first local sight of number at at. A poll, not a push,
// that brings the target before its scheduled probe shows the probe was
// late by at least the gap: the offset moves earlier by it, so a voter
// that lags the chain (an own node behind the vendors) and polls on its
// own never sets the phase the vendors' blocks are fetched at. A push
// skips the probe and moves nothing: it costs no fetch.
func (c *headClock) saw(number uint64, at time.Time, pushed bool, bt time.Duration) {
	if n := len(c.sights); n > 0 && number <= c.sights[n-1].number {
		return
	}
	if c.scheduled && c.out == nil && number >= c.target {
		c.skips++
		if !pushed && c.early == 0 && c.misses == 0 && bt > 0 && c.nextAt.After(at) {
			c.off = max(c.off-max(float64(c.nextAt.Sub(at))/float64(bt), 1.0/400), -0.5)
		}
	}
	c.scheduled = false
	s := clockSight{number: number, at: at, pushed: pushed}
	s.onTime = pushed && (!c.pushLane || bt <= 0 || !at.After(c.predict(number, bt).Add(c.pushLate(bt))))
	if c.phasePoint(s, bt) {
		c.fit = appendSight(c.fit, s)
	}
	c.pushLane = c.pushLane || pushed
	if s.onTime {
		c.missed = 0
	} else {
		c.missed++
	}
	c.sights = appendSight(c.sights, s)
}

// phasePoint reports whether sight s moves the phase. Until a push
// brought a block, every sight does: the lane's blocks come by fetch. In
// a push lane, an on-time push does, and, while the lane is not push-fed,
// a fetch answer before its predicted sight: an answer only bounds when
// the block became available from above, so a later one says nothing,
// and a late push says when that source sent it, not when the block was
// out. The off/early rule stays the only way the fires move later.
func (c *headClock) phasePoint(s clockSight, bt time.Duration) bool {
	switch {
	case !c.pushLane || len(c.fit) == 0 || bt <= 0:
		return true
	case s.pushed:
		return s.onTime
	default:
		return !c.pushFed() && s.at.Before(c.predict(s.number, bt))
	}
}

// appendSight appends s to sights, keeping the newest clockFit.
func appendSight(sights []clockSight, s clockSight) []clockSight {
	sights = append(sights, s)
	if over := len(sights) - clockFit; over > 0 {
		sights = slices.Delete(sights, 0, over)
	}
	return sights
}

// top is the highest number sighted, 0 before any.
func (c *headClock) top() uint64 {
	if len(c.sights) == 0 {
		return 0
	}
	return c.sights[len(c.sights)-1].number
}

// sightOf is when number was first sighted; zero when it is not among the
// recent sights.
func (c *headClock) sightOf(number uint64) time.Time {
	i, ok := slices.BinarySearchFunc(c.sights, number, func(s clockSight, n uint64) int {
		switch {
		case s.number < n:
			return -1
		case s.number > n:
			return 1
		}
		return 0
	})
	if !ok {
		return time.Time{}
	}
	return c.sights[i].at
}

// predict is when number's first sight is due: a robust fit of the
// phase points' phase, the median of each point's time less its number's
// multiple of bt, plus number's multiple of bt.
func (c *headClock) predict(number uint64, bt time.Duration) time.Time {
	top, phase := c.phase(bt)
	return top.at.Add(phase[len(phase)/2] + time.Duration(number-top.number)*bt)
}

// phase is the newest phase point and every point's offset from it, less
// the blocks between at bt, ascending.
func (c *headClock) phase(bt time.Duration) (clockSight, []time.Duration) {
	top := c.fit[len(c.fit)-1]
	offsets := make([]time.Duration, len(c.fit))
	for i, s := range c.fit {
		offsets[i] = s.at.Sub(top.at) - time.Duration(s.number-top.number)*bt
	}
	slices.Sort(offsets)
	return top, offsets
}

// pushFed reports whether on-time pushes bring the lane's blocks: a fetch
// only hedges a late one (spec §6.8 skip) and is sent at pushLate after
// the predicted sight, not a round trip before it. After clockFit/2 first
// sights in a row that no on-time push brought (late pushes, fetches) the
// clock fetches every block, until an on-time push returns.
func (c *headClock) pushFed() bool {
	return c.pushLane && c.missed < clockFit/2
}

// pushLate is how long after its predicted sight a pushed block is late:
// the fit's largest residual over the phase points, about their 95th
// percentile, at least bt/10 and at most bt.
func (c *headClock) pushLate(bt time.Duration) time.Duration {
	_, offsets := c.phase(bt)
	return min(max(offsets[len(offsets)-1]-offsets[len(offsets)/2], bt/10), bt)
}

// due is when the tracker must look at the clock again: its next fire,
// the deadline that hedges the fetch in flight, or the end of an answered
// idle edge; zero while none is set.
func (c *headClock) due() time.Time {
	if !c.edgeBy.IsZero() && (c.nextAt.IsZero() || c.edgeBy.Before(c.nextAt)) {
		return c.edgeBy
	}
	return c.nextAt
}

// voter is v's record, made on first use.
func (c *headClock) voter(id string) *clockVoter {
	v := c.ups[id]
	if v == nil {
		v = &clockVoter{}
		c.ups[id] = v
	}
	return v
}

// rttOf is id's round trip EMA, clockUnknownRTT before a sample.
func (c *headClock) rttOf(id string) time.Duration {
	if v := c.ups[id]; v != nil && v.samples > 0 {
		return v.rtt
	}
	return clockUnknownRTT
}

// sampleRTT folds one round trip of id into its EMAs.
func (c *headClock) sampleRTT(id string, rtt time.Duration) {
	v := c.voter(id)
	if v.samples == 0 {
		v.rtt, v.dev = rtt, rtt/4
	} else {
		d := rtt - v.rtt
		v.rtt += d / 8
		v.dev += (abs(d) - v.dev) / 4
	}
	v.samples++
}

// sampleLate folds how far behind the first sight id's answer was.
func (c *headClock) sampleLate(id string, late time.Duration) {
	v := c.voter(id)
	v.late += (late - v.late) / 4
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// hedgeAfter is how long a fetch to id may take before the clock hedges
// it: its round trip's 95th percentile plus the larger of its round trip
// and bt/10.
func (c *headClock) hedgeAfter(id string, bt time.Duration) time.Duration {
	rtt, dev := clockUnknownRTT, clockUnknownRTT/4
	if v := c.ups[id]; v != nil && v.samples > 0 {
		rtt, dev = v.rtt, v.dev
	}
	return rtt + 2*dev + max(rtt, bt/10)
}

// clockCandidate is one voter the clock may fetch: its poller and the
// cadences its config sets.
type clockCandidate struct {
	id       string
	poller   common.EvmStatePoller
	interval time.Duration // statePollerInterval, 0 none
	debounce time.Duration // an explicit statePollerDebounce, 0 none
}

// candidates are the lane's voters the clock may fetch now, in selection
// order: each with a poller, not backing off, and not cordoned for
// another chain's identity, whose heads never vote (spec §5): its fast
// answers would win every fire while the view waits.
func (t *ChainTracker) clockCandidates() []clockCandidate {
	n, ok := t.network.(interface {
		EvmAllUpstreams(context.Context) []common.Upstream
	})
	if !ok || len(t.voters.Voters) == 0 {
		return nil
	}
	var ups []common.Upstream
	t.callNetwork("clock-upstreams", func() { ups = n.EvmAllUpstreams(t.ctx) })
	out := make([]clockCandidate, 0, len(t.voters.Voters))
	for _, id := range t.voters.Voters {
		i := slices.IndexFunc(ups, func(u common.Upstream) bool { return u.Id() == id })
		if i < 0 {
			continue
		}
		eu, ok := ups[i].(common.EvmUpstream)
		if !ok {
			continue
		}
		p := eu.EvmStatePoller()
		if p == nil || p.IsObjectNull() || p.HeadBackingOff() {
			continue
		}
		if c, ok := p.(interface{ ChainIdCordoned() bool }); ok && c.ChainIdCordoned() {
			continue
		}
		cand := clockCandidate{id: id, poller: p}
		if cfg := ups[i].Config(); cfg != nil && cfg.Evm != nil {
			cand.interval = cfg.Evm.StatePollerInterval.Duration()
			cand.debounce = cfg.Evm.StatePollerDebounce.Duration()
		}
		out = append(out, cand)
	}
	return out
}

// pickTarget is the voter a fire goes to, never skip: a voter whose turn
// is due, the one whose last local fetch is oldest, so its full poll's
// latest part is met (spec §6.8 turns); else the fastest by expected first
// sight, round trip plus lateness, round-robin among those within a
// quarter of its round trip: each millisecond of a slower target adds to
// the served age, and the first read of the block goes to its holder.
// A turn is the block's fire too, so only a voter within a quarter block
// of the fastest takes one, and only one polled less often than every two
// blocks: a far or lagging voter, or one polled every block, polls on its
// own. A voter the clock never measured takes its turn: only a clock
// answer measures it, so else it would never take one and its full poll
// would fetch on its own. An idle edge's fetch (edge) has a read waiting
// on it: it takes no turn. ok false with none.
func (t *ChainTracker) pickTarget(cands []clockCandidate, skip string, now time.Time, bt time.Duration, edge bool) (clockCandidate, bool) {
	c := &t.clock
	score := func(id string) time.Duration {
		s := c.rttOf(id)
		if v := c.ups[id]; v != nil {
			s += v.late
		}
		return s
	}
	best := time.Duration(-1)
	var bestRTT time.Duration
	for _, cand := range cands {
		if cand.id == skip {
			continue
		}
		if s := score(cand.id); best < 0 || s < best {
			best, bestRTT = s, c.rttOf(cand.id)
		}
	}
	if best < 0 {
		return clockCandidate{}, false
	}
	var turn *clockCandidate
	var turnAt time.Time
	for i := range cands {
		cand := &cands[i]
		if v := c.ups[cand.id]; edge || cand.id == skip || cand.interval <= 2*bt || v != nil && v.samples > 0 && score(cand.id) > best+bt/4 {
			continue
		}
		last := cand.poller.LatestAskAt()
		if now.Sub(last) < cand.interval-2*bt {
			continue
		}
		if turn == nil || last.Before(turnAt) {
			turn, turnAt = cand, last
		}
	}
	if turn != nil {
		return *turn, true
	}
	var near []clockCandidate
	for _, cand := range cands {
		if cand.id != skip && score(cand.id) <= best+bestRTT/4 {
			near = append(near, cand)
		}
	}
	c.rr++
	return near[c.rr%len(near)], true
}

// clockDemand reports whether the clock runs, and the fastest pace its
// demand needs heads at (0 every block): a read of latest within
// readDemand, or a tap that needs newHeads (demand).
func (t *ChainTracker) clockDemand(now time.Time) (pace time.Duration, ok bool) {
	if at := t.latestReadAt.Load(); at > 0 && now.Sub(time.Unix(0, at)) < t.readDemand() {
		return 0, true
	}
	return t.demand()
}

// readDemand is how long a read of latest keeps the clock running: 2 W.
// Reads W to 2 W apart then meet a running clock, not an idle edge: an
// edge then costs q fetches, and a view up to W old serves no wait; reads
// further apart still stop the clock on a network nobody reads.
func (t *ChainTracker) readDemand() time.Duration { return 2 * t.W() }

// runClock runs after every batch: it counts the fires a push made
// needless, ends a served idle edge, hedges a late fetch, and fires or
// schedules the next fetch while the lane has demand (spec §6.8). An idle
// edge whose fetch is out or due fires at once and not as a probe; once
// answered, the clock runs as usual while the edge waits for the view.
func (t *ChainTracker) runClock() {
	c := &t.clock
	if c.skips > 0 {
		telemetry.CounterHandle(telemetry.MetricHeadClockTotal, t.projectId(), t.networkLabel(), t.laneLabel(), "probe", "skipped_push").Add(float64(c.skips))
		c.skips = 0
	}
	now, bt := time.Now(), t.BlockTime()
	edge := t.edge.Load()
	if edge != nil && c.edgeTo > 0 {
		if h := t.state.Load().viewHead(); h != nil && h.number >= c.edgeTo || !now.Before(c.edgeBy) {
			c.edgeDone = true
		}
	}
	if edge != nil && c.edgeDone {
		close(*edge)
		t.edge.Store(nil)
		edge, c.edge, c.edgeDone, c.edgeTo, c.edgeBy = nil, nil, false, 0, time.Time{}
	}
	waiting := edge != nil && c.edgeTo == 0
	c.nextAt = time.Time{}
	pace, demanded := t.clockDemand(now)
	if !demanded && edge == nil {
		c.scheduled = false
		return
	}
	if c.out != nil {
		if waiting && c.edge == nil {
			c.edge = c.out
		}
		if c.hedge == nil && !c.out.hedged && c.out.top == c.top() {
			if at := c.out.sent.Add(c.hedgeAfter(c.out.id, bt)); now.Before(at) {
				c.nextAt = at
			} else {
				c.out.hedged = true
				if cand, ok := t.pickTarget(t.clockCandidates(), c.out.id, now, bt, waiting); ok {
					c.hedge = t.fire(cand.id, now, "hedge")
				}
			}
		}
		return
	}
	if stale := t.staleVoters(t.clockCandidates()); len(stale) > 0 {
		// All at once: each answer moves one vote, and the jumped tree takes
		// the pins only with more votes than the older one (rule 4), so one
		// after another costs a round trip each.
		if c.out = t.fire(stale[0].id, now, "catch_up"); c.out != nil {
			for _, cand := range stale[1:] {
				t.fire(cand.id, now, "catch_up")
			}
			if waiting {
				c.edge = c.out
			}
			return
		}
	}
	if cand, ok := t.cappingVoter(t.clockCandidates()); ok {
		if c.out = t.fire(cand.id, now, "guarantee"); c.out != nil {
			if waiting {
				c.edge = c.out
			}
			return
		}
	}
	if holders := t.holderVoters(t.clockCandidates()); len(holders) > 0 {
		// All at once: the pin serves once q counted votes back it.
		if c.out = t.fire(holders[0].id, now, "holder"); c.out != nil {
			for _, cand := range holders[1:] {
				t.fire(cand.id, now, "holder")
			}
			if waiting {
				c.edge = c.out
			}
			return
		}
	}
	if target := c.top() + 1; target != c.target {
		c.target, c.early, c.retryAt = target, 0, time.Time{}
	}
	at, probe := now, !waiting && len(c.sights) > 0
	if probe {
		at = t.fireAt(bt, pace)
	}
	if now.Before(at) {
		c.nextAt, c.scheduled = at, true
		return
	}
	cands := t.clockCandidates()
	cand, ok := t.pickTarget(cands, "", now, bt, waiting)
	if !ok {
		if edge != nil {
			c.edgeDone = true
			c.nextAt = now
		}
		return
	}
	kind := "cold"
	switch {
	case waiting:
		kind = "edge"
	case probe && c.early > 0:
		kind = "retry"
	case probe:
		kind = "probe"
	}
	c.out = t.fire(cand.id, now, kind)
	if waiting {
		c.edge = c.out
		for _, id := range t.edgeVoters(cands, cand.id) {
			t.fire(id, now, "edge")
		}
	}
}

// edgeVoters are the voters an idle edge's fetch to first is sent beside:
// the next fastest by round trip plus lateness, so q voters answer at
// once. After an idle W the top sight is blocks old and the edge's answer
// jumps over blocks the pins lack: one vote on it is a root of its own
// that the older votes outweigh, so q answers link it in one round trip,
// not one after another (spec §6.8 catch-up).
func (t *ChainTracker) edgeVoters(cands []clockCandidate, first string) []string {
	c := &t.clock
	score := func(id string) time.Duration {
		s := c.rttOf(id)
		if v := c.ups[id]; v != nil {
			s += v.late
		}
		return s
	}
	var ids []string
	for _, cand := range cands {
		if cand.id != first {
			ids = append(ids, cand.id)
		}
	}
	slices.SortFunc(ids, func(a, b string) int { return cmp.Compare(score(a), score(b)) })
	return ids[:min(len(ids), max(t.voters.Need, 1)-1)]
}

// staleVoters are the voters the clock fetches now because a counted vote
// stands above the pins off their tree (spec §6.8 catch-up): a tracker no
// history serves holds a vote that jumped blocks as a root of its own, and
// rule 4 weighs it against the older votes on the pins, so the pins stay
// until those voters vote again. Each is a voter whose vote is on the pins
// and whose last fetch the clock took was sent before that vote was
// sighted, as many as the jumped tree lacks to outvote the pins' tree if
// each answer moves a vote; each voter is fetched once per such vote.
func (t *ChainTracker) staleVoters(cands []clockCandidate) []clockCandidate {
	head := t.head()
	if head == nil {
		return nil
	}
	pinRoot := t.pinRoot()
	var ahead *node
	for u, n := range t.votes {
		if t.counts(u) && t.alive(n) && n.h.number > head.number && t.rootOf(n) != pinRoot && (ahead == nil || n.h.number > ahead.h.number) {
			ahead = n
		}
	}
	if ahead == nil {
		return nil
	}
	aheadRoot := t.rootOf(ahead)
	onPins, onAhead := 0, 0
	for u, n := range t.votes {
		switch {
		case !t.counts(u) || !t.alive(n):
		case t.rootOf(n) == pinRoot:
			onPins++
		case t.rootOf(n) == aheadRoot:
			onAhead++
		}
	}
	// A tracker history serves re-anchors only with more than half the
	// lane's voters (rule 4).
	lacks := max(1, (onPins-onAhead)/2+1)
	if t.served {
		lacks = max(lacks, len(t.voters.Voters)/2+1-onAhead)
	}
	seen := t.clock.sightOf(ahead.h.number)
	var out []clockCandidate
	for _, cand := range cands {
		if len(out) == lacks {
			break
		}
		n := t.votes[cand.id]
		if t.alive(n) && t.rootOf(n) == pinRoot && t.clock.taken[cand.id].Before(seen) {
			out = append(out, cand)
		}
	}
	return out
}

// cappingVoter is a guarantee subset's voter the clock fetches now because
// the guarantee cap holds the Head below the pinned head (spec §6.8): a
// push brings the pins every block, but only that voter's newer head
// raises the cap. Each capping voter is fetched once per new pinned head.
func (t *ChainTracker) cappingVoter(cands []clockCandidate) (clockCandidate, bool) {
	head, g := t.head(), t.voters.Guarantee
	if head == nil || g <= 0 || head.number <= uint64(g) {
		return clockCandidate{}, false
	}
	seen := t.clock.sightOf(head.number)
	if seen.IsZero() {
		return clockCandidate{}, false
	}
	for _, cand := range cands {
		if slices.Contains(t.voters.Capping, cand.id) && t.clock.taken[cand.id].Before(seen) {
			return cand, true
		}
	}
	return clockCandidate{}, false
}

// holderVoters are the voters the clock fetches now because the highest
// voted pin lacks holders: its header serves only at q counted votes (a
// mismatch, spec §6.5), so the view's Head stays below it. A push brings
// such a pin every block and makes each probe needless, so without these
// fetches the view would stay below the pins until the next full poll.
// Each is a voter whose vote is on the pins below that pin and whose last
// answer the clock took was sent before the pin was sighted, as many as
// the pin lacks; each is fetched once per such pin.
func (t *ChainTracker) holderVoters(cands []clockCandidate) []clockCandidate {
	s := t.state.Load()
	b := s.backing
	if b == nil || len(b.backs) == 0 {
		return nil
	}
	top := b.backs[len(b.backs)-1].i
	pin := s.pinAt(b.base + uint64(top))
	lacks := max(b.need, 1) - (len(b.backs) - b.atLeast(top))
	if b.served >= top || pin == nil || lacks <= 0 {
		return nil
	}
	seen := t.clock.sightOf(pin.number)
	if seen.IsZero() {
		return nil
	}
	pinRoot := t.pinRoot()
	var out []clockCandidate
	for _, cand := range cands {
		if len(out) == lacks {
			break
		}
		n := t.votes[cand.id]
		if t.alive(n) && t.rootOf(n) == pinRoot && n.h.number < pin.number && t.clock.taken[cand.id].Before(seen) {
			out = append(out, cand)
		}
	}
	return out
}

// fireAt is when the clock fetches its target: the target's predicted
// sight plus the tracked offset, less the expected target's round trip; a
// retry's own time after an early answer; after misses, the last fire plus
// bt doubled per miss, at most W; never sooner than pace after the last
// fire.
func (t *ChainTracker) fireAt(bt, pace time.Duration) time.Time {
	c := &t.clock
	if !c.offSet {
		c.off, c.offSet = 0.1, true
	}
	var at time.Time
	switch {
	case c.misses > 0:
		at = c.lastFire.Add(min(bt<<min(c.misses-1, 16), t.W()))
	case c.early > 0:
		at = c.retryAt
	case c.pushFed():
		at = c.predict(c.target, bt).Add(c.pushLate(bt))
	default:
		at = c.predict(c.target, bt).Add(time.Duration(c.off*float64(bt)) - c.bestRTT())
	}
	if p := c.lastFire.Add(pace); pace > 0 && at.Before(p) {
		at = p
	}
	return at
}

// bestRTT is the lowest round trip the clock measured, clockUnknownRTT
// before any.
func (c *headClock) bestRTT() time.Duration {
	best := time.Duration(0)
	for _, v := range c.ups {
		if v.samples > 0 && (best == 0 || v.rtt < best) {
			best = v.rtt
		}
	}
	if best == 0 {
		return clockUnknownRTT
	}
	return best
}

// fire sends the clock's fetch of kind to voter id: a hash Ask newer than
// the last answer the clock took from it and than the top sight, so
// neither a fetch the clock already read nor one sent before the top
// block was seen meets it.
func (t *ChainTracker) fire(id string, now time.Time, kind string) *clockFire {
	c := &t.clock
	f := &clockFire{id: id, sent: now, top: c.top(), kind: kind}
	after := c.taken[id]
	if n := len(c.sights); n > 0 && c.sights[n-1].at.After(after) {
		after = c.sights[n-1].at
	}
	c.lastFire = now
	seq := c.seq
	if !t.askHead(id, common.HeadNeedHash, 0, after, common.AskClock, func(a common.HeadAnswer) { t.clockAnswered(seq, f, a) }) {
		return nil
	}
	return f
}

// clockAnswered takes the answer to fire f on the tracker's goroutine,
// after the vote it carried: a new block resets the misses, and a probe's
// pulls the offset earlier; a probe's answer still at f's top was early,
// which pushes the offset later and schedules the one retry, or a miss
// once the retry was spent; an answer below what another voter brought
// marks its voter late. Every answer is counted by f's kind and outcome.
func (t *ChainTracker) clockAnswered(seq uint64, f *clockFire, a common.HeadAnswer) {
	c := &t.clock
	if seq != c.seq {
		t.countFire(f, a, "dropped")
		return
	}
	switch f {
	case c.out:
		c.out = c.hedge
		c.hedge = nil
	case c.hedge:
		c.hedge = nil
	}
	edge := f == c.edge
	now, bt := time.Now(), t.BlockTime()
	if !a.At.IsZero() {
		if a.At.After(c.taken[f.id]) {
			c.taken[f.id] = a.At
		}
		if a.Outcome == common.AskFetched && a.Err == nil {
			c.sampleRTT(f.id, now.Sub(a.At))
		}
	}
	sh, ok := a.Header.(SourceHeader)
	if edge {
		// The edge waits for the view to reach what its fetch brought:
		// a jump over blocks the pins lack reaches it only once linked.
		if h := t.state.Load().viewHead(); a.Err == nil && ok && sh.h != nil && (h == nil || sh.h.number > h.number) {
			c.edgeTo, c.edgeBy = sh.h.number, now.Add(t.W())
		} else {
			c.edgeDone = true
		}
	}
	outcome := "failed"
	switch {
	case a.Err != nil || !ok || sh.h == nil:
	case sh.h.number > f.top:
		outcome, c.misses = "new", 0
		c.sampleLate(f.id, 0)
		if f.probe() && !f.hedged {
			c.off = max(c.off-1.0/400, -0.5)
		}
	case c.top() > f.top:
		// Another channel brought the block while this voter still
		// answered the one before: it lags.
		outcome = "behind"
		if seen := c.sightOf(c.top()); !seen.IsZero() {
			c.sampleLate(f.id, now.Sub(seen))
		}
	case !f.probe():
		outcome = "same"
	default:
		outcome = "early"
		// Only the block's first early answer says the phase fired early:
		// a retry's says the node is still behind, and an operator's
		// debounce retries many times a block, each of which would push
		// the offset a further 8% of a block later.
		if c.early == 0 {
			c.off = min(c.off+32.0/400, 1)
		}
		c.early++
		debounce := t.clockDebounce(f.id)
		switch {
		case debounce > 0:
			// The operator's own pace for its node (#790): it keeps
			// retrying every debounce.
			c.retryAt = now.Add(debounce)
		case c.early == 1:
			c.retryAt = now.Add(max(2*c.rttOf(f.id), bt/10))
		default:
			c.misses++
		}
	}
	t.countFire(f, a, outcome)
}

// countFire counts fire f's answer by kind and outcome; an Ask another
// fetch met is joined: it fetched nothing.
func (t *ChainTracker) countFire(f *clockFire, a common.HeadAnswer, outcome string) {
	if a.Outcome != common.AskFetched {
		outcome = "joined"
	}
	telemetry.CounterHandle(telemetry.MetricHeadClockTotal, t.projectId(), t.networkLabel(), t.laneLabel(), f.kind, outcome).Inc()
}

// clockDebounce is voter id's explicit statePollerDebounce, 0 when it
// sets none.
func (t *ChainTracker) clockDebounce(id string) time.Duration {
	n, ok := t.network.(interface {
		EvmAllUpstreams(context.Context) []common.Upstream
	})
	if !ok {
		return 0
	}
	var d time.Duration
	t.callNetwork("clock-debounce", func() {
		for _, u := range n.EvmAllUpstreams(t.ctx) {
			if u.Id() == id {
				if cfg := u.Config(); cfg != nil && cfg.Evm != nil {
					d = cfg.Evm.StatePollerDebounce.Duration()
				}
				return
			}
		}
	})
	return d
}

// LatestRead marks a read of the lane's latest (spec §6.8 demand): for
// readDemand after it the block clock runs. The first read after that is
// the idle edge, which fetches heads at once; every read of that edge
// waits for the view to reach the fetched head, at most W or until ctx
// ends, so none serves a Head older than it; with no pins yet nothing
// waits, and tags pass through (§7). A head number (number) waits only
// while the view has stood for W, so it is never older than W; else it
// answers from the view at once, as from memory (§5).
func (t *ChainTracker) LatestRead(ctx context.Context, number bool) {
	now := time.Now()
	prev := t.latestReadAt.Swap(now.UnixNano())
	edge := t.edge.Load()
	if edge == nil && (prev == 0 || now.Sub(time.Unix(0, prev)) >= t.readDemand()) {
		if pace, ok := t.demand(); !ok || pace > 0 {
			ch := make(chan struct{})
			if t.edge.CompareAndSwap(nil, &ch) {
				t.mb.put(trackerMsg{kind: msgTick})
			}
			edge = t.edge.Load()
		}
	}
	s := t.state.Load()
	if edge == nil || s.viewHead() == nil || number && now.Sub(s.viewAt) < t.W() {
		return
	}
	w := time.NewTimer(t.W())
	defer w.Stop()
	select {
	case <-*edge:
	case <-ctx.Done():
	case <-w.C:
	}
}

// MarkRead marks a lane tap's read, which needs the lane's view to move:
// the block clock runs for readDemand after it, without the idle edge's
// wait.
func (t *ChainTracker) MarkRead() {
	if prev := t.latestReadAt.Swap(time.Now().UnixNano()); prev == 0 || time.Since(time.Unix(0, prev)) >= t.readDemand() {
		t.mb.put(trackerMsg{kind: msgTick})
	}
}
