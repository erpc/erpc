package policy

import (
	"github.com/erpc/erpc/common"
)

// OverrideOrderForTest replaces the cache for `(networkID, "*")` with the
// given ordered upstream IDs (resolved against the network's registered
// upstreams). Unknown ids are dropped. The slot's ticker stays running, but
// the next tick will overwrite this override — tests that want stable
// ordering for the duration of the test should also stop the slot via the
// engine's lifecycle.
//
// This replaces the legacy `upstream.ReorderUpstreams` used by 20+
// retry/hedge/failsafe tests that need deterministic ordering rather than
// any policy behavior.
func OverrideOrderForTest(e *Engine, networkID string, ids ...string) {
	e.mu.RLock()
	slot, ok := e.slots[slotKey{networkID, "*"}]
	reg := e.networks[networkID]
	e.mu.RUnlock()
	if !ok || reg == nil {
		return
	}
	index := make(map[string]common.Upstream, len(reg.upstreams))
	for _, u := range reg.upstreams {
		index[u.Id()] = u
	}
	ordered := make([]common.Upstream, 0, len(ids))
	for _, id := range ids {
		if u, ok := index[id]; ok {
			ordered = append(ordered, u)
		}
	}
	slot.cache.Store(&ordered)
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
