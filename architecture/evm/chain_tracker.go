package evm

import (
	"bytes"
	"context"
	"encoding/json"
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

	flightMu  sync.Mutex
	hInflight map[string]*flight[*chainHeader]
	rInflight map[string]*flight[[]integrity.Receipt]

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
	msgHeader msgKind = iota
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

// mailbox never blocks a sender and never drops anything but headers: when
// headers fill it, answers drop first, then each source keeps only its
// newest header.
type mailbox struct {
	mu      sync.Mutex
	queue   []trackerMsg
	headers int
	wake    chan struct{}
}

func (m *mailbox) put(msg trackerMsg) {
	m.mu.Lock()
	if msg.kind == msgHeader {
		if m.headers >= mailboxHeaderCap {
			m.shedLocked()
		}
		if m.headers >= mailboxHeaderCap && msg.vote == voteNone {
			m.mu.Unlock()
			return
		}
		m.headers++
	}
	m.queue = append(m.queue, msg)
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *mailbox) shedLocked() {
	kept := m.queue[:0]
	for _, msg := range m.queue {
		if msg.kind == msgHeader && msg.vote == voteNone {
			m.headers--
			continue
		}
		kept = append(kept, msg)
	}
	m.queue = kept
	if m.headers < mailboxHeaderCap {
		return
	}
	type voter struct {
		upstream string
		vote     voteKind
	}
	newest := make(map[voter]bool)
	for i := len(m.queue) - 1; i >= 0; i-- {
		msg := m.queue[i]
		if msg.kind != msgHeader {
			continue
		}
		k := voter{msg.upstream, msg.vote}
		if newest[k] {
			m.queue[i].kind = msgTick // superseded: a tick only recounts
			m.headers--
			continue
		}
		newest[k] = true
	}
}

func (m *mailbox) take() []trackerMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queue
	m.queue, m.headers = nil, 0
	return q
}

