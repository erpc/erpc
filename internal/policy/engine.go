package policy

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
)

// Engine is the per-project unified selection engine. It owns one Slot per
// active (network, method) pair plus the sobek runtime pool. Request-path
// reads go through Engine.GetOrdered, which is wait-free.
type Engine struct {
	projectID string
	logger    *zerolog.Logger
	tracker   *health.Tracker
	pool      *runtimePool

	mu    sync.RWMutex
	slots map[slotKey]*Slot

	// networks holds the registered (networkId → []Upstream, cfg) so newly
	// referenced methods can lazily spin up additional slots under
	// `evalPerMethod=true`.
	networks map[string]*networkRegistration

	// paused gates the per-slot ticker. Read every tick via
	// `s.engine.paused.Load()`. When true the ticker still wakes up but
	// returns immediately without running tickOnce, so the cache (and
	// the request path that reads it) stays frozen at the last verdict.
	// Wired up so the simulator's "pause" button stops the policy
	// engine from churning while traffic generation is halted — without
	// this, the policy would happily continue re-evaluating against
	// drifting tracker metrics, and the operator's "what was the last
	// verdict?" snapshot wouldn't be stable while paused.
	//
	// Request-path GetOrdered is intentionally NOT gated by paused: a
	// late-arriving request mid-pause still gets a deterministic order
	// rather than nil. This matches the simulator's contract (Execute
	// itself drops requests when paused, so this is belt-and-braces).
	paused atomic.Bool

	// stepLogEnabled gates the JS-side per-step trail (one entry per
	// chainable stdlib step invoked, plus per-upstream annotations).
	// When false (production default), the JS wrapper around `define`
	// fast-paths and pushes nothing — zero overhead beyond the call
	// indirection. When true, the slot's Decision carries `StepLog` +
	// `Annotations` and the simulator / DEBUG eRPC logs surface them.
	// Toggled via `SetStepLogEnabled`.
	stepLogEnabled atomic.Bool

	appCtx context.Context
	cancel context.CancelFunc
}

type slotKey struct {
	network string
	method  string
}

type networkRegistration struct {
	// upstreamsFn is queried at every tick so newly-bootstrapped upstreams
	// become visible to the engine without a re-register. Static fixtures
	// can use a closure that returns a frozen slice.
	upstreamsFn func() []common.Upstream
	cfg         *common.SelectionPolicyConfig
}

// NewEngine constructs an Engine. `tracker` is read-only from the engine's
// perspective — the engine snapshots metrics from it once per tick. The
// `runtimePrimer` is invoked on each freshly-created sobek runtime; pass
// nil to skip primings (the default policy works against the bare runtime).
func NewEngine(
	parentCtx context.Context,
	logger *zerolog.Logger,
	projectID string,
	tracker *health.Tracker,
	runtimePrimer func(*common.Runtime) error,
) *Engine {
	ctx, cancel := context.WithCancel(parentCtx)
	lg := logger.With().Str("component", "policy-engine").Str("projectId", projectID).Logger()
	return &Engine{
		projectID: projectID,
		logger:    &lg,
		tracker:   tracker,
		pool:      newRuntimePool(runtimePrimer),
		slots:     make(map[slotKey]*Slot),
		networks:  make(map[string]*networkRegistration),
		appCtx:    ctx,
		cancel:    cancel,
	}
}

