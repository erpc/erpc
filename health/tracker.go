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

// Instead of ups|network|method strings, we store them in a small struct.
// This avoids the overhead of string splitting and concatenation at high RPS.
type tripletKey struct {
	ups, network, method string
}

// Similarly for network metadata, which is keyed by (ups, network).
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
	LatencySecs            *QuantileTracker `json:"latencySecs"`
	ErrorsTotal            atomic.Int64     `json:"errorsTotal"`
	SelfRateLimitedTotal   atomic.Int64     `json:"selfRateLimitedTotal"`
	RemoteRateLimitedTotal atomic.Int64     `json:"remoteRateLimitedTotal"`
	RequestsTotal          atomic.Int64     `json:"requestsTotal"`
	BlockHeadLag           atomic.Int64     `json:"blockHeadLag"`
	FinalizationLag        atomic.Int64     `json:"finalizationLag"`
	Cordoned               atomic.Bool      `json:"cordoned"`
	CordonedReason         atomic.Value     `json:"cordonedReason"`
}

// ErrorRate returns the fraction of requests that resulted in an error.
func (m *TrackedMetrics) ErrorRate() float64 {
	reqs := m.RequestsTotal.Load()
	if reqs == 0 {
		return 0
	}
	return float64(m.ErrorsTotal.Load()) / float64(reqs)
}

func (m *TrackedMetrics) GetLatencySecs() common.QuantileTracker {
	return m.LatencySecs
}

// ThrottledRate returns fraction of requests that were throttled (self or remote).
func (m *TrackedMetrics) ThrottledRate() float64 {
	reqs := m.RequestsTotal.Load()
	if reqs == 0 {
		return 0
	}
	throttled := float64(m.RemoteRateLimitedTotal.Load()) + float64(m.SelfRateLimitedTotal.Load())
	return throttled / float64(reqs)
}

