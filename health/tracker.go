package health

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// ------------------------------------
// Key Types
// ------------------------------------

// upstreamKey represents "upstream × method".
// A nil Upstream means the "all-upstreams" aggregate
// and method "*" means the "all-methods" aggregate.
type upstreamKey struct {
	ups    common.Upstream
	method string
}

// networkKey represents "network × method".
// A "*" network means the "all-networks" aggregate
// and method "*" means the "all-methods" aggregate.
type networkKey struct {
	network string
	method  string
}

// metadataKey represents "upstream × network".
// A nil Upstream means the "all-upstreams" aggregate
// and network "*" means the "all-networks" aggregate.
type metadataKey struct {
	upstream common.Upstream
	network  string
}

// ------------------------------------
// Network Metadata & Timer
// ------------------------------------

type NetworkMetadata struct {
	evmLatestBlockNumber    atomic.Int64
	evmFinalizedBlockNumber atomic.Int64
}

type Timer struct {
	start         time.Time
	upstream      common.Upstream
	method        string
	compositeType string
	tracker       *Tracker
	finality      common.DataFinalityState
}

func (t *Timer) ObserveDuration(isSuccess bool) {
	duration := time.Since(t.start)
	t.tracker.RecordUpstreamDuration(t.upstream, t.method, duration, isSuccess, t.compositeType, t.finality)
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
		"errorRate":              m.ErrorRate(),
		"throttledRate":          m.ThrottledRate(),
	})
}

// Reset zeroes out counters for the next window.
// Note: blockHeadLag and finalizationLag are NOT reset because they are
// state metrics that represent current conditions, not cumulative counts.
func (m *TrackedMetrics) Reset() {
	m.ErrorsTotal.Store(0)
	m.RequestsTotal.Store(0)
	m.SelfRateLimitedTotal.Store(0)
	m.RemoteRateLimitedTotal.Store(0)
	// DO NOT reset m.BlockHeadLag - it's a state metric, not cumulative
	// DO NOT reset m.FinalizationLag - it's a state metric, not cumulative
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
	logger     *zerolog.Logger

	metadata   sync.Map // map[common.Upstream]*NetworkMetadata
	upsMetrics sync.Map // map[upsKey]*TrackedMetrics
	ntwMetrics sync.Map // map[ntwKey]*TrackedMetrics
}

// NewTracker constructs a new Tracker, using sync.Map for concurrency.
func NewTracker(logger *zerolog.Logger, projectId string, windowSize time.Duration) *Tracker {
	return &Tracker{
		logger:     logger,
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
			t.upsMetrics.Range(func(key, value any) bool {
				if tm, ok := value.(*TrackedMetrics); ok {
					tm.Reset()
				}
				return true // keep iterating
			})
			t.ntwMetrics.Range(func(key, value any) bool {
				if tm, ok := value.(*TrackedMetrics); ok {
					tm.Reset()
				}
				return true // keep iterating
			})
		}
	}
}

// For real-time aggregator updates, we store expansions of the key:
func (t *Tracker) getUpsKeys(upstream common.Upstream, method string) []upstreamKey {
	return []upstreamKey{
		{upstream, method},
		{upstream, "*"}, // any-method for this upstream
	}
}

func (t *Tracker) getNtwKeys(up common.Upstream, method string) []networkKey {
	net := up.NetworkId()
	return []networkKey{
		{net, method},
		{net, "*"}, // all methods on this network
	}
}

// getMetadata fetches or creates *NetworkMetadata from sync.Map
func (t *Tracker) getMetadata(mtdKey metadataKey) *NetworkMetadata {
	if v, ok := t.metadata.Load(mtdKey); ok {
		return v.(*NetworkMetadata)
	}
	nm := &NetworkMetadata{}
	actual, _ := t.metadata.LoadOrStore(mtdKey, nm)
	return actual.(*NetworkMetadata)
}

