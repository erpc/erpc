package evm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
)

// Spec §6 bounds; variables so tests run fast.
var (
	settleAsks     = 3           // targets a settle round Asks
	catchUpMax     = time.Second // longest gap between catch-ups while latest caps the sends
	followTapPause = time.Second // before a follow tap that failed restarts
)

// extension walks or fills one held root downward (§6 rules 1 and 2, §8):
// by number, each answer checked against the parentHash of the block above,
// and a mismatch fetched by hash. A walk takes class 1 and at most
// reorgWindow blocks; fill takes class 3 and runs until the root joins
// another tree or reaches the window base. Below reorgWindow blocks under
// the head, fill waits for demand (fillsBelow) without ending.
type extension struct {
	sender string // the voter a walk asks by hash; "" sweeps
	walk   int    // blocks left to walk; 0 fills
	walked bool   // a walk ran from this tree already
	byHash bool   // the next height's by-number answer did not link
	busy   bool
	atBase bool // the window below the head ends here, for good
	// capped: reorgWindow below the top of a tree without the pins. Fill
	// goes on once the tree holds the pins, or while a Read waits on fill
	// in it (fillRoots).
	capped bool
}

// canFetch: a tracker without a network to sweep never walks or fills;
// its votes count once held.
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

// keeps is the window (§9) for h in a tree whose highest header is top:
// inside the window below the head (the highest held header while nothing
// is pinned). A tree without the pins keeps a header more than
// reorgWindow above the head only within reorgWindow below its own top, so
// a tree far above the head stays small; below that the window alone
// bounds it, so a branch a re-anchor left keeps its fork point for fill to
// join. top is read only for a tree without the pins.
func (t *ChainTracker) keeps(top, h *chainHeader, pinTree bool) bool {
	ref := t.windowRef()
	if ref != nil && !t.holds(ref, h) {
		return false
	}
	return pinTree || (ref != nil && h.number <= ref.number+uint64(t.window)) || t.nearTop(top, h)
}

// nearTop reports whether h lies within reorgWindow below top.
func (t *ChainTracker) nearTop(top, h *chainHeader) bool {
	return h.number+uint64(t.window) >= top.number
}

// outside ends the walk or fill e of root r when h, the next header down,
// lies outside the window: for good below the head's window, and capped
// reorgWindow below the top of a tree neither uncapped, the pins' tree or
// one a Read waits on fill in, which bounds the fetches of a tree that may
// never join.
func (t *ChainTracker) outside(e *extension, r *node, h *chainHeader, uncapped bool) bool {
	switch {
	case !t.keeps(nil, h, true):
		e.atBase = true
	case uncapped || t.nearTop(t.treeTop(r), h):
		return false
	default:
		e.capped = true
	}
	e.walk, t.treesDirty = 0, true
	return true
}

// fillRoots are the roots of the trees that hold a cursor a Read waits on
// fill for; nil while none does.
func (t *ChainTracker) fillRoots() map[*node]bool {
	t.fillMu.Lock()
	defer t.fillMu.Unlock()
	var roots map[*node]bool
	for h := range t.fillReads {
		if n := t.nodes[h]; n != nil {
			if roots == nil {
				roots = make(map[*node]bool)
			}
			roots[rootOf(n)] = true
		}
	}
	return roots
}

// extendRoots starts the next step of every root's walk or fill.
func (t *ChainTracker) extendRoots() {
	if !t.canFetch() {
		return
	}
	pinRoot, reads := t.pinRoot(), t.fillRoots()
	for r, e := range t.exts {
		if uncapped := r == pinRoot || reads[r]; !e.busy && !e.atBase && (!e.capped || uncapped) {
			t.extend(r, e, uncapped)
		}
	}
}

// fillsBelow reports whether fill fetches height n: within reorgWindow
// blocks below the window's reference, or deeper while a Resumable tap is
// open or a Read waits on fill. Only consumers that resume from their own
// cursor read below reorgWindow, so the rest of the window is fetched for
// them alone.
func (t *ChainTracker) fillsBelow(n uint64) bool {
	ref := t.windowRef()
	return ref == nil || n+uint64(t.window) >= ref.number || t.deepFill.Load() > 0
}

// windowRef is the header the window hangs from: the head, or the highest
// held header while nothing is pinned.
func (t *ChainTracker) windowRef() *chainHeader {
	if h := t.head(); h != nil {
		return h
	}
	return t.maxHeld
}

