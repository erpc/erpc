package health

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
)

type NetworkMetadata struct {
	evmLatestBlockNumber    int64
	evmFinalizedBlockNumber int64
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

type TrackedMetrics struct {
	Mutex                  sync.RWMutex     `json:"-"`
	LatencySecs            *QuantileTracker `json:"latencySecs"`
	ErrorsTotal            float64          `json:"errorsTotal"`
	SelfRateLimitedTotal   float64          `json:"selfRateLimitedTotal"`
	RemoteRateLimitedTotal float64          `json:"remoteRateLimitedTotal"`
	RequestsTotal          float64          `json:"requestsTotal"`
	BlockHeadLag           float64          `json:"blockHeadLag"`
	FinalizationLag        float64          `json:"finalizationLag"`
	Cordoned               bool             `json:"cordoned"`
	CordonedReason         string           `json:"cordonedReason"`
	LastCollect            time.Time        `json:"lastCollect"`
}

func (m *TrackedMetrics) ErrorRate() float64 {
	if m.RequestsTotal == 0 {
		return 0
	}
	return m.ErrorsTotal / m.RequestsTotal
}

func (m *TrackedMetrics) ThrottledRate() float64 {
	if m.RequestsTotal == 0 {
		return 0
	}
	return (m.RemoteRateLimitedTotal + m.SelfRateLimitedTotal) / m.RequestsTotal
}

type Tracker struct {
	projectId string
	mu        sync.RWMutex

	// ups|network|method -> metrics
	metrics map[string]*TrackedMetrics
	// ups|network -> metadata
	metadata map[string]*NetworkMetadata

	windowSize time.Duration
}

func NewTracker(projectId string, windowSize time.Duration) *Tracker {
	return &Tracker{
		projectId:  projectId,
		metrics:    make(map[string]*TrackedMetrics),
		metadata:   make(map[string]*NetworkMetadata),
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
				t.metrics[key].BlockHeadLag = 0

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
	return ups + common.KeySeparator + network + common.KeySeparator + method
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
				BlockHeadLag:           0,
				LastCollect:            time.Now(),
			}
		}
	}
}

func (t *Tracker) Cordon(ups, network, method, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := t.getKey(ups, network, method)
	log.Debug().Str("upstream", ups).Str("network", network).Str("method", method).Str("reason", reason).Msg("cordoning upstream to disable routing")

	t.metrics[key].Mutex.Lock()
	t.metrics[key].Cordoned = true
	t.metrics[key].CordonedReason = reason
	t.metrics[key].Mutex.Unlock()
}

