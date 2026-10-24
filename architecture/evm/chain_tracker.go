package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
)

// maxOrphansPerHeight bounds the held headers at one height that are not
// pinned (spec §8); the oldest goes first.
const maxOrphansPerHeight = 4

// reconfirmCooldown bounds how often one block number's pin is re-confirmed
// against a fresh canonical fetch: within the cooldown the current pin is
// trusted as-is, so a fork-flapping tip does not re-fetch per request.
const reconfirmCooldown = time.Second

// mailboxHeaderCap bounds the headers waiting in a tracker's mailbox.
var mailboxHeaderCap = 1024

// trackerPanicsToReset is how many messages of one kind may panic, with
// none of that kind handled cleanly between them, before the tracker resets
// to empty (handle).
const trackerPanicsToReset = 3

// TrackerMessageHook, when set, runs on a tracker's goroutine before each
// message is applied, with the message's header when it carries one and
// whether it is a source vote; a panic in it is the message's panic. Tests
// use it to stall or panic a tracker; production never sets it.
var TrackerMessageHook atomic.Pointer[func(t *ChainTracker, upstreamId string, header *indexer.Cursor, sourceVote bool)]

// ChainTracker is fork choice for one (network, group) on one goroutine, and
// the only writer of its chain feed: headers by hash, pins, receipts. Votes
// move the pins (spec §6); answers are only held.
type ChainTracker struct {
	ctx     context.Context
	network common.Network
	// selector is the use-upstream selector of a group tracker, "" for the
	// network-wide one; group is its lane label and groupKey its lane key.
	selector string
	group    string
	groupKey string
	window   int
	// backfill is the chain time the network-wide tracker holds above its
	// floor of window blocks (spec §8); 0 for group trackers.
	backfill time.Duration
	// laneHead reads the lane's published Head; nil when the network has none.
	laneHead func() common.EvmHead

	mb    mailbox
	state atomic.Pointer[feedState]
	trees atomic.Pointer[treeState]
	// txHeadAt is when the head last moved to a block with transactions,
	// in unix nanoseconds; 0 before one.
	txHeadAt atomic.Int64

	// held, receipts and anchors are read by any goroutine; only the
	// tracker's goroutine writes them.
	held     sync.Map // lowercase hash -> *chainHeader
	receipts sync.Map // lowercase block hash -> *heldReceipts
	anchors  sync.Map // int64 -> *anchorEntry: finalized narrow anchors

	// revertedFinal holds the spans of finalized blocks a followed
	// majority branch reverted (recordMove), newest last, at most
	// maxRevertedFinal; any goroutine reads it.
	revertedFinal atomic.Pointer[[]heightSpan]
	// unlinkedPins are the pins a re-anchor left below the new tree; only
	// the tracker's goroutine reads or writes them (recordMove).
	unlinkedPins []*chainHeader

	wantMu      sync.Mutex
	wants       map[uint64]indexer.ReadOpts
	wantID      uint64
	wantVersion atomic.Uint64 // moves on each MayMatch Want and release
	// deepFill counts open Resumable taps and Reads that wait on fill:
	// while any is, fill runs down to the window base, else it stops
	// reorgWindow blocks below the head.
	deepFill atomic.Int32
	// fillReads counts, by cursor hash, the Reads that wait on fill: fill
	// of a tree holding one runs past its reorgWindow cap (fillRoots).
	fillMu    sync.Mutex
	fillReads map[[32]byte]int

	reconfirmMu   sync.Mutex
	reconfirmedAt map[int64]time.Time

	// fetch runs every fetch of this tracker; one per network.
	fetch *fetchScheduler

	// following is true while the tracker's integrity follow tap runs.
	following atomic.Bool

	// receiptsFailing is when every receipts and logs fetch since the last
	// success started failing, in unix nanoseconds; 0 after a success.
	receiptsFailing atomic.Int64

	// Owned by the tracker's goroutine.
	nodes    map[[32]byte]*node
	waiting  map[[32]byte][]*node // held roots by the parent hash they lack
	heights  map[uint64][]*node   // held headers per height, oldest first
	exts     map[*node]*extension // every held root: its walk or fill
	votes    map[string]*node     // source upstream id -> its vote
	pending  map[string]*node     // source upstream id -> a vote its walk holds back
	ask      askState             // §6 rule 6
	askSeq   uint64               // names the latest ask; a reset moves it on
	settle   *settleState         // §6 rule 5; nil until the first tie
	settleIn time.Duration        // the next undecided settle's backoff
	pins     []*node              // root..head, parentHash-linked
	maxHeld  *chainHeader
	wakeAt   time.Time
	wake     *time.Timer
	// catchUpAt is when the next catch-up may ask while the network's latest
	// block holds back a backed pin, or, while the vote need holds one, when
	// the tracker asks again (the network paces those polls); zero while
	// neither does (gate). heldSince is when the vote need began to hold one.
	catchUpAt time.Time
	heldSince time.Time
	// coldUntil is W after the tracker started or last reset, W read then:
	// until it, voters that are sources and never reported count as hash
	// voters (spec §6.5 cold window). A W learned later neither ends nor
	// reopens the window.
	coldUntil time.Time
	// voters is the lane's voter set as the network's intake last read it,
	// before the message being handled.
	voters HashVoters
	// offPins is, per voter whose vote left the pinned branch, when the
	// tracker first polled it for its head and when last; demoted are those
	// still off W after that first poll, sorted, as last handed to the
	// network (spec §6.3 minority fork). Network-wide only.
	offPins map[string]offPin
	demoted []string
	forkAt  time.Time
	// sampled is the confirmed head last fed to the block time.
	sampled *chainHeader

	receiptsOrder []receiptsEntry
	jobs          map[[32]byte]*receiptsJob // receipts fetches by block hash
	// needs caches feedNeeds per block while the registry (needsVersion)
	// and the bloom skip (needsTrusted) stay; needsWants is that registry.
	needs        map[[32]byte]bool
	needsWants   []func([]byte) bool
	needsVersion uint64
	needsTrusted bool
	// receiptsDirty: the pins, a job or held receipts changed since the
	// receipts were last planned.
	receiptsDirty bool
	// probe is armed while receipts fail: the outage's head-block fetches.
	probe         *time.Timer
	anchorNumbers []int64
	anchorVotes   map[int64][]anchorVote // the hashes vendors answered per anchor
	// panics counts, per panicSlot, the panics since a message of that slot
	// was last handled cleanly (handle).
	panics [panicSlots]int
	stack  []*node // walkTree's scratch
	// treesDirty: a header was held or dropped, or a walk or fill ended,
	// since trees was last published.
	treesDirty bool
}

type receiptsEntry struct {
	key    string
	number int64 // -1 when the receipts name none
}

type node struct {
	h        *chainHeader
	parent   *node
	children []*node
}

// feedState is what the tracker publishes after each change. Readers never
// take a lock the tracker waits on.
type feedState struct {
	base      uint64 // number of pins[0]
	pins      []*chainHeader
	confirmed int            // index in pins of the confirmed head; -1 none
	changed   chan struct{}  // closed once a newer state is published
	logs      indexer.Cursor // the logs head (spec §1)
	// backing is the confirmed head's count over pins, for ReconfirmPin,
	// tap start and the gRPC stall rule.
	backing *pinBacking
}

func (s *feedState) head() *chainHeader {
	if len(s.pins) == 0 {
		return nil
	}
	return s.pins[len(s.pins)-1]
}

// confirmedHead is the confirmed head (spec §6.5), nil while no pin is.
func (s *feedState) confirmedHead() *chainHeader {
	if s.confirmed < 0 || s.confirmed >= len(s.pins) {
		return nil
	}
	return s.pins[s.confirmed]
}

func (s *feedState) pinAt(n uint64) *chainHeader {
	if len(s.pins) == 0 || n < s.base || n-s.base >= uint64(len(s.pins)) {
		return nil
	}
	return s.pins[n-s.base]
}

// pinned is c's header while c is pinned.
func (s *feedState) pinned(c indexer.Cursor) *chainHeader {
	if h := s.pinAt(c.Number); h != nil && h.hash == c.Hash {
		return h
	}
	return nil
}

// treeState is what a Read whose cursor is outside the pins waits on: the
// roots whose walk or fill ended for good without joining another tree,
// and the root of the pins' tree, computed over state. The tracker
// publishes it after each message that held or dropped a header, ended an
// extension or moved the pins.
type treeState struct {
	state   *feedState
	ended   map[[32]byte]struct{}
	pinRoot *chainHeader  // nil while nothing is pinned
	changed chan struct{} // closed once a newer state is published
}

// pinsMayJoin reports whether the pins' fill still runs above root's
// height, so it may yet join root's tree.
func (tr *treeState) pinsMayJoin(root *chainHeader) bool {
	if tr.pinRoot == nil {
		return false
	}
	_, ended := tr.ended[tr.pinRoot.hash]
	return !ended && tr.pinRoot.number > root.number
}

type voteKind uint8

const (
	voteNone   voteKind = iota // a client or integrity answer
	voteSource                 // a source header that passed intake
)

type msgKind uint8

const (
	msgHeader  msgKind = iota // a source vote or a client answer
	msgFetched                // a tracker fetch's answer; never shed
	msgAnchors
	msgReceipts
	msgTick
	msgSync
	msgCall   // a tracker-owned fetch ended: fn runs on the tracker
	msgSettle // ReconfirmPin asks for a settle; settled gets its outcome
	msgKinds
)

type trackerMsg struct {
	kind     msgKind
	upstream string
	vote     voteKind
	raw      json.RawMessage
	hdr      *chainHeader // set when the sender already parsed raw
	anchors  []anchor
	held     *heldReceipts
	done     chan struct{}
	fn       func()
	settled  chan settleOutcome
}

type anchor struct {
	number int64
	hash   string
	vendor string
}

// anchorVote is one hash vendors answered for an anchor's number.
type anchorVote struct {
	hash    string
	vendors []string
}

// anchorEntry is what HashAt serves for a finalized number below the
// window: the hash the most vendors answered, the first on a tie; agreed
// while 2 or more vendors answered it and no other hash as many.
type anchorEntry struct {
	hash   string
	agreed bool
}

// mailbox never blocks a sender. It sheds only client answers and source
// headers: when they fill it, answers drop first, then each source keeps
// only its newest header. Fetch results, anchors and receipts never drop; at
// most one tick waits.
type mailbox struct {
	mu    sync.Mutex
	queue []trackerMsg
	shed  int // sheddable messages queued
	tick  bool
	wake  chan struct{}
}

func sheddable(msg trackerMsg) bool {
	return msg.kind == msgHeader
}

func (m *mailbox) put(msg trackerMsg) {
	m.mu.Lock()
	switch {
	case msg.kind == msgTick:
		if m.tick {
			m.mu.Unlock()
			return
		}
		m.tick = true
	case sheddable(msg):
		if m.shed >= mailboxHeaderCap {
			m.shedLocked()
		}
		if m.shed >= mailboxHeaderCap && msg.vote == voteNone {
			m.mu.Unlock()
			return
		}
		m.shed++
	}
	m.queue = append(m.queue, msg)
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// shedLocked drops queued client answers, then, while still full, every
// source header a newer one of the same source supersedes.
func (m *mailbox) shedLocked() {
	kept := m.queue[:0]
	for _, msg := range m.queue {
		if sheddable(msg) && msg.vote == voteNone {
			m.shed--
			continue
		}
		kept = append(kept, msg)
	}
	m.queue = kept
	if m.shed < mailboxHeaderCap {
		return
	}
	newest := make(map[string]bool)
	superseded := make([]bool, len(m.queue))
	for i := len(m.queue) - 1; i >= 0; i-- {
		if msg := m.queue[i]; sheddable(msg) {
			superseded[i] = newest[msg.upstream]
			newest[msg.upstream] = true
		}
	}
	kept = m.queue[:0]
	for i, msg := range m.queue {
		if superseded[i] {
			m.shed--
			continue
		}
		kept = append(kept, msg)
	}
	m.queue = kept
}

func (m *mailbox) take() []trackerMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queue
	m.queue, m.shed, m.tick = nil, 0, false
	return q
}

func newChainTracker(ctx context.Context, n common.Network, window int, backfill time.Duration, selector, groupKey, group string, laneHead func() common.EvmHead, fetch *fetchScheduler) *ChainTracker {
	t := &ChainTracker{
		ctx:           ctx,
		network:       n,
		selector:      selector,
		group:         group,
		groupKey:      groupKey,
		window:        window,
		backfill:      backfill,
		fetch:         fetch,
		mb:            mailbox{wake: make(chan struct{}, 1)},
		wants:         make(map[uint64]indexer.ReadOpts),
		reconfirmedAt: make(map[int64]time.Time),
	}
	t.laneHead = laneHead
	t.clear()
	s := &feedState{changed: make(chan struct{}), backing: &pinBacking{}}
	t.state.Store(s)
	t.trees.Store(&treeState{state: s, changed: make(chan struct{})})
	fetch.bg.Go(func() { t.run(ctx) })
	return t
}

