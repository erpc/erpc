package health

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
)

// ------------------------------------
// Key Types
// ------------------------------------

type tripletKey struct {
	ups, network, method string
}

type duoKey struct {
	ups, network string
}

// ------------------------------------
// Network Metadata & Timer
// ------------------------------------

type NetworkMetadata struct {
	evmLatestBlockNumber    atomic.Int64
	evmFinalizedBlockNumber atomic.Int64
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

// ------------------------------------
// TrackedMetrics
// ------------------------------------

type TrackedMetrics struct {
	ResponseQuantiles      *QuantileTracker `json:"responseQuantiles"`
	ErrorsTotal            atomic.Int64     `json:"errorsTotal"`
	SelfRateLimitedTotal   atomic.Int64     `json:"selfRateLimitedTotal"`
	RemoteRateLimitedTotal atomic.Int64     `json:"remoteRateLimitedTotal"`
	RequestsTotal          atomic.Int64     `json:"requestsTotal"`
	BlockHeadLag           atomic.Int64     `json:"blockHeadLag"`
	FinalizationLag        atomic.Int64     `json:"finalizationLag"`
	Cordoned               atomic.Bool      `json:"cordoned"`
	CordonedReason         atomic.Value     `json:"cordonedReason"`
}

func (m *TrackedMetrics) ErrorRate() float64 {
	reqs := m.RequestsTotal.Load()
	if reqs == 0 {
		return 0
	}
	return float64(m.ErrorsTotal.Load()) / float64(reqs)
}

func (m *TrackedMetrics) GetResponseQuantiles() common.QuantileTracker {
	return m.ResponseQuantiles
}

func (m *TrackedMetrics) ThrottledRate() float64 {
	reqs := m.RequestsTotal.Load()
	if reqs == 0 {
		return 0
	}
	throttled := float64(m.RemoteRateLimitedTotal.Load()) + float64(m.SelfRateLimitedTotal.Load())
	return throttled / float64(reqs)
}

func (m *TrackedMetrics) MarshalJSON() ([]byte, error) {
	return common.SonicCfg.Marshal(map[string]interface{}{
		"responseQuantiles":      m.ResponseQuantiles,
		"errorsTotal":            m.ErrorsTotal.Load(),
		"selfRateLimitedTotal":   m.SelfRateLimitedTotal.Load(),
		"remoteRateLimitedTotal": m.RemoteRateLimitedTotal.Load(),
		"requestsTotal":          m.RequestsTotal.Load(),
		"blockHeadLag":           m.BlockHeadLag.Load(),
		"finalizationLag":        m.FinalizationLag.Load(),
		"cordoned":               m.Cordoned.Load(),
		"cordonedReason":         m.CordonedReason.Load(),
	})
}

// Reset zeroes out counters for the next window.
func (m *TrackedMetrics) Reset() {
	m.ErrorsTotal.Store(0)
	m.RequestsTotal.Store(0)
	m.SelfRateLimitedTotal.Store(0)
	m.RemoteRateLimitedTotal.Store(0)
	m.BlockHeadLag.Store(0)
	m.FinalizationLag.Store(0)
	m.ResponseQuantiles.Reset()

	// Optionally uncordon
	m.Cordoned.Store(false)
	m.CordonedReason.Store("")
}

// ------------------------------------
// Tracker
// ------------------------------------

type Tracker struct {
	projectId  string
	windowSize time.Duration

	// Replace the maps + mu with sync.Map for concurrency:
	metrics  sync.Map // map[tripletKey]*TrackedMetrics
	metadata sync.Map // map[duoKey]*NetworkMetadata
}

// NewTracker constructs a new Tracker, using sync.Map for concurrency.
func NewTracker(projectId string, windowSize time.Duration) *Tracker {
	return &Tracker{
		projectId:  projectId,
		windowSize: windowSize,
	}
}

// Bootstrap starts the goroutine that periodically resets the metrics.
func (t *Tracker) Bootstrap(ctx context.Context) {
	go t.resetMetricsLoop(ctx)
}

// resetMetricsLoop periodically resets metrics each windowSize.
func (t *Tracker) resetMetricsLoop(ctx context.Context) {
	ticker := time.NewTicker(t.windowSize)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Range over sync.Map to reset all known metrics
			t.metrics.Range(func(key, value any) bool {
				if tm, ok := value.(*TrackedMetrics); ok {
					tm.Reset()
				}
				return true // keep iterating
			})
		}
	}
}

// For real-time aggregator updates, we store expansions of the key:
func (t *Tracker) getKeys(ups, network, method string) []tripletKey {
	// same expansions as before
	return []tripletKey{
		{ups, network, method},
		{ups, network, "*"},
		{ups, "*", "*"},
		{"*", network, method},
		{"*", network, "*"},
	}
}

