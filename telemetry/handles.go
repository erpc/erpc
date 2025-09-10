package telemetry

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Caches for label-bound metric handles to avoid per-call Vec map lookups and locks.
// Keyed by the Vec pointer plus the full labels key.

type counterKey struct {
	vec *prometheus.CounterVec
	key string
}

type gaugeKey struct {
	vec *prometheus.GaugeVec
	key string
}

type observerKey struct {
	vec *prometheus.HistogramVec
	key string
}

var (
	counterHandleCache  sync.Map // map[counterKey]prometheus.Counter
	gaugeHandleCache    sync.Map // map[gaugeKey]prometheus.Gauge
	observerHandleCache sync.Map // map[observerKey]prometheus.Observer
)

func labelsKey(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	// Use a small, zero-alloc builder for common small label sets.
	// Separator '\x1f' (unit separator) minimizes collision with label values.
	var b strings.Builder
	// Pre-size: assume avg 12 chars per label
	b.Grow(len(labels) * 12)
	for i, s := range labels {
		if i > 0 {
			b.WriteByte('\x1f')
		}
		b.WriteString(s)
	}
	return b.String()
}

// CounterHandle returns a cached child counter for the given labels.
func CounterHandle(cv *prometheus.CounterVec, labels ...string) prometheus.Counter {
	k := counterKey{vec: cv, key: labelsKey(labels)}
	if v, ok := counterHandleCache.Load(k); ok {
		return v.(prometheus.Counter)
	}
	c := cv.WithLabelValues(labels...)
	actual, _ := counterHandleCache.LoadOrStore(k, c)
	return actual.(prometheus.Counter)
}

// GaugeHandle returns a cached child gauge for the given labels.
func GaugeHandle(gv *prometheus.GaugeVec, labels ...string) prometheus.Gauge {
	k := gaugeKey{vec: gv, key: labelsKey(labels)}
	if v, ok := gaugeHandleCache.Load(k); ok {
		return v.(prometheus.Gauge)
	}
	g := gv.WithLabelValues(labels...)
	actual, _ := gaugeHandleCache.LoadOrStore(k, g)
	return actual.(prometheus.Gauge)
}

// ObserverHandle returns a cached child observer for the given labels.
func ObserverHandle(hv *prometheus.HistogramVec, labels ...string) prometheus.Observer {
	k := observerKey{vec: hv, key: labelsKey(labels)}
	if v, ok := observerHandleCache.Load(k); ok {
		return v.(prometheus.Observer)
	}
	o := hv.WithLabelValues(labels...)
	actual, _ := observerHandleCache.LoadOrStore(k, o)
	return actual.(prometheus.Observer)
}

// ResetHandleCache clears all handle caches. Call this after re-creating metric Vecs.
func ResetHandleCache() {
	counterHandleCache = sync.Map{}
	gaugeHandleCache = sync.Map{}
	observerHandleCache = sync.Map{}
}
