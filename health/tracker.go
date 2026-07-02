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

// upstreamKey represents "upstream × method × finality".
// A nil Upstream means the "all-upstreams" aggregate;
// method "*" means the "all-methods" aggregate;
// finality `DataFinalityStateAll` (-1) means the "all-finalities"
// aggregate. The finality axis is only populated when the tracker's
// `trackByFinality` flag is on — see `Tracker.EnableFinalityTracking`.
// When the flag is off, only the all-finalities slot is ever written
// to, and reads of a specific-finality key transparently fall back to
// it via `GetUpstreamMethodMetrics`.
type upstreamKey struct {
	ups      common.Upstream
	method   string
	finality common.DataFinalityState
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

	// Dynamic block time via EMA on on-chain block timestamps.
	// Uses block.timestamp (integer seconds) normalized by block count gap.
	// For fast chains where consecutive blocks share the same timestamp,
	// samples are skipped until the timestamp advances, then blockGap
	// normalization recovers sub-second precision.
	// Callers should use GetNetworkBlockTime which returns 0 until enough
	// samples have been collected.
	evmBlockTime              atomic.Int64 // computed result in nanoseconds (read by consumers)
	evmBlockTimeMu            sync.Mutex   // protects EMA state below
	evmBlockTimePrevBlock     int64        // last block number fed to EMA
	evmBlockTimePrevTimestamp int64        // block.timestamp (seconds) of that block
	evmBlockTimeEmaNs         float64      // current EMA value in nanoseconds
	evmBlockTimeSamples       int          // number of EMA samples collected
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

// TrackedMetrics is the per-(upstream, method) rolling-window
// observation set the policy engine reads on every eval. Every
// counter + the quantile sketch is sliding-window (rollingBuckets
// sub-buckets each), so degradations show up within
// windowSize/rollingBuckets seconds (a few hundred ms at the
// production default) rather than the full window-tumble cliff the
// previous single-bucket design produced.
//
// `BlockHeadLag` / `FinalizationLag` stay as plain atomic.Int64 —
// they're STATE metrics fed continuously by state pollers, not
// counts, so rolling-window semantics don't apply.
type TrackedMetrics struct {
	ResponseQuantiles      *QuantileTracker `json:"responseQuantiles"`
	ErrorsTotal            *RollingCounter  `json:"errorsTotal"`
	RemoteRateLimitedTotal *RollingCounter  `json:"remoteRateLimitedTotal"`
	RequestsTotal          *RollingCounter  `json:"requestsTotal"`
	MisbehaviorsTotal      *RollingCounter  `json:"misbehaviorsTotal"`
	BlockHeadLag           atomic.Int64     `json:"blockHeadLag"`
	FinalizationLag        atomic.Int64     `json:"finalizationLag"`
	Cordoned               atomic.Bool      `json:"cordoned"`
	LastCordonedReason     atomic.Value     `json:"lastCordonedReason"`
	// CordonedAtMs is unix-millis when Cordoned was last flipped true.
	// `0` means not cordoned (or never cordoned). Read by Uncordon to
	// observe the cordon-duration histogram for dashboards.
	CordonedAtMs atomic.Int64 `json:"cordonedAtMs"`

	// LastAccessedAtMs is unix-millis of the last Record* or Get*
	// touching this entry. Drives the tracker's idle-sweep — entries
	// that haven't been touched in `idleAfter` get evicted from the
	// `upsMetrics` / `ntwMetrics` maps + their matching Prometheus
	// MetricVec label sets.
	//
	// Defends against method-flood: a hostile client hitting random
	// JSON-RPC method names (`eth_random1`, `eth_random2`, ...) could
	// otherwise grow the per-method map without bound. With idle
	// sweep, stale methods drop out shortly after the attack stops.
	//
	// Set once on construction (so freshly-allocated entries don't
	// look idle to the very next sweep tick) and refreshed on every
	// hot-path write.
	LastAccessedAtMs atomic.Int64 `json:"-"`
}

// newTrackedMetrics constructs an empty TrackedMetrics with all
// rolling-window components initialized. Used at every sync.Map insert
// in the tracker so callers never see a half-built record.
func newTrackedMetrics(logger *zerolog.Logger) *TrackedMetrics {
	tm := &TrackedMetrics{
		ResponseQuantiles:      NewQuantileTracker(logger),
		ErrorsTotal:            NewRollingCounter(),
		RemoteRateLimitedTotal: NewRollingCounter(),
		RequestsTotal:          NewRollingCounter(),
		MisbehaviorsTotal:      NewRollingCounter(),
	}
	// Seed LastAccessedAtMs to "now" so a brand-new entry doesn't
	// look idle to a sweep that fires before the first hot-path write.
	tm.LastAccessedAtMs.Store(time.Now().UnixMilli())
	return tm
}

// touch refreshes the per-entry idle timestamp. Called from every
// Record* and Get* path. Cheap: one atomic store; avoids time.Now()
// when the millisecond hasn't advanced (a request burst within the
// same ms keeps the same value).
func (m *TrackedMetrics) touch(nowMs int64) {
	m.LastAccessedAtMs.Store(nowMs)
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
	throttled := float64(m.RemoteRateLimitedTotal.Load())
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

// Rotate advances every rolling-window component forward by one
// sub-bucket: the OLDEST bucket gets zeroed (its slice of the window
// drops out) and a fresh slot opens at the newest position for
// incoming samples. Called periodically by the tracker's
// rotateMetricsLoop at windowSize/rollingBuckets cadence.
//
// State metrics (BlockHeadLag, FinalizationLag) and Cordoned status
// are untouched — they reflect deliberate decisions or current chain
// conditions, not cumulative counts. Cordoning in particular is the
// strongest "do not use" signal and must be cleared explicitly via
// `Uncordon()`, not by a tick of the clock.
func (m *TrackedMetrics) Rotate() {
	m.ErrorsTotal.RotateOldest()
	m.RequestsTotal.RotateOldest()
	m.RemoteRateLimitedTotal.RotateOldest()
	m.MisbehaviorsTotal.RotateOldest()
	m.ResponseQuantiles.RotateOldest()
}

// Reset wipes every counter and the quantile sketch fully. Test/admin
// helper — the request path uses Rotate.
func (m *TrackedMetrics) Reset() {
	m.ErrorsTotal.Wipe()
	m.RequestsTotal.Wipe()
	m.RemoteRateLimitedTotal.Wipe()
	m.MisbehaviorsTotal.Wipe()
	m.ResponseQuantiles.Reset()
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

	// trackByFinality switches Record* between a 2-key write (current
	// behavior — only the all-finalities aggregate) and a 4-key write
	// (per-finality + all-finalities + cross-method finality rollups).
	// The policy engine flips this to true via EnableFinalityTracking
	// at network-registration time when any network's `EvalScope`
	// includes finality. Once true it stays true — flipping back
	// would orphan partially-populated keys and confuse the eval.
	//
	// Reads are atomic.Bool (1 cycle in the request path) so the
	// off-path stays as cheap as today; the cost only shows up for
	// projects that opted into per-finality scoping.
	trackByFinality atomic.Bool

	// idleEvictionAfter is the duration past which an unaccessed
	// (upstream, method, finality) or (network, method) entry gets
	// evicted from upsMetrics / ntwMetrics. Defaults to 30 min — well
	// above any realistic `scoreMetricsWindowSize` (10s simulator …
	// 5m production) so we never evict an entry that's still
	// contributing to the rolling window. Zero disables sweeping.
	//
	// Defends against method-flood: a hostile client hammering
	// random JSON-RPC method names (`eth_random1`, `eth_random2`,
	// ...) would otherwise grow the per-method map without bound.
	// With idle sweep, stale method buckets drop out a few sweep
	// intervals after the attack stops, the matching Prometheus
	// MetricVec label-sets get explicitly deleted, and steady-state
	// memory tracks ACTUAL request patterns rather than peak
	// adversarial cardinality.
	idleEvictionAfter time.Duration

	// Cache of pre-bound Prometheus observers for upstream request duration
	// Keyed by the full label set to avoid per-request MetricVec map lookups.
	urdObsCache sync.Map // map[urdoKey]prometheus.Observer

	// Caches for other hot-path metrics
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

// cachedObserver wraps a Prometheus Observer with an idle timestamp so
// the tracker's sweep loop can evict label-sets that haven't received
// an observation in `idleEvictionAfter` — and call
// `MetricVec.DeleteLabelValues(...)` to release the matching series
// from the Prometheus registry. Without this, every unique label
// combination a request EVER triggers stays in the registry forever
// (Prometheus's append-only model).
//
// Hot-path overhead: one atomic.Int64 store per cache hit. The cache
// itself is sync.Map (LoadOrStore on miss, Load on hit) — same as
// before, just unwrapping a pointer.
type cachedObserver struct {
	obs              prometheus.Observer
	lastAccessedAtMs atomic.Int64
}

// cachedCounter is the parallel wrapper for the rate-limited counter
// cache — same idle-tracking story, different Prom type.
type cachedCounter struct {
	ctr              prometheus.Counter
	lastAccessedAtMs atomic.Int64
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
	nowMs := time.Now().UnixMilli()
	if v, ok := t.urdObsCache.Load(key); ok {
		co := v.(*cachedObserver)
		co.lastAccessedAtMs.Store(nowMs)
		return co.obs
	}
	co := &cachedObserver{
		obs: telemetry.MetricUpstreamRequestDuration.WithLabelValues(
			key.project, key.vendor, key.network, key.upstream, key.category, key.composite, key.finality, key.user,
		),
	}
	co.lastAccessedAtMs.Store(nowMs)
	actual, _ := t.urdObsCache.LoadOrStore(key, co)
	return actual.(*cachedObserver).obs
}

// Reuse the same shape previously used for upstream rate limit counters to keep cache keys stable for remote.
type rrltKey struct {
	project   string
	vendor    string
	network   string
	upstream  string
	category  string
	user      string
	agentName string
	finality  string
}

func (t *Tracker) getRemoteRateLimitedCounter(up common.Upstream, method, userId, agentName, finality string) prometheus.Counter {
	key := rrltKey{t.projectId, up.VendorName(), up.NetworkLabel(), up.Id(), method, userId, agentName, finality}
	nowMs := time.Now().UnixMilli()
	if v, ok := t.remoteRateLimitedCounterCache.Load(key); ok {
		cc := v.(*cachedCounter)
		cc.lastAccessedAtMs.Store(nowMs)
		return cc.ctr
	}
	cc := &cachedCounter{
		ctr: telemetry.MetricRateLimitsTotal.WithLabelValues(
			key.project,   // project
			key.network,   // network
			key.vendor,    // vendor
			key.upstream,  // upstream
			key.category,  // category
			key.finality,  // finality
			key.user,      // user
			key.agentName, // agent_name
			"<remote>",    // budget
			"remote",      // scope (remote upstream)
			"",            // auth
			"upstream",    // origin
		),
	}
	cc.lastAccessedAtMs.Store(nowMs)
	actual, _ := t.remoteRateLimitedCounterCache.LoadOrStore(key, cc)
	return actual.(*cachedCounter).ctr
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

// DefaultIdleEvictionAfter is the conservative idle threshold the
// tracker uses for sweeping stale (method/network)-keyed entries when
// the caller doesn't override it. Long enough to safely survive a
// quiet period in production (5m scoreMetricsWindowSize) without
// evicting still-rotating buckets, short enough to bound memory
// under a method-flood that runs for hours.
const DefaultIdleEvictionAfter = 30 * time.Minute

// NewTracker constructs a new Tracker, using sync.Map for concurrency.
func NewTracker(logger *zerolog.Logger, projectId string, windowSize time.Duration) *Tracker {
	return &Tracker{
		logger:             logger,
		projectId:          projectId,
		windowSize:         windowSize,
		upstreamsByNetwork: make(map[string][]upstreamKey),
		idleEvictionAfter:  DefaultIdleEvictionAfter,
	}
}

// SetIdleEvictionAfter overrides the default idle eviction threshold.
// Pass `0` to disable sweeping entirely (matches pre-fix behavior).
// Used by tests that want to exercise eviction without waiting 30
// minutes, and by operators who want a tighter bound for
// high-cardinality-method workloads.
func (t *Tracker) SetIdleEvictionAfter(d time.Duration) {
	t.idleEvictionAfter = d
}

// Bootstrap starts the goroutine that rotates rolling-window buckets.
func (t *Tracker) Bootstrap(ctx context.Context) {
	go t.rotateMetricsLoop(ctx)
}

// rotateMetricsLoop advances every tracked metric's sliding window by
// one sub-bucket on each tick. The tick interval is
// windowSize / rollingBuckets — so a 5s window with 10 buckets
// rotates every 500ms, dropping ~10% of the accumulated data per
// rotation. Compared to the previous "wipe everything every
// windowSize" tumble, a freshly-degraded upstream's metrics start
// reflecting the new state within one rotation interval rather than
// waiting (worst case) the full windowSize.
func (t *Tracker) rotateMetricsLoop(ctx context.Context) {
	interval := t.windowSize / rollingBuckets
	if interval <= 0 {
		// Defensive — a misconfigured tracker (windowSize < rollingBuckets
		// or negative) would otherwise panic on time.NewTicker(0). Snap
		// to a 1ms floor; the caller almost certainly meant "fast".
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Tick counter so we can schedule the idle sweep at a coarser
	// cadence than rotation (every Nth tick) — sweep work is O(map size)
	// and shouldn't run every 100ms when rotation does.
	var rotationCount uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.upsMetrics.Range(func(key, value any) bool {
				if tm, ok := value.(*TrackedMetrics); ok {
					tm.Rotate()
				}
				return true
			})
			t.ntwMetrics.Range(func(key, value any) bool {
				if tm, ok := value.(*TrackedMetrics); ok {
					tm.Rotate()
				}
				return true
			})

			// Idle eviction: every `sweepEveryRotations` ticks (roughly
			// every `windowSize`), walk both maps and drop entries that
			// haven't been touched in `idleEvictionAfter`. Skipped when
			// the threshold is 0 (sweep disabled) or the rotation
			// counter hasn't reached the next sweep boundary yet.
			rotationCount++
			if t.idleEvictionAfter > 0 && rotationCount%sweepEveryRotations == 0 {
				t.sweepIdle()
			}
		}
	}
}

// sweepEveryRotations sets how often (in rotation ticks) the idle
// sweep runs. With rollingBuckets=10, sweep fires once per full
// rolling window — a 30 min threshold against a 1 min window means
// up to ~30 sweep passes between an entry going idle and being
// evicted (acceptable; the goal is bounded memory, not
// minute-precision eviction).
const sweepEveryRotations = uint64(rollingBuckets)

// sweepIdle removes (upstream, method, finality) and (network, method)
// entries from the in-memory tracker maps when they haven't been
// touched in `idleEvictionAfter`. Bounds memory under method-flood
// attacks while leaving steady-state hot keys alone.
//
// Skipped buckets:
//
//   - All-finalities wildcard (`(*, "*", All)`) — only the most-narrow
//     buckets are user-controllable; the wildcard rollups are bounded
//     by the upstream count and serve as the read-path fallback.
//   - Method "*" wildcard — same reasoning: the cross-method aggregate
//     is bounded; we evict the narrow per-method buckets only.
//   - Cordoned entries — admin-set state must not be evicted, ever.
//     If a cordoned upstream has been silent for 30 min the cordon
//     would otherwise vanish on the next request.
//
// Prometheus side: idle entries in the urdObsCache /
// remoteRateLimitedCounterCache get DeleteLabelValues'd on their
// parent MetricVec, releasing the registered series so the
// `/metrics` endpoint stops re-emitting stale label combos. Without
// this, even with the in-memory cache evicted, the Prometheus
// registry would keep the series forever (append-only model).
func (t *Tracker) sweepIdle() {
	cutoffMs := time.Now().Add(-t.idleEvictionAfter).UnixMilli()

	t.upsMetrics.Range(func(key, value any) bool {
		k := key.(upstreamKey)
		if k.method == "*" {
			return true // never evict the per-upstream wildcard rollup
		}
		tm := value.(*TrackedMetrics)
		if tm.Cordoned.Load() {
			return true // preserve admin-set cordon state
		}
		if tm.LastAccessedAtMs.Load() >= cutoffMs {
			return true
		}
		t.upsMetrics.Delete(key)
		return true
	})

	t.ntwMetrics.Range(func(key, value any) bool {
		k := key.(networkKey)
		if k.method == "*" {
			return true
		}
		tm := value.(*TrackedMetrics)
		if tm.LastAccessedAtMs.Load() >= cutoffMs {
			return true
		}
		t.ntwMetrics.Delete(key)
		return true
	})

	t.sweepIdleObservers(cutoffMs)
}

// sweepIdleObservers drops idle Prometheus observer/counter cache
// entries AND deletes their underlying MetricVec label-sets so the
// registry side of the cardinality blow-up actually shrinks. The
// cache key IS the label tuple — we reuse it directly for the
// DeleteLabelValues call, which is why the labels stayed structured
// throughout (the audit found the cache, the cache held the labels,
// the labels are what we now release).
func (t *Tracker) sweepIdleObservers(cutoffMs int64) {
	t.urdObsCache.Range(func(key, value any) bool {
		co := value.(*cachedObserver)
		if co.lastAccessedAtMs.Load() >= cutoffMs {
			return true
		}
		k := key.(urdoKey)
		t.urdObsCache.Delete(key)
		telemetry.MetricUpstreamRequestDuration.DeleteLabelValues(
			k.project, k.vendor, k.network, k.upstream, k.category, k.composite, k.finality, k.user,
		)
		return true
	})

	t.remoteRateLimitedCounterCache.Range(func(key, value any) bool {
		cc := value.(*cachedCounter)
		if cc.lastAccessedAtMs.Load() >= cutoffMs {
			return true
		}
		k := key.(rrltKey)
		t.remoteRateLimitedCounterCache.Delete(key)
		telemetry.MetricRateLimitsTotal.DeleteLabelValues(
			k.project, k.network, k.vendor, k.upstream, k.category, k.finality, k.user, k.agentName,
			"<remote>", "remote", "", "upstream",
		)
		return true
	})
}

// getUpsKeys expands a (upstream, method, finality) record into the
// set of bucket keys the Record* hot path must increment. When the
// engine hasn't opted into finality tracking, only the
// all-finalities rollups exist — that's the current behavior.
//
// Off (`trackByFinality == false`) — 2 keys:
//
//	(ups, method,    All)        — current per-method aggregate
//	(ups, "*",       All)        — current any-method aggregate
//
// On (`trackByFinality == true`)  — 4 keys:
//
//	(ups, method,    finality)   — most specific
//	(ups, method,    All)        — cross-finality rollup per method
//	(ups, "*",       finality)   — cross-method rollup per finality
//	(ups, "*",       All)        — full wildcard (== off-mode any-method)
//
// Off-mode behavior is identical to pre-finality tracker — same key
// count, same key shapes (with finality=All filling the slot that
// used to be implicit).
func (t *Tracker) getUpsKeys(upstream common.Upstream, method string, finality common.DataFinalityState) []upstreamKey {
	if !t.trackByFinality.Load() {
		return []upstreamKey{
			{upstream, method, common.DataFinalityStateAll},
			{upstream, "*", common.DataFinalityStateAll},
		}
	}
	// Avoid a 4th identical write when the caller passed All directly
	// (no specific finality known). Falls back to the 2-key set with
	// the per-method + any-method aggregates.
	if finality == common.DataFinalityStateAll {
		return []upstreamKey{
			{upstream, method, common.DataFinalityStateAll},
			{upstream, "*", common.DataFinalityStateAll},
		}
	}
	return []upstreamKey{
		{upstream, method, finality},
		{upstream, method, common.DataFinalityStateAll},
		{upstream, "*", finality},
		{upstream, "*", common.DataFinalityStateAll},
	}
}

// EnableFinalityTracking flips the tracker into 4-key mode so
// subsequent Record* writes populate per-(method, finality) entries
// in addition to the all-finalities rollups. Idempotent + monotonic
// — once on, stays on, because flipping back would orphan partial
// keys and starve the eval that depends on them. Safe to call
// concurrently with Record* via atomic.Bool semantics; a flip races
// with at most one in-flight Record* which will see the old value
// and write 2 keys instead of 4 (a single missing tick of
// per-finality data on the boundary, indistinguishable from the
// natural sliding-window noise).
func (t *Tracker) EnableFinalityTracking() {
	t.trackByFinality.Store(true)
}

// IsFinalityTracked reports whether the tracker is currently writing
// per-finality keys. Used by diagnostic surfaces (admin, simulator)
// to label their output with the active grain.
func (t *Tracker) IsFinalityTracked() bool {
	return t.trackByFinality.Load()
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
	tm := newTrackedMetrics(t.logger)
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
	tm := newTrackedMetrics(t.logger)
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

	// Cordon state is finality-agnostic — operators cordon "drpc for
	// eth_call", not "drpc for eth_call when reading finalized data".
	// Store on the all-finalities key so every finality-specific
	// lookup sees the same cordon flag.
	tm := t.getUpsMetrics(upstreamKey{upstream, method, common.DataFinalityStateAll})
	wasCordoned := tm.Cordoned.Swap(true)
	tm.LastCordonedReason.Store(reason)
	if !wasCordoned {
		// Only record the start timestamp on the OFF→ON transition so
		// repeated cordons (e.g. operator updating the reason) don't
		// reset the duration accounting mid-cordon.
		tm.CordonedAtMs.Store(time.Now().UnixMilli())
		telemetry.MetricUpstreamCordonEventTotal.WithLabelValues(
			t.projectId, upstream.NetworkId(), upstream.Id(), "cordon",
		).Inc()
	}

	t.getCordonedGauge(upstream, method, reason).Set(1)
}

func (t *Tracker) Uncordon(upstream common.Upstream, method string, reason string) {
	lg := upstream.Logger()
	lg.Debug().
		Str("method", method).
		Msg("uncordoning upstream to enable routing")

	// Cordon state is finality-agnostic — operators cordon "drpc for
	// eth_call", not "drpc for eth_call when reading finalized data".
	// Store on the all-finalities key so every finality-specific
	// lookup sees the same cordon flag.
	tm := t.getUpsMetrics(upstreamKey{upstream, method, common.DataFinalityStateAll})
	wasCordoned := tm.Cordoned.Swap(false)
	tm.LastCordonedReason.Store("")
	if wasCordoned {
		startedMs := tm.CordonedAtMs.Swap(0)
		if startedMs > 0 {
			dur := time.Duration(time.Now().UnixMilli()-startedMs) * time.Millisecond
			if dur > 0 {
				telemetry.MetricUpstreamCordonDurationSeconds.WithLabelValues(
					t.projectId, upstream.NetworkId(), upstream.Id(),
				).Observe(dur.Seconds())
			}
		}
		telemetry.MetricUpstreamCordonEventTotal.WithLabelValues(
			t.projectId, upstream.NetworkId(), upstream.Id(), "uncordon",
		).Inc()
	}

	t.getCordonedGauge(upstream, method, reason).Set(0)
}

// IsCordoned checks if (ups, network, method) or (ups, network, "*") is cordoned.
// Cordon flags live on the all-finalities key (see Cordon).
func (t *Tracker) IsCordoned(upstream common.Upstream, method string) bool {
	if val, ok := t.upsMetrics.Load(upstreamKey{upstream, "*", common.DataFinalityStateAll}); ok {
		if val.(*TrackedMetrics).Cordoned.Load() {
			return true
		}
	}
	if val, ok := t.upsMetrics.Load(upstreamKey{upstream, method, common.DataFinalityStateAll}); ok {
		return val.(*TrackedMetrics).Cordoned.Load()
	}
	return false
}

// CordonedReason returns the cordon reason and whether the (upstream,
// method) is currently cordoned. Falls back to the wildcard (`"*"`)
// cordon if the specific method scope isn't cordoned. Used by admin
// endpoints + tooling to surface "why is this upstream out" without
// going through the metrics-snapshot JSON path.
func (t *Tracker) CordonedReason(upstream common.Upstream, method string) (string, bool) {
	if val, ok := t.upsMetrics.Load(upstreamKey{upstream, "*", common.DataFinalityStateAll}); ok {
		tm := val.(*TrackedMetrics)
		if tm.Cordoned.Load() {
			if r, ok := tm.LastCordonedReason.Load().(string); ok {
				return r, true
			}
			return "", true
		}
	}
	if val, ok := t.upsMetrics.Load(upstreamKey{upstream, method, common.DataFinalityStateAll}); ok {
		tm := val.(*TrackedMetrics)
		if tm.Cordoned.Load() {
			if r, ok := tm.LastCordonedReason.Load().(string); ok {
				return r, true
			}
			return "", true
		}
	}
	return "", false
}

// ------------------------------------
// Basic Request & Failure Tracking
// ------------------------------------

func (t *Tracker) RecordUpstreamRequest(up common.Upstream, method string, finality common.DataFinalityState) {
	nowMs := time.Now().UnixMilli()
	for _, k := range t.getUpsKeys(up, method, finality) {
		tm := t.getUpsMetrics(k)
		tm.RequestsTotal.Add(1)
		tm.touch(nowMs)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		tm := t.getNtwMetrics(nk)
		tm.RequestsTotal.Add(1)
		tm.touch(nowMs)
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
		// Feed the quantile only when `isSuccess` — the caller already
		// filtered to outcomes whose duration is a real latency signal:
		// successful responses, EVM reverts (upstream did real work),
		// and canceled-by-hedge / client-disconnect / engine-timeout
		// observations (lower-bound on completion time, important for
		// scoring upstreams that lose every hedge race).
		// Hard upstream errors (connection refused, server 5xx,
		// throttling) stay out so an upstream that's failing fast
		// doesn't get crowned "fastest in the pool".
		nowMs := time.Now().UnixMilli()
		for _, k := range t.getUpsKeys(up, method, finality) {
			tm := t.getUpsMetrics(k)
			tm.ResponseQuantiles.Add(sec)
			tm.touch(nowMs)
		}
		for _, nk := range t.getNtwKeys(up, method) {
			tm := t.getNtwMetrics(nk)
			tm.ResponseQuantiles.Add(sec)
			tm.touch(nowMs)
		}
	}
	// Use cached observer to avoid per-request MetricVec lookups/locks.
	obs := t.getUpstreamRequestDurationObserver(up, method, comp, finality, userId)
	obs.Observe(sec)
}

func (t *Tracker) RecordUpstreamFailure(up common.Upstream, method string, finality common.DataFinalityState, err error) {
	// Ignore errors that do not reflect upstream quality:
	// - ExecutionException: valid blockchain state (e.g. revert)
	// - ExcludedByPolicy / RequestSkipped / Shadowing: internal routing decisions
	// - Unsupported: capability, not quality
	// - CapacityExceeded: remote 429, already penalized via ThrottledRate
	// - ClientSideException: user sent a bad request, not upstream's fault
	// - RequestCanceled / HedgeCancelled: indistinguishable from a client
	//   disconnect at this layer (both surface as context cancellation). Hedge
	//   attempts are excluded from RequestsTotal / ErrorsTotal at the call
	//   site in upstream.tryForward, so cancellations only reach here for
	//   primary attempts — where they almost always mean "client gave up,"
	//   not "upstream failed."  Slowness is already captured by ResponseQuantiles.
	if common.HasErrorCode(
		err,
		common.ErrCodeEndpointExecutionException,
		common.ErrCodeUpstreamExcludedByPolicy,
		common.ErrCodeUpstreamRequestSkipped,
		common.ErrCodeUpstreamShadowing,
		common.ErrCodeEndpointUnsupported,
		common.ErrCodeEndpointCapacityExceeded,
		common.ErrCodeEndpointClientSideException,
		common.ErrCodeEndpointRequestCanceled,
		common.ErrCodeUpstreamHedgeCancelled,
	) {
		return
	}

	nowMs := time.Now().UnixMilli()
	for _, k := range t.getUpsKeys(up, method, finality) {
		tm := t.getUpsMetrics(k)
		tm.ErrorsTotal.Add(1)
		tm.touch(nowMs)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		tm := t.getNtwMetrics(nk)
		tm.ErrorsTotal.Add(1)
		tm.touch(nowMs)
	}
}

func (t *Tracker) RecordUpstreamMisbehavior(up common.Upstream, method string, finality common.DataFinalityState) {
	nowMs := time.Now().UnixMilli()
	for _, k := range t.getUpsKeys(up, method, finality) {
		tm := t.getUpsMetrics(k)
		tm.MisbehaviorsTotal.Add(1)
		tm.touch(nowMs)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		tm := t.getNtwMetrics(nk)
		tm.MisbehaviorsTotal.Add(1)
		tm.touch(nowMs)
	}
}

func (t *Tracker) RecordUpstreamRemoteRateLimited(ctx context.Context, up common.Upstream, method string, req *common.NormalizedRequest) {
	var finality common.DataFinalityState
	var userId, agentName, finalityStr string
	if req != nil {
		userId = req.UserId()
		agentName = req.AgentName()
		finality = req.Finality(ctx)
		finalityStr = finality.String()
	} else {
		userId = "n/a"
		agentName = "unknown"
		finality = common.DataFinalityStateAll
	}

	nowMs := time.Now().UnixMilli()
	for _, k := range t.getUpsKeys(up, method, finality) {
		tm := t.getUpsMetrics(k)
		tm.RemoteRateLimitedTotal.Add(1)
		tm.touch(nowMs)
	}
	for _, nk := range t.getNtwKeys(up, method) {
		tm := t.getNtwMetrics(nk)
		tm.RemoteRateLimitedTotal.Add(1)
		tm.touch(nowMs)
	}

	t.getRemoteRateLimitedCounter(up, method, userId, agentName, finalityStr).Inc()
}

// --------------------------------------------
// Accessors
// --------------------------------------------

// GetUpstreamMethodMetrics returns the rolling-window metrics for the
// (upstream, method, finality) bucket. When per-finality tracking is
// off OR the specific bucket has no recorded data, the lookup
// transparently falls back to the all-finalities aggregate so the
// eval is never starved of signal. Pass `DataFinalityStateAll` to
// request the rollup directly.
//
// Touches `LastAccessedAtMs` so an actively-read entry (the policy
// engine's per-tick eval reads every upstream's metrics) is treated
// as "alive" by the idle sweep — otherwise a chain with all writes
// going to the wildcard slot could see specific-bucket entries
// evicted out from under the read path.
func (t *Tracker) GetUpstreamMethodMetrics(up common.Upstream, method string, finality common.DataFinalityState) *TrackedMetrics {
	nowMs := time.Now().UnixMilli()
	if finality == common.DataFinalityStateAll || !t.trackByFinality.Load() {
		tm := t.getUpsMetrics(upstreamKey{up, method, common.DataFinalityStateAll})
		tm.touch(nowMs)
		return tm
	}
	// Use the LIVE map (don't lazy-create) for the specific-finality
	// bucket — if no Record* has fed it yet, fall through to the
	// aggregate rather than handing the eval an empty
	// counters/quantile sketch.
	if v, ok := t.upsMetrics.Load(upstreamKey{up, method, finality}); ok {
		tm := v.(*TrackedMetrics)
		tm.touch(nowMs)
		return tm
	}
	tm := t.getUpsMetrics(upstreamKey{up, method, common.DataFinalityStateAll})
	tm.touch(nowMs)
	return tm
}

// GetUpstreamMetrics returns the per-method *TrackedMetrics map for
// `ups`, keyed by method name and exposing the all-finalities
// aggregate bucket per method (the key getUpsKeys always populates).
//
// HOT PATH — invoked once per upstream per slot tick from
// `policy.snapshotMetrics`. Earlier versions did `t.upsMetrics.Range`
// over the GLOBAL sync.Map and filtered to the target upstream; under
// a typical 50-network × 5-upstream × 30-method × 4-finality
// deployment that map is tens of thousands of entries, and the
// Range-then-`pk.ups.Id() == ups.Id()` filter consumed two thirds of
// total process CPU in a real pprof capture (sync.Map.Range = 32%
// flat, Upstream.Id = 22% flat, GetUpstreamMetrics.func1 = 10%
// flat). This implementation uses the pre-built
// `upstreamsByNetwork` index to scope the lookup to O(keys for
// this network), then direct-loads each candidate.
//
// Semantics match the previous behavior: returned map keys are method
// names, values are the all-finalities *TrackedMetrics (so callers
// that don't care about finality see a single deterministic snapshot
// per method).
func (t *Tracker) GetUpstreamMetrics(ups common.Upstream) map[string]*TrackedMetrics {
	targetID := ups.Id()
	net := ups.NetworkId()

	t.mu.RLock()
	relevantKeys := t.upstreamsByNetwork[net]
	t.mu.RUnlock()

	if len(relevantKeys) == 0 {
		// Cold fallback — index not populated yet (no Record* has
		// fed it). Identical semantics to the legacy walk; expected
		// to fire only on the first eval after a tracker boot, never
		// in steady state.
		out := map[string]*TrackedMetrics{}
		t.upsMetrics.Range(func(k, v any) bool {
			pk := k.(upstreamKey)
			if pk.ups != nil && pk.ups.Id() == targetID {
				out[pk.method] = v.(*TrackedMetrics)
			}
			return true
		})
		return out
	}

	// Hot path: O(per-network keys). `upstreamsByNetwork[net]` is
	// deduped on (ups_id, method) with an arbitrary finality stored
	// per entry — we ignore the stored finality and direct-load the
	// `DataFinalityStateAll` bucket, which `getUpsKeys` always
	// populates alongside any per-finality writes.
	out := make(map[string]*TrackedMetrics, len(relevantKeys))
	for _, k := range relevantKeys {
		if k.ups == nil || k.ups.Id() != targetID {
			continue
		}
		aggKey := upstreamKey{ups: ups, method: k.method, finality: common.DataFinalityStateAll}
		if v, ok := t.upsMetrics.Load(aggKey); ok {
			out[k.method] = v.(*TrackedMetrics)
		}
	}
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
				// Block-head/finalization lag is a per-upstream property, but the
				// (ups,method) dedup index stores a single, arbitrary-finality key
				// per method. Selection slots at the network and network-method
				// grains read the {method, All} rollup; if the indexed key was a
				// specific finality, that rollup is left starved and lag-based
				// predicates/scoring silently no-op. Mirror onto the All rollup
				// (which getUpsKeys always populates) so lag is seen at every grain.
				if k.finality != common.DataFinalityStateAll {
					if av, ok := t.upsMetrics.Load(upstreamKey{k.ups, k.method, common.DataFinalityStateAll}); ok {
						setLag(av.(*TrackedMetrics), lag)
					}
				}
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
				// Also mirror onto the {method, All} rollup — the indexed key may be
				// a per-finality slot rather than the {method, All} one (see
				// updateNetworkLagMetrics) — so per-method-grain reads see the lag.
				if k.finality != common.DataFinalityStateAll {
					if av, ok := t.upsMetrics.Load(upstreamKey{k.ups, k.method, common.DataFinalityStateAll}); ok {
						setLag(av.(*TrackedMetrics), lag)
					}
				}
			}
		}
	}
}