// getMetadata fetches or creates *NetworkMetadata from sync.Map
func (t *Tracker) getMetadata(k duoKey) *NetworkMetadata {
	if val, ok := t.metadata.Load(k); ok {
		return val.(*NetworkMetadata)
	}

	nm := &NetworkMetadata{}
	actual, loaded := t.metadata.LoadOrStore(k, nm)
	if loaded {
		return actual.(*NetworkMetadata)
	}
	return nm
}

// getMetrics fetches or creates *TrackedMetrics from sync.Map
func (t *Tracker) getMetrics(k tripletKey) *TrackedMetrics {
	if val, ok := t.metrics.Load(k); ok {
		return val.(*TrackedMetrics)
	}
	newTm := &TrackedMetrics{
		ResponseQuantiles: NewQuantileTracker(),
	}
	actual, loaded := t.metrics.LoadOrStore(k, newTm)
	if loaded {
		return actual.(*TrackedMetrics)
	}
	return newTm
}

// --------------------
// Cordon / Uncordon
// --------------------

func (t *Tracker) Cordon(ups, network, method, reason string) {
	log.Debug().Str("upstream", ups).
		Str("network", network).
		Str("method", method).
		Str("reason", reason).
		Msg("cordoning upstream to disable routing")

	tm := t.getMetrics(tripletKey{ups, network, method})
	tm.Cordoned.Store(true)
	tm.CordonedReason.Store(reason)

	MetricUpstreamCordoned.WithLabelValues(t.projectId, network, ups, method).Set(1)
}

func (t *Tracker) Uncordon(ups, network, method string) {
	tm := t.getMetrics(tripletKey{ups, network, method})
	tm.Cordoned.Store(false)
	tm.CordonedReason.Store("")

	MetricUpstreamCordoned.WithLabelValues(t.projectId, network, ups, method).Set(0)
}

// IsCordoned checks if (ups, network, method) or (ups, network, "*") is cordoned.
func (t *Tracker) IsCordoned(ups, network, method string) bool {
	// If the entire upstream for that network is cordoned, treat it as cordoned
	if val, ok := t.metrics.Load(tripletKey{ups, network, "*"}); ok {
		tm := val.(*TrackedMetrics)
		if tm.Cordoned.Load() {
			return true
		}
	}
	// Then check the exact method
	if val, ok := t.metrics.Load(tripletKey{ups, network, method}); ok {
		tm := val.(*TrackedMetrics)
		return tm.Cordoned.Load()
	}
	return false
}

// ------------------------------------
// Basic Request & Failure Tracking
// ------------------------------------

func (t *Tracker) RecordUpstreamRequest(ups, network, method string) {
	keys := t.getKeys(ups, network, method)
	for _, k := range keys {
		m := t.getMetrics(k)
		m.RequestsTotal.Add(1)
	}
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
	keys := t.getKeys(ups, network, method)
	sec := duration.Seconds()
	for _, k := range keys {
		m := t.getMetrics(k)
		m.ResponseQuantiles.Add(sec)
	}
	MetricUpstreamRequestDuration.WithLabelValues(t.projectId, network, ups, method).Observe(sec)
}

func (t *Tracker) RecordUpstreamFailure(ups, network, method string) {
	keys := t.getKeys(ups, network, method)
	for _, k := range keys {
		m := t.getMetrics(k)
		m.ErrorsTotal.Add(1)
	}
}

