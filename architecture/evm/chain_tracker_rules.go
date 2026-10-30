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
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Spec §6 bounds; variables so tests run fast.
var (
	settleAsks     = 3           // targets a settle round Asks
	catchUpMax     = time.Second // longest gap between catch-ups while latest caps the sends
	followTapPause = time.Second // before a follow tap that failed restarts
)

// meetsEvidence is the evidence quorum (spec §6.10): content answered by
// answering distinct vendors is corroborated on a network whose upstreams
// name networkVendors vendors when they are 2, or 1 on a one-vendor
// network. A network whose vendors are unknown (0) needs 2.
func meetsEvidence(answering, networkVendors int) bool {
	return answering >= 2 || answering == 1 && networkVendors == 1
}

// VendorKey is the vendor the evidence quorum counts u under: its
// VendorName, or, unnamed, its own id, so unnamed nodes never pool into
// one vendor that a single bad node could speak for.
func VendorKey(u common.Upstream) string {
	if v := u.VendorName(); v != "" {
		return v
	}
	return u.Id()
}

// Vendors is the network's vendor table, which the evidence quorum reads
// (spec §6.10): the VendorKey of each upstream on the network, and how many
// distinct vendors they name. An upstream off the network has no vendor,
// so its answer counts for none. Intake publishes it; it is never
// modified once published.
type Vendors struct {
	Of       map[string]string
	Distinct int
}

// noVendors is the table before intake published one: no vendor known.
var noVendors = &Vendors{}

// vendorsOf is how many distinct vendors (VendorKey) ups name.
func vendorsOf(ups []common.Upstream) int {
	var seen []string
	for _, u := range ups {
		if v := VendorKey(u); !slices.Contains(seen, v) {
			seen = append(seen, v)
		}
	}
	return len(seen)
}

