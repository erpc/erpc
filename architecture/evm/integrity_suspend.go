package evm

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog/log"
)

// CheckSuspendBlocks is how many blocks a suspended header check stays off
// before it runs observe-only (spec §12).
var CheckSuspendBlocks atomic.Uint64

func init() { CheckSuspendBlocks.Store(100) }

// heightsKept bounds the judged heights each check remembers below the
// newest one while the tracker has no head.
const heightsKept = 64

// failAhead is how far above the tracker head a header still counts for a
// check's suspension: headers further ahead, which one vendor can forge at
// will, are no evidence about the chain.
const failAhead = 2

type checkState uint8

const (
	checkEnforced checkState = iota
	checkOff
	checkObserve
)

// heightVerdict is what the headers at one height did against one check.
type heightVerdict struct {
	failVendor  string    // the first vendor whose header failed
	failVendors int       // distinct vendors whose headers failed, at most 2
	failedAt    time.Time // when enough vendors had failed; zero until then
	passed      bool
}

// failing: enough vendors' headers failed the check here, and none passed.
func (v *heightVerdict) failing() bool { return !v.passed && !v.failedAt.IsZero() }

type checkHealth struct {
	state   checkState
	heights map[uint64]*heightVerdict
	// pair: the failing heights pairLo < pairHi are judged heights in a
	// row (no height between them passed), both failing since pairAt; the
	// run a suspension waits a live bound on.
	pair   bool
	pairLo uint64
	pairHi uint64
	pairAt time.Time
	offAt  uint64 // the tracker head when the check was suspended
	// offKnown is false while the tracker had no head at suspension; the
	// first head known sets offAt.
	offKnown bool
}

// checkSuspensions is one network's unanimous-failure state per header
// check (spec §12): a check no header passes at 2 heights in a row, within
// the live bound and from any upstream, is wrong for this chain, so it is
// suspended. After CheckSuspendBlocks it runs observe-only, and it
// enforces again once it passes at 2 heights in a row. With 2 or more
// vendors on the network, a height fails only when headers of 2 vendors
// failed there, as the bloom rule (§10): one vendor alone never suspends a
// check. Every validation of a header records its outcomes here; the modes
// are a snapshot every validation reads without a lock.
type checkSuspensions struct {
	network common.Network
	// tracker is the network-wide tracker: head, live bound, window and
	// upstreams. Set once it starts.
	tracker atomic.Pointer[ChainTracker]
	// modes holds the checks not enforced now; nil while every check is.
	modes atomic.Pointer[map[string]integrity.CheckMode]
	// modeFn is mode, bound once, for integrity.Input.Modes.
	modeFn func(string) integrity.CheckMode

	mu     sync.Mutex
	checks map[string]*checkHealth
	// tops is the highest height each vendor's judged headers reached.
	tops      map[string]uint64
	onSuspend []func()
}

func newCheckSuspensions(n common.Network) *checkSuspensions {
	s := &checkSuspensions{network: n, checks: make(map[string]*checkHealth), tops: make(map[string]uint64)}
	s.modeFn = s.mode
	return s
}

// mode is how the network runs check id now.
func (s *checkSuspensions) mode(id string) integrity.CheckMode {
	if m := s.modes.Load(); m != nil {
		return (*m)[id]
	}
	return integrity.CheckEnforced
}

// vendorKey names u's vendor; an upstream without one is its own vendor.
func vendorKey(u common.Upstream) string {
	if v := u.VendorName(); v != "" {
		return v
	}
	return u.Id()
}

// failVendorsNeeded is how many vendors' headers must fail a check at a
// height: 2 on a network of 2 or more vendors, else 1, so a one-vendor
// network's upstream alone decides.
func (s *checkSuspensions) failVendorsNeeded() int {
	t := s.tracker.Load()
	if t == nil {
		return 1
	}
	first := ""
	for _, u := range t.receiptsUpstreams() {
		switch v := vendorKey(u); {
		case first == "":
			first = v
		case v != first:
			return 2
		}
	}
	return 1
}

// outcomeVerdict maps a header check's outcome to passed or failed; judged
// is false for outcomes that say nothing about the header.
func outcomeVerdict(outcome string) (passed, judged bool) {
	switch outcome {
	case "pass", "reconfirmed":
		return true, true
	case "reject", "would_reject", "reject_recoverable", "record_only":
		return false, true
	}
	return false, false
}