// Wait waits, once the network's ctx ended, until no goroutine of the
// network's trackers and their fetches runs.
func (t *ChainTracker) Wait() {
	t.fetch.bg.Wait()
}

// clear empties the tracker's own state.
func (t *ChainTracker) clear() {
	t.nodes = make(map[[32]byte]*node)
	t.waiting = make(map[[32]byte][]*node)
	t.heights = make(map[uint64][]*node)
	t.exts = make(map[*node]*extension)
	t.votes = make(map[string]*node)
	t.pending = make(map[string]*node)
	t.ask = askState{}
	t.askSeq++
	t.settle, t.settleIn = nil, 0
	t.pins, t.maxHeld = nil, nil
	t.coldUntil = time.Now().Add(t.W())
	t.offPins = make(map[string]offPin)
	t.treesDirty = true
	for _, j := range t.jobs {
		t.endJob(j, nil)
	}
	t.jobs = make(map[[32]byte]*receiptsJob)
	t.needs = make(map[[32]byte]bool)
	t.receiptsDirty = true
}

// Observe is the tracker's only input of headers. vote is true for a source
// header that passed intake; client and integrity answers never vote.
func (t *ChainTracker) Observe(upstreamId string, raw json.RawMessage, vote bool) {
	v := voteNone
	if vote {
		v = voteSource
	}
	t.mb.put(trackerMsg{kind: msgHeader, upstream: upstreamId, vote: v, raw: raw})
}

// VotersChanged makes the tracker read its voters again: the policy's
// eligible set, a bench or a hash vote moved.
func (t *ChainTracker) VotersChanged() {
	t.mb.put(trackerMsg{kind: msgTick})
}

func (t *ChainTracker) observeAnswer(upstreamId string, h *chainHeader) {
	t.mb.put(trackerMsg{kind: msgHeader, upstream: upstreamId, vote: voteNone, hdr: h})
}

// observeSource is Observe for a source header intake already parsed.
func (t *ChainTracker) observeSource(upstreamId string, h *chainHeader) {
	t.mb.put(trackerMsg{kind: msgHeader, upstream: upstreamId, vote: voteSource, hdr: h})
}

func (t *ChainTracker) observeFetched(upstreamId string, h *chainHeader) {
	t.mb.put(trackerMsg{kind: msgFetched, upstream: upstreamId, vote: voteNone, hdr: h})
}

// sync waits until the tracker handled every message sent before it.
func (t *ChainTracker) sync(ctx context.Context) {
	done := make(chan struct{})
	t.mb.put(trackerMsg{kind: msgSync, done: done})
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (t *ChainTracker) run(ctx context.Context) {
	defer func() {
		if t.wake != nil {
			t.wake.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.mb.wake:
		}
		for _, msg := range t.mb.take() {
			t.handle(msg)
		}
	}
}

// handle applies one message, then moves walks, fill, settles and asks on.
// Each step recovers on its own: a panicking message is dropped, and a
// panicking step of update never blames the message. 3 panics in one
// panicSlot, with no message of that slot handled cleanly between them,
// reset the tracker to empty: other kinds come between any two of one kind,
// so a state that breaks only source votes, or only fetch answers, still
// resets, while one vendor's bad header among clean ones never does.
func (t *ChainTracker) handle(msg trackerMsg) {
	if msg.done != nil {
		defer close(msg.done)
	}
	if msg.kind != msgSync {
		t.readVoters()
	}
	ok := t.guard("message", func() { t.receive(msg) }, func() {
		if msg.kind == msgHeader && msg.vote == voteSource {
			telemetry.CounterHandle(telemetry.MetricChainTrackerSourceHeadsTotal,
				t.projectId(), t.networkLabel(), msg.upstream, "malformed").Inc()
		}
		if msg.settled != nil {
			report(msg.settled, settleUnanswered)
		}
	})
	if msg.kind != msgSync {
		ok = t.update() && ok
	}
	slot := panicSlot(msg)
	if ok {
		t.panics[slot] = 0
		return
	}
	t.panics[slot]++
	if t.panics[slot] >= trackerPanicsToReset {
		t.panics = [panicSlots]int{}
		t.reset()
		telemetry.CounterHandle(telemetry.MetricChainTrackerResetTotal, t.projectId(), t.networkLabel()).Inc()
	}
}

// panicSlots is one run per message kind, plus source votes apart from
// the client answers that share their kind.
const panicSlots = int(msgKinds) + 1

// panicSlot is the run a message's panics count in.
func panicSlot(msg trackerMsg) int {
	if msg.kind == msgHeader && msg.vote == voteSource {
		return int(msgKinds)
	}
	return int(msg.kind)
}

func (t *ChainTracker) receive(msg trackerMsg) {
	if hook := TrackerMessageHook.Load(); hook != nil {
		var cur *indexer.Cursor
		if msg.hdr != nil {
			c := msg.hdr.cursor()
			cur = &c
		} else if msg.raw != nil {
			if c, err := ParseHeader(msg.raw); err == nil {
				cur = &c
			}
		}
		(*hook)(t, msg.upstream, cur, msg.kind == msgHeader && msg.vote == voteSource)
	}
	switch msg.kind {
	case msgHeader, msgFetched:
		t.handleHeader(msg)
	case msgAnchors:
		t.handleAnchors(msg.anchors)
	case msgReceipts:
		t.holdReceipts(msg.held)
	case msgCall:
		msg.fn()
	case msgSettle:
		t.requestSettle(msg.settled)
	case msgTick, msgSync:
	}
}

// guard runs one step and reports whether it ended without a panic. A
// panic is counted and logged, onPanic runs, and a settle that ended in
// the step still answers its waiters.
func (t *ChainTracker) guard(step string, fn func(), onPanic func()) (ok bool) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
			"chain-tracker",
			fmt.Sprintf("project:%s network:%s group:%s", t.projectId(), t.networkLabel(), t.group),
			common.ErrorFingerprint(rec),
		).Inc()
		log.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).
			Str("network", t.networkLabel()).Str("group", t.group).Str("step", step).Msg("chain tracker step panicked")
		if onPanic != nil {
			onPanic()
		}
		if s := t.settle; s != nil && !s.inflight {
			s.notify(settleUnanswered)
		}
	}()
	fn()
	return true
}

// update runs after every message: walks that nothing waits on end, votes
// whose walks ended move (rule 3), a settle whose answers are in ends
// (rule 5), the pins follow the votes (rule 4), roots extend (rules 1, 2),
// the no-source ask runs (rule 6), the next deadline is armed and the
// trees are published for Reads outside the pins.
func (t *ChainTracker) update() bool {
	ok := t.guard("choose", func() {
		t.endWalks()
		t.promote()
		if s := t.settle; s != nil && s.inflight && len(s.out) == 0 && !t.anyWalking(s.answers) {
			t.endSettle(s)
		} else {
			t.choose()
		}
	}, nil)
	ok = t.guard("forks", t.trackForks, nil) && ok
	ok = t.guard("fill", t.extendRoots, nil) && ok
	ok = t.guard("ask", t.maybeAsk, nil) && ok
	ok = t.guard("send", t.gateSend, nil) && ok
	ok = t.guard("trees", t.publishTrees, nil) && ok
	ok = t.guard("receipts", t.planReceipts, nil) && ok
	return t.guard("wake", t.rearm, nil) && ok
}

// publishTrees publishes the roots whose extension ended for good, once a
// header was held or dropped, an extension ended or the pins moved. A
// capped tree has not ended: a Read in it lifts the cap (spec §10.1). A
// tracker that cannot fetch extends no root.
func (t *ChainTracker) publishTrees() {
	pub := t.state.Load()
	if !t.treesDirty && t.trees.Load().state == pub {
		return
	}
	t.treesDirty = false
	pinRoot := t.pinRoot()
	s := &treeState{state: pub, ended: make(map[[32]byte]struct{}), changed: make(chan struct{})}
	if pinRoot != nil {
		s.pinRoot = pinRoot.h
	}
	for r, e := range t.exts {
		if e.atBase || !t.canFetch() {
			s.ended[r.h.hash] = struct{}{}
		}
	}
	close(t.trees.Swap(s).changed)
}

func (t *ChainTracker) handleHeader(msg trackerMsg) {
	h := msg.hdr
	if h == nil {
		var err error
		if h, err = parseChainHeader(msg.raw); err != nil {
			return
		}
	}
	// A source header from a non-voter (syncing, benched, outside the
	// lane) is no vote (spec §6.2, #202): holding it would walk or fill
	// the heights only it has, by number, from it.
	if msg.vote == voteSource && !t.counts(msg.upstream) && !slices.Contains(t.voters.Voters, msg.upstream) {
		return
	}
	nd := t.nodes[h.hash]
	if nd == nil {
		// Rule 1: every vote is held. Answers are held only inside the
		// window, and never walk.
		if msg.vote == voteNone && !t.inWindow(h) {
			return
		}
		nd = t.hold(h)
	}
	if msg.vote != voteSource {
		if len(t.pins) == 0 {
			t.evict()
		}
		return
	}
	// Rule 3: the vote moves once its walk ends; until then the previous
	// one stays.
	t.pending[msg.upstream] = nd
	t.walkFrom(nd, msg.upstream)
}

// promote moves each pending vote whose walk ended. An ancestor of the
// current vote does not move it back.
func (t *ChainTracker) promote() {
	for u, nd := range t.pending {
		if !t.alive(nd) {
			delete(t.pending, u)
			continue
		}
		if t.walking(nd) {
			continue
		}
		delete(t.pending, u)
		if cur := t.votes[u]; cur == nil || !t.alive(cur) || !isAncestor(nd, cur) {
			t.votes[u] = nd
		}
	}
	if nd := t.ask.pending; nd != nil && (!t.alive(nd) || !t.walking(nd)) {
		t.ask.pending = nil
		if t.alive(nd) {
			t.ask.vote, t.ask.voter = nd, t.ask.pendingVoter
			t.ask.conflict = t.conflicts(nd)
		}
	}
}

// alive reports whether n is still held.
func (t *ChainTracker) alive(n *node) bool {
	return n != nil && t.nodes[n.h.hash] == n
}

// head is the highest pin, nil while nothing is pinned.
func (t *ChainTracker) head() *chainHeader {
	if len(t.pins) == 0 {
		return nil
	}
	return t.pins[len(t.pins)-1].h
}

// holds is the window (spec §8) below ref: at least reorgWindow blocks,
// and for the network-wide tracker every header whose timestamp is within
// maxBackfill of ref's. A header without a timestamp gets the floor only.
func (t *ChainTracker) holds(ref, h *chainHeader) bool {
	if h.number+uint64(t.window) >= ref.number {
		return true
	}
	return t.backfill > 0 && h.timestamp > 0 && ref.timestamp >= h.timestamp &&
		time.Duration(ref.timestamp-h.timestamp)*time.Second <= t.backfill
}

// inWindow reports whether an answer lies inside the window below the head
// and at most reorgWindow blocks above it. Without pins every header is.
func (t *ChainTracker) inWindow(h *chainHeader) bool {
	head := t.head()
	if head == nil {
		return true
	}
	return t.holds(head, h) && h.number <= head.number+uint64(t.window)
}

// farAhead is rule 1's exception: a vote more than maxBackfill after the
// head by header time (reorgWindow blocks for a group tracker, or headers
// without time) becomes a root at once.
func (t *ChainTracker) farAhead(h *chainHeader) bool {
	head := t.head()
	if head == nil {
		return false
	}
	if t.backfill > 0 && h.timestamp > 0 && head.timestamp > 0 {
		return h.timestamp > head.timestamp && time.Duration(h.timestamp-head.timestamp)*time.Second > t.backfill
	}
	return h.number > head.number+uint64(t.window)
}

// HashVoters is what the network's intake says of one lane's voters (spec
// §6.2, §6.5), read once per tracker message.
type HashVoters struct {
	// Voters are the lane's voters in selection order; settles and
	// no-source asks go to them.
	Voters []string
	// Hash are the hash voters: voters whose hash age is at most W, and,
	// within the cold window, voters that are sources and never reported.
	// Need is q.Of(len(Hash)).
	Hash []string
	Need int
	// Vendors names the vendor of each upstream of the network; Evidence is
	// the evidence quorum (spec §6.10): 2 distinct vendors when the network
	// has 2 or more, else 1.
	Vendors  map[string]string
	Evidence int
	// Next is when the soonest hash voter's hash age passes W; zero while
	// none will without a new vote.
	Next time.Time
	// all: no intake reads the voters, so every vote counts and one backs
	// a pin.
	all bool
}

