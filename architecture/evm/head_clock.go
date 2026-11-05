package evm

import (
	"cmp"
	"context"
	"math"
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
// next block, it spends the voters' turns, the latest polls their
// statePollerInterval pays for, at the times the block may arrive: a
// ladder of rungs whose last sights the block at its 97th percentile,
// each rung one due voter's fetch; the first sight stops the ladder, and
// the turns not sent vote at once. It runs while the lane has demand: a
// tap that needs newHeads, or a read of latest within 2 W. Only the
// tracker's goroutine touches it.
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
	// off is where the ladder's last rung sights the next block relative
	// to its predicted sight, in block times, so it holds while the block
	// time estimate moves from its cold value. It moves once per block, at
	// the block's first sight (saw): up 32 steps when a tail rung brought
	// it, down 1 when a rung did, never for a missed slot; so it settles
	// where 3% of the blocks need a tail rung (k ≈ 1.9 at any spread).
	off    float64
	offSet bool
	// out is the fetch off the ladder the clock waits on (catch-up,
	// guarantee, holder, idle edge, cold) until it answers or its hedge
	// time passes; nil while none is.
	out *clockFire
	// target is the number the clock fetches for: the top sight's next;
	// planned once its fires are laid (plan).
	target  uint64
	planned bool
	// rungs are target's fires not sent yet, by send time; sent the ones
	// sent; votes the due voters that take no rung (laggards), which
	// vote at target's sight. k is the ladder's rung count, the tail
	// rungs it may add; tails those added; delta the rungs' spacing;
	// misses the fires past the tail, which double the delay up to W;
	// lastDone when the last fire answered or was hedged.
	rungs    []clockRung
	sent     []*clockFire
	votes    []string
	k, tails int
	delta    time.Duration
	misses   int
	lastDone time.Time
	// ladder: target's sight moves off; predicted is when it was due;
	// kEarly: the ladder's last rung (S_k) answered below the target.
	ladder, kEarly   bool
	predicted        time.Time
	nextAt, lastFire time.Time
	// scheduled: target's last rung was not sent when a sight brought
	// target, which the next run counts as skipped_push.
	scheduled bool
	skips     int
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
	// at is when it reached this instance; from when its evidence was
	// taken: a fetch's send time, a push's receipt.
	at, from time.Time
	// pushed: a push, not a fetch, brought it first; onTime: a push no
	// later than its predicted sight plus the push allowance (pushLate).
	pushed, onTime bool
}

// clockRung is one of the target's fires not sent yet: to voter id, ""
// for the fastest free voter at its send, at at, of kind (clockFire).
// turn: the voter's turn, which votes at the target's sight if not sent
// by then; again: a refire after a joined answer, once per rung.
type clockRung struct {
	id          string
	at          time.Time
	kind        string
	turn, again bool
}

type clockFire struct {
	id   string
	sent time.Time
	// top is the clock's top sight when it was sent: an answer above it
	// is a new block.
	top uint64
	// hedged: its hedge time passed unanswered and the next fire went;
	// done: answered.
	hedged, done bool
	turn, again  bool
	// kind is why it was sent, the kind label of erpc_head_clock_total:
	// rung (a ladder rung before the last), probe (the ladder's last rung,
	// S_k, or the block's one fire), retry (a tail rung, or a fire whose
	// delay doubles), vote (a turn sent at the target's sight), catch_up,
	// guarantee, holder, edge (the idle edge's), cold (before any sight).
	// Only a rung's, probe's or retry's answer is the ladder's.
	kind string
}

// ladder reports whether f is one of the ladder's fires.
func (f *clockFire) ladder() bool {
	return f.kind == "rung" || f.kind == "probe" || f.kind == "retry"
}

