package evm

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/telemetry"
)

// FilterStateBytes bounds, per network, the per-filter state the tracker
// holds on its receipts (streams spec §10); a var so tests lower it.
var FilterStateBytes atomic.Int64

func init() { FilterStateBytes.Store(32 << 20) }

// filterState is one filter's state on one block's receipts: the
// positions of the logs it matches, and each LogsEncoding's encoding of
// them. A filter a client makes up adds one per held block, so its bytes
// count against FilterStateBytes.
type filterState struct {
	key       string
	pos       []int32
	encodings sync.Map // *LogsEncoding -> *headerEncoded

	// Guarded by filterStates.mu: its place in the order while counted,
	// and the bytes it counts.
	elem *list.Element
	size int64
}

// filterStateBase is what a filterState holds besides its key, positions
// and encodings: the struct, its map entry and its order element.
const filterStateBase = 192

// filterStates is the tracker's per-filter state on the receipts it
// holds, oldest first, at most FilterStateBytes: past it the oldest goes,
// and the next tap with that filter on that block builds it again.
// Receipts the tracker does not hold (a Read's own fetch) keep theirs
// uncounted, for as long as that Read holds them.
type filterStates struct {
	mu    sync.Mutex
	order list.List // of filterStateRef
	bytes int64
}

type filterStateRef struct {
	r *heldReceipts
	s *filterState
}

// filterOf is f's state on r, made once per (receipts, filter); nil when a
// log of r does not parse.
func (t *ChainTracker) filterOf(r *heldReceipts, f *logfilter.LogsFilter) *filterState {
	x := r.index()
	if x == nil {
		return nil
	}
	key := f.Key()
	if v, ok := r.filters.Load(key); ok {
		return v.(*filterState)
	}
	s := &filterState{key: key, pos: x.positions(f)}
	if v, loaded := r.filters.LoadOrStore(key, s); loaded {
		return v.(*filterState)
	}
	t.filters.mu.Lock()
	if r.held {
		s.size = int64(filterStateBase + len(key) + 4*cap(s.pos))
		s.elem = t.filters.order.PushBack(filterStateRef{r, s})
		t.filters.bytes += s.size
		t.evictFiltersLocked()
	}
	t.filters.mu.Unlock()
	return s
}

// grewFilter counts n more bytes on s: an encoding made of it.
func (t *ChainTracker) grewFilter(s *filterState, n int) {
	t.filters.mu.Lock()
	if s.elem != nil {
		s.size += int64(n)
		t.filters.bytes += int64(n)
		t.evictFiltersLocked()
	}
	t.filters.mu.Unlock()
}

// evictFiltersLocked drops the oldest states while the bytes pass the
// bound, and publishes the count.
func (t *ChainTracker) evictFiltersLocked() {
	limit := FilterStateBytes.Load()
	evicted := 0
	for t.filters.bytes > limit && t.filters.order.Len() > 0 {
		ref := t.filters.order.Front().Value.(filterStateRef)
		ref.r.filters.CompareAndDelete(ref.s.key, ref.s)
		t.forgetFilterLocked(ref.s)
		evicted++
	}
	if evicted > 0 {
		telemetry.CounterHandle(telemetry.MetricChainTrackerFilterStateEvictedTotal, t.projectId(), t.networkLabel()).Add(float64(evicted))
	}
	telemetry.GaugeHandle(telemetry.MetricChainTrackerFilterStateBytes, t.projectId(), t.networkLabel()).Set(float64(t.filters.bytes))
}

func (t *ChainTracker) forgetFilterLocked(s *filterState) {
	t.filters.order.Remove(s.elem)
	t.filters.bytes -= s.size
	s.elem, s.size = nil, 0
}

// holdFilters counts r's filter states from now on: the tracker holds r.
func (t *ChainTracker) holdFilters(r *heldReceipts) {
	t.filters.mu.Lock()
	r.held = true
	t.filters.mu.Unlock()
}

// dropFilters stops counting r's filter states: the tracker let r go, and
// its states go with it.
func (t *ChainTracker) dropFilters(r *heldReceipts) {
	t.filters.mu.Lock()
	defer t.filters.mu.Unlock()
	r.held = false
	r.filters.Range(func(_, v any) bool {
		if s := v.(*filterState); s.elem != nil {
			t.forgetFilterLocked(s)
		}
		return true
	})
	telemetry.GaugeHandle(telemetry.MetricChainTrackerFilterStateBytes, t.projectId(), t.networkLabel()).Set(float64(t.filters.bytes))
}
