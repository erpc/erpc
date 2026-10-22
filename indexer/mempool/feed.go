// Package mempool is one network's mempool feed: a ring of deduplicated
// pending transactions per subscribe variant (spec §11).
package mempool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
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

// ErrVariantLimit refuses a variant past MaxVariants open ones.
var ErrVariantLimit = errors.New("too many newPendingTransactions variants on the network")

// Feed is one network's mempool feed. Sources Add what they receive; taps
// read a variant's ring from their own sequence.
type Feed struct {
	projectId, network string

	mu    sync.Mutex
	rings map[string]*ring
	// seen holds the dedup keys (variant, hash) added within DedupFor, at
	// most MaxHashes; order lists them oldest first from head.
	seen  map[string]struct{}
	order []seenKey
	head  int
}

type seenKey struct {
	key string
	at  time.Time
}

// ring holds a variant's newest RingSize entries; entry seq s (from 1) is
// at entries[s%RingSize]. next is the seq the next entry gets.
type ring struct {
	refs    int
	entries []json.RawMessage
	next    uint64
	changed chan struct{}
}

var _ indexer.MempoolFeed = (*Feed)(nil)

// New returns an empty feed; projectId and network label its metrics.
func New(projectId, network string) *Feed {
	return &Feed{projectId: projectId, network: network, rings: make(map[string]*ring), seen: make(map[string]struct{})}
}

// Acquire opens variant until release; opened reports that no one held it,
// so its sources are only now asked for it. A variant past MaxVariants
// open ones fails with ErrVariantLimit. The ring goes with its last holder.
func (f *Feed) Acquire(variant string) (release func(), opened bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rings[variant]
	if r == nil {
		if len(f.rings) >= MaxVariants {
			return nil, false, ErrVariantLimit
		}
		r = &ring{entries: make([]json.RawMessage, RingSize), next: 1, changed: make(chan struct{})}
		f.rings[variant] = r
		opened = true
	}
	r.refs++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if r.refs--; r.refs == 0 && f.rings[variant] == r {
				delete(f.rings, variant)
				close(r.changed)
			}
		})
	}, opened, nil
}

// Add puts raw on variant's ring unless the variant is closed or the
// transaction was added to it within DedupFor. A payload without a hash
// is kept without dedup.
func (f *Feed) Add(variant string, raw json.RawMessage) {
	key := txHash(raw)
	now := time.Now()
	f.mu.Lock()
	r := f.rings[variant]
	if r == nil {
		f.mu.Unlock()
		return
	}
	dropped := false
	if key != "" {
		f.expireLocked(now)
		key = variant + "\x00" + key
		if _, ok := f.seen[key]; ok {
			dropped = true
		} else {
			f.seen[key] = struct{}{}
			f.order = append(f.order, seenKey{key: key, at: now})
			if len(f.seen) > MaxHashes {
				f.popLocked()
			}
		}
	}
	if !dropped {
		r.entries[r.next%RingSize] = raw
		r.next++
		close(r.changed)
		r.changed = make(chan struct{})
	}
	entries := len(f.seen)
	f.mu.Unlock()
	if dropped {
		telemetry.CounterHandle(telemetry.MetricSubscriptionDedupDroppedTotal, f.projectId, f.network, indexer.SubTypeNewPendingTransactions).Inc()
		return
	}
	if key != "" {
		telemetry.GaugeHandle(telemetry.MetricSubscriptionDedupEntries, f.projectId, f.network).Set(float64(entries))
	}
}

func (f *Feed) expireLocked(now time.Time) {
	ttl := DedupFor.Load()
	for f.head < len(f.order) && now.Sub(f.order[f.head].at) >= ttl {
		f.popLocked()
	}
}

func (f *Feed) popLocked() {
	delete(f.seen, f.order[f.head].key)
	f.order[f.head] = seenKey{}
	f.head++
	// Compact once the popped half dominates, so order stays O(MaxHashes).
	if f.head > len(f.order)/2 && f.head > 1024 {
		f.order = append(f.order[:0], f.order[f.head:]...)
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
func (f *Feed) Read(ctx context.Context, variant string, after uint64) ([]json.RawMessage, uint64, error) {
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
			out := make([]json.RawMessage, 0, newest-after)
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