// recordOutcomes notes each header check's verdict on vendor's header at
// height, then moves every check on with time. A verdict counts only at
// or below the reference plus failAhead: the higher of the tracker head
// and the height the headers of 2 vendors reached (the one vendor's on a
// one-vendor network), so a check every vendor fails still meets the
// heights above a head that stopped, while one vendor's headers far ahead
// count for nothing.
func (s *checkSuspensions) recordOutcomes(height uint64, vendor string, outcomes []integrity.CheckOutcome) {
	need := 0
	for _, oc := range outcomes {
		if passed, judged := outcomeVerdict(oc.Outcome); judged && !passed {
			need = s.failVendorsNeeded()
			break
		}
	}
	head, headKnown := s.head()
	now := time.Now()
	changed, suspended := false, false
	s.mu.Lock()
	if height > s.tops[vendor] {
		s.tops[vendor] = height
	}
	ref, refKnown := s.refLocked(head, headKnown)
	if !refKnown || height <= ref+failAhead {
		for _, oc := range outcomes {
			passed, judged := outcomeVerdict(oc.Outcome)
			if !judged || !slices.Contains(integrity.HeaderChecks, oc.CheckID) {
				continue
			}
			h := s.checks[oc.CheckID]
			if h == nil {
				h = &checkHealth{heights: make(map[uint64]*heightVerdict)}
				s.checks[oc.CheckID] = h
			}
			if !refKnown {
				ref = height
			}
			changed = s.recordLocked(oc.CheckID, h, height, vendor, passed, need, ref, refKnown, now) || changed
		}
	}
	for id, h := range s.checks {
		c, sus := s.stepLocked(id, h, head, headKnown, now)
		changed, suspended = changed || c, suspended || sus
	}
	if changed {
		s.publishModesLocked()
	}
	s.mu.Unlock()
	if suspended {
		s.suspended()
	}
}

// refLocked is the height verdicts are judged against: the higher of the
// tracker head and the second-highest height any vendor's header reached,
// or the highest while only one vendor's headers were judged.
func (s *checkSuspensions) refLocked(head uint64, headKnown bool) (uint64, bool) {
	var first, second uint64
	for _, top := range s.tops {
		switch {
		case top > first:
			first, second = top, first
		case top > second:
			second = top
		}
	}
	quorum := second
	if len(s.tops) == 1 {
		quorum = first
	}
	if !headKnown && quorum == 0 {
		return 0, false
	}
	return max(head, quorum), true
}

// recordLocked notes one verdict and reports whether the check's mode
// changed: an observed check passing at 2 judged heights in a row
// enforces again.
func (s *checkSuspensions) recordLocked(id string, h *checkHealth, height uint64, vendor string, passed bool, need int, ref uint64, refKnown bool, now time.Time) bool {
	if h.state == checkOff {
		return false
	}
	v := h.heights[height]
	if v == nil {
		v = &heightVerdict{}
		h.heights[height] = v
		s.pruneLocked(h, ref, refKnown)
	}
	if passed {
		if v.passed {
			return false
		}
		v.passed = true
		if h.pair && height >= h.pairLo && height <= h.pairHi {
			repairLocked(h)
		}
		if h.state != checkObserve || !passedNextTo(h, height) {
			return false
		}
		h.state, h.heights, h.pair = checkEnforced, make(map[uint64]*heightVerdict), false
		log.Warn().Str("project", s.network.ProjectId()).Str("network", s.network.Label()).Str("check", id).
			Uint64("height", height).Msg("integrity: check passes again at 2 heights in a row; enforcing it")
		return true
	}
	if v.passed || !v.failedAt.IsZero() {
		return false
	}
	switch {
	case v.failVendors == 0:
		v.failVendor, v.failVendors = vendor, 1
	case v.failVendors == 1 && vendor != v.failVendor:
		v.failVendors = 2
	}
	if v.failVendors < need {
		return false
	}
	v.failedAt = now
	if !h.pair {
		repairLocked(h)
	}
	return false
}

// passedNextTo reports whether the judged height next to height, below or
// above, passed: 2 judged heights in a row pass.
func passedNextTo(h *checkHealth, height uint64) bool {
	var below, above *heightVerdict
	var belowAt, aboveAt uint64
	for k, v := range h.heights {
		if !v.passed && v.failedAt.IsZero() {
			continue // not judged yet: no vendor's verdict counts here
		}
		switch {
		case k < height && (below == nil || k > belowAt):
			below, belowAt = v, k
		case k > height && (above == nil || k < aboveAt):
			above, aboveAt = v, k
		}
	}
	return below != nil && below.passed || above != nil && above.passed
}

