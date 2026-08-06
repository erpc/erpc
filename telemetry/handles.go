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
	vec any // holds a comparable pointer (*prometheus.CounterVec or *LabeledCounter)
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

// CounterIncrementable is satisfied by both *prometheus.CounterVec and
// *LabeledCounter, so CounterHandle can cache handles for either.
type CounterIncrementable interface {
	WithLabelValues(labels ...string) prometheus.Counter
	DeleteLabelValues(labels ...string) bool
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

// counterHandleCreateMu serializes CounterHandle's miss path (create +
// cache-store) against sweep eviction (cache-delete + DeleteLabelValues).
// Without it, a miss between the sweep's cache delete and its registry
// delete would re-cache the still-registered child right before the sweep
// removes it from the vec — leaving a hot cached handle whose increments
// are never scraped again. The hit path stays lock-free: misses happen once
// per label tuple per (re)birth, so the lock is off the hot path.
var counterHandleCreateMu sync.Mutex

// CounterHandle returns a cached child counter for the given labels and
// refreshes its idle timestamp. Callers must re-fetch the handle per use
// (CounterHandle(...).Inc()) rather than holding the returned Counter —
// after an idle sweep evicts the series, a held child mutates an object
// that is no longer collected.
func CounterHandle(cv CounterIncrementable, labels ...string) prometheus.Counter {
	// Key on the POST-filter labels. Under a CounterLabelFilter several
	// full-schema tuples collapse onto one underlying series; keying on the
	// full tuple would create one cache entry per tuple sharing that series,
	// and the idle sweep would then delete a series another live entry is
	// still incrementing. Projecting first keeps it one entry per series.
	keyLabels := labels
	if lc, ok := cv.(*LabeledCounter); ok {
		keyLabels = lc.ActiveLabelValues(labels)
	}
	k := counterKey{vec: cv, key: labelsKey(keyLabels)}
	nowMs := time.Now().UnixMilli()
	if v, ok := counterHandleCache.Load(k); ok {
		h := v.(*cachedCounterHandle)
		h.lastAccessedAtMs.Store(nowMs)
		return h.counter
	}
	counterHandleCreateMu.Lock()
	defer counterHandleCreateMu.Unlock()
	// Re-check under the lock: another miss may have created the entry, or
	// a sweep may have just evicted it (in which case WithLabelValues below
	// creates a fresh series — never a doomed one, since eviction holds the
	// same lock across cache-delete + registry-delete).
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
	counterHandleCache.Store(k, h)
	return h.counter
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
// since cutoffMs. Cache delete and registry delete happen atomically with
// respect to CounterHandle's miss path (counterHandleCreateMu), so a racing
// re-fetch either lands before eviction (refreshing the timestamp, which the
// under-lock re-check honors) or after it (recreating the series fresh at
// zero — the same semantics as a process restart). A hit-path caller that
// obtained the child just before eviction may lose the increments of that
// single use; bounded, restart-equivalent.
func sweepIdleCounterHandlesBefore(cutoffMs int64) int {
	evicted := 0
	counterHandleCache.Range(func(key, value any) bool {
		h := value.(*cachedCounterHandle)
		if h.lastAccessedAtMs.Load() >= cutoffMs {
			return true
		}
		counterHandleCreateMu.Lock()
		// Re-check under the lock: a miss-path fetch may have refreshed or
		// replaced the entry since the lock-free check above.
		v, ok := counterHandleCache.Load(key)
		if !ok || v.(*cachedCounterHandle).lastAccessedAtMs.Load() >= cutoffMs {
			counterHandleCreateMu.Unlock()
			return true
		}
		k := key.(counterKey)
		counterHandleCache.Delete(key)
		k.vec.(CounterIncrementable).DeleteLabelValues(v.(*cachedCounterHandle).vals...)
		counterHandleCreateMu.Unlock()
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
