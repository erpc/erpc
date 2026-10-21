package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
)

// cacheSlack keeps a few extra receipts beyond the window so concurrent forks
// near the tip don't evict something still needed.
const cacheSlack = 2

// reconfirmCooldown bounds how often one block number's pin is re-confirmed
// against a fresh canonical fetch: within the cooldown the current pin is
// trusted as-is, so a fork-flapping tip does not re-fetch per request.
const reconfirmCooldown = time.Second

// mailboxHeaderCap bounds the headers waiting in a tracker's mailbox.
var mailboxHeaderCap = 1024

// Live bound and block-time limits (spec §1).
var (
	liveBoundFloor   = 2 * time.Second
	liveBoundCeiling = 60 * time.Second
	defaultBlockTime = 12 * time.Second
)

// trackerPanicsToReset is how many messages in a row may panic before the
// tracker resets to empty.
const trackerPanicsToReset = 3

// ChainTracker is fork choice for one (network, group) on one goroutine, and
// the only writer of its chain feed: headers by hash, pins, receipts. Votes
// move the pins (spec §6 rules 3 and 4); answers are only held.
type ChainTracker struct {
	network common.Network
	// selector is the use-upstream selector of a group tracker, "" for the
	// network-wide one; group is its lane label.
	selector  string
	group     string
	window    int
	finalized func() int64

	mb    mailbox
	state atomic.Pointer[feedState]

	// held, receipts and anchors are read by any goroutine; only the
	// tracker's goroutine writes them.
	held     sync.Map // lowercase hash -> *chainHeader
	receipts sync.Map // lowercase block hash -> []integrity.Receipt
	anchors  sync.Map // int64 -> lowercase hash: finalized narrow anchors

	wantMu sync.Mutex
	wants  map[uint64]indexer.ReadOpts
	wantID uint64

	reconfirmMu   sync.Mutex
	reconfirmedAt map[int64]time.Time

	// fetch runs every fetch of this tracker; one per network.
	fetch *fetchScheduler

	// lastCountedVote is when a source vote that counts last arrived, in
	// unix nanoseconds.
	lastCountedVote atomic.Int64

	// Owned by the tracker's goroutine.
	nodes         map[[32]byte]*node
	waiting       map[[32]byte][]*node // held roots by the parent hash they lack
	votes         map[string]*node     // source upstream id -> its vote
	askVote       *node                // the no-source vote (§6 rule 6)
	pins          []*node              // root..head, parentHash-linked
	maxHeld       uint64
	receiptsOrder []string
	anchorNumbers []int64
	panics        int
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
	blockTime time.Duration // mean header-time step over the pins; 0 unknown
	changed   chan struct{} // closed once a newer state is published
}

func (s *feedState) head() *chainHeader {
	if len(s.pins) == 0 {
		return nil
	}
	return s.pins[len(s.pins)-1]
}

func (s *feedState) pinAt(n uint64) *chainHeader {
	if len(s.pins) == 0 || n < s.base || n-s.base >= uint64(len(s.pins)) {
		return nil
	}
	return s.pins[n-s.base]
}

type voteKind uint8

const (
	voteNone   voteKind = iota // a client or integrity answer
	voteSource                 // a source header that passed intake
	voteAsk                    // the no-source answer (§6 rule 6)
)

type msgKind uint8

const (
	msgHeader  msgKind = iota // a source vote, an ask vote or a client answer
	msgFetched                // a tracker fetch's answer; never shed
	msgAnchors
	msgReceipts
	msgTick
	msgSync
)

type trackerMsg struct {
	kind     msgKind
	upstream string
	vote     voteKind
	raw      json.RawMessage
	hdr      *chainHeader // set when the sender already parsed raw
	anchors  []anchor
	key      string
	receipts []integrity.Receipt
	done     chan struct{}
}

type anchor struct {
	number int64
	hash   string
}

// mailbox never blocks a sender. It sheds only client answers and source
// headers: when they fill it, answers drop first, then each source keeps
// only its newest header. Fetch results, asks, anchors and receipts never
// drop; at most one tick waits.
type mailbox struct {
	mu    sync.Mutex
	queue []trackerMsg
	shed  int // sheddable messages queued
	tick  bool
	wake  chan struct{}
}

