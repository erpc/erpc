package headcache

import (
	"github.com/erpc/erpc/telemetry"
)

// metrics mirrors Stats onto registered Prometheus families. It is optional:
// a Cache without EnableMetrics has a nil *metrics and every method no-ops, so
// the cache's behavior is identical with or without it. All updates are
// in-process (no store reads) and happen at the same sites as Stats.
type metrics struct {
	project, network string
}

// EnableMetrics labels this cache's Prometheus series with project and the
// network's Network.Label(). Call once, before Start.
func (c *Cache) EnableMetrics(project, network string) {
	c.metrics = &metrics{project: project, network: network}
	c.metrics.subscribers(0)
	c.metrics.leader(false)
}

func (m *metrics) request(hit bool) {
	if m == nil {
		return
	}
	r := "miss"
	if hit {
		r = "hit"
	}
	telemetry.CounterHandle(telemetry.MetricHeadCacheRequestsTotal, m.project, m.network, r).Inc()
}

func (m *metrics) fetch(ok bool) {
	if m == nil {
		return
	}
	r := "rejected"
	if ok {
		r = "ok"
	}
	telemetry.CounterHandle(telemetry.MetricHeadCacheFetchesTotal, m.project, m.network, r).Inc()
}

func (m *metrics) reorg() {
	if m != nil {
		telemetry.MetricHeadCacheReorgsTotal.WithLabelValues(m.project, m.network).Inc()
	}
}

func (m *metrics) published() {
	if m != nil {
		telemetry.MetricHeadCachePublishedTotal.WithLabelValues(m.project, m.network).Inc()
	}
}

func (m *metrics) acquired() {
	if m != nil {
		telemetry.MetricHeadCacheLeaderEpochsTotal.WithLabelValues(m.project, m.network).Inc()
	}
}

func (m *metrics) leader(held bool) {
	if m == nil {
		return
	}
	v := 0.0
	if held {
		v = 1
	}
	telemetry.MetricHeadCacheLeader.WithLabelValues(m.project, m.network).Set(v)
}

func (m *metrics) snapshot(s *Snapshot, bytes int64) {
	if m == nil {
		return
	}
	telemetry.MetricHeadCacheBytes.WithLabelValues(m.project, m.network).Set(float64(bytes))
	if s == nil {
		telemetry.MetricHeadCacheHead.DeleteLabelValues(m.project, m.network)
		telemetry.MetricHeadCacheSnapshotTimestamp.DeleteLabelValues(m.project, m.network)
		return
	}
	telemetry.MetricHeadCacheHead.WithLabelValues(m.project, m.network).Set(float64(s.Head))
	if !s.At.IsZero() {
		telemetry.MetricHeadCacheSnapshotTimestamp.WithLabelValues(m.project, m.network).Set(float64(s.At.UnixNano()) / 1e9)
	}
}

func (m *metrics) subscribers(n int) {
	if m != nil {
		telemetry.MetricHeadCacheSubscribers.WithLabelValues(m.project, m.network).Set(float64(n))
	}
}

func (m *metrics) subClosed(reason string) {
	if m != nil {
		telemetry.CounterHandle(telemetry.MetricHeadCacheSubscriberClosedTotal, m.project, m.network, reason).Inc()
	}
}

// stop removes this cache's gauge series so a stopped (or restarted) network
// never reports a stale leader/head/subscriber value. Counters stay monotonic.
func (m *metrics) stop() {
	if m == nil {
		return
	}
	for _, g := range []interface{ DeleteLabelValues(...string) bool }{
		telemetry.MetricHeadCacheLeader,
		telemetry.MetricHeadCacheHead,
		telemetry.MetricHeadCacheBytes,
		telemetry.MetricHeadCacheSnapshotTimestamp,
		telemetry.MetricHeadCacheSubscribers,
	} {
		g.DeleteLabelValues(m.project, m.network)
	}
}