// RegisterNetwork creates (or replaces) the "*" slot for the given network
// and starts its ticker. If `cfg.EvalPerMethod` is true, additional slots
// are created lazily when GetOrdered is called with a specific method.
//
// The `upstreamsFn` callback is invoked at every tick so newly-bootstrapped
// upstreams become visible to the engine without a re-register. For static
// configurations a closure returning a frozen slice is fine.
//
// If `cfg.EvalFunc` is the placeholder identity policy
// (`common.DefaultSelectionPolicySource`), this method upgrades it to
// the embedded rich default (sortByScore + preferTag + stickyPrimary
// + probeExcluded).
//
// The initial eval runs synchronously so callers can rely on a non-empty
// cache being present once RegisterNetwork returns.
func (e *Engine) RegisterNetwork(networkID string, upstreamsFn func() []common.Upstream, cfg *common.SelectionPolicyConfig) error {
	if err := upgradeDefaultPolicy(cfg); err != nil {
		return err
	}
	if upstreamsFn == nil {
		upstreamsFn = func() []common.Upstream { return nil }
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.networks[networkID] = &networkRegistration{
		upstreamsFn: upstreamsFn,
		cfg:         cfg,
	}

	// Tear down any pre-existing slots for this network so reconfiguration
	// is atomic from the request-path's perspective.
	for k, slot := range e.slots {
		if k.network == networkID {
			slot.stop()
			delete(e.slots, k)
		}
	}

	wildcard := newSlot(e, networkID, "*", upstreamsFn, cfg)
	e.slots[slotKey{networkID, "*"}] = wildcard

	// Initial eval is synchronous so the first request sees a populated cache.
	wildcard.tickOnce()
	wildcard.start(e.appCtx)

	return nil
}

// UnregisterNetwork stops every slot for the given network and clears the
// registration.
func (e *Engine) UnregisterNetwork(networkID string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for k, slot := range e.slots {
		if k.network == networkID {
			slot.stop()
			delete(e.slots, k)
		}
	}
	delete(e.networks, networkID)
}

// GetOrdered returns the cached eligible upstreams for `(networkID, method)`,
// in policy-chosen order. The lookup is wait-free in the common path.
//
// Fallback chain:
//  1. Exact slot for (network, method)
//  2. Wildcard slot for (network, "*")
//
// On either hit, the returned slice is the SAME backing array the slot
// stores atomically — callers MUST treat it as read-only.
func (e *Engine) GetOrdered(networkID, method string) []common.Upstream {
	slot := e.lookupSlot(networkID, method)
	if slot == nil {
		return nil
	}
	if ordered := slot.cache.Load(); ordered != nil {
		return *ordered
	}
	return nil
}

// LastEvalAt returns the wall-clock timestamp of the slot's most recent
// successful tick, or the zero value if the slot is unknown / has never
// produced a decision.
func (e *Engine) LastEvalAt(networkID, method string) time.Time {
	slot := e.lookupSlot(networkID, method)
	if slot == nil {
		return time.Time{}
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.lastEvalAt
}

// LastSwitchAt returns the timestamp of the most recent primary-change
// the slot recorded (used by `stickyPrimary` to enforce the
// `minSwitchInterval` cooldown). Zero value if no switch has happened.
// Diagnostics consume this to render "primary held for Xs / sticky
// locked for Ys".
func (e *Engine) LastSwitchAt(networkID, method string) time.Time {
	slot := e.lookupSlot(networkID, method)
	if slot == nil {
		return time.Time{}
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.lastSwitchAt == nil {
		return time.Time{}
	}
	return *slot.lastSwitchAt
}

// RecentDecisions returns up to `limit` of the most-recent decisions
// the slot has produced, OLDEST-first. `limit <= 0` returns all
// retained entries (currently capped at `decisionsRingSize`). Returns
// nil if the slot is unknown / no ticks yet.
//
// Diagnostic tooling consumes this to render a tick-by-tick replay
// ("why did the order change at T=4.2s?"). For production
// observability, see the Prometheus `policy_selection_*` families and
// per-request traces.
func (e *Engine) RecentDecisions(networkID, method string, limit int) []*Decision {
	slot := e.lookupSlot(networkID, method)
	if slot == nil {
		return nil
	}
	return slot.recentDecisions(limit)
}

// GetScores returns the per-upstream `score` map produced by the
// slot's most recent successful tick. Entries are the EXACT values the
// JS `sortByScore(...)` step assigned to each upstream, so anything
// comparing scores (UIs, admin endpoints, sorting checks) gets the
// engine's authoritative ranking — no duplicated formula, no drift.
//
// Returns nil if the slot is unknown, has never produced a tick, or
// the running policy doesn't include a scoring step.
func (e *Engine) GetScores(networkID, method string) map[string]float64 {
	slot := e.lookupSlot(networkID, method)
	if slot == nil {
		return nil
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if len(slot.lastScores) == 0 {
		return nil
	}
	out := make(map[string]float64, len(slot.lastScores))
	for id, v := range slot.lastScores {
		out[id] = v
	}
	return out
}

// lookupSlot is the shared resolver used by the per-slot accessors —
// exact (network, method) match falls back to the network's "*" slot.
func (e *Engine) lookupSlot(networkID, method string) *Slot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if slot, ok := e.slots[slotKey{networkID, method}]; ok {
		return slot
	}
	return e.slots[slotKey{networkID, "*"}]
}

// SetPaused gates every slot's ticker. While paused, the goroutine still
// wakes on each tick but skips `tickOnce`, so:
//   - the cache (atomically swapped at the end of `tickOnce`) is frozen
//     at the last successful verdict — request-path readers see a stable
//     ordering for the duration of the pause
//   - cross-tick state (excludedSince map, lastSwitchAt, decision ring,
//     tracker EMA) doesn't drift relative to the frozen verdict
//
// Edge-triggered: callers wanting "advance one tick under pause" must
// SetPaused(false), wait, SetPaused(true) — or call TickForTest on each
// slot. Used by the simulator's pause button to freeze the engine alongside
// frontend traffic generation; production callers shouldn't need this.
func (e *Engine) SetPaused(p bool) {
	e.paused.Store(p)
}

// IsPaused reports whether the per-slot ticker is currently gated.
func (e *Engine) IsPaused() bool {
	return e.paused.Load()
}

// SetStepLogEnabled toggles per-tick step-log + annotation capture.
// While enabled, each Decision the engine produces carries:
//   - StepLog: chronological trail of stdlib steps invoked
//   - Annotations: per-upstream `annotate(u, note)` strings
//
// This is the input the simulator's "policy history" detail view
// consumes and the source DEBUG-level eRPC logs use for per-step
// breakdowns. Off by default to keep production-path overhead at
// "one extra function call indirection per stdlib step". Callers that
// flip this on should mirror their toggle to the engine's log level
// (debug → on, info+ → off) so log volume stays sane.
func (e *Engine) SetStepLogEnabled(enabled bool) {
	e.stepLogEnabled.Store(enabled)
}

// IsStepLogEnabled reports whether per-step trail capture is active.
func (e *Engine) IsStepLogEnabled() bool {
	return e.stepLogEnabled.Load()
}

// Stop cancels every slot's ticker and releases pooled runtimes.
func (e *Engine) Stop() {
	e.mu.Lock()
	for _, slot := range e.slots {
		slot.stop()
	}
	e.slots = make(map[slotKey]*Slot)
	e.mu.Unlock()
	e.cancel()
}