// readVoters reads the lane's voters from the network's intake; a network
// without intake counts every vote. A panic in the read keeps the last set.
// A change of the network-wide hash voters tells the network, whose
// reconciler rechecks the topics (spec §5.2).
func (t *ChainTracker) readVoters() {
	n, ok := t.network.(interface {
		EvmHashVoters(selector string, w time.Duration, cold bool) HashVoters
	})
	if !ok {
		// Without intake the vendors are unknown: no vote alone is evidence
		// enough for PinFresh, which then needs a settle.
		t.voters = HashVoters{Need: 1, Evidence: 2, all: true}
		return
	}
	w := t.W()
	cold := time.Now().Before(t.coldUntil)
	old := t.voters.Hash
	t.callNetwork("voters", func() { t.voters = n.EvmHashVoters(t.selector, w, cold) })
	if t.selector != "" || slices.Equal(old, t.voters.Hash) {
		return
	}
	if c, ok := t.network.(interface{ EvmHashVotersChanged() }); ok {
		t.callNetwork("hash-voters-changed", c.EvmHashVotersChanged)
	}
}

// counts reports whether upstreamId's vote counts in this tracker: it is a
// hash voter of the lane.
func (t *ChainTracker) counts(upstreamId string) bool {
	return t.voters.all || slices.Contains(t.voters.Hash, upstreamId)
}

// offPin is one voter off the pinned branch: when the tracker first polled
// it for its head, and when last.
type offPin struct{ since, polled time.Time }

// trackForks is spec §6.3's minority fork, for the network-wide tracker.
// When the pins move off a voter's vote, the tracker polls that voter at
// once, and again each W while its vote stays off: a vote from before a
// reorg is no evidence of a fork, as a healthy upstream answers on the new
// branch. A voter still off W after the first poll loses its number vote
// and goes last in routing, until its vote is on the branch again. The
// network reads the demoted set; forkAt is the next poll or demotion.
func (t *ChainTracker) trackForks() {
	if t.selector != "" {
		return
	}
	now, w := time.Now(), t.W()
	for u := range t.offPins {
		if _, ok := t.votes[u]; !ok {
			delete(t.offPins, u)
		}
	}
	var demoted, poll []string
	t.forkAt = time.Time{}
	soonest := func(at time.Time) {
		if t.forkAt.IsZero() || at.Before(t.forkAt) {
			t.forkAt = at
		}
	}
	for u, nd := range t.votes {
		if !t.alive(nd) || !t.conflicts(nd) {
			delete(t.offPins, u)
			continue
		}
		op, ok := t.offPins[u]
		if !ok || !now.Before(op.polled.Add(w)) {
			if !ok {
				op.since = now
			}
			op.polled = now
			t.offPins[u] = op
			poll = append(poll, u)
		}
		soonest(op.polled.Add(w))
		if at := op.since.Add(w); now.After(at) {
			demoted = append(demoted, u)
		} else {
			soonest(at)
		}
	}
	if len(poll) > 0 {
		if p, ok := t.network.(interface{ EvmPollForks(ids []string) }); ok {
			t.callNetwork("fork-poll", func() { p.EvmPollForks(poll) })
		}
	}
	slices.Sort(demoted)
	if slices.Equal(demoted, t.demoted) {
		return
	}
	t.demoted = demoted
	if n, ok := t.network.(interface{ EvmForkDemoted(ids []string) }); ok {
		t.callNetwork("fork-demoted", func() { n.EvmForkDemoted(demoted) })
	}
}

// hold adds h to the held trees: under its parent when held, and over every
// held root that names it as parent. A header whose parent is not held is a
// root, and fill extends it (rule 2).
func (t *ChainTracker) hold(h *chainHeader) *node {
	nd := &node{h: h}
	if p := t.nodes[h.parentHash]; p != nil && p.h.number+1 == h.number {
		nd.parent = p
		p.children = append(p.children, nd)
	} else {
		t.waiting[h.parentHash] = append(t.waiting[h.parentHash], nd)
		t.exts[nd] = &extension{atBase: h.number == 0}
	}
	var kept []*node
	for _, c := range t.waiting[h.hash] {
		if c.h.number != h.number+1 {
			kept = append(kept, c)
			continue
		}
		c.parent = nd
		nd.children = append(nd.children, c)
		delete(t.exts, c)
	}
	if len(kept) > 0 {
		t.waiting[h.hash] = kept
	} else {
		delete(t.waiting, h.hash)
	}
	t.nodes[h.hash] = nd
	t.held.Store(h.hashHex, h)
	t.treesDirty = true
	t.heights[h.number] = append(t.heights[h.number], nd)
	if t.maxHeld == nil || h.number > t.maxHeld.number {
		t.maxHeld = h
	}
	t.capOrphans(h.number, nd)
	return nd
}

// capOrphans keeps at most maxOrphansPerHeight unpinned headers at a
// height, dropping the oldest childless one that no counted vote points
// to; keep, just held, stays. An orphan with held children stays, so a
// drop never cuts a tree inside the window.
func (t *ChainTracker) capOrphans(number uint64, keep *node) {
	orphans := 0
	for _, n := range t.heights[number] {
		if !t.pinned(n) {
			orphans++
		}
	}
	if orphans <= maxOrphansPerHeight {
		return
	}
	counted := t.baseVotes()
	for _, n := range slices.Clone(t.heights[number]) {
		if orphans <= maxOrphansPerHeight {
			return
		}
		if n != keep && len(n.children) == 0 && !t.pinned(n) && !slices.ContainsFunc(counted, func(v voter) bool { return v.n == n }) {
			t.drop(n, true)
			orphans--
		}
	}
}

// drop releases a held header. Only eviction drops a header with children;
// they become roots. base: n left the window below the head, so fill does
// not fetch it again. Else only the reorgWindow cap of a tree without the
// pins dropped it: the children's tree stays capped, and fill fetches n
// again once that tree holds the pins or a Read waits on fill in it (spec
// §10.1: a cursor held in the other tree waits while fill links it).
func (t *ChainTracker) drop(n *node, base bool) {
	delete(t.nodes, n.h.hash)
	t.held.Delete(n.h.hashHex)
	t.treesDirty = true
	t.heights[n.h.number] = slices.DeleteFunc(t.heights[n.h.number], func(o *node) bool { return o == n })
	if len(t.heights[n.h.number]) == 0 {
		delete(t.heights, n.h.number)
	}
	if p := n.parent; p != nil {
		p.children = slices.DeleteFunc(p.children, func(o *node) bool { return o == n })
	} else {
		t.unwait(n)
		delete(t.exts, n)
	}
	for _, c := range n.children {
		c.parent = nil
		t.waiting[c.h.parentHash] = append(t.waiting[c.h.parentHash], c)
		t.exts[c] = &extension{atBase: base, capped: !base}
	}
	n.children = nil
	for u, v := range t.votes {
		if v == n {
			delete(t.votes, u)
		}
	}
}

func (t *ChainTracker) unwait(n *node) {
	ns := slices.DeleteFunc(t.waiting[n.h.parentHash], func(o *node) bool { return o == n })
	if len(ns) == 0 {
		delete(t.waiting, n.h.parentHash)
	} else {
		t.waiting[n.h.parentHash] = ns
	}
}

func isAncestor(a, b *node) bool {
	for n := b.parent; n != nil; n = n.parent {
		if n == a {
			return true
		}
	}
	return false
}

func rootOf(n *node) *node {
	for n.parent != nil {
		n = n.parent
	}
	return n
}

func (t *ChainTracker) pinned(n *node) bool {
	if len(t.pins) == 0 {
		return false
	}
	base := t.pins[0].h.number
	if n.h.number < base || n.h.number-base >= uint64(len(t.pins)) {
		return false
	}
	return t.pins[n.h.number-base] == n
}

// voter is one counted vote: who cast it and for which held header.
type voter struct {
	who string
	n   *node
}

// baseVotes are the votes of sources that count, or the no-source vote
// while none of those votes (rule 6).
func (t *ChainTracker) baseVotes() []voter {
	out := make([]voter, 0, len(t.votes)+1)
	for u, v := range t.votes {
		if t.counts(u) {
			out = append(out, voter{u, v})
		}
	}
	if len(out) == 0 && t.alive(t.ask.vote) {
		out = append(out, voter{t.ask.voter, t.ask.vote})
	}
	return out
}

// voteKey names a set of votes.
func voteKey(vs []voter) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = v.who + "=" + v.n.h.hashHex
	}
	slices.Sort(parts)
	return strings.Join(parts, ";")
}

// settleKey names the decision a settle is asked for, so its answers count
// only while that decision stands: the tied branches, so new heads on them
// keep it, or the votes when nothing ties.
func settleKey(base []voter, tie []*node) string {
	if len(tie) == 0 {
		return "votes:" + voteKey(base)
	}
	parts := make([]string, len(tie))
	for i, n := range tie {
		parts[i] = n.h.hashHex
	}
	slices.Sort(parts)
	return "tie:" + strings.Join(parts, ";")
}

// decide is rule 4 over the counted votes: the source votes, plus the
// answers of a settle asked for this decision. It returns no path while
// that settle's answers are out: the pins stay. tied reports a tie rule 4
// still meets; conflict a no-source answer off the pins.
func (t *ChainTracker) decide() (path []*node, key string, tied, conflict bool) {
	base := t.baseVotes()
	var tie []*node
	if len(base) > 0 {
		path, tie = t.pick(base)
	}
	key = settleKey(base, tie)
	if s := t.settle; s != nil && s.key == key {
		if s.inflight {
			return nil, key, false, false
		}
		if answers := t.settleAnswers(s); len(answers) > 0 {
			path, tie = t.pick(append(base, answers...))
		}
	}
	conflict = len(base) == 1 && base[0].n == t.ask.vote && t.ask.conflict
	return path, key, len(tie) > 0, conflict
}

// settleAnswers are the answers of s that count: held, and walked.
func (t *ChainTracker) settleAnswers(s *settleState) []voter {
	var out []voter
	for who, a := range s.answers {
		if t.alive(a) && !t.walking(a) {
			out = append(out, voter{who, a})
		}
	}
	return out
}

// decidedAnswers are the answers of the decided settle while the decision
// it was asked for stands, over the counted votes base; none otherwise.
func (t *ChainTracker) decidedAnswers(base []voter) []voter {
	s := t.settle
	if s == nil || !s.decided {
		return nil
	}
	var tie []*node
	if len(base) > 0 {
		_, tie = t.pick(base)
	}
	if settleKey(base, tie) != s.key {
		return nil
	}
	return t.settleAnswers(s)
}

// choose moves the pins to rule 4's path; a tie, or a no-source answer
// that conflicts with the pins, starts a settle when one may start.
func (t *ChainTracker) choose() {
	path, key, tied, conflict := t.decide()
	if path == nil {
		return
	}
	if (tied || conflict) && t.maySettle(key) && t.startSettle(key, nil, false) {
		return
	}
	t.apply(path)
}

// pick is rule 4: the tree with the most votes, then from its root the
// child whose subtree holds the most votes. Ties keep the pins. tie is the
// first tie met: the tied roots, or the tied children at the fork point.
func (t *ChainTracker) pick(counted []voter) (path []*node, tie []*node) {
	counts := make(map[*node]int)
	roots := make(map[*node]struct{})
	for _, v := range counted {
		for a := v.n; a != nil; a = a.parent {
			counts[a]++
			if a.parent == nil {
				roots[a] = struct{}{}
			}
		}
	}
	pinRoot := t.pinRoot()
	better := func(r, cur *node) bool {
		switch {
		case cur == nil:
			return true
		case counts[r] != counts[cur]:
			return counts[r] > counts[cur]
		case cur == pinRoot:
			return false
		case r == pinRoot:
			return true
		case r.h.number != cur.h.number:
			return r.h.number > cur.h.number
		default:
			return bytes.Compare(r.h.hash[:], cur.h.hash[:]) > 0
		}
	}
	var root *node
	for r := range roots {
		if better(r, root) {
			root = r
		}
	}
	for r := range roots {
		if counts[r] == counts[root] {
			tie = append(tie, r)
		}
	}
	if len(tie) < 2 {
		tie = nil
	}

	path = []*node{root}
	for cur := root; ; {
		most := 0
		for _, c := range cur.children {
			most = max(most, counts[c])
		}
		var next *node
		ties := 0
		for _, c := range cur.children {
			if most > 0 && counts[c] == most {
				ties++
				if ties == 1 || t.pinned(c) {
					next = c
				}
			}
		}
		if ties > 1 && tie == nil {
			for _, c := range cur.children {
				if counts[c] == most {
					tie = append(tie, c)
				}
			}
		}
		if most == 0 || ties > 1 && !t.pinned(next) {
			// No voted child, or a tie: follow the pinned child, if any.
			next = nil
			for _, c := range cur.children {
				if t.pinned(c) {
					next = c
				}
			}
			if most > 0 && next != nil && counts[next] != most {
				next = nil
			}
		}
		if next == nil {
			break
		}
		path = append(path, next)
		cur = next
	}
	return path, tie
}