// getUpsMetrics fetches or creates *TrackedMetrics from sync.Map
func (t *Tracker) getUpsMetrics(k upstreamKey) *TrackedMetrics {
	if v, ok := t.upsMetrics.Load(k); ok {
		return v.(*TrackedMetrics)
	}
	tm := &TrackedMetrics{ResponseQuantiles: NewQuantileTracker()}
	actual, _ := t.upsMetrics.LoadOrStore(k, tm)
	return actual.(*TrackedMetrics)
}

func (t *Tracker) getNtwMetrics(k networkKey) *TrackedMetrics {
	if v, ok := t.ntwMetrics.Load(k); ok {
		return v.(*TrackedMetrics)
	}
	tm := &TrackedMetrics{ResponseQuantiles: NewQuantileTracker()}
	actual, _ := t.ntwMetrics.LoadOrStore(k, tm)
	return actual.(*TrackedMetrics)
}

// --------------------
// Cordon / Uncordon
// --------------------

func (t *Tracker) Cordon(upstream common.Upstream, method, reason string) {
	lg := upstream.Logger()
	lg.Debug().
		Str("method", method).
		Str("reason", reason).
		Msg("cordoning upstream to disable routing")

	tm := t.getUpsMetrics(upstreamKey{upstream, method})
	tm.Cordoned.Store(true)
	tm.CordonedReason.Store(reason)

	telemetry.MetricUpstreamCordoned.
		WithLabelValues(t.projectId, upstream.VendorName(), upstream.NetworkId(), upstream.Id(), method).
		Set(1)
}

func (t *Tracker) Uncordon(upstream common.Upstream, method string) {
	lg := upstream.Logger()
	lg.Debug().
		Str("method", method).
		Msg("uncordoning upstream to enable routing")

	tm := t.getUpsMetrics(upstreamKey{upstream, method})
	tm.Cordoned.Store(false)
	tm.CordonedReason.Store("")

	telemetry.MetricUpstreamCordoned.
		WithLabelValues(t.projectId, upstream.VendorName(), upstream.NetworkId(), upstream.Id(), method).
		Set(0)
}

// IsCordoned checks if (ups, network, method) or (ups, network, "*") is cordoned.
func (t *Tracker) IsCordoned(upstream common.Upstream, method string) bool {
	if val, ok := t.upsMetrics.Load(upstreamKey{upstream, "*"}); ok {
		if val.(*TrackedMetrics).Cordoned.Load() {
			return true
		}
	}
	if val, ok := t.upsMetrics.Load(upstreamKey{upstream, method}); ok {
		return val.(*TrackedMetrics).Cordoned.Load()
	}
	return false
}

// ------------------------------------
// Basic Request & Failure Tracking
// ------------------------------------

func (t *Tracker) RecordUpstreamRequest(up common.Upstream, method string) {
	for _, k := range t.getUpsKeys(up, method) {
		t.getUpsMetrics(k).RequestsTotal.Add(1)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		t.getNtwMetrics(nk).RequestsTotal.Add(1)
	}
}

func (t *Tracker) RecordUpstreamDurationStart(upstream common.Upstream, method string, compositeType string, finality common.DataFinalityState) *Timer {
	if compositeType == "" {
		compositeType = "none"
	}
	return &Timer{
		start:         time.Now(),
		upstream:      upstream,
		method:        method,
		compositeType: compositeType,
		finality:      finality,
		tracker:       t,
	}
}

func (t *Tracker) RecordUpstreamDuration(up common.Upstream, method string, d time.Duration, isSuccess bool, comp string, finality common.DataFinalityState) {
	if comp == "" {
		comp = "none"
	}
	sec := d.Seconds()
	if isSuccess {
		// We must calculate response time quantiles for successful requests only,
		// Otherwise we might falsely attribute "best latency" to an upstream that's just failing fast.
		for _, k := range t.getUpsKeys(up, method) {
			t.getUpsMetrics(k).ResponseQuantiles.Add(sec)
		}
		for _, nk := range t.getNtwKeys(up, method) {
			t.getNtwMetrics(nk).ResponseQuantiles.Add(sec)
		}
	}
	telemetry.MetricUpstreamRequestDuration.
		WithLabelValues(t.projectId, up.VendorName(), up.NetworkId(), up.Id(), method, comp, finality.String()).
		Observe(sec)
}