func (m *TrackedMetrics) MarshalJSON() ([]byte, error) {
	// Use your existing common.SonicCfg (or std library) to JSON-encode.
	return common.SonicCfg.Marshal(map[string]interface{}{
		"latencySecs":            m.LatencySecs,
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
	m.LatencySecs.Reset()
	// You may or may not want to uncordon on reset.
	// If you want to preserve cordon across windows, comment these out:
	m.Cordoned.Store(false)
	m.CordonedReason.Store("")
}

// ------------------------------------
// Tracker
// ------------------------------------

type Tracker struct {
	projectId  string
	windowSize time.Duration

	// Protected by mu:
	mu       sync.RWMutex
	metrics  map[tripletKey]*TrackedMetrics // ups-network-method => metrics
	metadata map[duoKey]*NetworkMetadata    // ups-network => metadata
}

// NewTracker constructs a new Tracker, using a standard map + RWMutex for concurrency.
func NewTracker(projectId string, windowSize time.Duration) *Tracker {
	return &Tracker{
		projectId:  projectId,
		windowSize: windowSize,
		metrics:    make(map[tripletKey]*TrackedMetrics),
		metadata:   make(map[duoKey]*NetworkMetadata),
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
			// Acquire exclusive lock before resetting
			t.mu.Lock()
			for _, tm := range t.metrics {
				tm.Reset()
			}
			t.mu.Unlock()
		}
	}
}

// For real-time aggregator updates, we store 4 expansions of the key:
// 1) The exact key (ups, network, method)
// 2) The aggregator for that ups across *all methods* => (ups, network, "*")
// 3) The aggregator for *all ups* that network, specific method => ("*", network, method)
// 4) The aggregator for the entire network => ("*", network, "*")
func (t *Tracker) getKeys(ups, network, method string) []tripletKey {
	return []tripletKey{
		{ups, network, method},
		{ups, network, "*"},
		{"*", network, method},
		{"*", network, "*"},
	}
}

func (t *Tracker) getMetadataKey(ups, network string) duoKey {
	return duoKey{ups, network}
}

// getMetrics retrieves or creates the TrackedMetrics for a specific (ups, network, method).
func (t *Tracker) getMetrics(k tripletKey) *TrackedMetrics {
	t.mu.RLock()
	m, ok := t.metrics[k]
	t.mu.RUnlock()
	if ok {
		return m
	}

	// If not found, upgrade to a write lock and create it.
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok = t.metrics[k]; ok {
		return m
	}
	m = &TrackedMetrics{
		LatencySecs: NewQuantileTracker(),
	}
	t.metrics[k] = m
	return m
}

// getMetadata retrieves or creates the NetworkMetadata for a specific (ups, network).
func (t *Tracker) getMetadata(k duoKey) *NetworkMetadata {
	t.mu.RLock()
	md, ok := t.metadata[k]
	t.mu.RUnlock()
	if ok {
		return md
	}

	// If not found, upgrade to a write lock and create it.
	t.mu.Lock()
	defer t.mu.Unlock()
	if md, ok = t.metadata[k]; ok {
		return md
	}
	md = &NetworkMetadata{}
	t.metadata[k] = md
	return md
}

// --------------------
// Cordon / Uncordon
// --------------------

// Cordon sets "cordoned" to true for the exact (ups, network, method).
func (t *Tracker) Cordon(ups, network, method, reason string) {
	log.Debug().Str("upstream", ups).
		Str("network", network).
		Str("method", method).
		Str("reason", reason).
		Msg("cordoning upstream to disable routing")

	tm := t.getMetrics(tripletKey{ups, network, method})
	tm.Cordoned.Store(true)
	tm.CordonedReason.Store(reason)

	// Example gauge update if you have a Prometheus metric:
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
	t.mu.RLock()
	if t.metrics[tripletKey{ups, network, "*"}] != nil {
		if t.metrics[tripletKey{ups, network, "*"}].Cordoned.Load() {
			t.mu.RUnlock()
			return true
		}
	}
	// Then check the exact method
	if t.metrics[tripletKey{ups, network, method}] != nil {
		cordoned := t.metrics[tripletKey{ups, network, method}].Cordoned.Load()
		t.mu.RUnlock()
		return cordoned
	}
	t.mu.RUnlock()
	return false
}

// ------------------------------------
// Basic Request & Failure Tracking
// ------------------------------------

// RecordUpstreamRequest increments the RequestsTotal counters for the relevant expansions.
func (t *Tracker) RecordUpstreamRequest(ups, network, method string) {
	keys := t.getKeys(ups, network, method)
	for _, k := range keys {
		m := t.getMetrics(k)
		m.RequestsTotal.Add(1)
	}
}

// Start a Timer (for convenience).
func (t *Tracker) RecordUpstreamDurationStart(ups, network, method string) *Timer {
	return &Timer{
		start:   time.Now(),
		network: network,
		ups:     ups,
		method:  method,
		tracker: t,
	}
}

// RecordUpstreamDuration adds duration-latency for the expansions.
func (t *Tracker) RecordUpstreamDuration(ups, network, method string, duration time.Duration) {
	keys := t.getKeys(ups, network, method)
	sec := duration.Seconds()
	for _, k := range keys {
		m := t.getMetrics(k)
		m.LatencySecs.Add(sec)
	}
	MetricUpstreamRequestDuration.WithLabelValues(t.projectId, network, ups, method).Observe(sec)
}

// RecordUpstreamFailure increments the ErrorsTotal counters for the expansions.
func (t *Tracker) RecordUpstreamFailure(ups, network, method string) {
	keys := t.getKeys(ups, network, method)
	for _, k := range keys {
		m := t.getMetrics(k)
		m.ErrorsTotal.Add(1)
	}
}

// RecordUpstreamSelfRateLimited increments the self-throttling counters for the expansions.
func (t *Tracker) RecordUpstreamSelfRateLimited(ups, network, method string) {
	keys := t.getKeys(ups, network, method)
	for _, k := range keys {
		m := t.getMetrics(k)
		m.SelfRateLimitedTotal.Add(1)
	}
	MetricUpstreamSelfRateLimitedTotal.WithLabelValues(t.projectId, network, ups, method).Inc()
}

// RecordUpstreamRemoteRateLimited increments the remote-throttling counters for the expansions.
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

// GetUpstreamMethodMetrics returns the exact metrics for (ups, network, method).
func (t *Tracker) GetUpstreamMethodMetrics(ups, network, method string) *TrackedMetrics {
	return t.getMetrics(tripletKey{ups, network, method})
}

// GetUpstreamMetrics returns all method-suffix metrics for a particular ups.
func (t *Tracker) GetUpstreamMetrics(upsId string) map[string]*TrackedMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]*TrackedMetrics)
	for k, tm := range t.metrics {
		// If ups matches upsId
		if k.ups == upsId {
			// e.g. the "network|method" portion for your key:
			suffix := k.network + "|" + k.method
			result[suffix] = tm
		}
	}
	return result
}

