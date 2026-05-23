package policy

import (
	"sync"

	"github.com/erpc/erpc/common"
)

// stickyStore is the cross-slot primary register that lets
// `stickyPrimary({scope})` keep one primary in agreement across multiple
// per-(network, method, finality) slots. Entries are keyed by a
// scope-resolved tuple — slots that share the same key share the same
// sticky state.
//
// Slot writes are extremely sparse (only on actual primary switches),
// reads happen once per tick per slot. A single `sync.RWMutex` is fine
// for fleet scale (sub-µs contention even at 12 K slots × 0.16 ticks/s).
type stickyStore struct {
	mu      sync.RWMutex
	entries map[stickyKey]stickyEntry
}

// stickyKey collapses the scope into the tuple actually used to index
// the register. Each axis is either the real value or `"*"` (when the
// scope spans that axis).
//
//	scope=NETWORK                   → (network,  "*",     "*")
//	scope=NETWORK_METHOD            → (network,  method,  "*")
//	scope=NETWORK_FINALITY          → (network,  "*",     finality)
//	scope=NETWORK_METHOD_FINALITY   → (network,  method,  finality)  [≡ slotKey]
type stickyKey struct {
	network  string
	method   string
	finality string
}

type stickyEntry struct {
	// Primary is the currently-elected primary upstream id, or "" if
	// no primary has been chosen yet (cold start).
	Primary string
	// LastSwitchAt is unix-millis of the last primary change. Used by
	// the JS-side `minSwitchInterval` anti-flap timer. Zero when
	// Primary is "" (cold start) or set by the very first writer (so
	// the cooldown clock starts immediately, not at the second tick).
	LastSwitchAt int64
}

func newStickyStore() *stickyStore {
	return &stickyStore{entries: make(map[stickyKey]stickyEntry)}
}

// resolveStickyKey collapses the scope spec into the tuple that indexes
// the register. Unknown scope values fall through to the most-granular
// shape (`network-method-finality`) — degrades to per-slot independence
// rather than blowing up an in-flight eval.
func resolveStickyKey(scope common.EvalScope, network, method, finality string) stickyKey {
	switch scope {
	case common.EvalScopeNetwork:
		return stickyKey{network: network, method: "*", finality: "*"}
	case common.EvalScopeNetworkMethod:
		return stickyKey{network: network, method: method, finality: "*"}
	case common.EvalScopeNetworkFinality:
		return stickyKey{network: network, method: "*", finality: finality}
	case common.EvalScopeNetworkMethodFinality:
		return stickyKey{network: network, method: method, finality: finality}
	default:
		return stickyKey{network: network, method: method, finality: finality}
	}
}

// Get returns the current primary + lastSwitchAt for `key`. The boolean
// is false when no entry exists (cold start) so the caller can
// distinguish "no previous primary" from `Primary == ""`.
func (s *stickyStore) Get(key stickyKey) (string, int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[key]
	if !ok {
		return "", 0, false
	}
	return e.Primary, e.LastSwitchAt, true
}

// Set writes `primary` + `lastSwitchAt` for `key`. Replaces any
// existing entry. Callers MUST decide whether to write at all — the
// JS-side hysteresis / minSwitchInterval guards live in the eval, not
// here.
func (s *stickyStore) Set(key stickyKey, primary string, lastSwitchAt int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = stickyEntry{Primary: primary, LastSwitchAt: lastSwitchAt}
}

// snapshot returns a copy of every entry under a single read lock —
// used by diagnostic surfaces (admin RPC, simulator) that want the
// whole register without N round-trips.
func (s *stickyStore) snapshot() map[stickyKey]stickyEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[stickyKey]stickyEntry, len(s.entries))
	for k, v := range s.entries {
		out[k] = v
	}
	return out
}