// clockVoter is what the clock learned of one voter: its round trip's EMA
// and mean deviation, and late, an EMA of how far behind the first sight
// its answers were, so a lagging voter takes no rung. asked and askedAt
// are its last fetch's answer and send time, open until a first sight
// above it decides whether it lagged (answered). firedAt is the send of
// the clock's last fetch of it, out that fetch while unanswered.
type clockVoter struct {
	rtt, dev, late time.Duration
	samples        int
	asked          uint64
	askedAt        time.Time
	open           bool
	firedAt        time.Time
	out            *clockFire
}

func (c *headClock) reset() {
	seq := c.seq + 1
	*c = headClock{seq: seq, ups: make(map[string]*clockVoter), taken: make(map[string]time.Time)}
}

// saw records the first local sight of number at at, its evidence taken
// at from. The target's sight moves the offset once (spec §6.8): down a
// step when the ladder's last rung did not answer below it, up 32 when
// it did, and not at all when the sight came half a block or more after
// its predicted sight: a missed slot or a halt says nothing of the
// arrival spread. Each open fetch answer below number is decided
// (answered).
func (c *headClock) saw(number uint64, at, from time.Time, pushed bool, bt time.Duration) {
	if n := len(c.sights); n > 0 && number <= c.sights[n-1].number {
		return
	}
	if number >= c.target {
		if c.scheduled {
			c.skips++
		}
		if c.ladder {
			switch {
			case !c.kEarly:
				c.off = max(c.off-1.0/400, -0.5)
			case at.Before(c.predicted.Add(bt / 2)):
				c.off = min(c.off+32.0/400, 1)
			}
			c.ladder = false
		}
	}
	c.scheduled = false
	s := clockSight{number: number, at: at, from: from, pushed: pushed}
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
	for id, v := range c.ups {
		if v.open && v.asked < number {
			v.open = false
			if v.askedAt.After(from) {
				c.sampleLate(id, v.askedAt.Sub(from))
			}
		}
	}
}

