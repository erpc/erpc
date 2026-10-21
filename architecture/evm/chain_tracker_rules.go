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
	atBase bool // the window below the head ends here, for good
	// capped: reorgWindow below the top of a tree without the pins. Fill
	// goes on once the tree holds the pins.
	capped bool
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

// pinRoot is the root of the pins' tree, nil while nothing is pinned.
func (t *ChainTracker) pinRoot() *node {
	if len(t.pins) == 0 {
		return nil
	}
	return rootOf(t.pins[0])
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
	if e == nil || e.atBase || e.capped || e.walked || r == t.pinRoot() {
		return
	}
	e.walk, e.walked, e.sender = t.window, true, sender
}

// endWalks ends each walk whose tree came to hold the pins, so the votes
// in it apply at once, and each walk no pending vote, ask answer or settle
// answer waits on any more; fill goes on from where it stopped.
func (t *ChainTracker) endWalks() {
	pinRoot := t.pinRoot()
	var awaited map[*node]bool
	for r, e := range t.exts {
		if e.walk == 0 {
			continue
		}
		if r != pinRoot {
			if awaited == nil {
				awaited = t.rootsHolding(false)
			}
			if awaited[r] {
				continue
			}
		}
		e.walk, e.sender = 0, ""
	}
}

// rootsHolding are the roots of the trees that hold a vote a walk keeps
// back, an ask's answer not yet the vote, or a settle's answer; with votes,
// also the trees of the votes and of the no-source vote.
func (t *ChainTracker) rootsHolding(votes bool) map[*node]bool {
	roots := make(map[*node]bool)
	add := func(n *node) {
		if t.alive(n) {
			roots[rootOf(n)] = true
		}
	}
	for _, n := range t.pending {
		add(n)
	}
	add(t.ask.pending)
	if s := t.settle; s != nil {
		for _, a := range s.answers {
			add(a)
		}
	}
	if votes {
		for _, n := range t.votes {
			add(n)
		}
		add(t.ask.vote)
	}
	return roots
}

// treeTop is the highest held header of r's tree.
func (t *ChainTracker) treeTop(r *node) *chainHeader {
	top := r.h
	t.walkTree(r, func(n *node) {
		if n.h.number > top.number {
			top = n.h
		}
	})
	return top
}

// walkTree calls fn for every node of r's tree; fn must not walk a tree.
func (t *ChainTracker) walkTree(r *node, fn func(*node)) {
	stack := append(t.stack[:0], r)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = append(stack[:len(stack)-1], n.children...)
		fn(n)
	}
	t.stack = stack[:0]
}

// keeps is the window (§8) for h in a tree whose highest header is top:
// inside the window below the head (the highest held header while nothing
// is pinned), and, for a tree without the pins, at most reorgWindow blocks
// below its own top, so a tree far above the head stays small. top is
// read only for a tree without the pins.
func (t *ChainTracker) keeps(top, h *chainHeader, pinTree bool) bool {
	if ref := t.windowRef(); ref != nil && !t.holds(ref, h) {
		return false
	}
	return pinTree || h.number+uint64(t.window) >= top.number
}

// outside ends the walk or fill e of root r when h, the next header down,
// lies outside the window: for good below the head's window, and capped
// below the floor of a tree without the pins.
func (t *ChainTracker) outside(e *extension, r *node, h *chainHeader, pinTree bool) bool {
	switch {
	case !t.keeps(nil, h, true):
		e.atBase = true
	case pinTree || t.keeps(t.treeTop(r), h, false):
		return false
	default:
		e.capped = true
	}
	e.walk, t.treesDirty = 0, true
	return true
}

