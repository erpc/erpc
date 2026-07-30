package telemetry

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Caches for label-bound metric handles to avoid per-call Vec map lookups and locks.
// Keyed by the Vec pointer plus the full labels key.

type counterKey struct {
	vec *prometheus.CounterVec
	key string
}

// cachedCounterHandle pairs a child counter with the label tuple that
// created it and an idle timestamp, so SweepIdleCounterHandles can evict
// the cache entry AND release the underlying series via DeleteLabelValues.
// Without the explicit delete, every label combination ever observed stays
// in the Prometheus registry for the process lifetime (append-only model) —
// which is unbounded for label-sets keyed by caller-controlled inputs
// (method, userId, agentName, ...).
type cachedCounterHandle struct {
	counter          prometheus.Counter
	vals             []string
	lastAccessedAtMs atomic.Int64
}

type gaugeKey struct {
	vec *prometheus.GaugeVec
	key string
}

// HistogramObservable is satisfied by both *prometheus.HistogramVec and
// *LabeledHistogram, so ObserverHandle can cache handles for either.
type HistogramObservable interface {
	WithLabelValues(labels ...string) prometheus.Observer
}

type observerKey struct {
	vec any // holds a comparable pointer (*prometheus.HistogramVec or *LabeledHistogram)
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

// CounterHandle returns a cached child counter for the given labels and
// refreshes its idle timestamp. Callers must re-fetch the handle per use
// (CounterHandle(...).Inc()) rather than holding the returned Counter —
// after an idle sweep evicts the series, a held child mutates an object
// that is no longer collected.
func CounterHandle(cv *prometheus.CounterVec, labels ...string) prometheus.Counter {
	k := counterKey{vec: cv, key: labelsKey(labels)}
	nowMs := time.Now().UnixMilli()
	if v, ok := counterHandleCache.Load(k); ok {
		h := v.(*cachedCounterHandle)
		h.lastAccessedAtMs.Store(nowMs)
		return h.counter
	}
	h := &cachedCounterHandle{
		counter: cv.WithLabelValues(labels...),
		vals:    append([]string(nil), labels...),
	}
	h.lastAccessedAtMs.Store(nowMs)
	actual, _ := counterHandleCache.LoadOrStore(k, h)
	ah := actual.(*cachedCounterHandle)
	ah.lastAccessedAtMs.Store(nowMs)
	return ah.counter
}

// DefaultCounterIdleEvictionAfter is the conservative default idle threshold
// for counter series eviction: long enough that only clearly-dead label
// combinations are released. Overridden via metrics.counterIdleEvictionAfter
// (see common.MetricsConfig; wired in erpc/init.go via
// SetCounterIdleEvictionAfter).
const DefaultCounterIdleEvictionAfter = 24 * time.Hour

var counterIdleEvictionAfterMs atomic.Int64

func init() {
	counterIdleEvictionAfterMs.Store(DefaultCounterIdleEvictionAfter.Milliseconds())
}

// SetCounterIdleEvictionAfter overrides the idle threshold for counter series
// eviction. Zero (or negative) disables eviction entirely. Typically called
// once at startup from config.
func SetCounterIdleEvictionAfter(d time.Duration) {
	counterIdleEvictionAfterMs.Store(d.Milliseconds())
}

// SweepIdleCounterHandles evicts cached counter handles idle for at least
// the configured threshold (DefaultCounterIdleEvictionAfter unless overridden)
// and deletes their label-sets from the parent CounterVec, releasing the
// series from the Prometheus registry so /metrics stops re-emitting stale
// label combinations. Driven by the health tracker's idle sweep cadence.
// Returns the number of evicted series; no-op (0) when eviction is disabled.
func SweepIdleCounterHandles() int {
	afterMs := counterIdleEvictionAfterMs.Load()
	if afterMs <= 0 {
		return 0
	}
	return sweepIdleCounterHandlesBefore(time.Now().UnixMilli() - afterMs)
}

// sweepIdleCounterHandlesBefore evicts cached counter handles not touched
// since cutoffMs. Safe to call concurrently with CounterHandle: a racing
// re-fetch recreates the series fresh at zero, which is the same semantics
// as a process restart.
func sweepIdleCounterHandlesBefore(cutoffMs int64) int {
	evicted := 0
	counterHandleCache.Range(func(key, value any) bool {
		h := value.(*cachedCounterHandle)
		if h.lastAccessedAtMs.Load() >= cutoffMs {
			return true
		}
		k := key.(counterKey)
		counterHandleCache.Delete(key)
		k.vec.DeleteLabelValues(h.vals...)
		evicted++
		return true
	})
	return evicted
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
// hv may be *prometheus.HistogramVec or *LabeledHistogram.
//
// When hv is a *LabeledHistogram with active filtering, the cache key uses
// the post-filter label values so multiple full-label tuples that resolve
// to the same underlying observer share a single cache entry.
func ObserverHandle(hv HistogramObservable, labels ...string) prometheus.Observer {
	keyLabels := labels
	if lh, ok := hv.(*LabeledHistogram); ok {
		keyLabels = lh.ActiveLabelValues(labels)
	}
	k := observerKey{vec: hv, key: labelsKey(keyLabels)}
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
