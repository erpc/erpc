package policy

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/grafana/sobek"
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

// slotKey identifies one slot in the engine. The wildcard for an
// unspecified dimension is `"*"`. Concrete values:
//
//   - When neither EvalPerMethod nor EvalPerFinality is set: only one
//     slot exists per network, keyed `("*", "*")`. `GetOrdered` always
//     returns its cached order regardless of the caller's method or
//     finality args.
//   - When EvalPerMethod is on (and EvalPerFinality off): slots are
//     lazy-created as `(method, "*")` on first request for that
//     method. The wildcard `("*", "*")` serves cold-start traffic.
//   - When EvalPerFinality is on (and EvalPerMethod off): slots are
//     lazy-created as `("*", finality)` on first request.
//   - When both are on: slots are lazy-created as `(method, finality)`.
//     The wildcard `("*", "*")` still serves cold-start before any
//     specific slot has produced a tick.
type slotKey struct {
	network  string
	method   string
	finality string
}

type networkRegistration struct {
	// upstreamsFn is queried at every tick so newly-bootstrapped upstreams
	// become visible to the engine without a re-register. Static fixtures
	// can use a closure that returns a frozen slice.
	upstreamsFn func() []common.Upstream
	cfg         *common.SelectionPolicyConfig
	// networkLabel is the human-friendly alias (e.g. "base", "mainnet")
	// used on Prometheus labels. Distinct from the canonical `networkID`
	// (e.g. "evm:8453") which stays the engine's internal key. Defaults
	// to networkID when the caller passes "".
	networkLabel string
}

