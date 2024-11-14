package health

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	// "sync"
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
	// Mutex                  sync.RWMutex     `json:"-"`
	LatencySecs            *QuantileTracker `json:"latencySecs"`
	ErrorsTotal            atomic.Int64     `json:"errorsTotal"`
	SelfRateLimitedTotal   atomic.Int64     `json:"selfRateLimitedTotal"`
	RemoteRateLimitedTotal atomic.Int64     `json:"remoteRateLimitedTotal"`
	RequestsTotal          atomic.Int64     `json:"requestsTotal"`
	BlockHeadLag           atomic.Int64     `json:"blockHeadLag"`
	FinalizationLag        atomic.Int64     `json:"finalizationLag"`
	Cordoned               atomic.Bool      `json:"cordoned"`
	CordonedReason         string           `json:"cordonedReason"`
}

func (m *TrackedMetrics) ErrorRate() float64 {
	// m.Mutex.RLock()
	// defer m.Mutex.RUnlock()

	if m.RequestsTotal.Load() == 0 {
		return 0
	}
	return float64(m.ErrorsTotal.Load()) / float64(m.RequestsTotal.Load())
}

func (m *TrackedMetrics) ThrottledRate() float64 {
	// m.Mutex.RLock()
	// defer m.Mutex.RUnlock()

	if m.RequestsTotal.Load() == 0 {
		return 0
	}
	return (float64(m.RemoteRateLimitedTotal.Load()) + float64(m.SelfRateLimitedTotal.Load())) / float64(m.RequestsTotal.Load())
}

func (mt *TrackedMetrics) Reset() {
	mt.ErrorsTotal.Store(0)
	mt.RequestsTotal.Store(0)
	mt.SelfRateLimitedTotal.Store(0)
	mt.RemoteRateLimitedTotal.Store(0)
	mt.LatencySecs.Reset()
	mt.BlockHeadLag.Store(0)
}

type Tracker struct {
	projectId string

	// ups|network|method -> metrics
	metrics sync.Map // map[string]*TrackedMetrics
	// ups|network -> metadata
	metadata sync.Map // map[string]*NetworkMetadata

	windowSize time.Duration
}