// answered takes voter id's fetch sent at sent that answered number: a
// fetch sent before it that brought a higher number shows the voter
// lagged by at least the gap, whichever answer reached this instance
// first. A first sight above number decides it now if there is one, else
// when it comes (saw); each answer is decided once, as the clock's answer
// and as its vote.
func (c *headClock) answered(id string, number uint64, sent time.Time) {
	v := c.voter(id)
	if v.asked == number && v.askedAt.Equal(sent) {
		return
	}
	v.asked, v.askedAt, v.open = number, sent, true
	var from time.Time
	for _, s := range c.sights {
		if s.number > number && (from.IsZero() || s.from.Before(from)) {
			from = s.from
		}
	}
	if from.IsZero() {
		return
	}
	v.open = false
	if sent.After(from) {
		c.sampleLate(id, sent.Sub(from))
	}
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

// fastest is the candidate with the lowest score, round trip plus
// lateness, and no clock fetch in flight: each millisecond of a slower
// one adds to the served age. ok false with none.
func (c *headClock) fastest(cands []clockCandidate) (clockCandidate, bool) {
	var best clockCandidate
	ok := false
	for _, cand := range cands {
		if !c.busy(cand.id) && (!ok || c.score(cand.id) < c.score(best.id)) {
			best, ok = cand, true
		}
	}
	return best, ok
}

// byScore sorts cands by score, fastest first.
func (c *headClock) byScore(cands []clockCandidate) []clockCandidate {
	out := slices.Clone(cands)
	slices.SortStableFunc(out, func(a, b clockCandidate) int { return cmp.Compare(c.score(a.id), c.score(b.id)) })
	return out
}

// busy reports whether a clock fetch of id is in flight: another Ask of
// it would only join that fetch.
func (c *headClock) busy(id string) bool {
	v := c.ups[id]
	return v != nil && v.out != nil
}

// dueTurns are the candidates whose turn is due by at (spec §6.8): each
// is polled, and its last local fetch, the clock's included, was sent
// its interval or more before at. At most ceil(Σ min(1, bt/I)) of them
// a block, the lane's turns per block, the longest waiting first: a
// voter past them waits a block, which its poll's extra block allows,
// so a block where two turns of a 0.28-a-block lane fall costs one
// fetch, not two. A voter polled every 0 s pays for no turn.
func (c *headClock) dueTurns(cands []clockCandidate, at time.Time, bt time.Duration) []clockCandidate {
	type due struct {
		cand clockCandidate
		last time.Time
	}
	var dues []due
	var turns float64
	for _, cand := range cands {
		if cand.interval <= 0 {
			continue
		}
		turns += min(1, float64(bt)/float64(cand.interval))
		last := cand.poller.LatestAskAt()
		if v := c.ups[cand.id]; v != nil && v.firedAt.After(last) {
			last = v.firedAt
		}
		if !last.Add(cand.interval).After(at) {
			dues = append(dues, due{cand, last})
		}
	}
	slices.SortStableFunc(dues, func(a, b due) int { return a.last.Compare(b.last) })
	out := make([]clockCandidate, 0, len(dues))
	for _, d := range dues[:min(len(dues), max(1, int(math.Ceil(turns-1e-9))))] {
		out = append(out, d.cand)
	}
	return out
}

// score is id's expected first sight after a fire: its round trip plus
// its lateness.
func (c *headClock) score(id string) time.Duration {
	s := c.rttOf(id)
	if v := c.ups[id]; v != nil {
		s += v.late
	}
	return s
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

// runClock runs after every batch: it counts the fires a sight made
// needless, ends a served idle edge, votes the turns a sight left unsent,
// and fires or schedules the next fetches while the lane has demand
// (spec §6.8). A fetch off the ladder (catch-up, guarantee, holder, idle
// edge, cold) holds the ladder until it answers or its hedge time passes:
// then the next fire is its hedge. An idle edge whose fetch is out or due
// fires at once and not as a rung; once answered, the clock runs as usual
// while the edge waits for the view.
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
		c.scheduled, c.planned, c.ladder, c.rungs, c.votes = false, false, false, nil, nil
		return
	}
	var cands []clockCandidate
	if target := c.top() + 1; target != c.target {
		cands = t.clockCandidates()
		t.vote(cands, now, bt, pace)
		c.target, c.planned = target, false
	}
	if c.out != nil && !c.out.hedged {
		if waiting && c.edge == nil {
			c.edge = c.out
		}
		if at := c.out.sent.Add(c.hedgeAfter(c.out.id, bt)); now.Before(at) {
			c.nextAt = at
			return
		}
		c.out.hedged = true
	}
	if cands == nil {
		cands = t.clockCandidates()
	}
	free := slices.DeleteFunc(slices.Clone(cands), func(cand clockCandidate) bool { return c.busy(cand.id) })
	if stale := t.staleVoters(free); len(stale) > 0 {
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
	if cand, ok := t.cappingVoter(free); ok {
		if c.out = t.fire(cand.id, now, "guarantee"); c.out != nil {
			if waiting {
				c.edge = c.out
			}
			return
		}
	}
	if holders := t.holderVoters(free); len(holders) > 0 {
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
	if waiting || len(c.sights) == 0 {
		cand, ok := c.fastest(cands)
		if !ok {
			if edge != nil {
				c.edgeDone = true
				c.nextAt = now
			}
			return
		}
		kind := "cold"
		if waiting {
			kind = "edge"
		}
		c.out = t.fire(cand.id, now, kind)
		if waiting {
			c.edge = c.out
			for _, id := range t.edgeVoters(cands, cand.id) {
				t.fire(id, now, "edge")
			}
		}
		return
	}
	if !c.planned {
		t.plan(cands, now, bt, pace)
	}
	t.climb(cands, now, bt)
	if len(c.rungs) > 0 {
		_, ph := c.phase(bt)
		println("DBGRUN", now.Format("05.000"), c.target, len(cands), pace, c.rungs[0].at.Sub(now).Milliseconds(), c.rungs[0].kind, bt.Milliseconds(), len(c.fit), ph[0].Milliseconds(), ph[len(ph)/2].Milliseconds(), ph[len(ph)-1].Milliseconds(), c.fit[len(c.fit)-1].number, c.top())
	} else {
		println("DBGRUN", now.Format("05.000"), c.target, len(cands), pace, "norungs", len(c.sent))
	}
}

// plan lays the target's fires (spec §6.8) and, while the clock fetches
// every block, hands each candidate's latest polls to it for 2 W
// (ClockTurns). A push-fed lane fires once, at the push allowance after
// the predicted sight P; a lane whose demand sets a pace fires once,
// no sooner than the pace after the last fire, and takes no turns.
// Else the ladder: each voter whose turn is due by the last rung's sight
// S_k = P + off·bt takes one turn, a rung when its score is within bt/4
// of the fastest voter's or unmeasured, else a vote at the target's
// sight. The k rungs, fastest first, sight at S_k − (k−j)·Δ, Δ the span
// from the fit's earliest residual to S_k over k, each sent its voter's
// round trip before. With no rung due, the fastest voter fetches at S_k:
// the block's one fetch.
func (t *ChainTracker) plan(cands []clockCandidate, now time.Time, bt, pace time.Duration) {
	c := &t.clock
	if !c.offSet {
		c.off, c.offSet = 0.1, true
	}
	c.planned, c.ladder, c.kEarly, c.scheduled = true, false, false, true
	c.rungs, c.sent, c.votes = nil, nil, nil
	c.k, c.tails, c.misses, c.lastDone = 1, 0, 0, time.Time{}
	p := c.predict(c.target, bt)
	sk := p.Add(time.Duration(c.off * float64(bt)))
	_, phase := c.phase(bt)
	c.delta = max(sk.Sub(p.Add(phase[0]-phase[len(phase)/2])), 0)
	at := sk.Add(-c.bestRTT(cands))
	if c.pushFed() {
		at = p.Add(c.pushLate(bt))
	}
	if pace > 0 {
		if p := c.lastFire.Add(pace); at.Before(p) {
			at = p
		}
		c.rungs = []clockRung{{at: at, kind: "probe"}}
		return
	}
	for _, cand := range cands {
		cand.poller.ClockTurns(now.Add(2 * t.W()))
	}
	if c.pushFed() {
		c.rungs = []clockRung{{at: at, kind: "probe"}}
		return
	}
	best := time.Duration(-1)
	for _, cand := range cands {
		if s := c.score(cand.id); best < 0 || s < best {
			best = s
		}
	}
	var turns []clockCandidate
	for _, cand := range c.byScore(c.dueTurns(cands, sk, bt)) {
		if v := c.ups[cand.id]; v == nil || v.samples == 0 || c.score(cand.id) <= best+bt/4 {
			turns = append(turns, cand)
		} else {
			c.votes = append(c.votes, cand.id)
		}
	}
	c.ladder, c.predicted = true, p
	if len(turns) == 0 {
		c.rungs = []clockRung{{at: at, kind: "probe"}}
		return
	}
	c.k = len(turns)
	c.delta /= time.Duration(c.k)
	for j, cand := range turns {
		r := clockRung{id: cand.id, at: sk.Add(-time.Duration(c.k-1-j)*c.delta - c.rttOf(cand.id)), kind: "rung", turn: true}
		if j == c.k-1 {
			r.kind = "probe"
		}
		c.rungs = append(c.rungs, r)
	}
	slices.SortStableFunc(c.rungs, func(a, b clockRung) int { return a.at.Compare(b.at) })
}

// vote sends, at the target's first sight, the turns its ladder left
// unsent and its laggards' (spec §6.8 stop at sight): q hash votes on the
// sighted block within a round trip, each a voter's turn, which its poll
// would else fetch. On a push-fed lane each voter whose turn is due votes
// at each first sight.
func (t *ChainTracker) vote(cands []clockCandidate, now time.Time, bt, pace time.Duration) {
	c := &t.clock
	ids := c.votes
	for _, r := range c.rungs {
		if r.turn {
			ids = append(ids, r.id)
		}
	}
	if pace == 0 && c.pushFed() {
		for _, cand := range c.dueTurns(cands, now, bt) {
			ids = append(ids, cand.id)
		}
	}
	c.rungs, c.votes = nil, nil
	for _, cand := range cands {
		if slices.Contains(ids, cand.id) && !c.busy(cand.id) {
			t.fire(cand.id, now, "vote")
		}
	}
}

// climb sends the target's fires whose time came and hedges the late
// ones (spec §6.8): a fire unanswered after hedgeAfter sends the next one
// now, a tail rung when none is left. Once every fire answered below the
// target, tail rungs follow every tailGap, to voters in score order, at
// most k of them; then fires that double the delay up to W.
func (t *ChainTracker) climb(cands []clockCandidate, now time.Time, bt time.Duration) {
	c := &t.clock
	for _, f := range c.sent {
		if !f.done && !f.hedged && !now.Before(f.sent.Add(c.hedgeAfter(f.id, bt))) {
			f.hedged, c.lastDone = true, now
			if len(c.rungs) > 0 {
				c.rungs[0].at = now
			} else {
				t.addTail(cands, now, bt, true)
			}
		}
	}
	idle := func() bool {
		return len(c.rungs) == 0 && !slices.ContainsFunc(c.sent, func(f *clockFire) bool { return !f.done && !f.hedged })
	}
	// Twice: a fire skipped now (a turn whose voter left) leaves the
	// ladder idle, and only a tail rung wakes the clock again.
	for range 2 {
		if idle() {
			t.addTail(cands, now, bt, false)
		}
		for len(c.rungs) > 0 && !c.rungs[0].at.After(now) {
			r := c.rungs[0]
			c.rungs = c.rungs[1:]
			t.fireRung(cands, r, now)
		}
	}
	if len(c.rungs) > 0 {
		c.nextAt = c.rungs[0].at
	}
	for _, f := range c.sent {
		if at := f.sent.Add(c.hedgeAfter(f.id, bt)); !f.done && !f.hedged && (c.nextAt.IsZero() || at.Before(c.nextAt)) {
			c.nextAt = at
		}
	}
}

// addTail adds the target's next fire after the ladder brought no block:
// a tail rung (retry) to the next voter in score order, at the last
// answer plus its explicit statePollerDebounce (#790), else plus
// max(2 RTT, min(Δ, bt/10)); after k of them, one to the fastest voter
// at the last fire plus bt doubled per miss, at most W. A hedge's goes
// now.
func (t *ChainTracker) addTail(cands []clockCandidate, now time.Time, bt time.Duration, hedge bool) {
	c := &t.clock
	r := clockRung{kind: "retry", at: now}
	if order := c.byScore(cands); c.tails < c.k && len(order) > 0 {
		cand := order[c.tails%len(order)]
		c.tails++
		r.id = cand.id
		gap := cand.debounce
		if gap <= 0 {
			gap = max(2*c.bestRTT(cands), min(c.delta, bt/10))
		}
		if !hedge {
			r.at = c.lastDone.Add(gap)
		}
	} else {
		c.misses++
		if !hedge {
			r.at = c.lastFire.Add(min(bt<<min(c.misses-1, 16), t.W()))
		}
	}
	c.rungs = append(c.rungs, r)
}

// fireRung sends r. A turn whose voter left the candidates or has a
// clock fetch in flight is skipped: that fetch is its turn. Any other
// fire goes to the fastest free voter instead.
func (t *ChainTracker) fireRung(cands []clockCandidate, r clockRung, now time.Time) {
	c := &t.clock
	if r.kind == "probe" {
		c.scheduled = false
	}
	id := r.id
	if id == "" || c.busy(id) || !slices.ContainsFunc(cands, func(cand clockCandidate) bool { return cand.id == id }) {
		cand, ok := c.fastest(cands)
		println("DBGRUNG", r.kind, r.id, ok, cand.id, len(cands), cands[0].id, c.busy(cands[0].id))
		if r.turn || !ok {
			c.lastDone = now
			return
		}
		id = cand.id
	}
	f := t.fire(id, now, r.kind)
	if f == nil {
		c.lastDone = now
		return
	}
	f.turn, f.again = r.turn, r.again
	c.sent = append(c.sent, f)
}

// edgeVoters are the voters an idle edge's fetch to first is sent beside:
// the next fastest by round trip plus lateness, so q voters answer at
// once. After an idle W the top sight is blocks old and the edge's answer
// jumps over blocks the pins lack: one vote on it is a root of its own
// that the older votes outweigh, so q answers link it in one round trip,
// not one after another (spec §6.8 catch-up).
func (t *ChainTracker) edgeVoters(cands []clockCandidate, first string) []string {
	c := &t.clock
	var ids []string
	for _, cand := range cands {
		if cand.id != first {
			ids = append(ids, cand.id)
		}
	}
	slices.SortFunc(ids, func(a, b string) int { return cmp.Compare(c.score(a), c.score(b)) })
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
// The fastest go first.
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
		n := t.votes[cand.id]
		if t.alive(n) && t.rootOf(n) == pinRoot && t.clock.taken[cand.id].Before(seen) {
			out = append(out, cand)
		}
	}
	out = t.clock.byScore(out)
	return out[:min(len(out), lacks)]
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

// bestRTT is the lowest round trip the clock measured of cands,
// clockUnknownRTT before any.
func (c *headClock) bestRTT(cands []clockCandidate) time.Duration {
	best := time.Duration(0)
	for _, cand := range cands {
		if v := c.ups[cand.id]; v != nil && v.samples > 0 && (best == 0 || v.rtt < best) {
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
	println("DBGFIRE", now.Format("05.000"), id, kind, c.top(), c.target, len(c.rungs), c.misses, c.tails)
	seq := c.seq
	if !t.askHead(id, common.HeadNeedHash, 0, after, common.AskClock, func(a common.HeadAnswer) { t.clockAnswered(seq, f, a) }) {
		return nil
	}
	v := c.voter(id)
	v.firedAt, v.out = now, f
	return f
}

// clockAnswered takes the answer to fire f on the tracker's goroutine,
// after the vote it carried, and counts it by f's kind and outcome. A
// ladder fire's Ask another fetch met at or below f's top says nothing of
// the target (joined): the rung goes again at once to the same voter,
// once, whose fetch the taken answer then makes fresh. An answer below
// what a fetch sent before it brought marks its voter late (answered).
func (t *ChainTracker) clockAnswered(seq uint64, f *clockFire, a common.HeadAnswer) {
	c := &t.clock
	if seq != c.seq {
		t.countFire(f, a, "dropped")
		return
	}
	f.done = true
	if f == c.out {
		c.out = nil
	}
	if v := c.ups[f.id]; v != nil && v.out == f {
		v.out = nil
	}
	edge := f == c.edge
	now := time.Now()
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
	if a.Err == nil && ok && sh.h != nil && !a.At.IsZero() {
		c.answered(f.id, sh.h.number, a.At)
	}
	current := f.top == c.top()
	outcome := "failed"
	switch {
	case a.Err != nil || !ok || sh.h == nil:
	case sh.h.number > f.top:
		outcome = "new"
	case a.Outcome != common.AskFetched && f.ladder() && current && !f.again:
		c.rungs = slices.Insert(c.rungs, 0, clockRung{id: f.id, at: now, kind: f.kind, turn: f.turn, again: true})
		t.countFire(f, a, "early")
		return
	case !current:
		// Another channel brought the block while this voter still
		// answered the one before.
		outcome = "behind"
	case f.ladder():
		outcome = "early"
		c.kEarly = c.kEarly || f.kind == "probe"
	default:
		outcome = "same"
	}
	if current {
		c.lastDone = now
	}
	println("DBGANS", now.Format("05.000"), f.id, f.kind, outcome, f.top, c.top())
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
