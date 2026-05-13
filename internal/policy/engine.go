package policy

import (
	"context"
	"sync"
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

	appCtx context.Context
	cancel context.CancelFunc
}

type slotKey struct {
	network string
	method  string
}

type networkRegistration struct {
	upstreams []common.Upstream
	cfg       *common.SelectionPolicyConfig
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
// If `cfg.Eval` is the placeholder identity policy (`common.DefaultSelectionPolicySource`),
// this method upgrades it to the embedded rich default (sortByScore +
// preferGroup + stickyPrimary + probeExcluded) — that policy uses std-lib
// methods which only exist once a runtime has been primed. common/ cannot
// reach internal/policy/, so the substitution happens here.
//
// The initial eval runs synchronously so callers can rely on a non-empty
// cache being present once RegisterNetwork returns.
func (e *Engine) RegisterNetwork(networkID string, upstreams []common.Upstream, cfg *common.SelectionPolicyConfig) error {
	if err := upgradeDefaultPolicy(cfg); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.networks[networkID] = &networkRegistration{
		upstreams: upstreams,
		cfg:       cfg,
	}

	// Tear down any pre-existing slots for this network so reconfiguration
	// is atomic from the request-path's perspective.
	for k, slot := range e.slots {
		if k.network == networkID {
			slot.stop()
			delete(e.slots, k)
		}
	}

	wildcard := newSlot(e, networkID, "*", upstreams, cfg)
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
	e.mu.RLock()
	slot, ok := e.slots[slotKey{networkID, method}]
	if !ok {
		slot = e.slots[slotKey{networkID, "*"}]
	}
	e.mu.RUnlock()
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
	e.mu.RLock()
	slot, ok := e.slots[slotKey{networkID, method}]
	if !ok {
		slot = e.slots[slotKey{networkID, "*"}]
	}
	e.mu.RUnlock()
	if slot == nil {
		return time.Time{}
	}
	if d := slot.ring.latest(); d != nil {
		return d.TickAt
	}
	return time.Time{}
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