func newChainTracker(ctx context.Context, n common.Network, window int, selector, group string, finalized func() int64) *ChainTracker {
	if window <= 0 {
		window = common.DefaultChainTrackerReorgWindow
	}
	t := &ChainTracker{
		network:       n,
		selector:      selector,
		group:         group,
		window:        window,
		finalized:     finalized,
		mb:            mailbox{wake: make(chan struct{}, 1)},
		wants:         make(map[uint64]indexer.ReadOpts),
		reconfirmedAt: make(map[int64]time.Time),
		hInflight:     make(map[string]*flight[*chainHeader]),
		rInflight:     make(map[string]*flight[[]integrity.Receipt]),
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
	case msgHeader:
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
		if !t.mayHold(h) {
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

// mayHold reports whether h lies inside the window: at most reorgWindow
// blocks below the head and above it. Without pins every header is held.
func (t *ChainTracker) mayHold(h *chainHeader) bool {
	if len(t.pins) == 0 {
		return true
	}
	head := t.pins[len(t.pins)-1].h.number
	return h.number+uint64(t.window) >= head && h.number <= head+uint64(t.window)
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
	var counts func(string) bool
	if n, ok := t.network.(interface{ EvmSourceVotes(string) bool }); ok {
		counts = n.EvmSourceVotes
	}
	out := make([]*node, 0, len(t.votes)+1)
	for u, v := range t.votes {
		if counts == nil || counts(u) {
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

// apply makes path the pins: the network-wide tracker moves the network's
// latest block first, then taps wake.
func (t *ChainTracker) apply(path []*node) {
	old := t.pins
	if len(old) == len(path) && (len(old) == 0 || old[0] == path[0] && old[len(old)-1] == path[len(path)-1]) {
		return
	}
	onPath := func(n *node) bool {
		i := int(n.h.number) - int(path[0].h.number)
		return i >= 0 && i < len(path) && path[i] == n
	}
	if len(old) > 0 {
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
	t.pins = path
	t.evict()
	head := t.pins[len(t.pins)-1].h
	if t.selector == "" && (len(old) == 0 || old[len(old)-1].h != head) {
		if noter, ok := t.network.(interface {
			NoteObservedLatestBlock(context.Context, int64)
		}); ok {
			noter.NoteObservedLatestBlock(context.Background(), int64(head.number))
		}
		telemetry.GaugeHandle(telemetry.MetricNetworkSubscriptionLastHeadTimestamp, t.projectId(), t.networkLabel()).SetToCurrentTime()
	}
	t.publish()
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
	i := 0
	for i < len(t.pins) && t.pins[i].h.number < lo {
		i++
	}
	t.pins = t.pins[i:]
	t.evictAnchors(int64(lo))
}

func (t *ChainTracker) publish() {
	s := &feedState{pins: make([]*chainHeader, len(t.pins)), changed: make(chan struct{})}
	for i, n := range t.pins {
		s.pins[i] = n.h
	}
	if len(s.pins) > 0 {
		s.base = s.pins[0].number
	}
	s.blockTime = meanBlockTime(s.pins, t.window)
	old := t.state.Swap(s)
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

// Head returns the head, or, with nothing pinned yet, waits for the first
// pin and returns its parent, so the first pinned block is the tap's first.
func (t *ChainTracker) Head(ctx context.Context) (indexer.Cursor, error) {
	for {
		s := t.state.Load()
		if h := s.head(); h != nil {
			return h.cursor(), nil
		}
		select {
		case <-ctx.Done():
			return indexer.Cursor{}, ctx.Err()
		case <-s.changed:
		}
		if s = t.state.Load(); len(s.pins) > 0 {
			root := s.pins[0]
			return indexer.Cursor{Number: root.number - 1, Hash: root.parentHash}, nil
		}
	}
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

// ReconfirmPin fetches the block at number fresh and reports its hash. The
// answer is held, not voted: only votes move pins.
//
// Within reconfirmCooldown it returns the current pin as PinRateLimited,
// which carries no new evidence, so the engine degrades the verdict instead
// of rejecting on a pin it could not re-check.
func (t *ChainTracker) ReconfirmPin(ctx context.Context, number int64) (string, integrity.PinConfirmation) {
	if number < 0 {
		return "", integrity.PinUnverifiable
	}
	t.reconfirmMu.Lock()
	at, recent := t.reconfirmedAt[number]
	t.reconfirmMu.Unlock()
	if pin, pinned := t.HashAt(number); recent && pinned && time.Since(at) < reconfirmCooldown {
		return pin, integrity.PinRateLimited
	}
	h, ok := doOnce(&t.flightMu, t.hInflight, fmt.Sprintf("reconfirm:%d", number), func() (*chainHeader, bool) {
		// Fresh: this is the pin-vs-response tie-break, so it must reach
		// upstreams rather than replay the cached entry.
		return t.fetchHeader(ctx, "eth_getBlockByNumber", fmt.Sprintf("0x%x", number), true, auxKindHeader)
	})
	if !ok {
		return "", integrity.PinUnverifiable
	}
	t.reconfirmMu.Lock()
	t.reconfirmedAt[number] = time.Now()
	for n := range t.reconfirmedAt {
		if n < number-int64(t.window) {
			delete(t.reconfirmedAt, n)
		}
	}
	t.reconfirmMu.Unlock()
	return h.hashHex, integrity.PinFresh
}

// flight coalesces concurrent misses for one key into a single fetch.
type flight[T any] struct {
	wg  sync.WaitGroup
	val T
	ok  bool
}

func doOnce[T any](mu *sync.Mutex, inflight map[string]*flight[T], key string, fn func() (T, bool)) (T, bool) {
	mu.Lock()
	if f, ok := inflight[key]; ok {
		mu.Unlock()
		f.wg.Wait()
		return f.val, f.ok
	}
	f := &flight[T]{}
	f.wg.Add(1)
	inflight[key] = f
	mu.Unlock()

	defer func() {
		mu.Lock()
		delete(inflight, key)
		mu.Unlock()
		f.wg.Done()
	}()

	f.val, f.ok = fn()
	return f.val, f.ok
}

// headerByHash returns the held header for a hash, fetching it once on a
// miss.
func (t *ChainTracker) headerByHash(ctx context.Context, hash string) (*integrity.Header, bool) {
	if key, ok := hashKey(hash); ok {
		if h, ok := t.held.Load(key); ok {
			return h.(*chainHeader).parsed, true
		}
	}
	h, ok := doOnce(&t.flightMu, t.hInflight, hash, func() (*chainHeader, bool) {
		return t.fetchHeader(ctx, "eth_getBlockByHash", hash, false, auxKindHeader)
	})
	if !ok {
		return nil, false
	}
	return h.parsed, true
}

// headerByNumber returns the pinned header at a number, fetching the block
// by number once on a miss.
func (t *ChainTracker) headerByNumber(ctx context.Context, number int64, blockRef string) (*integrity.Header, bool) {
	if h, ok := t.HeaderAt(number); ok {
		return h, true
	}
	h, ok := doOnce(&t.flightMu, t.hInflight, fmt.Sprintf("n:%d", number), func() (*chainHeader, bool) {
		return t.fetchHeader(ctx, "eth_getBlockByNumber", blockRef, false, auxKindHeader)
	})
	if !ok {
		return nil, false
	}
	return h.parsed, true
}

// receiptsByHash returns a block's receipts, fetching them once on a miss.
func (t *ChainTracker) receiptsByHash(ctx context.Context, blockHash string) ([]integrity.Receipt, bool) {
	if r, ok := t.CachedReceipts(blockHash); ok {
		return r, true
	}
	return doOnce(&t.flightMu, t.rInflight, blockHash, func() ([]integrity.Receipt, bool) {
		return t.resolveReceipts(ctx, blockHash)
	})
}

// fetchDirectives marks the fetch internal (no recursion into the engine)
// and pins it to the tracker's group. fresh=true skips cache reads, for
// anything that re-checks a disputed pin.
func (t *ChainTracker) fetchDirectives(fresh bool) *common.RequestDirectives {
	d := &common.RequestDirectives{IsInternal: true}
	if t.selector != "" {
		d.UseUpstream = t.selector
	}
	if fresh {
		d.SkipCacheRead = "true"
	}
	return d
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

func (t *ChainTracker) resolveHeader(ctx context.Context, method, blockRef string, fresh bool) (*integrity.Header, bool) {
	h, ok := t.fetchHeader(ctx, method, blockRef, fresh, auxKindHeader)
	if !ok {
		return nil, false
	}
	return h.parsed, true
}

// fetchHeader fetches a header through the network (group-scoped) and hands
// the answer to the tracker, without a vote.
func (t *ChainTracker) fetchHeader(ctx context.Context, method, blockRef string, fresh bool, kind string) (*chainHeader, bool) {
	if t.network == nil {
		return nil, false
	}
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"%s","params":["%s",false]}`, method, blockRef)))
	req.SetDirectives(t.fetchDirectives(fresh))
	req.SetNetwork(t.network)

	resp, err := t.network.Forward(ctx, req)
	var h *chainHeader
	upstreamId := ""
	if err == nil && resp != nil {
		if u := resp.Upstream(); u != nil {
			upstreamId = u.Id()
		}
		if jrr, jerr := resp.JsonRpcResponse(ctx); jerr == nil && jrr != nil {
			if parsed, perr := parseChainHeader(bytes.Clone(jrr.GetResultBytes())); perr == nil {
				h = parsed
			}
		}
	}
	// A by-hash fetch must return that hash, a by-number fetch that number;
	// a node answering with another block would otherwise be held under the
	// wrong key.
	if h != nil && !headerMatchesRef(h, method, blockRef) {
		h = nil
	}
	num := int64(-1)
	if h != nil {
		num = int64(h.number)
	}
	t.emitAux(kind, method, t.finalityLabel(num), h != nil)
	if h == nil {
		return nil, false
	}
	t.observeAnswer(upstreamId, h)
	return h, true
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

// resolveReceipts fetches a block's receipts by hash, group-scoped, and
// hands them to the tracker.
func (t *ChainTracker) resolveReceipts(ctx context.Context, blockHash string) ([]integrity.Receipt, bool) {
	if t.network == nil {
		return nil, false
	}
	key, ok := hashKey(blockHash)
	if !ok {
		return nil, false
	}
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["%s"]}`, blockHash)))
	req.SetDirectives(t.fetchDirectives(false))
	req.SetNetwork(t.network)

	resp, err := t.network.Forward(ctx, req)
	var receipts []integrity.Receipt
	num := int64(-1)
	got := false
	if err == nil && resp != nil {
		if jrr, jerr := resp.JsonRpcResponse(ctx); jerr == nil && jrr != nil {
			if common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &receipts) == nil {
				got = true
				if len(receipts) > 0 {
					num, _ = common.HexToInt64(receipts[0].BlockNumber)
				}
			}
		}
	}
	// A node may answer a by-hash receipts request with another block's
	// receipts (a losing fork at that height); that is "canonical
	// unavailable", never evidence, and is not cached.
	if got && !receiptsMatchBlock(receipts, blockHash) {
		got = false
		receipts = nil
	}
	t.emitAux("canonical_receipts", "eth_getBlockReceipts", t.finalityLabel(num), got)
	if !got || len(receipts) == 0 {
		// An empty answer cached under the hash would blind corroboration
		// for that block for good.
		return nil, false
	}
	t.mb.put(trackerMsg{kind: msgReceipts, key: key, receipts: receipts})
	return receipts, true
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
	mu     sync.Mutex
	groups map[string]*ChainTracker // group key ("" network-wide) -> tracker
}

var chainTrackers sync.Map // common.Network -> *networkTrackers

func trackersOf(n common.Network, ctx context.Context) *networkTrackers {
	if v, ok := chainTrackers.Load(n); ok {
		return v.(*networkTrackers)
	}
	v, _ := chainTrackers.LoadOrStore(n, &networkTrackers{ctx: ctx, groups: make(map[string]*ChainTracker)})
	return v.(*networkTrackers)
}

// StartChainTrackers starts the network-wide tracker of n; it and every
// group tracker of n stop with ctx.
func StartChainTrackers(ctx context.Context, n common.Network) *ChainTracker {
	return trackersOf(n, ctx).get(n, "", "", "")
}

// NetworkChainTracker returns the network-wide tracker of n.
func NetworkChainTracker(n common.Network) *ChainTracker {
	return trackersOf(n, context.Background()).get(n, "", "", "")
}

func lookupChainTracker(n common.Network, groupKey string) *ChainTracker {
	v, ok := chainTrackers.Load(n)
	if !ok {
		return nil
	}
	nt := v.(*networkTrackers)
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
	window := common.DefaultChainTrackerReorgWindow
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil {
		window = cfg.Evm.ChainTracker.EffectiveReorgWindow()
	}
	t := newChainTracker(nt.ctx, n, window, selector, lane, networkFinalized(n))
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
// sub-group yields the network-wide tracker.
func ChainTrackerOf(ctx context.Context, n common.Network, selector string) *ChainTracker {
	if n == nil {
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
	return trackersOf(n, context.Background()).get(n, groupKey, selector, lane)
}

// ObserveSourceHeader hands a source header that passed intake to the
// network-wide tracker and to every group tracker whose selector matches u.
func ObserveSourceHeader(n common.Network, u common.Upstream, raw json.RawMessage, vote bool) {
	nt := trackersOf(n, context.Background())
	nt.mu.Lock()
	targets := make([]*ChainTracker, 0, len(nt.groups))
	for _, t := range nt.groups {
		if t.selector == "" {
			continue
		}
		if m, _ := common.UpstreamMatchesSelector(t.selector, u); m {
			targets = append(targets, t)
		}
	}
	nt.mu.Unlock()
	nt.get(n, "", "", "").Observe(u.Id(), raw, vote)
	for _, t := range targets {
		t.Observe(u.Id(), raw, vote)
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
