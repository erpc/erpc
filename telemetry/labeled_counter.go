package telemetry

import (
	"fmt"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// LabeledCounter wraps a prometheus.CounterVec whose label set is the
// intersection of a canonical schema and what the metrics customizations retain.
// Call sites always pass values for the full schema (in schema order); the
// wrapper forwards only the retained positions to the underlying Vec.
//
// Counters need label customization more than histograms do: dropping a label
// from a histogram only removes buckets, while a counter label like a
// caller-supplied user-agent is frequently the single largest contributor to
// /metrics size. And some counter labels are load-bearing for billing or
// attribution pipelines, so the choice has to be made per deployment rather than
// baked in.
//
// Dropping a label collapses every series that differed only in that label
// into one. The counters remain correct — their sums are preserved — but the
// dropped dimension is no longer queryable. Check downstream consumers before
// dropping a label that a billing or attribution pipeline groups by.
type LabeledCounter struct {
	opts       prometheus.CounterOpts
	metricName string
	schema     []string
	// state holds the Vec and its matching label projection as one immutable
	// value. rebuildInPlace swaps the whole thing atomically so readers never
	// see a Vec paired with the wrong activeIdx; the two must move together.
	state atomic.Pointer[counterState]
}

// counterState is the mutable pair a rebuild replaces: the underlying Vec and
// the schema→retained-position projection that matches its label set.
type counterState struct {
	activeIdx []int
	vec       *prometheus.CounterVec
}

// newLabeledCounterUnregistered builds the counter without registering it. This
// is the only way production counters are built: Prometheus freezes a metric's
// label-set hash for the life of the registry, so registering at package init
// would make label customizations impossible to apply later.
// DefineLabeledCounter hands the result to the manager, which registers it once
// Configure has installed the policy.
func newLabeledCounterUnregistered(opts prometheus.CounterOpts, schema []string) *LabeledCounter {
	lc := &LabeledCounter{
		opts:       opts,
		metricName: opts.Name,
		schema:     schema,
	}
	lc.state.Store(lc.buildState())
	return lc
}

// buildState resolves the retained label positions under the current policy and
// builds a Vec projected onto them.
func (lc *LabeledCounter) buildState() *counterState {
	family := familyName(lc.opts.Namespace, lc.opts.Subsystem, lc.opts.Name)
	idx := currentPolicy().labelIndices(family, kindCounter, lc.schema)
	active := make([]string, len(idx))
	for i, j := range idx {
		active[i] = lc.schema[j]
	}
	return &counterState{
		activeIdx: idx,
		vec:       prometheus.NewCounterVec(lc.opts, active),
	}
}

// rebuildInPlace re-creates the underlying CounterVec under the CURRENT policy,
// keeping this pointer's identity so the package-level var and every call site
// that captured it stay valid. The new Vec is published with a single atomic
// store, so a concurrent WithLabelValues/Collect/Describe reader sees either the
// old state or the new one, never a torn mix of the two.
//
// Prometheus freezes dimHashesByName for a metric's fqName even after
// Unregister, so a label-set change cannot be applied by unregister+re-register:
// the rebuild has to happen while the counter is still unregistered. The manager
// is what guarantees that ordering — it rebuilds only definitions it has not yet
// registered.
func (lc *LabeledCounter) rebuildInPlace() {
	lc.state.Store(lc.buildState())
}

func (lc *LabeledCounter) Describe(ch chan<- *prometheus.Desc) { lc.state.Load().vec.Describe(ch) }
func (lc *LabeledCounter) Collect(ch chan<- prometheus.Metric) { lc.state.Load().vec.Collect(ch) }
func (lc *LabeledCounter) Reset()                              { lc.state.Load().vec.Reset() }

func (lc *LabeledCounter) assertArity(vals []string) {
	if len(vals) != len(lc.schema) {
		panic(fmt.Sprintf("labeled_counter: %s expected %d label values (%v), got %d",
			lc.metricName, len(lc.schema), lc.schema, len(vals)))
	}
}

// WithLabelValues accepts values for the FULL schema and filters internally to
// the labels the current policy retains. Panics on length mismatch to
// surface miswired call sites immediately.
func (lc *LabeledCounter) WithLabelValues(vals ...string) prometheus.Counter {
	lc.assertArity(vals)
	st := lc.state.Load()
	if len(st.activeIdx) == len(lc.schema) {
		return st.vec.WithLabelValues(vals...)
	}
	return st.vec.WithLabelValues(project(vals, st.activeIdx)...)
}

// DeleteLabelValues removes the series for the given FULL-schema label set,
// honoring the same convention as WithLabelValues. Returns true if a series
// existed and was deleted. Used by the idle-counter sweep to release series
// for label combinations that have gone quiet.
func (lc *LabeledCounter) DeleteLabelValues(vals ...string) bool {
	lc.assertArity(vals)
	st := lc.state.Load()
	if len(st.activeIdx) == len(lc.schema) {
		return st.vec.DeleteLabelValues(vals...)
	}
	return st.vec.DeleteLabelValues(project(vals, st.activeIdx)...)
}

// ActiveLabelValues projects full-schema values down to the retained subset, so
// callers can key their own caches on the effective (post-filter) labels. This
// is what lets multiple full-label tuples that now resolve to the same series
// share one handle-cache entry instead of fighting over it.
func (lc *LabeledCounter) ActiveLabelValues(vals []string) []string {
	lc.assertArity(vals)
	st := lc.state.Load()
	if len(st.activeIdx) == len(lc.schema) {
		return vals
	}
	return project(vals, st.activeIdx)
}

// project selects the retained positions out of a full-schema value slice.
func project(vals []string, activeIdx []int) []string {
	active := make([]string, len(activeIdx))
	for i, idx := range activeIdx {
		active[i] = vals[idx]
	}
	return active
}
