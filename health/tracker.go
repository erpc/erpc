package health

import (
	"context"
	"sync"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

type TrackedMetrics struct {
	P90LatencySecs         float64   `json:"p90LatencySecs"`
	ErrorsTotal            float64   `json:"errorsTotal"`
	SelfRateLimitedTotal   float64   `json:"selfRateLimitedTotal"`
	RemoteRateLimitedTotal float64   `json:"remoteRateLimitedTotal"`
	RequestsTotal          float64   `json:"requestsTotal"`
	LastCollect            time.Time `json:"lastCollect"`
}

type Tracker struct {
	projectId string

	mu        sync.RWMutex
	upstreams map[string]common.Upstream

	// a map of upstream -> network (or *) -> method (or *) -> metrics
	windowMetrics map[string]map[string]map[string]*TrackedMetrics
	windowSize    time.Duration

	prometheusRefreshInterval time.Duration
	cachedMetrics             []*dto.MetricFamily
	cachedMetricsmu           sync.RWMutex
}

var metricRequestTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "erpc",
	Name:      "upstream_request_total",
	Help:      "Total number of requests to upstreams in the current window.",
}, []string{"project", "network", "upstream", "method"})

var metricRequestDuration = promauto.NewSummaryVec(prometheus.SummaryOpts{
	Namespace: "erpc",
	Name:      "upstream_request_duration_seconds",
	Help:      "Duration of requests to upstreams.",
	Objectives: map[float64]float64{
		0.5:  0.05,
		0.9:  0.01,
		0.99: 0.001,
	},
}, []string{"project", "network", "upstream", "method"})

var metricErrorTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "erpc",
	Name:      "upstream_request_errors_total",
	Help:      "Total number of errors for requests to upstreams in the current window.",
}, []string{"project", "network", "upstream", "method", "error"})

var metricSelfRateLimitedTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "erpc",
	Name:      "upstream_request_self_rate_limited_total",
	Help:      "Total number of self-imposed rate limited requests to upstreams in the current window.",
}, []string{"project", "network", "upstream", "method"})

var metricRemoteRateLimitedTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "erpc",
	Name:      "upstream_request_remote_rate_limited_total",
	Help:      "Total number of remote rate limited requests by upstreams in the current window.",
}, []string{"project", "network", "upstream", "method"})

func NewTracker(projectId string, windowSize, prometheusRefreshInterval time.Duration) *Tracker {
	t := &Tracker{
		projectId:                 projectId,
		upstreams:                 make(map[string]common.Upstream),
		windowMetrics:             make(map[string]map[string]map[string]*TrackedMetrics),
		windowSize:                windowSize,
		prometheusRefreshInterval: prometheusRefreshInterval,
	}

	return t
}

func (t *Tracker) Bootstrap(ctx context.Context) {
	go t.updatePrometheusMetrics(ctx)
	go t.refreshCachedMetrics(ctx)
}

func (t *Tracker) updatePrometheusMetrics(ctx context.Context) {
	ticker := time.NewTicker(t.windowSize)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.mu.Lock()
			for _, upsList := range t.windowMetrics {
				for _, methods := range upsList {
					for _, metrics := range methods {
						if metrics == nil {
							continue
						}
						metrics.ErrorsTotal = 0
						metrics.RequestsTotal = 0
						metrics.SelfRateLimitedTotal = 0
						metrics.RemoteRateLimitedTotal = 0
						// Update Prometheus metrics
						// metricRequestTotal.WithLabelValues(t.projectId, network, ups, method).Set(metrics.RequestsTotal)
						// metricErrorTotal.WithLabelValues(t.projectId, network, ups, method, "all").Set(metrics.ErrorsTotal)
						// metricSelfRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Set(metrics.SelfRateLimitedTotal)
						// metricRemoteRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Set(metrics.RemoteRateLimitedTotal)
					}
				}
			}
			// t.windowMetrics = make(map[string]map[string]map[string]*TrackedMetrics)
			t.mu.Unlock()
		}
	}
}

func (t *Tracker) refreshCachedMetrics(ctx context.Context) {
	ticker := time.NewTicker(t.prometheusRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// TODO find a cheaper way especially when there are many projects
			mfs, err := prometheus.DefaultGatherer.Gather()
			if err == nil {
				t.cachedMetricsmu.Lock()
				t.cachedMetrics = mfs
				t.cachedMetricsmu.Unlock()
			}
		}
	}
}

func (t *Tracker) RegisterUpstream(ups common.Upstream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.upstreams[ups.Config().Id] = ups
}

func (t *Tracker) ensureMetricsInitialized(network, ups, method string) {
	if _, ok := t.windowMetrics[ups]; !ok {
		t.windowMetrics[ups] = make(map[string]map[string]*TrackedMetrics)
	}

	if _, ok := t.windowMetrics[ups][network]; !ok {
		t.windowMetrics[ups][network] = make(map[string]*TrackedMetrics)
	}
	if _, ok := t.windowMetrics[ups][network][method]; !ok {
		t.windowMetrics[ups][network][method] = &TrackedMetrics{}
	}
	if _, ok := t.windowMetrics[ups][network]["*"]; !ok {
		t.windowMetrics[ups][network]["*"] = &TrackedMetrics{}
	}

	if _, ok := t.windowMetrics[ups]["*"]; !ok {
		t.windowMetrics[ups]["*"] = make(map[string]*TrackedMetrics)
	}
	if _, ok := t.windowMetrics[ups]["*"][method]; !ok {
		t.windowMetrics[ups]["*"][method] = &TrackedMetrics{}
	}
	if _, ok := t.windowMetrics[ups]["*"]["*"]; !ok {
		t.windowMetrics[ups]["*"]["*"] = &TrackedMetrics{}
	}
}

