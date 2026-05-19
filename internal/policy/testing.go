package policy

import (
	"sort"
	"time"

	"github.com/erpc/erpc/common"
)

// OverrideOrderForTest replaces the cache for `(networkID, "*")` with the
// given ordered upstream IDs (resolved against the network's registered
// upstreams). If no IDs are given, all registered upstreams are pinned in
// ascending-id order — mirrors the legacy `upstream.ReorderUpstreams`
// convenience used by retry/hedge/failsafe tests that just want a
// deterministic order without caring about policy behavior.
//
// Unknown ids are dropped. The slot's ticker stays running, but the next
// tick will overwrite this override — tests that want stable ordering for
// the duration of the test should set `EvalInterval: 0` on the network's
// SelectionPolicy.
func OverrideOrderForTest(e *Engine, networkID string, ids ...string) {
	e.mu.RLock()
	slot, ok := e.slots[slotKey{networkID, "*", "*"}]
	reg := e.networks[networkID]
	e.mu.RUnlock()
	if !ok || reg == nil {
		return
	}
	// Stop the ticker for the lifetime of this override — tests that pin
	// don't want background re-eval clobbering their cache mid-test, and
	// thousands of test fixtures running at 1s tick each pushes the
	// race-detector CI suite past its 20-minute budget.
	slot.stop()
	ups := reg.upstreamsFn()
	index := make(map[string]common.Upstream, len(ups))
	for _, u := range ups {
		index[u.Id()] = u
	}
	if len(ids) == 0 {
		ids = make([]string, 0, len(ups))
		for id := range index {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	ordered := make([]common.Upstream, 0, len(ids))
	for _, id := range ids {
		if u, ok := index[id]; ok {
			ordered = append(ordered, u)
		}
	}
	slot.cache.Store(&ordered)
}

// OverrideAllForTest applies OverrideOrderForTest to EVERY network this
// engine knows about. This is the direct replacement for the legacy
// `upstream.ReorderUpstreams(registry, ids...)` which had no per-network
// scoping. If `ids` is empty, each network's upstreams are pinned in
// ascending-id order.
//
// Nil engine is treated as a no-op so tests that build a Network without
// an engine (or via a partial fixture) don't panic.
func OverrideAllForTest(e *Engine, ids ...string) {
	if e == nil {
		return
	}
	e.mu.RLock()
	networks := make([]string, 0, len(e.networks))
	for net := range e.networks {
		networks = append(networks, net)
	}
	e.mu.RUnlock()
	for _, net := range networks {
		OverrideOrderForTest(e, net, ids...)
	}
}

// TickForTest forces one synchronous eval cycle on the slot, bypassing the
// ticker. Useful when a test wants to drive metrics and then observe the
// resulting order without sleeping.
func TickForTest(e *Engine, networkID, method string) {
	e.mu.RLock()
	slot, ok := e.slots[slotKey{networkID, method, "*"}]
	if !ok {
		slot = e.slots[slotKey{networkID, "*", "*"}]
	}
	e.mu.RUnlock()
	if slot != nil {
		slot.tickOnce()
	}
}

// ResetSlotStateForTest clears the cross-tick bookkeeping of a slot
// (previousOrder / previousExcluded / lastSwitchAt / excludedSince)
// so subsequent ticks compute as if they were the first. Used by test
// harnesses that register a network, then bootstrap upstreams
// asynchronously, then need a clean baseline before seeding metrics
// — without this the initial-tick result (which may have run against
// a partial upstream list) lingers as the slot's `previousOrder` and
// disturbs `stickyPrimary`'s tie-breaking on the next tick.
func ResetSlotStateForTest(e *Engine, networkID, method string) {
	slot := lookupSlotForTest(e, networkID, method)
	if slot == nil {
		return
	}
	slot.mu.Lock()
	slot.previousOrder = nil
	slot.previousExcluded = nil
	slot.excludedSince = make(map[string]int64)
	slot.lastSwitchAt = nil
	slot.mu.Unlock()
}

// SetFinalityForTest overrides `ctx.finality` on the slot's next tick(s)
// without threading finality through the request layer. Useful for
// testing policies that branch by finality (e.g. the default policy's
// finality-conditional `stickyPrimary`).
//
// Pass "" to clear. Accepted values: "realtime", "unfinalized",
// "finalized", "unknown" — matches `EvalContext.Finality`.
func SetFinalityForTest(e *Engine, networkID, method, finality string) {
	slot := lookupSlotForTest(e, networkID, method)
	if slot == nil {
		return
	}
	slot.mu.Lock()
	slot.testFinality = finality
	slot.mu.Unlock()
}

// AdvanceEvalNowForTest adds `delta` to `ctx.now` on the slot's next
// tick(s). The slot's `excludedSince` timestamps stay anchored to real
// time, so this effectively simulates "wall clock has advanced by
// `delta` since the upstream was excluded" — exactly what
// `probeExcluded(reAdmitAfter:...)` reads.
//
// Calls accumulate: two AdvanceEvalNowForTest(60s) bumps push ctx.now
// forward by 120 s. Pass a negative delta to roll back.
func AdvanceEvalNowForTest(e *Engine, networkID, method string, delta time.Duration) {
	slot := lookupSlotForTest(e, networkID, method)
	if slot == nil {
		return
	}
	slot.mu.Lock()
	slot.testNowOffset += delta.Milliseconds()
	slot.mu.Unlock()
}

// lookupSlotForTest resolves a slot by (network, method), falling back
// to the network's wildcard slot if no method-specific slot exists.
// Tests that need to address a finality-specific slot should call
// `lookupSlot(networkID, method, finality)` directly via the Engine
// once that helper is exported — for now this stays method-scoped to
// keep existing test fixtures unchanged.
func lookupSlotForTest(e *Engine, networkID, method string) *Slot {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	slot, ok := e.slots[slotKey{networkID, method, "*"}]
	if !ok {
		slot = e.slots[slotKey{networkID, "*", "*"}]
	}
	e.mu.RUnlock()
	return slot
}

// LatestDecisionOutputForTest returns the cached output of the most
// recent eval as ordered upstream IDs (those that survived) plus the
// excluded IDs (those the eval dropped). Tests use this to make
// assertions like "after a degradation, rpc-A was excluded" without
// any persistent ring buffer.
func LatestDecisionOutputForTest(e *Engine, networkID, method string) (order []string, excluded []string) {
	e.mu.RLock()
	slot, ok := e.slots[slotKey{networkID, method, "*"}]
	if !ok {
		slot = e.slots[slotKey{networkID, "*", "*"}]
	}
	e.mu.RUnlock()
	if slot == nil {
		return nil, nil
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return cloneStrings(slot.previousOrder), cloneStrings(slot.previousExcluded)
}