// apply makes path the pins in one step: the pins inside the window are
// computed first, compared with the published state, then committed and
// published together.
func (t *ChainTracker) apply(path []*node) {
	head := path[len(path)-1].h
	i := 0
	for i < len(path)-1 && !t.holds(head, path[i].h) {
		i++
	}
	path = path[i:]
	pub := t.state.Load()
	if len(pub.pins) == len(path) && pub.pins[0] == path[0].h && pub.head() == head {
		return
	}
	s := newFeedState(path)
	b := t.gate(path)
	s.confirmed, s.backing = b.confirmed(), b
	t.pins, t.treesDirty, t.receiptsDirty = path, true, true
	s.logs = t.logsHeadOf(s)
	if t.selector == "" && pub.head() != head {
		telemetry.GaugeHandle(telemetry.MetricNetworkSubscriptionLastHeadTimestamp, t.projectId(), t.networkLabel()).SetToCurrentTime()
	}
	if pub.head() != head && !head.emptyTxs {
		t.txHeadAt.Store(time.Now().UnixNano())
	}
	old := t.state.Swap(s)
	close(old.changed)
	t.sampleConfirmed(s)

	t.recordMove(pub.pins, path)
	t.evict()
}

// recordMove counts a re-anchor, or the depth of a reorg, from old to path,
// and sets the head gauge. A reverted block at or below the finalized
// height is followed, logged as an error (final_hash_changed) and kept in
// revertedFinal, so no permanent cache entry serves the old branch (spec
// §6.5). A re-anchor's old pins below the new tree are decided once fill
// links that tree down to them (unlinkedPins), or dropped once they leave
// the window. Headers compare by hash: fill can fetch an evicted block
// again as a new header.
func (t *ChainTracker) recordMove(old []*chainHeader, path []*node) {
	head := path[len(path)-1].h
	telemetry.MetricChainTrackerHead.WithLabelValues(t.projectId(), t.networkLabel(), t.group).Set(float64(head.number))
	if len(old) == 0 {
		return
	}
	base := path[0].h.number
	pathAt := func(n uint64) *chainHeader {
		if n >= base && n-base < uint64(len(path)) {
			return path[n-base].h
		}
		return nil
	}
	fork := -1
	for i := len(old) - 1; i >= 0; i-- {
		if p := pathAt(old[i].number); p != nil && p.hash == old[i].hash {
			fork = i
			break
		}
	}
	if fork < 0 {
		// Spec §15 labels re-anchors by network only: the network-wide
		// tracker's count.
		if t.selector == "" {
			telemetry.CounterHandle(telemetry.MetricChainTrackerReanchorTotal, t.projectId(), t.networkLabel()).Inc()
		}
	} else if reverted := len(old) - 1 - fork; reverted > 0 {
		telemetry.MetricChainTrackerReorgDepth.WithLabelValues(t.projectId(), t.networkLabel(), t.group).
			Observe(float64(reverted))
	}
	var span heightSpan
	replaced := func(hs []*chainHeader) {
		for _, h := range hs {
			if p := pathAt(h.number); p != nil && p.hash != h.hash {
				if span.hi == 0 {
					span.lo = int64(h.number)
				}
				span.lo, span.hi = min(span.lo, int64(h.number)), max(span.hi, int64(h.number))
			}
		}
	}
	replaced(old)
	replaced(t.unlinkedPins)
	var unlinked []*chainHeader
	keepUnlinked := func(hs []*chainHeader) {
		for _, h := range hs {
			if h.number < base && t.holds(head, h) {
				unlinked = append(unlinked, h)
			}
		}
	}
	keepUnlinked(t.unlinkedPins)
	if fork < 0 {
		keepUnlinked(old)
	}
	t.unlinkedPins = unlinked
	if fin := t.finalizedHead(); span.hi > 0 && fin > 0 && span.lo <= fin {
		log.Error().Str("network", t.networkLabel()).Str("group", t.group).Str("invariant", "final_hash_changed").
			Int64("revertedFrom", span.lo).Int64("finalized", fin).Uint64("head", head.number).
			Msg("chain tracker followed a majority branch that reverts a finalized block")
		t.addRevertedFinal(heightSpan{lo: span.lo, hi: min(span.hi, fin)})
	}
}

// maxRevertedFinal bounds revertedFinal: a majority revert of finalized
// blocks is a rare chain event.
const maxRevertedFinal = 16

// heightSpan is the block numbers lo through hi.
type heightSpan struct{ lo, hi int64 }

// addRevertedFinal publishes s in revertedFinal, the oldest span leaving
// at the bound. Only the tracker's goroutine calls it.
func (t *ChainTracker) addRevertedFinal(s heightSpan) {
	var spans []heightSpan
	if p := t.revertedFinal.Load(); p != nil {
		spans = *p
	}
	if len(spans) >= maxRevertedFinal {
		spans = spans[len(spans)-maxRevertedFinal+1:]
	}
	next := append(append(make([]heightSpan, 0, len(spans)+1), spans...), s)
	t.revertedFinal.Store(&next)
}

// RevertedFinal reports whether a followed majority branch reverted block
// n while it was at or below Finalized on this instance: a permanent
// cache entry for n may hold the old branch.
func (t *ChainTracker) RevertedFinal(n int64) bool {
	if t == nil {
		return false
	}
	p := t.revertedFinal.Load()
	if p == nil {
		return false
	}
	for _, s := range *p {
		if n >= s.lo && n <= s.hi {
			return true
		}
	}
	return false
}

// evict drops every held header outside its tree's window (keeps), every
// tree above the head that holds no vote or awaited answer, and the
// receipts of blocks more than reorgWindow below the head. The floor of
// reorgWindow blocks stays through a chain halt.
func (t *ChainTracker) evict() {
	ref := t.windowRef()
	if ref == nil {
		return
	}
	head, pinRoot := t.head(), t.pinRoot()
	var voted map[*node]bool
	var out []*node
	for r := range t.exts {
		unvotedAbove := false
		if head != nil && r != pinRoot && r.h.number > head.number {
			if voted == nil {
				voted = t.rootsHolding(true)
			}
			unvotedAbove = !voted[r]
		}
		var top *chainHeader
		if r != pinRoot {
			top = t.treeTop(r)
		}
		t.walkTree(r, func(n *node) {
			if unvotedAbove || !t.keeps(top, n.h, r == pinRoot) {
				out = append(out, n)
			}
		})
	}
	for _, n := range out {
		if t.alive(n) {
			t.drop(n, !t.holds(ref, n.h))
		}
	}
	t.evictReceipts()
}

func newFeedState(pins []*node) *feedState {
	s := &feedState{pins: make([]*chainHeader, len(pins)), changed: make(chan struct{})}
	for i, n := range pins {
		s.pins[i] = n.h
	}
	if len(s.pins) > 0 {
		s.base = s.pins[0].number
	}
	return s
}

func (t *ChainTracker) publish() {
	s := newFeedState(t.pins)
	b := t.gate(t.pins)
	s.confirmed, s.backing = b.confirmed(), b
	s.logs = t.logsHeadOf(s)
	close(t.state.Swap(s).changed)
	t.sampleConfirmed(s)
}

// sampleConfirmed feeds each new confirmed head of the network-wide tracker
// into the network's block time (spec §6.7): only headers the confirmed
// head reached are samples, so a lagging or bogus source header never is.
func (t *ChainTracker) sampleConfirmed(s *feedState) {
	h := s.confirmedHead()
	if t.selector != "" || h == nil || h == t.sampled || h.timestamp == 0 {
		return
	}
	t.sampled = h
	if n, ok := t.network.(interface{ EvmSampleBlockTime(number, timestamp int64) }); ok {
		t.callNetwork("block-time", func() { n.EvmSampleBlockTime(int64(h.number), int64(h.timestamp)) })
	}
}

// pinBacking is the confirmed head's count over the pins (spec §6.5): for
// each counted vote and decided settle answer, the index of the highest pin
// it backs and its vendor, a vote backing every pin on its branch at or
// below it; and how many pins, from the lowest, no counted vote disputes.
type pinBacking struct {
	base  uint64
	backs []pinBack // by index, ascending
	// need is q.Of(hash voters); voters is how many hash voters there are;
	// evidence is the evidence quorum (spec §6.10).
	need, voters, evidence int
	// ceiling is the highest number taps may send: the lane's Latest.
	ceiling uint64
	// undisputed is how many pins, from the lowest, every counted vote
	// descends from or extends, walking ones included; a pin above them has
	// a hash voter on another branch, or may yet be reorged away.
	undisputed int
}

// pinBack is one counted vote: the highest pin it backs and its vendor.
type pinBack struct {
	i      int
	vendor string
}

// atLeast is the index of the first back at or above pin index i.
func (b *pinBacking) atLeast(i int) int {
	first, _ := slices.BinarySearchFunc(b.backs, i, func(p pinBack, i int) int { return p.i - i })
	return first
}

// confirmed is the confirmed head: the index of the highest pin with need
// counted votes at or above it and a number at most the ceiling; -1 while
// no pin is. A pin one voter alone backs, such as a cold instance's first
// header, so waits for a second while two hash voters exist.
func (b *pinBacking) confirmed() int {
	need := max(b.need, 1)
	if len(b.backs) < need || b.ceiling < b.base {
		return -1
	}
	i := b.backs[len(b.backs)-need].i
	if b.base+uint64(i) > b.ceiling {
		i = int(b.ceiling - b.base)
	}
	return i
}

// capped returns the highest pin the votes back while the lane's Latest,
// not the vote need, holds it back; ok false otherwise.
func (b *pinBacking) capped() (number uint64, ok bool) {
	need := max(b.need, 1)
	if len(b.backs) < need {
		return 0, false
	}
	number = b.base + uint64(b.backs[len(b.backs)-need].i)
	return number, number > b.ceiling
}

// held returns the highest pin a vote backs while the vote need holds it
// back; ok false otherwise.
func (b *pinBacking) held() (number uint64, ok bool) {
	if len(b.backs) == 0 {
		return 0, false
	}
	top, need := b.backs[len(b.backs)-1].i, max(b.need, 1)
	if len(b.backs) >= need && b.backs[len(b.backs)-need].i >= top {
		return 0, false
	}
	return b.base + uint64(top), true
}

// confirms reports whether the pin at n is PinFresh (spec §6.5): confirmed,
// backed by the evidence quorum of vendors, and undisputed.
func (b *pinBacking) confirms(n uint64) bool {
	if n < b.base || n-b.base >= uint64(b.undisputed) || n > b.ceiling {
		return false
	}
	backs := b.backs[b.atLeast(int(n-b.base)):]
	if len(backs) < max(b.need, 1) {
		return false
	}
	var vendors []string
	for _, p := range backs {
		if !slices.Contains(vendors, p.vendor) {
			vendors = append(vendors, p.vendor)
		}
	}
	return len(vendors) >= b.evidence
}

// same reports whether b and o confirm the same pins and count the same
// hash voters.
func (b *pinBacking) same(o *pinBacking) bool {
	return o != nil && b.base == o.base && b.need == o.need && b.voters == o.voters && b.evidence == o.evidence &&
		b.undisputed == o.undisputed && b.ceiling == o.ceiling && slices.Equal(b.backs, o.backs)
}