// extendRoots starts the next step of every root's walk or fill.
func (t *ChainTracker) extendRoots() {
	if !t.canFetch() {
		return
	}
	pinRoot := t.pinRoot()
	for r, e := range t.exts {
		if !e.busy && !e.atBase && (!e.capped || r == pinRoot) {
			t.extend(r, e, r == pinRoot)
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

func (t *ChainTracker) extend(r *node, e *extension, pinTree bool) {
	if r.h.number == 0 {
		e.atBase, e.walk, t.treesDirty = true, 0, true
		return
	}
	// The parent is no newer than r: when even r's time puts its height
	// outside the window, the parent is outside too.
	if t.outside(e, r, &chainHeader{number: r.h.number - 1, timestamp: r.h.timestamp}, pinTree) {
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
	t.submitHeader(t.headerSpec(class, method, ref, upstream, false), func(h *chainHeader, _ string, err error) {
		if t.exts[r] != e {
			return
		}
		e.busy = false
		switch {
		case err == nil && h.hash == want && h.number+1 == r.h.number:
			t.extended(r, e, h)
		case err == nil && h.hash == want:
			// The parent's hash under another number: no answer joins this
			// root, so its extension ends and it stays a root.
			e.atBase, e.walk, t.treesDirty = true, 0, true
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
	if t.outside(e, r, h, r == t.pinRoot()) {
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
func (t *ChainTracker) submitHeader(spec fetchSpec, done func(h *chainHeader, from string, err error)) {
	w := t.fetch.submit(nil, spec)
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
	settleDeferred                        // it would wait on a backoff
	settleDecided
)

// settleState is one settle (§6 rule 5): latest asked from up to 3
// authoritative upstreams that do not vote. Each answer, once its walk
// ended, adds one vote while the decision it was asked for stands.
type settleState struct {
	key       string               // the decision: settleKey
	answers   map[string]*node     // upstream id -> its answer
	out       map[string]fetchSpec // upstream id -> its ask still out
	inflight  bool
	decided   bool
	backedOff bool // an ask failed at once on its key's backoff
	retryAt   time.Time
	waiters   []chan settleOutcome
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

// startSettle asks for latest for the decision key and reports whether a
// round went out. With no upstream to ask, the settle ends undecided.
// client asks fail at once on a key's backoff instead of waiting it out.
func (t *ChainTracker) startSettle(key string, waiter chan settleOutcome, client bool) bool {
	if !t.canFetch() {
		return false
	}
	s := &settleState{key: key, answers: make(map[string]*node), out: make(map[string]fetchSpec), inflight: true}
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
		spec := t.headerSpec(classHead, "eth_getBlockByNumber", "latest", u, client)
		s.out[u] = spec
		t.submitHeader(spec, func(h *chainHeader, from string, err error) {
			if t.settle != s {
				return
			}
			delete(s.out, u)
			if err != nil {
				s.backedOff = s.backedOff || errors.Is(err, errFetchBackoff)
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
// votes and the answers. A tie, or no answer, leaves the pins and allows
// the next settle after the scheduler's backoff. The pins move before the
// waiters hear the outcome, so they read the pins it left.
func (t *ChainTracker) endSettle(s *settleState) {
	s.inflight = false
	path, _, tied, _ := t.decide()
	if path != nil {
		t.apply(path)
	}
	answered := 0
	for _, a := range s.answers {
		if t.alive(a) {
			answered++
		}
	}
	switch {
	case answered == 0 && s.backedOff:
		t.undecided(s, settleDeferred)
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
}

// nextSettleDelay is the scheduler's backoff for a settle that stays
// undecided: 200 ms, doubling, capped at 5 s.
func (t *ChainTracker) nextSettleDelay() time.Duration {
	t.settleIn = min(max(2*t.settleIn, fetchBackoffMin), fetchBackoffMax)
	return t.settleIn
}

// maySettle: one settle at a time. A new one waits for the round in
// flight and for an undecided one's backoff; a decided one stands while
// its decision does.
func (t *ChainTracker) maySettle(key string) bool {
	switch s := t.settle; {
	case s == nil:
		return true
	case s.inflight:
		return false
	case s.decided:
		return s.key != key
	default:
		return !time.Now().Before(s.retryAt)
	}
}

// requestSettle is ReconfirmPin's settle, on the client's path: it joins
// the round in flight, or starts one with client asks. A settle that would
// wait on a backoff, a fetch's or its own, reports deferred at once.
func (t *ChainTracker) requestSettle(settled chan settleOutcome) {
	if s := t.settle; s != nil {
		switch {
		case s.inflight && !t.waitsBackoff(s):
			s.waiters = append(s.waiters, settled)
			return
		case s.inflight || !s.decided && time.Now().Before(s.retryAt):
			report(settled, settleDeferred)
			return
		}
	}
	_, key, _, _ := t.decide()
	if !t.startSettle(key, settled, true) {
		report(settled, settleUnanswered)
	}
}

// waitsBackoff reports whether an ask of s still out waits out its key's
// backoff before it starts.
func (t *ChainTracker) waitsBackoff(s *settleState) bool {
	for _, spec := range s.out {
		if t.fetch.waitsBackoff(spec) {
			return true
		}
	}
	return false
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

// askWanted reports whether rule 6 asks once the next ask is due: demand,
// no source vote that counts, and no ask out.
func (t *ChainTracker) askWanted() bool {
	if !t.canFetch() || t.ask.busy || !t.demanded() {
		return false
	}
	for u := range t.votes {
		if t.counts(u) {
			return false
		}
	}
	return true
}

// maybeAsk asks the first authoritative upstream for latest; after an ask
// that failed, the next one sweeps the whole list in selection order.
func (t *ChainTracker) maybeAsk() {
	now := time.Now()
	if !t.askWanted() || now.Before(t.askDue()) {
		return
	}
	t.ask.last = now
	targets := []string{""}
	if n, ok := t.network.(interface{ EvmAuthoritativeUpstreams(string) []string }); ok {
		ups := n.EvmAuthoritativeUpstreams(t.selector)
		if len(ups) == 0 {
			t.ask.failed = true
			return
		}
		targets = ups[:1]
		if t.ask.failed {
			targets = ups
		}
	}
	t.askSeq++
	t.ask.busy = true
	t.askNext(t.askSeq, targets)
}

// askNext asks targets[0]; on a failure the next target is asked. seq
// names the ask: an answer to an ask from before a reset is dropped.
func (t *ChainTracker) askNext(seq uint64, targets []string) {
	target := targets[0]
	t.submitHeader(t.headerSpec(classHead, "eth_getBlockByNumber", "latest", target, false), func(h *chainHeader, from string, err error) {
		if seq != t.askSeq {
			return
		}
		if err != nil && len(targets) > 1 {
			t.askNext(seq, targets[1:])
			return
		}
		t.ask.busy, t.ask.failed = false, err != nil
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

// demanded reports whether newHeads demand stands for this tracker: an
// open tap of the network for the network-wide tracker; a group tracker's
// own integrity follow tap, since a network tap never reads a group.
func (t *ChainTracker) demanded() bool {
	if t.selector != "" {
		return t.following.Load()
	}
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

// rearm ticks the tracker at its next deadline: the next no-source ask, the
// end of an undecided settle's backoff, or the end of the cold window, when
// sources that never reported stop counting for the delivery gate. It runs
// after every message, so a deadline that fires never hides a later one.
func (t *ChainTracker) rearm() {
	now := time.Now()
	var at time.Time
	if t.askWanted() {
		if due := t.askDue(); due.After(now) {
			at = due
		}
	}
	if s := t.settle; s != nil && !s.inflight && !s.decided && s.retryAt.After(now) && (at.IsZero() || s.retryAt.Before(at)) {
		at = s.retryAt
	}
	if cold := t.since.Add(t.LiveBound()); cold.After(now) && (at.IsZero() || cold.Before(at)) {
		at = cold
	}
	if at.Equal(t.wakeAt) {
		return
	}
	t.wakeAt = at
	switch {
	case at.IsZero():
		if t.wake != nil {
			t.wake.Stop()
		}
	case t.wake == nil:
		t.wake = time.AfterFunc(at.Sub(now), func() { t.mb.put(trackerMsg{kind: msgTick}) })
	default:
		t.wake.Reset(at.Sub(now))
	}
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
	t.following.Store(true)
	t.mb.put(trackerMsg{kind: msgTick})
	defer func() {
		t.following.Store(false)
		t.mb.put(trackerMsg{kind: msgTick})
	}()
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