// recomputeNetworkBlockHead re-derives a network-level block head as the max of
// the stored per-upstream values (getVal picks the latest vs finalized axis).
// Called after an accepted per-upstream rollback: the network head is monotonic
// under forward progress, so the only safe way to move it backwards is to
// re-derive it from its inputs — adopting a single (possibly lagging) upstream's
// sample directly would let genuinely-behind upstreams drag the head down.
func (t *Tracker) recomputeNetworkBlockHead(net string, getVal func(*NetworkMetadata) int64) int64 {
	t.mu.RLock()
	relevantKeys := t.upstreamsByNetwork[net]
	t.mu.RUnlock()

	var maxVal int64
	seen := make(map[common.Upstream]struct{}, len(relevantKeys))
	consider := func(ups common.Upstream) {
		if ups == nil {
			return
		}
		if _, done := seen[ups]; done {
			return
		}
		seen[ups] = struct{}{}
		if v := getVal(t.getMetadata(metadataKey{ups, net})); v > maxVal {
			maxVal = v
		}
	}
	if len(relevantKeys) == 0 {
		// Fallback if the index is not ready — mirrors updateNetworkLagMetrics.
		t.upsMetrics.Range(func(key, _ any) bool {
			if k, ok := key.(upstreamKey); ok && k.ups != nil && k.ups.NetworkId() == net {
				consider(k.ups)
			}
			return true
		})
	} else {
		for _, k := range relevantKeys {
			consider(k.ups)
		}
	}
	return maxVal
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

		// Feed block.timestamp into EMA for dynamic block time estimation.
		// Uses on-chain timestamps (not local clock) so the EMA tracks actual
		// chain production rate, not our polling cadence. For fast chains where
		// consecutive blocks share the same integer-second timestamp, samples
		// are skipped until the timestamp advances; blockGap normalization
		// recovers sub-second precision.
		if blockTimestamp > 0 {
			t.updateBlockTimeSample(ntwMeta, netLabel, blockNumber, blockTimestamp)
		}

		// Atomically update timestamp when network-level block number is updated
		if blockTimestamp > 0 {
			ntwMeta.evmLatestBlockTimestamp.Store(blockTimestamp)

			detectedAtMs := time.Now().UnixMilli()
			distanceMs := detectedAtMs - blockTimestamp*1000
			telemetry.MetricNetworkLatestBlockTimestampDistance.WithLabelValues(
				t.projectId,
				netLabel,
				"evm_state_poller",
			).Set(float64(distanceMs) / 1000.0)
		}
	}

	// 2) Update this upstream's latest block
	upsMeta := t.getMetadata(mdKey)
	oldUpsVal := upsMeta.evmLatestBlockNumber.Load()
	if blockNumber > oldUpsVal {
		upsMeta.evmLatestBlockNumber.Store(blockNumber)
		g := t.getLatestBlockGauge(t.projectId, vendor, netLabel, id)
		g.Set(float64(blockNumber))
	} else if oldUpsVal-blockNumber > common.DefaultToleratedBlockHeadRollback {
		// The upstream reports a head far behind the one stored for it: treat it
		// as a correction (a deep reorg, or a previously recorded bogus sample),
		// mirroring the shared-state counter semantics. Max-only storage would
		// pin the bad sample — and every lag-based routing decision derived
		// from it — until process restart.
		upsMeta.evmLatestBlockNumber.Store(blockNumber)
		g := t.getLatestBlockGauge(t.projectId, vendor, netLabel, id)
		g.Set(float64(blockNumber))
		lg.Warn().
			Int64("previousValue", oldUpsVal).
			Int64("newValue", blockNumber).
			Msg("applied large latest block rollback for upstream in tracker")

		// The rolled-back value may have been the network head: re-derive it
		// from the remaining per-upstream values.
		if ntwVal := ntwMeta.evmLatestBlockNumber.Load(); ntwVal > blockNumber {
			recomputed := t.recomputeNetworkBlockHead(net, func(meta *NetworkMetadata) int64 {
				return meta.evmLatestBlockNumber.Load()
			})
			if recomputed > 0 && recomputed < ntwVal {
				ntwMeta.evmLatestBlockNumber.Store(recomputed)
				t.getLatestBlockGauge(t.projectId, "*", netLabel, "*").Set(float64(recomputed))
				needsGlobalUpdate = true
				lg.Warn().
					Int64("previousValue", ntwVal).
					Int64("newValue", recomputed).
					Msg("re-derived network latest block after upstream rollback in tracker")
			}
		}
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

	// The dedup index (`upstreamsByNetwork`) is not guaranteed to carry the
	// {*, All} wildcard aggregate for this upstream — it dedups per (id,
	// method) and the aggregate can be created lazily (e.g. a policy read, or
	// a poll firing before any request traffic indexes it). When it's absent,
	// the index-driven writes above fill every per-method bucket but leave the
	// {*, All} bucket — the one network-scope selection policies read — at 0,
	// so blockNumberLagAbove silently never fires. Write it directly here, the
	// same way request metrics always reach {*, All} via getUpsKeys.
	t.getUpsMetrics(upstreamKey{upstream, "*", common.DataFinalityStateAll}).BlockHeadLag.Store(upsLag)
}

