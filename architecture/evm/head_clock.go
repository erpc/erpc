package evm

import (
	"context"
	"slices"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
)

// clockFit is how many recent blocks the block clock's phase fits.
const clockFit = 32

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
	// off is where a fire is sent relative to the next block's predicted
	// sight, less the target's round trip: it moves up by 0.95 step after
	// an early answer and down by 0.05 step after a timely one, so it
	// settles where 5% of the fires come early (k ≈ 1.65 at any spread).
	off    time.Duration
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
	// (ChainTracker.LatestRead); edgeDone once it answered.
	edge     *clockFire
	edgeDone bool
	// seq names the clock's generation: a reset moves it on, so an answer
	// from before the reset is dropped.
	seq uint64
}

type clockSight struct {
	number uint64
	at     time.Time
}

type clockFire struct {
	id   string
	sent time.Time
	// top is the clock's top sight when it was sent: an answer above it
	// is a new block.
	top    uint64
	hedged bool
}

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

// saw records the first local sight of number at at.
func (c *headClock) saw(number uint64, at time.Time) {
	if n := len(c.sights); n > 0 && number <= c.sights[n-1].number {
		return
	}
	if c.scheduled && c.out == nil && number >= c.target {
		c.skips++
	}
	c.scheduled = false
	c.sights = append(c.sights, clockSight{number, at})
	if over := len(c.sights) - clockFit; over > 0 {
		c.sights = slices.Delete(c.sights, 0, over)
	}
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

// predict is when number's first sight is due: a robust fit of the recent
// sights' phase, the median of each sight's time less its number's
// multiple of bt, plus number's multiple of bt.
func (c *headClock) predict(number uint64, bt time.Duration) time.Time {
	top := c.sights[len(c.sights)-1]
	offsets := make([]time.Duration, len(c.sights))
	for i, s := range c.sights {
		offsets[i] = s.at.Sub(top.at) - time.Duration(s.number-top.number)*bt
	}
	slices.Sort(offsets)
	phase := offsets[len(offsets)/2]
	return top.at.Add(phase + time.Duration(number-top.number)*bt)
}

// due is when the tracker must look at the clock again: its next fire, or
// the deadline that hedges the fetch in flight; zero while none is set.
func (c *headClock) due() time.Time { return c.nextAt }

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
// order: each with a poller, not backing off.
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
// sight, round trip plus lateness, round-robin among those within one
// round trip of it, so tip calls spread. ok false with none.
func (t *ChainTracker) pickTarget(cands []clockCandidate, skip string, now time.Time, bt time.Duration) (clockCandidate, bool) {
	c := &t.clock
	var turn *clockCandidate
	var turnAt time.Time
	for i := range cands {
		cand := &cands[i]
		if cand.id == skip || cand.interval <= 0 {
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
	var near []clockCandidate
	for _, cand := range cands {
		if cand.id != skip && score(cand.id) <= best+bestRTT {
			near = append(near, cand)
		}
	}
	c.rr++
	return near[c.rr%len(near)], true
}

// clockDemand reports whether the clock runs, and the fastest pace its
// demand needs heads at (0 every block): a read of latest within W, or a
// tap that needs newHeads (demand).
func (t *ChainTracker) clockDemand(now time.Time) (pace time.Duration, ok bool) {
	if at := t.latestReadAt.Load(); at > 0 && now.Sub(time.Unix(0, at)) < t.W() {
		return 0, true
	}
	return t.demand()
}

// runClock runs after every batch: it counts the fires a push made
// needless, ends a served idle edge, hedges a late fetch, and fires or
// schedules the next fetch while the lane has demand (spec §6.8).
func (t *ChainTracker) runClock() {
	c := &t.clock
	if c.skips > 0 {
		telemetry.CounterHandle(telemetry.MetricHeadClockTotal, t.projectId(), t.networkLabel(), t.laneLabel(), "skipped_push").Add(float64(c.skips))
		c.skips = 0
	}
	now, bt := time.Now(), t.BlockTime()
	edge := t.edge.Load()
	if edge != nil && c.edgeDone {
		close(*edge)
		t.edge.Store(nil)
		edge, c.edge, c.edgeDone = nil, nil, false
	}
	c.nextAt = time.Time{}
	pace, demanded := t.clockDemand(now)
	if !demanded && edge == nil {
		c.scheduled = false
		return
	}
	if c.out != nil {
		if edge != nil && c.edge == nil {
			c.edge = c.out
		}
		if c.hedge == nil && !c.out.hedged && c.out.top == c.top() {
			if at := c.out.sent.Add(c.hedgeAfter(c.out.id, bt)); now.Before(at) {
				c.nextAt = at
			} else {
				c.out.hedged = true
				if cand, ok := t.pickTarget(t.clockCandidates(), c.out.id, now, bt); ok {
					c.hedge = t.fire(cand.id, now)
					telemetry.CounterHandle(telemetry.MetricHeadClockTotal, t.projectId(), t.networkLabel(), t.laneLabel(), "hedge").Inc()
				}
			}
		}
		return
	}
	if cand, ok := t.staleVoter(t.clockCandidates()); ok {
		if c.out = t.fire(cand.id, now); c.out != nil {
			if edge != nil {
				c.edge = c.out
			}
			telemetry.CounterHandle(telemetry.MetricHeadClockTotal, t.projectId(), t.networkLabel(), t.laneLabel(), "catch_up").Inc()
			return
		}
	}
	if target := c.top() + 1; target != c.target {
		c.target, c.early, c.retryAt = target, 0, time.Time{}
	}
	at := now
	if edge == nil && len(c.sights) > 0 {
		at = t.fireAt(bt, pace)
	}
	if now.Before(at) {
		c.nextAt, c.scheduled = at, true
		return
	}
	cands := t.clockCandidates()
	cand, ok := t.pickTarget(cands, "", now, bt)
	if !ok {
		if edge != nil {
			c.edgeDone = true
			c.nextAt = now
		}
		return
	}
	c.out = t.fire(cand.id, now)
	if edge != nil {
		c.edge = c.out
	}
}

// staleVoter is a voter the clock fetches now because a counted vote stands
// above the pins off their tree (spec §6.8 catch-up): a tracker no history
// serves holds a vote that jumped blocks as a root of its own, and rule 4
// weighs it against the older votes on the pins, so the pins stay until
// those voters vote again. It is a voter whose vote is on the pins and
// whose last fetch the clock took was sent before that vote was sighted;
// each voter is fetched once per such vote.
func (t *ChainTracker) staleVoter(cands []clockCandidate) (clockCandidate, bool) {
	head := t.head()
	if head == nil {
		return clockCandidate{}, false
	}
	pinRoot := t.pinRoot()
	var ahead *node
	for u, n := range t.votes {
		if t.counts(u) && t.alive(n) && n.h.number > head.number && t.rootOf(n) != pinRoot && (ahead == nil || n.h.number > ahead.h.number) {
			ahead = n
		}
	}
	if ahead == nil {
		return clockCandidate{}, false
	}
	seen := t.clock.sightOf(ahead.h.number)
	for _, cand := range cands {
		n := t.votes[cand.id]
		if t.alive(n) && t.rootOf(n) == pinRoot && t.clock.taken[cand.id].Before(seen) {
			return cand, true
		}
	}
	return clockCandidate{}, false
}

// fireAt is when the clock fetches its target: the target's predicted
// sight plus the tracked offset, less the expected target's round trip; a
// retry's own time after an early answer; after misses, the last fire plus
// bt doubled per miss, at most W; never sooner than pace after the last
// fire.
func (t *ChainTracker) fireAt(bt, pace time.Duration) time.Time {
	c := &t.clock
	if !c.offSet {
		c.off, c.offSet = bt/10, true
	}
	var at time.Time
	switch {
	case c.misses > 0:
		at = c.lastFire.Add(min(bt<<min(c.misses-1, 16), t.W()))
	case c.early > 0:
		at = c.retryAt
	default:
		at = c.predict(c.target, bt).Add(c.off - c.bestRTT())
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

// fire sends the clock's fetch to voter id: a hash Ask newer than the
// last answer the clock took from it and than the top sight, so neither a
// fetch the clock already read nor one sent before the top block was seen
// meets it.
func (t *ChainTracker) fire(id string, now time.Time) *clockFire {
	c := &t.clock
	f := &clockFire{id: id, sent: now, top: c.top()}
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
// after the vote it carried: a new block resets the misses and pulls the
// offset earlier; an answer still at f's top was early, which pushes the
// offset later and schedules the one retry, or a miss once the retry was
// spent; an answer below what another voter brought marks its voter late.
func (t *ChainTracker) clockAnswered(seq uint64, f *clockFire, a common.HeadAnswer) {
	c := &t.clock
	if seq != c.seq {
		return
	}
	switch f {
	case c.out:
		c.out = c.hedge
		c.hedge = nil
	case c.hedge:
		c.hedge = nil
	}
	if f == c.edge {
		c.edgeDone = true
	}
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
	outcome := "failed"
	step := max(bt/20, time.Millisecond)
	switch {
	case a.Err != nil || !ok || sh.h == nil:
	case sh.h.number > f.top:
		outcome, c.misses = "new", 0
		c.sampleLate(f.id, 0)
		if !f.hedged {
			c.off = max(c.off-step/20, -bt/2)
		}
	case c.top() > f.top:
		// Another channel brought the block while this voter still
		// answered the one before: it lags.
		outcome = "early"
		if seen := c.sightOf(c.top()); !seen.IsZero() {
			c.sampleLate(f.id, now.Sub(seen))
		}
	default:
		outcome = "early"
		c.off = min(c.off+step*19/20, bt)
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
	telemetry.CounterHandle(telemetry.MetricHeadClockTotal, t.projectId(), t.networkLabel(), t.laneLabel(), outcome).Inc()
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

// LatestRead marks a read of the lane's latest (spec §6.8 demand): for W
// after it the block clock runs. The first read after W with no demand is
// the idle edge: every read of that edge waits for the clock's first
// fetch, at most W or until ctx ends, so none serves a Head older than
// it; with no pins yet nothing waits, and tags pass through (§7).
func (t *ChainTracker) LatestRead(ctx context.Context) {
	now := time.Now()
	prev := t.latestReadAt.Swap(now.UnixNano())
	edge := t.edge.Load()
	if edge == nil && (prev == 0 || now.Sub(time.Unix(0, prev)) >= t.W()) {
		if pace, ok := t.demand(); !ok || pace > 0 {
			ch := make(chan struct{})
			if t.edge.CompareAndSwap(nil, &ch) {
				t.mb.put(trackerMsg{kind: msgTick})
			}
			edge = t.edge.Load()
		}
	}
	if edge == nil || t.state.Load().viewHead() == nil {
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
// the block clock runs for W after it, without the idle edge's wait.
func (t *ChainTracker) MarkRead() {
	if prev := t.latestReadAt.Swap(time.Now().UnixNano()); prev == 0 || time.Since(time.Unix(0, prev)) >= t.W() {
		t.mb.put(trackerMsg{kind: msgTick})
	}
}
