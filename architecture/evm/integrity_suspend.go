package evm

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// CheckSuspendBlocks is how many blocks a suspended header check stays off
// before it runs observe-only (spec §6.9).
var CheckSuspendBlocks atomic.Uint64

func init() { CheckSuspendBlocks.Store(100) }

// Bloom check suspension (spec §6.9); variables so tests run fast.
var (
	bloomSuspendRun = 8 // mismatching blocks in a row that suspend the bloom check
	bloomRearmRun   = 8 // matching blocks in a row that re-arm it
)

// bloomCheckID is the §13 id of the receipts bloom check.
const bloomCheckID = "bloomMatch"

// heightsKept bounds the judged heights each check remembers below the
// newest one while nothing is pinned.
const heightsKept = 64

// suspendRule is one check's suspension parameters (spec §6.9).
type suspendRule struct {
	failRun int  // failing judged heights in a row that suspend the check
	live    bool // the run must have failed for W first
	// offFirst: a suspended check is off until the pins moved
	// CheckSuspendBlocks, then observe-only; else observe-only at once.
	offFirst bool
	passRun  int // passing judged heights in a row that enforce an observed check
}

// ruleOf is check id's suspension rule: the bloom check's, or the header
// checks'.
func ruleOf(id string) suspendRule {
	if id == bloomCheckID {
		return suspendRule{failRun: bloomSuspendRun, passRun: bloomRearmRun}
	}
	return suspendRule{failRun: 2, live: true, offFirst: true, passRun: 2}
}

type checkState uint8

const (
	checkEnforced checkState = iota
	checkOff
	checkObserve
)

// heightVerdict is what the answers at one height did against one check.
type heightVerdict struct {
	failVendors []string  // distinct vendors whose answers failed here
	failedAt    time.Time // when they met the evidence quorum; zero until then
	passed      bool
}

// failing: vendors meeting the evidence quorum failed the check here, and
// no answer passed.
func (v *heightVerdict) failing() bool { return !v.passed && !v.failedAt.IsZero() }

func (v *heightVerdict) judged() bool { return v.passed || !v.failedAt.IsZero() }

type checkHealth struct {
	rule    suspendRule
	state   checkState
	heights map[uint64]*heightVerdict
	// run: rule.failRun judged heights in a row are failing, all of them
	// since runAt; the run a suspension waits on.
	run   bool
	runAt time.Time
	offAt uint64 // the pins' head when the check was suspended
	// offKnown is false while nothing was pinned at suspension; the first
	// head pinned sets offAt.
	offKnown bool
}

// checkSuspensions is one network's suspension of integrity checks (spec
// §6.9): one mechanism, each check with its own suspendRule. A check whose
// answers fail at rule.failRun judged heights in a row, with none passing
// there, is wrong for this chain, so it is suspended; it is enforced again
// once it passes at rule.passRun heights in a row. A height fails only when
// the vendors whose answers failed there meet the evidence quorum
// (meetsEvidence). Heights count within reorgWindow of the pins' head, and
// offAt is that head: no upstream's own tip moves either. The modes are a
// snapshot every validation reads without a lock.
type checkSuspensions struct {
	network common.Network
	// tracker is the network-wide tracker: pins, live bound, window and
	// upstreams. Set once it starts.
	tracker atomic.Pointer[ChainTracker]
	// modes holds the checks not enforced now; nil while every check is.
	modes atomic.Pointer[map[string]integrity.CheckMode]
	// modeFn is mode, bound once, for integrity.Input.Modes.
	modeFn func(string) integrity.CheckMode

	mu        sync.Mutex
	checks    map[string]*checkHealth
	onSuspend []func()
}