func (t *Tracker) SetLatestBlockNumberForNetwork(network string, blockNumber int64) {
	ntwMeta := t.getMetadata(metadataKey{nil, network})
	ntwMeta.evmLatestBlockNumber.Store(blockNumber)
}

// ------------------------------------
// Dynamic Block Time (EMA)
// ------------------------------------

const (
	blockTimeEmaAlpha   = 0.1 // smoothing factor; effective window ~19 samples
	blockTimeMinSamples = 3   // minimum EMA samples before emitting (requires 4 observations total)
)

// updateBlockTimeSample feeds a new block into the EMA using on-chain
// block.timestamp (integer seconds). For fast chains where consecutive blocks
// share the same timestamp, we skip the sample and do NOT advance prev — the
// block gap accumulates until the timestamp ticks, then normalization recovers
// sub-second precision (e.g. 1s / 4 blocks = 250ms for Arbitrum).
func (t *Tracker) updateBlockTimeSample(ntwMeta *NetworkMetadata, netLabel string, blockNumber int64, blockTimestamp int64) {
	ntwMeta.evmBlockTimeMu.Lock()
	defer ntwMeta.evmBlockTimeMu.Unlock()

	prevBlock := ntwMeta.evmBlockTimePrevBlock
	prevTimestamp := ntwMeta.evmBlockTimePrevTimestamp

	// First observation: store as previous and return.
	if prevBlock == 0 {
		ntwMeta.evmBlockTimePrevBlock = blockNumber
		ntwMeta.evmBlockTimePrevTimestamp = blockTimestamp
		return
	}

	// Reject out-of-order or duplicate blocks. A block far behind prev means the
	// head was corrected by a large rollback: re-anchor so sampling resumes
	// instead of stalling until the chain re-passes the old (possibly bogus) prev.
	blockGap := blockNumber - prevBlock
	if blockGap <= 0 {
		if prevBlock-blockNumber > common.DefaultToleratedBlockHeadRollback {
			ntwMeta.evmBlockTimePrevBlock = blockNumber
			ntwMeta.evmBlockTimePrevTimestamp = blockTimestamp
		}
		return
	}

	// Skip if timestamp hasn't advanced (fast chains where blocks share the same second).
	// Do NOT advance prev — let blockGap accumulate until the timestamp ticks.
	timestampDeltaSec := blockTimestamp - prevTimestamp
	if timestampDeltaSec <= 0 {
		return
	}

	// Per-block sample in nanoseconds (seconds → ms → ns, divided by block count).
	sampleNs := float64(timestampDeltaSec) * 1e9 / float64(blockGap)

	// Update EMA.
	if ntwMeta.evmBlockTimeSamples == 0 {
		ntwMeta.evmBlockTimeEmaNs = sampleNs
	} else {
		ntwMeta.evmBlockTimeEmaNs = blockTimeEmaAlpha*sampleNs + (1-blockTimeEmaAlpha)*ntwMeta.evmBlockTimeEmaNs
	}
	ntwMeta.evmBlockTimeSamples++

	// Store current as previous.
	ntwMeta.evmBlockTimePrevBlock = blockNumber
	ntwMeta.evmBlockTimePrevTimestamp = blockTimestamp

	// Don't emit until we have enough samples.
	if ntwMeta.evmBlockTimeSamples < blockTimeMinSamples {
		return
	}

	blockTimeNs := int64(ntwMeta.evmBlockTimeEmaNs)

	// Sanity bounds: reject absurd values.
	// Reset the internal EMA to the last published value so that recovery from a
	// prolonged halt doesn't create a delayed spike when the EMA first crosses
	// back below the threshold (e.g. jumping from 2s to ~119s).
	if blockTimeNs < int64(10*time.Millisecond) || blockTimeNs > int64(120*time.Second) {
		if lastGood := ntwMeta.evmBlockTime.Load(); lastGood > 0 {
			ntwMeta.evmBlockTimeEmaNs = float64(lastGood)
		}
		return
	}

	ntwMeta.evmBlockTime.Store(blockTimeNs)

	telemetry.MetricNetworkDynamicBlockTime.WithLabelValues(
		t.projectId, netLabel,
	).Set(float64(time.Duration(blockTimeNs).Milliseconds()))
}

