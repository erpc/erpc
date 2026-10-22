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
// newest one.
const heightsKept = 64

type checkState uint8

const (
	checkEnforced checkState = iota
	checkOff
	checkObserve
)

// heightVerdict is what the headers at one height did against one check.
type heightVerdict struct {
	failedAt time.Time // the first failing header; zero while none failed
	passed   bool
}

type checkHealth struct {
	state   checkState
	heights map[uint64]*heightVerdict
	offAt   uint64 // the head when the check was suspended
}

// checkSuspensions is one network's unanimous-failure state per header
// check (spec §12): a check no header passes at 2 heights in a row, within
// the live bound and from any upstream, is wrong for this chain, so it is
// suspended. After CheckSuspendBlocks it runs observe-only, and it
// enforces again once it passes at 2 heights in a row. Every validation of
// a header records its outcomes here and reads each check's mode here.
type checkSuspensions struct {
	network common.Network
	tracker func() *ChainTracker // the network-wide tracker: head, live bound

	mu        sync.Mutex
	checks    map[string]*checkHealth
	onSuspend []func()
}

func newCheckSuspensions(n common.Network, tracker func() *ChainTracker) *checkSuspensions {
	return &checkSuspensions{network: n, tracker: tracker, checks: make(map[string]*checkHealth)}
}

// mode is how the network runs check id now.
func (s *checkSuspensions) mode(id string) integrity.CheckMode {
	s.mu.Lock()
	h := s.checks[id]
	if h == nil {
		s.mu.Unlock()
		return integrity.CheckEnforced
	}
	suspended := s.stepLocked(id, h)
	st := h.state
	s.mu.Unlock()
	if suspended {
		s.suspended()
	}
	switch st {
	case checkOff:
		return integrity.CheckOff
	case checkObserve:
		return integrity.CheckObserve
	}
	return integrity.CheckEnforced
}

// record notes that a header at height passed check id or failed it.
func (s *checkSuspensions) record(id string, height uint64, passed bool) {
	s.mu.Lock()
	h := s.checks[id]
	if h == nil {
		h = &checkHealth{heights: make(map[uint64]*heightVerdict)}
		s.checks[id] = h
	}
	if h.state != checkOff {
		v := h.heights[height]
		if v == nil {
			v = &heightVerdict{}
			h.heights[height] = v
		}
		if passed {
			v.passed = true
		} else if v.failedAt.IsZero() {
			v.failedAt = time.Now()
		}
		if h.state == checkObserve && passed && height > 0 {
			if p := h.heights[height-1]; p != nil && p.passed {
				h.state, h.heights = checkEnforced, make(map[uint64]*heightVerdict)
				log.Warn().Str("project", s.network.ProjectId()).Str("network", s.network.Label()).Str("check", id).
					Uint64("height", height).Msg("integrity: check passes again at 2 heights in a row; enforcing it")
			}
		}
		for k := range h.heights {
			if k+heightsKept < height {
				delete(h.heights, k)
			}
		}
	}
	suspended := s.stepLocked(id, h)
	s.mu.Unlock()
	if suspended {
		s.suspended()
	}
}

// stepLocked moves h on with time and reports whether it suspended the
// check now: an enforced check whose headers failed, with none passing,
// at 2 judged heights without a passing height between them, each for a
// live bound, is suspended; a suspended one runs observe-only once the
// head has moved CheckSuspendBlocks past where it was suspended.
func (s *checkSuspensions) stepLocked(id string, h *checkHealth) bool {
	head, headKnown := s.head()
	switch h.state {
	case checkOff:
		if headKnown && head >= h.offAt+CheckSuspendBlocks.Load() {
			h.state, h.heights = checkObserve, make(map[uint64]*heightVerdict)
			log.Warn().Str("project", s.network.ProjectId()).Str("network", s.network.Label()).Str("check", id).
				Uint64("head", head).Msg("integrity: suspended check runs observe-only")
		}
		return false
	case checkObserve:
		return false
	}
	bound, now := s.bound(), time.Now()
	keys := make([]uint64, 0, len(h.heights))
	for k := range h.heights {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	failed := 0
	for _, k := range keys {
		v := h.heights[k]
		switch {
		case v.passed:
			failed = 0
		case !v.failedAt.IsZero() && now.Sub(v.failedAt) >= bound:
			failed++
		}
		if failed < 2 {
			continue
		}
		h.state, h.heights, h.offAt = checkOff, make(map[uint64]*heightVerdict), max(head, k)
		telemetry.MetricIntegrityCheckSuspendedTotal.WithLabelValues(s.network.ProjectId(), s.network.Label(), id).Inc()
		log.Error().Str("project", s.network.ProjectId()).Str("network", s.network.Label()).Str("check", id).
			Uint64("height", k).Msg("integrity: no upstream's header passes this check at 2 heights in a row; suspending it for the network")
		return true
	}
	return false
}

func (s *checkSuspensions) head() (uint64, bool) {
	if t := s.tracker(); t != nil {
		if c, ok := t.Tip(); ok {
			return c.Number, true
		}
	}
	return 0, false
}

func (s *checkSuspensions) bound() time.Duration {
	if t := s.tracker(); t != nil {
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

// recordOutcomes notes each header check's verdict on a header at height.
func (s *checkSuspensions) recordOutcomes(height uint64, outcomes []integrity.CheckOutcome) {
	for _, oc := range outcomes {
		if !slices.Contains(integrity.HeaderChecks, oc.CheckID) {
			continue
		}
		switch oc.Outcome {
		case "pass", "reconfirmed":
			s.record(oc.CheckID, height, true)
		case "reject", "would_reject", "reject_recoverable", "record_only":
			s.record(oc.CheckID, height, false)
		}
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
