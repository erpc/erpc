// Package relay is one network's relay feed: for each relayed subscription
// kind, a ring of deduplicated messages per variant (spec §11), bounded by
// the kind's common.RelaySpec.
package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

// Feed is one network's relay feed. Sources Add what they receive to a
// relay topic; taps read a topic's ring from their own sequence.
type Feed struct {
	projectId, network string
	dialect            common.PubSubDialect
	// entries is keys, set under mu on every change.
	entries prometheus.Gauge

	mu    sync.Mutex
	kinds map[string]*kind
	// keys is how many keys the kinds' dedup sets hold together.
	keys int
}

// kind is one relayed kind with an open ring.
type kind struct {
	spec  *common.RelaySpec
	rings map[string]*ring // by variant
	// seen holds each key added within spec.DedupFor, at most
	// spec.MaxKeys, with the rings it was added to; order lists those
	// keys, each once, oldest first from head.
	seen  map[string]seenIn
	order []seenKey
	head  int
}

// seenIn is the rings a key was added to: first, then any others. Most
// keys reach one ring, so more stays nil.
type seenIn struct {
	first *ring
	more  []*ring
}

func (in seenIn) has(r *ring) bool { return in.first == r || slices.Contains(in.more, r) }

// without is in less r; false when no ring is left.
func (in seenIn) without(r *ring) (seenIn, bool) {
	if i := slices.Index(in.more, r); i >= 0 {
		in.more = slices.Delete(in.more, i, i+1)
		return in, true
	}
	if in.first != r {
		return in, true
	}
	if len(in.more) == 0 {
		return seenIn{}, false
	}
	last := len(in.more) - 1
	in.first, in.more = in.more[last], in.more[:last]
	return in, true
}

type seenKey struct {
	key string
	at  time.Time // its first add
}

// ring holds a variant's newest entries, as many as its kind's RingSize;
// entry seq s (from 1) is at entries[s%len(entries)]. next is the seq the
// next entry gets.
type ring struct {
	entries []json.RawMessage
	next    uint64
	changed chan struct{}
}

var _ indexer.RelayFeed = (*Feed)(nil)

// New returns an empty feed for the relayed kinds of dialect; projectId and
// network label its metrics.
func New(projectId, network string, dialect common.PubSubDialect) *Feed {
	return &Feed{
		projectId: projectId,
		network:   network,
		dialect:   dialect,
		entries:   telemetry.GaugeHandle(telemetry.MetricSubscriptionDedupEntries, projectId, network),
		kinds:     make(map[string]*kind),
	}
}

// Spec is the bounds of kind; nil when the dialect does not relay it.
func (f *Feed) Spec(kind string) *common.RelaySpec {
	if f.dialect == nil {
		return nil
	}
	k, ok := f.dialect.Kind(kind)
	if !ok {
		return nil
	}
	return k.Relay
}

