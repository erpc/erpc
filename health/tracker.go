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

// ErrorRate returns the fraction of requests that resulted in an error.
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
		{ups, "*", "*"},
		{"*", network, method},
		{"*", network, "*"},
	}
}

func (t *Tracker) getMetadata(k duoKey) *NetworkMetadata {
    t.mu.RLock()
	md, ok := t.metadata[k]
	t.mu.RUnlock()
	if !ok {
		md = &NetworkMetadata{}
		t.mu.Lock()
		t.metadata[k] = md
		t.mu.Unlock()
	}
	return md
}

func (t *Tracker) getMetrics(k tripletKey) *TrackedMetrics {
    t.mu.RLock()
	m, ok := t.metrics[k]
	t.mu.RUnlock()
	if !ok {
		m = &TrackedMetrics{ResponseQuantiles: NewQuantileTracker()}
		t.mu.Lock()
		t.metrics[k] = m
		t.mu.Unlock()
	}
	return m
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
		m.ResponseQuantiles.Add(sec)
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

func (t *Tracker) GetNetworkMethodMetrics(network, method string) *TrackedMetrics {
	return t.getMetrics(tripletKey{"*", network, method})
}

// --------------------------------------------
// Block Number & Lag Tracking
// --------------------------------------------

// SetLatestBlockNumber updates HEAD block for (ups, network), then sets block-lag in
// the relevant expansions. Also updates aggregator metrics for ("*", network, "*").
func (t *Tracker) SetLatestBlockNumber(ups, network string, blockNumber int64) {
	// Prepare the keys
	mdKey := duoKey{ups: ups, network: network}
	ntwMdKey := duoKey{ups: "*", network: network}

	// We’ll need to track whether we updated the network’s highest block
	needsGlobalUpdate := false

	// 1) Possibly update the network-level highest block head
	ntwMeta := t.getMetadata(ntwMdKey)
	oldNtwVal := ntwMeta.evmLatestBlockNumber.Load()
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
	//    (difference between the network-level “highest block” and this upstream’s block)
	upsLag := ntwMeta.evmLatestBlockNumber.Load() - upsMeta.evmLatestBlockNumber.Load()
	MetricUpstreamBlockHeadLag.
		WithLabelValues(t.projectId, network, ups).
		Set(float64(upsLag))

	// 4) Update the TrackedMetrics.BlockHeadLag fields accordingly
	//    - if needsGlobalUpdate == true, update for all upstreams on this network
	//    - otherwise, only update for this single upstream
	if needsGlobalUpdate {
		// t.mu.RLock()
		// defer t.mu.RUnlock()
		// Loop through *all* known metrics
		for k, tm := range t.metrics {
			if k.network == network {
				// Recompute lag for each upstream in this network
				otherUpsMeta := t.getMetadata(duoKey{ups: k.ups, network: k.network})
				otherLag := ntwMeta.evmLatestBlockNumber.Load() - otherUpsMeta.evmLatestBlockNumber.Load()

				tm.BlockHeadLag.Store(otherLag)
				MetricUpstreamBlockHeadLag.
					WithLabelValues(t.projectId, network, k.ups).
					Set(float64(otherLag))
			}
		}
	} else {
		// Only update items for this single upstream in this network (all methods)
		t.mu.RLock()
		defer t.mu.RUnlock()
		for k, tm := range t.metrics {
			// We match by ups and network, ignoring method
			if k.ups == ups && k.network == network {
				tm.BlockHeadLag.Store(upsLag)
			}
		}
	}
}

func (t *Tracker) SetFinalizedBlockNumber(ups, network string, blockNumber int64) {
	mdKey := duoKey{ups, network}
	ntwMdKey := duoKey{"*", network}

	upsMeta := t.getMetadata(mdKey)
	ntwMeta := t.getMetadata(ntwMdKey)

	needsGlobalUpdate := false

	// Possibly update the network-level highest finalized block
	oldNtwVal := ntwMeta.evmFinalizedBlockNumber.Load()
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

	// Recompute finalization lag for just this upstream
	ntwVal := ntwMeta.evmFinalizedBlockNumber.Load()
	upsVal := upsMeta.evmFinalizedBlockNumber.Load()
	upsLag := ntwVal - upsVal

	// Update Prometheus for this upstream
	MetricUpstreamFinalizationLag.
		WithLabelValues(t.projectId, network, ups).
		Set(float64(upsLag))

	// Now decide whether to update only this upstream or all upstreams in the network
	if needsGlobalUpdate {
		// The highest finalized block in the network changed;
		// update every upstream's finalization lag for this network.
		// t.mu.RLock()
		// defer t.mu.RUnlock()

		for k, tm := range t.metrics {
			if k.network == network {
				otherUpsMeta := t.getMetadata(duoKey{ups: k.ups, network: k.network})
				otherLag := ntwVal - otherUpsMeta.evmFinalizedBlockNumber.Load()

				tm.FinalizationLag.Store(otherLag)
				MetricUpstreamFinalizationLag.
					WithLabelValues(t.projectId, network, k.ups).
					Set(float64(otherLag))
			}
		}
	} else {
		// Only update finalization lag for this single upstream (all methods)
		t.mu.RLock()
		defer t.mu.RUnlock()

		for k, tm := range t.metrics {
			if k.ups == ups && k.network == network {
				tm.FinalizationLag.Store(upsLag)
			}
		}
	}
}
