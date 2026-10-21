package evm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
)

// Spec §6 bounds; variables so tests run fast.
var (
	settleAsks       = 3                // upstreams a settle asks
	noSourceAskMax   = 12 * time.Second // longest gap between no-source asks
	noSourceAskRetry = time.Second      // after an ask that found no answer
	followTapPause   = time.Second      // before a follow tap that failed restarts
)

// extension walks or fills one held root downward (§6 rules 1 and 2, §8):
// by number, each answer checked against the parentHash of the block above,
// and a mismatch fetched by hash. A walk takes class 1 and at most
// reorgWindow blocks; fill takes class 3 and runs until the root joins
// another tree or reaches the window base.
type extension struct {
	sender string // the voter a walk asks by hash; "" sweeps
	walk   int    // blocks left to walk; 0 fills
	walked bool   // a walk ran from this tree already
	byHash bool   // the next height's by-number answer did not link
	busy   bool
	atBase bool
}

// canFetch: a tracker without a network to sweep never walks, fills,
// settles or asks; its votes count once held.
func (t *ChainTracker) canFetch() bool {
	return t.network != nil && t.fetch != nil && t.fetch.sweep != nil
}

// walking reports whether n's tree is still being walked; votes in it wait.
func (t *ChainTracker) walking(n *node) bool {
	e := t.exts[rootOf(n)]
	return e != nil && e.walk > 0 && !e.atBase
}

func (t *ChainTracker) anyWalking(ns map[string]*node) bool {
	for _, n := range ns {
		if t.alive(n) && t.walking(n) {
			return true
		}
	}
	return false
}

// walkFrom is rule 1 for a vote nd whose tree does not reach the pins: walk
// it in class 1, asking its sender by hash on a mismatch. An empty tracker's
// first vote, and a vote more than maxBackfill after the head, become roots
// at once.
func (t *ChainTracker) walkFrom(nd *node, sender string) {
	if !t.canFetch() || len(t.pins) == 0 || t.farAhead(nd.h) {
		return
	}
	r := rootOf(nd)
	e := t.exts[r]
	if e == nil || e.atBase || e.walked || r == rootOf(t.pins[0]) {
		return
	}
	e.walk, e.walked, e.sender = t.window, true, sender
}

// extendRoots starts the next step of every root's walk or fill.
func (t *ChainTracker) extendRoots() {
	if !t.canFetch() {
		return
	}
	for r, e := range t.exts {
		if !e.busy && !e.atBase {
			t.extend(r, e)
		}
	}
}

// windowRef is the header the window hangs from: the head, or the highest
// held header while nothing is pinned.
func (t *ChainTracker) windowRef() *chainHeader {
	if h := t.head(); h != nil {
		return h
	}
	return t.maxHeld
}

func (t *ChainTracker) extend(r *node, e *extension) {
	// The parent is no newer than r: when even r's time puts its height
	// outside the window, the parent is outside too.
	if r.h.number == 0 || !t.holds(t.windowRef(), &chainHeader{number: r.h.number - 1, timestamp: r.h.timestamp}) {
		e.atBase, e.walk = true, 0
		return
	}
	class := classFill
	if e.walk > 0 {
		class = classHead
	}
	want := r.h.parentHash
	method, ref, upstream := "eth_getBlockByNumber", fmt.Sprintf("0x%x", r.h.number-1), ""
	if e.byHash {
		method, ref, upstream = "eth_getBlockByHash", fmt.Sprintf("0x%x", want), e.sender
	}
	e.busy = true
	t.submitHeader(class, method, ref, upstream, func(h *chainHeader, _ string, err error) {
		if t.exts[r] != e {
			return
		}
		e.busy = false
		if err == nil && h.hash == want && h.number+1 == r.h.number {
			t.extended(r, e, h)
			return
		}
		switch {
		case !e.byHash:
			e.byHash = true
		case e.walk > 0:
			// The sender does not serve its own parent: the walk ends here,
			// its lowest header stays a root, and fill sweeps for the rest.
			e.walk, e.sender = 0, ""
		}
	})
}

// extended holds a root's fetched parent; the root joins under it, and the
// walk or fill moves to the parent while the parent is itself a root.
func (t *ChainTracker) extended(r *node, e *extension, h *chainHeader) {
	if !t.holds(t.windowRef(), h) {
		e.atBase, e.walk = true, 0
		return
	}
	p := t.nodes[h.hash]
	if p == nil {
		p = t.hold(h)
	}
	if p.parent != nil {
		return
	}
	if ne := t.exts[p]; ne != nil {
		ne.sender, ne.walked = e.sender, e.walked
		ne.walk = max(0, e.walk-1)
	}
}

// submitHeader queues a tracker-owned header fetch; the tracker waits on it
// and done runs on the tracker's goroutine with the answer.
func (t *ChainTracker) submitHeader(class fetchClass, method, ref, upstream string, done func(h *chainHeader, from string, err error)) {
	w := t.fetch.submit(nil, t.headerSpec(class, method, ref, upstream, false))
	go func() {
		res := w.wait(t.ctx)
		h, _ := res.val.(*chainHeader)
		err := res.err
		if err == nil && h == nil {
			err = errFetchUndecoded
		}
		t.mb.put(trackerMsg{kind: msgCall, fn: func() { done(h, res.upstream, err) }})
	}()
}

