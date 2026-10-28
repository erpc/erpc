package health

import (
	"cmp"
	"context"
	"math"
	"slices"
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

	// On the network entry only (nil upstream). headMu serializes every head
	// publication for the network so a slow re-derivation can never overwrite a
	// newer one; reporters is every upstream that has reported a head here.
	headMu    sync.Mutex
	reporters []headReporter
	// refMovedAt is when the lag reference (evmLatestBlockNumber) last
	// rose; firstVotes are when this instance first received a hash vote
	// for each recent block number (ageHashVote); agesAt is when every
	// reporter's age gauges were last set. Guarded by headMu.
	refMovedAt time.Time
	firstVotes []firstVote
	agesAt     time.Time

	// On upstream entries only; guarded by the network entry's headMu.
	// headChangedAt is when this upstream's stored latest head last changed;
	// headBehindSince is when a peer was first seen proving the chain went on
	// without it (zero = not behind); headBenchedAt is when it was last benched
	// and headBenchRef the network head right after. They keep the clock's
	// monotonic reading, so a wall-clock step never makes a head look stalled.
	// headTsFloor is the newest block timestamp (seconds, never past the
	// clock when stored) seen for any of its heads, a lower bound on its
	// current head's (evmLatestBlockTimestamp is exact or 0). headStale is
	// the staleness verdict (see headIsBehind) of the last evaluation that
	// knew its head's timestamp: moves without one (a peer pod's poll through
	// shared state, a response suggestion) keep it only while a live peer
	// stays a stall window of blocks ahead.
	// headBenched is read lock-free by the served-tip ballots (see
	// Tracker.IsHeadBenched).
	headChangedAt   time.Time
	headBehindSince time.Time
	headBenchedAt   time.Time
	headBenchRef    int64
	headTsFloor     int64
	headStale       bool
	headBenched     atomic.Bool
	// headReport and finalizedReport are this upstream's last latest and
	// finalized reports, changed or not (see headReport). On upstream entries
	// only; guarded by the network entry's headMu.
	headReport      headReport
	finalizedReport headReport
	// hashVote is this upstream's newest hash vote's number (spec §6.3): a
	// header this instance received by push or fetched with cache reads
	// skipped; 0 before the first. Written under the network entry's
	// headMu, read without it by routing (HashVoteNumber). hashExpired: its
	// socket closed since. hashBehindSince is when the lag reference first
	// stood above it, zero while it does not; hashVotedAt is when the vote
	// arrived. On upstream entries only; guarded by the network entry's
	// headMu.
	hashVote        atomic.Int64
	hashExpired     bool
	hashBehindSince time.Time
	hashVotedAt     time.Time
	// ageGauges are this upstream's report-age gauges, number then hash,
	// bound to ageLabel, its network label; guarded by the network entry's
	// headMu.
	ageGauges [2]prometheus.Gauge
	ageLabel  string
	// cadence reads the upstream's backed-off fetch cadence
	// (SetFetchCadence); nil until its state poller exists. On upstream
	// entries only; guarded by the network entry's headMu.
	cadence func() time.Duration

	// Dynamic block time via EMA on on-chain block timestamps: on the network
	// entry from the corroborated head, on an upstream entry from that
	// upstream's own head (the head bench uses it while the network's is
	// unknown). Uses block.timestamp (integer seconds) normalized by block
	// count gap. For fast chains where consecutive blocks share the same
	// timestamp, samples are skipped until the timestamp advances, then
	// blockGap normalization recovers sub-second precision.
	// Callers should use GetNetworkBlockTime which returns 0 until the
	// first sample (sampleBlockTime).
	evmBlockTime              atomic.Int64 // computed result in nanoseconds (read by consumers)
	evmBlockTimeMu            sync.Mutex   // protects EMA state below
	evmBlockTimePrevBlock     int64        // last block number fed to EMA
	evmBlockTimePrevTimestamp int64        // block.timestamp (seconds) of that block
	evmBlockTimeEmaNs         float64      // current EMA value in nanoseconds
	evmBlockTimeSamples       int          // number of EMA samples collected
	// evmBlockTimeSpanStart: prev is a timestamp change, so a sample may
	// span from it.
	evmBlockTimeSpanStart bool
	// evmBlockTimeFloor is the shortest average block time the blocks fed
	// since the anchor allow, in nanoseconds: integer timestamps truncate
	// each end by under 1s, so (Δts − 1s) / Δblocks. 0 until the span
	// covers 2s. The anchor is the timestamp change the spans start from,
	// set again after a rollback or a halt.
	evmBlockTimeFloor           atomic.Int64
	evmBlockTimeAnchorBlock     int64
	evmBlockTimeAnchorTimestamp int64
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

// ObserveCancelled records a primary attempt cancelled because another
// attempt won the hedge race: the elapsed time is a lower bound on its
// latency.
func (t *Timer) ObserveCancelled() {
	duration := time.Since(t.start)
	t.tracker.RecordUpstreamCancelledDuration(t.upstream, t.method, duration, t.compositeType, t.finality, t.userId)
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

	// cancelledQuantiles holds the elapsed time of primary attempts
	// cancelled because another attempt won the hedge race: lower bounds
	// on their latency. Kept out of ResponseQuantiles, which also drives
	// adaptive timeouts and hedge delays; only PolicyQuantiles reads it.
	// Allocated on the first such cancellation.
	cancelledQuantiles atomic.Pointer[QuantileTracker]
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

// PolicyQuantiles reads qtiles off ResponseQuantiles merged with the
// cancelled-attempt lower bounds. Each answered or hedge-lost primary
// attempt then counts as min(latency, elapsed at cancel), so these
// quantiles never exceed the true ones. Without the lower bounds, an
// upstream whose every attempt loses a hedge race keeps an empty sketch
// and reads as 0 s, the fastest.
func (m *TrackedMetrics) PolicyQuantiles(qtiles []float64) []time.Duration {
	return m.ResponseQuantiles.quantilesWith(m.cancelledQuantiles.Load(), qtiles)
}

func (m *TrackedMetrics) addCancelled(logger *zerolog.Logger, seconds float64) {
	q := m.cancelledQuantiles.Load()
	if q == nil {
		q = NewQuantileTracker(logger)
		if !m.cancelledQuantiles.CompareAndSwap(nil, q) {
			q = m.cancelledQuantiles.Load()
		}
	}
	q.Add(seconds)
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
	if q := m.cancelledQuantiles.Load(); q != nil {
		q.RotateOldest()
	}
}

// Reset wipes every counter and the quantile sketch fully. Test/admin
// helper — the request path uses Rotate.
func (m *TrackedMetrics) Reset() {
	m.ErrorsTotal.Wipe()
	m.RequestsTotal.Wipe()
	m.RemoteRateLimitedTotal.Wipe()
	m.MisbehaviorsTotal.Wipe()
	m.ResponseQuantiles.Reset()
	if q := m.cancelledQuantiles.Load(); q != nil {
		q.Reset()
	}
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
	// benchHooks are the networks' bench-change callbacks (OnHeadBenchChange).
	benchHooks sync.Map // map[networkId]func()

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
	rollbackGaugeCache            sync.Map // map[rbKey]prometheus.Gauge

	// now is the clock of the head bench state machine.
	now func() time.Time
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

// rbKey is ubKey plus the block-head axis. The latest and finalized state
// pollers both record rollbacks for the same upstream, so the axis has to be
// part of the key or the two would share one gauge and overwrite each other.
type rbKey struct {
	project  string
	vendor   string
	network  string
	upstream string
	finality string
}

// finality is the head axis that rolled back: "latest" or "finalized".
func (t *Tracker) getRollbackGauge(up common.Upstream, finality string) prometheus.Gauge {
	key := rbKey{t.projectId, up.VendorName(), up.NetworkLabel(), up.Id(), finality}
	if v, ok := t.rollbackGaugeCache.Load(key); ok {
		return v.(prometheus.Gauge)
	}
	g := telemetry.MetricUpstreamBlockHeadLargeRollback.WithLabelValues(
		key.project, key.vendor, key.network, key.upstream, key.finality,
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
		now:                time.Now,
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

// SetClockForTest replaces the head bench clock. Call it before any head is
// recorded.
func (t *Tracker) SetClockForTest(now func() time.Time) {
	t.now = now
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

	// Direct counter emissions (upstream/erpc hot paths) route through
	// telemetry.CounterHandle; sweep their idle label-sets so
	// caller-controlled label combinations (method, userId, agentName, ...)
	// don't accumulate in the registry forever. The counter sweep has its
	// own, more conservative threshold (metrics.counterIdleEvictionAfter,
	// default 24h) — only the cadence is shared with this observer sweep.
	telemetry.SweepIdleCounterHandles()
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
		if method == "*" {
			return []upstreamKey{{upstream, "*", common.DataFinalityStateAll}}
		}
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
	// Same reason, other axis: when the caller has no specific method the
	// per-method and any-method entries ARE the same key, so the 4-key set
	// degenerates to two keys written twice each and every Add lands twice.
	// Callers that record something not attributable to one method (a head
	// rollback belongs to the upstream, not to eth_call) pass "*" and would
	// otherwise be counted double against every other misbehaviour.
	if method == "*" {
		return []upstreamKey{
			{upstream, "*", finality},
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
	if method == "*" {
		// Same degeneracy as getUpsKeys: both entries are the same key.
		return []networkKey{{net, "*"}}
	}
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

// loadOrStoreUpsMetrics is like getUpsMetrics but does not register the key in
// the upstreamsByNetwork index. Use for buckets (e.g. the {*,All} wildcard
// aggregate) that must not appear as iterable index entries.
func (t *Tracker) loadOrStoreUpsMetrics(k upstreamKey) *TrackedMetrics {
	if v, ok := t.upsMetrics.Load(k); ok {
		return v.(*TrackedMetrics)
	}
	v, _ := t.upsMetrics.LoadOrStore(k, newTrackedMetrics(t.logger))
	return v.(*TrackedMetrics)
}

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
		// successful responses and EVM reverts (upstream did real work).
		// Cancelled attempts go through RecordUpstreamCancelledDuration.
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

// RecordUpstreamCancelledDuration records the elapsed time of a primary
// attempt cancelled because another attempt won the hedge race. It feeds
// only the per-upstream lower-bound sketch read by PolicyQuantiles;
// ResponseQuantiles and the network aggregates stay real-latency only.
func (t *Tracker) RecordUpstreamCancelledDuration(up common.Upstream, method string, d time.Duration, comp string, finality common.DataFinalityState, userId string) {
	if comp == "" {
		comp = "none"
	}
	sec := d.Seconds()
	nowMs := time.Now().UnixMilli()
	for _, k := range t.getUpsKeys(up, method, finality) {
		tm := t.getUpsMetrics(k)
		tm.addCancelled(t.logger, sec)
		tm.touch(nowMs)
	}
	t.getUpstreamRequestDurationObserver(up, method, comp, finality, userId).Observe(sec)
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
	//   not "upstream failed."  Slowness shows in PolicyQuantiles instead.
	// - BlockUnavailable: the requested height is outside this upstream's
	//   served window or has not been applied yet. Retry/failover still
	//   happens; scoring lag is BlockHeadLag, not ErrorRate. Counting these
	//   as errors punishes a recent-only replica for honest refuses.
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
		common.ErrCodeUpstreamBlockUnavailable,
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

	// Export the same event once, with the caller's own (method, finality).
	// The loops above fan one event across the in-process rollup buckets that
	// scoring reads; Prometheus does its own aggregation, so emitting per
	// bucket key would multiply every misbehavior by the rollup fan-out.
	telemetry.CounterHandle(telemetry.MetricUpstreamMisbehaviorTotal,
		t.projectId,
		up.VendorName(),
		up.NetworkLabel(),
		up.Id(),
		method,
		finality.String(),
	).Inc()
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
	getUpstreamValue func(common.Upstream, *NetworkMetadata) int64,
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
			upsValue := getUpstreamValue(k.ups, upsMeta)
			if upsValue <= 0 {
				lg.Debug().Str("networkId", net).
					Str("upstreamId", k.ups.Id()).
					Int64("value", upsValue).
					Msg("ignoring lag tracking for non-positive value")
				return true
			}
			lag := blocksBehind(networkValue, upsValue)
			setLag(tm, lag)
			if k.finality != common.DataFinalityStateAll {
				setLag(t.getUpsMetrics(upstreamKey{k.ups, k.method, common.DataFinalityStateAll}), lag)
			}
			setLag(t.loadOrStoreUpsMetrics(upstreamKey{k.ups, "*", common.DataFinalityStateAll}), lag)
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
				upsValue := getUpstreamValue(k.ups, upsMeta)
				if upsValue <= 0 {
					lg.Debug().Str("networkId", net).
						Str("upstreamId", k.ups.Id()).
						Int64("value", upsValue).
						Msg("ignoring lag tracking for non-positive value")
					continue
				}
				lag := blocksBehind(networkValue, upsValue)
				setLag(tm, lag)
				// Block-head/finalization lag is a per-upstream property, but the
				// (ups,method) dedup index stores a single, arbitrary-finality key
				// per method. Selection slots at the network and network-method
				// grains read the {method, All} rollup; if the indexed key was a
				// specific finality, that rollup is left starved and lag-based
				// predicates/scoring silently no-op. Mirror onto the All rollup
				// (which getUpsKeys always populates) so lag is seen at every grain.
				if k.finality != common.DataFinalityStateAll {
					setLag(t.getUpsMetrics(upstreamKey{k.ups, k.method, common.DataFinalityStateAll}), lag)
				}
				// Also mirror onto the {*, All} wildcard-method aggregate. Policies
				// with evalScope:network evaluate at method="*" and read this bucket
				// directly. SetLatestBlockNumber writes it for the upstream whose
				// poller fires, but never for peer upstreams processed here — so a
				// CB-open upstream whose poller is stale or absent would read lag=0
				// and bypass blockNumberLagAbove silently. Write it unconditionally
				// so the predicate fires regardless of whether the upstream's own
				// poller last wrote to it.
				setLag(t.loadOrStoreUpsMetrics(upstreamKey{k.ups, "*", common.DataFinalityStateAll}), lag)
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
				if k.finality != common.DataFinalityStateAll {
					setLag(t.getUpsMetrics(upstreamKey{k.ups, k.method, common.DataFinalityStateAll}), lag)
				}
				setLag(t.loadOrStoreUpsMetrics(upstreamKey{k.ups, "*", common.DataFinalityStateAll}), lag)
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
					setLag(t.getUpsMetrics(upstreamKey{k.ups, k.method, common.DataFinalityStateAll}), lag)
				}
				// Mirror onto the {*, All} wildcard-method aggregate for evalScope:network
				// policies (same reasoning as in updateNetworkLagMetrics above).
				setLag(t.loadOrStoreUpsMetrics(upstreamKey{k.ups, "*", common.DataFinalityStateAll}), lag)
			}
		}
	}
}

func blocksBehind(networkHead, upstreamHead int64) int64 {
	if upstreamHead >= networkHead {
		return 0
	}
	return networkHead - upstreamHead
}

// headReporter is one upstream that has reported a head on a network, with its
// per-network metadata resolved once (metadata entries are never evicted) and
// its stall window (headStallAfter), refreshed at the start of each
// evaluateHeadBench.
type headReporter struct {
	ups        common.Upstream
	meta       *NetworkMetadata
	stallAfter time.Duration
}

// addHeadReporter registers reporter as a head reporter on the network and
// returns its metadata. Caller holds ntwMeta.headMu.
func (t *Tracker) addHeadReporter(ntwMeta *NetworkMetadata, net string, reporter common.Upstream) *NetworkMetadata {
	id := reporter.Id()
	for _, r := range ntwMeta.reporters {
		if r.ups.Id() == id {
			return r.meta
		}
	}
	meta := t.getMetadata(metadataKey{reporter, net})
	ntwMeta.reporters = append(ntwMeta.reporters, headReporter{ups: reporter, meta: meta})
	return meta
}

// corroboratedNetworkHead derives the network head for one axis (the lag
// reference): the second-highest head among the non-benched reporters, the
// only one while a single reporter has a head. When every reporter is benched
// it fails open to all of them. It returns the reporter holding that head so
// the caller can carry that block's timestamp. Caller holds ntwMeta.headMu.
func corroboratedNetworkHead(ntwMeta *NetworkMetadata, getVal func(*NetworkMetadata) int64) (int64, common.Upstream) {
	if head, holder := secondHighestHead(ntwMeta, getVal, true); head > 0 {
		return head, holder
	}
	return secondHighestHead(ntwMeta, getVal, false)
}

func secondHighestHead(ntwMeta *NetworkMetadata, getVal func(*NetworkMetadata) int64, skipBenched bool) (int64, common.Upstream) {
	var first, second int64
	var firstUps, secondUps common.Upstream
	for _, r := range ntwMeta.reporters {
		if skipBenched && r.meta.headBenched.Load() {
			continue
		}
		v := getVal(r.meta)
		switch {
		case v <= 0:
		case v > first:
			second, secondUps = first, firstUps
			first, firstUps = v, r.ups
		case v > second:
			second, secondUps = v, r.ups
		}
	}
	if second > 0 {
		return second, secondUps
	}
	return first, firstUps
}

// headStallAfter is how long an upstream may stay behind before its head
// is benched: max(30s, 3 × its backed-off fetch cadence) (spec §6.1), so
// ordinary poll skew never benches anyone.
func headStallAfter(r headReporter, blockTime time.Duration) time.Duration {
	return max(30*time.Second, 3*fetchCadence(r.ups, r.meta, blockTime))
}

// fetchCadence is how often ups's head reaches this instance now, backoff
// included: its state poller's reader (SetFetchCadence), else its
// configured poll period. Caller holds the network's headMu.
func fetchCadence(ups common.Upstream, meta *NetworkMetadata, blockTime time.Duration) time.Duration {
	if meta.cadence != nil {
		if d := meta.cadence(); d > 0 {
			return d
		}
	}
	return pollPeriod(ups, blockTime)
}

// SetFetchCadence gives the head bench ups's backed-off fetch cadence
// (spec §6.1, §6.8). The state poller injects it at construction, so health
// never reads a poller field another goroutine may still be writing.
func (t *Tracker) SetFetchCadence(ups common.Upstream, cadence func() time.Duration) {
	net := ups.NetworkId()
	ntwMeta := t.getMetadata(metadataKey{nil, net})
	meta := t.getMetadata(metadataKey{ups, net})
	ntwMeta.headMu.Lock()
	meta.cadence = cadence
	ntwMeta.headMu.Unlock()
}

// pollPeriod is how often an upstream's head is fetched: the longer of the
// poll interval and the poll debounce, because a poll inside the debounce
// reuses the stored head (see EvmStatePoller.resolveDebounce); an unset
// debounce follows the block time, which bounds the derived one.
func pollPeriod(ups common.Upstream, blockTime time.Duration) time.Duration {
	period, debounce := time.Duration(0), blockTime
	if cfg := ups.Config(); cfg != nil && cfg.Evm != nil {
		period = cfg.Evm.StatePollerInterval.Duration()
		if d := cfg.Evm.StatePollerDebounce.Duration(); d > 0 {
			debounce = d
		}
	}
	return max(period, debounce)
}

// headReport is an upstream's last head report on one axis: when it came and
// the network head right after it. A stored head ages between polls by
// design; counting that age as lag made a healthy upstream polled every 10s
// on a 0.4s chain read 25 blocks behind late in each poll window, and a
// policy ticking at the same period read that same phase on every tick:
// excluded for good on some processes, never on others. So for one poll
// period after a report, what the network head made since counts as made by
// the upstream too: its lag stays what it was at the report. Past the period
// the credit freezes and a missing report counts as lag again.
type headReport struct {
	at      time.Time
	network int64
	credit  int64
}

func (r *headReport) reported(now time.Time, network int64) {
	r.at, r.network, r.credit = now, network, 0
}

// presumed is the head the upstream is presumed to hold at now, given the
// network head then.
func (r *headReport) presumed(head, network int64, now time.Time, period time.Duration) int64 {
	if r.at.IsZero() || head <= 0 {
		return head
	}
	if now.Sub(r.at) <= period {
		r.credit = max(0, network-r.network)
	}
	return head + r.credit
}

// evaluateHeadBench runs the head bench state machine over every reporter of
// the network after `changed` (if non-nil) stored a new latest head. A
// reporter is "behind" while its number age runs (numberBehind: the lag
// reference stands above its counter) or a non-benched peer proves the
// chain went on without it (headIsBehind); one behind without a break for its stall window
// is benched. A benched reporter rejoins once it has moved since, no rule
// still proves it behind, and it reached the corroborated non-benched head
// (the second-highest, or the only one): adding a voter at or above the k-th
// highest head never lowers it, and a lone far-ahead rogue cannot hold the
// bar out of reach. It also rejoins when no non-benched peer is left.
// Requiring the benched one to move keeps a stuck head from flapping.
// It reports whether anyone was benched or rejoined. Caller holds
// ntwMeta.headMu.
func (t *Tracker) evaluateHeadBench(ntwMeta *NetworkMetadata, changed *NetworkMetadata) (membershipChanged bool) {
	now := t.now()
	blockTime := time.Duration(ntwMeta.evmBlockTime.Load())
	reporters := ntwMeta.reporters
	for i := range reporters {
		reporters[i].stallAfter = headStallAfter(reporters[i], blockTime)
	}
	if changed != nil {
		changed.headChangedAt = now
	}
	ref, _ := corroboratedNetworkHead(ntwMeta, func(m *NetworkMetadata) int64 { return m.evmLatestBlockNumber.Load() })
	// A reference that stopped moving runs no number age (spec §6.1): a
	// halted chain, or a pair frozen above one that went on, benches no one
	// by it; headIsBehind's rules decide those.
	if ref <= ntwMeta.evmLatestBlockNumber.Load() && now.Sub(ntwMeta.refMovedAt) > FreshnessWindow(blockTime) {
		ref = 0
	}
	for _, u := range reporters {
		head := u.meta.evmLatestBlockNumber.Load()
		behind := head > 0 && (numberBehind(u, changed, ref, now, blockTime) || headIsBehind(u, reporters, now, blockTime))
		switch {
		case !behind:
			u.meta.headBehindSince = time.Time{}
		case u.meta.headBehindSince.IsZero():
			u.meta.headBehindSince = now
		}
	}
	for _, u := range reporters {
		if u.meta.headBenched.Load() || u.meta.headBehindSince.IsZero() {
			continue
		}
		if now.Sub(u.meta.headBehindSince) < u.stallAfter {
			continue
		}
		u.meta.headBenched.Store(true)
		u.meta.headBenchedAt = now
		u.meta.headBenchRef = 0 // taken below, once the bar excludes it
		membershipChanged = true
		telemetry.MetricUpstreamHeadBenchedTotal.WithLabelValues(t.projectId, u.ups.VendorName(), u.ups.NetworkLabel(), u.ups.Id()).Inc()
		common.TransitionLog(u.ups.Logger(), zerolog.WarnLevel, u.ups.NetworkLabel(), "bench").
			Str("upstreamId", u.ups.Id()).
			Int64("head", u.meta.evmLatestBlockNumber.Load()).
			Dur("stallAfter", u.stallAfter).
			Msg("benched upstream head: it stayed behind peers that kept advancing; it leaves the served-tip ballots and the lag reference until it catches up")
	}
	bar, _ := secondHighestHead(ntwMeta, func(m *NetworkMetadata) int64 { return m.evmLatestBlockNumber.Load() }, true)
	for _, u := range reporters {
		if !u.meta.headBenched.Load() {
			continue
		}
		uHead := u.meta.evmLatestBlockNumber.Load()
		if u.meta.headBenchRef == 0 {
			u.meta.headBenchRef = uHead
			if bar > 0 {
				u.meta.headBenchRef = bar
			}
		}
		// A move in the evaluation that benched it does not count, or a node
		// benched on its own report would rejoin on the next one without
		// moving; a node still proven behind (a replay that reached a voter as
		// stale as itself) would rejoin and be benched again a window later.
		if bar > 0 && (!u.meta.movedSinceBench() || uHead < bar || !u.meta.headBehindSince.IsZero()) {
			continue
		}
		u.meta.headBenched.Store(false)
		u.meta.headBehindSince = time.Time{}
		u.meta.headStale = false
		membershipChanged = true
		common.TransitionLog(u.ups.Logger(), zerolog.InfoLevel, u.ups.NetworkLabel(), "bench").
			Str("upstreamId", u.ups.Id()).
			Int64("head", uHead).Int64("bar", bar).
			Msg("upstream head rejoined: it caught up with its non-benched peers")
	}
	return membershipChanged
}

// movedSinceBench reports whether the head moved after the evaluation that
// benched it; a move in that same evaluation does not count. Caller holds the
// network's headMu.
func (m *NetworkMetadata) movedSinceBench() bool {
	return m.headChangedAt.After(m.headBenchedAt)
}

// numberBehind reports whether the lag reference ref (0 while it does not
// move) stands above u's counter (spec §6.1 number age). For one fetch cadence after u's last
// report, what the reference made since counts as made by u too (#1296):
// a slow poll is not lag, while a source that keeps reporting below the
// reference stays behind however often it moves. changed: the reporter
// whose head this evaluation stored, its own fresh report.
func numberBehind(u headReporter, changed *NetworkMetadata, ref int64, now time.Time, blockTime time.Duration) bool {
	head := u.meta.evmLatestBlockNumber.Load()
	if ref <= 0 || head >= ref {
		return false
	}
	if r := u.meta.headReport; u.meta != changed && !r.at.IsZero() && now.Sub(r.at) <= fetchCadence(u.ups, u.meta, blockTime) {
		head += max(0, ref-r.network)
	}
	return head < ref
}

// headIsBehind reports whether a non-benched peer proves the chain went on
// without u:
//   - by height: a peer that moved after u last moved is above u within a
//     plausible lead (benchPlausibleLead); one such peer is enough. Each move
//     of u resets this, so it measures a stall;
//   - by staleness: a live peer (one that moved within its or u's stall
//     window) is above u within a plausible lead, and its newest block (never
//     counted as newer than now) is at least u's stall window younger than
//     u's head block. That holds while u moves, so it catches a node
//     replaying far behind the tip: after a restore, a restart or a deep
//     rollback. A head stored without its timestamp keeps the last verdict
//     that had one, while the live peer is still a stall window of blocks
//     ahead: only u's own timestamp can prove it stale, so a rogue peer
//     claiming a higher head never can;
//   - by time: a peer that moved after u last moved has a newest block at
//     least u's stall window younger than u's head block, whatever the
//     heights. That catches a head frozen above a chain that restarted lower
//     (a dead fork) and a static far-ahead one. It takes two such peers while
//     two exist, so one wrong-chain peer cannot bench the fleet.
//
// Requiring the peer to move keeps a chain halt (nobody moves) from benching
// anyone; the plausible lead and the two-peer rule keep a moving wrong-chain
// head from benching the honest fleet, and a node at the tip has a head block
// as young as any honest peer's. Caller holds ntwMeta.headMu.
func headIsBehind(u headReporter, reporters []headReporter, now time.Time, blockTime time.Duration) bool {
	uHead, uTs := u.meta.evmLatestBlockNumber.Load(), u.meta.evmLatestBlockTimestamp.Load()
	stallAfter := u.stallAfter
	stallSecs := int64(stallAfter / time.Second)
	peers, younger := 0, 0
	byHeight, stale := false, false
	for _, v := range reporters {
		vHead := v.meta.evmLatestBlockNumber.Load()
		if v.meta == u.meta || vHead <= 0 || v.meta.headBenched.Load() {
			continue
		}
		peers++
		bt := blockTime
		if bt <= 0 {
			// The network block time follows the corroborated head, which
			// never advances while it is the stalled one (a process that
			// starts mid-outage): measure the chain by the peer's own blocks.
			bt = time.Duration(v.meta.evmBlockTime.Load())
		}
		lead := vHead - uHead
		plausible := lead > 0 && lead <= benchPlausibleLead(u.meta, now, bt)
		live := now.Sub(v.meta.headChangedAt) < max(stallAfter, v.stallAfter)
		if plausible && live {
			if uTs > 0 {
				stale = stale || v.meta.headTsFloor-uTs >= stallSecs
			} else if u.meta.headStale && lead*bt.Milliseconds() >= stallAfter.Milliseconds() {
				stale = true
			}
		}
		if !v.meta.headChangedAt.After(u.meta.headChangedAt) {
			continue
		}
		byHeight = byHeight || plausible
		if uTs > 0 && v.meta.headTsFloor-uTs >= stallSecs {
			younger++
		}
	}
	u.meta.headStale = stale
	return byHeight || stale || (younger > 0 && younger >= min(2, peers))
}

// benchPlausibleLead is the most blocks a peer may be ahead of u and still
// count as the chain moving on without it: the rollback tolerance every head
// layer shares, plus twice what the chain produced since u's head was current
// (the earlier of when u last moved and the newest block timestamp seen for
// u, so a process that starts mid-outage still sees the outage). A larger
// lead is another chain, not a stall. Unknown block time leaves only the
// tolerance.
func benchPlausibleLead(u *NetworkMetadata, now time.Time, blockTime time.Duration) int64 {
	lead := int64(common.DefaultToleratedBlockHeadRollback)
	btMs := blockTime.Milliseconds()
	if btMs <= 0 {
		return lead
	}
	stale := now.Sub(u.headChangedAt).Milliseconds()
	if ts := u.headTsFloor; ts > 0 {
		stale = max(stale, now.UnixMilli()-ts*1000)
	}
	if stale > 0 {
		lead += 2 * stale / btMs
	}
	return lead
}

// OnHeadBenchChange registers fn to run when an upstream of networkId is
// benched or rejoins. fn runs under the network's head lock: it must not
// block. One per network; a later one replaces it.
func (t *Tracker) OnHeadBenchChange(networkId string, fn func()) {
	t.benchHooks.Store(networkId, fn)
}

// IsHeadBenched reports whether the upstream's latest head is benched: it
// stayed behind peers that kept advancing, and has not caught up since.
// Benched heads are left out of the network lag reference and of the served-tip
// ballots.
func (t *Tracker) IsHeadBenched(ups common.Upstream) bool {
	if v, ok := t.metadata.Load(metadataKey{ups, ups.NetworkId()}); ok {
		return v.(*NetworkMetadata).headBenched.Load()
	}
	return false
}

// RecordHashVote makes number ups's newest hash vote (spec §6.3). Its hash
// age runs while the lag reference stands above it. replay: the header is
// one integrity held back earlier, so its arrival time stays the previous
// vote's; the gRPC stall clock reads it.
func (t *Tracker) RecordHashVote(ups common.Upstream, number int64, replay bool) {
	if number <= 0 {
		return
	}
	net := ups.NetworkId()
	ntwMeta := t.getMetadata(metadataKey{nil, net})
	ntwMeta.headMu.Lock()
	defer ntwMeta.headMu.Unlock()
	meta := t.addHeadReporter(ntwMeta, net, ups)
	now := t.now()
	ntwMeta.firstVotes = firstVoteAt(ntwMeta.firstVotes, number, now)
	meta.hashVote.Store(number)
	meta.hashExpired = false
	if !replay {
		meta.hashVotedAt = now
	}
	meta.ageHashVote(ntwMeta.evmLatestBlockNumber.Load(), ntwMeta.firstVotes, now)
	t.reportAges(ups, meta, now)
}

// firstVote is when this instance first received a hash vote, from any
// upstream, for a block number.
type firstVote struct {
	number int64
	at     time.Time
}

// firstVotesKept bounds the numbers whose first hash vote a network keeps.
const firstVotesKept = 256

// firstVoteAt notes at as number's first hash vote unless one is noted;
// fv stays sorted by number, the lowest leaving past firstVotesKept.
func firstVoteAt(fv []firstVote, number int64, at time.Time) []firstVote {
	i, found := slices.BinarySearchFunc(fv, number, func(v firstVote, n int64) int { return cmp.Compare(v.number, n) })
	if found {
		return fv
	}
	fv = slices.Insert(fv, i, firstVote{number, at})
	if over := len(fv) - firstVotesKept; over > 0 {
		fv = slices.Delete(fv, 0, over)
	}
	return fv
}

// ExpireHashVote ends the freshness of ups's hash vote at once: its socket
// closed, so its next header can come only after a reconnect (spec §15.1).
func (t *Tracker) ExpireHashVote(ups common.Upstream) {
	net := ups.NetworkId()
	ntwMeta := t.getMetadata(metadataKey{nil, net})
	ntwMeta.headMu.Lock()
	defer ntwMeta.headMu.Unlock()
	meta := t.addHeadReporter(ntwMeta, net, ups)
	meta.hashExpired = true
	t.reportAges(ups, meta, t.now())
}

// RenewHashVote makes ups's expired hash vote fresh again: after a
// reconnect its first header is that vote, so on a halted chain no newer one
// comes (spec §15.1). It reports whether the vote was expired.
func (t *Tracker) RenewHashVote(ups common.Upstream) bool {
	net := ups.NetworkId()
	ntwMeta := t.getMetadata(metadataKey{nil, net})
	ntwMeta.headMu.Lock()
	defer ntwMeta.headMu.Unlock()
	meta := t.addHeadReporter(ntwMeta, net, ups)
	if meta.hashVote.Load() == 0 || !meta.hashExpired {
		return false
	}
	meta.hashExpired = false
	t.reportAges(ups, meta, t.now())
	return true
}

// HashVote is what this instance knows of an upstream's newest hash vote
// (spec §6.1, §6.3).
type HashVote struct {
	// Age is how long the lag reference has stood above it, counted from
	// the first hash vote above it that reached this instance; it runs only
	// while the reference moves, so a halted chain trips no silent-socket
	// watch.
	Age time.Duration
	// Expired: its socket closed since, so it is no longer fresh whatever
	// its age.
	Expired bool
	// At is when it arrived; Number is its block number.
	At     time.Time
	Number int64
}

// HashVote is ups's newest hash vote; false before its first.
func (t *Tracker) HashVote(ups common.Upstream) (HashVote, bool) {
	ntwMeta := t.getMetadata(metadataKey{nil, ups.NetworkId()})
	meta := t.getMetadata(metadataKey{ups, ups.NetworkId()})
	ntwMeta.headMu.Lock()
	defer ntwMeta.headMu.Unlock()
	age, ok := meta.hashAge(t.now())
	return HashVote{Age: age, Expired: meta.hashExpired, At: meta.hashVotedAt, Number: meta.hashVote.Load()}, ok
}

// HashVoteNumber is the number of ups's newest hash vote, 0 before its
// first; it takes no lock, for routing (spec §6.1).
func (t *Tracker) HashVoteNumber(ups common.Upstream) int64 {
	if v, ok := t.metadata.Load(metadataKey{ups, ups.NetworkId()}); ok {
		return v.(*NetworkMetadata).hashVote.Load()
	}
	return 0
}

// HeadCredit is how many blocks ups is presumed to have gained since it
// reported head at at, its head counter and that report's time, for
// ordering only (spec §6.4, #1352): what latest made beyond ups's lag then
// (its {*, All} BlockHeadLag, which keeps the lag at the report for one
// poll period, see headReport), at most the blocks the chain can make
// since at by the network block time, so a latest that rose only because
// peers reported late is no progress. 0 for a head that is unknown, at or
// above latest, benched, or tracked above latest, and until the block time
// is known.
func (t *Tracker) HeadCredit(ups common.Upstream, head int64, at time.Time, latest int64) int64 {
	if head <= 0 || head >= latest || at.IsZero() {
		return 0
	}
	net := ups.NetworkId()
	um, ok := t.metadata.Load(metadataKey{ups, net})
	if !ok {
		return 0
	}
	meta := um.(*NetworkMetadata)
	nm, ok := t.metadata.Load(metadataKey{nil, net})
	if !ok || meta.headBenched.Load() || meta.evmLatestBlockNumber.Load() > latest {
		return 0
	}
	blockTime := time.Duration(nm.(*NetworkMetadata).evmBlockTime.Load())
	m, ok := t.upsMetrics.Load(upstreamKey{ups, "*", common.DataFinalityStateAll})
	if !ok || blockTime <= 0 {
		return 0
	}
	byLag := latest - m.(*TrackedMetrics).BlockHeadLag.Load() - head
	byTime := int64(t.now().Sub(at) / blockTime)
	return max(0, min(byLag, byTime))
}

// ageHashVote starts the hash age when the lag reference ref stands above
// the hash vote, and ends it once the vote reaches ref. The age counts from
// the first hash vote above it that reached this instance (fv) at or below
// ref, so a voter that goes silent stops being a hash voter W after
// another voter's first header (spec §15.1, U3), even when the reference
// passes it later; votes above ref, such as a lone far-ahead one, never
// start it. A newer vote still below ref keeps the age running: a lagging
// source that keeps pushing stays behind. It reports whether the age
// started or ended. Caller holds the network's headMu.
func (m *NetworkMetadata) ageHashVote(ref int64, fv []firstVote, now time.Time) (changed bool) {
	vote := m.hashVote.Load()
	if vote == 0 || vote >= ref {
		changed = !m.hashBehindSince.IsZero()
		m.hashBehindSince = time.Time{}
		return changed
	}
	if !m.hashBehindSince.IsZero() {
		return false
	}
	m.hashBehindSince = now
	if at := firstVoteAbove(fv, vote, ref); !at.IsZero() && at.Before(now) {
		m.hashBehindSince = at
	}
	return true
}

// firstVoteAbove is when the earliest hash vote above vote, at or below
// ref, reached this instance (fv); zero when none did.
func firstVoteAbove(fv []firstVote, vote, ref int64) (at time.Time) {
	i, _ := slices.BinarySearchFunc(fv, vote+1, func(v firstVote, n int64) int { return cmp.Compare(v.number, n) })
	for _, v := range fv[i:] {
		if v.number > ref {
			break
		}
		if at.IsZero() || v.at.Before(at) {
			at = v.at
		}
	}
	return at
}

// HashAgeAt is the hash age a header at number of the network has now
// (spec §6.1): how long since the first hash vote above it, at or below
// the lag reference, reached this instance; 0 while none did.
func (t *Tracker) HashAgeAt(networkId string, number int64) time.Duration {
	ntwMeta := t.getMetadata(metadataKey{nil, networkId})
	ntwMeta.headMu.Lock()
	defer ntwMeta.headMu.Unlock()
	at := firstVoteAbove(ntwMeta.firstVotes, number, ntwMeta.evmLatestBlockNumber.Load())
	if at.IsZero() {
		return 0
	}
	return t.now().Sub(at)
}

// hashAge: see HashVote.Age. Caller holds the network's headMu.
func (m *NetworkMetadata) hashAge(now time.Time) (time.Duration, bool) {
	if m.hashVote.Load() == 0 {
		return 0, false
	}
	if m.hashBehindSince.IsZero() {
		return 0, true
	}
	return now.Sub(m.hashBehindSince), true
}

// reportAges sets ups's report-age gauges: number age is how long peers
// have proven the chain went on without its counter (the bench clock),
// hash age as HashVote.Age, +Inf without a fresh hash vote. Caller holds
// the network's headMu.
func (t *Tracker) reportAges(ups common.Upstream, m *NetworkMetadata, now time.Time) {
	number := 0.0
	if !m.headBehindSince.IsZero() {
		number = now.Sub(m.headBehindSince).Seconds()
	}
	hash := math.Inf(1)
	if age, ok := m.hashAge(now); ok && !m.hashExpired {
		hash = age.Seconds()
	}
	// The upstream learns its network label after it first reports, so a
	// bound gauge is bound again when the label moved, and the series under
	// the old label goes: it would report a frozen age.
	if label := ups.NetworkLabel(); m.ageGauges[0] == nil || m.ageLabel != label {
		if m.ageGauges[0] != nil {
			for _, ev := range []string{"number", "hash"} {
				telemetry.MetricUpstreamReportAgeSeconds.DeleteLabelValues(t.projectId, m.ageLabel, ups.Id(), ev)
			}
		}
		m.ageLabel = label
		m.ageGauges[0] = telemetry.GaugeHandle(telemetry.MetricUpstreamReportAgeSeconds, t.projectId, label, ups.Id(), "number")
		m.ageGauges[1] = telemetry.GaugeHandle(telemetry.MetricUpstreamReportAgeSeconds, t.projectId, label, ups.Id(), "hash")
	}
	m.ageGauges[0].Set(number)
	m.ageGauges[1].Set(hash)
}

func (t *Tracker) SetLatestBlockNumber(upstream common.Upstream, blockNumber int64, blockTimestamp int64) {
	t.setLatestBlockNumber(upstream, blockNumber, blockTimestamp, false)
}

// RewindLatestBlockNumber lowers upstream's head to blockNumber, a
// hash-proven rollback write (spec §14), whatever the drop: the
// tolerance SetLatestBlockNumber keeps for lagging reports does not apply,
// and it is no misbehaviour.
func (t *Tracker) RewindLatestBlockNumber(upstream common.Upstream, blockNumber int64) {
	t.setLatestBlockNumber(upstream, blockNumber, 0, true)
}

func (t *Tracker) setLatestBlockNumber(upstream common.Upstream, blockNumber int64, blockTimestamp int64, rewind bool) {
	id := upstream.Id()
	net := upstream.NetworkId()
	netLabel := upstream.NetworkLabel()
	vendor := upstream.VendorName()
	// The network id goes on each event, not on a child logger built per
	// report: most reports log nothing.
	lg := upstream.Logger()

	lg.Trace().Str("networkId", net).Int64("value", blockNumber).Msg("updating latest block number in tracker")
	if blockNumber <= 0 {
		lg.Warn().Str("networkId", net).Int64("value", blockNumber).Msg("ignoring setting non-positive latest block number in tracker")
		return
	}

	upsMeta := t.getMetadata(metadataKey{upstream, net})
	ntwMeta := t.getMetadata(metadataKey{nil, net})
	ntwMeta.headMu.Lock()
	defer ntwMeta.headMu.Unlock()

	oldUpsVal := upsMeta.evmLatestBlockNumber.Load()
	headChanged := false
	if blockNumber > oldUpsVal {
		upsMeta.evmLatestBlockNumber.Store(blockNumber)
		upsMeta.evmLatestBlockTimestamp.Store(blockTimestamp)
		g := t.getLatestBlockGauge(t.projectId, vendor, netLabel, id)
		g.Set(float64(blockNumber))
		headChanged = true
	} else if blockNumber < oldUpsVal && (rewind || oldUpsVal-blockNumber > common.DefaultToleratedBlockHeadRollback) {
		// The upstream reports a head far behind the one stored for it, or a
		// rewind proves a lower one: treat it as a correction (a deep reorg,
		// or a previously recorded bogus sample), mirroring the shared-state
		// counter semantics. Max-only storage would pin the bad sample — and
		// every lag-based routing decision derived from it — until process
		// restart.
		upsMeta.evmLatestBlockNumber.Store(blockNumber)
		upsMeta.evmLatestBlockTimestamp.Store(blockTimestamp)
		g := t.getLatestBlockGauge(t.projectId, vendor, netLabel, id)
		g.Set(float64(blockNumber))
		lg.Warn().Str("networkId", net).
			Int64("previousValue", oldUpsVal).
			Int64("newValue", blockNumber).
			Bool("rewind", rewind).
			Msg("applied latest block rollback for upstream in tracker")
		upsMeta.headTsFloor = 0
		headChanged = true
	} else if blockNumber == oldUpsVal && blockTimestamp > 0 && upsMeta.evmLatestBlockTimestamp.Load() == 0 {
		upsMeta.evmLatestBlockTimestamp.Store(blockTimestamp)
	}
	if blockTimestamp > 0 && blockNumber == upsMeta.evmLatestBlockNumber.Load() {
		upsMeta.headTsFloor = max(upsMeta.headTsFloor, min(blockTimestamp, t.now().Unix()))
	}

	t.addHeadReporter(ntwMeta, net, upstream)
	var changed *NetworkMetadata
	if headChanged {
		changed = upsMeta
		if blockTimestamp > 0 {
			upsMeta.sampleBlockTime(blockNumber, blockTimestamp)
		}
	}
	membershipChanged := t.evaluateHeadBench(ntwMeta, changed)
	if membershipChanged {
		// Bench membership also decides the finalized lag reference.
		t.rederiveNetworkFinalized(ntwMeta, net, netLabel, lg)
		if fn, ok := t.benchHooks.Load(net); ok {
			fn.(func())()
		}
	}

	oldNtwVal := ntwMeta.evmLatestBlockNumber.Load()
	ntwBn, headReporter := corroboratedNetworkHead(ntwMeta, func(meta *NetworkMetadata) int64 {
		return meta.evmLatestBlockNumber.Load()
	})
	if ntwBn <= 0 {
		lg.Warn().Str("networkId", net).Int64("value", ntwBn).Msg("ignoring block head lag tracking for non-positive block number in tracker")
		return
	}
	needsGlobalUpdate := ntwBn != oldNtwVal
	if needsGlobalUpdate {
		if ntwBn > oldNtwVal {
			ntwMeta.refMovedAt = t.now()
		}
		ntwMeta.evmLatestBlockNumber.Store(ntwBn)
		g := t.getLatestBlockGauge(t.projectId, "*", netLabel, "*")
		g.Set(float64(ntwBn))
		if ntwBn < oldNtwVal {
			lg.Warn().Str("networkId", net).
				Int64("previousValue", oldNtwVal).
				Int64("newValue", ntwBn).
				Msg("re-derived network latest block from corroborated upstream heads in tracker")
		}
	}

	// The network timestamp and block-time EMA follow the corroborated head,
	// whichever upstream reported it.
	if ntwBn >= oldNtwVal && headReporter != nil {
		headTs := t.getMetadata(metadataKey{headReporter, net}).evmLatestBlockTimestamp.Load()
		if headTs > 0 && (ntwBn > oldNtwVal || ntwMeta.evmLatestBlockTimestamp.Load() != headTs) {
			// Uses on-chain timestamps (not local clock) so the EMA tracks actual
			// chain production rate, not our polling cadence. For fast chains where
			// consecutive blocks share the same integer-second timestamp, samples
			// are skipped until the timestamp advances; blockGap normalization
			// recovers sub-second precision.
			t.updateBlockTimeSample(ntwMeta, netLabel, ntwBn, headTs)

			ntwMeta.evmLatestBlockTimestamp.Store(headTs)

			detectedAtMs := time.Now().UnixMilli()
			distanceMs := detectedAtMs - headTs*1000
			telemetry.MetricNetworkLatestBlockTimestampDistance.WithLabelValues(
				t.projectId,
				netLabel,
				"evm_state_poller",
			).Set(float64(distanceMs) / 1000.0)
		}
	}

	// 3) Recompute block head lag for this upstream. A benched head that has
	// not moved since its bench counts as no higher than the network head was
	// then, so a head frozen above a chain that went on below it lags by what
	// the network made since, even once the network passes its height (its
	// blocks may be a dead fork). A benched head that moves again (replaying)
	// lags by its real distance. A head that is not benched lags by what the
	// network made beyond its last report (headReport).
	now, blockTime := t.now(), time.Duration(ntwMeta.evmBlockTime.Load())
	upsMeta.headReport.reported(now, ntwBn)
	// Ages are set for the reporter and for reporters whose hash age started
	// or ended; every reporter's, which run with time, once a second.
	allAges := now.Sub(ntwMeta.agesAt) >= reportAgesEvery
	if allAges {
		ntwMeta.agesAt = now
	}
	for _, r := range ntwMeta.reporters {
		if r.meta.ageHashVote(ntwBn, ntwMeta.firstVotes, now) || allAges || r.meta == upsMeta {
			t.reportAges(r.ups, r.meta, now)
		}
	}
	lagHead := func(ups common.Upstream, meta *NetworkMetadata) int64 {
		h := meta.evmLatestBlockNumber.Load()
		if !meta.headBenched.Load() {
			return meta.headReport.presumed(h, ntwBn, now, pollPeriod(ups, blockTime))
		}
		if meta.headBenchRef > 0 && !meta.movedSinceBench() {
			return min(h, meta.headBenchRef)
		}
		return h
	}
	upsLag := blocksBehind(ntwBn, lagHead(upstream, upsMeta))
	gLag := t.getHeadLagGauge(t.projectId, vendor, netLabel, id)
	gLag.Set(float64(upsLag))

	// 4) Update the TrackedMetrics.BlockHeadLag fields for Upstream(s)
	if needsGlobalUpdate || membershipChanged {
		// Recompute for every upstream in the network
		t.updateNetworkLagMetrics(
			net,
			ntwBn,
			lagHead,
			func(tm *TrackedMetrics, lag int64) { tm.BlockHeadLag.Store(lag) },
			t.getHeadLagGauge,
			lg,
		)
		// Every reporter's {*, All} lag follows the new head, served or not:
		// the index above holds only upstreams with traffic or a policy read,
		// and a reporter missing from it would keep the lag it had when its
		// poller last reported, after the poller stopped. Network-scope lag
		// policies and HeadCredit read this bucket.
		for _, r := range ntwMeta.reporters {
			if r.meta.evmLatestBlockNumber.Load() <= 0 {
				continue
			}
			lag := blocksBehind(ntwBn, lagHead(r.ups, r.meta))
			t.loadOrStoreUpsMetrics(upstreamKey{r.ups, "*", common.DataFinalityStateAll}).BlockHeadLag.Store(lag)
			t.getHeadLagGauge(t.projectId, r.ups.VendorName(), netLabel, r.ups.Id()).Set(float64(lag))
		}
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
	t.loadOrStoreUpsMetrics(upstreamKey{upstream, "*", common.DataFinalityStateAll}).BlockHeadLag.Store(upsLag)
}

// ------------------------------------
// Dynamic Block Time (EMA)
// ------------------------------------

// blockTimeEmaAlpha is the EMA's smoothing factor; effective window ~19
// samples.
const blockTimeEmaAlpha = 0.1

// blockTimeSampleCap bounds a block time sample to this many times the EMA.
const blockTimeSampleCap = 3

// reportAgesEvery is how often a head update sets every reporter's
// report-age gauges, which run with time.
const reportAgesEvery = time.Second

// ColdBlockTime is every reader's block time until the network's first
// sample (spec §6.7).
const ColdBlockTime = time.Second

// FreshnessWindow is W for block time bt (spec §1): max(3 × bt, 2s), at
// most 60s; ColdBlockTime while bt is unknown (0).
func FreshnessWindow(bt time.Duration) time.Duration {
	if bt <= 0 {
		bt = ColdBlockTime
	}
	return min(max(3*bt, 2*time.Second), 60*time.Second)
}

// SampleConfirmedHeaderTime feeds the timestamp of a source header the
// network's confirmed head reached into its block time EMA (spec §6.7): a
// local sample, as the poller's are. Pushed heads carry no timestamp
// through the shared counter, so an instance that polls rarely would
// otherwise learn the block time late; a lagging or bogus header the
// confirmed head never reached is no sample.
func (t *Tracker) SampleConfirmedHeaderTime(networkId, networkLabel string, number, timestamp int64) {
	if number <= 0 || timestamp <= 0 {
		return
	}
	t.updateBlockTimeSample(t.getMetadata(metadataKey{nil, networkId}), networkLabel, number, timestamp)
}

// updateBlockTimeSample feeds the network head's block into the network block
// time EMA and publishes the result.
func (t *Tracker) updateBlockTimeSample(ntwMeta *NetworkMetadata, netLabel string, blockNumber int64, blockTimestamp int64) {
	if ntwMeta.sampleBlockTime(blockNumber, blockTimestamp) {
		telemetry.MetricNetworkDynamicBlockTime.WithLabelValues(
			t.projectId, netLabel,
		).Set(float64(time.Duration(ntwMeta.evmBlockTime.Load()).Milliseconds()))
	}
}

// sampleBlockTime feeds a block's on-chain timestamp (integer seconds) into
// m's block time EMA and floor, and reports whether it stored a new
// evmBlockTime (spec §6.7). A sample spans two observations whose
// timestamps differ: seconds elapsed over blocks elapsed. Blocks that share
// a timestamp extend the span, so integer-second timestamps need no special
// case. The first block seen may sit anywhere in its second, so spans start
// at the first timestamp change after it; the first sample seeds the EMA.
func (m *NetworkMetadata) sampleBlockTime(blockNumber int64, blockTimestamp int64) bool {
	m.evmBlockTimeMu.Lock()
	defer m.evmBlockTimeMu.Unlock()

	prevBlock, prevTimestamp := m.evmBlockTimePrevBlock, m.evmBlockTimePrevTimestamp
	if prevBlock == 0 {
		m.evmBlockTimePrevBlock, m.evmBlockTimePrevTimestamp = blockNumber, blockTimestamp
		return false
	}

	// Reject out-of-order or duplicate blocks. A block far behind prev means the
	// head was corrected by a large rollback: start over from it, so sampling
	// resumes instead of stalling until the chain re-passes the old prev.
	blockGap := blockNumber - prevBlock
	if blockGap <= 0 {
		if prevBlock-blockNumber > common.DefaultToleratedBlockHeadRollback {
			m.evmBlockTimePrevBlock, m.evmBlockTimePrevTimestamp = blockNumber, blockTimestamp
			m.evmBlockTimeSpanStart = false
		}
		return false
	}
	timestampDeltaSec := blockTimestamp - prevTimestamp
	if timestampDeltaSec <= 0 {
		return false
	}
	spanned := m.evmBlockTimeSpanStart
	m.evmBlockTimePrevBlock, m.evmBlockTimePrevTimestamp = blockNumber, blockTimestamp
	m.evmBlockTimeSpanStart = true
	if !spanned {
		m.evmBlockTimeAnchorBlock, m.evmBlockTimeAnchorTimestamp = blockNumber, blockTimestamp
		return false
	}

	sampleNs := float64(timestampDeltaSec) * 1e9 / float64(blockGap)
	if m.evmBlockTimeSamples == 0 {
		// No EMA yet tells a halt apart, so the seed never enters the
		// floor: it restarts at the seed's end, as the halt clip does.
		m.evmBlockTimeEmaNs = sampleNs
		m.evmBlockTimeAnchorBlock, m.evmBlockTimeAnchorTimestamp = blockNumber, blockTimestamp
	} else {
		// One span across a halt counts as 3× the EMA, so it never sets W
		// to its cap (spec §6.7). The floor restarts at its end: a pause
		// adds seconds without blocks, so an average across it overstates
		// the block time while blocks are made.
		if halt := blockTimeSampleCap * m.evmBlockTimeEmaNs; sampleNs > halt {
			sampleNs = halt
			m.evmBlockTimeAnchorBlock, m.evmBlockTimeAnchorTimestamp = blockNumber, blockTimestamp
		}
		m.evmBlockTimeEmaNs = blockTimeEmaAlpha*sampleNs + (1-blockTimeEmaAlpha)*m.evmBlockTimeEmaNs
	}
	m.evmBlockTimeSamples++
	if dts, dn := blockTimestamp-m.evmBlockTimeAnchorTimestamp, blockNumber-m.evmBlockTimeAnchorBlock; dts >= 2 && dn > 0 {
		m.evmBlockTimeFloor.Store((dts - 1) * int64(time.Second) / dn)
	}

	if !m.storeBlockTime(int64(m.evmBlockTimeEmaNs)) {
		// Reset the internal EMA to the last published value so that recovery
		// from a prolonged halt doesn't create a delayed spike when the EMA
		// first crosses back below the threshold (e.g. jumping from 2s to
		// ~119s).
		if lastGood := m.evmBlockTime.Load(); lastGood > 0 {
			m.evmBlockTimeEmaNs = float64(lastGood)
		}
		return false
	}
	return true
}

// storeBlockTime publishes ns as the block time unless it is absurd.
// Caller holds evmBlockTimeMu.
func (m *NetworkMetadata) storeBlockTime(ns int64) bool {
	if ns < int64(10*time.Millisecond) || ns > int64(120*time.Second) {
		return false
	}
	m.evmBlockTime.Store(ns)
	return true
}

// GetNetworkBlockTime returns the estimated block time for a network: 0
// until its first sample (sampleBlockTime); readers that need a value
// meanwhile use ColdBlockTime.
func (t *Tracker) GetNetworkBlockTime(networkId string) time.Duration {
	ntwMeta := t.getMetadata(metadataKey{nil, networkId})

	if v := ntwMeta.evmBlockTime.Load(); v > 0 {
		return time.Duration(v)
	}

	return 0
}

// GetNetworkBlockTimeFloor returns the shorter of the network's block time
// EMA and its floor (evmBlockTimeFloor): a block time the evidence says the
// chain's average is at least, for a reader that must never overestimate
// it, as the derived finality depth (spec §6.4). 0 while both are unknown.
func (t *Tracker) GetNetworkBlockTimeFloor(networkId string) time.Duration {
	m := t.getMetadata(metadataKey{nil, networkId})
	ema, floor := m.evmBlockTime.Load(), m.evmBlockTimeFloor.Load()
	if floor > 0 && (ema <= 0 || floor < ema) {
		return time.Duration(floor)
	}
	return time.Duration(ema)
}

// GetNetworkFinalizedBlock returns the network's corroborated finalized block
// (see corroboratedNetworkHead): the second-highest finalized block across its
// non-benched upstreams, or the only one. 0 until reported.
func (t *Tracker) GetNetworkFinalizedBlock(networkId string) int64 {
	return t.getMetadata(metadataKey{nil, networkId}).evmFinalizedBlockNumber.Load()
}

func (t *Tracker) SetFinalizedBlockNumber(upstream common.Upstream, blockNumber int64) {
	// The network id goes on each event, not on a child logger built per
	// report: most reports log nothing.
	lg := upstream.Logger()
	net := upstream.NetworkId()

	lg.Trace().Str("networkId", net).Int64("value", blockNumber).Msg("updating finalized block number in tracker")

	if blockNumber <= 0 {
		lg.Warn().Str("networkId", net).Int64("value", blockNumber).Msg("ignoring setting non-positive block number in finalized block tracker")
		return
	}

	id := upstream.Id()
	netLabel := upstream.NetworkLabel()
	vendor := upstream.VendorName()

	upsMeta := t.getMetadata(metadataKey{upstream, net})
	ntwMeta := t.getMetadata(metadataKey{nil, net})
	ntwMeta.headMu.Lock()
	defer ntwMeta.headMu.Unlock()

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
		lg.Warn().Str("networkId", net).
			Int64("previousValue", oldUpsVal).
			Int64("newValue", blockNumber).
			Msg("applied large finalized block rollback for upstream in tracker")
	}

	t.addHeadReporter(ntwMeta, net, upstream)
	ntwVal, _ := t.rederiveNetworkFinalized(ntwMeta, net, netLabel, lg)
	if ntwVal <= 0 {
		lg.Warn().Str("networkId", net).Int64("value", ntwVal).Msg("ignoring finalization lag tracking for negative block number in tracker")
		return
	}

	// The re-derivation above credited this upstream against its previous
	// report; its lag now is the one at this report.
	upsMeta.finalizedReport.reported(t.now(), ntwVal)
	upsLag := blocksBehind(ntwVal, upsMeta.evmFinalizedBlockNumber.Load())

	// Update Prometheus for this upstream
	gLag := t.getFinalizationLagGauge(t.projectId, vendor, netLabel, id)
	gLag.Set(float64(upsLag))

	t.updateSingleUpstreamLag(
		id,
		net,
		upsLag,
		func(tm *TrackedMetrics, lag int64) { tm.FinalizationLag.Store(lag) },
	)

	// Same {*, All} wildcard-aggregate guarantee as SetLatestBlockNumber (see
	// the comment there): the dedup index may not carry the "*" rollup, so
	// write it directly to keep finalization-lag-based scoring/predicates honest.
	t.loadOrStoreUpsMetrics(upstreamKey{upstream, "*", common.DataFinalityStateAll}).FinalizationLag.Store(upsLag)
}

// rederiveNetworkFinalized re-derives the network finalized head from the
// non-benched reporters and, when it moved, every upstream's finalization lag.
// It runs on each finalized report and when the latest-axis bench changes who
// counts. It returns the network value (<= 0 while unknown) and whether it
// moved. Caller holds ntwMeta.headMu.
func (t *Tracker) rederiveNetworkFinalized(ntwMeta *NetworkMetadata, net, netLabel string, lg *zerolog.Logger) (int64, bool) {
	oldNtwVal := ntwMeta.evmFinalizedBlockNumber.Load()
	getVal := func(meta *NetworkMetadata) int64 { return meta.evmFinalizedBlockNumber.Load() }
	ntwVal, _ := corroboratedNetworkHead(ntwMeta, getVal)
	if ntwVal <= 0 || ntwVal == oldNtwVal {
		return ntwVal, false
	}
	ntwMeta.evmFinalizedBlockNumber.Store(ntwVal)
	t.getFinalizedBlockGauge(t.projectId, "*", netLabel, "*").Set(float64(ntwVal))
	if ntwVal < oldNtwVal {
		lg.Warn().Str("networkId", net).
			Int64("previousValue", oldNtwVal).
			Int64("newValue", ntwVal).
			Msg("re-derived network finalized block from corroborated upstream heads in tracker")
	}
	now, blockTime := t.now(), time.Duration(ntwMeta.evmBlockTime.Load())
	t.updateNetworkLagMetrics(
		net,
		ntwVal,
		func(ups common.Upstream, meta *NetworkMetadata) int64 {
			f := meta.evmFinalizedBlockNumber.Load()
			if meta.headBenched.Load() {
				return f
			}
			return meta.finalizedReport.presumed(f, ntwVal, now, pollPeriod(ups, blockTime))
		},
		func(tm *TrackedMetrics, lag int64) { tm.FinalizationLag.Store(lag) },
		t.getFinalizationLagGauge,
		lg,
	)
	return ntwVal, true
}

func (t *Tracker) RecordBlockHeadLargeRollback(upstream common.Upstream, finality string, currentVal, newVal int64) {
	rollback := currentVal - newVal

	net := upstream.NetworkId()
	lg := upstream.Logger().With().Str("networkId", net).Logger()
	lg.Debug().
		Str("finality", finality).
		Int64("currentValue", currentVal).
		Int64("newValue", newVal).
		Int64("rollback", rollback).
		Msgf("recording block rollback in tracker")

	t.getRollbackGauge(upstream, finality).Set(float64(rollback))

	// A large head rollback is also a misbehaviour, and recording it as one is
	// what lets the EXISTING machinery act on it. Until now this method only set
	// a gauge, so an upstream could roll its head back every few seconds and stay
	// fully weighted: its head kept voting in the served-tip ballot and it kept
	// being picked to answer "latest" from whichever node pool it landed on.
	//
	// MisbehaviorsTotal is already a rolling counter on TrackedMetrics, already
	// carried by routing.scoreMultipliers (`misbehaviors`), and already visible
	// to selectionPolicy evalFunc. So one rollback -- a real, rare, legitimate
	// deep reorg -- barely moves the score, while an endpoint flapping between
	// two node pools accumulates until the operator's own policy deprioritises or
	// drops it. The threshold stays where it belongs, in configuration, instead
	// of being a constant compiled in here.
	//
	// Stratified by head axis so a finalized-head rollback does not penalise the
	// upstream for realtime traffic and vice versa.
	state := common.DataFinalityStateRealtime
	if finality == "finalized" {
		state = common.DataFinalityStateFinalized
	}
	t.RecordUpstreamMisbehavior(upstream, "*", state)
}
