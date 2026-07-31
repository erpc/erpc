package telemetry

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestCounterVec builds a CounterVec on a private registry so tests never
// touch the global package metrics.
func newTestCounterVec(t *testing.T, name string) *prometheus.CounterVec {
	t.Helper()
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name}, []string{"a", "b"})
	reg := prometheus.NewRegistry()
	reg.MustRegister(vec)
	ResetHandleCache()
	t.Cleanup(ResetHandleCache)
	return vec
}

// sleepMs guarantees time.Now().UnixMilli() strictly advances across the gap.
func sleepMs(t *testing.T) {
	t.Helper()
	time.Sleep(3 * time.Millisecond)
}

// A future-cutoff sweep evicts every idle series from BOTH the cache and the
// parent vec: DeleteLabelValues must release the registry series, not just
// drop the cache entry.
func TestSweepIdleCounterHandles_EvictsIdleSeries(t *testing.T) {
	vec := newTestCounterVec(t, "test_sweep_evicts_total")

	CounterHandle(vec, "x", "1").Inc()
	CounterHandle(vec, "y", "2").Inc()
	if got := testutil.CollectAndCount(vec); got != 2 {
		t.Fatalf("precondition: expected 2 series, got %d", got)
	}

	evicted := sweepIdleCounterHandlesBefore(time.Now().UnixMilli() + 1000)
	if evicted != 2 {
		t.Fatalf("expected 2 evicted, got %d", evicted)
	}
	if got := testutil.CollectAndCount(vec); got != 0 {
		t.Fatalf("expected 0 series after sweep, got %d — DeleteLabelValues did not release the series", got)
	}
}

// A past-cutoff sweep evicts nothing: series stay in the registry and the
// cached handle keeps incrementing the SAME series.
func TestSweepIdleCounterHandles_RetainsActiveSeries(t *testing.T) {
	vec := newTestCounterVec(t, "test_sweep_retains_total")

	CounterHandle(vec, "x", "1").Inc()
	CounterHandle(vec, "y", "2").Inc()

	if evicted := sweepIdleCounterHandlesBefore(time.Now().UnixMilli() - 60_000); evicted != 0 {
		t.Fatalf("expected 0 evicted with past cutoff, got %d", evicted)
	}
	if got := testutil.CollectAndCount(vec); got != 2 {
		t.Fatalf("expected 2 series retained, got %d", got)
	}

	CounterHandle(vec, "x", "1").Inc()
	if got := testutil.ToFloat64(CounterHandle(vec, "x", "1")); got != 2 {
		t.Fatalf("expected same series to continue at 2, got %v", got)
	}
}

// Re-fetching a handle refreshes its idle timestamp: a sweep with a cutoff
// taken before the re-fetch must evict nothing, even though the series was
// CREATED before that cutoff.
func TestCounterHandle_TouchRefreshPreventsEviction(t *testing.T) {
	vec := newTestCounterVec(t, "test_touch_refresh_total")

	CounterHandle(vec, "x", "1").Inc() // created at t0
	sleepMs(t)
	cutoff := time.Now().UnixMilli() // t0 < cutoff: without a touch, evictable
	sleepMs(t)
	CounterHandle(vec, "x", "1") // touch refreshes lastAccessedAtMs past cutoff

	if evicted := sweepIdleCounterHandlesBefore(cutoff); evicted != 0 {
		t.Fatalf("expected 0 evicted after touch refresh, got %d", evicted)
	}
	if got := testutil.CollectAndCount(vec); got != 1 {
		t.Fatalf("expected series to survive sweep, got %d series", got)
	}
}

// After eviction, the same label tuple is reborn as a FRESH series starting
// at zero — same semantics as a process restart.
func TestCounterHandle_RebirthAfterEvictionStartsAtZero(t *testing.T) {
	vec := newTestCounterVec(t, "test_rebirth_total")

	h := CounterHandle(vec, "x", "1")
	h.Inc()
	h.Inc()

	if evicted := sweepIdleCounterHandlesBefore(time.Now().UnixMilli() + 1000); evicted != 1 {
		t.Fatalf("expected 1 evicted, got %d", evicted)
	}

	CounterHandle(vec, "x", "1").Inc()
	if got := testutil.ToFloat64(CounterHandle(vec, "x", "1")); got != 1 {
		t.Fatalf("expected reborn series at 1, got %v (old count resurrected?)", got)
	}
	if got := testutil.CollectAndCount(vec); got != 1 {
		t.Fatalf("expected 1 series after rebirth, got %d", got)
	}
}