// Open opens t's ring if it is not open; ErrRelayVariantLimitExceeded when
// its kind's MaxVariants rings are open.
func (f *Feed) Open(t indexer.Topic) error {
	name, variant, ok := t.Relay()
	if !ok {
		return fmt.Errorf("%q is not a relay topic", t)
	}
	spec := f.Spec(name)
	if spec == nil {
		return fmt.Errorf("%q is not a relayed kind", name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.kinds[name]
	open := 0
	if k != nil {
		if k.rings[variant] != nil {
			return nil
		}
		open = len(k.rings)
	}
	if open >= spec.MaxVariants {
		return common.NewErrRelayVariantLimitExceeded(name, spec.MaxVariants)
	}
	if k == nil {
		k = &kind{spec: spec, rings: make(map[string]*ring), seen: make(map[string]seenIn)}
		f.kinds[name] = k
	}
	k.rings[variant] = &ring{entries: make([]json.RawMessage, spec.RingSize), next: 1, changed: make(chan struct{})}
	return nil
}

// Close closes t's ring: its readers end and its keys stop suppressing.
// The last ring of a kind to close empties the kind's dedup set.
func (f *Feed) Close(t indexer.Topic) {
	name, variant, _ := t.Relay()
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.kinds[name]
	if k == nil || k.rings[variant] == nil {
		return
	}
	r := k.rings[variant]
	delete(k.rings, variant)
	close(r.changed)
	if len(k.rings) == 0 {
		f.keys -= len(k.seen)
		delete(f.kinds, name)
	} else {
		order := make([]seenKey, 0, len(k.seen))
		for _, sk := range k.order[k.head:] {
			if in, ok := k.seen[sk.key].without(r); ok {
				k.seen[sk.key] = in
				order = append(order, sk)
			} else {
				delete(k.seen, sk.key)
				f.keys--
			}
		}
		k.order, k.head = order, 0
	}
	f.entries.Set(float64(f.keys))
}

// Add puts raw on t's ring unless the ring is closed or raw's key was
// added to it while the key is held: for its kind's DedupFor from its
// first add to any ring of the kind. A message without a key is kept
// without dedup.
func (f *Feed) Add(t indexer.Topic, raw json.RawMessage) {
	name, variant, ok := t.Relay()
	if !ok {
		return
	}
	spec := f.Spec(name)
	if spec == nil {
		return
	}
	var key string
	if spec.Key != nil {
		key = spec.Key(raw)
	}
	now := time.Now()
	f.mu.Lock()
	k := f.kinds[name]
	var r *ring
	if k != nil {
		r = k.rings[variant]
	}
	if r == nil {
		f.mu.Unlock()
		return
	}
	dropped := false
	if key != "" {
		f.expireLocked(k, now)
		switch in, ok := k.seen[key]; {
		case in.has(r):
			dropped = true
		case ok:
			in.more = append(in.more, r)
			k.seen[key] = in
		default:
			k.seen[key] = seenIn{first: r}
			k.order = append(k.order, seenKey{key: key, at: now})
			f.keys++
			if len(k.seen) > k.spec.MaxKeys {
				f.popLocked(k)
			}
		}
		f.entries.Set(float64(f.keys))
	}
	if !dropped {
		r.entries[r.next%uint64(len(r.entries))] = raw
		r.next++
		close(r.changed)
		r.changed = make(chan struct{})
	}
	f.mu.Unlock()
	if dropped {
		telemetry.CounterHandle(telemetry.MetricSubscriptionDedupDroppedTotal, f.projectId, f.network, name).Inc()
	}
}

func (f *Feed) expireLocked(k *kind, now time.Time) {
	for k.head < len(k.order) && now.Sub(k.order[k.head].at) >= k.spec.DedupFor {
		f.popLocked(k)
	}
}

func (f *Feed) popLocked(k *kind) {
	delete(k.seen, k.order[k.head].key)
	f.keys--
	k.order[k.head] = seenKey{}
	k.head++
	// Compact into a new array once the popped half dominates, so order
	// stays O(MaxKeys) and the old array is freed.
	if k.head > len(k.order)/2 && k.head > 1024 {
		k.order = slices.Clone(k.order[k.head:])
		k.head = 0
	}
}

// ringLocked is t's open ring, or nil.
func (f *Feed) ringLocked(t indexer.Topic) *ring {
	name, variant, _ := t.Relay()
	if k := f.kinds[name]; k != nil {
		return k.rings[variant]
	}
	return nil
}

// Tail is t's newest sequence, 0 before its first entry.
func (f *Feed) Tail(t indexer.Topic) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.ringLocked(t); r != nil {
		return r.next - 1
	}
	return 0
}

// Read returns every entry of t after seq, waiting for one, bounded by
// ctx. A seq whose next entry left the ring is ErrOutOfRange.
func (f *Feed) Read(ctx context.Context, t indexer.Topic, after uint64) ([]json.RawMessage, uint64, error) {
	for {
		f.mu.Lock()
		r := f.ringLocked(t)
		if r == nil {
			f.mu.Unlock()
			return nil, after, fmt.Errorf("relay topic %q is not open", t)
		}
		size := uint64(len(r.entries))
		newest := r.next - 1
		oldest := uint64(1)
		if newest > size {
			oldest = newest - size + 1
		}
		if after+1 < oldest {
			f.mu.Unlock()
			return nil, after, indexer.OutOfRangeError{Reason: indexer.OutOfRangeLag}
		}
		if after < newest {
			out := make([]json.RawMessage, 0, newest-after)
			for s := after + 1; s <= newest; s++ {
				out = append(out, r.entries[s%size])
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