// backingOf counts the votes backing each pin of path. A decided settle's
// answer on the pinned branch backs the whole branch the settle chose, so
// a split the settle decided flows at once.
func (t *ChainTracker) backingOf(path []*node) *pinBacking {
	b := &pinBacking{need: t.voters.Need, voters: len(t.voters.Hash), evidence: t.voters.Evidence, ceiling: t.sendCeiling()}
	if t.voters.all {
		b.voters = 1
	}
	if len(path) == 0 {
		return b
	}
	base, head := path[0].h.number, len(path)-1
	b.base, b.undisputed = base, len(path)
	// pinUnder is the index of the highest pin n descends from; -1 none.
	pinUnder := func(n *node) int {
		for a := n; a != nil && a.h.number >= base; a = a.parent {
			if d := a.h.number - base; d < uint64(len(path)) && path[d] == a {
				return int(d)
			}
		}
		return -1
	}
	// backs adds the highest pin n backs; branch: n backs the whole path
	// when it holds the pinned branch.
	backs := func(v voter, branch bool) {
		i := pinUnder(v.n)
		if i < 0 {
			return
		}
		if branch && (path[i] == v.n || i == head) {
			i = head
		}
		b.backs = append(b.backs, pinBack{i, t.voters.Vendors[v.who]})
	}
	votes := t.baseVotes()
	for _, v := range votes {
		backs(v, false)
		if t.conflicts(v.n) {
			b.undisputed = min(b.undisputed, pinUnder(v.n)+1)
		}
	}
	for _, a := range t.decidedAnswers(votes) {
		backs(a, true)
	}
	slices.SortFunc(b.backs, func(x, y pinBack) int { return x.i - y.i })
	for u, nd := range t.pending {
		if t.alive(nd) && t.walking(nd) && t.counts(u) {
			b.undisputed = min(b.undisputed, pinUnder(nd)+1)
		}
	}
	return b
}

// gate is the pins' backing for a publish. While a tap waits and the
// lane's Latest holds back a pin the votes back, it asks the voters whose
// head is below that pin for their heads once per block time, at most
// catchUpMax apart (spec §6.5 catch-up): a voter that is no source, polled
// every statePollerInterval, would otherwise hold every tap until its next
// poll. A pin the vote need holds back for one block time is polled for
// from the voters whose hash vote here is below it, at each publish: the
// network paces those polls per upstream across lanes (EvmPollHeads).
// Their shared counters may already show it, moved by another instance or
// a client response, which votes no hash. A poller skips a refresh inside
// its debounce, so rearm asks again at catchUpAt; a counter that moves
// wakes the tracker (LatestChanged).
func (t *ChainTracker) gate(path []*node) *pinBacking {
	b := t.backingOf(path)
	now := time.Now()
	pin, capped := b.capped()
	held, holding := b.held()
	switch {
	case !holding:
		t.heldSince = time.Time{}
	case t.heldSince.IsZero():
		t.heldSince = now
	}
	if !capped && !holding || !t.demanded() {
		t.catchUpAt = time.Time{}
		return b
	}
	if !capped {
		// Pushes of one block arrive together: only a pin held one block
		// time is asked for.
		if ripe := t.heldSince.Add(t.BlockTime()); now.Before(ripe) {
			t.catchUpAt = maxTime(t.catchUpAt, ripe)
			return b
		}
		t.catchUpAt = now.Add(t.BlockTime())
		var below []string
		for _, u := range t.voters.Voters {
			if nd := t.votes[u]; nd == nil || !t.alive(nd) || nd.h.number < held {
				below = append(below, u)
			}
		}
		if p, ok := t.network.(interface{ EvmPollHeads(ids []string) }); ok && len(below) > 0 {
			t.callNetwork("catch-up", func() { p.EvmPollHeads(below) })
		}
		return b
	}
	if now.Before(t.catchUpAt) {
		return b
	}
	t.catchUpAt = now.Add(min(t.BlockTime(), catchUpMax))
	if r, ok := t.network.(interface {
		EvmRefreshHeadsBelow(string, int64)
	}); ok {
		t.callNetwork("catch-up", func() { r.EvmRefreshHeadsBelow(t.selector, int64(pin)) })
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// sendCeiling is the highest number this tracker's taps may send: its
// lane's Latest, so no transport shows a block eth_blockNumber does not,
// and one source alone never moves either (spec §6.5). Latest unknown, no
// voter yet, holds every send (§7).
func (t *ChainTracker) sendCeiling() (ceiling uint64) {
	if t.laneHead == nil {
		return math.MaxUint64
	}
	t.callNetwork("send-ceiling", func() { ceiling = uint64(max(t.laneHead().Latest, 0)) })
	return ceiling
}

// finalizedHead is the lane's Head.Finalized, 0 while unknown or when the
// network has no Head.
func (t *ChainTracker) finalizedHead() (fin int64) {
	if t.laneHead == nil {
		return 0
	}
	t.callNetwork("finalized", func() { fin = t.laneHead().Finalized })
	return fin
}

// callNetwork runs a call into the network's head code from the tracker's
// goroutine. A panic there is counted and dropped, so it never fails the
// message being handled: a bug outside the tracker would otherwise reset it
// every 3 messages. A dropped ceiling call leaves 0, which holds the sends.
func (t *ChainTracker) callNetwork(what string, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues("chain-tracker-"+what,
				fmt.Sprintf("project:%s network:%s", t.projectId(), t.networkLabel()), common.ErrorFingerprint(rec)).Inc()
			log.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).
				Str("network", t.networkLabel()).Msg("chain tracker call into the network panicked")
		}
	}()
	fn()
}

// gateSend republishes the pins when the confirmed head, the pins PinFresh
// covers or the hash voters moved while the pins stayed: a vote moved up
// the pinned branch or started a walk off it, a hash vote aged past W, the
// cold window ended, or the lane's Latest moved. A change taps cannot see
// keeps the state's changed channel, so no tap wakes for it; the confirmed
// head moving, or the last hash voter leaving or the first arriving (tap
// start and the gRPC stall rule read it), wakes them.
func (t *ChainTracker) gateSend() {
	pub := t.state.Load()
	if len(pub.pins) == 0 {
		return
	}
	b := t.gate(t.pins)
	c := b.confirmed()
	if c == pub.confirmed && b.same(pub.backing) {
		return
	}
	s := &feedState{base: pub.base, pins: pub.pins, confirmed: c, changed: pub.changed, backing: b}
	if c == pub.confirmed && (b.voters > 0) == (pub.backing.voters > 0) {
		s.logs = pub.logs
		t.state.Store(s)
		return
	}
	s.changed = make(chan struct{})
	s.logs = t.logsHeadOf(s)
	close(t.state.Swap(s).changed)
	t.sampleConfirmed(s)
}

// reset empties the tracker; a pending settle reports undecided.
func (t *ChainTracker) reset() {
	if s := t.settle; s != nil {
		s.notify(settleUnanswered)
	}
	t.clear()
	t.held.Range(func(k, _ any) bool { t.held.Delete(k); return true })
	t.receipts.Range(func(k, _ any) bool { t.receipts.Delete(k); return true })
	t.anchors.Range(func(k, _ any) bool { t.anchors.Delete(k); return true })
	t.receiptsOrder, t.anchorNumbers, t.anchorVotes = nil, nil, nil
	t.publish()
	t.publishTrees()
}

// handleAnchors keeps the newest reorgWindow finalized anchors, each with
// the hashes vendors answered for it; HashAt serves them only below the
// window.
func (t *ChainTracker) handleAnchors(as []anchor) {
	if t.anchorVotes == nil {
		t.anchorVotes = make(map[int64][]anchorVote)
	}
	for _, a := range as {
		votes, ok := t.anchorVotes[a.number]
		if !ok {
			t.anchorNumbers = append(t.anchorNumbers, a.number)
		}
		i := slices.IndexFunc(votes, func(v anchorVote) bool { return v.hash == a.hash })
		if i < 0 {
			votes = append(votes, anchorVote{hash: a.hash})
			i = len(votes) - 1
		}
		if !slices.Contains(votes[i].vendors, a.vendor) {
			votes[i].vendors = append(votes[i].vendors, a.vendor)
		}
		t.anchorVotes[a.number] = votes
		t.anchors.Store(a.number, anchorOf(votes))
	}
	for len(t.anchorNumbers) > t.window {
		t.anchors.Delete(t.anchorNumbers[0])
		delete(t.anchorVotes, t.anchorNumbers[0])
		t.anchorNumbers = t.anchorNumbers[1:]
	}
}

func anchorOf(votes []anchorVote) *anchorEntry {
	best, second := 0, 0
	for i, v := range votes {
		switch n := len(v.vendors); {
		case n > len(votes[best].vendors):
			best, second = i, len(votes[best].vendors)
		case i != best && n > second:
			second = n
		}
	}
	n := len(votes[best].vendors)
	return &anchorEntry{hash: votes[best].hash, agreed: n >= 2 && n > second}
}

// receiptsCap bounds held receipts when their blocks name no number: every
// height of the floor, each with its orphans.
func (t *ChainTracker) receiptsCap() int {
	return (t.window + 1) * (1 + maxOrphansPerHeight)
}

// --- indexer.ChainFeed ---

// Head is a new tap's start: the confirmed head while a hash voter exists,
// else its next change; fill moving the pins' base is not one. After a wait
// the tap starts after the pins' head as it waited, the newest block it
// could have known, so a confirmed head that moves several blocks up still
// sends every block pinned since; with no pin then, or one a reorg
// replaced, the new confirmed head is the tap's first block.
func (t *ChainTracker) Head(ctx context.Context) (indexer.Cursor, error) {
	s := t.state.Load()
	prev := s.confirmedHead()
	if prev != nil && s.backing.voters > 0 {
		return prev.cursor(), nil
	}
	known := s.head()
	for {
		select {
		case <-ctx.Done():
			return indexer.Cursor{}, ctx.Err()
		case <-s.changed:
		}
		s = t.state.Load()
		h := s.confirmedHead()
		if h == nil || h == prev {
			continue
		}
		switch {
		case known != nil && known.number < h.number && s.pinned(known.cursor()) != nil:
			return known.cursor(), nil
		case h.number == 0:
			return h.cursor(), nil
		}
		return indexer.Cursor{Number: h.number - 1, Hash: h.parentHash}, nil
	}
}

// HashVoting reports whether the tracker's lane has a hash voter now (spec
// §6.1); a network without intake always has.
func (t *ChainTracker) HashVoting() bool {
	return t.state.Load().backing.voters > 0
}

// Read returns the next non-empty batch from c (spec §9.1), by the first
// case that matches:
//   - c pinned, or the parent of the lowest pin: the pins above it;
//   - c held, or fetched, under another number: out of range;
//   - c below the window: out of range;
//   - c held in the pins' tree: c's branch reverted down to the fork
//     point, the pins above the fork point applied;
//   - c held in another tree: a wait while fill may link it; out of range
//     once that tree's fill ended and the pins' fill ended or passed
//     below it;
//   - c not held, above the head: a wait for the head;
//   - c not held, inside the window: a wait for fill down to c.Number,
//     then c fetched by hash: found, it is held and filled as above, or
//     out of range when the window no longer holds it; null from every
//     upstream, out of range; any other failure, fetched again after the
//     key's backoff.
//
// While Read waits on fill, fill runs below reorgWindow, and past the cap
// of the tree that holds c (wantFill).
//
// Read waits only on the state the tracker publishes, and the tracker
// never waits on a Read.
//
// With o.MayMatch, every block the tap needs carries its receipts: Applied
// ends before the first block whose receipts are not held yet, and Read
// waits for them, fetched by hash, as it does for the receipts of Reverted
// blocks. A new head alone does not restart that wait.
func (t *ChainTracker) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
	fetched := false
	var retryAt time.Time
	rr := &readReceipts{o: o, got: make(map[[32]byte]*heldReceipts)}
	var job *receiptsJob
	var stalled *chainHeader // a block whose job ended without receipts
	var unfill func()        // set while the Read holds fill below reorgWindow
	defer func() {
		t.leaveJob(job)
		if unfill != nil {
			unfill()
		}
	}()
	for {
		s, tr := t.state.Load(), t.trees.Load()
		rr.missing = nil
		b, step, err := t.readAt(s, tr, c, rr)
		if err != nil || len(b.Applied) > 0 || len(b.Reverted) > 0 {
			return b, err
		}
		switch {
		case step == readWaitFill && unfill == nil:
			unfill = t.wantFill(c.Hash)
		case step != readWaitFill && unfill != nil:
			unfill()
			unfill = nil
		}
		var retry <-chan time.Time
		var receipts <-chan struct{}
		if m := rr.missing; m != nil && m != stalled {
			if job == nil || job.h != m {
				t.leaveJob(job)
				if job = t.joinJob(ctx, m); job == nil {
					return indexer.Batch{}, ctx.Err()
				}
			}
			receipts = job.done
		}
		if step == readFetch {
			if wait := time.Until(retryAt); wait > 0 {
				retry = time.After(wait)
			} else if !fetched {
				if retryAt, err = t.fetchCursor(ctx, c); err != nil {
					return indexer.Batch{}, err
				}
				fetched = retryAt.IsZero()
				continue
			}
		}
		trees := tr.changed
		if step == readWaitPins {
			trees = nil
		}
		select {
		case <-ctx.Done():
			return indexer.Batch{}, ctx.Err()
		case <-s.changed:
			stalled = nil
		case <-trees:
		case <-retry:
		case <-receipts:
			if job.res != nil {
				rr.got[job.h.hash] = job.res
			} else {
				stalled = job.h
			}
			job = nil
			continue
		}
		fetched = false
	}
}

