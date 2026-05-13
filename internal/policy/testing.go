package policy

import (
	"sort"

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
	slot, ok := e.slots[slotKey{networkID, "*"}]
	reg := e.networks[networkID]
	e.mu.RUnlock()
	if !ok || reg == nil {
		return
	}
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
	slot, ok := e.slots[slotKey{networkID, method}]
	if !ok {
		slot = e.slots[slotKey{networkID, "*"}]
	}
	e.mu.RUnlock()
	if slot != nil {
		slot.tickOnce()
	}
}

// DecisionsForTest returns the slot's ring-buffer snapshot
// (oldest → newest).
func DecisionsForTest(e *Engine, networkID, method string) []*Decision {
	e.mu.RLock()
	slot, ok := e.slots[slotKey{networkID, method}]
	if !ok {
		slot = e.slots[slotKey{networkID, "*"}]
	}
	e.mu.RUnlock()
	if slot == nil {
		return nil
	}
	return slot.ring.snapshot()
}