func (t *ChainTracker) extend(r *node, e *extension, uncapped bool) {
	if r.h.number == 0 {
		e.atBase, e.walk, t.treesDirty = true, 0, true
		return
	}
	// The parent is no newer than r: when even r's time puts its height
	// outside the window, the parent is outside too.
	if t.outside(e, r, &chainHeader{number: r.h.number - 1, timestamp: r.h.timestamp}, uncapped) {
		return
	}
	class := classFill
	if e.walk > 0 {
		class = classHead
	} else if !t.fillsBelow(r.h.number - 1) {
		return
	}
	want := r.h.parentHash
	method, ref, upstream := "eth_getBlockByNumber", fmt.Sprintf("0x%x", r.h.number-1), ""
	if e.byHash {
		method, ref, upstream = "eth_getBlockByHash", fmt.Sprintf("0x%x", want), e.sender
	}
	e.busy = true
	t.submitHeader(t.headerSpec(class, method, ref, upstream, false), func(h *chainHeader, _ string, _ time.Time, err error) {
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
	if t.outside(e, r, h, r == t.pinRoot() || t.fillRoots()[r]) {
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
func (t *ChainTracker) submitHeader(spec fetchSpec, done func(h *chainHeader, from string, sent time.Time, err error)) {
	w := t.fetch.submit(nil, spec)
	t.fetch.bg.Go(func() {
		res := w.wait(t.ctx)
		h, _ := res.val.(*chainHeader)
		err := res.err
		if err == nil && h == nil {
			err = errFetchUndecoded
		}
		t.mb.put(trackerMsg{kind: msgCall, fn: func() { done(h, res.upstream, res.sent, err) }})
	})
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
	settleUnanswered settleOutcome = iota // no target answered, or none to Ask
	settleTied                            // answers came, and rule 4 still ties
	settleDeferred                        // it waits on an Ask's backoff, or its client left first
	settleMoved                           // the pins under its key moved before it ended
	settleDecided
)

// settleState is one settle round (§6 rule 5, Settle rounds): an Ask with a
// hash need of each of up to settleAsks targets for one key. Each answer,
// once its walk ended, is its voter's hash vote, or, from a target without
// a counted vote, one more vote for this decision.
type settleState struct {
	key      roundKey
	answers  map[string]*node // target id -> its answer
	out      int              // Asks still out
	inflight bool
	decided  bool
	// next is the soonest moment one of its targets' Asks may fetch again.
	next    time.Time
	retryAt time.Time
	waiters []chan settleOutcome
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

// headAsker is the network's Ask of one upstream's head (spec §6.8).
type headAsker interface {
	EvmAskHead(ctx context.Context, upstreamId string, need common.HeadNeed, reason common.AskReason) common.HeadAnswer
}

var errHeadAskPanicked = errors.New("head ask panicked")

// askHead Asks upstream id's head off the tracker's goroutine; done runs
// on it with the answer. A panic in the Ask is a failed answer. false: the
// network has no Ask.
func (t *ChainTracker) askHead(id string, need common.HeadNeed, reason common.AskReason, done func(common.HeadAnswer)) bool {
	n, ok := t.network.(headAsker)
	if !ok || t.fetch == nil {
		return false
	}
	t.fetch.bg.Go(func() {
		a := common.HeadAnswer{Err: errHeadAskPanicked}
		t.callNetwork("head-ask", func() { a = n.EvmAskHead(t.ctx, id, need, reason) })
		t.mb.put(trackerMsg{kind: msgCall, fn: func() { done(a) }})
	})
	return true
}

// heldAnswer holds an Ask's header; nil for a failed Ask.
func (t *ChainTracker) heldAnswer(a common.HeadAnswer) *node {
	if a.Err != nil || len(a.Header) == 0 {
		return nil
	}
	h, err := parseChainHeader(a.Header)
	if err != nil {
		return nil
	}
	return t.heldOrHold(h)
}

// startSettle starts round s: an Ask of each target with need, reason
// settle. With no target to Ask, s ends unanswered and false is returned.
func (t *ChainTracker) startSettle(s *settleState, targets []string, need common.HeadNeed, waiter chan settleOutcome) bool {
	s.answers, s.inflight = make(map[string]*node), true
	if waiter != nil {
		s.waiters = append(s.waiters, waiter)
	}
	t.settle = s
	for _, id := range targets {
		if t.askHead(id, need, common.AskSettle, func(a common.HeadAnswer) { t.settleAnswered(s, id, a) }) {
			s.out++
		}
	}
	if s.out > 0 {
		return true
	}
	s.inflight = false
	t.undecided(s, settleUnanswered)
	return false
}

// startDispute starts a round over k, a tie or a conflicting no-source
// answer, when one may start; skip is the voter whose answer k disputes.
func (t *ChainTracker) startDispute(k roundKey, skip string) bool {
	return t.maySettle(k) && t.startSettle(&settleState{key: k}, t.tieTargets(skip), common.HeadNeedHash, nil)
}

// settleAnswered takes target id's answer to round s. A failed Ask, a
// panic or backoff included, ends that target's part of the round.
func (t *ChainTracker) settleAnswered(s *settleState, id string, a common.HeadAnswer) {
	if t.settle != s {
		return
	}
	s.out--
	if !a.Next.IsZero() && (s.next.IsZero() || a.Next.Before(s.next)) {
		s.next = a.Next
	}
	nd := t.heldAnswer(a)
	if nd == nil {
		return
	}
	s.answers[id] = nd
	if t.votes[id] == nd {
		t.voteAt[id] = maxTime(t.voteAt[id], a.Sent)
	}
	t.walkFrom(nd, id)
}

// endSettle decides once every answer is in and walked: rule 4 over the
// votes and the answers. A round whose key no longer stands never reports
// decided: its answers no longer count, and the path came from votes
// alone. A tie, or no answer, allows the next round after the targets'
// Ask backoff. The pins move before the waiters hear the outcome, so they
// read the pins it left.
func (t *ChainTracker) endSettle(s *settleState) {
	s.inflight = false
	stands := t.stands(s.key)
	path, tie, _ := t.decide()
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
	case !stands:
		t.undecided(s, settleMoved)
	case answered == 0:
		t.undecided(s, settleUnanswered)
	case len(tie) > 0:
		t.undecided(s, settleTied)
	default:
		// The decision is the key now: the block it pinned is its pinned
		// side, so its answers count while that stands.
		s.decided, s.key = true, t.rekey(s.key)
		telemetry.CounterHandle(telemetry.MetricChainTrackerSettleTotal, t.projectId(), t.networkLabel(), "decided").Inc()
		// The decided answers back their branch now: waiters read that.
		t.gateSend()
		s.notify(settleDecided)
	}
}

// undecided ends s without a decision. The next round over its key waits
// for the soonest of its targets' Ask backoffs; with no Ask answered, for
// one block time.
func (t *ChainTracker) undecided(s *settleState, o settleOutcome) {
	s.retryAt = s.next
	if s.retryAt.IsZero() {
		s.retryAt = time.Now().Add(t.BlockTime())
	}
	telemetry.CounterHandle(telemetry.MetricChainTrackerSettleTotal, t.projectId(), t.networkLabel(), "undecided").Inc()
	s.notify(o)
}

// maySettle: one round at a time. A round over k waits for the round in
// flight; after a decided round over k it does not run while that key
// stands, after an undecided one not before its retry.
func (t *ChainTracker) maySettle(k roundKey) bool {
	switch s := t.settle; {
	case s == nil:
		return true
	case s.inflight:
		return false
	case s.key.name != k.name:
		return true
	case s.decided:
		return !t.stands(s.key)
	default:
		return !time.Now().Before(s.retryAt)
	}
}

// requestSettle is ReconfirmPin's settle: it joins the round in flight and
// waits for its answers, or runs one over the dispute rule 4 meets, else
// over the pin at number. A decided round over that key that still stands
// reports decided at once; an undecided one before its retry, deferred.
func (t *ChainTracker) requestSettle(settled chan settleOutcome, number int64) {
	if s := t.settle; s != nil && s.inflight {
		s.waiters = append(s.waiters, settled)
		return
	}
	k, skip := keyOf(nil, nil), ""
	if number >= 0 {
		k = keyOf(t.pinNode(uint64(number)), nil)
	}
	switch _, tie, conflict := t.decide(); {
	case len(tie) > 0:
		k = t.tieKey(tie)
	case conflict:
		k, skip = t.disputeOf(t.ask.vote), t.ask.voter
	}
	if s := t.settle; s != nil && s.key.name == k.name {
		switch {
		case s.decided && t.stands(s.key):
			report(settled, settleDecided)
			return
		case !s.decided && time.Now().Before(s.retryAt):
			report(settled, settleDeferred)
			return
		}
	}
	t.startSettle(&settleState{key: k}, t.tieTargets(skip), common.HeadNeedHash, settled)
}

// tieTargets are the targets of a round over a tie, a conflicting
// no-source answer or a client's re-check (spec §6.8): the voters without a
// counted vote here, then the non-voters, in selection order, up to
// settleAsks; never skip, the voter whose answer is disputed.
func (t *ChainTracker) tieTargets(skip string) []string {
	var out []string
	for _, id := range t.voters.Voters {
		if id != skip && !(t.counts(id) && t.alive(t.votes[id])) && len(out) < settleAsks {
			out = append(out, id)
		}
	}
	for _, id := range t.voters.Others {
		if id != skip && len(out) < settleAsks {
			out = append(out, id)
		}
	}
	return out
}

// refreshTargets are §6.3's refresh for voter off's vote nd off the pins:
// the voters whose counted vote backs the pins at nd's branch with
// evidence older than nd's, oldest first, up to settleAsks.
func (t *ChainTracker) refreshTargets(off string, nd *node) []string {
	from, at := t.branchOf(nd).h.number, t.voteAt[off]
	var out []string
	for u, v := range t.votes {
		if u != off && t.counts(u) && t.alive(v) && !t.conflicts(v) && v.h.number >= from && t.voteAt[u].Before(at) {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return t.voteAt[a].Compare(t.voteAt[b]) })
	return out[:min(len(out), settleAsks)]
}

// refreshed reports whether voter u's vote nd off the pins was refreshed
// (spec §6.3): a round over its dispute ended, or no vote behind the pins
// there is older. Otherwise it starts that round when one may start,
// Asking each target with a fork need: a fetch sent before the round, as
// old as the vote it would refresh, is no answer.
func (t *ChainTracker) refreshed(u string, nd *node) bool {
	k := t.disputeOf(nd)
	if s := t.settle; s != nil && s.key.name == k.name {
		return !s.inflight
	}
	targets := t.refreshTargets(u, nd)
	if len(targets) == 0 {
		return true
	}
	if t.maySettle(k) {
		t.startSettle(&settleState{key: k}, targets, common.HeadNeedFork, nil)
	}
	return false
}

// askState is §6 rule 6: with demand and no counted vote, the tracker Asks
// the first voter in selection order with a hash need, once per cadence:
// the Ask's own debounce paces it (due, its answer's Next). That answer is
// the only vote until the next.
type askState struct {
	vote, pending       *node
	voter, pendingVoter string
	out                 bool      // an Ask is out
	due                 time.Time // when the voter's Ask may fetch again
}

// noSourceWanted reports whether rule 6 Asks: demand, a voter, no counted
// vote and no Ask out.
func (t *ChainTracker) noSourceWanted() bool {
	if t.ask.out || len(t.voters.Voters) == 0 || !t.demanded() {
		return false
	}
	for u := range t.votes {
		if t.counts(u) {
			return false
		}
	}
	return true
}

// askNoSource is rule 6's Ask, reason no_source, once it is due.
func (t *ChainTracker) askNoSource() {
	if !t.noSourceWanted() || time.Now().Before(t.ask.due) {
		return
	}
	id, seq := t.voters.Voters[0], t.askSeq
	t.ask.out = t.askHead(id, common.HeadNeedHash, common.AskNoSource, func(a common.HeadAnswer) {
		if seq != t.askSeq {
			return
		}
		t.ask.out, t.ask.due = false, a.Next
		if t.ask.due.IsZero() {
			t.ask.due = time.Now().Add(t.BlockTime())
		}
		if nd := t.heldAnswer(a); nd != nil {
			t.ask.pending, t.ask.pendingVoter = nd, id
			t.walkFrom(nd, id)
		}
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

// conflicts reports whether a vote leaves the pinned branch: neither
// pinned nor above the head on its branch.
func (t *ChainTracker) conflicts(nd *node) bool {
	if len(t.pins) == 0 || t.pinned(nd) {
		return false
	}
	return !isAncestor(t.pins[len(t.pins)-1], nd)
}

// rearm ticks the tracker at its next deadline: the no-source voter's next
// Ask, the end of an undecided settle's backoff, the next catch-up while
// the lane's Latest caps the sends, the end of the cold window, the moment
// the soonest hash voter's hash age passes W, or the moment a vote off the
// pins has been off for W (minority fork). It runs after every message, so
// a deadline that fires never hides a later one.
func (t *ChainTracker) rearm() {
	now := time.Now()
	var at time.Time
	if t.noSourceWanted() && t.ask.due.After(now) {
		at = t.ask.due
	}
	if s := t.settle; s != nil && !s.inflight && !s.decided && s.retryAt.After(now) && (at.IsZero() || s.retryAt.Before(at)) {
		at = s.retryAt
	}
	if c := t.catchUpAt; c.After(now) && (at.IsZero() || c.Before(at)) {
		at = c
	}
	for _, due := range []time.Time{t.coldUntil, t.voters.Next, t.forkAt} {
		if due.After(now) && (at.IsZero() || due.Before(at)) {
			at = due
		}
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
	for ctx.Err() == nil {
		err := taps.RunChain(ctx, key, "integrity", "follow", t, nil, indexer.ReadOpts{}, func(b indexer.Batch) error {
			if len(b.Applied) == 0 {
				return nil
			}
			if p := stateProberFor(t.network.Id()); p != nil {
				p.onNewHead(int64(b.Next.Number))
			}
			if t.laneHead != nil {
				if served := t.laneHead().Latest; served > 0 {
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