// readReceipts is what one Read asks of receipts: the tap's ReadOpts, the
// receipts fetched for it that the feed does not hold, and the first block
// whose receipts it lacks.
type readReceipts struct {
	o       indexer.ReadOpts
	got     map[[32]byte]*heldReceipts
	missing *chainHeader
}

// lacks reports whether the tap needs h's receipts and has none; with
// them, blk carries them.
func (rr *readReceipts) lacks(t *ChainTracker, h *chainHeader, blk *indexer.Block) bool {
	if !t.tapNeeds(rr.o, h) {
		return false
	}
	r := rr.got[h.hash]
	if r == nil {
		r = t.heldReceiptsOf(h.hash)
	}
	if r == nil {
		return true
	}
	blk.Receipts = r.raw
	return false
}

// readStep is what a Read without a batch does next.
type readStep uint8

const (
	readWaitPins  readStep = iota // until the pins change
	readWaitTrees                 // until the pins or the held trees change
	readWaitFill                  // as readWaitTrees, while fill may place c
	readFetch                     // fetch c by hash
)

// readAt is one pass of Read's cases over one published state.
func (t *ChainTracker) readAt(s *feedState, tr *treeState, c indexer.Cursor, rr *readReceipts) (indexer.Batch, readStep, error) {
	head := s.head()
	if head == nil {
		return indexer.Batch{}, readWaitPins, nil
	}
	if p := s.pinAt(c.Number); p != nil && p.hash == c.Hash {
		return t.applied(s, c.Number+1, rr), readWaitPins, nil
	}
	if c.Number+1 == s.base && s.pins[0].parentHash == c.Hash {
		return t.applied(s, s.base, rr), readWaitPins, nil
	}
	held := t.heldHeader(c.Hash)
	if held != nil && held.number != c.Number {
		return indexer.Batch{}, readWaitPins, indexer.OutOfRangeError{Reason: indexer.OutOfRangeCursorUnknown}
	}
	if tr.state != s {
		// The trees of these pins are not published yet.
		return indexer.Batch{}, readWaitTrees, nil
	}
	if c.Number < s.base && t.baseFinal(s, tr) {
		return indexer.Batch{}, readWaitPins, indexer.OutOfRangeError{Reason: indexer.OutOfRangeBelowWindow}
	}
	if held != nil {
		return t.revert(s, tr, held, rr)
	}
	if c.Number > head.number {
		return indexer.Batch{}, readWaitTrees, nil
	}
	if c.Number < s.base {
		return indexer.Batch{}, readWaitFill, nil
	}
	return indexer.Batch{}, readFetch, nil
}

// applied is the pins from number from to the confirmed head, at most
// reorgWindow of them, ending before the first block whose receipts the tap
// needs and lacks; empty while from is above the confirmed head. from is at
// least the lowest pin.
func (t *ChainTracker) applied(s *feedState, from uint64, rr *readReceipts) indexer.Batch {
	head := s.confirmedHead()
	if head == nil || from > head.number {
		return indexer.Batch{}
	}
	last := min(head.number, from+uint64(t.window)-1)
	b := indexer.Batch{Applied: make([]indexer.Block, 0, last-from+1)}
	for n := from; n <= last; n++ {
		p := s.pinAt(n)
		blk := p.block()
		if rr.lacks(t, p, &blk) {
			if len(b.Applied) == 0 {
				rr.missing = p
			}
			break
		}
		b.Applied = append(b.Applied, blk)
	}
	if len(b.Applied) == 0 {
		return indexer.Batch{}
	}
	end := b.Applied[len(b.Applied)-1]
	b.Next = indexer.Cursor{Number: end.Number, Hash: end.Hash}
	return b
}

// revert places a held cursor h. Down its parents it meets a pin, or the
// parent of the lowest pin, the fork point: h's branch reverts down to it
// and the confirmed pins above it apply. A branch that only extends the
// head, or a fork point at or above the confirmed head, waits instead. Or
// it meets its tree's root: unlinked.
func (t *ChainTracker) revert(s *feedState, tr *treeState, h *chainHeader, rr *readReceipts) (indexer.Batch, readStep, error) {
	var reverted []indexer.Block
	for x := h; ; {
		blk := x.block()
		if rr.lacks(t, x, &blk) {
			rr.missing = x
			return indexer.Batch{}, readWaitPins, nil
		}
		reverted = append(reverted, blk)
		var b indexer.Batch
		switch p := s.pinAt(x.number - 1); {
		case x.number == s.base && x.parentHash == s.pins[0].parentHash:
			b = t.applied(s, s.base, rr)
		case x.number == 0:
			return t.unlinked(tr, x)
		case p != nil && p.hash == x.parentHash:
			b = t.applied(s, p.number+1, rr)
		default:
			parent := t.heldHeader(x.parentHash)
			if parent == nil || parent.number+1 != x.number {
				return t.unlinked(tr, x)
			}
			x = parent
			continue
		}
		if len(b.Applied) == 0 {
			return indexer.Batch{}, readWaitPins, nil
		}
		b.Reverted = reverted
		return b, readWaitPins, nil
	}
}

// unlinked is a held cursor whose tree, rooted at root, does not hold the
// pins: it waits while either tree's fill may still join them, and is out
// of range once root's fill ended and the pins' fill ended or passed below
// root (spec §9.1).
func (t *ChainTracker) unlinked(tr *treeState, root *chainHeader) (indexer.Batch, readStep, error) {
	if _, ended := tr.ended[root.hash]; ended && !tr.pinsMayJoin(root) {
		return indexer.Batch{}, readWaitPins, indexer.OutOfRangeError{Reason: indexer.OutOfRangeForkLeftWindow}
	}
	return indexer.Batch{}, readWaitFill, nil
}

// baseFinal reports whether the lowest pin is the window's base for good:
// block 0, or its tree's fill ended there.
func (t *ChainTracker) baseFinal(s *feedState, tr *treeState) bool {
	_, ended := tr.ended[s.pins[0].hash]
	return s.base == 0 || ended
}

// heldHeader is the held header with hash h, nil when none is.
func (t *ChainTracker) heldHeader(h [32]byte) *chainHeader {
	v, ok := t.held.Load(hexHash(h))
	if !ok {
		return nil
	}
	return v.(*chainHeader)
}

// fetchCursor fetches a Read's cursor by hash in class 3; the scheduler
// hands a found header to the tracker, and fetchCursor returns once the
// tracker handled it. The fetch is the Read's own: queued, it drops when
// its last Read leaves. Only a null answer is out of range; on any other
// failure it returns when the Read may fetch again: the end of the key's
// backoff.
func (t *ChainTracker) fetchCursor(ctx context.Context, c indexer.Cursor) (retryAt time.Time, err error) {
	unknown := indexer.OutOfRangeError{Reason: indexer.OutOfRangeCursorUnknown}
	if !t.canFetch() {
		return time.Time{}, unknown
	}
	spec := t.headerSpec(classFill, "eth_getBlockByHash", hexHash(c.Hash), "", false)
	res := t.fetch.submit(t, spec).wait(ctx)
	switch {
	case ctx.Err() != nil:
		return time.Time{}, ctx.Err()
	case errors.Is(res.err, errFetchNotFound):
		return time.Time{}, unknown
	case errors.Is(res.err, errSchedulerClosed):
		return time.Time{}, res.err
	case res.err != nil:
		if end := t.fetch.backoffEnd(spec); end.After(time.Now()) {
			return end, nil
		}
		return time.Now(), nil
	}
	if res.val.(*chainHeader).number != c.Number {
		return time.Time{}, unknown
	}
	t.sync(ctx)
	if ctx.Err() != nil {
		return time.Time{}, ctx.Err()
	}
	// The tracker holds no answer outside the window, so a cursor it did
	// not hold at or below the head is gone for good.
	if t.heldHeader(c.Hash) == nil {
		if head := t.state.Load().head(); head == nil || c.Number <= head.number {
			return time.Time{}, indexer.OutOfRangeError{Reason: indexer.OutOfRangeBelowWindow}
		}
	}
	return time.Time{}, nil
}

// Want registers o until release; the tracker then fetches, at pin time,
// the receipts of each block o.MayMatch wants (spec §10), and fills the
// whole window while o is Resumable.
func (t *ChainTracker) Want(o indexer.ReadOpts) (release func()) {
	var id uint64
	if o.MayMatch != nil {
		t.wantMu.Lock()
		t.wantID++
		id = t.wantID
		t.wants[id] = o
		t.wantMu.Unlock()
		t.wantVersion.Add(1)
	}
	if o.Resumable {
		t.deepFill.Add(1)
	}
	t.mb.put(trackerMsg{kind: msgTick})
	var once sync.Once
	return func() {
		once.Do(func() {
			if o.MayMatch != nil {
				t.wantMu.Lock()
				delete(t.wants, id)
				t.wantMu.Unlock()
				t.wantVersion.Add(1)
			}
			if o.Resumable {
				t.deepFill.Add(-1)
			}
			t.mb.put(trackerMsg{kind: msgTick})
		})
	}
}

// wantFill holds fill below reorgWindow, and past the cap of the tree that
// holds c, until release: for a Read that waits on fill (spec §9.1).
func (t *ChainTracker) wantFill(c [32]byte) (release func()) {
	t.deepFill.Add(1)
	t.fillMu.Lock()
	if t.fillReads == nil {
		t.fillReads = make(map[[32]byte]int)
	}
	t.fillReads[c]++
	t.fillMu.Unlock()
	t.mb.put(trackerMsg{kind: msgTick})
	return func() {
		t.fillMu.Lock()
		if t.fillReads[c]--; t.fillReads[c] == 0 {
			delete(t.fillReads, c)
		}
		t.fillMu.Unlock()
		t.deepFill.Add(-1)
		t.mb.put(trackerMsg{kind: msgTick})
	}
}

// Tip returns the head now, without waiting.
func (t *ChainTracker) Tip() (indexer.Cursor, bool) {
	if h := t.state.Load().head(); h != nil {
		return h.cursor(), true
	}
	return indexer.Cursor{}, false
}

// Confirmed returns the confirmed head now, without waiting.
func (t *ChainTracker) Confirmed() (indexer.Cursor, bool) {
	if h := t.state.Load().confirmedHead(); h != nil {
		return h.cursor(), true
	}
	return indexer.Cursor{}, false
}

// Pinned reports whether c is pinned now. A tap checks each block just
// before it sends it, so a block a reorg reverted after the tap's Read is
// never sent.
func (t *ChainTracker) Pinned(c indexer.Cursor) bool { return t.state.Load().pinned(c) != nil }

// Changed is closed once the tracker publishes a newer state: the pins,
// the confirmed head or its hash voters may have moved.
func (t *ChainTracker) Changed() <-chan struct{} { return t.state.Load().changed }

// TxHeadAt is when the head last moved to a block with transactions; zero
// before one. Pending-transaction sources stay silent only while it is old.
func (t *ChainTracker) TxHeadAt() time.Time {
	if at := t.txHeadAt.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}

// ReorgWindow is the tracker's window in blocks.
func (t *ChainTracker) ReorgWindow() int { return t.window }

// BlockTime is the network's block time (GetNetworkBlockTime, which the
// poller's debounce reads too); health.ColdBlockTime until the first
// sample, 2 timestamp changes after the first observed header (spec §6.7).
func (t *ChainTracker) BlockTime() time.Duration {
	if n, ok := t.network.(interface{ EvmBlockTime() time.Duration }); ok {
		if bt := n.EvmBlockTime(); bt > 0 {
			return bt
		}
	}
	return health.ColdBlockTime
}

// W is the freshness window (health.FreshnessWindow, spec §1). Hash
// voters, the cold window, the reconciler timer, the suspension window and
// every re-arm read it.
func (t *ChainTracker) W() time.Duration {
	return health.FreshnessWindow(t.BlockTime())
}

// --- integrity.History, ChainSegment, ReceiptCache, PinReconfirmer ---