// pruneLocked drops the heights that left the window below ref, and, once
// ref is known, those more than failAhead above it.
func (s *checkSuspensions) pruneLocked(h *checkHealth, ref uint64, refKnown bool) {
	window := uint64(heightsKept)
	if t := s.tracker.Load(); t != nil && refKnown {
		window = uint64(t.ReorgWindow())
	}
	repair := false
	for k := range h.heights {
		if k+window < ref || refKnown && k > ref+failAhead {
			delete(h.heights, k)
			repair = repair || h.pair && (k == h.pairLo || k == h.pairHi)
		}
	}
	if repair {
		repairLocked(h)
	}
}

// repairLocked finds, among failing heights, the 2 judged in a row that
// have failed longest. It runs only when a height starts failing, or a
// height of the pair passed or left: never per passing header.
func repairLocked(h *checkHealth) {
	h.pair = false
	for lo, a := range h.heights {
		if !a.failing() {
			continue
		}
		// next: the lowest height above lo that passed or fails.
		var next *heightVerdict
		var hi uint64
		for k, v := range h.heights {
			if k > lo && (v.passed || v.failing()) && (next == nil || k < hi) {
				next, hi = v, k
			}
		}
		if next == nil || next.passed {
			continue
		}
		at := later(a.failedAt, next.failedAt)
		if !h.pair || at.Before(h.pairAt) {
			h.pair, h.pairLo, h.pairHi, h.pairAt = true, lo, hi, at
		}
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// stepLocked moves h on with time and reports whether its mode changed and
// whether it was suspended now: an enforced check whose headers failed at
// 2 judged heights in a row, with none passing there, for a live bound, is
// suspended at the tracker head; a suspended one runs observe-only once the
// head has moved CheckSuspendBlocks past that head.
func (s *checkSuspensions) stepLocked(id string, h *checkHealth, head uint64, headKnown bool, now time.Time) (changed, suspended bool) {
	switch h.state {
	case checkOff:
		if !headKnown {
			return false, false
		}
		if !h.offKnown {
			h.offAt, h.offKnown = head, true
		}
		if head >= h.offAt+CheckSuspendBlocks.Load() {
			h.state, h.heights, h.pair = checkObserve, make(map[uint64]*heightVerdict), false
			log.Warn().Str("project", s.network.ProjectId()).Str("network", s.network.Label()).Str("check", id).
				Uint64("head", head).Msg("integrity: suspended check runs observe-only")
			return true, false
		}
		return false, false
	case checkObserve:
		return false, false
	}
	if !h.pair || now.Sub(h.pairAt) < s.bound() {
		return false, false
	}
	lo := h.pairLo
	h.state, h.heights, h.pair = checkOff, make(map[uint64]*heightVerdict), false
	h.offAt, h.offKnown = head, headKnown
	telemetry.MetricIntegrityCheckSuspendedTotal.WithLabelValues(s.network.ProjectId(), s.network.Label(), id).Inc()
	log.Error().Str("project", s.network.ProjectId()).Str("network", s.network.Label()).Str("check", id).
		Uint64("height", lo).Uint64("head", head).Msg("integrity: no upstream's header passes this check at 2 heights in a row; suspending it for the network")
	return true, true
}

// publishModesLocked publishes the mode of every check not enforced.
func (s *checkSuspensions) publishModesLocked() {
	var m map[string]integrity.CheckMode
	for id, h := range s.checks {
		mode := integrity.CheckEnforced
		switch h.state {
		case checkOff:
			mode = integrity.CheckOff
		case checkObserve:
			mode = integrity.CheckObserve
		}
		if mode == integrity.CheckEnforced {
			continue
		}
		if m == nil {
			m = make(map[string]integrity.CheckMode)
		}
		m[id] = mode
	}
	if m == nil {
		s.modes.Store(nil)
		return
	}
	s.modes.Store(&m)
}

func (s *checkSuspensions) head() (uint64, bool) {
	if t := s.tracker.Load(); t != nil {
		if c, ok := t.Tip(); ok {
			return c.Number, true
		}
	}
	return 0, false
}

func (s *checkSuspensions) bound() time.Duration {
	if t := s.tracker.Load(); t != nil {
		return t.LiveBound()
	}
	return liveBoundFloor
}

func (s *checkSuspensions) suspended() {
	s.mu.Lock()
	fns := slices.Clone(s.onSuspend)
	s.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func suspensionsOf(n common.Network) *checkSuspensions {
	if nt := registeredTrackers(n); nt != nil {
		return nt.suspend
	}
	return nil
}

// OnCheckSuspended registers fn to run each time a header check of n is
// suspended, so the headers that check held back are kept. fn must not
// block.
func OnCheckSuspended(n common.Network, fn func()) {
	s := suspensionsOf(n)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.onSuspend = append(s.onSuspend, fn)
	s.mu.Unlock()
}
