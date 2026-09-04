package telemetry

import (
	"fmt"

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
	activeIdx  []int
	vec        *prometheus.CounterVec
}

// newLabeledCounterUnregistered builds the counter without registering it. This
// is the only way production counters are built: Prometheus freezes a metric's
// label-set hash for the life of the registry, so registering at package init
// would make label customizations impossible to apply later.
// DefineLabeledCounter hands the result to the manager, which registers it once
// Configure has installed the policy.
func newLabeledCounterUnregistered(opts prometheus.CounterOpts, schema []string) *LabeledCounter {
	family := familyName(opts.Namespace, opts.Subsystem, opts.Name)
	idx := currentPolicy().labelIndices(family, kindCounter, schema)
	active := make([]string, len(idx))
	for i, j := range idx {
		active[i] = schema[j]
	}
	return &LabeledCounter{
		opts:       opts,
		metricName: opts.Name,
		schema:     schema,
		activeIdx:  idx,
		vec:        prometheus.NewCounterVec(opts, active),
	}
}

// rebuildInPlace re-creates the underlying CounterVec under the CURRENT policy,
// keeping this pointer's identity so the package-level var and every call site
// that captured it stay valid.
//
// Prometheus freezes dimHashesByName for a metric's fqName even after
// Unregister, so a label-set change cannot be applied by unregister+re-register:
// the rebuild has to happen while the counter is still unregistered. The manager
// is what guarantees that ordering — it rebuilds only definitions it has not yet
// registered.
func (lc *LabeledCounter) rebuildInPlace() {
	replacement := newLabeledCounterUnregistered(lc.opts, lc.schema)
	lc.activeIdx = replacement.activeIdx
	lc.vec = replacement.vec
}

func (lc *LabeledCounter) Describe(ch chan<- *prometheus.Desc) { lc.vec.Describe(ch) }
func (lc *LabeledCounter) Collect(ch chan<- prometheus.Metric) { lc.vec.Collect(ch) }
func (lc *LabeledCounter) Reset()                              { lc.vec.Reset() }

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
	if len(lc.activeIdx) == len(lc.schema) {
		return lc.vec.WithLabelValues(vals...)
	}
	return lc.vec.WithLabelValues(lc.project(vals)...)
}

// DeleteLabelValues removes the series for the given FULL-schema label set,
// honoring the same convention as WithLabelValues. Returns true if a series
// existed and was deleted. Used by the idle-counter sweep to release series
// for label combinations that have gone quiet.
func (lc *LabeledCounter) DeleteLabelValues(vals ...string) bool {
	lc.assertArity(vals)
	if len(lc.activeIdx) == len(lc.schema) {
		return lc.vec.DeleteLabelValues(vals...)
	}
	return lc.vec.DeleteLabelValues(lc.project(vals)...)
}

// ActiveLabelValues projects full-schema values down to the retained subset, so
// callers can key their own caches on the effective (post-filter) labels. This
// is what lets multiple full-label tuples that now resolve to the same series
// share one handle-cache entry instead of fighting over it.
func (lc *LabeledCounter) ActiveLabelValues(vals []string) []string {
	lc.assertArity(vals)
	if len(lc.activeIdx) == len(lc.schema) {
		return vals
	}
	return lc.project(vals)
}

func (lc *LabeledCounter) project(vals []string) []string {
	active := make([]string, len(lc.activeIdx))
	for i, idx := range lc.activeIdx {
		active[i] = vals[idx]
	}
	return active
}