func sheddable(msg trackerMsg) bool {
	return msg.kind == msgHeader && msg.vote != voteAsk
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

func newChainTracker(ctx context.Context, n common.Network, window int, selector, group string, finalized func() int64, fetch *fetchScheduler) *ChainTracker {
	t := &ChainTracker{
		network:       n,
		selector:      selector,
		group:         group,
		window:        window,
		finalized:     finalized,
		fetch:         fetch,
		mb:            mailbox{wake: make(chan struct{}, 1)},
		wants:         make(map[uint64]indexer.ReadOpts),
		reconfirmedAt: make(map[int64]time.Time),
		nodes:         make(map[[32]byte]*node),
		waiting:       make(map[[32]byte][]*node),
		votes:         make(map[string]*node),
	}
	t.state.Store(&feedState{changed: make(chan struct{})})
	go t.run(ctx)
	return t
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

// LivenessChanged makes the tracker count its votes again: a source that
// stopped being live no longer votes.
func (t *ChainTracker) LivenessChanged() {
	t.mb.put(trackerMsg{kind: msgTick})
}

func (t *ChainTracker) observeAnswer(upstreamId string, h *chainHeader) {
	t.mb.put(trackerMsg{kind: msgHeader, upstream: upstreamId, vote: voteNone, hdr: h})
}

func (t *ChainTracker) observeFetched(upstreamId string, h *chainHeader) {
	t.mb.put(trackerMsg{kind: msgFetched, upstream: upstreamId, vote: voteNone, hdr: h})
}

// voteAsk makes h the no-source vote and waits until the tracker applied it.
func (t *ChainTracker) voteAsk(ctx context.Context, h *chainHeader) {
	t.mb.put(trackerMsg{kind: msgHeader, vote: voteAsk, hdr: h})
	t.sync(ctx)
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

// handle applies one message. A panic drops the message; 3 in a row reset
// the tracker to empty.
func (t *ChainTracker) handle(msg trackerMsg) {
	defer func() {
		if msg.done != nil {
			close(msg.done)
		}
		rec := recover()
		if rec == nil {
			t.panics = 0
			return
		}
		telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
			"chain-tracker",
			fmt.Sprintf("project:%s network:%s group:%s", t.projectId(), t.networkLabel(), t.group),
			common.ErrorFingerprint(rec),
		).Inc()
		log.Error().Interface("panic", rec).Str("stack", string(debug.Stack())).
			Str("network", t.networkLabel()).Str("group", t.group).Msg("chain tracker dropped a message that panicked")
		if msg.kind == msgHeader && msg.vote == voteSource {
			telemetry.CounterHandle(telemetry.MetricChainTrackerSourceHeadsTotal,
				t.projectId(), t.networkLabel(), msg.upstream, "malformed").Inc()
		}
		t.panics++
		if t.panics >= trackerPanicsToReset {
			t.panics = 0
			t.reset()
			telemetry.CounterHandle(telemetry.MetricChainTrackerResetTotal, t.projectId(), t.networkLabel()).Inc()
		}
	}()
	switch msg.kind {
	case msgHeader, msgFetched:
		t.handleHeader(msg)
	case msgAnchors:
		t.handleAnchors(msg.anchors)
	case msgReceipts:
		t.handleReceipts(msg.key, msg.receipts)
	case msgTick:
		t.choose()
	case msgSync:
	}
}

func (t *ChainTracker) handleHeader(msg trackerMsg) {
	h := msg.hdr
	if h == nil {
		var err error
		if h, err = parseChainHeader(msg.raw); err != nil {
			return
		}
	}
	nd := t.nodes[h.hash]
	if nd == nil {
		// Rule 1: every vote is held; a vote whose parent is not held is a
		// root. Only answers must lie inside the window.
		if msg.vote == voteNone && !t.inWindow(h) {
			return
		}
		nd = t.hold(h)
	}
	switch msg.vote {
	case voteSource:
		// Rule 3: an ancestor of the current vote does not move it back.
		if cur := t.votes[msg.upstream]; cur == nil || !isAncestor(nd, cur) {
			t.votes[msg.upstream] = nd
		}
		if t.counts(msg.upstream) {
			t.lastCountedVote.Store(time.Now().UnixNano())
		}
	case voteAsk:
		t.askVote = nd
	case voteNone:
		if len(t.pins) == 0 {
			t.evict()
		}
		return
	}
	t.choose()
}

// inWindow reports whether h lies at most reorgWindow blocks below or above
// the head. Without pins every header is.
func (t *ChainTracker) inWindow(h *chainHeader) bool {
	if len(t.pins) == 0 {
		return true
	}
	head := t.pins[len(t.pins)-1].h.number
	return h.number+uint64(t.window) >= head && h.number <= head+uint64(t.window)
}

// counts is the network intake's predicate for a source's vote: live and
// authoritative now. A network without intake counts every vote.
func (t *ChainTracker) counts(upstreamId string) bool {
	if n, ok := t.network.(interface{ EvmSourceVotes(string) bool }); ok {
		return n.EvmSourceVotes(upstreamId)
	}
	return true
}

// hold adds h to the held trees: under its parent when held, and over every
// held root that names it as parent. Rule 1: a header whose parent is not
// held is a root.
func (t *ChainTracker) hold(h *chainHeader) *node {
	nd := &node{h: h}
	if p := t.nodes[h.parentHash]; p != nil && p.h.number+1 == h.number {
		nd.parent = p
		p.children = append(p.children, nd)
	} else {
		t.waiting[h.parentHash] = append(t.waiting[h.parentHash], nd)
	}
	for _, c := range t.waiting[h.hash] {
		if c.h.number == h.number+1 {
			c.parent = nd
			nd.children = append(nd.children, c)
		}
	}
	delete(t.waiting, h.hash)
	t.nodes[h.hash] = nd
	t.held.Store(h.hashHex, h)
	if h.number > t.maxHeld {
		t.maxHeld = h.number
	}
	return nd
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

// countedVotes are the votes of live authoritative sources, or the
// no-source vote while none of those votes.
func (t *ChainTracker) countedVotes() []*node {
	out := make([]*node, 0, len(t.votes)+1)
	for u, v := range t.votes {
		if t.counts(u) {
			out = append(out, v)
		}
	}
	if len(out) == 0 && t.askVote != nil {
		out = append(out, t.askVote)
	}
	return out
}

// choose is rule 4: the tree with the most votes, then from its root the
// child whose subtree holds the most votes. Ties keep the pins.
func (t *ChainTracker) choose() {
	counted := t.countedVotes()
	if len(counted) == 0 {
		return
	}
	counts := make(map[*node]int)
	roots := make(map[*node]struct{})
	for _, v := range counted {
		for a := v; a != nil; a = a.parent {
			counts[a]++
			if a.parent == nil {
				roots[a] = struct{}{}
			}
		}
	}
	var pinRoot *node
	if len(t.pins) > 0 {
		pinRoot = rootOf(t.pins[0])
	}
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

	path := []*node{root}
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
	t.apply(path)
}

// apply makes path the pins in one step: the pins inside the window are
// computed first, compared with the published state, then committed and
// published together. The network-wide tracker moves the network's latest
// block before taps wake.
func (t *ChainTracker) apply(path []*node) {
	head := path[len(path)-1].h
	lo := uint64(0)
	if head.number >= uint64(t.window) {
		lo = head.number - uint64(t.window)
	}
	i := 0
	for i < len(path)-1 && path[i].h.number < lo {
		i++
	}
	path = path[i:]
	pub := t.state.Load()
	if len(pub.pins) == len(path) && pub.pins[0] == path[0].h && pub.head() == head {
		return
	}
	s := newFeedState(path, t.window)
	t.pins = path
	if t.selector == "" && pub.head() != head {
		t.noteLatest(head.number)
	}
	old := t.state.Swap(s)
	close(old.changed)

	t.recordMove(pub.pins, path)
	t.evict()
}

// noteLatest floors the network's latest block at n; a panic there never
// splits the pins from the published state.
func (t *ChainTracker) noteLatest(n uint64) {
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues("chain-tracker-note",
				fmt.Sprintf("project:%s network:%s", t.projectId(), t.networkLabel()), common.ErrorFingerprint(rec)).Inc()
		}
	}()
	if noter, ok := t.network.(interface {
		NoteObservedLatestBlock(context.Context, int64)
	}); ok {
		noter.NoteObservedLatestBlock(context.Background(), int64(n))
	}
	telemetry.GaugeHandle(telemetry.MetricNetworkSubscriptionLastHeadTimestamp, t.projectId(), t.networkLabel()).SetToCurrentTime()
}

// recordMove counts a re-anchor, or the depth of a reorg, from old to path.
func (t *ChainTracker) recordMove(old []*chainHeader, path []*node) {
	if len(old) == 0 {
		return
	}
	base := path[0].h.number
	onPath := func(h *chainHeader) bool {
		return h.number >= base && h.number-base < uint64(len(path)) && path[h.number-base].h == h
	}
	fork := -1
	for i := len(old) - 1; i >= 0; i-- {
		if onPath(old[i]) {
			fork = i
			break
		}
	}
	if fork < 0 {
		telemetry.CounterHandle(telemetry.MetricChainTrackerReanchorTotal, t.projectId(), t.networkLabel()).Inc()
	} else if reverted := len(old) - 1 - fork; reverted > 0 {
		telemetry.MetricIntegrityReorgDepth.WithLabelValues(t.projectId(), t.networkLabel(), t.group).
			Observe(float64(reverted))
	}
}

// evict drops every held header more than reorgWindow blocks below the
// head, or below the highest held header while nothing is pinned.
func (t *ChainTracker) evict() {
	ref := t.maxHeld
	if len(t.pins) > 0 {
		ref = t.pins[len(t.pins)-1].h.number
	}
	if ref < uint64(t.window) {
		return
	}
	lo := ref - uint64(t.window)
	for hash, n := range t.nodes {
		if n.h.number >= lo {
			continue
		}
		delete(t.nodes, hash)
		t.held.Delete(n.h.hashHex)
		for _, c := range n.children {
			c.parent = nil
			t.waiting[c.h.parentHash] = append(t.waiting[c.h.parentHash], c)
		}
	}
	for parent, ns := range t.waiting {
		kept := ns[:0]
		for _, n := range ns {
			if n.h.number >= lo {
				kept = append(kept, n)
			}
		}
		if len(kept) == 0 {
			delete(t.waiting, parent)
		} else {
			t.waiting[parent] = kept
		}
	}
	for u, v := range t.votes {
		if v.h.number < lo {
			delete(t.votes, u)
		}
	}
	if t.askVote != nil && t.askVote.h.number < lo {
		t.askVote = nil
	}
	t.evictAnchors(int64(lo))
}

func newFeedState(pins []*node, window int) *feedState {
	s := &feedState{pins: make([]*chainHeader, len(pins)), changed: make(chan struct{})}
	for i, n := range pins {
		s.pins[i] = n.h
	}
	if len(s.pins) > 0 {
		s.base = s.pins[0].number
	}
	s.blockTime = meanBlockTime(s.pins, window)
	return s
}

func (t *ChainTracker) publish() {
	old := t.state.Swap(newFeedState(t.pins, t.window))
	close(old.changed)
}

// meanBlockTime is the mean header-time step over the last window pins
// that carry a timestamp; 0 until 2 exist.
func meanBlockTime(pins []*chainHeader, window int) time.Duration {
	if len(pins) > window {
		pins = pins[len(pins)-window:]
	}
	var first, last *chainHeader
	for _, p := range pins {
		if p.timestamp == 0 {
			continue
		}
		if first == nil {
			first = p
		}
		last = p
	}
	if first == nil || last == first || last.timestamp <= first.timestamp {
		return 0
	}
	return time.Duration(float64(last.timestamp-first.timestamp) * float64(time.Second) / float64(last.number-first.number))
}

func (t *ChainTracker) reset() {
	t.nodes = make(map[[32]byte]*node)
	t.waiting = make(map[[32]byte][]*node)
	t.votes = make(map[string]*node)
	t.askVote, t.pins, t.maxHeld = nil, nil, 0
	t.held.Range(func(k, _ any) bool { t.held.Delete(k); return true })
	t.receipts.Range(func(k, _ any) bool { t.receipts.Delete(k); return true })
	t.anchors.Range(func(k, _ any) bool { t.anchors.Delete(k); return true })
	t.receiptsOrder, t.anchorNumbers = nil, nil
	t.publish()
}

func (t *ChainTracker) handleAnchors(as []anchor) {
	for _, a := range as {
		if _, ok := t.anchors.Load(a.number); !ok {
			t.anchorNumbers = append(t.anchorNumbers, a.number)
		}
		t.anchors.Store(a.number, a.hash)
	}
	ref := int64(t.maxHeld)
	for _, n := range t.anchorNumbers {
		ref = max(ref, n)
	}
	t.evictAnchors(ref - int64(t.window))
}

func (t *ChainTracker) evictAnchors(lo int64) {
	kept := t.anchorNumbers[:0]
	for _, n := range t.anchorNumbers {
		if n < lo {
			t.anchors.Delete(n)
			continue
		}
		kept = append(kept, n)
	}
	t.anchorNumbers = kept
}

func (t *ChainTracker) handleReceipts(key string, receipts []integrity.Receipt) {
	if _, seen := t.receipts.Load(key); !seen {
		t.receiptsOrder = append(t.receiptsOrder, key)
	}
	t.receipts.Store(key, receipts)
	for len(t.receiptsOrder) > t.window+cacheSlack {
		t.receipts.Delete(t.receiptsOrder[0])
		t.receiptsOrder = t.receiptsOrder[1:]
	}
}

// --- indexer.ChainFeed ---

// Head is a new tap's start: the head while a source is live, else the
// next pin change. After a wait it returns the new head's parent, so the
// new head is the tap's first block.
func (t *ChainTracker) Head(ctx context.Context) (indexer.Cursor, error) {
	s := t.state.Load()
	if h := s.head(); h != nil && t.sourceLive() {
		return h.cursor(), nil
	}
	for {
		select {
		case <-ctx.Done():
			return indexer.Cursor{}, ctx.Err()
		case <-s.changed:
		}
		s = t.state.Load()
		if h := s.head(); h != nil {
			if h.number == 0 {
				return h.cursor(), nil
			}
			return indexer.Cursor{Number: h.number - 1, Hash: h.parentHash}, nil
		}
	}
}

// sourceLive reports whether any of the network's sources is live; a
// network without intake counts as live.
func (t *ChainTracker) sourceLive() bool {
	if n, ok := t.network.(interface{ EvmAnySourceLive() bool }); ok {
		return n.EvmAnySourceLive()
	}
	return true
}

// Read returns the pins after c, at most reorgWindow of them, waiting for
// the next pin change while c is the head. c must be pinned, or be the
// parent of the lowest pin; any other cursor is out of range.
func (t *ChainTracker) Read(ctx context.Context, c indexer.Cursor, _ indexer.ReadOpts) (indexer.Batch, error) {
	for {
		s := t.state.Load()
		if len(s.pins) > 0 {
			var from uint64
			switch p := s.pinAt(c.Number); {
			case p != nil && p.hash == c.Hash:
				from = c.Number - s.base + 1
			case c.Number+1 == s.base && s.pins[0].parentHash == c.Hash:
				from = 0
			default:
				return indexer.Batch{}, indexer.ErrOutOfRange
			}
			if from < uint64(len(s.pins)) {
				end := min(uint64(len(s.pins)), from+uint64(t.window))
				b := indexer.Batch{Applied: make([]indexer.Block, 0, end-from)}
				for _, p := range s.pins[from:end] {
					b.Applied = append(b.Applied, p.block())
				}
				b.Next = s.pins[end-1].cursor()
				return b, nil
			}
		}
		select {
		case <-ctx.Done():
			return indexer.Batch{}, ctx.Err()
		case <-s.changed:
		}
	}
}

// Want registers o until release.
func (t *ChainTracker) Want(o indexer.ReadOpts) (release func()) {
	t.wantMu.Lock()
	t.wantID++
	id := t.wantID
	t.wants[id] = o
	t.wantMu.Unlock()
	return func() {
		t.wantMu.Lock()
		delete(t.wants, id)
		t.wantMu.Unlock()
	}
}

// Tip returns the head now, without waiting.
func (t *ChainTracker) Tip() (indexer.Cursor, bool) {
	if h := t.state.Load().head(); h != nil {
		return h.cursor(), true
	}
	return indexer.Cursor{}, false
}

// ReorgWindow is the tracker's window in blocks.
func (t *ChainTracker) ReorgWindow() int { return t.window }

// BlockTime is the mean header-time step over the pins; until 2 exist, the
// network's estimate, else 12s.
func (t *ChainTracker) BlockTime() time.Duration {
	if bt := t.state.Load().blockTime; bt > 0 {
		return bt
	}
	if n, ok := t.network.(interface{ EvmBlockTime() time.Duration }); ok {
		if bt := n.EvmBlockTime(); bt > 0 {
			return bt
		}
	}
	return defaultBlockTime
}

// LiveBound is max(3 × block time, 2s), at most 60s.
func (t *ChainTracker) LiveBound() time.Duration {
	return min(max(3*t.BlockTime(), liveBoundFloor), liveBoundCeiling)
}

// --- integrity.History, ChainSegment, ReceiptCache, PinReconfirmer ---

// HashAt is the pin at a number, or a finalized anchor below the pins.
func (t *ChainTracker) HashAt(number int64) (string, bool) {
	if number < 0 {
		return "", false
	}
	if p := t.state.Load().pinAt(uint64(number)); p != nil {
		return p.hashHex, true
	}
	if h, ok := t.anchors.Load(number); ok {
		return h.(string), true
	}
	return "", false
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

// CachedReceipts is a held-data read of a block's receipts by hash.
func (t *ChainTracker) CachedReceipts(blockHash string) ([]integrity.Receipt, bool) {
	key, ok := hashKey(blockHash)
	if !ok {
		return nil, false
	}
	r, ok := t.receipts.Load(key)
	if !ok {
		return nil, false
	}
	return r.([]integrity.Receipt), true
}

// ReconfirmPin reports the pin at number once it is current. While a source
// that counts voted within the live bound, the pin is current as it is.
// Otherwise the block is fetched fresh and becomes the no-source vote
// (§6 rule 6), and the pin it leaves is returned. PinFresh only ever carries
// a pin that equals the fresh answer.
//
// Within reconfirmCooldown, or while the fetch's key is in backoff, it
// returns the current pin as PinRateLimited, which carries no new evidence,
// so the engine degrades the verdict instead of rejecting on a pin it could
// not re-check.
func (t *ChainTracker) ReconfirmPin(ctx context.Context, number int64) (string, integrity.PinConfirmation) {
	if number < 0 {
		return "", integrity.PinUnverifiable
	}
	pin, pinned := t.HashAt(number)
	if pinned && time.Since(time.Unix(0, t.lastCountedVote.Load())) < t.LiveBound() {
		return pin, integrity.PinFresh
	}
	t.reconfirmMu.Lock()
	at, recent := t.reconfirmedAt[number]
	t.reconfirmMu.Unlock()
	if recent && pinned && time.Since(at) < reconfirmCooldown {
		return pin, integrity.PinRateLimited
	}
	h, err := t.fetchHeader(ctx, classHead, "eth_getBlockByNumber", fmt.Sprintf("0x%x", number), auxKindHeader, fetchReconfirm)
	if errors.Is(err, errFetchBackoff) {
		return pin, integrity.PinRateLimited
	}
	if err != nil {
		return "", integrity.PinUnverifiable
	}
	t.voteAsk(ctx, h)
	t.reconfirmMu.Lock()
	t.reconfirmedAt[number] = time.Now()
	for n := range t.reconfirmedAt {
		if n < number-int64(t.window) {
			delete(t.reconfirmedAt, n)
		}
	}
	t.reconfirmMu.Unlock()
	if pin, pinned = t.HashAt(number); pinned && pin == h.hashHex {
		return pin, integrity.PinFresh
	}
	// A counted source vote keeps another branch pinned: no fresh verdict.
	return pin, integrity.PinRateLimited
}

// headerByHash returns the held header for a hash, fetching it on a miss.
func (t *ChainTracker) headerByHash(ctx context.Context, hash string) (*integrity.Header, bool) {
	key, ok := hashKey(hash)
	if !ok {
		return nil, false
	}
	if h, ok := t.held.Load(key); ok {
		return h.(*chainHeader).parsed, true
	}
	h, err := t.fetchHeader(ctx, classHead, "eth_getBlockByHash", key, auxKindHeader, fetchClient)
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
	h, err := t.fetchHeader(ctx, t.fetchClassAt(number, classHead), "eth_getBlockByNumber", blockRef, auxKindHeader, fetchClient)
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
	if number < 0 || t.finalized == nil {
		return "unknown"
	}
	fin := t.finalized()
	if fin <= 0 {
		return "unknown"
	}
	if number <= fin {
		return "finalized"
	}
	return "unfinalized"
}

// auxKind labels why a header fetch happened: the follower fetches every
// block, which would swamp what the checks themselves cost.
const (
	auxKindHeader = "canonical_header"
	auxKindFollow = "chain_follow"
)

// resolveTag fetches the header a block tag names, such as "finalized".
func (t *ChainTracker) resolveTag(ctx context.Context, tag string) (*integrity.Header, bool) {
	h, err := t.fetchHeader(ctx, classHead, "eth_getBlockByNumber", tag, auxKindHeader, fetchClient)
	if err != nil {
		return nil, false
	}
	return h.parsed, true
}

// headerFetch says who asks for a header and so how its fetch behaves.
type headerFetch uint8

const (
	// fetchTracker: the tracker's own work; waits out the key's backoff.
	fetchTracker headerFetch = iota
	// fetchClient: a check on a client request path; never waits out the
	// key's backoff.
	fetchClient
	// fetchReconfirm: as fetchClient, and never from the cache, even at a
	// finalized height: the cached entry may be what seeded the pin.
	fetchReconfirm
)

// fetchHeader fetches a header through the scheduler, group-scoped; the
// scheduler hands the answer to the tracker without a vote.
func (t *ChainTracker) fetchHeader(ctx context.Context, class fetchClass, method, blockRef, kind string, by headerFetch) (*chainHeader, error) {
	if t.network == nil {
		return nil, errNoTrackerSweep
	}
	res := t.fetch.fetch(ctx, t, fetchSpec{
		class:    class,
		kind:     "header",
		selector: t.selector,
		method:   method,
		params:   `["` + blockRef + `",false]`,
		byHash:   method == "eth_getBlockByHash",
		final:    by != fetchReconfirm && method == "eth_getBlockByNumber" && t.atOrBelowFinalized(blockRef),
		client:   by != fetchTracker,
		decode: func(result []byte) (any, error) {
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
	})
	if errors.Is(res.err, errFetchBackoff) {
		return nil, res.err
	}
	h, _ := res.val.(*chainHeader)
	num := int64(-1)
	if h != nil {
		num = int64(h.number)
	}
	t.emitAux(kind, method, t.finalityLabel(num), h != nil)
	if h == nil {
		if res.err == nil {
			return nil, errFetchUndecoded
		}
		return nil, res.err
	}
	return h, nil
}

// atOrBelowFinalized reports whether a hex block number is at or below the
// network's finalized height, where the block can no longer change.
func (t *ChainTracker) atOrBelowFinalized(blockRef string) bool {
	if t.finalized == nil {
		return false
	}
	n, err := common.HexToInt64(blockRef)
	if err != nil {
		return false
	}
	fin := t.finalized()
	return fin > 0 && n <= fin
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

// fetchedReceipts is a receipts answer the tracker holds under its block
// hash.
type fetchedReceipts struct {
	key      string
	receipts []integrity.Receipt
}

// fetched hands a tracker fetch's answer to the mailbox, where it is never
// shed.
func (t *ChainTracker) fetched(res fetchResult) {
	switch v := res.val.(type) {
	case *chainHeader:
		t.observeFetched(res.upstream, v)
	case fetchedReceipts:
		t.mb.put(trackerMsg{kind: msgReceipts, key: v.key, receipts: v.receipts})
	}
}

// resolveReceipts fetches a block's receipts by hash through the
// scheduler, group-scoped; the scheduler hands them to the tracker. Only a
// block known to lie within reorgWindow of the head is tip receipts; any
// other is fill.
func (t *ChainTracker) resolveReceipts(ctx context.Context, blockHash string, number int64) ([]integrity.Receipt, bool) {
	if t.network == nil {
		return nil, false
	}
	key, ok := hashKey(blockHash)
	if !ok {
		return nil, false
	}
	if h, ok := t.held.Load(key); ok {
		number = int64(h.(*chainHeader).number)
	}
	class := classFill
	if number >= 0 {
		class = t.fetchClassAt(number, classTipReceipts)
	}
	res := t.fetch.fetch(ctx, t, fetchSpec{
		class:    class,
		kind:     "receipts",
		selector: t.selector,
		method:   "eth_getBlockReceipts",
		params:   `["` + key + `"]`,
		byHash:   true,
		client:   true,
		decode: func(result []byte) (any, error) {
			var receipts []integrity.Receipt
			if err := common.SonicCfg.Unmarshal(result, &receipts); err != nil {
				return nil, err
			}
			// A node may answer a by-hash receipts request with another
			// block's receipts (a losing fork at that height): "canonical
			// unavailable", never evidence. An empty answer held under the
			// hash would blind corroboration for that block for good.
			if len(receipts) == 0 || !receiptsMatchBlock(receipts, key) {
				return nil, errFetchUndecoded
			}
			return fetchedReceipts{key: key, receipts: receipts}, nil
		},
	})
	if errors.Is(res.err, errFetchBackoff) {
		return nil, false
	}
	v, ok := res.val.(fetchedReceipts)
	num := int64(-1)
	if ok {
		num, _ = common.HexToInt64(v.receipts[0].BlockNumber)
	}
	t.emitAux("canonical_receipts", "eth_getBlockReceipts", t.finalityLabel(num), ok)
	return v.receipts, ok
}

// receiptsMatchBlock reports whether every receipt claims the expected block
// hash. Receipts without a blockHash can't prove membership.
func receiptsMatchBlock(receipts []integrity.Receipt, blockHash string) bool {
	for i := range receipts {
		if !strings.EqualFold(receipts[i].BlockHash, blockHash) {
			return false
		}
	}
	return true
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
// (receipts/tx), judged against the serving upstream's finalized height.
func observeNarrowView(ctx context.Context, t *ChainTracker, u common.Upstream, rs *common.NormalizedResponse) {
	if t == nil || rs == nil {
		return
	}
	eu, ok := u.(common.EvmUpstream)
	if !ok {
		return
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return
	}
	t.observeNarrowAnchors(eu.EvmEffectiveFinalizedBlock(), jrr.GetResultBytes())
}

// observeNarrowAnchors records number→hash anchors from a narrow response,
// only for finalized blocks: a single narrow response must not redefine a
// block at a jittery tip, but a finalized answer is settled.
func (t *ChainTracker) observeNarrowAnchors(fin int64, result []byte) {
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
			as = append(as, anchor{number: n, hash: key})
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
// request's block proves it has no transactions and used no gas. It never
// fetches.
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
	header, ok := t.HeaderAt(number)
	if !ok || header.RawTransactions == nil || len(header.RawTransactions) != 0 {
		return false
	}
	gas, err := common.HexToInt64(header.GasUsed)
	return err == nil && gas == 0
}

// --- registry: one tracker per (network, group) ---

type networkTrackers struct {
	ctx    context.Context
	window int
	fetch  *fetchScheduler
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
	nt := &networkTrackers{ctx: ctx, window: ct.ReorgWindow, fetch: newFetchScheduler(ctx, n), groups: make(map[string]*ChainTracker)}
	if v, loaded := chainTrackers.LoadOrStore(n, nt); loaded {
		nt = v.(*networkTrackers)
	} else {
		context.AfterFunc(ctx, func() { chainTrackers.CompareAndDelete(n, nt) })
	}
	return nt.get(n, "", "", "")
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
	t := newChainTracker(nt.ctx, n, nt.window, selector, lane, networkFinalized(n), nt.fetch)
	nt.groups[groupKey] = t
	if cfg := n.Config(); cfg != nil && cfg.Integrity != nil && cfg.Integrity.Follow.IsEnabled() {
		newChainFollower(t, networkLatest(n), cfg.Integrity.Follow).start(nt.ctx)
		// The state prober rides on the follower: its pinned headers are
		// the anchor for judging an upstream's state claims.
		if cfg.Integrity.StateProbe.IsEnabled() {
			startStateProber(n, t, cfg.Integrity.StateProbe)
		}
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
// Whether that vote counts is decided when the trackers count votes.
func ObserveSourceHeader(n common.Network, u common.Upstream, raw json.RawMessage) {
	nt := registeredTrackers(n)
	if nt == nil {
		return
	}
	nt.mu.Lock()
	targets := make([]*ChainTracker, 0, len(nt.groups))
	for _, t := range nt.groups {
		if t.selector == "" {
			targets = append(targets, t)
			continue
		}
		if m, _ := common.UpstreamMatchesSelector(t.selector, u); m {
			targets = append(targets, t)
		}
	}
	nt.mu.Unlock()
	for _, t := range targets {
		t.Observe(u.Id(), raw, true)
	}
}

// LivenessChanged makes every tracker of n count its votes again.
func LivenessChanged(n common.Network) {
	v, ok := chainTrackers.Load(n)
	if !ok {
		return
	}
	nt := v.(*networkTrackers)
	nt.mu.Lock()
	defer nt.mu.Unlock()
	for _, t := range nt.groups {
		t.LivenessChanged()
	}
}

// networkLatest returns the network's head-height getter, or nil.
func networkLatest(n common.Network) func(context.Context) int64 {
	if fn, ok := n.(interface {
		EvmHighestLatestBlockNumber(context.Context) int64
	}); ok {
		return fn.EvmHighestLatestBlockNumber
	}
	return nil
}

// networkFinalized returns a best-effort finalized-height getter, or nil.
func networkFinalized(n common.Network) func() int64 {
	if fn, ok := n.(interface {
		EvmHighestFinalizedBlockNumber(context.Context) int64
	}); ok {
		return func() int64 { return fn.EvmHighestFinalizedBlockNumber(context.Background()) }
	}
	return nil
}

var (
	_ indexer.ChainFeed        = (*ChainTracker)(nil)
	_ integrity.History        = (*ChainTracker)(nil)
	_ integrity.ChainSegment   = (*ChainTracker)(nil)
	_ integrity.ReceiptCache   = (*ChainTracker)(nil)
	_ integrity.PinReconfirmer = (*ChainTracker)(nil)
)