// HashAt is the pin at a number, or a finalized anchor below the window:
// inside the window the pins win.
func (t *ChainTracker) HashAt(number int64) (string, bool) {
	if number < 0 {
		return "", false
	}
	s := t.state.Load()
	if p := s.pinAt(uint64(number)); p != nil {
		return p.hashHex, true
	}
	if e := t.anchorBelow(s, number); e != nil {
		return e.hash, true
	}
	return "", false
}

// anchorBelow is the finalized anchor at number when s pins nothing there:
// below the pins, or with no pins; nil otherwise.
func (t *ChainTracker) anchorBelow(s *feedState, number int64) *anchorEntry {
	if len(s.pins) > 0 && uint64(number) >= s.base {
		return nil
	}
	if e, ok := t.anchors.Load(number); ok {
		return e.(*anchorEntry)
	}
	return nil
}

// confirmation is how ReconfirmPin reports an anchor: PinFresh only while
// the by-number answers of 2 or more vendors agree on it.
func (e *anchorEntry) confirmation() integrity.PinConfirmation {
	if e.agreed {
		return integrity.PinFresh
	}
	return integrity.PinRateLimited
}

// FollowedRange is the pins: one parentHash-linked chain from a root to the
// head.
func (t *ChainTracker) FollowedRange() (from, to int64, ok bool) {
	s := t.state.Load()
	if len(s.pins) == 0 {
		return 0, 0, false
	}
	return int64(s.base), int64(s.head().number), true
}

// HeaderAt is the pinned header at a height. Held data only: a consecutive
// check never triggers a fetch, and a miss means the check skips.
func (t *ChainTracker) HeaderAt(number int64) (*integrity.Header, bool) {
	if number < 0 {
		return nil, false
	}
	if p := t.state.Load().pinAt(uint64(number)); p != nil {
		return p.parsed, true
	}
	return nil, false
}

// HeaderByHash is a held header by hash, pinned or not. It never fetches.
func (t *ChainTracker) HeaderByHash(hash string) (*integrity.Header, bool) {
	key, ok := hashKey(hash)
	if !ok {
		return nil, false
	}
	if h, ok := t.held.Load(key); ok {
		return h.(*chainHeader).parsed, true
	}
	return nil, false
}

// CachedReceipts is a held-data read of a block's receipts by hash. Logs
// held from the eth_getLogs fallback are not receipts integrity can check.
func (t *ChainTracker) CachedReceipts(blockHash string) ([]integrity.Receipt, bool) {
	var hash [32]byte
	if !decodeHex32(blockHash, &hash) {
		return nil, false
	}
	r := t.heldReceiptsOf(hash)
	if r == nil || r.fromLogs {
		return nil, false
	}
	return r.integrityView(), true
}

// heldHistory is what a check on eRPC's own data reads (a tracker fetch, a
// source header): the tracker's held headers and receipts, and never its
// pins, since that data is what moves them. It fetches nothing (spec §7).
type heldHistory struct{ t *ChainTracker }

func (h heldHistory) HashAt(int64) (string, bool) { return "", false }
func (h heldHistory) FollowedRange() (int64, int64, bool) {
	return h.t.FollowedRange()
}
func (h heldHistory) HeaderAt(number int64) (*integrity.Header, bool) { return h.t.HeaderAt(number) }
func (h heldHistory) HeaderByHash(hash string) (*integrity.Header, bool) {
	return h.t.HeaderByHash(hash)
}
func (h heldHistory) CachedReceipts(blockHash string) ([]integrity.Receipt, bool) {
	return h.t.CachedReceipts(blockHash)
}

var (
	_ integrity.ChainSegment = heldHistory{}
	_ integrity.ReceiptCache = heldHistory{}
)

// ReconfirmPin reports the pin at number once it is current: at once while
// it is PinFresh (spec §6.5): confirmed, backed by the evidence quorum of
// vendors, and no counted vote, walking or not, lies off its branch, so
// neither a pin resting on one vendor nor one a reorg may yet replace
// licenses a hard reject. Otherwise the tracker settles first (§6 rule 5)
// and the pin it leaves is returned: PinFresh only when the settle decided
// and no walking vote lies off the pin's branch, so never for a hash the
// tracker did not pin.
//
// Within reconfirmCooldown, and whenever the settle did not decide (its
// answers tie, it would wait on a backoff, or no upstream answered), it
// returns the current pin as PinRateLimited, which carries no new
// evidence, so the engine degrades the verdict instead of rejecting on a
// pin it could not re-check. Its settle asks are client fetches. A
// finalized anchor below the pins never settles: it is PinFresh only while
// 2 or more vendors agree on it. Only a number with neither pin nor anchor
// is PinUnverifiable.
func (t *ChainTracker) ReconfirmPin(ctx context.Context, number int64) (string, integrity.PinConfirmation) {
	if number < 0 {
		return "", integrity.PinUnverifiable
	}
	n := uint64(number)
	s := t.state.Load()
	p := s.pinAt(n)
	if p != nil && s.backing.confirms(n) {
		return p.hashHex, integrity.PinFresh
	}
	if p == nil {
		if e := t.anchorBelow(s, number); e != nil {
			return e.hash, e.confirmation()
		}
	}
	t.reconfirmMu.Lock()
	at, recent := t.reconfirmedAt[number]
	t.reconfirmMu.Unlock()
	if recent && p != nil && time.Since(at) < reconfirmCooldown {
		return p.hashHex, integrity.PinRateLimited
	}
	outcome := t.settleNow(ctx)
	t.reconfirmMu.Lock()
	t.reconfirmedAt[number] = time.Now()
	for k := range t.reconfirmedAt {
		if k < number-int64(t.window) {
			delete(t.reconfirmedAt, k)
		}
	}
	t.reconfirmMu.Unlock()
	s = t.state.Load()
	if p = s.pinAt(n); p == nil {
		if e := t.anchorBelow(s, number); e != nil {
			return e.hash, e.confirmation()
		}
		return "", integrity.PinUnverifiable
	}
	if outcome == settleDecided && n-s.base < uint64(s.backing.undisputed) {
		return p.hashHex, integrity.PinFresh
	}
	return p.hashHex, integrity.PinRateLimited
}

// settleNow asks the tracker for a settle and waits for its outcome,
// bounded by ctx.
func (t *ChainTracker) settleNow(ctx context.Context) settleOutcome {
	settled := make(chan settleOutcome, 1)
	t.mb.put(trackerMsg{kind: msgSettle, settled: settled})
	select {
	case o := <-settled:
		return o
	case <-ctx.Done():
		return settleUnanswered
	}
}

// headerByHash returns the held header for a hash, fetching it on a miss.
func (t *ChainTracker) headerByHash(ctx context.Context, hash string) (*integrity.Header, bool) {
	key, ok := hashKey(hash)
	if !ok {
		return nil, false
	}
	if h, ok := t.HeaderByHash(key); ok {
		return h, true
	}
	h, err := t.fetchHeader(ctx, classHead, "eth_getBlockByHash", key)
	if err != nil {
		return nil, false
	}
	return h.parsed, true
}

// headerByNumber returns the pinned header at a number, fetching the block
// by number on a miss.
func (t *ChainTracker) headerByNumber(ctx context.Context, number int64, blockRef string) (*integrity.Header, bool) {
	if h, ok := t.HeaderAt(number); ok {
		return h, true
	}
	h, err := t.fetchHeader(ctx, t.fetchClassAt(number, classHead), "eth_getBlockByNumber", blockRef)
	if err != nil {
		return nil, false
	}
	return h.parsed, true
}

// receiptsByHash returns a block's receipts, fetching them on a miss. number
// is the block's number when the caller knows it, else -1.
func (t *ChainTracker) receiptsByHash(ctx context.Context, blockHash string, number int64) ([]integrity.Receipt, bool) {
	if r, ok := t.CachedReceipts(blockHash); ok {
		return r, true
	}
	return t.resolveReceipts(ctx, blockHash, number)
}

// fetchClassAt is near for a height within the window below the head, or
// with no head or height known; fill for anything older.
func (t *ChainTracker) fetchClassAt(number int64, near fetchClass) fetchClass {
	if head := t.state.Load().head(); head != nil && number >= 0 && uint64(number)+uint64(t.window) < head.number {
		return classFill
	}
	return near
}

// finalityLabel classifies a fetched block against the group's finalized
// height.
func (t *ChainTracker) finalityLabel(number int64) string {
	if number < 0 {
		return "unknown"
	}
	fin := t.finalizedHead()
	if fin <= 0 {
		return "unknown"
	}
	if number <= fin {
		return "finalized"
	}
	return "unfinalized"
}

// auxKindHeader labels an integrity check's header fetch.
const auxKindHeader = "canonical_header"

// resolveTag fetches the header a block tag names, such as "finalized".
func (t *ChainTracker) resolveTag(ctx context.Context, tag string) (*integrity.Header, bool) {
	h, err := t.fetchHeader(ctx, classHead, "eth_getBlockByNumber", tag)
	if err != nil {
		return nil, false
	}
	return h.parsed, true
}

// headerSpec is one header fetch, group-scoped. client: on a client request
// path, so it never waits out the key's backoff.
func (t *ChainTracker) headerSpec(class fetchClass, method, blockRef, upstream string, client bool) fetchSpec {
	return fetchSpec{
		class:    class,
		kind:     "header",
		selector: t.selector,
		upstream: upstream,
		method:   method,
		params:   `["` + blockRef + `",false]`,
		byHash:   method == "eth_getBlockByHash",
		client:   client,
		decode: func(result []byte) (any, error) {
			if r := bytes.TrimSpace(result); len(r) == 0 || bytes.Equal(r, []byte("null")) {
				return nil, errFetchNotFound
			}
			h, err := parseChainHeader(result)
			if err != nil {
				return nil, err
			}
			// A by-hash fetch must return that hash, a by-number fetch that
			// number; a node answering with another block would otherwise be
			// held under the wrong key.
			if !headerMatchesRef(h, method, blockRef) {
				return nil, errFetchUndecoded
			}
			return h, nil
		},
	}
}

// fetchHeader fetches a header for an integrity check through the
// scheduler; the scheduler hands the answer to the tracker without a vote.
func (t *ChainTracker) fetchHeader(ctx context.Context, class fetchClass, method, blockRef string) (*chainHeader, error) {
	if t.network == nil {
		return nil, errNoTrackerSweep
	}
	res := t.fetch.fetch(ctx, t, t.headerSpec(class, method, blockRef, "", true))
	if errors.Is(res.err, errFetchBackoff) {
		return nil, res.err
	}
	h, _ := res.val.(*chainHeader)
	num := int64(-1)
	if h != nil {
		num = int64(h.number)
	}
	t.emitAux(auxKindHeader, method, t.finalityLabel(num), h != nil)
	if h == nil {
		if res.err == nil {
			return nil, errFetchUndecoded
		}
		return nil, res.err
	}
	return h, nil
}

// headerMatchesRef reports whether a fetched header is the block the request
// anchored on: same hash by hash, same number by number (tags have no
// anchor).
func headerMatchesRef(h *chainHeader, method, blockRef string) bool {
	switch method {
	case "eth_getBlockByHash":
		key, ok := hashKey(blockRef)
		return ok && key == h.hashHex
	case "eth_getBlockByNumber":
		if want, err := common.HexToInt64(blockRef); err == nil {
			return int64(h.number) == want
		}
	}
	return true
}

// fetched hands a tracker fetch's answer to the mailbox, where it is never
// shed.
func (t *ChainTracker) fetched(res fetchResult) {
	switch v := res.val.(type) {
	case *chainHeader:
		t.observeFetched(res.upstream, v)
	case *heldReceipts:
		t.mb.put(trackerMsg{kind: msgReceipts, held: v})
	}
}

// resolveReceipts fetches a block's receipts by hash through the
// scheduler, group-scoped, with the answer and bloom checks of the feed's
// own fetches; the scheduler hands them to the tracker. Only a block known
// to lie within reorgWindow of the head is tip receipts; any other is fill.
func (t *ChainTracker) resolveReceipts(ctx context.Context, blockHash string, number int64) ([]integrity.Receipt, bool) {
	if t.network == nil {
		return nil, false
	}
	var hash [32]byte
	if !decodeHex32(blockHash, &hash) {
		return nil, false
	}
	h := t.heldHeader(hash)
	if h != nil {
		number = int64(h.number)
	}
	class := classFill
	if number >= 0 {
		class = t.fetchClassAt(number, classTipReceipts)
	}
	spec := t.receiptsSpec(class, hash, h, false)
	spec.client = true
	res := t.fetch.fetch(ctx, t, spec)
	if errors.Is(res.err, errFetchBackoff) {
		return nil, false
	}
	if errors.Is(res.err, errBloomMismatch) && h != nil {
		t.countBloomMismatch(res.upstream, spec.method, h)
	}
	r, ok := res.val.(*heldReceipts)
	num := int64(-1)
	if ok {
		num = r.number
	}
	t.emitAux("canonical_receipts", "eth_getBlockReceipts", t.finalityLabel(num), ok)
	if !ok {
		return nil, false
	}
	return r.integrityView(), true
}