func (t *Tracker) RecordUpstreamRequest(network, ups, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.upstreams[ups]; !ok {
		return
	}

	t.ensureMetricsInitialized(network, ups, method)
	t.incrementMetric(network, ups, method, "RequestsTotal", 1)
	metricRequestTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) RecordUpstreamDurationStart(network, ups, method string) *prometheus.Timer {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(network, ups, method)
	return prometheus.NewTimer(metricRequestDuration.WithLabelValues(t.projectId, network, ups, method))
}

func (t *Tracker) RecordUpstreamFailure(network, ups, method, errorType string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(network, ups, method)
	t.incrementMetric(network, ups, method, "ErrorsTotal", 1)
	metricErrorTotal.WithLabelValues(t.projectId, network, ups, method, errorType).Inc()
}

func (t *Tracker) RecordUpstreamSelfRateLimited(network, ups, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(network, ups, method)
	t.incrementMetric(network, ups, method, "SelfRateLimitedTotal", 1)
	metricSelfRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) RecordUpstreamRemoteRateLimited(network, ups, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(network, ups, method)
	t.incrementMetric(network, ups, method, "RemoteRateLimitedTotal", 1)
	metricRemoteRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) incrementMetric(network, ups, method, metricName string, value float64) {
	t.incrementMetricHelper(t.windowMetrics, network, ups, method, metricName, value)
	t.incrementMetricHelper(t.windowMetrics, network, ups, "*", metricName, value)
	t.incrementMetricHelper(t.windowMetrics, ups, "*", "*", metricName, value)
}

func (t *Tracker) incrementMetricHelper(metrics map[string]map[string]map[string]*TrackedMetrics, network, ups, method, metricName string, value float64) {
	if m, ok := metrics[ups][network][method]; ok {
		switch metricName {
		case "RequestsTotal":
			m.RequestsTotal += value
		case "ErrorsTotal":
			m.ErrorsTotal += value
		case "SelfRateLimitedTotal":
			m.SelfRateLimitedTotal += value
		case "RemoteRateLimitedTotal":
			m.RemoteRateLimitedTotal += value
		}
	}
}

func (t *Tracker) GetUpstreamMethodMetrics(network, ups, method string) *TrackedMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// If we have data for the method on this specific network, use it
	if metrics, ok := t.windowMetrics[ups][network][method]; ok {
		return t.calculateMetrics(network, ups, method, metrics)
	}

	// If we have data for any method on the network, use it
	if metrics, ok := t.windowMetrics[ups][network]["*"]; ok {
		return t.calculateMetrics(network, ups, "*", metrics)
	}

	// If we have data for the method any network of this upstream, use it
	if metrics, ok := t.windowMetrics[ups]["*"][method]; ok {
		return t.calculateMetrics("*", ups, method, metrics)
	}

	// If we have data for any method and any network of this upstream, use it
	if metrics, ok := t.windowMetrics[ups]["*"]["*"]; ok {
		return t.calculateMetrics("*", ups, "*", metrics)
	}

	return &TrackedMetrics{
		RequestsTotal:          0,
		ErrorsTotal:            0,
		SelfRateLimitedTotal:   0,
		RemoteRateLimitedTotal: 0,
		LastCollect:            time.Now(),
	}
}

func (t *Tracker) calculateMetrics(network, ups, method string, m *TrackedMetrics) *TrackedMetrics {
	result := &TrackedMetrics{
		RequestsTotal:          m.RequestsTotal,
		ErrorsTotal:            m.ErrorsTotal,
		SelfRateLimitedTotal:   m.SelfRateLimitedTotal,
		RemoteRateLimitedTotal: m.RemoteRateLimitedTotal,
		LastCollect:            time.Now(),
	}

	t.cachedMetricsmu.RLock()
	defer t.cachedMetricsmu.RUnlock()

	for _, mf := range t.cachedMetrics {
		if mf.GetName() == "erpc_upstream_request_duration_seconds" {
			for _, m := range mf.GetMetric() {
				if m.GetLabel() != nil {
					match := true
					for _, l := range m.GetLabel() {
						switch l.GetName() {
						case "project":
							if l.GetValue() != t.projectId {
								match = false
							}
						case "network":
							if l.GetValue() != network {
								match = false
							}
						case "upstream":
							if l.GetValue() != ups {
								match = false
							}
						case "method":
							if l.GetValue() != method {
								match = false
							}
						}
					}
					if match {
						summary := m.GetSummary()
						if summary != nil {
							for _, q := range summary.GetQuantile() {
								if q.GetQuantile() == 0.9 {
									result.P90LatencySecs = q.GetValue()
									return result
								}
							}
						}
						break
					}
				}
			}
			break
		}
	}

	return result
}