// Eviction is per-entry, not cache-wide: stale entries of vec A are swept
// while fresh entries of vec B survive, and the return value counts only
// what was evicted.
func TestSweepIdleCounterHandles_PerEntryEvictionAcrossVecs(t *testing.T) {
	vecA := newTestCounterVec(t, "test_sweep_stale_total")
	vecB := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_sweep_fresh_total"}, []string{"a", "b"})
	prometheus.NewRegistry().MustRegister(vecB)

	CounterHandle(vecA, "x", "1").Inc()
	CounterHandle(vecA, "y", "2").Inc()
	sleepMs(t)
	cutoff := time.Now().UnixMilli() // A's entries are strictly older than cutoff
	CounterHandle(vecB, "x", "1").Inc()
	CounterHandle(vecB, "y", "2").Inc() // B's entries are at/after cutoff

	if evicted := sweepIdleCounterHandlesBefore(cutoff); evicted != 2 {
		t.Fatalf("expected exactly 2 evicted (vec A only), got %d", evicted)
	}
	if got := testutil.CollectAndCount(vecA); got != 0 {
		t.Fatalf("expected vec A emptied, got %d series", got)
	}
	if got := testutil.CollectAndCount(vecB); got != 2 {
		t.Fatalf("expected vec B untouched with 2 series, got %d", got)
	}
}

// SetCounterIdleEvictionAfter(0) disables eviction: the public sweep returns
// 0 and leaves the series registered, even for entries old enough that any
// positive threshold would have evicted them.
func TestSweepIdleCounterHandles_DisabledThresholdEvictsNothing(t *testing.T) {
	t.Cleanup(func() { SetCounterIdleEvictionAfter(DefaultCounterIdleEvictionAfter) })
	vec := newTestCounterVec(t, "test_sweep_disabled_total")

	SetCounterIdleEvictionAfter(0)
	CounterHandle(vec, "x", "1").Inc()
	sleepMs(t) // old enough that a 1ms threshold would evict it

	if evicted := SweepIdleCounterHandles(); evicted != 0 {
		t.Fatalf("expected 0 evicted with eviction disabled, got %d", evicted)
	}
	if got := testutil.CollectAndCount(vec); got != 1 {
		t.Fatalf("expected series retained with eviction disabled, got %d", got)
	}
}

// The public no-arg sweep respects the configured threshold: with a 1ms
// threshold, an entry idle for >=5ms is evicted and its series released.
func TestSweepIdleCounterHandles_ConfiguredThresholdEvicts(t *testing.T) {
	t.Cleanup(func() { SetCounterIdleEvictionAfter(DefaultCounterIdleEvictionAfter) })
	vec := newTestCounterVec(t, "test_sweep_configured_total")

	SetCounterIdleEvictionAfter(1 * time.Millisecond)
	CounterHandle(vec, "x", "1").Inc()
	time.Sleep(5 * time.Millisecond)

	if evicted := SweepIdleCounterHandles(); evicted < 1 {
		t.Fatalf("expected >=1 evicted past configured threshold, got %d", evicted)
	}
	if got := testutil.CollectAndCount(vec); got != 0 {
		t.Fatalf("expected series released after threshold sweep, got %d", got)
	}
}

// Regression stress test for the sweep/fetch orphan race: the old ordering in
// sweepIdleCounterHandlesBefore deleted the cache entry BEFORE
// vec.DeleteLabelValues, so a concurrent CounterHandle miss could re-cache the
// still-registered child that the sweep then deregistered — a permanently
// orphaned hot handle whose increments never reach the registry. The fix
// serializes the miss path against eviction (counterHandleCreateMu).
//
// Invariant defended: after any sweep/fetch interleaving quiesces, an Inc()
// through CounterHandle must be visible via vec.WithLabelValues on the same
// label tuple. WithLabelValues on a swept tuple recreates the series at 0 —
// fine; the invariant is DELTA visibility, not absolute counts.
func TestCounterHandle_SweepRaceNeverOrphansHandles(t *testing.T) {
	vec := newTestCounterVec(t, "test_sweep_race_orphan_total")
	labels := []string{"x", "1"}
	key := counterKey{vec: vec, key: labelsKey(labels)}

	CounterHandle(vec, labels...).Inc() // seed the cache entry

	for round := range 600 {
		// Age the cached entry deterministically so the sweep sees it stale.
		v, ok := counterHandleCache.Load(key)
		if !ok {
			t.Fatalf("round %d: cache entry missing before aging", round)
		}
		v.(*cachedCounterHandle).lastAccessedAtMs.Store(time.Now().UnixMilli() - 10_000)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			sweepIdleCounterHandlesBefore(time.Now().UnixMilli() - 5_000)
		}()
		go func() {
			defer wg.Done()
			CounterHandle(vec, labels...).Inc()
		}()
		wg.Wait()

		// Orphan detector: an Inc through the cached handle must land on the
		// vec's currently-registered child. If the race re-cached a
		// deregistered child, `after` stays at `before`.
		before := testutil.ToFloat64(vec.WithLabelValues(labels...))
		CounterHandle(vec, labels...).Inc()
		after := testutil.ToFloat64(vec.WithLabelValues(labels...))
		if after != before+1 {
			t.Fatalf("round %d: orphaned counter handle: before=%v after=%v — Inc through cached handle is invisible in the registry", round, before, after)
		}
	}
}