func (t *Tracker) RecordUpstreamFailure(up common.Upstream, method string, err error) {
	// We want to ignore errors that should not affect the scores:
	if common.HasErrorCode(
		err,
		common.ErrCodeEndpointExecutionException,
		common.ErrCodeUpstreamExcludedByPolicy,
		common.ErrCodeUpstreamRequestSkipped,
		common.ErrCodeUpstreamShadowing,
	) {
		return
	}

	for _, k := range t.getUpsKeys(up, method) {
		t.getUpsMetrics(k).ErrorsTotal.Add(1)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		t.getNtwMetrics(nk).ErrorsTotal.Add(1)
	}
}

func (t *Tracker) RecordUpstreamSelfRateLimited(up common.Upstream, method string) {
	for _, k := range t.getUpsKeys(up, method) {
		t.getUpsMetrics(k).SelfRateLimitedTotal.Add(1)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		t.getNtwMetrics(nk).SelfRateLimitedTotal.Add(1)
	}
	telemetry.MetricUpstreamSelfRateLimitedTotal.
		WithLabelValues(t.projectId, up.VendorName(), up.NetworkId(), up.Id(), method).
		Inc()
}

func (t *Tracker) RecordUpstreamRemoteRateLimited(up common.Upstream, method string) {
	for _, k := range t.getUpsKeys(up, method) {
		t.getUpsMetrics(k).RemoteRateLimitedTotal.Add(1)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		t.getNtwMetrics(nk).RemoteRateLimitedTotal.Add(1)
	}
	telemetry.MetricUpstreamRemoteRateLimitedTotal.
		WithLabelValues(t.projectId, up.VendorName(), up.NetworkId(), up.Id(), method).
		Inc()
}

// --------------------------------------------
// Accessors
// --------------------------------------------

func (t *Tracker) GetUpstreamMethodMetrics(up common.Upstream, method string) *TrackedMetrics {
	return t.getUpsMetrics(upstreamKey{up, method})
}

func (t *Tracker) GetUpstreamMetrics(ups common.Upstream) map[string]*TrackedMetrics {
	out := map[string]*TrackedMetrics{}
	t.upsMetrics.Range(func(k, v any) bool {
		pk := k.(upstreamKey)
		if pk.ups != nil && pk.ups.Id() == ups.Id() {
			out[pk.method] = v.(*TrackedMetrics)
		}
		return true
	})
	return out
}

func (t *Tracker) GetNetworkMethodMetrics(network, method string) *TrackedMetrics {
	return t.getNtwMetrics(networkKey{network, method})
}

// --------------------------------------------
// Block Number & Lag Tracking
// --------------------------------------------

