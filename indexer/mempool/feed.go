// Package mempool is one network's mempool feed: a ring of deduplicated
// pending transactions per subscribe variant (spec §11).
package mempool

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus"
)

// Bounds of one network's feed (spec §13.2).
const (
	RingSize    = 10_000
	MaxHashes   = 100_000
	MaxVariants = 16
)

// DedupFor is how long a transaction hash suppresses its repeats; a var so
// tests can change it.
var DedupFor = util.NewDuration(60 * time.Second)

// ErrVariantLimit refuses a ring past MaxVariants open ones.
var ErrVariantLimit = errors.New("too many newPendingTransactions variants on the network")

// Feed is one network's mempool feed. Sources Add what they receive; taps
// read a variant's ring from their own sequence.
type Feed struct {
	projectId, network string
	// entries is the dedup set's size, set under mu on every change.
	entries prometheus.Gauge

	mu    sync.Mutex
	rings map[string]*ring
	// slots marks the ring slots in use; each open ring has one bit.
	slots uint16
	// seen holds each transaction hash added within DedupFor, at most
	// MaxHashes, with the slots of the rings it was added to; order lists
	// those hashes, each once, oldest first from head.
	seen  map[string]uint16
	order []seenKey
	head  int
}

type seenKey struct {
	hash string
	at   time.Time // its first add
}

// ring holds a variant's newest RingSize entries; entry seq s (from 1) is
// at entries[s%RingSize]. next is the seq the next entry gets.
type ring struct {
	slot    uint16 // its bit in slots and seen
	entries []*indexer.PendingTx
	next    uint64
	changed chan struct{}
}

var _ indexer.MempoolFeed = (*Feed)(nil)

// New returns an empty feed; projectId and network label its metrics.
func New(projectId, network string) *Feed {
	return &Feed{
		projectId: projectId,
		network:   network,
		entries:   telemetry.GaugeHandle(telemetry.MetricSubscriptionDedupEntries, projectId, network),
		rings:     make(map[string]*ring),
		seen:      make(map[string]uint16),
	}
}

// Open opens variant's ring if it is not open; ErrVariantLimit when
// MaxVariants rings are open.
func (f *Feed) Open(variant string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rings[variant] != nil {
		return nil
	}
	if len(f.rings) >= MaxVariants {
		return ErrVariantLimit
	}
	slot := uint16(1)
	for f.slots&slot != 0 {
		slot <<= 1
	}
	f.slots |= slot
	f.rings[variant] = &ring{slot: slot, entries: make([]*indexer.PendingTx, RingSize), next: 1, changed: make(chan struct{})}
	return nil
}

// Close closes variant's ring: its readers end and its hashes stop
// suppressing. The last ring to close empties the dedup set.
func (f *Feed) Close(variant string) {
	f.mu.Lock()
	r := f.rings[variant]
	if r == nil {
		f.mu.Unlock()
		return
	}
	delete(f.rings, variant)
	close(r.changed)
	f.slots &^= r.slot
	if len(f.rings) == 0 {
		f.seen, f.order, f.head = make(map[string]uint16), nil, 0
	} else {
		order := make([]seenKey, 0, len(f.seen))
		for _, k := range f.order[f.head:] {
			if slots := f.seen[k.hash] &^ r.slot; slots != 0 {
				f.seen[k.hash] = slots
				order = append(order, k)
			} else {
				delete(f.seen, k.hash)
			}
		}
		f.order, f.head = order, 0
	}
	f.entries.Set(float64(len(f.seen)))
	f.mu.Unlock()
}

// Add puts raw on variant's ring unless the variant is closed or the
// transaction was added to it while its hash is held: for DedupFor from
// its first add to any ring. A payload without a hash is kept without
// dedup.
func (f *Feed) Add(variant string, raw json.RawMessage) {
	hash := txHash(raw)
	now := time.Now()
	f.mu.Lock()
	r := f.rings[variant]
	if r == nil {
		f.mu.Unlock()
		return
	}
	dropped := false
	if hash != "" {
		f.expireLocked(now)
		switch slots, ok := f.seen[hash]; {
		case slots&r.slot != 0:
			dropped = true
		case ok:
			f.seen[hash] = slots | r.slot
		default:
			f.seen[hash] = r.slot
			f.order = append(f.order, seenKey{hash: hash, at: now})
			if len(f.seen) > MaxHashes {
				f.popLocked()
			}
		}
		f.entries.Set(float64(len(f.seen)))
	}
	if !dropped {
		r.entries[r.next%RingSize] = &indexer.PendingTx{Raw: raw}
		r.next++
		close(r.changed)
		r.changed = make(chan struct{})
	}
	f.mu.Unlock()
	if dropped {
		telemetry.CounterHandle(telemetry.MetricSubscriptionDedupDroppedTotal, f.projectId, f.network, indexer.SubTypeNewPendingTransactions).Inc()
	}
}

func (f *Feed) expireLocked(now time.Time) {
	ttl := DedupFor.Load()
	for f.head < len(f.order) && now.Sub(f.order[f.head].at) >= ttl {
		f.popLocked()
	}
}

func (f *Feed) popLocked() {
	delete(f.seen, f.order[f.head].hash)
	f.order[f.head] = seenKey{}
	f.head++
	// Compact into a new array once the popped half dominates, so order
	// stays O(MaxHashes) and the old array is freed.
	if f.head > len(f.order)/2 && f.head > 1024 {
		f.order = slices.Clone(f.order[f.head:])
		f.head = 0
	}
}

// Tail is variant's newest sequence, 0 before its first entry.
func (f *Feed) Tail(variant string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rings[variant]; r != nil {
		return r.next - 1
	}
	return 0
}

// Read returns every entry of variant after seq, waiting for one, bounded
// by ctx. A seq whose next entry left the ring is ErrOutOfRange.
func (f *Feed) Read(ctx context.Context, variant string, after uint64) ([]*indexer.PendingTx, uint64, error) {
	for {
		f.mu.Lock()
		r := f.rings[variant]
		if r == nil {
			f.mu.Unlock()
			return nil, after, errors.New("newPendingTransactions variant is not open")
		}
		newest := r.next - 1
		oldest := uint64(1)
		if newest > RingSize {
			oldest = newest - RingSize + 1
		}
		if after+1 < oldest {
			f.mu.Unlock()
			return nil, after, indexer.OutOfRangeError{Reason: indexer.OutOfRangeLag}
		}
		if after < newest {
			out := make([]*indexer.PendingTx, 0, newest-after)
			for s := after + 1; s <= newest; s++ {
				out = append(out, r.entries[s%RingSize])
			}
			f.mu.Unlock()
			return out, newest, nil
		}
		changed := r.changed
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, after, ctx.Err()
		case <-changed:
		}
	}
}

// txHash is a pending transaction's identity: the hash a string payload
// is, or an object payload's hash, lowercased; "" when it has none.
func txHash(raw json.RawMessage) string {
	var s string
	if err := common.SonicCfg.Unmarshal(raw, &s); err == nil && s != "" {
		return strings.ToLower(s)
	}
	var obj struct {
		Hash string `json:"hash"`
	}
	if err := common.SonicCfg.Unmarshal(raw, &obj); err == nil {
		return strings.ToLower(obj.Hash)
	}
	return ""
}
