package health

import (
	"context"
	"strings"
	"sync"
	"time"
)

type TrackedMetrics struct {
	Mutex                  sync.RWMutex     `json:"-"`
	LatencySecs            *QuantileTracker `json:"latencySecs"`
	ErrorsTotal            float64          `json:"errorsTotal"`
	SelfRateLimitedTotal   float64          `json:"selfRateLimitedTotal"`
	RemoteRateLimitedTotal float64          `json:"remoteRateLimitedTotal"`
	RequestsTotal          float64          `json:"requestsTotal"`
	LastCollect            time.Time        `json:"lastCollect"`
}

type Timer struct {
	start   time.Time
	network string
	ups     string
	method  string
	tracker *Tracker
}

func (t *Timer) ObserveDuration() {
	duration := time.Since(t.start)
	t.tracker.RecordUpstreamDuration(t.ups, t.network, t.method, duration)
}

type Tracker struct {
	projectId  string
	mu         sync.RWMutex
	metrics    map[string]*TrackedMetrics
	windowSize time.Duration
}

func NewTracker(projectId string, windowSize time.Duration) *Tracker {
	return &Tracker{
		projectId:  projectId,
		metrics:    make(map[string]*TrackedMetrics),
		windowSize: windowSize,
	}
}

func (t *Tracker) Bootstrap(ctx context.Context) {
	go t.resetMetrics(ctx)
}

func (t *Tracker) resetMetrics(ctx context.Context) {
	ticker := time.NewTicker(t.windowSize)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.mu.Lock()
			for key := range t.metrics {
				t.metrics[key].Mutex.Lock()
				t.metrics[key].ErrorsTotal = 0
				t.metrics[key].RequestsTotal = 0
				t.metrics[key].SelfRateLimitedTotal = 0
				t.metrics[key].RemoteRateLimitedTotal = 0
				t.metrics[key].LatencySecs.Reset()
				t.metrics[key].LastCollect = time.Now()
				t.metrics[key].Mutex.Unlock()
			}
			t.mu.Unlock()
		}
	}
}

func (t *Tracker) getKeys(ups, network, method string) []string {
	return []string{
		t.getKey(ups, network, method),
		t.getKey(ups, network, "*"),
		t.getKey(ups, "*", method),
		t.getKey(ups, "*", "*"),
	}
}

func (t *Tracker) getKey(ups, network, method string) string {
	return ups + ":" + network + ":" + method
}

func (t *Tracker) ensureMetricsInitialized(ups, network, method string) {
	for _, key := range t.getKeys(ups, network, method) {
		if _, ok := t.metrics[key]; !ok {
			t.metrics[key] = &TrackedMetrics{
				LatencySecs:            NewQuantileTracker(t.windowSize),
				ErrorsTotal:            0,
				SelfRateLimitedTotal:   0,
				RemoteRateLimitedTotal: 0,
				RequestsTotal:          0,
				LastCollect:            time.Now(),
			}
		}
	}
}

func (t *Tracker) RecordUpstreamRequest(ups, network, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(ups, network, method)
	for _, key := range t.getKeys(ups, network, method) {
		t.metrics[key].Mutex.Lock()
		t.metrics[key].RequestsTotal++
		t.metrics[key].Mutex.Unlock()
	}

	MetricUpstreamRequestTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) RecordUpstreamDurationStart(ups, network, method string) *Timer {
	return &Timer{
		start:   time.Now(),
		network: network,
		ups:     ups,
		method:  method,
		tracker: t,
	}
}

func (t *Tracker) RecordUpstreamDuration(ups, network, method string, duration time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(ups, network, method)
	for _, key := range t.getKeys(ups, network, method) {
		t.metrics[key].LatencySecs.Add(duration.Seconds())
	}

	MetricUpstreamRequestDuration.WithLabelValues(t.projectId, network, ups, method).Observe(duration.Seconds())
}

func (t *Tracker) RecordUpstreamFailure(ups, network, method, errorType string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(ups, network, method)
	for _, key := range t.getKeys(ups, network, method) {
		t.metrics[key].Mutex.Lock()
		t.metrics[key].ErrorsTotal++
		t.metrics[key].Mutex.Unlock()
	}

	MetricUpstreamErrorTotal.WithLabelValues(t.projectId, network, ups, method, errorType).Inc()
}

func (t *Tracker) RecordUpstreamSelfRateLimited(ups, network, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(ups, network, method)
	for _, key := range t.getKeys(ups, network, method) {
		t.metrics[key].Mutex.Lock()
		t.metrics[key].SelfRateLimitedTotal++
		t.metrics[key].Mutex.Unlock()
	}

	MetricUpstreamSelfRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) RecordUpstreamRemoteRateLimited(ups, network, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ensureMetricsInitialized(ups, network, method)
	for _, key := range t.getKeys(ups, network, method) {
		t.metrics[key].Mutex.Lock()
		t.metrics[key].RemoteRateLimitedTotal++
		t.metrics[key].Mutex.Unlock()
	}

	MetricUpstreamRemoteRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) GetUpstreamMethodMetrics(ups, network, method string) *TrackedMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()

	key := t.getKey(ups, network, method)
	metrics, ok := t.metrics[key]
	if !ok {
		t.ensureMetricsInitialized(ups, network, method)
		metrics = t.metrics[key]
	}

	return metrics
}

func (t *Tracker) GetUpstreamMetrics(upsId string) map[string]*TrackedMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// network:method -> metrics
	var result = make(map[string]*TrackedMetrics)

	for key, value := range t.metrics {
		if strings.HasPrefix(key, upsId) {
			result[strings.TrimPrefix(key, upsId+":")] = value
		}
	}

	return result
}