func (t *Tracker) SetLatestBlockNumber(upstream common.Upstream, blockNumber int64) {
	id := upstream.Id()
	net := upstream.NetworkId()
	vendor := upstream.VendorName()
	lg := upstream.Logger().With().Str("networkId", net).Logger()

	lg.Trace().Int64("value", blockNumber).Msg("updating latest block number in tracker")
	if blockNumber <= 0 {
		lg.Warn().Int64("value", blockNumber).Msg("ignoring setting non-positive latest block number in tracker")
		return
	}

	mdKey := metadataKey{upstream, net}
	ntwMdKey := metadataKey{nil, net}

	// 1) Possibly update the network-level highest block head
	ntwMeta := t.getMetadata(ntwMdKey)
	oldNtwVal := ntwMeta.evmLatestBlockNumber.Load()
	needsGlobalUpdate := false
	if blockNumber > oldNtwVal {
		ntwMeta.evmLatestBlockNumber.Store(blockNumber)
		telemetry.MetricUpstreamLatestBlockNumber.
			WithLabelValues(t.projectId, "*", net, "*").
			Set(float64(blockNumber))
		needsGlobalUpdate = true
	}

	// 2) Update this upstream's latest block
	upsMeta := t.getMetadata(mdKey)
	oldUpsVal := upsMeta.evmLatestBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmLatestBlockNumber.Store(blockNumber)
		telemetry.MetricUpstreamLatestBlockNumber.
			WithLabelValues(t.projectId, vendor, net, id).
			Set(float64(blockNumber))
	}

	// 3) Recompute block head lag for this upstream
	ntwBn := ntwMeta.evmLatestBlockNumber.Load()
	if ntwBn <= 0 {
		lg.Warn().Int64("value", ntwBn).Msg("ignoring block head lag tracking for non-positive block number in tracker")
		return
	}

	upsLag := ntwBn - upsMeta.evmLatestBlockNumber.Load()
	telemetry.MetricUpstreamBlockHeadLag.
		WithLabelValues(t.projectId, vendor, net, id).
		Set(float64(upsLag))

	// 4) Update the TrackedMetrics.BlockHeadLag fields for Upstream(s)
	if needsGlobalUpdate {
		// Recompute for every upstream in the network
		t.upsMetrics.Range(func(key, value any) bool {
			k, ok := key.(upstreamKey)
			if !ok {
				return true
			}
			otherUps := k.ups
			if otherUps == nil {
				return true
			}
			otherNet := otherUps.NetworkId()
			if otherNet == net {
				tm := value.(*TrackedMetrics)
				otherUpsMeta := t.getMetadata(metadataKey{otherUps, net})
				otherVal := otherUpsMeta.evmLatestBlockNumber.Load()
				if otherVal <= 0 {
					lg.Debug().Str("otherUpstreamId", otherUps.Id()).Int64("value", otherVal).Msg("ignoring block head lag tracking for non-positive block number in tracker")
					return true
				}
				otherLag := ntwBn - otherVal
				tm.BlockHeadLag.Store(otherLag)
				telemetry.MetricUpstreamBlockHeadLag.
					WithLabelValues(t.projectId, otherUps.VendorName(), net, otherUps.Id()).
					Set(float64(otherLag))
			}
			return true
		})
	} else {
		// Only update items for this single upstream in this network
		t.upsMetrics.Range(func(key, value any) bool {
			k, ok := key.(upstreamKey)
			if !ok {
				return true
			}
			if k.ups == nil {
				return true
			}
			if k.ups.Id() == id && k.ups.NetworkId() == net {
				tm := value.(*TrackedMetrics)
				tm.BlockHeadLag.Store(upsLag)
			}
			return true
		})
	}
}

func (t *Tracker) SetLatestBlockNumberForNetwork(network string, blockNumber int64) {
	ntwMeta := t.getMetadata(metadataKey{nil, network})
	ntwMeta.evmLatestBlockNumber.Store(blockNumber)
	telemetry.MetricUpstreamLatestBlockNumber.WithLabelValues(t.projectId, "*", network, "*").Set(float64(blockNumber))
}

