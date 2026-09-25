package headcache

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is an in-process Store with the same fencing semantics as a
// shared backend. It backs "local" mode (one store per process) and lets tests
// run several Cache instances against one store to model a fleet.
type MemoryStore struct {
	mu       sync.Mutex
	now      func() time.Time
	blocks   map[string]memBlock // scope/hash
	leases   map[string]*Lease   // scope
	epochs   map[string]uint64   // scope -> last granted epoch
	snaps    map[string]*Snapshot
	watchers map[string]map[chan struct{}]struct{}
	// Unavailable, when set, makes every call fail with ErrStoreUnavailable
	// (tests use it to model backend loss).
	unavailable bool
}

type memBlock struct {
	rec       *BlockRecord
	expiresAt time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		now:      time.Now,
		blocks:   map[string]memBlock{},
		leases:   map[string]*Lease{},
		epochs:   map[string]uint64{},
		snaps:    map[string]*Snapshot{},
		watchers: map[string]map[chan struct{}]struct{}{},
	}
}

// SetUnavailable toggles simulated backend loss.
func (m *MemoryStore) SetUnavailable(v bool) {
	m.mu.Lock()
	m.unavailable = v
	m.mu.Unlock()
}

// ExpireLease force-expires the scope's lease (tests: model a stalled leader).
func (m *MemoryStore) ExpireLease(scope Scope) {
	m.mu.Lock()
	delete(m.leases, scope.Key())
	m.mu.Unlock()
}

func (m *MemoryStore) PutBlock(_ context.Context, scope Scope, rec *BlockRecord, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return ErrStoreUnavailable
	}
	now := m.now()
	// Opportunistic sweep of expired records keeps the map bounded by ttl.
	for k, b := range m.blocks {
		if !b.expiresAt.IsZero() && now.After(b.expiresAt) {
			delete(m.blocks, k)
		}
	}
	var exp time.Time
	if ttl > 0 {
		exp = now.Add(ttl)
	}
	m.blocks[scope.Key()+"/"+rec.Hash] = memBlock{rec: rec, expiresAt: exp}
	return nil
}

func (m *MemoryStore) GetBlock(_ context.Context, scope Scope, hash string) (*BlockRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return nil, ErrStoreUnavailable
	}
	b, ok := m.blocks[scope.Key()+"/"+hash]
	if !ok || (!b.expiresAt.IsZero() && m.now().After(b.expiresAt)) {
		return nil, ErrNotFound
	}
	return b.rec, nil
}

func (m *MemoryStore) liveLease(key string) *Lease {
	l := m.leases[key]
	if l != nil && m.now().After(l.ExpiresAt) {
		return nil
	}
	return l
}

func (m *MemoryStore) AcquireLease(_ context.Context, scope Scope, holder string, ttl time.Duration) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return nil, ErrStoreUnavailable
	}
	k := scope.Key()
	if m.liveLease(k) != nil {
		return nil, ErrLeaseHeld
	}
	m.epochs[k]++
	l := &Lease{Scope: scope, Holder: holder, Epoch: m.epochs[k], ExpiresAt: m.now().Add(ttl)}
	m.leases[k] = l
	cp := *l
	return &cp, nil
}

func (m *MemoryStore) current(lease *Lease) bool {
	l := m.liveLease(lease.Scope.Key())
	return l != nil && l.Epoch == lease.Epoch && l.Holder == lease.Holder
}

func (m *MemoryStore) RenewLease(_ context.Context, lease *Lease, ttl time.Duration) (*Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return nil, ErrStoreUnavailable
	}
	if !m.current(lease) {
		return nil, ErrLeaseLost
	}
	l := m.leases[lease.Scope.Key()]
	l.ExpiresAt = m.now().Add(ttl)
	cp := *l
	return &cp, nil
}

func (m *MemoryStore) ReleaseLease(_ context.Context, lease *Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return ErrStoreUnavailable
	}
	if m.current(lease) {
		delete(m.leases, lease.Scope.Key())
	}
	return nil
}

func (m *MemoryStore) PublishSnapshot(_ context.Context, lease *Lease, snap *Snapshot) error {
	m.mu.Lock()
	if m.unavailable {
		m.mu.Unlock()
		return ErrStoreUnavailable
	}
	if !m.current(lease) || snap.Epoch != lease.Epoch {
		m.mu.Unlock()
		return ErrLeaseLost
	}
	k := lease.Scope.Key()
	if !snap.Valid() {
		m.mu.Unlock()
		return ErrInvalidSnapshot
	}
	if prev := m.snaps[k]; prev != nil && prev.Epoch == snap.Epoch && snap.Seq <= prev.Seq {
		m.mu.Unlock()
		return ErrInvalidSnapshot
	}
	cp := *snap
	cp.Hashes = append([]string(nil), snap.Hashes...)
	m.snaps[k] = &cp
	m.pruneScopeLocked(k, cp.Hashes)
	for ch := range m.watchers[k] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	m.mu.Unlock()
	return nil
}

// pruneScopeLocked drops this scope's records that the just-committed
// snapshot no longer references. Without it the in-process store retained
// every hydrated block for RecordTTL (default one hour), regardless of the
// canonical window and MaxBytes. It runs only after fencing and validation
// accepted the snapshot, and never touches other scopes.
//
// This is safe: the leader writes every referenced record before publishing,
// so nothing the snapshot needs is removed. Delivered events and each Cache's
// local window hold their own *BlockRecord pointers, so pruning never
// invalidates them. A follower that races a newer publish finds the old hash
// missing and treats that height as a cache miss, as it already does for
// expired records.
func (m *MemoryStore) pruneScopeLocked(scopeKey string, keep []string) {
	prefix := scopeKey + "/"
	live := make(map[string]struct{}, len(keep))
	for _, h := range keep {
		live[prefix+h] = struct{}{}
	}
	for k := range m.blocks {
		if len(k) <= len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		if _, ok := live[k]; !ok {
			delete(m.blocks, k)
		}
	}
}

// blockCount returns the number of stored records for a scope (tests).
func (m *MemoryStore) blockCount(scope Scope) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := scope.Key() + "/"
	n := 0
	for k := range m.blocks {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func (m *MemoryStore) LoadSnapshot(_ context.Context, scope Scope) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return nil, ErrStoreUnavailable
	}
	s, ok := m.snaps[scope.Key()]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	cp.Hashes = append([]string(nil), s.Hashes...)
	return &cp, nil
}

func (m *MemoryStore) WatchSnapshots(_ context.Context, scope Scope) (<-chan struct{}, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := scope.Key()
	ch := make(chan struct{}, 1)
	if m.watchers[k] == nil {
		m.watchers[k] = map[chan struct{}]struct{}{}
	}
	m.watchers[k][ch] = struct{}{}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.watchers[k], ch)
			m.mu.Unlock()
		})
	}, nil
}