func newCheckSuspensions(n common.Network) *checkSuspensions {
	s := &checkSuspensions{network: n, checks: make(map[string]*checkHealth)}
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

// networkVendors is how many vendors the network's upstreams name; 0 while
// the tracker has not started.
func (s *checkSuspensions) networkVendors() int {
	if t := s.tracker.Load(); t != nil {
		return t.networkVendors()
	}
	return 0
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

// recordOutcomes notes each header check's verdict on vendor's answer at
// height, and a bloomMatch failure of its receipts: a receipt's bloom that
// matches its own logs says nothing of the header's, so a bloom pass comes
// only from the receipts path (recordBloom).
func (s *checkSuspensions) recordOutcomes(height uint64, vendor string, outcomes []integrity.CheckOutcome) {
	vendors := -1
	for _, oc := range outcomes {
		if passed, judged := outcomeVerdict(oc.Outcome); judged && !passed {
			vendors = s.networkVendors()
			break
		}
	}
	s.record(height, vendor, vendors, func(yield func(id string, passed bool)) {
		for _, oc := range outcomes {
			passed, judged := outcomeVerdict(oc.Outcome)
			if judged && (slices.Contains(integrity.HeaderChecks, oc.CheckID) || oc.CheckID == bloomCheckID && !passed) {
				yield(oc.CheckID, passed)
			}
		}
	})
}

// recordBloom notes the bloom check's verdict on vendor's receipts of the
// block at height; an answer of no vendor ("") counts for nothing.
func (s *checkSuspensions) recordBloom(height uint64, vendor string, matched bool) {
	if vendor == "" {
		return
	}
	vendors := -1
	if !matched {
		vendors = s.networkVendors()
	}
	s.record(height, vendor, vendors, func(yield func(string, bool)) { yield(bloomCheckID, matched) })
}

// record notes the verdicts of vendor's answer at height, then moves every
// check on with time. networkVendors is read only by failing verdicts.
func (s *checkSuspensions) record(height uint64, vendor string, networkVendors int, verdicts func(yield func(id string, passed bool))) {
	head, headKnown := s.head()
	window := s.window()
	now := time.Now()
	var suspendedIds []string
	changed := false
	s.mu.Lock()
	if !headKnown || height+window >= head && height <= head+window {
		verdicts(func(id string, passed bool) {
			h := s.checks[id]
			if h == nil {
				h = &checkHealth{rule: ruleOf(id), heights: make(map[uint64]*heightVerdict)}
				s.checks[id] = h
			}
			changed = s.recordLocked(id, h, height, vendor, passed, networkVendors, head, headKnown, window, now) || changed
		})
	}
	for id, h := range s.checks {
		c, sus := s.stepLocked(id, h, head, headKnown, now)
		changed = changed || c
		if sus {
			suspendedIds = append(suspendedIds, id)
		}
	}
	if changed {
		s.publishModesLocked()
	}
	s.mu.Unlock()
	for _, id := range suspendedIds {
		s.suspended(id)
	}
}

// recordLocked notes one verdict and reports whether the check's mode
// changed: an observed check passing at rule.passRun judged heights in a
// row enforces again.
func (s *checkSuspensions) recordLocked(id string, h *checkHealth, height uint64, vendor string, passed bool, networkVendors int, head uint64, headKnown bool, window uint64, now time.Time) bool {
	if h.state == checkOff {
		return false
	}
	v := h.heights[height]
	if v == nil {
		v = &heightVerdict{}
		h.heights[height] = v
		pruneLocked(h, height, head, headKnown, window)
	}
	if passed {
		if v.passed {
			return false
		}
		v.passed = true
		if h.run {
			runLocked(h)
		}
		if h.state != checkObserve || passRunAt(h, height) < h.rule.passRun {
			return false
		}
		h.state, h.heights, h.run = checkEnforced, make(map[uint64]*heightVerdict), false
		common.TransitionLog(&log.Logger, zerolog.WarnLevel, s.network.Label(), "suspension").Str("project", s.network.ProjectId()).Str("check", id).
			Uint64("height", height).Int("heights", h.rule.passRun).Msg("integrity: check passes again at heights in a row; enforcing it")
		return true
	}
	if v.passed || !v.failedAt.IsZero() {
		return false
	}
	if !slices.Contains(v.failVendors, vendor) {
		v.failVendors = append(v.failVendors, vendor)
	}
	if !meetsEvidence(len(v.failVendors), networkVendors) {
		return false
	}
	v.failedAt = now
	runLocked(h)
	return false
}

// judgedHeights are h's judged heights, ascending.
func judgedHeights(h *checkHealth) []uint64 {
	ks := make([]uint64, 0, len(h.heights))
	for k, v := range h.heights {
		if v.judged() {
			ks = append(ks, k)
		}
	}
	slices.Sort(ks)
	return ks
}

// passRunAt is how many passing judged heights in a row hold height.
func passRunAt(h *checkHealth, height uint64) int {
	ks := judgedHeights(h)
	i, ok := slices.BinarySearch(ks, height)
	if !ok {
		return 0
	}
	lo, hi := i, i
	for lo > 0 && h.heights[ks[lo-1]].passed {
		lo--
	}
	for hi+1 < len(ks) && h.heights[ks[hi+1]].passed {
		hi++
	}
	return hi - lo + 1
}

// runLocked finds, among judged heights, the rule.failRun in a row all
// failing that have failed longest. It runs when a height starts failing,
// or, while a run exists, a height passed or left: never per passing
// answer otherwise.
func runLocked(h *checkHealth) {
	h.run = false
	ks := judgedHeights(h)
	n := max(h.rule.failRun, 1)
	streak := 0
	for i, k := range ks {
		if !h.heights[k].failing() {
			streak = 0
			continue
		}
		if streak++; streak < n {
			continue
		}
		var at time.Time
		for _, kk := range ks[i-n+1 : i+1] {
			at = later(at, h.heights[kk].failedAt)
		}
		if !h.run || at.Before(h.runAt) {
			h.run, h.runAt = true, at
		}
	}
}

// pruneLocked drops the heights more than window from the pins' head, or,
// while nothing is pinned, more than heightsKept below newest.
func pruneLocked(h *checkHealth, newest, head uint64, headKnown bool, window uint64) {
	gone := false
	for k := range h.heights {
		if headKnown && (k+window < head || k > head+window) || !headKnown && k+heightsKept < newest {
			delete(h.heights, k)
			gone = true
		}
	}
	if gone && h.run {
		runLocked(h)
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// stepLocked moves h on with time and reports whether its mode changed and
// whether it was suspended now: an enforced check with a failing run, for
// a live bound when its rule waits one, is suspended at the pins' head:
// off, or observe-only at once; an off check runs observe-only once the
// pins' head has moved CheckSuspendBlocks past that head.
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
			h.state, h.heights, h.run = checkObserve, make(map[uint64]*heightVerdict), false
			common.TransitionLog(&log.Logger, zerolog.WarnLevel, s.network.Label(), "suspension").Str("project", s.network.ProjectId()).Str("check", id).
				Uint64("head", head).Msg("integrity: suspended check runs observe-only")
			return true, false
		}
		return false, false
	case checkObserve:
		return false, false
	}
	if !h.run || h.rule.live && now.Sub(h.runAt) < s.bound() {
		return false, false
	}
	h.state, h.heights, h.run = checkObserve, make(map[uint64]*heightVerdict), false
	if h.rule.offFirst {
		h.state = checkOff
	}
	h.offAt, h.offKnown = head, headKnown
	telemetry.MetricIntegrityCheckSuspendedTotal.WithLabelValues(s.network.ProjectId(), s.network.Label(), id).Inc()
	common.TransitionLog(&log.Logger, zerolog.ErrorLevel, s.network.Label(), "suspension").Str("project", s.network.ProjectId()).Str("check", id).
		Uint64("head", head).Int("heights", h.rule.failRun).Msg("integrity: no answer passes this check at heights in a row; suspending it for the network")
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

// head is the pins' head of the network-wide tracker.
func (s *checkSuspensions) head() (uint64, bool) {
	if t := s.tracker.Load(); t != nil {
		if c, ok := t.Tip(); ok {
			return c.Number, true
		}
	}
	return 0, false
}

func (s *checkSuspensions) window() uint64 {
	if t := s.tracker.Load(); t != nil {
		return uint64(t.ReorgWindow())
	}
	return heightsKept
}

func (s *checkSuspensions) bound() time.Duration {
	if t := s.tracker.Load(); t != nil {
		return t.W()
	}
	return health.FreshnessWindow(0)
}

// suspended runs what follows a suspension of check id: a header check
// keeps the headers it held back; the bloom check has its blocks fetched
// again.
func (s *checkSuspensions) suspended(id string) {
	if id == bloomCheckID {
		if nt := registeredTrackers(s.network); nt != nil {
			for _, t := range nt.trackers() {
				t.mb.put(trackerMsg{kind: msgCall, fn: t.refetchJobs})
			}
		}
		return
	}
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
