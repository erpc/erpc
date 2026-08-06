package telemetry

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestLabeledCounter builds a LabeledCounter on a private registry under the
// given filter, so tests never touch the global package metrics.
func newTestLabeledCounter(t *testing.T, name string, schema []string, drop []string, overrides map[string][]string) *LabeledCounter {
	t.Helper()
	t.Cleanup(func() { SetCounterLabelFilter(nil, nil) })
	t.Cleanup(ResetHandleCache)
	SetCounterLabelFilter(drop, overrides)
	lc := newLabeledCounterUnregistered(prometheus.CounterOpts{Name: name}, schema)
	reg := prometheus.NewRegistry()
	reg.MustRegister(lc)
	ResetHandleCache()
	return lc
}

// Dropping a label collapses the series that differed only in it, and the
// counter total is preserved — the sum is what stays correct, the dimension is
// what is lost.
func TestLabeledCounter_DropCollapsesSeriesAndPreservesSum(t *testing.T) {
	lc := newTestLabeledCounter(t, "test_lc_drop_total", []string{"network", "agent_name"}, []string{"agent_name"}, nil)

	lc.WithLabelValues("evm:1", "agent-a").Inc()
	lc.WithLabelValues("evm:1", "agent-b").Inc()
	lc.WithLabelValues("evm:1", "agent-c").Inc()

	if got := testutil.CollectAndCount(lc); got != 1 {
		t.Fatalf("expected 3 agents to collapse to 1 series, got %d", got)
	}
	if got := testutil.ToFloat64(lc.vec.WithLabelValues("evm:1")); got != 3 {
		t.Fatalf("expected collapsed counter to total 3, got %v", got)
	}
}

// A per-metric override re-adds a dropped label for that metric only, so a
// fleet-wide drop can spare the one counter a downstream pipeline reads.
func TestLabeledCounter_OverrideKeepsLabelForNamedMetric(t *testing.T) {
	schema := []string{"network", "agent_name"}
	overrides := map[string][]string{"test_lc_kept_total": {"agent_name"}}

	kept := newTestLabeledCounter(t, "test_lc_kept_total", schema, []string{"agent_name"}, overrides)
	kept.WithLabelValues("evm:1", "agent-a").Inc()
	kept.WithLabelValues("evm:1", "agent-b").Inc()
	if got := testutil.CollectAndCount(kept); got != 2 {
		t.Fatalf("override should preserve agent_name: expected 2 series, got %d", got)
	}

	// Same filter, a metric NOT named in the overrides still drops the label.
	dropped := newLabeledCounterUnregistered(prometheus.CounterOpts{Name: "test_lc_other_total"}, schema)
	reg := prometheus.NewRegistry()
	reg.MustRegister(dropped)
	dropped.WithLabelValues("evm:1", "agent-a").Inc()
	dropped.WithLabelValues("evm:1", "agent-b").Inc()
	if got := testutil.CollectAndCount(dropped); got != 1 {
		t.Fatalf("unnamed metric should drop agent_name: expected 1 series, got %d", got)
	}
}

// Call sites keep passing the FULL schema; a wrong arity is a miswiring and
// must fail loudly rather than silently mislabel a series.
func TestLabeledCounter_PanicsOnArityMismatch(t *testing.T) {
	lc := newTestLabeledCounter(t, "test_lc_arity_total", []string{"network", "agent_name"}, []string{"agent_name"}, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on short label list")
		}
	}()
	lc.WithLabelValues("evm:1")
}

// CounterHandle must key on POST-filter labels. Two full tuples differing only
// in a dropped label are the same underlying series, so they must share one
// cache entry — otherwise the idle sweep can evict the series out from under a
// tuple that is still being incremented.
func TestCounterHandle_CollapsedTuplesShareOneCacheEntry(t *testing.T) {
	lc := newTestLabeledCounter(t, "test_lc_handle_total", []string{"network", "agent_name"}, []string{"agent_name"}, nil)

	a := CounterHandle(lc, "evm:1", "agent-a")
	b := CounterHandle(lc, "evm:1", "agent-b")
	if a != b {
		t.Fatal("collapsed tuples must resolve to the same cached child counter")
	}

	entries := 0
	counterHandleCache.Range(func(_, _ any) bool { entries++; return true })
	if entries != 1 {
		t.Fatalf("expected 1 cache entry for collapsed tuples, got %d", entries)
	}
}

// The regression this keying prevents: tuple A goes quiet while tuple B — the
// same underlying series — stays hot. A sweep must not delete the live series.
func TestCounterHandle_SweepKeepsSeriesLiveViaSiblingTuple(t *testing.T) {
	lc := newTestLabeledCounter(t, "test_lc_sweep_sibling_total", []string{"network", "agent_name"}, []string{"agent_name"}, nil)

	CounterHandle(lc, "evm:1", "agent-a").Inc()
	sleepMs(t)
	cutoff := time.Now().UnixMilli()
	sleepMs(t)
	// agent-b refreshes the shared entry after the cutoff.
	CounterHandle(lc, "evm:1", "agent-b").Inc()

	if evicted := sweepIdleCounterHandlesBefore(cutoff); evicted != 0 {
		t.Fatalf("expected 0 evicted while a sibling tuple is hot, got %d", evicted)
	}
	if got := testutil.CollectAndCount(lc); got != 1 {
		t.Fatalf("live series must survive the sweep, got %d series", got)
	}
	if got := testutil.ToFloat64(lc.vec.WithLabelValues("evm:1")); got != 2 {
		t.Fatalf("expected both increments retained, got %v", got)
	}
}

// The sweep releases a filtered counter's series through the full-schema
// DeleteLabelValues projection — the stored tuple is full-schema, the vec is
// not, and the projection has to line up or nothing is deleted.
func TestCounterHandle_SweepDeletesFilteredSeries(t *testing.T) {
	lc := newTestLabeledCounter(t, "test_lc_sweep_delete_total", []string{"network", "agent_name"}, []string{"agent_name"}, nil)

	CounterHandle(lc, "evm:1", "agent-a").Inc()
	if got := testutil.CollectAndCount(lc); got != 1 {
		t.Fatalf("precondition: expected 1 series, got %d", got)
	}
	if evicted := sweepIdleCounterHandlesBefore(time.Now().UnixMilli() + 1000); evicted != 1 {
		t.Fatalf("expected 1 evicted, got %d", evicted)
	}
	if got := testutil.CollectAndCount(lc); got != 0 {
		t.Fatalf("expected series released from the vec, got %d", got)
	}
}

// With no filter installed, a LabeledCounter is a pass-through: every schema
// label is retained. Guards against the filter defaulting to "drop everything".
func TestLabeledCounter_NoFilterRetainsFullSchema(t *testing.T) {
	lc := newTestLabeledCounter(t, "test_lc_nofilter_total", []string{"network", "agent_name"}, nil, nil)

	lc.WithLabelValues("evm:1", "agent-a").Inc()
	lc.WithLabelValues("evm:1", "agent-b").Inc()
	if got := testutil.CollectAndCount(lc); got != 2 {
		t.Fatalf("unfiltered counter should keep both series, got %d", got)
	}
	if got := lc.ActiveLabelValues([]string{"evm:1", "agent-a"}); len(got) != 2 {
		t.Fatalf("expected full projection, got %v", got)
	}
}