// GetNetworkMetrics returns the pre-aggregated metrics for the entire network,
// keyed by ("*", network, "*") so you do NOT need a heavy pass over all upstreams.
func (t *Tracker) GetNetworkMetrics(network string) *TrackedMetrics {
	return t.getMetrics(tripletKey{"*", network, "*"})
}

func (t *Tracker) GetNetworkMethodMetrics(network, method string) *TrackedMetrics {
	return t.getMetrics(tripletKey{"*", network, method})
}

// --------------------------------------------
// Block Number & Lag Tracking
// --------------------------------------------

// SetLatestBlockNumber updates HEAD block for (ups, network), then sets block-lag in
// the relevant expansions. Also updates aggregator metrics for ("*", network, "*").
func (t *Tracker) SetLatestBlockNumber(ups, network string, blockNumber int64) {
	mdKey := t.getMetadataKey(ups, network)
	ntwMdKey := t.getMetadataKey("*", network)

	upsMeta := t.getMetadata(mdKey)
	ntwMeta := t.getMetadata(ntwMdKey)

	// Possibly update the network-level highest block head
	oldNtwVal := ntwMeta.evmLatestBlockNumber.Load()
	if blockNumber > oldNtwVal {
		ntwMeta.evmLatestBlockNumber.Store(blockNumber)
		MetricUpstreamLatestBlockNumber.WithLabelValues(t.projectId, network, "*").Set(float64(blockNumber))
	}

	// Update this upstream's latest block
	oldUpsVal := upsMeta.evmLatestBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmLatestBlockNumber.Store(blockNumber)
		MetricUpstreamLatestBlockNumber.WithLabelValues(t.projectId, network, ups).Set(float64(blockNumber))
	}

	// Recompute lag for this upstream
	ntwVal := ntwMeta.evmLatestBlockNumber.Load()
	upsVal := upsMeta.evmLatestBlockNumber.Load()
	upsLag := ntwVal - upsVal
	MetricUpstreamBlockHeadLag.WithLabelValues(t.projectId, network, ups).Set(float64(upsLag))

	// Update the four expansions (ups,network,method -> blockHeadLag)
	// We do NOT have method here, so we handle all possible methods via "prefix" in the map,
	// or simply store the lag in those expansions. Because we do a full aggregator approach,
	// we can do a quick pass. At high RPS, you might do something more nuanced.
	prefixExact := tripletKey{ups: ups, network: network}
	t.mu.RLock()
	for k, tm := range t.metrics {
		if k.ups == prefixExact.ups && k.network == prefixExact.network {
			tm.BlockHeadLag.Store(upsLag)
		}
	}
	t.mu.RUnlock()
}

// SetFinalizedBlockNumber updates finalization block for (ups, network), then sets final-lag.
func (t *Tracker) SetFinalizedBlockNumber(ups, network string, blockNumber int64) {
	mdKey := t.getMetadataKey(ups, network)
	ntwMdKey := t.getMetadataKey("*", network)

	upsMeta := t.getMetadata(mdKey)
	ntwMeta := t.getMetadata(ntwMdKey)

	// Possibly update the network-level highest finalized block
	oldNtwVal := ntwMeta.evmFinalizedBlockNumber.Load()
	if blockNumber > oldNtwVal {
		ntwMeta.evmFinalizedBlockNumber.Store(blockNumber)
		MetricUpstreamFinalizedBlockNumber.WithLabelValues(t.projectId, network, "*").Set(float64(blockNumber))
	}

	// Update this upstream's finalized block
	oldUpsVal := upsMeta.evmFinalizedBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmFinalizedBlockNumber.Store(blockNumber)
		MetricUpstreamFinalizedBlockNumber.WithLabelValues(t.projectId, network, ups).Set(float64(blockNumber))
	}

	// Recompute finalization-lag
	ntwVal := ntwMeta.evmFinalizedBlockNumber.Load()
	upsVal := upsMeta.evmFinalizedBlockNumber.Load()
	upsLag := ntwVal - upsVal
	MetricUpstreamFinalizationLag.WithLabelValues(t.projectId, network, ups).Set(float64(upsLag))

	// Update expansions similarly, by scanning the map for that (ups, network).
	prefixExact := tripletKey{ups: ups, network: network}
	t.mu.RLock()
	for k, tm := range t.metrics {
		if k.ups == prefixExact.ups && k.network == prefixExact.network {
			tm.FinalizationLag.Store(upsLag)
		}
	}
	t.mu.RUnlock()
}
