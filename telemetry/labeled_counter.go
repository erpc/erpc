package telemetry

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// buildLabelSets normalizes the (dropLabels, keepOverrides) config pair into
// the lookup shape both the histogram and counter filters use. Shared so the
// two filters cannot drift in how they parse config.
func buildLabelSets(dropLabels []string, keepOverrides map[string][]string) (map[string]struct{}, map[string]map[string]struct{}) {
	drop := make(map[string]struct{}, len(dropLabels))
	for _, l := range dropLabels {
		if l = strings.TrimSpace(l); l != "" {
			drop[l] = struct{}{}
		}
	}
	overrides := make(map[string]map[string]struct{}, len(keepOverrides))
	for metricName, keep := range keepOverrides {
		metricName = strings.TrimSpace(metricName)
		if metricName == "" {
			continue
		}
		set := make(map[string]struct{}, len(keep))
		for _, l := range keep {
			if l = strings.TrimSpace(l); l != "" {
				set[l] = struct{}{}
			}
		}
		overrides[metricName] = set
	}
	return drop, overrides
}

// activeIndicesFor returns the positions of `schema` retained given a drop set
// and the per-metric keep overrides.
func activeIndicesFor(drop map[string]struct{}, keepOverrides map[string]map[string]struct{}, metricName string, schema []string) []int {
	overrides := keepOverrides[metricName]
	out := make([]int, 0, len(schema))
	for i, l := range schema {
		if _, dropped := drop[l]; dropped {
			if _, kept := overrides[l]; !kept {
				continue
			}
		}
		out = append(out, i)
	}
	return out
}

// CounterLabelFilter decides which labels a CounterVec exposes.
//
// Global `drop` removes labels from every counter built through
// NewLabeledCounter; per-metric `keepOverrides` re-add labels for specific
// metric names (the Prometheus Name without the namespace prefix, e.g.
// "upstream_request_total").
//
// This is the counter-side twin of HistogramLabelFilter. Counters need their
// own knob because dropping a label from a histogram only removes buckets,
// while a counter label like a caller-supplied user-agent is frequently the
// single largest contributor to /metrics size — and unlike histograms, some
// counter labels are load-bearing for billing/attribution pipelines, so the
// choice has to be made per deployment rather than baked in.
type CounterLabelFilter struct {
	drop          map[string]struct{}
	keepOverrides map[string]map[string]struct{}
}

var (
	counterFilterMu      sync.RWMutex
	currentCounterFilter = &CounterLabelFilter{drop: map[string]struct{}{}}
)

// SetCounterLabelFilter installs the filter used by subsequent
// NewLabeledCounter calls. Because the package-level counters are constructed
// at init (before config is read), callers must follow this with
// RebuildFilteredCounters to re-create them under the new filter — exactly the
// SetHistogramLabelFilter + SetHistogramBuckets sequence.
func SetCounterLabelFilter(dropLabels []string, keepOverrides map[string][]string) {
	drop, overrides := buildLabelSets(dropLabels, keepOverrides)
	counterFilterMu.Lock()
	currentCounterFilter = &CounterLabelFilter{drop: drop, keepOverrides: overrides}
	counterFilterMu.Unlock()
}

// activeIndices returns the positions from `schema` retained under the filter.
func (f *CounterLabelFilter) activeIndices(metricName string, schema []string) []int {
	return activeIndicesFor(f.drop, f.keepOverrides, metricName, schema)
}

// LabeledCounter wraps a prometheus.CounterVec whose label set is the
// intersection of a canonical schema and the current CounterLabelFilter.
// Call sites always pass values for the full schema (in schema order); the
// wrapper forwards only the retained positions to the underlying Vec.
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

// NewLabeledCounter creates a CounterVec honoring the current filter and
// registers it with prometheus.DefaultRegisterer. Prefer the package-level
// counters (built unregistered at init, registered once via
// RebuildFilteredCounters) for production metrics — Prometheus freezes a
// metric's label-set hash for the life of the registry, so registering at
// package init would make counterDropLabels impossible to apply later.
func NewLabeledCounter(opts prometheus.CounterOpts, schema []string) *LabeledCounter {
	return registerOrReuseCounter(newLabeledCounterUnregistered(opts, schema))
}

// newLabeledCounterUnregistered builds the counter without registering it, so
// tests can use a private registry.
func newLabeledCounterUnregistered(opts prometheus.CounterOpts, schema []string) *LabeledCounter {
	counterFilterMu.RLock()
	idx := currentCounterFilter.activeIndices(opts.Name, schema)
	counterFilterMu.RUnlock()
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

// registerOrReuseCounter mirrors registerOrReuse for histograms: on a repeat
// registration with an identical label set, return the already-registered
// collector instead of panicking, which keeps the rebuild idempotent.
func registerOrReuseCounter(lc *LabeledCounter) *LabeledCounter {
	err := prometheus.Register(lc)
	if err == nil {
		return lc
	}
	if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
		if existing, ok := are.ExistingCollector.(*LabeledCounter); ok {
			return existing
		}
	}
	panic(err)
}

// Rebuild re-creates this counter under the CURRENT filter without registering
// it. Prometheus freezes dimHashesByName for a metric's fqName even after
// Unregister, so a label-set change cannot be applied by unregister+re-register
// — the only safe path is the histogram one: build unregistered under the
// filter, then register once. Returns the replacement; callers reassign their
// package-level var, then registerOrReuseCounter.
func (lc *LabeledCounter) Rebuild() *LabeledCounter {
	return newLabeledCounterUnregistered(lc.opts, lc.schema)
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
// the labels retained by the current filter. Panics on length mismatch to
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