// emitAux records an auxiliary fetch, labeled with the group, the method and
// the target block's finality.
func (t *ChainTracker) emitAux(kind, method, finality string, ok bool) {
	outcome := "error"
	if ok {
		outcome = "ok"
	}
	telemetry.MetricIntegrityAuxRequest.WithLabelValues(
		t.network.ProjectId(), "", t.network.Label(), "", t.group, kind, method, finality, outcome,
	).Inc()
}

// projectId / networkLabel are nil-safe metric label helpers.
func (t *ChainTracker) projectId() string {
	if t.network == nil {
		return ""
	}
	return t.network.ProjectId()
}

func (t *ChainTracker) networkLabel() string {
	if t.network == nil {
		return ""
	}
	return t.network.Label()
}

// observeBlockView hands a validated block response to the tracker as an
// answer: held, never voted.
func observeBlockView(ctx context.Context, t *ChainTracker, u common.Upstream, rs *common.NormalizedResponse) {
	if t == nil || rs == nil {
		return
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return
	}
	h, err := parseChainHeader(bytes.Clone(jrr.GetResultBytes()))
	if err != nil {
		return
	}
	upstreamId := ""
	if u != nil {
		upstreamId = u.Id()
	}
	t.observeAnswer(upstreamId, h)
}

func isBlockMethod(methodLower string) bool {
	return methodLower == "eth_getblockbynumber" || methodLower == "eth_getblockbyhash"
}

// isAnchoredNarrowMethod reports methods whose response carries a single
// block's {number, hash} (receipts/tx).
func isAnchoredNarrowMethod(methodLower string) bool {
	switch methodLower {
	case "eth_gettransactionreceipt", "eth_getblockreceipts", "eth_gettransactionbyhash":
		return true
	}
	return false
}

type blockAnchorLite struct {
	BlockNumber string `json:"blockNumber"`
	BlockHash   string `json:"blockHash"`
}

// observeNarrowView feeds finalized anchors from a narrow response
// (receipts/tx), judged against the lane's Head.Finalized (spec §6.5).
func observeNarrowView(ctx context.Context, t *ChainTracker, u common.Upstream, rs *common.NormalizedResponse) {
	if t == nil || rs == nil {
		return
	}
	if _, ok := u.(common.EvmUpstream); !ok {
		return
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return
	}
	t.observeNarrowAnchors(t.finalizedHead(), vendorKey(u), jrr.GetResultBytes())
}

// observeNarrowAnchors records number→hash anchors from vendor's narrow
// response, only for finalized blocks: a single narrow response must not
// redefine a block at a jittery tip, but a finalized answer is settled.
func (t *ChainTracker) observeNarrowAnchors(fin int64, vendor string, result []byte) {
	if t == nil || fin <= 0 || len(result) == 0 {
		return
	}
	var as []anchor
	add := func(numHex, hash string) {
		key, ok := hashKey(hash)
		if !ok || numHex == "" {
			return
		}
		if n, err := common.HexToInt64(numHex); err == nil && n >= 0 && n <= fin {
			as = append(as, anchor{number: n, hash: key, vendor: vendor})
		}
	}
	var arr []blockAnchorLite
	if common.SonicCfg.Unmarshal(result, &arr) == nil && len(arr) > 0 {
		for i := range arr {
			add(arr[i].BlockNumber, arr[i].BlockHash)
		}
	} else {
		var one blockAnchorLite
		if common.SonicCfg.Unmarshal(result, &one) == nil {
			add(one.BlockNumber, one.BlockHash)
		}
	}
	if len(as) > 0 {
		t.mb.put(trackerMsg{kind: msgAnchors, anchors: as})
	}
}

// HasVerifiedEmptyTraceBlock reports whether the pinned header at the
// request's block proves it has no transactions and used no gas: its hash
// recomputes, its transactionsRoot is the empty-trie root and its gasUsed
// is 0. Header fields alone prove it, since source headers carry no
// transactions, but only fields the hash commits to; a chain whose profile
// turns transactionsRootRecompute off may leave system transactions out of
// the root, so it proves nothing there. It never fetches.
func HasVerifiedEmptyTraceBlock(req *common.NormalizedRequest) bool {
	if req == nil || req.Network() == nil {
		return false
	}
	ctx := context.Background()
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return false
	}
	jrq.RLockWithTrace(ctx)
	var ref string
	if len(jrq.Params) > 0 {
		ref, _ = jrq.Params[0].(string)
	}
	jrq.RUnlock()
	if original, ok := req.EvmBlockRef().(string); ok && original != "" && original != "*" && (original[0] < '0' || original[0] > '9') {
		return false
	}
	if _, ok := trimHexPrefix(ref); !ok || len(ref) < 3 {
		return false
	}
	number, err := common.HexToInt64(ref)
	if err != nil || number < 0 {
		return false
	}
	n := req.Network()
	groupKey := ""
	if dirs := req.Directives(); dirs != nil && dirs.UseUpstream != "" {
		if gn, ok := n.(interface {
			EvmUpstreamGroupForSelector(context.Context, string) (string, string)
		}); ok {
			groupKey, _ = gn.EvmUpstreamGroupForSelector(ctx, dirs.UseUpstream)
		}
	}
	t := lookupChainTracker(n, groupKey)
	if t == nil {
		return false
	}
	h := t.state.Load().pinAt(uint64(number))
	if h == nil || !h.emptyTxs || t.fetch.bloom.prof().emptyOff || !h.hashRecomputes() {
		return false
	}
	gas, err := common.HexToInt64(h.parsed.GasUsed)
	return err == nil && gas == 0
}

// --- registry: one tracker per (network, group) ---

type networkTrackers struct {
	ctx      context.Context
	window   int
	backfill time.Duration
	fetch    *fetchScheduler
	// suspend is the network's unanimous-failure state of header checks.
	suspend *checkSuspensions
	// checks is the network's compiled header checks.
	checks atomic.Pointer[headerChecks]
	mu     sync.Mutex
	groups map[string]*ChainTracker // group key ("" network-wide) -> tracker
}

var chainTrackers sync.Map // common.Network -> *networkTrackers

// StartChainTrackers starts the network-wide tracker of n and registers n
// for group trackers; all of them stop, and n leaves the registry, when ctx
// ends.
func StartChainTrackers(ctx context.Context, n common.Network) *ChainTracker {
	ct := common.EvmChainTrackerConfig{}
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil && cfg.Evm.ChainTracker != nil {
		ct = *cfg.Evm.ChainTracker
	}
	ct.SetDefaults()
	nt := &networkTrackers{ctx: ctx, window: ct.ReorgWindow, backfill: ct.MaxBackfill.Duration(), fetch: newFetchScheduler(ctx, n), groups: make(map[string]*ChainTracker)}
	nt.suspend = newCheckSuspensions(n)
	if v, loaded := chainTrackers.LoadOrStore(n, nt); loaded {
		nt = v.(*networkTrackers)
	} else {
		context.AfterFunc(ctx, func() { chainTrackers.CompareAndDelete(n, nt) })
	}
	t := nt.get(n, "", "", "")
	nt.suspend.tracker.CompareAndSwap(nil, t)
	return t
}

func registeredTrackers(n common.Network) *networkTrackers {
	if v, ok := chainTrackers.Load(n); ok {
		return v.(*networkTrackers)
	}
	return nil
}

func lookupChainTracker(n common.Network, groupKey string) *ChainTracker {
	nt := registeredTrackers(n)
	if nt == nil {
		return nil
	}
	nt.mu.Lock()
	defer nt.mu.Unlock()
	return nt.groups[groupKey]
}

func (nt *networkTrackers) get(n common.Network, groupKey, selector, lane string) *ChainTracker {
	nt.mu.Lock()
	defer nt.mu.Unlock()
	if t := nt.groups[groupKey]; t != nil {
		return t
	}
	// Only the network-wide tracker holds maxBackfill of chain time; group
	// trackers hold reorgWindow blocks.
	backfill := time.Duration(0)
	if selector == "" {
		backfill = nt.backfill
	}
	var head func() common.EvmHead
	if ln, ok := n.(common.EvmNetwork); ok {
		head = func() common.EvmHead { return ln.EvmLaneHead(groupKey, selector) }
	}
	t := newChainTracker(nt.ctx, n, nt.window, backfill, selector, groupKey, lane, head, nt.fetch)
	nt.groups[groupKey] = t
	if cfg := n.Config(); cfg != nil && cfg.Integrity != nil && cfg.Integrity.Follow.IsEnabled() {
		// The state prober rides on the follow tap: its pinned headers are
		// the anchor for judging an upstream's state claims.
		if cfg.Integrity.StateProbe.IsEnabled() {
			startStateProber(nt.ctx, n, t, cfg.Integrity.StateProbe)
		}
		nt.fetch.bg.Go(func() { t.follow(nt.ctx) })
	}
	return t
}

// ChainTrackerOf returns the tracker of n's node group for a use-upstream
// selector, the same grouping as latest-block tracking
// (EvmUpstreamGroupForSelector). A selector that carves out no real
// sub-group yields the network-wide tracker. nil while n has no trackers.
func ChainTrackerOf(ctx context.Context, n common.Network, selector string) *ChainTracker {
	if n == nil {
		return nil
	}
	nt := registeredTrackers(n)
	if nt == nil {
		return nil
	}
	var groupKey, lane string
	if selector != "" {
		if gn, ok := n.(interface {
			EvmUpstreamGroupForSelector(context.Context, string) (string, string)
		}); ok {
			if k, l := gn.EvmUpstreamGroupForSelector(ctx, selector); k != "" {
				groupKey, lane = k, l
			}
		}
	}
	if groupKey == "" {
		selector = ""
	}
	return nt.get(n, groupKey, selector, lane)
}

// ObserveSourceHeader hands an accepted source header to the network-wide
// tracker and to every group tracker whose selector matches u, as u's vote.
// Each tracker drops it unless u is one of its lane's voters.
func ObserveSourceHeader(n common.Network, u common.Upstream, sh SourceHeader) {
	nt := registeredTrackers(n)
	if nt == nil {
		return
	}
	id := u.Id()
	nt.mu.Lock()
	for _, t := range nt.groups {
		if t.selector == "" {
			t.observeSource(id, sh.h)
			continue
		}
		if m, _ := common.UpstreamMatchesSelector(t.selector, u); m {
			t.observeSource(id, sh.h)
		}
	}
	nt.mu.Unlock()
}

// VotersChanged makes every tracker of n read its voters again.
func VotersChanged(n common.Network) { tickTrackers(n) }

// LatestChanged makes the tracker of n's lane groupKey ("" network-wide)
// gate its sends on the lane's Latest again (spec §6.5); the other lanes'
// trackers did not move. It never blocks.
func LatestChanged(n common.Network, groupKey string) {
	nt := registeredTrackers(n)
	if nt == nil {
		return
	}
	nt.mu.Lock()
	t := nt.groups[groupKey]
	nt.mu.Unlock()
	if t != nil {
		t.VotersChanged()
	}
}

// DemandChanged makes every tracker of n check whether it asks for latest
// with no hash voter (§6 rule 6). It never blocks.
func DemandChanged(n common.Network) { tickTrackers(n) }

// HeldNumber is the number of the header with hash that t holds; false
// when it holds none, or t is nil. It never fetches and takes no lock.
func (t *ChainTracker) HeldNumber(hash string) (int64, bool) {
	if t == nil {
		return 0, false
	}
	key, ok := hashKey(hash)
	if !ok {
		return 0, false
	}
	if h, ok := t.held.Load(key); ok {
		return int64(h.(*chainHeader).number), true
	}
	return 0, false
}

func tickTrackers(n common.Network) {
	nt := registeredTrackers(n)
	if nt == nil {
		return
	}
	nt.mu.Lock()
	defer nt.mu.Unlock()
	for _, t := range nt.groups {
		t.VotersChanged()
	}
}

var (
	_ indexer.ChainFeed        = (*ChainTracker)(nil)
	_ integrity.History        = (*ChainTracker)(nil)
	_ integrity.ChainSegment   = (*ChainTracker)(nil)
	_ integrity.ReceiptCache   = (*ChainTracker)(nil)
	_ integrity.PinReconfirmer = (*ChainTracker)(nil)
)