// heldOrHold returns the held node for h, holding it first when new.
func (t *ChainTracker) heldOrHold(h *chainHeader) *node {
	if nd := t.nodes[h.hash]; nd != nil {
		return nd
	}
	return t.hold(h)
}

// settleOutcome is what a settle round came to.
type settleOutcome uint8

const (
	settleUnanswered settleOutcome = iota // no upstream answered, or none to ask
	settleTied                            // answers came, and rule 4 still ties
	settleDecided
)

// settleState is one settle (§6 rule 5): latest asked from up to 3
// authoritative upstreams that do not vote. Each answer, once its walk
// ended, adds one vote while the votes it was asked for stand.
type settleState struct {
	key      string           // voteKey of the votes it was asked for
	answers  map[string]*node // upstream id -> its answer
	asked    int              // answers still out
	inflight bool
	decided  bool
	retryAt  time.Time
	waiters  []chan settleOutcome
}

func (s *settleState) notify(o settleOutcome) {
	for _, w := range s.waiters {
		report(w, o)
	}
	s.waiters = nil
}

// report hands a settle's outcome to one waiter; it never blocks.
func report(ch chan settleOutcome, o settleOutcome) {
	select {
	case ch <- o:
	default:
	}
}

// startSettle asks for latest for the votes named key and reports whether
// a round went out. With no upstream to ask, the settle ends undecided.
func (t *ChainTracker) startSettle(key string, waiter chan settleOutcome) bool {
	if !t.canFetch() {
		return false
	}
	s := &settleState{key: key, answers: make(map[string]*node), inflight: true}
	if prev := t.settle; prev != nil && prev.inflight {
		s.waiters = prev.waiters
	}
	if waiter != nil {
		s.waiters = append(s.waiters, waiter)
	}
	t.settle = s
	ups := t.settleUpstreams(t.baseVotes())
	if len(ups) == 0 {
		s.inflight = false
		t.undecided(s, settleUnanswered)
		return false
	}
	for _, u := range ups {
		s.asked++
		t.submitHeader(classHead, "eth_getBlockByNumber", "latest", u, func(h *chainHeader, from string, err error) {
			if t.settle != s {
				return
			}
			s.asked--
			if err != nil {
				return
			}
			who := u
			if who == "" {
				who = from
			}
			nd := t.heldOrHold(h)
			s.answers[who] = nd
			t.walkFrom(nd, who)
		})
	}
	return true
}

// endSettle decides once every answer is in and walked: rule 4 over the
// votes and the answers. A tie, or no answer, leaves the pins and runs the
// settle again after the scheduler's backoff.
func (t *ChainTracker) endSettle(s *settleState) {
	s.inflight = false
	s.retryAt = time.Now().Add(t.nextSettleDelay())
	tied := t.choose()
	answered := 0
	for _, a := range s.answers {
		if t.alive(a) {
			answered++
		}
	}
	switch {
	case answered == 0:
		t.undecided(s, settleUnanswered)
	case tied:
		t.undecided(s, settleTied)
	default:
		s.decided, t.settleIn = true, 0
		telemetry.CounterHandle(telemetry.MetricChainTrackerSettleTotal, t.projectId(), t.networkLabel(), "decided").Inc()
		s.notify(settleDecided)
	}
}

func (t *ChainTracker) undecided(s *settleState, o settleOutcome) {
	if s.retryAt.IsZero() {
		s.retryAt = time.Now().Add(t.nextSettleDelay())
	}
	telemetry.CounterHandle(telemetry.MetricChainTrackerSettleTotal, t.projectId(), t.networkLabel(), "undecided").Inc()
	s.notify(o)
	t.scheduleWake(s.retryAt)
}

// nextSettleDelay is the scheduler's backoff for a settle that stays
// undecided: 200 ms, doubling, capped at 5 s.
func (t *ChainTracker) nextSettleDelay() time.Duration {
	t.settleIn = min(max(2*t.settleIn, fetchBackoffMin), fetchBackoffMax)
	return t.settleIn
}

// requestSettle is ReconfirmPin's settle: it joins the round in flight, or
// starts one for the current votes.
func (t *ChainTracker) requestSettle(settled chan settleOutcome) {
	if s := t.settle; s != nil && s.inflight {
		s.waiters = append(s.waiters, settled)
		return
	}
	if !t.startSettle(voteKey(t.baseVotes()), settled) {
		report(settled, settleUnanswered)
	}
}

