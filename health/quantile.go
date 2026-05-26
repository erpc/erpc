package health

import (
	"math"
	"sync"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/bytedance/sonic"
	"github.com/rs/zerolog"
)

// QuantileTracker holds a sliding-window estimate of a duration
// distribution. Internally a ring of `rollingBuckets` DDSketches; Add
// writes to the newest, GetQuantile merges all into an ephemeral
// sketch and queries it, RotateOldest re-inits the oldest sketch and
// advances the head pointer. Same surface as the original single-
// sketch version — drop-in for callers — but the effective rolling
// window doesn't collapse to zero at rotation time.
type QuantileTracker struct {
	logger *zerolog.Logger
	mu     sync.RWMutex
	// buckets is a ring buffer of sketches; head points at the OLDEST
	// (rotated-out-next). Newest = (head + N - 1) % N.
	buckets []*ddsketch.DDSketch
	head    int
}

func NewQuantileTracker(logger *zerolog.Logger) *QuantileTracker {
	q := &QuantileTracker{
		logger:  logger,
		buckets: make([]*ddsketch.DDSketch, rollingBuckets),
	}
	for i := range q.buckets {
		// 1% relative accuracy — matches the original single-sketch tracker.
		q.buckets[i], _ = ddsketch.NewDefaultDDSketch(0.01)
	}
	return q
}

// Add writes the duration sample to the newest sub-bucket.
func (q *QuantileTracker) Add(value float64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.buckets)
	cur := (q.head + n - 1) % n
	if err := q.buckets[cur].Add(value); err != nil {
		q.logger.Warn().Err(err).Float64("value", value).Msg("failed to add value to quantile tracker")
	}
}

// Reset wipes ALL buckets — full window flush. Test/admin only.
// The request path uses RotateOldest for sliding semantics.
func (q *QuantileTracker) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.buckets {
		q.buckets[i], _ = ddsketch.NewDefaultDDSketch(0.01)
	}
	q.head = 0
}

// RotateOldest re-inits the oldest bucket (dropping that slice of the
// window) and advances head — the (formerly-newest + 1) slot becomes
// the new write target. Called by Tracker.rotateMetricsLoop.
func (q *QuantileTracker) RotateOldest() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.buckets[q.head], _ = ddsketch.NewDefaultDDSketch(0.01)
	q.head = (q.head + 1) % len(q.buckets)
}

func (q *QuantileTracker) MarshalJSON() ([]byte, error) {
	// Build the merged view once, then read every quantile off it — one
	// merge instead of five.
	merged := q.mergedSnapshot()
	return sonic.Marshal(struct {
		P50 float64 `json:"p50"`
		P70 float64 `json:"p70"`
		P90 float64 `json:"p90"`
		P95 float64 `json:"p95"`
		P99 float64 `json:"p99"`
	}{
		P50: quantileSeconds(q.logger, merged, 0.50),
		P70: quantileSeconds(q.logger, merged, 0.70),
		P90: quantileSeconds(q.logger, merged, 0.90),
		P95: quantileSeconds(q.logger, merged, 0.95),
		P99: quantileSeconds(q.logger, merged, 0.99),
	})
}

func (q *QuantileTracker) GetQuantile(qtile float64) time.Duration {
	merged := q.mergedSnapshot()
	seconds := quantileSeconds(q.logger, merged, qtile)
	return time.Duration(seconds * float64(time.Second))
}

// GetQuantiles returns the requested quantiles in one shot, sharing
// ONE merged snapshot across all queries. Drop-in replacement for a
// loop of `GetQuantile(q[i])` calls — same result, 1/Nth the merge
// cost. Use this whenever you need ≥2 quantiles off the same tracker.
//
// HOT PATH — `policy.snapshotMetrics` reads p50/p70/p90/p95/p99 per
// (upstream, method) per slot tick. Calling `GetQuantile` five times
// in a row hit pprof at 90% of GetQuantile's cum time in
// `mergedSnapshot`, which allocates a fresh DDSketch and merges all
// ring-buffer buckets EVERY call. With per-method snapshots at
// e.g. 5 upstreams × 30 methods × 5 quantiles per tick, that's 750
// redundant merges per network tick.
//
// Result slice matches the input slice 1:1; an empty/zero-bucket
// sketch yields a zero Duration in every slot.
func (q *QuantileTracker) GetQuantiles(qtiles []float64) []time.Duration {
	out := make([]time.Duration, len(qtiles))
	if len(qtiles) == 0 {
		return out
	}
	merged := q.mergedSnapshot()
	for i, qt := range qtiles {
		seconds := quantileSeconds(q.logger, merged, qt)
		out[i] = time.Duration(seconds * float64(time.Second))
	}
	return out
}

// mergedSnapshot returns an ephemeral DDSketch holding the union of
// all sub-buckets. Holds the read lock for the duration of the merge —
// concurrent Adds may briefly block, but DDSketch.MergeWith is fast
// (microseconds for typical sketch sizes) and the merge runs once
// per quantile query (or once per MarshalJSON when all five are
// pulled together).
func (q *QuantileTracker) mergedSnapshot() *ddsketch.DDSketch {
	q.mu.RLock()
	defer q.mu.RUnlock()
	out, _ := ddsketch.NewDefaultDDSketch(0.01)
	for _, b := range q.buckets {
		if b == nil {
			continue
		}
		if err := out.MergeWith(b); err != nil {
			q.logger.Warn().Err(err).Msg("failed to merge quantile bucket; skipping")
		}
	}
	return out
}

// quantileSeconds wraps the GetValueAtQuantile error/NaN/Inf guards
// that the original implementation applied. Returns 0 (rather than a
// poisoned value) on any pathological state.
func quantileSeconds(logger *zerolog.Logger, sketch *ddsketch.DDSketch, qtile float64) float64 {
	if sketch == nil {
		return 0
	}
	seconds, err := sketch.GetValueAtQuantile(qtile)
	if err != nil {
		// Empty sketch — no samples yet.
		return 0
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		logger.Warn().
			Float64("qtile", qtile).
			Float64("rawSeconds", seconds).
			Bool("isNaN", math.IsNaN(seconds)).
			Bool("isInf", math.IsInf(seconds, 0)).
			Msg("quantile tracker returned invalid value, returning 0")
		return 0
	}
	return seconds
}