// NewEngine constructs an Engine. `tracker` is read-only from the engine's
// perspective — the engine snapshots metrics from it once per tick. The
// `runtimePrimer` is invoked on each freshly-created sobek runtime; pass
// nil to skip primings (the default policy works against the bare runtime).
//
// `userScript` is the compiled program of the user's TS config file
// (when the config was loaded from `.ts`/`.js`; nil for YAML). When
// non-nil, the pool evaluates it in every freshly-created runtime AFTER
// the primer — that materializes the user's helpers + imports + the
// `__erpcFns` registry natively in each runtime, so `EvalFunc` sentinels
// resolve to real sobek functions whose closure scope is the user's
// module.
func NewEngine(
	parentCtx context.Context,
	logger *zerolog.Logger,
	projectID string,
	tracker *health.Tracker,
	runtimePrimer func(*common.Runtime) error,
	userScript *sobek.Program,
) *Engine {
	ctx, cancel := context.WithCancel(parentCtx)
	lg := logger.With().Str("component", "policy-engine").Str("projectId", projectID).Logger()
	return &Engine{
		projectID: projectID,
		logger:    &lg,
		tracker:   tracker,
		pool:      newRuntimePool(runtimePrimer, userScript),
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
// `networkLabel` is the human-friendly alias used on Prometheus labels —
// pass `Network.Label()` from the caller. When the caller passes "" it
// falls back to `networkID` so test/fixture callers don't have to plumb
// a label they don't care about.
//
// The initial eval runs synchronously so callers can rely on a non-empty
// cache being present once RegisterNetwork returns.
func (e *Engine) RegisterNetwork(networkID, networkLabel string, upstreamsFn func() []common.Upstream, cfg *common.SelectionPolicyConfig) error {
	if err := upgradeDefaultPolicy(cfg); err != nil {
		return err
	}
	if upstreamsFn == nil {
		upstreamsFn = func() []common.Upstream { return nil }
	}
	if networkLabel == "" {
		networkLabel = networkID
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.networks[networkID] = &networkRegistration{
		upstreamsFn:  upstreamsFn,
		cfg:          cfg,
		networkLabel: networkLabel,
	}

	// Tear down any pre-existing slots for this network so reconfiguration
	// is atomic from the request-path's perspective.
	for k, slot := range e.slots {
		if k.network == networkID {
			slot.stop()
			delete(e.slots, k)
		}
	}

	wildcard := newSlot(e, networkID, networkLabel, "*", "*", upstreamsFn, cfg)
	e.slots[slotKey{networkID, "*", "*"}] = wildcard

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

// GetOrdered returns the cached eligible upstreams for
// `(networkID, method, finality)`, in policy-chosen order. The lookup
// is wait-free in the common path.
//
// `method` and `finality` are "narrowing" hints — the engine resolves
// them against the slot map according to the network's
// `EvalPerMethod` / `EvalPerFinality` config:
//
//   - Neither flag set: only the wildcard `("*", "*")` slot exists;
//     `method`/`finality` are ignored.
//   - `EvalPerMethod=true`: slots are keyed by the method;
//     `finality` is ignored.
//   - `EvalPerFinality=true`: slots are keyed by the finality;
//     `method` is ignored.
//   - Both: slots are keyed by `(method, finality)`.
//
// On a cold-start miss (slot just lazy-created, no tick yet) the
// wildcard slot's cache is returned as a fallback. The returned slice
// is the SAME backing array the slot stores atomically — callers MUST
// treat it as read-only.
func (e *Engine) GetOrdered(networkID, method, finality string) []common.Upstream {
	slot, wildcard := e.lookupSlotWithFallback(networkID, method, finality)
	if slot != nil {
		if ordered := slot.cache.Load(); ordered != nil && len(*ordered) > 0 {
			return *ordered
		}
	}
	if wildcard != nil {
		if ordered := wildcard.cache.Load(); ordered != nil {
			return *ordered
		}
	}
	return nil
}

// LastEvalAt returns the wall-clock timestamp of the slot's most recent
// successful tick, or the zero value if the slot is unknown / has never
// produced a decision.
func (e *Engine) LastEvalAt(networkID, method, finality string) time.Time {
	slot := e.lookupSlot(networkID, method, finality)
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
func (e *Engine) LastSwitchAt(networkID, method, finality string) time.Time {
	slot := e.lookupSlot(networkID, method, finality)
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
func (e *Engine) RecentDecisions(networkID, method, finality string, limit int) []*Decision {
	slot := e.lookupSlot(networkID, method, finality)
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
func (e *Engine) GetScores(networkID, method, finality string) map[string]float64 {
	slot := e.lookupSlot(networkID, method, finality)
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

// effectiveKey collapses the caller's `(method, finality)` hint to the
// slot-key shape the network is configured for. Empty / "*" inputs are
// preserved as wildcards.
func effectiveKey(cfg *common.SelectionPolicyConfig, networkID, method, finality string) slotKey {
	m, f := "*", "*"
	if cfg != nil {
		if cfg.EvalPerMethod && method != "" && method != "*" {
			m = method
		}
		if cfg.EvalPerFinality && finality != "" && finality != "*" {
			f = finality
		}
	}
	return slotKey{networkID, m, f}
}

// lookupSlot is the shared resolver used by per-slot accessors that
// want a single slot to read from (admin endpoints, diagnostics):
// exact `(network, method, finality)` match falls back to the network's
// wildcard slot. Does NOT lazy-create — those are read-only paths.
func (e *Engine) lookupSlot(networkID, method, finality string) *Slot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	reg := e.networks[networkID]
	var cfg *common.SelectionPolicyConfig
	if reg != nil {
		cfg = reg.cfg
	}
	if slot, ok := e.slots[effectiveKey(cfg, networkID, method, finality)]; ok {
		return slot
	}
	return e.slots[slotKey{networkID, "*", "*"}]
}

// lookupSlotWithFallback is the request-path resolver used by
// `GetOrdered`. Differs from `lookupSlot` in two ways:
//
//  1. Returns BOTH the exact slot AND the wildcard so the caller can
//     fall back to wildcard while a freshly-created per-bucket slot's
//     cache is still empty (between creation and first tickOnce).
//  2. Lazy-creates the per-(method, finality) slot if `EvalPerMethod`
//     and/or `EvalPerFinality` is on AND no slot exists yet. The new
//     slot inherits the same `upstreamsFn` + cfg as the wildcard and
//     starts its own ticker — subsequent requests for this bucket get
//     a bucket-specific ranking.
//
// First call for a fresh bucket pays a brief write-lock; later calls
// are wait-free RLock reads like the rest of the request path.
func (e *Engine) lookupSlotWithFallback(networkID, method, finality string) (slot *Slot, wildcard *Slot) {
	e.mu.RLock()
	reg, hasReg := e.networks[networkID]
	var cfg *common.SelectionPolicyConfig
	if hasReg {
		cfg = reg.cfg
	}
	wildcard = e.slots[slotKey{networkID, "*", "*"}]
	key := effectiveKey(cfg, networkID, method, finality)
	// No-narrowing case: key == wildcard, just return.
	if key.method == "*" && key.finality == "*" {
		e.mu.RUnlock()
		return wildcard, wildcard
	}
	if s, ok := e.slots[key]; ok {
		e.mu.RUnlock()
		return s, wildcard
	}
	e.mu.RUnlock()

	if !hasReg || cfg == nil {
		return nil, wildcard
	}

	// Upgrade to write lock + double-check (another goroutine may have
	// won the race while we waited).
	e.mu.Lock()
	if s, ok := e.slots[key]; ok {
		e.mu.Unlock()
		return s, wildcard
	}
	// Pass the resolved key's method/finality (with wildcards collapsed)
	// to the new slot so its ticker emits ctx.finality / metric labels
	// at the right scope. `reg.networkLabel` was set at RegisterNetwork
	// time — falls back to networkID if the caller didn't supply one.
	label := reg.networkLabel
	if label == "" {
		label = networkID
	}
	s := newSlot(e, networkID, label, key.method, key.finality, reg.upstreamsFn, reg.cfg)
	e.slots[key] = s
	e.mu.Unlock()
	// Start the slot's ticker asynchronously so this request-path call
	// returns immediately. The slot's cache stays nil until the first
	// tick completes, hence the wildcard fallback in `GetOrdered`.
	s.start(e.appCtx)
	return s, wildcard
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
