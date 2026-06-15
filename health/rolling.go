package health

import "sync/atomic"

// rollingBuckets controls the granularity of every sliding-window
// metric in this package — RollingCounter AND QuantileTracker. With
// N buckets and a tracker windowSize of W, each rotation drops 1/N
// of the accumulated data and exposes a fresh sub-bucket for new
// writes. The effective rolling window is W; the tumble cliff that a
// single-bucket reset would create is replaced by N small steps.
//
// 10 is a balance: small enough that the periodic-rotate loop stays
// cheap (10 atomic operations + 1 DDSketch re-init per rotation per
// tracked metric), large enough that the per-rotate data loss is 10%
// of the window rather than the 100% of a flat tumble. Tied to a
// package constant rather than a per-tracker field so the same
// granularity applies to upstream + network metrics uniformly.
const rollingBuckets = 10

// RollingCounter is a sliding-window atomic counter. The window is
// split into `rollingBuckets` sub-buckets; Add writes to the newest,
// Load sums all, RotateOldest resets the oldest bucket and advances
// the head pointer to expose a fresh "newest" bucket for future Add
// calls. The effective rolling window is rollingBuckets times the
// caller's rotation interval (see Tracker.rotateMetricsLoop).
//
// Drop-in replacement for `atomic.Int64` at the call sites in
// TrackedMetrics: Add(int64) / Load() int64 keep the original
// semantics, with the difference that "old" samples drift out
// gradually instead of vanishing on a tumble.
type RollingCounter struct {
	buckets []atomic.Int64
	// head indexes the OLDEST bucket — the one that gets zeroed on the
	// next RotateOldest call. The NEWEST (write target) is at
	// (head + N - 1) % N. Stored atomically so Add doesn't race with
	// concurrent RotateOldest invocations on the same counter.
	head atomic.Int32
}

// NewRollingCounter constructs a counter with the package-default
// bucket count. Buckets start zero-valued; Load returns 0 until the
// first Add.
func NewRollingCounter() *RollingCounter {
	return &RollingCounter{buckets: make([]atomic.Int64, rollingBuckets)}
}

// Add increments the newest bucket. Safe to call concurrently with
// RotateOldest and Load — the worst case under a rotation race is
// that a delta lands in a bucket that's about to be rotated out
// (i.e. counts as one rotation older than it should), which is a
// negligible smoothing error for sliding-window semantics.
func (c *RollingCounter) Add(delta int64) {
	n := len(c.buckets)
	cur := (int(c.head.Load()) + n - 1) % n
	c.buckets[cur].Add(delta)
}

// Load returns the sum across all buckets — the count of events in
// the rolling window. O(N) but N is tiny (10).
func (c *RollingCounter) Load() int64 {
	var sum int64
	for i := range c.buckets {
		sum += c.buckets[i].Load()
	}
	return sum
}

// RotateOldest zeros the oldest bucket and advances the head pointer
// by one so the (formerly-newest + 1) becomes the new write target.
// Called periodically by the tracker's rotateMetricsLoop.
func (c *RollingCounter) RotateOldest() {
	n := len(c.buckets)
	head := int(c.head.Load())
	c.buckets[head].Store(0)
	c.head.Store(int32((head + 1) % n))
}

// Wipe zeros every bucket. Used by Reset for test/admin paths — the
// request path uses RotateOldest, never Wipe.
func (c *RollingCounter) Wipe() {
	for i := range c.buckets {
		c.buckets[i].Store(0)
	}
	c.head.Store(0)
}

// Store wipes every bucket and sets the newest one to v. Load() will
// return v on the next read. Used by tests that prime the counter to
// a specific known value; the request path uses Add.
func (c *RollingCounter) Store(v int64) {
	for i := range c.buckets {
		c.buckets[i].Store(0)
	}
	n := len(c.buckets)
	cur := (int(c.head.Load()) + n - 1) % n
	c.buckets[cur].Store(v)
}