// settleUpstreams are up to settleAsks authoritative upstreams that do not
// vote, in selection order. A network without intake is swept once in
// selection order.
func (t *ChainTracker) settleUpstreams(base []voter) []string {
	n, ok := t.network.(interface{ EvmAuthoritativeUpstreams(string) []string })
	if !ok {
		return []string{""}
	}
	var out []string
	for _, id := range n.EvmAuthoritativeUpstreams(t.selector) {
		if slices.ContainsFunc(base, func(v voter) bool { return v.who == id }) {
			continue
		}
		out = append(out, id)
		if len(out) == settleAsks {
			break
		}
	}
	return out
}

// askState is §6 rule 6: with demand and no live source, the tracker asks
// the first authoritative upstream for latest each block time; that answer
// is the only vote until the next.
type askState struct {
	vote, pending       *node
	voter, pendingVoter string
	conflict            bool // vote is not on the pins it was asked against
	busy                bool
	last                time.Time // when the last ask went out
	failed              bool      // it found no upstream, or no answer
}

// askDue is the next ask's time: one block time, at most noSourceAskMax,
// after the last; sooner after one that failed. Block time is read now, so
// fill that dates the pins shortens the wait.
func (t *ChainTracker) askDue() time.Time {
	every := min(t.BlockTime(), noSourceAskMax)
	if t.ask.failed {
		every = min(every, noSourceAskRetry)
	}
	return t.ask.last.Add(every)
}

func (t *ChainTracker) maybeAsk() {
	if !t.canFetch() || t.ask.busy {
		return
	}
	now := time.Now()
	if due := t.askDue(); now.Before(due) {
		t.scheduleWake(due)
		return
	}
	if !t.demanded() {
		return
	}
	for u := range t.votes {
		if t.counts(u) {
			return
		}
	}
	t.ask.last = now
	target := ""
	if n, ok := t.network.(interface{ EvmAuthoritativeUpstreams(string) []string }); ok {
		ups := n.EvmAuthoritativeUpstreams(t.selector)
		if len(ups) == 0 {
			t.ask.failed = true
			t.scheduleWake(t.askDue())
			return
		}
		target = ups[0]
	}
	t.ask.busy = true
	t.submitHeader(classHead, "eth_getBlockByNumber", "latest", target, func(h *chainHeader, from string, err error) {
		t.ask.busy, t.ask.failed = false, err != nil
		t.scheduleWake(t.askDue())
		if err != nil {
			return
		}
		who := target
		if who == "" {
			who = from
		}
		nd := t.heldOrHold(h)
		t.ask.pending, t.ask.pendingVoter = nd, who
		t.walkFrom(nd, who)
	})
}

// demanded reports whether an open tap needs newHeads.
func (t *ChainTracker) demanded() bool {
	n, ok := t.network.(interface{ EvmHeadsDemanded() bool })
	return ok && n.EvmHeadsDemanded()
}

// conflicts reports whether a no-source answer leaves the pinned branch:
// neither pinned nor above the head on its branch.
func (t *ChainTracker) conflicts(nd *node) bool {
	if len(t.pins) == 0 || t.pinned(nd) {
		return false
	}
	return !isAncestor(t.pins[len(t.pins)-1], nd)
}

// scheduleWake ticks the tracker at at, unless an earlier tick is due.
func (t *ChainTracker) scheduleWake(at time.Time) {
	now := time.Now()
	if t.wakeAt.After(now) && !at.Before(t.wakeAt) {
		return
	}
	t.wakeAt = at
	d := max(at.Sub(now), 0)
	if t.wake == nil {
		t.wake = time.AfterFunc(d, func() { t.mb.put(trackerMsg{kind: msgTick}) })
		return
	}
	t.wake.Reset(d)
}

// follow runs the integrity follow tap (spec §12): it holds newHeads
// demand, so polls run without clients, hands each new head to the state
// prober and sets the head's lag behind the network's served tip. Out of
// range restarts it after the head.
func (t *ChainTracker) follow(ctx context.Context) {
	tn, ok := t.network.(interface {
		EvmTaps() (*indexer.Taps, string)
	})
	if !ok {
		return
	}
	taps, key := tn.EvmTaps()
	if taps == nil {
		return
	}
	latest := networkLatest(t.network)
	for ctx.Err() == nil {
		err := taps.RunChain(ctx, key, "integrity", "follow", t, nil, indexer.ReadOpts{}, func(b indexer.Batch) error {
			if len(b.Applied) == 0 {
				return nil
			}
			if p := stateProberFor(t.network.Id()); p != nil {
				p.onNewHead(int64(b.Next.Number))
			}
			if latest != nil {
				if served := latest(ctx); served > 0 {
					telemetry.MetricChainTrackerLag.WithLabelValues(t.projectId(), t.networkLabel(), t.group).
						Set(float64(max(0, served-int64(b.Next.Number))))
				}
			}
			return nil
		})
		if ctx.Err() != nil || errors.Is(err, indexer.ErrOutOfRange) {
			continue
		}
		log.Warn().Err(err).Str("network", t.networkLabel()).Str("group", t.group).
			Msg("integrity follow tap ended; restarting after the head")
		select {
		case <-ctx.Done():
		case <-time.After(followTapPause):
		}
	}
}