func (t *Tracker) SetFinalizedBlockNumber(upstream common.Upstream, blockNumber int64) {
	lg := upstream.Logger().With().Str("networkId", upstream.NetworkId()).Logger()

	lg.Trace().Int64("value", blockNumber).Msg("updating finalized block number in tracker")

	if blockNumber <= 0 {
		lg.Warn().Int64("value", blockNumber).Msg("ignoring setting non-positive block number in finalized block tracker")
		return
	}

	id := upstream.Id()
	net := upstream.NetworkId()
	vendor := upstream.VendorName()

	mdKey := metadataKey{upstream, net}
	ntwMdKey := metadataKey{nil, net}

	upsMeta := t.getMetadata(mdKey)
	ntwMeta := t.getMetadata(ntwMdKey)

	// Possibly update the network-level highest finalized block
	oldNtwVal := ntwMeta.evmFinalizedBlockNumber.Load()
	needsGlobalUpdate := false
	if blockNumber > oldNtwVal {
		ntwMeta.evmFinalizedBlockNumber.Store(blockNumber)
		telemetry.MetricUpstreamFinalizedBlockNumber.
			WithLabelValues(t.projectId, "*", net, "*").
			Set(float64(blockNumber))
		needsGlobalUpdate = true
	}

	// Update this upstream's finalized block
	oldUpsVal := upsMeta.evmFinalizedBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmFinalizedBlockNumber.Store(blockNumber)
		telemetry.MetricUpstreamFinalizedBlockNumber.
			WithLabelValues(t.projectId, vendor, net, id).
			Set(float64(blockNumber))
	}

	// Recompute finalization lag for this upstream
	ntwVal := ntwMeta.evmFinalizedBlockNumber.Load()
	if ntwVal <= 0 {
		lg.Warn().Int64("value", ntwVal).Msg("ignoring finalization lag tracking for negative block number in tracker")
		return
	}

	upsVal := upsMeta.evmFinalizedBlockNumber.Load()
	upsLag := ntwVal - upsVal

	// Update Prometheus for this upstream
	telemetry.MetricUpstreamFinalizationLag.
		WithLabelValues(t.projectId, vendor, net, id).
		Set(float64(upsLag))

	// Update the finalization lag across the network if needed
	if needsGlobalUpdate {
		t.upsMetrics.Range(func(key, value any) bool {
			k, ok := key.(upstreamKey)
			if !ok {
				return true
			}
			otherUps := k.ups
			if otherUps == nil {
				return true
			}
			otherNet := otherUps.NetworkId()
			if otherNet == net {
				tm := value.(*TrackedMetrics)
				otherUpsMeta := t.getMetadata(metadataKey{otherUps, net})
				otherVal := otherUpsMeta.evmFinalizedBlockNumber.Load()
				if otherVal <= 0 {
					lg.Debug().Str("otherUpstreamId", otherUps.Id()).Int64("value", otherVal).Msg("ignoring finalization lag tracking for non-positive block number in tracker")
					return true
				}
				otherLag := ntwVal - otherVal
				tm.FinalizationLag.Store(otherLag)
				telemetry.MetricUpstreamFinalizationLag.
					WithLabelValues(t.projectId, otherUps.VendorName(), net, otherUps.Id()).
					Set(float64(otherLag))
			}
			return true
		})
	} else {
		// Only update finalization lag for this single upstream
		t.upsMetrics.Range(func(key, value any) bool {
			k, ok := key.(upstreamKey)
			if !ok {
				return true
			}
			if k.ups == nil {
				return true
			}
			if k.ups.Id() == id && k.ups.NetworkId() == net {
				tm := value.(*TrackedMetrics)
				tm.FinalizationLag.Store(upsLag)
			}
			return true
		})
	}
}

func (t *Tracker) SetFinalizedBlockNumberForNetwork(network string, blockNumber int64) {
	ntwMeta := t.getMetadata(metadataKey{nil, network})
	ntwMeta.evmFinalizedBlockNumber.Store(blockNumber)
	telemetry.MetricUpstreamFinalizedBlockNumber.WithLabelValues(t.projectId, "*", network, "*").Set(float64(blockNumber))
}

func (t *Tracker) RecordBlockHeadLargeRollback(upstream common.Upstream, finality string, currentVal, newVal int64) {
	rollback := currentVal - newVal

	id := upstream.Id()
	net := upstream.NetworkId()
	vendor := upstream.VendorName()

	lg := upstream.Logger().With().Str("networkId", net).Logger()
	lg.Debug().
		Int64("currentValue", currentVal).
		Int64("newValue", newVal).
		Int64("rollback", rollback).
		Msgf("recording block rollback in tracker")

	telemetry.MetricUpstreamBlockHeadLargeRollback.
		WithLabelValues(t.projectId, vendor, net, id).
		Set(float64(rollback))
}