func NewTracker(projectId string, windowSize time.Duration) *Tracker {
	return &Tracker{
		projectId:  projectId,
		metrics:    sync.Map{},
		metadata:   sync.Map{},
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
			t.metrics.Range(func(key, value interface{}) bool {
				value.(*TrackedMetrics).Reset()
				return true
			})
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

func (t *Tracker) getMetrics(key string) *TrackedMetrics {
	metrics, _ := t.metrics.LoadOrStore(key, &TrackedMetrics{
		LatencySecs: NewQuantileTracker(t.windowSize),
	})
	return metrics.(*TrackedMetrics)
}

func (t *Tracker) getMetadata(key string) *NetworkMetadata {
	metadata, _ := t.metadata.LoadOrStore(key, &NetworkMetadata{})
	return metadata.(*NetworkMetadata)
}

func (t *Tracker) Cordon(ups, network, method, reason string) {
	log.Debug().Str("upstream", ups).Str("network", network).Str("method", method).Str("reason", reason).Msg("cordoning upstream to disable routing")

	metrics := t.getMetrics(t.getKey(ups, network, method))
	metrics.Cordoned.Store(true)
	metrics.CordonedReason = reason
}

func (t *Tracker) Uncordon(ups, network, method string) {
	metrics := t.getMetrics(t.getKey(ups, network, method))
	metrics.Cordoned.Store(false)
}

func (t *Tracker) RecordUpstreamRequest(ups, network, method string) {
	metricsList := make([]*TrackedMetrics, 0)
	for _, key := range t.getKeys(ups, network, method) {
		metricsList = append(metricsList, t.getMetrics(key))
	}

	for _, metrics := range metricsList {
		metrics.RequestsTotal.Add(1)
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
	metricsList := make([]*TrackedMetrics, 0)
	for _, key := range t.getKeys(ups, network, method) {
		metricsList = append(metricsList, t.getMetrics(key))
	}

	for _, metrics := range metricsList {
		metrics.LatencySecs.Add(duration.Seconds())
	}

	MetricUpstreamRequestDuration.WithLabelValues(t.projectId, network, ups, method).Observe(duration.Seconds())
}

func (t *Tracker) RecordUpstreamFailure(ups, network, method, errorType string) {
	metricsList := make([]*TrackedMetrics, 0)
	for _, key := range t.getKeys(ups, network, method) {
		metricsList = append(metricsList, t.getMetrics(key))
	}

	for _, metrics := range metricsList {
		metrics.ErrorsTotal.Add(1)
	}

	MetricUpstreamErrorTotal.WithLabelValues(t.projectId, network, ups, method, errorType).Inc()
}

func (t *Tracker) RecordUpstreamSelfRateLimited(ups, network, method string) {
	for _, key := range t.getKeys(ups, network, method) {
		t.getMetrics(key).SelfRateLimitedTotal.Add(1)
	}

	MetricUpstreamSelfRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) RecordUpstreamRemoteRateLimited(ups, network, method string) {
	for _, key := range t.getKeys(ups, network, method) {
		t.getMetrics(key).RemoteRateLimitedTotal.Add(1)
	}

	MetricUpstreamRemoteRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) GetUpstreamMethodMetrics(ups, network, method string) *TrackedMetrics {
	return t.getMetrics(t.getKey(ups, network, method))
}

func (t *Tracker) SetLatestBlockNumber(ups, network string, blockNumber int64) {
	upsKey := t.getKey(ups, network, "*")
	ntwKey := t.getKey("*", network, "*")

	needsGlobalUpdate := false

	// Update network-level highest block head
	ntwMeta := t.getMetadata(ntwKey)
	if ntwMeta.evmLatestBlockNumber < blockNumber {
		ntwMeta.evmLatestBlockNumber = blockNumber
		needsGlobalUpdate = true
	}

	// Update block head for this upstream
	upsMeta := t.getMetadata(upsKey)
	if upsMeta.evmLatestBlockNumber < blockNumber {
		upsMeta.evmLatestBlockNumber = blockNumber
	}

	upsLag := ntwMeta.evmLatestBlockNumber - upsMeta.evmLatestBlockNumber
	MetricUpstreamBlockHeadLag.WithLabelValues(t.projectId, network, ups).Set(float64(upsLag))

	if needsGlobalUpdate {
		// Loop over all metrics and update items for all upstreams of this network (all their methods),
		// because the common reference value (highest latest block) has changed.
		t.metrics.Range(func(k, v interface{}) bool {
			key := k.(string)
			mt := v.(*TrackedMetrics)
			parts := strings.SplitN(key, common.KeySeparator, 3)
			if parts[0] != "*" {
				if parts[1] == network {
					if parts[0] == ups {
						mt.BlockHeadLag.Store(upsLag)
					} else {
						otherUpsMeta := t.getMetadata(t.getKey(parts[0], network, "*"))
						otherUpsLag := ntwMeta.evmLatestBlockNumber - otherUpsMeta.evmLatestBlockNumber
						mt.BlockHeadLag.Store(otherUpsLag)
						MetricUpstreamBlockHeadLag.WithLabelValues(t.projectId, network, parts[0]).Set(float64(otherUpsLag))
					}
				}
			}
			return true
		})
	} else {
		// Only update items for this upstream and this network (all their methods)
		prefix := ups + common.KeySeparator + network + common.KeySeparator
		t.metrics.Range(func(k, v interface{}) bool {
			key := k.(string)
			mt := v.(*TrackedMetrics)
			if strings.HasPrefix(key, prefix) {
				mt.BlockHeadLag.Store(upsLag)
			}
			return true
		})
	}
}

func (t *Tracker) SetFinalizedBlockNumber(ups, network string, blockNumber int64) {
	upsKey := t.getKey(ups, network, "*")
	ntwKey := t.getKey("*", network, "*")

	var ntwMeta *NetworkMetadata
	var upsMeta *NetworkMetadata

	needsGlobalUpdate := false

	// Update network-level highest block head
	if val, ok := t.metadata.Load(ntwKey); !ok {
		ntwMeta = &NetworkMetadata{
			evmFinalizedBlockNumber: blockNumber,
		}
		t.metadata.Store(ntwKey, ntwMeta)
	} else {
		ntwMeta = val.(*NetworkMetadata)
		if ntwMeta.evmFinalizedBlockNumber < blockNumber {
			ntwMeta.evmFinalizedBlockNumber = blockNumber
			needsGlobalUpdate = true
		}
	}

	// Update block head for this upstream
	upsMeta = t.getMetadata(upsKey)
	if upsMeta.evmFinalizedBlockNumber < blockNumber {
		upsMeta.evmFinalizedBlockNumber = blockNumber
	}

	upsLag := ntwMeta.evmFinalizedBlockNumber - upsMeta.evmFinalizedBlockNumber
	MetricUpstreamFinalizationLag.WithLabelValues(t.projectId, network, ups).Set(float64(upsLag))

	if needsGlobalUpdate {
		// Loop over all metrics and update items for all upstreams of this network (all their methods),
		// because the common reference value (highest latest block) has changed.
		t.metrics.Range(func(k, v interface{}) bool {
			key := k.(string)
			mt := v.(*TrackedMetrics)
			parts := strings.SplitN(key, common.KeySeparator, 3)
			if parts[0] != "*" {
				if parts[1] == network {
					if parts[0] == ups {
						mt.FinalizationLag.Store(upsLag)
					} else {
						if ov, ok := t.metadata.Load(t.getKey(parts[0], network, "*")); ok {
							otherUpsMeta := ov.(*NetworkMetadata)
							otherUpsLag := ntwMeta.evmFinalizedBlockNumber - otherUpsMeta.evmFinalizedBlockNumber
							mt.FinalizationLag.Store(otherUpsLag)
							MetricUpstreamFinalizationLag.WithLabelValues(t.projectId, network, parts[0]).Set(float64(otherUpsLag))
						}
					}
				}
			}
			return true
		})
	} else {
		// Only update items for this upstream and this network (all their methods)
		prefix := ups + common.KeySeparator + network + common.KeySeparator
		t.metrics.Range(func(k, v interface{}) bool {
			key := k.(string)
			mt := v.(*TrackedMetrics)
			if strings.HasPrefix(key, prefix) {
				mt.FinalizationLag.Store(upsLag)
			}
			return true
		})
	}
}

func (t *Tracker) GetUpstreamMetrics(upsId string) map[string]*TrackedMetrics {
	// network:method -> metrics
	var result = make(map[string]*TrackedMetrics)

	t.metrics.Range(func(k, v interface{}) bool {
		key := k.(string)
		mt := v.(*TrackedMetrics)
		if strings.HasPrefix(key, upsId) {
			result[strings.TrimPrefix(key, upsId+common.KeySeparator)] = mt
		}
		return true
	})

	return result
}

func (t *Tracker) IsCordoned(ups, network, method string) bool {
	if method != "*" {
		if t.getMetrics(t.getKey(ups, network, "*")).Cordoned.Load() {
			return true
		}
	}

	return t.getMetrics(t.getKey(ups, network, method)).Cordoned.Load()
}
