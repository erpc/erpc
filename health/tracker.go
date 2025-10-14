package health

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus"
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
	evmLatestBlockTimestamp atomic.Int64
	evmFinalizedBlockNumber atomic.Int64
}

type Timer struct {
	start         time.Time
	upstream      common.Upstream
	method        string
	compositeType string
	tracker       *Tracker
	finality      common.DataFinalityState
	userId        string
}

func (t *Timer) ObserveDuration(isSuccess bool) {
	duration := time.Since(t.start)
	t.tracker.RecordUpstreamDuration(t.upstream, t.method, duration, isSuccess, t.compositeType, t.finality, t.userId)
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
	MisbehaviorsTotal      atomic.Int64     `json:"misbehaviorsTotal"`
	BlockHeadLag           atomic.Int64     `json:"blockHeadLag"`
	FinalizationLag        atomic.Int64     `json:"finalizationLag"`
	Cordoned               atomic.Bool      `json:"cordoned"`
	LastCordonedReason     atomic.Value     `json:"lastCordonedReason"`
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

func (m *TrackedMetrics) MisbehaviorRate() float64 {
	reqs := m.RequestsTotal.Load()
	if reqs == 0 {
		return 0
	}
	return float64(m.MisbehaviorsTotal.Load()) / float64(reqs)
}

func (m *TrackedMetrics) MarshalJSON() ([]byte, error) {
	return common.SonicCfg.Marshal(map[string]interface{}{
		"responseQuantiles":      m.ResponseQuantiles,
		"errorsTotal":            m.ErrorsTotal.Load(),
		"selfRateLimitedTotal":   m.SelfRateLimitedTotal.Load(),
		"remoteRateLimitedTotal": m.RemoteRateLimitedTotal.Load(),
		"requestsTotal":          m.RequestsTotal.Load(),
		"misbehaviorsTotal":      m.MisbehaviorsTotal.Load(),
		"blockHeadLag":           m.BlockHeadLag.Load(),
		"finalizationLag":        m.FinalizationLag.Load(),
		"cordoned":               m.Cordoned.Load(),
		"lastCordonedReason":     m.LastCordonedReason.Load(),
		"errorRate":              m.ErrorRate(),
		"throttledRate":          m.ThrottledRate(),
		"misbehaviorRate":        m.MisbehaviorRate(),
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
	m.MisbehaviorsTotal.Store(0)
	// DO NOT reset m.BlockHeadLag - it's a state metric, not cumulative
	// DO NOT reset m.FinalizationLag - it's a state metric, not cumulative
	m.ResponseQuantiles.Reset()

	// Optionally uncordon
	m.Cordoned.Store(false)
	m.LastCordonedReason.Store("")
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

	upstreamsByNetwork map[string][]upstreamKey // Track which upstreams belong to each network
	mu                 sync.RWMutex             // Protect the map

	// Cache of pre-bound Prometheus observers for upstream request duration
	// Keyed by the full label set to avoid per-request MetricVec map lookups.
	urdObsCache sync.Map // map[urdoKey]prometheus.Observer

	// Caches for other hot-path metrics
	selfRateLimitedCounterCache   sync.Map // map[srltKey]prometheus.Counter
	remoteRateLimitedCounterCache sync.Map // map[rrltKey]prometheus.Counter
	latestBlockGaugeCache         sync.Map // map[ubKey]prometheus.Gauge
	finalizedBlockGaugeCache      sync.Map // map[ubKey]prometheus.Gauge
	headLagGaugeCache             sync.Map // map[ubKey]prometheus.Gauge
	finalizationLagGaugeCache     sync.Map // map[ubKey]prometheus.Gauge
	cordonedGaugeCache            sync.Map // map[cordKey]prometheus.Gauge
	rollbackGaugeCache            sync.Map // map[ubKey]prometheus.Gauge
}

// urdoKey uniquely identifies a MetricUpstreamRequestDuration time series.
type urdoKey struct {
	project   string
	vendor    string
	network   string
	upstream  string
	category  string
	composite string
	finality  string
	user      string
}

func (t *Tracker) getUpstreamRequestDurationObserver(up common.Upstream, method, composite string, finality common.DataFinalityState, userId string) prometheus.Observer {
	key := urdoKey{
		project:   t.projectId,
		vendor:    up.VendorName(),
		network:   up.NetworkLabel(),
		upstream:  up.Id(),
		category:  method,
		composite: composite,
		finality:  finality.String(),
		user:      userId,
	}
	if v, ok := t.urdObsCache.Load(key); ok {
		return v.(prometheus.Observer)
	}
	obs := telemetry.MetricUpstreamRequestDuration.WithLabelValues(
		key.project, key.vendor, key.network, key.upstream, key.category, key.composite, key.finality, key.user,
	)
	actual, _ := t.urdObsCache.LoadOrStore(key, obs)
	return actual.(prometheus.Observer)
}

type srltKey struct {
	project   string
	vendor    string
	network   string
	upstream  string
	category  string
	user      string
	agentName string
}

func (t *Tracker) getSelfRateLimitedCounter(up common.Upstream, method, userId, agentName string) prometheus.Counter {
	key := srltKey{t.projectId, up.VendorName(), up.NetworkLabel(), up.Id(), method, userId, agentName}
	if v, ok := t.selfRateLimitedCounterCache.Load(key); ok {
		return v.(prometheus.Counter)
	}
	c := telemetry.MetricUpstreamSelfRateLimitedTotal.WithLabelValues(
		key.project, key.vendor, key.network, key.upstream, key.category, key.user, key.agentName,
	)
	actual, _ := t.selfRateLimitedCounterCache.LoadOrStore(key, c)
	return actual.(prometheus.Counter)
}

type rrltKey = srltKey

func (t *Tracker) getRemoteRateLimitedCounter(up common.Upstream, method, userId, agentName string) prometheus.Counter {
	key := rrltKey{t.projectId, up.VendorName(), up.NetworkLabel(), up.Id(), method, userId, agentName}
	if v, ok := t.remoteRateLimitedCounterCache.Load(key); ok {
		return v.(prometheus.Counter)
	}
	c := telemetry.MetricUpstreamRemoteRateLimitedTotal.WithLabelValues(
		key.project, key.vendor, key.network, key.upstream, key.category, key.user, key.agentName,
	)
	actual, _ := t.remoteRateLimitedCounterCache.LoadOrStore(key, c)
	return actual.(prometheus.Counter)
}

type ubKey struct {
	project  string
	vendor   string
	network  string
	upstream string
}

func (t *Tracker) getLatestBlockGauge(project, vendor, networkLabel, upstreamId string) prometheus.Gauge {
	key := ubKey{project, vendor, networkLabel, upstreamId}
	if v, ok := t.latestBlockGaugeCache.Load(key); ok {
		return v.(prometheus.Gauge)
	}
	g := telemetry.MetricUpstreamLatestBlockNumber.WithLabelValues(project, vendor, networkLabel, upstreamId)
	actual, _ := t.latestBlockGaugeCache.LoadOrStore(key, g)
	return actual.(prometheus.Gauge)
}

func (t *Tracker) getFinalizedBlockGauge(project, vendor, networkLabel, upstreamId string) prometheus.Gauge {
	key := ubKey{project, vendor, networkLabel, upstreamId}
	if v, ok := t.finalizedBlockGaugeCache.Load(key); ok {
		return v.(prometheus.Gauge)
	}
	g := telemetry.MetricUpstreamFinalizedBlockNumber.WithLabelValues(project, vendor, networkLabel, upstreamId)
	actual, _ := t.finalizedBlockGaugeCache.LoadOrStore(key, g)
	return actual.(prometheus.Gauge)
}

func (t *Tracker) getHeadLagGauge(project, vendor, networkLabel, upstreamId string) prometheus.Gauge {
	key := ubKey{project, vendor, networkLabel, upstreamId}
	if v, ok := t.headLagGaugeCache.Load(key); ok {
		return v.(prometheus.Gauge)
	}
	g := telemetry.MetricUpstreamBlockHeadLag.WithLabelValues(project, vendor, networkLabel, upstreamId)
	actual, _ := t.headLagGaugeCache.LoadOrStore(key, g)
	return actual.(prometheus.Gauge)
}

func (t *Tracker) getFinalizationLagGauge(project, vendor, networkLabel, upstreamId string) prometheus.Gauge {
	key := ubKey{project, vendor, networkLabel, upstreamId}
	if v, ok := t.finalizationLagGaugeCache.Load(key); ok {
		return v.(prometheus.Gauge)
	}
	g := telemetry.MetricUpstreamFinalizationLag.WithLabelValues(project, vendor, networkLabel, upstreamId)
	actual, _ := t.finalizationLagGaugeCache.LoadOrStore(key, g)
	return actual.(prometheus.Gauge)
}

type cordKey struct {
	project  string
	vendor   string
	network  string
	upstream string
	category string
	reason   string
}

func (t *Tracker) getCordonedGauge(up common.Upstream, method, reason string) prometheus.Gauge {
	key := cordKey{t.projectId, up.VendorName(), up.NetworkLabel(), up.Id(), method, reason}
	if v, ok := t.cordonedGaugeCache.Load(key); ok {
		return v.(prometheus.Gauge)
	}
	g := telemetry.MetricUpstreamCordoned.WithLabelValues(
		key.project, key.vendor, key.network, key.upstream, key.category, key.reason,
	)
	actual, _ := t.cordonedGaugeCache.LoadOrStore(key, g)
	return actual.(prometheus.Gauge)
}

func (t *Tracker) getRollbackGauge(up common.Upstream) prometheus.Gauge {
	key := ubKey{t.projectId, up.VendorName(), up.NetworkLabel(), up.Id()}
	if v, ok := t.rollbackGaugeCache.Load(key); ok {
		return v.(prometheus.Gauge)
	}
	g := telemetry.MetricUpstreamBlockHeadLargeRollback.WithLabelValues(
		t.projectId, up.VendorName(), up.NetworkLabel(), up.Id(),
	)
	actual, _ := t.rollbackGaugeCache.LoadOrStore(key, g)
	return actual.(prometheus.Gauge)
}

// NewTracker constructs a new Tracker, using sync.Map for concurrency.
func NewTracker(logger *zerolog.Logger, projectId string, windowSize time.Duration) *Tracker {
	return &Tracker{
		logger:             logger,
		projectId:          projectId,
		windowSize:         windowSize,
		upstreamsByNetwork: make(map[string][]upstreamKey),
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
	// Track this upstreamKey under its network for efficient global updates
	if k.ups != nil {
		net := k.ups.NetworkId()
		t.mu.Lock()
		// Deduplicate entries for the same upstream and method
		list := t.upstreamsByNetwork[net]
		exists := false
		for _, existing := range list {
			if existing.method == k.method && existing.ups != nil && existing.ups.Id() == k.ups.Id() {
				exists = true
				break
			}
		}
		if !exists {
			t.upstreamsByNetwork[net] = append(list, k)
		}
		t.mu.Unlock()
	}
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
	tm.LastCordonedReason.Store(reason)

	t.getCordonedGauge(upstream, method, reason).Set(1)
}

func (t *Tracker) Uncordon(upstream common.Upstream, method string, reason string) {
	lg := upstream.Logger()
	lg.Debug().
		Str("method", method).
		Msg("uncordoning upstream to enable routing")

	tm := t.getUpsMetrics(upstreamKey{upstream, method})
	tm.Cordoned.Store(false)
	tm.LastCordonedReason.Store("")

	t.getCordonedGauge(upstream, method, reason).Set(0)
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

func (t *Tracker) RecordUpstreamDurationStart(upstream common.Upstream, method string, compositeType string, finality common.DataFinalityState, userId string) *Timer {
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
		userId:        userId,
	}
}

func (t *Tracker) RecordUpstreamDuration(up common.Upstream, method string, d time.Duration, isSuccess bool, comp string, finality common.DataFinalityState, userId string) {
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
	// Use cached observer to avoid per-request MetricVec lookups/locks.
	obs := t.getUpstreamRequestDurationObserver(up, method, comp, finality, userId)
	obs.Observe(sec)
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

func (t *Tracker) RecordUpstreamMisbehavior(up common.Upstream, method string) {
	for _, k := range t.getUpsKeys(up, method) {
		t.getUpsMetrics(k).MisbehaviorsTotal.Add(1)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		t.getNtwMetrics(nk).MisbehaviorsTotal.Add(1)
	}
}

func (t *Tracker) RecordUpstreamSelfRateLimited(up common.Upstream, method string, req *common.NormalizedRequest) {
	for _, k := range t.getUpsKeys(up, method) {
		t.getUpsMetrics(k).SelfRateLimitedTotal.Add(1)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		t.getNtwMetrics(nk).SelfRateLimitedTotal.Add(1)
	}

	var userId, agentName string
	if req != nil {
		userId = req.UserId()
		agentName = req.AgentName()
	} else {
		userId = "n/a"
		agentName = "unknown"
	}

	t.getSelfRateLimitedCounter(up, method, userId, agentName).Inc()
}

func (t *Tracker) RecordUpstreamRemoteRateLimited(up common.Upstream, method string, req *common.NormalizedRequest) {
	for _, k := range t.getUpsKeys(up, method) {
		t.getUpsMetrics(k).RemoteRateLimitedTotal.Add(1)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		t.getNtwMetrics(nk).RemoteRateLimitedTotal.Add(1)
	}

	var userId, agentName string
	if req != nil {
		userId = req.UserId()
		agentName = req.AgentName()
	} else {
		userId = "n/a"
		agentName = "unknown"
	}

	t.getRemoteRateLimitedCounter(up, method, userId, agentName).Inc()
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

// updateNetworkLagMetrics updates lag metrics for all upstreams in a network
// This is a DRY helper to avoid code duplication between SetLatestBlockNumber and SetFinalizedBlockNumber
func (t *Tracker) updateNetworkLagMetrics(
	net string,
	networkValue int64,
	getUpstreamValue func(*NetworkMetadata) int64,
	setLag func(*TrackedMetrics, int64),
	getGauge func(string, string, string, string) prometheus.Gauge,
	lg *zerolog.Logger,
) {
	// Use the pre-built index to avoid ranging over ALL upstreams
	t.mu.RLock()
	relevantKeys := t.upstreamsByNetwork[net]
	t.mu.RUnlock()

	if len(relevantKeys) == 0 {
		// Fallback only if index not ready - this should be rare
		t.upsMetrics.Range(func(key, value any) bool {
			k, ok := key.(upstreamKey)
			if !ok || k.ups == nil {
				return true
			}
			if k.ups.NetworkId() != net {
				return true
			}
			tm := value.(*TrackedMetrics)
			upsMeta := t.getMetadata(metadataKey{k.ups, net})
			upsValue := getUpstreamValue(upsMeta)
			if upsValue <= 0 {
				lg.Debug().
					Str("upstreamId", k.ups.Id()).
					Int64("value", upsValue).
					Msg("ignoring lag tracking for non-positive value")
				return true
			}
			lag := networkValue - upsValue
			setLag(tm, lag)
			gauge := getGauge(t.projectId, k.ups.VendorName(), k.ups.NetworkLabel(), k.ups.Id())
			gauge.Set(float64(lag))
			return true
		})
	} else {
		// Optimized path: iterate only relevant keys for this network
		for _, k := range relevantKeys {
			if k.ups == nil {
				continue
			}
			if v, ok := t.upsMetrics.Load(k); ok {
				tm := v.(*TrackedMetrics)
				upsMeta := t.getMetadata(metadataKey{k.ups, net})
				upsValue := getUpstreamValue(upsMeta)
				if upsValue <= 0 {
					lg.Debug().
						Str("upstreamId", k.ups.Id()).
						Int64("value", upsValue).
						Msg("ignoring lag tracking for non-positive value")
					continue
				}
				lag := networkValue - upsValue
				setLag(tm, lag)
				gauge := getGauge(t.projectId, k.ups.VendorName(), k.ups.NetworkLabel(), k.ups.Id())
				gauge.Set(float64(lag))
			}
		}
	}
}

// updateSingleUpstreamLag updates lag metrics for a single upstream
func (t *Tracker) updateSingleUpstreamLag(
	id string,
	net string,
	lag int64,
	setLag func(*TrackedMetrics, int64),
) {
	// Use the pre-built index when available
	t.mu.RLock()
	relevantKeys := t.upstreamsByNetwork[net]
	t.mu.RUnlock()

	if len(relevantKeys) == 0 {
		// Fallback for safety if index not yet populated
		t.upsMetrics.Range(func(key, value any) bool {
			k, ok := key.(upstreamKey)
			if !ok || k.ups == nil {
				return true
			}
			if k.ups.Id() == id && k.ups.NetworkId() == net {
				tm := value.(*TrackedMetrics)
				setLag(tm, lag)
			}
			return true
		})
	} else {
		// Optimized path: check only relevant keys
		for _, k := range relevantKeys {
			if k.ups != nil && k.ups.Id() == id {
				if v, ok := t.upsMetrics.Load(k); ok {
					tm := v.(*TrackedMetrics)
					setLag(tm, lag)
				}
			}
		}
	}
}

func (t *Tracker) SetLatestBlockNumber(upstream common.Upstream, blockNumber int64, blockTimestamp int64) {
	id := upstream.Id()
	net := upstream.NetworkId()
	netLabel := upstream.NetworkLabel()
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
		g := t.getLatestBlockGauge(t.projectId, "*", netLabel, "*")
		g.Set(float64(blockNumber))
		needsGlobalUpdate = true

		// Atomically update timestamp when network-level block number is updated
		if blockTimestamp > 0 {
			oldTimestamp := ntwMeta.evmLatestBlockTimestamp.Load()
			if blockTimestamp > oldTimestamp {
				ntwMeta.evmLatestBlockTimestamp.Store(blockTimestamp)

				// Calculate and record distance metric from EVM state poller
				currentTime := time.Now().Unix()
				distance := currentTime - blockTimestamp
				telemetry.MetricNetworkLatestBlockTimestampDistance.WithLabelValues(
					t.projectId,
					netLabel,
					"evm_state_poller",
				).Set(float64(distance))
			}
		}
	}

	// 2) Update this upstream's latest block
	upsMeta := t.getMetadata(mdKey)
	oldUpsVal := upsMeta.evmLatestBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmLatestBlockNumber.Store(blockNumber)
		g := t.getLatestBlockGauge(t.projectId, vendor, netLabel, id)
		g.Set(float64(blockNumber))
	}

	// 3) Recompute block head lag for this upstream
	ntwBn := ntwMeta.evmLatestBlockNumber.Load()
	if ntwBn <= 0 {
		lg.Warn().Int64("value", ntwBn).Msg("ignoring block head lag tracking for non-positive block number in tracker")
		return
	}

	upsLag := ntwBn - upsMeta.evmLatestBlockNumber.Load()
	gLag := t.getHeadLagGauge(t.projectId, vendor, netLabel, id)
	gLag.Set(float64(upsLag))

	// 4) Update the TrackedMetrics.BlockHeadLag fields for Upstream(s)
	if needsGlobalUpdate {
		// Recompute for every upstream in the network
		t.updateNetworkLagMetrics(
			net,
			ntwBn,
			func(meta *NetworkMetadata) int64 { return meta.evmLatestBlockNumber.Load() },
			func(tm *TrackedMetrics, lag int64) { tm.BlockHeadLag.Store(lag) },
			t.getHeadLagGauge,
			&lg,
		)
	} else {
		// Only update items for this single upstream
		t.updateSingleUpstreamLag(
			id,
			net,
			upsLag,
			func(tm *TrackedMetrics, lag int64) { tm.BlockHeadLag.Store(lag) },
		)
	}
}

func (t *Tracker) SetLatestBlockNumberForNetwork(network string, blockNumber int64) {
	ntwMeta := t.getMetadata(metadataKey{nil, network})
	ntwMeta.evmLatestBlockNumber.Store(blockNumber)
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
	netLabel := upstream.NetworkLabel()
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
		g := t.getFinalizedBlockGauge(t.projectId, "*", netLabel, "*")
		g.Set(float64(blockNumber))
		needsGlobalUpdate = true
	}

	// Update this upstream's finalized block
	oldUpsVal := upsMeta.evmFinalizedBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmFinalizedBlockNumber.Store(blockNumber)
		g := t.getFinalizedBlockGauge(t.projectId, vendor, netLabel, id)
		g.Set(float64(blockNumber))
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
	gLag := t.getFinalizationLagGauge(t.projectId, vendor, netLabel, id)
	gLag.Set(float64(upsLag))

	// Update the finalization lag across the network if needed
	if needsGlobalUpdate {
		// Recompute for every upstream in the network
		t.updateNetworkLagMetrics(
			net,
			ntwVal,
			func(meta *NetworkMetadata) int64 { return meta.evmFinalizedBlockNumber.Load() },
			func(tm *TrackedMetrics, lag int64) { tm.FinalizationLag.Store(lag) },
			t.getFinalizationLagGauge,
			&lg,
		)
	} else {
		// Only update finalization lag for this single upstream
		t.updateSingleUpstreamLag(
			id,
			net,
			upsLag,
			func(tm *TrackedMetrics, lag int64) { tm.FinalizationLag.Store(lag) },
		)
	}
}

func (t *Tracker) RecordBlockHeadLargeRollback(upstream common.Upstream, finality string, currentVal, newVal int64) {
	rollback := currentVal - newVal

	net := upstream.NetworkId()
	lg := upstream.Logger().With().Str("networkId", net).Logger()
	lg.Debug().
		Int64("currentValue", currentVal).
		Int64("newValue", newVal).
		Int64("rollback", rollback).
		Msgf("recording block rollback in tracker")

	t.getRollbackGauge(upstream).Set(float64(rollback))
}