// GetNetworkBlockTime returns the EMA-estimated block time for a network.
// Returns 0 until at least blockTimeMinSamples have been collected.
func (t *Tracker) GetNetworkBlockTime(networkId string) time.Duration {
	ntwMeta := t.getMetadata(metadataKey{nil, networkId})

	if v := ntwMeta.evmBlockTime.Load(); v > 0 {
		return time.Duration(v)
	}

	return 0
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
	} else if oldUpsVal-blockNumber > common.DefaultToleratedBlockHeadRollback {
		// Same rollback semantics as SetLatestBlockNumber: accept large
		// corrections instead of pinning a bogus sample until restart.
		upsMeta.evmFinalizedBlockNumber.Store(blockNumber)
		g := t.getFinalizedBlockGauge(t.projectId, vendor, netLabel, id)
		g.Set(float64(blockNumber))
		lg.Warn().
			Int64("previousValue", oldUpsVal).
			Int64("newValue", blockNumber).
			Msg("applied large finalized block rollback for upstream in tracker")

		if ntwBn := ntwMeta.evmFinalizedBlockNumber.Load(); ntwBn > blockNumber {
			recomputed := t.recomputeNetworkBlockHead(net, func(meta *NetworkMetadata) int64 {
				return meta.evmFinalizedBlockNumber.Load()
			})
			if recomputed > 0 && recomputed < ntwBn {
				ntwMeta.evmFinalizedBlockNumber.Store(recomputed)
				t.getFinalizedBlockGauge(t.projectId, "*", netLabel, "*").Set(float64(recomputed))
				needsGlobalUpdate = true
				lg.Warn().
					Int64("previousValue", ntwBn).
					Int64("newValue", recomputed).
					Msg("re-derived network finalized block after upstream rollback in tracker")
			}
		}
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

	// Same {*, All} wildcard-aggregate guarantee as SetLatestBlockNumber (see
	// the comment there): the dedup index may not carry the "*" rollup, so
	// write it directly to keep finalization-lag-based scoring/predicates honest.
	t.getUpsMetrics(upstreamKey{upstream, "*", common.DataFinalityStateAll}).FinalizationLag.Store(upsLag)
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