func (t *Tracker) Uncordon(ups, network, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := t.getKey(ups, network, method)

	t.metrics[key].Mutex.Lock()
	t.metrics[key].Cordoned = false
	t.metrics[key].Mutex.Unlock()
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

func (t *Tracker) SetLatestBlockNumber(ups, network string, blockNumber int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	upsKey := t.getKey(ups, network, "*")
	ntwKey := t.getKey("*", network, "*")

	var ntwMeta *NetworkMetadata
	var upsMeta *NetworkMetadata
	var ok bool

	needsGlobalUpdate := false

	// Update network-level highest block head
	if ntwMeta, ok = t.metadata[ntwKey]; !ok {
		ntwMeta = &NetworkMetadata{
			evmLatestBlockNumber: blockNumber,
		}
		t.metadata[ntwKey] = ntwMeta
	} else {
		if ntwMeta.evmLatestBlockNumber < blockNumber {
			ntwMeta.evmLatestBlockNumber = blockNumber
			needsGlobalUpdate = true
		}
	}

	// Update block head for this upstream
	if upsMeta, ok = t.metadata[upsKey]; !ok {
		upsMeta = &NetworkMetadata{
			evmLatestBlockNumber: blockNumber,
		}
		t.metadata[upsKey] = upsMeta
	} else {
		upsMeta.evmLatestBlockNumber = blockNumber
	}

	upsLag := float64(ntwMeta.evmLatestBlockNumber - upsMeta.evmLatestBlockNumber)
	MetricUpstreamBlockHeadLag.WithLabelValues(t.projectId, network, ups).Set(upsLag)

	if needsGlobalUpdate {
		// Loop over all metrics and update items for all upstreams of this network (all their methods),
		// because the common reference value (highest latest block) has changed.
		for key, mt := range t.metrics {
			parts := strings.SplitN(key, common.KeySeparator, 3)
			if parts[0] != "*" {
				if parts[1] == network {
					if parts[0] == ups {
						mt.Mutex.Lock()
						mt.BlockHeadLag = upsLag
						mt.Mutex.Unlock()
					} else {
						otherUpsMeta, ok := t.metadata[t.getKey(parts[0], network, "*")]
						if ok {
							otherUpsLag := float64(ntwMeta.evmLatestBlockNumber - otherUpsMeta.evmLatestBlockNumber)
							mt.Mutex.Lock()
							mt.BlockHeadLag = otherUpsLag
							MetricUpstreamBlockHeadLag.WithLabelValues(t.projectId, network, parts[0]).Set(otherUpsLag)
							mt.Mutex.Unlock()
						}
					}
				}
			}
		}
	} else {
		// Only update items for this upstream and this network (all their methods)
		prefix := ups + common.KeySeparator + network + common.KeySeparator
		for key, mt := range t.metrics {
			if strings.HasPrefix(key, prefix) {
				mt.Mutex.Lock()
				mt.BlockHeadLag = upsLag
				mt.Mutex.Unlock()
			}
		}
	}
}

func (t *Tracker) SetFinalizedBlockNumber(ups, network string, blockNumber int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	upsKey := t.getKey(ups, network, "*")
	ntwKey := t.getKey("*", network, "*")

	var ntwMeta *NetworkMetadata
	var upsMeta *NetworkMetadata
	var ok bool

	needsGlobalUpdate := false

	// Update network-level highest block head
	if ntwMeta, ok = t.metadata[ntwKey]; !ok {
		ntwMeta = &NetworkMetadata{
			evmFinalizedBlockNumber: blockNumber,
		}
		t.metadata[ntwKey] = ntwMeta
	} else {
		if ntwMeta.evmFinalizedBlockNumber < blockNumber {
			ntwMeta.evmFinalizedBlockNumber = blockNumber
			needsGlobalUpdate = true
		}
	}

	// Update block head for this upstream
	if upsMeta, ok = t.metadata[upsKey]; !ok {
		upsMeta = &NetworkMetadata{
			evmFinalizedBlockNumber: blockNumber,
		}
		t.metadata[upsKey] = upsMeta
	} else {
		upsMeta.evmFinalizedBlockNumber = blockNumber
	}

	upsLag := float64(ntwMeta.evmFinalizedBlockNumber - upsMeta.evmFinalizedBlockNumber)
	MetricUpstreamFinalizationLag.WithLabelValues(t.projectId, network, ups).Set(upsLag)

	if needsGlobalUpdate {
		// Loop over all metrics and update items for all upstreams of this network (all their methods),
		// because the common reference value (highest latest block) has changed.
		for key, mt := range t.metrics {
			parts := strings.SplitN(key, common.KeySeparator, 3)
			if parts[0] != "*" {
				if parts[1] == network {
					if parts[0] == ups {
						mt.Mutex.Lock()
						mt.FinalizationLag = upsLag
						mt.Mutex.Unlock()
					} else {
						otherUpsMeta, ok := t.metadata[t.getKey(parts[0], network, "*")]
						if ok {
							otherUpsLag := float64(ntwMeta.evmFinalizedBlockNumber - otherUpsMeta.evmFinalizedBlockNumber)
							mt.Mutex.Lock()
							mt.FinalizationLag = otherUpsLag
							MetricUpstreamFinalizationLag.WithLabelValues(t.projectId, network, parts[0]).Set(otherUpsLag)
							mt.Mutex.Unlock()
						}
					}
				}
			}
		}
	} else {
		// Only update items for this upstream and this network (all their methods)
		prefix := ups + common.KeySeparator + network + common.KeySeparator
		for key, mt := range t.metrics {
			if strings.HasPrefix(key, prefix) {
				mt.Mutex.Lock()
				mt.FinalizationLag = upsLag
				mt.Mutex.Unlock()
			}
		}
	}
}

func (t *Tracker) GetUpstreamMetrics(upsId string) map[string]*TrackedMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// network:method -> metrics
	var result = make(map[string]*TrackedMetrics)

	for key, value := range t.metrics {
		if strings.HasPrefix(key, upsId) {
			result[strings.TrimPrefix(key, upsId+common.KeySeparator)] = value
		}
	}

	return result
}
