package telemetry

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// HistogramLabelFilter decides which labels a HistogramVec exposes.
//
// Global `drop` removes labels from every histogram; per-metric `keepOverrides`
// re-add labels for specific metric names (identified by the Prometheus metric
// Name, e.g. "network_request_duration_seconds", without the namespace prefix).
type HistogramLabelFilter struct {
	drop          map[string]struct{}
	keepOverrides map[string]map[string]struct{}
}

var (
	filterMu      sync.RWMutex
	currentFilter = &HistogramLabelFilter{drop: map[string]struct{}{}}
)

// SetHistogramLabelFilter installs a filter used by subsequent NewLabeledHistogram
// calls. Typically invoked once at startup from config, before SetHistogramBuckets.
func SetHistogramLabelFilter(dropLabels []string, keepOverrides map[string][]string) {
	f := &HistogramLabelFilter{
		drop:          make(map[string]struct{}, len(dropLabels)),
		keepOverrides: make(map[string]map[string]struct{}, len(keepOverrides)),
	}
	for _, l := range dropLabels {
		l = strings.TrimSpace(l)
		if l != "" {
			f.drop[l] = struct{}{}
		}
	}
	for metricName, keep := range keepOverrides {
		metricName = strings.TrimSpace(metricName)
		if metricName == "" {
			continue
		}
		set := make(map[string]struct{}, len(keep))
		for _, l := range keep {
			l = strings.TrimSpace(l)
			if l != "" {
				set[l] = struct{}{}
			}
		}
		f.keepOverrides[metricName] = set
	}
	filterMu.Lock()
	currentFilter = f
	filterMu.Unlock()
}

// activeIndices returns the positions from `schema` retained under the filter.
func (f *HistogramLabelFilter) activeIndices(metricName string, schema []string) []int {
	overrides := f.keepOverrides[metricName]
	out := make([]int, 0, len(schema))
	for i, l := range schema {
		if _, dropped := f.drop[l]; dropped {
			if _, kept := overrides[l]; !kept {
				continue
			}
		}
		out = append(out, i)
	}
	return out
}

// LabeledHistogram wraps a prometheus.HistogramVec whose label set is the
// intersection of a canonical schema and the current HistogramLabelFilter.
// Call sites always pass values for the full schema (in schema order); the
// wrapper forwards only the retained positions to the underlying Vec.
type LabeledHistogram struct {
	metricName string
	schema     []string
	activeIdx  []int
	vec        *prometheus.HistogramVec
}

// NewLabeledHistogram creates a HistogramVec using the current filter.
// The caller is responsible for registering it with prometheus.Registerer
// (use Register/Unregister helpers below to match the existing vec lifecycle).
func NewLabeledHistogram(opts prometheus.HistogramOpts, schema []string) *LabeledHistogram {
	filterMu.RLock()
	idx := currentFilter.activeIndices(opts.Name, schema)
	filterMu.RUnlock()
	active := make([]string, len(idx))
	for i, j := range idx {
		active[i] = schema[j]
	}
	return &LabeledHistogram{
		metricName: opts.Name,
		schema:     schema,
		activeIdx:  idx,
		vec:        prometheus.NewHistogramVec(opts, active),
	}
}

// Describe implements prometheus.Collector.
func (lh *LabeledHistogram) Describe(ch chan<- *prometheus.Desc) { lh.vec.Describe(ch) }

// Collect implements prometheus.Collector.
func (lh *LabeledHistogram) Collect(ch chan<- prometheus.Metric) { lh.vec.Collect(ch) }

// WithLabelValues accepts values for the FULL schema and filters internally to
// the labels retained by the current filter. Panics on length mismatch to
// surface miswired call sites immediately.
func (lh *LabeledHistogram) WithLabelValues(vals ...string) prometheus.Observer {
	if len(vals) != len(lh.schema) {
		panic(fmt.Sprintf("labeled_histogram: %s expected %d label values (%v), got %d",
			lh.metricName, len(lh.schema), lh.schema, len(vals)))
	}
	if len(lh.activeIdx) == len(lh.schema) {
		return lh.vec.WithLabelValues(vals...)
	}
	active := make([]string, len(lh.activeIdx))
	for i, idx := range lh.activeIdx {
		active[i] = vals[idx]
	}
	return lh.vec.WithLabelValues(active...)
}

// Reset clears the underlying HistogramVec.
func (lh *LabeledHistogram) Reset() { lh.vec.Reset() }