// extension walks or fills one held root downward (§6 rules 1 and 2, §8):
// by number, each answer checked against the parentHash of the block above,
// and a mismatch fetched by hash. A walk takes class 1 and at most
// reorgWindow blocks; fill takes class 3 and runs until the root joins
// another tree, reaches the window base, or names a parent one full by-hash
// sweep finds on no upstream. Below reorgWindow blocks under the head, fill
// waits for demand (fillsBelow) without ending.
type extension struct {
	sender string // the voter a walk asks by hash; "" sweeps
	walk   int    // blocks left to walk; 0 fills
	walked bool   // a walk ran from this tree already
	byHash bool   // the next height's by-number answer did not link
	busy   bool
	atBase bool // the window below the head ends here, for good
	// windowEnd: atBase because the window ended here; a final hold that
	// grows the window down reopens it (holdFinal).
	windowEnd bool
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

// readServed reads whether history serves the tracker (spec §6.5 rule 1):
// a tap is open on it, or its network has had a client request
// (ClientRequested) and runs an integrity check that reads its history,
// as main started its chain view at the first checked request. A tracker
// that history starts serving extends its roots from the head.
func (t *ChainTracker) readServed() {
	served := t.taps.Load() > 0
	if !served && t.network != nil {
		if nt := registeredTrackers(t.network); nt != nil && nt.requested.Load() {
			served = nt.headerChecksOf(t.network).readsHistory
		}
	}
	if served != t.served {
		t.served, t.treesDirty = served, true
	}
}

// extends reports whether the tracker walks and fills: it can fetch, and
// history serves it.
func (t *ChainTracker) extends() bool {
	return t.served && t.canFetch()
}

// replaceOlder is rule 1 for a tracker that no history serves: voter u's
// counted vote nd off the pins' tree is a root at once, which no walk or
// fill will join to another tree, and it replaces the older trees: every
// tree but nd's that holds no counted vote of another voter, pending vote
// or awaited answer. The pins of a replaced tree go with it; choose moves
// them. Trees another voter's vote holds stay, and rule 4 weighs them.
func (t *ChainTracker) replaceOlder(nd *node, u string) {
	keep := map[*node]bool{rootOf(nd): true}
	hold := func(n *node) {
		if t.alive(n) {
			keep[rootOf(n)] = true
		}
	}
	for _, v := range t.baseVotes() {
		if v.who != u {
			hold(v.n)
		}
	}
	for o, n := range t.pending {
		if o != u {
			hold(n)
		}
	}
	hold(t.ask.pending)
	if s := t.settle; s != nil {
		for _, a := range s.answers {
			hold(a)
		}
	}
	var old []*node
	for r := range t.exts {
		if !keep[r] {
			old = append(old, r)
		}
	}
	pinRoot := t.pinRoot()
	for _, r := range old {
		if r == pinRoot {
			t.pins = nil
		}
		var tree []*node
		t.walkTree(r, func(n *node) { tree = append(tree, n) })
		for _, n := range tree {
			if t.alive(n) {
				t.drop(n, true)
			}
		}
	}
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
// first vote, a vote more than maxBackfill after the head, and every vote
// of a tracker that does not extend become roots at once.
func (t *ChainTracker) walkFrom(nd *node, sender string) {
	if !t.extends() || len(t.pins) == 0 || t.farAhead(nd.h) {
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
// in it apply at once, each walk no pending vote, ask answer or settle
// answer waits on any more, and every walk once history no longer serves
// the tracker; fill goes on from where it stopped.
func (t *ChainTracker) endWalks() {
	pinRoot := t.pinRoot()
	var awaited map[*node]bool
	for r, e := range t.exts {
		if e.walk == 0 {
			continue
		}
		if t.served && r != pinRoot {
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
		e.atBase, e.windowEnd = true, true
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

// extendRoots starts the next step of every root's walk or fill, and
// drops the answers fetched ahead that no fill will reach.
func (t *ChainTracker) extendRoots() {
	if !t.extends() {
		return
	}
	pinRoot, reads := t.pinRoot(), t.fillRoots()
	for r, e := range t.exts {
		if uncapped := r == pinRoot || reads[r]; !e.busy && !e.atBase && (!e.capped || uncapped) {
			t.extend(r, e, uncapped)
		}
	}
	t.pruneAhead()
}

// fillAhead is how many heights below a filling root fill fetches by
// number at once, below reorgWindow under the head, in the pins' tree or
// a tree a Read waits on fill in (CT spec §9): fill links one header per
// answer, so one fetch at a time made a deep final hold wait thousands
// of round trips. Near the head, and in a capped tree, which may never
// join, fill still fetches one at a time. A var so tests can change it.
var fillAhead = uint64(32)

// aheadAnswer is a by-number answer fetched ahead of fill.
type aheadAnswer struct {
	h   *chainHeader
	err error
}

// fetchAhead keeps fill's by-number fetches of the fillAhead heights below
// root r in flight, down to where fill would stop. Their answers wait in
// ahead until fill reaches them, each checked then against the
// parentHash of the block above, as a single fetch's answer is. At the
// window's time base it may fetch up to fillAhead headers past it, once.
func (t *ChainTracker) fetchAhead(r *node) {
	gen := t.aheadGen
	for k := uint64(1); k <= fillAhead && k <= r.h.number; k++ {
		n := r.h.number - k
		if _, ok := t.ahead[n]; ok || t.aheadBusy[n] {
			continue
		}
		// No header below r is newer than r: r's time bounds the window.
		if !t.keeps(nil, &chainHeader{number: n, timestamp: r.h.timestamp}, true) || !t.fillsBelow(n) {
			return
		}
		t.aheadBusy[n] = true
		t.submitHeader(t.headerSpec(classFill, "eth_getBlockByNumber", fmt.Sprintf("0x%x", n), "", false), func(h *chainHeader, err error) {
			if t.aheadGen != gen {
				return
			}
			delete(t.aheadBusy, n)
			t.ahead[n] = aheadAnswer{h: h, err: err}
		})
	}
}

// pruneAhead drops the answers fetched ahead below no filling root.
func (t *ChainTracker) pruneAhead() {
	for n := range t.ahead {
		reached := false
		for r, e := range t.exts {
			if !e.atBase && e.walk == 0 && !e.byHash && n < r.h.number && n+fillAhead >= r.h.number {
				reached = true
				break
			}
		}
		if !reached {
			delete(t.ahead, n)
		}
	}
}

// fillsBelow reports whether fill fetches height n: within reorgWindow
// blocks below the window's reference, or deeper while a Resumable tap is
// open or a Read waits on fill, or down to the final hold's base. Only
// consumers that resume from their own cursor, or apply final blocks,
// read below reorgWindow, so the rest of the window is fetched for them
// alone.
func (t *ChainTracker) fillsBelow(n uint64) bool {
	ref := t.windowRef()
	return ref == nil || n+uint64(t.window) >= ref.number || t.deepFill.Load() > 0 || t.finalHolds(n)
}

// windowRef is the header the window hangs from: the head, or the highest
// held header while nothing is pinned.
func (t *ChainTracker) windowRef() *chainHeader {
	if h := t.head(); h != nil {
		return h
	}
	return t.maxHeld
}

// extend runs the next step of root r's walk or fill. Fill by number
// links every answer fetched ahead that has arrived, root after root;
// fill runs after the pins move in an update, so a link here asks for
// one more update, which moves the pins onto it.
func (t *ChainTracker) extend(r *node, e *extension, uncapped bool) {
	linked := false
	defer func() {
		if linked {
			t.mb.put(trackerMsg{kind: msgTick})
		}
	}()
	for r != nil {
		var used bool
		r, used = t.extendOnce(r, e, uncapped)
		linked = linked || used
		if r != nil {
			if e = t.exts[r]; e == nil || e.busy || e.atBase {
				return
			}
		}
	}
}

// extendOnce is one step of root r's walk or fill; it returns the root
// that goes on at once, as the parent an answer fetched ahead linked, and
// whether such an answer linked.
func (t *ChainTracker) extendOnce(r *node, e *extension, uncapped bool) (*node, bool) {
	// Block 0, or the chain's first block, which names the all-zero
	// parentHash: the root is final (spec §6.5 rule 2).
	if r.h.number == 0 || r.h.parentHash == ([32]byte{}) {
		e.atBase, e.walk, t.treesDirty = true, 0, true
		return nil, false
	}
	// The parent is no newer than r: when even r's time puts its height
	// outside the window, the parent is outside too.
	if t.outside(e, r, &chainHeader{number: r.h.number - 1, timestamp: r.h.timestamp}, uncapped) {
		return nil, false
	}
	class := classFill
	if e.walk > 0 {
		class = classHead
	} else if !t.fillsBelow(r.h.number - 1) {
		return nil, false
	}
	if ref := t.windowRef(); class == classFill && !e.byHash && uncapped && ref != nil && r.h.number+uint64(t.window) <= ref.number {
		t.fetchAhead(r)
		a, ok := t.ahead[r.h.number-1]
		if !ok {
			return nil, false
		}
		delete(t.ahead, r.h.number-1)
		links := a.err == nil && a.h.hash == r.h.parentHash
		if a.err == nil && !links {
			// The number path moved under the answers fetched ahead.
			for n := range t.ahead {
				if n < r.h.number {
					delete(t.ahead, n)
				}
			}
		}
		return t.answered(r, e, a.h, a.err), links
	}
	method, ref, upstream := "eth_getBlockByNumber", fmt.Sprintf("0x%x", r.h.number-1), ""
	if e.byHash {
		method, ref, upstream = "eth_getBlockByHash", fmt.Sprintf("0x%x", r.h.parentHash), e.sender
	}
	e.busy = true
	t.submitHeader(t.headerSpec(class, method, ref, upstream, false), func(h *chainHeader, err error) {
		if t.exts[r] != e {
			return
		}
		e.busy = false
		t.answered(r, e, h, err)
	})
	return nil, false
}

// answered applies the answer of a fetch of root r's parent; it returns
// the parent once held as a root that goes on.
func (t *ChainTracker) answered(r *node, e *extension, h *chainHeader, err error) *node {
	want := r.h.parentHash
	switch {
	case err == nil && h.hash == want && h.number+1 == r.h.number:
		return t.extended(r, e, h)
	case err == nil && h.hash == want:
		// The parent's hash under another number: no answer joins this
		// root, so its extension ends and it stays a root.
		e.atBase, e.walk, t.treesDirty = true, 0, true
	case !e.byHash && againstNumber(err):
		e.byHash = true
	case e.byHash && e.walk > 0:
		// The sender does not serve its own parent: the walk ends here,
		// its lowest header stays a root, and fill sweeps for the rest.
		e.walk, e.sender = 0, ""
	case e.byHash && errors.Is(err, errFetchNotFound):
		// Every upstream of a full sweep answered null for the parent:
		// the root is final (spec §6.5 rule 2), never asked again.
		e.atBase, t.treesDirty = true, true
	}
	return nil
}

// againstNumber reports whether a by-number fetch's outcome is evidence
// against the number path, so the extension asks by hash: an answer whose
// hash is not the child's parentHash, no block, an answer that does not
// decode, or one an integrity check rejected. A capacity refusal, timeout
// or transport failure is none: the extension stays by number, and the
// key's backoff spaces its retries.
func againstNumber(err error) bool {
	if err == nil || errors.Is(err, errFetchNotFound) || errors.Is(err, errFetchUndecoded) ||
		common.HasErrorCode(err, common.ErrCodeEndpointContentValidation) {
		return true
	}
	var ex *common.ErrUpstreamsExhausted
	if errors.As(err, &ex) {
		for _, e := range ex.Errors() {
			if common.HasErrorCode(e, common.ErrCodeEndpointContentValidation) {
				return true
			}
		}
	}
	return false
}

// extended holds a root's fetched parent; the root joins under it, and the
// walk or fill moves to the parent while the parent is itself a root,
// which it returns.
func (t *ChainTracker) extended(r *node, e *extension, h *chainHeader) *node {
	if t.outside(e, r, h, r == t.pinRoot() || t.fillRoots()[r]) {
		return nil
	}
	p := t.nodes[h.hash]
	if p == nil {
		p = t.hold(h)
	}
	if p.parent != nil {
		return nil
	}
	ne := t.exts[p]
	if ne == nil {
		return nil
	}
	ne.sender, ne.walked = e.sender, e.walked
	ne.walk = max(0, e.walk-1)
	return p
}

// submitHeader queues a tracker-owned header fetch; the tracker waits on it
// and done runs on the tracker's goroutine with the answer.
func (t *ChainTracker) submitHeader(spec fetchSpec, done func(h *chainHeader, err error)) {
	w := t.fetch.submit(nil, spec)
	t.fetch.bg.Go(func() {
		res := w.wait(t.ctx)
		h, _ := res.val.(*chainHeader)
		err := res.err
		if err == nil && h == nil {
			err = errFetchUndecoded
		}
		t.mb.put(trackerMsg{kind: msgCall, fn: func() { done(h, err) }})
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

var settleOutcomeNames = [...]string{"unanswered", "tied", "deferred", "moved", "decided"}

// settleReason is why a round started (spec §17).
type settleReason uint8

const (
	settleTie     settleReason = iota // a tie in rule 4
	settleDispute                     // a no-source answer off the pins (rule 6)
	settleFork                        // the refresh of a vote off the pins (§6.3)
	settleRecheck                     // a client's ReconfirmPin
)

var settleReasonNames = [...]string{"tie", "dispute", "fork", "recheck"}

// logSettle logs the end of round s (spec §17) under the key it started
// with: a decided round re-keys on the block it pinned, which names no
// branch and so hid what the round weighed.
func (t *ChainTracker) logSettle(s *settleState, o settleOutcome) {
	ev := t.transition(zerolog.InfoLevel, "settle").Str("reason", settleReasonNames[s.reason]).
		Str("outcome", settleOutcomeNames[o]).Int("answers", len(s.answers))
	if p := s.started.pinned; p != nil {
		ev = ev.Uint64("pinned", p.h.number).Str("pinnedHash", p.h.hashHex)
	}
	if len(s.started.branch) > 0 {
		b := s.started.branch[0].h
		ev = ev.Uint64("branch", b.number).Str("branchHash", b.hashHex)
	}
	ev.Msg("chain tracker settle ended")
}

// settleCount counts the end of round s in erpc_chain_tracker_settle_total.
func (t *ChainTracker) settleCount(s *settleState, outcome string) {
	telemetry.CounterHandle(telemetry.MetricChainTrackerSettleTotal, t.projectId(), t.networkLabel(), settleReasonNames[s.reason], outcome).Inc()
}

// settleState is one settle round (§6 rule 5, Settle rounds): an Ask with a
// hash need of each of up to settleAsks targets for one key, for at most
// W. Each answer, once its walk ended, is its voter's hash vote, or, from
// a target without a counted vote, one more vote for this decision.
type settleState struct {
	key      roundKey
	started  roundKey // key when the round started
	reason   settleReason
	answers  map[string]*node // target id -> its answer
	out      int              // Asks still out
	inflight bool
	decided  bool
	// ends is W after the round started: walks still out then are no
	// answer.
	ends time.Time
	// asked counts the targets Asked; deferred those whose Ask was refused
	// or backed off.
	asked, deferred int
	// next is the soonest moment one of its targets' Asks may fetch again.
	next time.Time
	// head is the head the round decided: its answers back no pin above.
	head    *node
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
	EvmAskHead(ctx context.Context, upstreamId string, need common.HeadNeed, pace time.Duration, reason common.AskReason) common.HeadAnswer
}

var errHeadAskPanicked = errors.New("head ask panicked")

// askHead Asks upstream id's head at pace off the tracker's goroutine;
// done runs on it with the answer. A panic in the Ask is a failed answer.
// false: the network has no Ask.
func (t *ChainTracker) askHead(id string, need common.HeadNeed, pace time.Duration, reason common.AskReason, done func(common.HeadAnswer)) bool {
	n, ok := t.network.(headAsker)
	if !ok || t.fetch == nil {
		return false
	}
	t.fetch.bg.Go(func() {
		a := common.HeadAnswer{Err: errHeadAskPanicked}
		t.callNetwork("head-ask", func() { a = n.EvmAskHead(t.ctx, id, need, pace, reason) })
		t.mb.put(trackerMsg{kind: msgCall, fn: func() { done(a) }})
	})
	return true
}

// heldAnswer holds an Ask's header; nil for a failed Ask.
func (t *ChainTracker) heldAnswer(a common.HeadAnswer) *node {
	sh, ok := a.Header.(SourceHeader)
	if a.Err != nil || !ok {
		return nil
	}
	return t.heldOrHold(sh.h)
}

// startSettle starts round s: an Ask of each target with need, reason
// settle. With every Ask refused, s ends at once and false is returned.
// With no target no round starts, nothing is logged and no retry is set:
// the next vote runs rule 4 again; a waiter hears unanswered.
func (t *ChainTracker) startSettle(s *settleState, targets []string, need common.HeadNeed, waiter chan settleOutcome) bool {
	if len(targets) == 0 {
		if waiter != nil {
			report(waiter, settleUnanswered)
		}
		return false
	}
	s.started, s.answers, s.inflight, s.ends = s.key, make(map[string]*node), true, time.Now().Add(t.W())
	if waiter != nil {
		s.waiters = append(s.waiters, waiter)
	}
	t.settle = s
	for _, id := range targets {
		s.asked++
		if t.askHead(id, need, 0, common.AskSettle, func(a common.HeadAnswer) { t.settleAnswered(s, id, a) }) {
			s.out++
		} else {
			s.deferred++
		}
	}
	if s.out > 0 {
		return true
	}
	t.endSettle(s)
	return false
}

// startDispute starts a round over k, a tie or a conflicting no-source
// answer, when one may start; skip is the voter whose answer k disputes.
func (t *ChainTracker) startDispute(k roundKey, skip string, reason settleReason) bool {
	return t.maySettle(k) && t.startSettle(&settleState{key: k, reason: reason}, t.tieTargets(k, skip), common.HeadNeedHash, nil)
}

// settleAnswered takes target id's answer to round s. A failed Ask, a
// panic or backoff included, ends that target's part of the round; an
// answer after the round ended is no answer.
func (t *ChainTracker) settleAnswered(s *settleState, id string, a common.HeadAnswer) {
	if t.settle != s || !s.inflight {
		return
	}
	s.out--
	if !a.Next.IsZero() && (s.next.IsZero() || a.Next.Before(s.next)) {
		s.next = a.Next
	}
	if a.Outcome == common.AskBackoff {
		s.deferred++
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

// endSettle decides once every answer is in and walked, or W after the
// round started: rule 4 over the votes and the walked answers. A round
// whose key no longer stands never reports decided: its answers no longer
// count, and the path came from votes alone. A round whose every Ask was
// refused or backed off is deferred. A tie, or no answer, allows the next
// round over its key after the targets' Ask backoff. The pins move before
// the waiters hear the outcome, so they read the pins it left.
func (t *ChainTracker) endSettle(s *settleState) {
	s.inflight = false
	t.markRefreshed(s.key)
	stands := t.stands(s.key)
	path, tie, _ := t.decide()
	if path != nil {
		t.apply(path)
	}
	answered := 0
	for _, a := range s.answers {
		if t.alive(a) && !t.walking(a) {
			answered++
		}
	}
	switch {
	case !stands:
		t.undecided(s, settleMoved)
	case answered == 0 && s.asked > 0 && s.deferred == s.asked:
		t.undecided(s, settleDeferred)
	case answered == 0:
		t.undecided(s, settleUnanswered)
	case len(tie) > 0:
		t.undecided(s, settleTied)
	default:
		// The decision is the key now: the block it pinned is its pinned
		// side, so its answers count while that stands.
		s.decided, s.key = true, t.rekey(s.key)
		if len(t.pins) > 0 {
			s.head = t.pins[len(t.pins)-1]
		}
		delete(t.retryAt, s.key.name)
		t.settleCount(s, "decided")
		t.logSettle(s, settleDecided)
		// The decided answers back their branch now: waiters read that.
		t.gateSend()
		s.notify(settleDecided)
	}
}

// undecided ends s without a decision. The next round over its key waits
// for the soonest of its targets' Ask backoffs; with none, for one block
// time.
func (t *ChainTracker) undecided(s *settleState, o settleOutcome) {
	now := time.Now()
	for k, at := range t.retryAt {
		if !at.After(now) {
			delete(t.retryAt, k)
		}
	}
	at := s.next
	if !at.After(now) {
		at = now.Add(t.BlockTime())
	}
	t.retryAt[s.key.name] = at
	t.settleCount(s, "undecided")
	t.logSettle(s, o)
	s.notify(o)
}

// waits reports whether a round over k waits on its retry time.
func (t *ChainTracker) waits(k roundKey) bool {
	return time.Now().Before(t.retryAt[k.name])
}

// keepsPlace reports whether the last round still holds its place:
// undecided and before its retry, or decided while its key stands and the
// votes alone meet its decision. A client's re-check of another key never
// displaces it.
func (t *ChainTracker) keepsPlace(s *settleState) bool {
	if s.decided {
		return t.stands(s.key) && t.meets(s.key, t.baseVotes())
	}
	return t.waits(s.key)
}

// maySettle: one round at a time. A round over k waits for the round in
// flight and for its own retry time; after a decided round over k it does
// not run while that key stands.
func (t *ChainTracker) maySettle(k roundKey) bool {
	switch s := t.settle; {
	case s != nil && s.inflight:
		return false
	case t.waits(k):
		return false
	case s != nil && s.decided && s.key.name == k.name:
		return !t.stands(s.key)
	default:
		return true
	}
}

// requestSettle is ReconfirmPin's settle: it joins the round in flight and
// waits for its answers, or runs one over the dispute rule 4 meets, else
// over the pin at number. A decided round over that key that still stands
// reports decided at once. A round over that key before its retry, or a
// last round over another key that still holds its place, defers it.
func (t *ChainTracker) requestSettle(settled chan settleOutcome, number int64) {
	s := t.settle
	if s != nil && s.inflight {
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
	switch {
	case s != nil && s.decided && s.key.name == k.name && t.stands(s.key):
		report(settled, settleDecided)
	case t.waits(k), s != nil && s.key.name != k.name && t.keepsPlace(s):
		report(settled, settleDeferred)
	default:
		t.startSettle(&settleState{key: k, reason: settleRecheck}, t.tieTargets(k, skip), common.HeadNeedHash, settled)
	}
}

// tieTargets are the targets of a round over k, a tie, a conflicting
// no-source answer or a client's re-check (spec §6.8): the voters without a
// counted vote that decides k, then the non-voters, in selection order, up
// to settleAsks; never skip, the voter whose answer is disputed. A vote
// below k's blocks, or in another tree, has no say in k: only its answer
// can break the tie.
func (t *ChainTracker) tieTargets(k roundKey, skip string) []string {
	var out []string
	for _, id := range t.voters.Voters {
		if v := t.votes[id]; id != skip && !(t.counts(id) && t.alive(v) && t.decides(k, v)) && len(out) < settleAsks {
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

// decides reports whether vote n takes part in round k: it is, or descends
// from, one of k's blocks. A key that names no block takes every vote.
func (t *ChainTracker) decides(k roundKey, n *node) bool {
	if k.pinned == nil && len(k.branch) == 0 {
		return true
	}
	in := func(b *node) bool { return b != nil && (b == n || isAncestor(b, n)) }
	return in(k.pinned) || slices.ContainsFunc(k.branch, in)
}

// refreshTargets are §6.3's refresh for voter off's vote nd off the pins:
// the voters, other than off, that are on no conflicting branch and whose
// vote is older than nd's, oldest evidence first (§6.8), up to settleAsks.
// A voter without a counted vote has the oldest vote: left out, a refresh
// heard only the backers of the pins and moved them on a majority of the
// voters that happened to be fresh (§19 C14).
func (t *ChainTracker) refreshTargets(off string) []string {
	at := t.voteAt[off]
	vote := func(u string) (time.Time, bool) {
		if v := t.votes[u]; t.counts(u) && t.alive(v) {
			return t.voteAt[u], !t.conflicts(v)
		}
		return time.Time{}, true
	}
	var out []string
	for _, u := range t.voters.Voters {
		if e, behind := vote(u); u != off && behind && e.Before(at) {
			out = append(out, u)
		}
	}
	if len(out) > settleAsks {
		t.oldestEvidenceFirst(out, func(u string) time.Time {
			e, _ := vote(u)
			return e
		})
	}
	return out[:min(len(out), settleAsks)]
}

// forkPoint names where nd's branch leaves the pins: the pinned parent of
// its first block off them; "" for a tree that does not join the pins.
func (t *ChainTracker) forkPoint(nd *node) string {
	if p := t.branchOf(nd).parent; p != nil {
		return p.h.hashHex
	}
	return ""
}

// refreshed reports whether voter u's vote nd off the pins was refreshed
// (spec §6.3) at its fork point: a round over its dispute ended there, a
// decided round over it stands, or no vote behind the pins is older.
// Otherwise it starts that round when one may start, Asking each target
// with a fork need: a fetch sent before the round, as old as the vote it
// would refresh, is no answer.
func (t *ChainTracker) refreshed(u string, nd *node) bool {
	fork := t.forkPoint(nd)
	if at, ok := t.refreshedAt[u]; ok && at == fork {
		return true
	}
	k := t.disputeOf(nd)
	if s := t.settle; s != nil && !s.inflight && s.decided && s.key.name == k.name && t.stands(s.key) {
		t.refreshedAt[u] = fork
		return true
	}
	targets := t.refreshTargets(u)
	if len(targets) == 0 {
		t.refreshedAt[u] = fork
		return true
	}
	if t.maySettle(k) {
		t.startSettle(&settleState{key: k, reason: settleFork}, targets, common.HeadNeedFork, nil)
	}
	return false
}

// markRefreshed records a round over k as the refresh of every vote off
// the pins that k disputes: a tie round over the same branch Asks the
// voters without a fresh vote too, and one round per dispute is the
// refresh (spec §6.5 rule 5). Waiting for a round of its own let the tie
// rounds over that branch starve it, so the off voter was never polled
// or demoted.
func (t *ChainTracker) markRefreshed(k roundKey) {
	if !k.disputed() {
		return
	}
	for u, v := range t.votes {
		if t.counts(u) && t.alive(v) && t.conflicts(v) && t.disputeOf(v).name == k.name {
			t.refreshedAt[u] = t.forkPoint(v)
		}
	}
}

// askState is §6 rule 6: with demand and no source with a fresh hash vote,
// the tracker Asks the first voter in selection order with a hash need,
// at the demand's pace (the Ask keeps it at least the cadence). That
// answer is the only vote until the next.
type askState struct {
	vote, pending       *node
	voter, pendingVoter string
	out                 bool // an Ask is out
	// due is the last answer's Next: the Ask at this pace fetches nothing
	// sooner, so the tracker wakes for it then rather than Ask again.
	due time.Time
}

// noSourceWanted reports whether rule 6 Asks: demand, a voter, no Ask out,
// and no source among the hash voters. A voter that is no source votes
// from each poll of it, rule 6's Asks and full polls included, yet no
// schedule keeps those coming.
func (t *ChainTracker) noSourceWanted() bool {
	if t.ask.out || len(t.voters.Voters) == 0 {
		return false
	}
	if _, ok := t.demand(); !ok {
		return false
	}
	return !slices.ContainsFunc(t.voters.Sources, t.counts)
}

// askNoSource is rule 6's Ask, reason no_source, once it is due.
func (t *ChainTracker) askNoSource() {
	if !t.noSourceWanted() || time.Now().Before(t.ask.due) {
		return
	}
	pace, _ := t.demand()
	id, seq := t.voters.Voters[0], t.askSeq
	t.ask.out = t.askHead(id, common.HeadNeedHash, pace, common.AskNoSource, func(a common.HeadAnswer) {
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

// demand reports whether newHeads demand stands for this tracker, and the
// fastest pace it needs heads at (0 every block): an open tap of the
// network for the network-wide tracker; a tap open on a group tracker, at
// the integrity follow interval, since only its follow tap reads a group.
func (t *ChainTracker) demand() (pace time.Duration, ok bool) {
	if t.selector != "" {
		return t.followInterval(), t.taps.Load() > 0
	}
	n, isNet := t.network.(interface {
		EvmHeadsDemand() (time.Duration, bool)
	})
	if !isNet {
		return 0, false
	}
	return n.EvmHeadsDemand()
}

// followInterval is the integrity follow tap's pace (spec §3 demand).
func (t *ChainTracker) followInterval() time.Duration {
	if t.network == nil {
		return common.DefaultIntegrityFollowInterval
	}
	if cfg := t.network.Config(); cfg != nil && cfg.Integrity != nil {
		return cfg.Integrity.Follow.IntervalOrDefault()
	}
	return common.DefaultIntegrityFollowInterval
}

// conflicts reports whether a vote is off the pins at a height they hold
// (spec §6.3): neither pinned nor above the head on its branch, and its
// first block off the pins is at a pinned height. A tree that does not
// link, above the head or below the pins' first block, is no evidence of
// a fork: a tracker that does not walk holds every poll's vote that way.
func (t *ChainTracker) conflicts(nd *node) bool {
	if len(t.pins) == 0 || t.pinned(nd) || isAncestor(t.pins[len(t.pins)-1], nd) {
		return false
	}
	return t.pinNode(t.branchOf(nd).h.number) != nil
}

// rearm ticks the tracker at its next deadline: the no-source voter's next
// Ask, the end of a settle round's W or of an undecided round's backoff,
// the next catch-up while the lane's Latest caps the sends, the end of the
// cold window, the moment the soonest hash voter's hash age passes W, or
// the moment a vote off the pins has been off for W (minority fork). It
// runs after every batch, so a deadline that fires never hides a later
// one.
func (t *ChainTracker) rearm() {
	now := time.Now()
	var at time.Time
	if t.noSourceWanted() {
		if due := t.ask.due; due.After(now) {
			at = due
		}
	}
	if s := t.settle; s != nil {
		due := t.retryAt[s.key.name]
		if s.inflight {
			due = s.ends
		}
		if due.After(now) && (at.IsZero() || due.Before(at)) {
			at = due
		}
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

// follow runs the integrity follow tap (spec §13) from the network's first
// client request (ClientRequested): it holds newHeads demand at
// integrity.follow.interval's pace, so polls run while clients are quiet,
// hands each new head to the state prober and sets the head's lag behind
// the network's served tip. Out of range restarts it after the head.
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
	for ctx.Err() == nil {
		err := taps.RunChain(ctx, key, "integrity", "follow", t, nil, indexer.ReadOpts{Pace: t.followInterval()}, func(b indexer.Batch) (int, error) {
			if len(b.Applied) == 0 {
				return 0, nil
			}
			if p := stateProberFor(t.network); p != nil {
				p.onNewHead(int64(b.Next.Number))
			}
			if t.laneHead != nil {
				if served := t.laneHead().Latest; served > 0 {
					telemetry.MetricChainTrackerLag.WithLabelValues(t.projectId(), t.networkLabel(), t.group).
						Set(float64(max(0, served-int64(b.Next.Number))))
				}
			}
			return len(b.Applied), nil
		})
		if ctx.Err() != nil {
			continue
		}
		if errors.Is(err, indexer.ErrOutOfRange) {
			t.transition(zerolog.InfoLevel, "out_of_range").Err(err).
				Msg("integrity follow tap out of range; restarting after the head")
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
