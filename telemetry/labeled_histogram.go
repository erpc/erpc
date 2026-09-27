package telemetry

import (
	"fmt"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// LabeledHistogram wraps a prometheus.HistogramVec whose label set is the
// intersection of a canonical schema and what the metrics customizations retain.
// Call sites always pass values for the full schema (in schema order); the
// wrapper forwards only the retained positions to the underlying Vec.
type LabeledHistogram struct {
	opts       prometheus.HistogramOpts
	metricName string
	schema     []string

	// configBuckets records that the declaration left Buckets empty, meaning
	// this histogram takes whatever metrics.histogramBuckets resolves to. The
	// histograms that declare their own buckets do so because the global
	// latency buckets resolve their range poorly, and must keep them.
	configBuckets bool

	// state holds the Vec and its matching label projection as one immutable
	// value, swapped atomically by rebuildInPlace so a concurrent reader never
	// pairs a Vec with the wrong activeIdx.
	state atomic.Pointer[histogramState]
}

// histogramState is the mutable pair a rebuild replaces: the underlying Vec and
// the schema→retained-position projection that matches its label set.
type histogramState struct {
	activeIdx []int
	vec       *prometheus.HistogramVec
}

// NewLabeledHistogram creates a HistogramVec under the current policy without
// registering it. Production histograms go through DefineLabeledHistogram, which
// hands the result to the manager; call this directly only when you need custom
// registration (e.g. a private registry in tests).
func NewLabeledHistogram(opts prometheus.HistogramOpts, schema []string) *LabeledHistogram {
	lh := &LabeledHistogram{
		opts:          opts,
		metricName:    opts.Name,
		schema:        schema,
		configBuckets: len(opts.Buckets) == 0,
	}
	lh.state.Store(buildHistogramState(opts, schema))
	return lh
}

// buildHistogramState resolves the retained label positions under the current
// policy and builds a Vec projected onto them, using the buckets in opts.
func buildHistogramState(opts prometheus.HistogramOpts, schema []string) *histogramState {
	family := familyName(opts.Namespace, opts.Subsystem, opts.Name)
	idx := currentPolicy().labelIndices(family, kindHistogram, schema)
	active := make([]string, len(idx))
	for i, j := range idx {
		active[i] = schema[j]
	}
	return &histogramState{
		activeIdx: idx,
		vec:       prometheus.NewHistogramVec(opts, active),
	}
}

// rebuildInPlace re-creates the underlying HistogramVec under the CURRENT policy.
// `buckets` replaces the declared boundaries when `explicit` — a customization
// named this family's buckets — or when the declaration left them empty and takes
// whatever metrics.histogramBuckets resolves to. Keeps this pointer's identity so
// the package-level var and every call site that captured it stay valid, and
// publishes the new Vec with a single atomic store. Must run before
// registration, for the same dimHashesByName reason as
// LabeledCounter.rebuildInPlace.
func (lh *LabeledHistogram) rebuildInPlace(buckets []float64, explicit bool) {
	opts := lh.opts
	if explicit || lh.configBuckets {
		opts.Buckets = buckets
	}
	lh.state.Store(buildHistogramState(opts, lh.schema))
}

func (lh *LabeledHistogram) Describe(ch chan<- *prometheus.Desc) { lh.state.Load().vec.Describe(ch) }
func (lh *LabeledHistogram) Collect(ch chan<- prometheus.Metric) { lh.state.Load().vec.Collect(ch) }

// WithLabelValues accepts values for the FULL schema and filters internally to
// the labels the current policy retains. Panics on length mismatch to
// surface miswired call sites immediately.
func (lh *LabeledHistogram) WithLabelValues(vals ...string) prometheus.Observer {
	if len(vals) != len(lh.schema) {
		panic(fmt.Sprintf("labeled_histogram: %s expected %d label values (%v), got %d",
			lh.metricName, len(lh.schema), lh.schema, len(vals)))
	}
	st := lh.state.Load()
	if len(st.activeIdx) == len(lh.schema) {
		return st.vec.WithLabelValues(vals...)
	}
	return st.vec.WithLabelValues(project(vals, st.activeIdx)...)
}

func (lh *LabeledHistogram) Reset() { lh.state.Load().vec.Reset() }

// DeleteLabelValues removes the series for the given label set from the
// underlying HistogramVec — wrapper around `prometheus.HistogramVec.DeleteLabelValues`
// that honors the same FULL-schema convention as WithLabelValues. The
// active-filter projection has to match exactly with what was used at
// creation time; passing the full schema in schema-order is how every
// call site already does it.
//
// Returns true if a series existed and was deleted, false if no such
// label combination was registered (idempotent / safe-to-call-twice).
//
// Used by the health tracker's idle-sweep loop to release Prometheus
// series for label combinations (method × userId × agentName, …) that
// haven't been observed in `idleEvictionAfter` — bounds the
// `/metrics` page's cardinality under method-flood attacks.
func (lh *LabeledHistogram) DeleteLabelValues(vals ...string) bool {
	if len(vals) != len(lh.schema) {
		panic(fmt.Sprintf("labeled_histogram: %s expected %d label values (%v), got %d",
			lh.metricName, len(lh.schema), lh.schema, len(vals)))
	}
	st := lh.state.Load()
	if len(st.activeIdx) == len(lh.schema) {
		return st.vec.DeleteLabelValues(vals...)
	}
	return st.vec.DeleteLabelValues(project(vals, st.activeIdx)...)
}

// ActiveLabelValues projects full-schema values down to the retained subset.
// Useful for callers that want to key their own caches on the effective
// (post-filter) labels so multiple full-label tuples that resolve to the same
// underlying series share a single cache entry.
func (lh *LabeledHistogram) ActiveLabelValues(vals []string) []string {
	if len(vals) != len(lh.schema) {
		panic(fmt.Sprintf("labeled_histogram: %s expected %d label values (%v), got %d",
			lh.metricName, len(lh.schema), lh.schema, len(vals)))
	}
	st := lh.state.Load()
	if len(st.activeIdx) == len(lh.schema) {
		return vals
	}
	return project(vals, st.activeIdx)
}