func (t *Tracker) RecordUpstreamSelfRateLimited(ups, network, method string) {
	keys := t.getKeys(ups, network, method)
	for _, k := range keys {
		m := t.getMetrics(k)
		m.SelfRateLimitedTotal.Add(1)
	}
	MetricUpstreamSelfRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

func (t *Tracker) RecordUpstreamRemoteRateLimited(ups, network, method string) {
	keys := t.getKeys(ups, network, method)
	for _, k := range keys {
		m := t.getMetrics(k)
		m.RemoteRateLimitedTotal.Add(1)
	}
	MetricUpstreamRemoteRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

// --------------------------------------------
// Accessors
// --------------------------------------------

func (t *Tracker) GetUpstreamMethodMetrics(ups, network, method string) *TrackedMetrics {
	return t.getMetrics(tripletKey{ups, network, method})
}

func (t *Tracker) GetUpstreamMetrics(upsId string) map[string]*TrackedMetrics {
	result := make(map[string]*TrackedMetrics)

	// Range over the sync.Map to find all that match upsId
	t.metrics.Range(func(key, value any) bool {
		k, ok := key.(tripletKey)
		if !ok {
			return true
		}
		if k.ups == upsId {
			tm := value.(*TrackedMetrics)
			suffix := k.network + "|" + k.method
			result[suffix] = tm
		}
		return true
	})
	return result
}

func (t *Tracker) GetNetworkMethodMetrics(network, method string) *TrackedMetrics {
	return t.getMetrics(tripletKey{"*", network, method})
}

// --------------------------------------------
// Block Number & Lag Tracking
// --------------------------------------------

func (t *Tracker) SetLatestBlockNumber(ups, network string, blockNumber int64) {
	mdKey := duoKey{ups: ups, network: network}
	ntwMdKey := duoKey{ups: "*", network: network}

	// 1) Possibly update the network-level highest block head
	ntwMeta := t.getMetadata(ntwMdKey)
	oldNtwVal := ntwMeta.evmLatestBlockNumber.Load()
	needsGlobalUpdate := false
	if blockNumber > oldNtwVal {
		ntwMeta.evmLatestBlockNumber.Store(blockNumber)
		MetricUpstreamLatestBlockNumber.
			WithLabelValues(t.projectId, network, "*").
			Set(float64(blockNumber))
		needsGlobalUpdate = true
	}

	// 2) Update this upstream’s latest block
	upsMeta := t.getMetadata(mdKey)
	oldUpsVal := upsMeta.evmLatestBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmLatestBlockNumber.Store(blockNumber)
		MetricUpstreamLatestBlockNumber.
			WithLabelValues(t.projectId, network, ups).
			Set(float64(blockNumber))
	}

	// 3) Recompute block head lag for this upstream
	upsLag := ntwMeta.evmLatestBlockNumber.Load() - upsMeta.evmLatestBlockNumber.Load()
	MetricUpstreamBlockHeadLag.
		WithLabelValues(t.projectId, network, ups).
		Set(float64(upsLag))

	// 4) Update the TrackedMetrics.BlockHeadLag fields
	if needsGlobalUpdate {
		// Recompute for every upstream in the network
		t.metrics.Range(func(key, value any) bool {
			k, ok := key.(tripletKey)
			if !ok {
				return true
			}
			if k.network == network {
				tm := value.(*TrackedMetrics)
				otherUpsMeta := t.getMetadata(duoKey{ups: k.ups, network: network})
				otherLag := ntwMeta.evmLatestBlockNumber.Load() - otherUpsMeta.evmLatestBlockNumber.Load()
				tm.BlockHeadLag.Store(otherLag)
				MetricUpstreamBlockHeadLag.
					WithLabelValues(t.projectId, network, k.ups).
					Set(float64(otherLag))
			}
			return true
		})
	} else {
		// Only update items for this single upstream in this network
		t.metrics.Range(func(key, value any) bool {
			k, ok := key.(tripletKey)
			if !ok {
				return true
			}
			if k.ups == ups && k.network == network {
				tm := value.(*TrackedMetrics)
				tm.BlockHeadLag.Store(upsLag)
			}
			return true
		})
	}
}

func (t *Tracker) SetFinalizedBlockNumber(ups, network string, blockNumber int64) {
	mdKey := duoKey{ups, network}
	ntwMdKey := duoKey{"*", network}

	upsMeta := t.getMetadata(mdKey)
	ntwMeta := t.getMetadata(ntwMdKey)

	// Possibly update the network-level highest finalized block
	oldNtwVal := ntwMeta.evmFinalizedBlockNumber.Load()
	needsGlobalUpdate := false
	if blockNumber > oldNtwVal {
		ntwMeta.evmFinalizedBlockNumber.Store(blockNumber)
		MetricUpstreamFinalizedBlockNumber.
			WithLabelValues(t.projectId, network, "*").
			Set(float64(blockNumber))
		needsGlobalUpdate = true
	}

	// Update this upstream's finalized block
	oldUpsVal := upsMeta.evmFinalizedBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmFinalizedBlockNumber.Store(blockNumber)
		MetricUpstreamFinalizedBlockNumber.
			WithLabelValues(t.projectId, network, ups).
			Set(float64(blockNumber))
	}

	// Recompute finalization lag for this upstream
	ntwVal := ntwMeta.evmFinalizedBlockNumber.Load()
	upsVal := upsMeta.evmFinalizedBlockNumber.Load()
	upsLag := ntwVal - upsVal

	// Update Prometheus for this upstream
	MetricUpstreamFinalizationLag.
		WithLabelValues(t.projectId, network, ups).
		Set(float64(upsLag))

	// Update the finalization lag across the network if needed
	if needsGlobalUpdate {
		t.metrics.Range(func(key, value any) bool {
			k, ok := key.(tripletKey)
			if !ok {
				return true
			}
			if k.network == network {
				tm := value.(*TrackedMetrics)
				otherUpsMeta := t.getMetadata(duoKey{ups: k.ups, network: k.network})
				otherLag := ntwVal - otherUpsMeta.evmFinalizedBlockNumber.Load()
				tm.FinalizationLag.Store(otherLag)
				MetricUpstreamFinalizationLag.
					WithLabelValues(t.projectId, network, k.ups).
					Set(float64(otherLag))
			}
			return true
		})
	} else {
		// Only update finalization lag for this single upstream
		t.metrics.Range(func(key, value any) bool {
			k, ok := key.(tripletKey)
			if !ok {
				return true
			}
			if k.ups == ups && k.network == network {
				tm := value.(*TrackedMetrics)
				tm.FinalizationLag.Store(upsLag)
			}
			return true
		})
	}
}
