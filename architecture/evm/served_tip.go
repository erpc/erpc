package evm

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ServedTipInput is a single observation: an upstream's last-known tip block.
// Callers are responsible for excluding syncing or cordoned upstreams BEFORE
// passing observations here; the picker treats every input as a candidate.
type ServedTipInput struct {
	// UpstreamID is preserved only for telemetry attribution. It is not used
	// in the pick.
	UpstreamID string

	// BlockNumber is the upstream's reported tip. Zero or negative values are
	// treated as "no data yet" and filtered before picking.
	BlockNumber int64
}

// ServedTipPick is the picker's output.
type ServedTipPick struct {
	// Tip is the value to advertise as latest/finalized: the highest block
	// number that a strict MAJORITY of the inputs have already reached, or 0
	// when there are no valid inputs.
	Tip int64

	// Freshest is the freshest CORROBORATED view: the 2nd-highest valid input
	// (or the only input when N=1) — the reference for the deliberate-lag
	// gauge (Freshest - Tip). Using the 2nd-highest instead of the raw max
	// means a single rogue far-future upstream cannot inflate the lag gauge
	// (the problem the old velocity gate solved via MaxEligible: one
	// wrong-chain endpoint used to make the gauge read hundreds of thousands
	// of blocks). The absolute per-upstream maxima remain observable via
	// erpc_upstream_latest_block_number.
	Freshest int64

	// Inputs is the number of valid (BlockNumber > 0) observations.
	Inputs int
}

// PickServedTip returns the freshest block number that a strict majority of
// the eligible upstreams have already reached: the floor(N/2)-th highest head
// (0-indexed, descending). This is the entire served-tip algorithm.
//
// One order statistic over the live heads provides every protection the
// previous cluster + velocity-gate + persistent-counter pipeline engineered
// separately — with zero state and zero configuration:
//
//   - GARBAGE-RESISTANT: a far-future tip from a rogue/wrong-chain upstream
//     cannot move the pick unless a strict majority agrees with it.
//   - STUCK-RESISTANT: a frozen or lagging upstream cannot hold the pick
//     back unless it IS the majority (a halted chain — where holding back is
//     the correct answer).
//   - SERVABLE: by construction at least floor(N/2)+1 upstreams already have
//     the advertised block, so interpolated "latest" requests land on
//     upstreams that can actually serve it.
//   - MONOTONIC IN PRACTICE: each input is itself a monotonic, rollback-
//     tolerant poller counter, and an order statistic over monotonic inputs
//     only regresses when the ELIGIBLE SET changes — bounded by the live
//     head spread (a couple of blocks), the same wobble any load-balanced
//     provider exhibits.
//   - WEDGE-IMMUNE: nothing is persisted and nothing is predicted — no
//     inherited counter, no anchor clock, no block-time estimate, no
//     absorbing state. The 2026-06 production incident (served tips silently
//     frozen hours in the past, fleet-wide) is structurally impossible here;
//     networks_served_tip_invariants_test.go (package erpc) pins that class
//     of outcome forever.
//
// Examples (heads descending): N=1 → that head; N=2 → the LOWER (never
// advertise a block only one upstream claims); N=3 → 2nd; N=4 → 3rd; N=5 → 3rd.
func PickServedTip(tips []ServedTipInput) ServedTipPick {
	heads := make([]int64, 0, len(tips))
	for _, t := range tips {
		if t.BlockNumber > 0 {
			heads = append(heads, t.BlockNumber)
		}
	}
	if len(heads) == 0 {
		return ServedTipPick{}
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i] > heads[j] })
	freshest := heads[0]
	if len(heads) > 1 {
		// Corroborated freshest: a single rogue far-future tip must not be
		// able to inflate the lag reference (see ServedTipPick.Freshest).
		freshest = heads[1]
	}
	return ServedTipPick{
		Tip:      heads[len(heads)/2],
		Freshest: freshest,
		Inputs:   len(heads),
	}
}

// ─── Long-term trajectory referee ────────────────────────────────────────────
//
// PickServedTip is a snapshot of one instant, and so is every filter feeding
// it: they all answer "what do the eligible upstreams say RIGHT NOW". That is
// exactly the assumption a majority stall breaks. If N upstreams freeze inside
// the lag-exclusion threshold (or a fleet-wide shared-counter freeze stalls the
// lag metric itself), the majority IS the stale group, and the 2 upstreams that
// genuinely have the chain's head are outvoted — no instantaneous rule can tell
// that apart from a chain that simply stopped.
//
// One thing does distinguish them: what the network's head has been doing for
// the last ten minutes. TipTrajectory records that track and lets the referee
// answer "which of these groups is where the chain should be by now".
//
// STRUCTURAL LIMITS — this is deliberately NOT the stateful tip pipeline that
// commit 7dee07c0 removed (a velocity gate plus a persisted monotonic clamp,
// which reached an absorbing state where every live tip was rejected forever
// and the served tip froze fleet-wide):
//
//   - ADVISORY, NEVER GENERATIVE. The referee only ever chooses among head
//     values live upstreams are offering in this very evaluation. It cannot
//     invent, remember, or extrapolate a servable block; the trajectory is used
//     ONLY to rank the groups those upstreams form.
//   - UPWARD-ONLY. It may raise the pick to a corroborated fresher group; it can
//     never lower or hold one. A referee that cannot hold a value back cannot
//     freeze a tip — the 7dee07c0 failure mode is unreachable, not merely
//     unlikely. (Erring low is already the majority picker's job, and the
//     regression guard's.)
//   - SELF-LIMITING EVIDENCE. Samples are the plain MEDIAN of the live heads —
//     never the served value, never the referee's own output. During a majority
//     stall the tracker therefore keeps recording the stalled median, the fitted
//     velocity decays, and the referee stops participating on its own after
//     roughly half a window. Its intervention has a built-in expiry that no
//     configuration can extend.
//   - FAIL OPEN ON ANY UNCERTAINTY, and no shared state: everything below is
//     per-process, in-memory, rebuilt from live observations after a restart.
//     Per-pod independence is the point — a shared head counter is how the
//     2026-08 celo trigger propagated fleet-wide in the first place.

const (
	// tipSampleInterval throttles recording to one sample per evaluation-with-
	// this-much-elapsed. Fast enough that the default 10-minute window holds
	// 120 samples, slow enough that the O(n log n) refit runs at most 0.2 Hz
	// per network+axis while reads stay O(1).
	tipSampleInterval = 5 * time.Second

	// tipMinSamples is the smallest buffer the fit is computed over: below it
	// a handful of points could define a "trajectory" on noise alone.
	tipMinSamples = 30

	// tipMaxSampleGap is how stale the freshest sample may be (as of the START
	// of an evaluation) for the fit to still describe the present. A gap larger
	// than this is where a halt or a burst hides, and extrapolating across it
	// is guessing — the referee steps aside instead.
	tipMaxSampleGap = time.Minute

	// tipMinGroupSize is the corroboration rule: one witness is not evidence,
	// however perfectly it matches the trajectory. A group must contain at
	// least two upstreams to be electable.
	tipMinGroupSize = 2

	// tipToleranceSigmas (k) scales the window's OWN residual spread into the
	// distance-from-expected a group may sit at and still count as
	// on-trajectory. The spread is a median absolute residual, which for
	// normal-ish noise is ≈0.67σ, so k=6 is ≈4σ: a chain that pauses and
	// bursts widens its own gate automatically, while a metronomic chain keeps
	// the floor (EvmServedTipConfig.MaxRegressionBlocks).
	tipToleranceSigmas = 6.0

	// tipClusterSeconds derives the cluster width from the fitted velocity:
	// heads within this much CHAIN PROGRESS of each other are the same view of
	// the head. 10s comfortably covers poller-interval skew and propagation
	// between healthy providers, and is far below any stall worth correcting.
	tipClusterSeconds = 10.0

	// tipMinClusterWidth keeps sub-second-block chains (and a small fitted
	// velocity generally) from splitting a healthy fleet into singletons.
	tipMinClusterWidth = 16

	// tipMaxClusterWidth is an overflow stop, not a tuning knob: an absurd
	// fitted velocity must not produce an int64 conversion of a huge float. A
	// too-wide cluster merges everything into one group, which can only make
	// the referee defer.
	tipMaxClusterWidth = int64(1) << 40

	// tipMinBufferCapacity / tipMaxBufferCapacity bound the ring derived from
	// the configured window (tipBufferCapacity).
	tipMinBufferCapacity = 64
	tipMaxBufferCapacity = 1024
)

// TipTrajectoryParams are the per-network knobs the referee reads on every
// evaluation. They come straight from EvmServedTipConfig, so a config change
// takes effect without rebuilding the tracker.
type TipTrajectoryParams struct {
	// Window is the minimum span of recorded history required before the
	// referee may participate at all. <= 0 disables the tracker entirely:
	// nothing is recorded and nothing is computed.
	Window time.Duration

	// ToleranceFloor is the minimum distance-from-expected a group may sit at
	// and still be on-trajectory, in blocks (EvmServedTipConfig's
	// MaxRegressionBlocks, defaulted by the caller). The observed residual
	// spread can only widen the gate, never narrow it below this.
	ToleranceFloor int64
}

// TipTrajectoryDecision is the referee's advice for one evaluation. Pick is
// always a value the caller may serve as-is: the majority pick it passed in,
// or a corroborated group's minimum.
type TipTrajectoryDecision struct {
	// Pick is the block to serve: the majority pick unless Overrode.
	Pick int64

	// Overrode reports that Pick differs from the majority pick — the only
	// outcome that is an intervention, and the one worth an alert.
	Overrode bool

	// Declined reports the near miss: the group the trajectory elected sat
	// ABOVE the majority pick — an override was on the table — and the referee
	// refused it because the group was not close enough to where the head
	// should be. It is the only other outcome worth counting, and like Overrode
	// it is silent in steady state: a fleet in one cluster elects that cluster,
	// and a fleet permanently split around its own median elects the median's
	// group (the trajectory is fitted to the median, so no permanently-offset
	// group is ever closer to it), so neither flag is set.
	Declined bool

	// Expected / Tolerance / VelocityPerSec describe the fit that produced the
	// decision. Populated only once the confidence gate passes; for logs.
	Expected       int64
	Tolerance      int64
	VelocityPerSec float64
}

// tipSample is one recorded observation of where the network's head was.
type tipSample struct {
	atMs int64
	head int64
}

// tipFit is the robust linear fit over the buffer, recomputed on insertion and
// read lock-free on every evaluation.
type tipFit struct {
	refMs   int64   // timestamp of the newest sample in the fit
	refHead float64 // FITTED head at refMs (not the raw sample: one poisoned sample must not define the anchor)
	vPerSec float64 // robust velocity, blocks/second
	spread  float64 // median absolute residual around the fit, blocks
	spanMs  int64   // newest − oldest sample timestamp
	count   int
}

// expectedAt extrapolates the fit to an instant on the tracker's timeline.
func (f *tipFit) expectedAt(nowMs int64) float64 {
	return f.refHead + f.vPerSec*float64(nowMs-f.refMs)/1000
}

// confident is the gate: unless EVERY condition holds the referee is a no-op
// and the caller's majority pick stands byte-identically. prevSampleAgeMs is
// the age of the freshest sample as of the START of this evaluation — the
// evaluation's own sample must not paper over a gap in the history.
func (f *tipFit) confident(nowMs int64, prevSampleAgeMs int64, p TipTrajectoryParams) bool {
	if f == nil || f.count < tipMinSamples {
		return false
	}
	if f.spanMs < p.Window.Milliseconds() {
		// Also the warm-up condition: an empty buffer after boot has no span.
		return false
	}
	if prevSampleAgeMs > tipMaxSampleGap.Milliseconds() {
		return false
	}
	if !(f.vPerSec > 0) || math.IsInf(f.vPerSec, 0) {
		// A non-advancing (or NaN) fit describes a halted chain, where the
		// majority pick is already the right answer.
		return false
	}
	return !math.IsInf(f.refHead, 0) && !math.IsNaN(f.refHead) && nowMs >= f.refMs
}

// TipTrajectory is one network+axis's long-term head track: a ring of head
// samples plus the robust fit over them. The zero value is ready to use and
// allocates on first use; a tracker whose Window is <= 0 never allocates at
// all.
type TipTrajectory struct {
	mu      sync.Mutex
	samples []tipSample
	next    int // ring write cursor
	count   int

	// ordered / slopes / resid are refit scratch, retained so the periodic fit
	// allocates nothing after warm-up.
	ordered []tipSample
	slopes  []float64
	resid   []float64

	// base is the instant the first sample was recorded; every timestamp below
	// is milliseconds since it, measured with time.Time's MONOTONIC reading.
	// A velocity fit is exactly the thing an NTP step would corrupt (a wall
	// clock that jumps injects a slope no chain ever had), so the timeline the
	// fit lives on never touches wall time.
	base         atomic.Pointer[time.Time]
	lastSampleMs atomic.Int64
	fit          atomic.Pointer[tipFit]
}

// SampleCount returns how many samples the tracker holds. Diagnostics and
// tests only — the decision path never needs it.
func (t *TipTrajectory) SampleCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count
}

// Observe records this evaluation's live-head median (throttled) and returns
// the trajectory's advice for it.
//
// `tips` must be the LIVE head observations of this same evaluation (the
// caller drops availability-bound-capped upstreams first — a serving range is
// not a head), and `median` the plain majority pick over them. Both the sample
// and the candidate groups come from those heads, so the referee's own output
// never feeds back into its evidence.
func (t *TipTrajectory) Observe(now time.Time, tips []ServedTipInput, median int64, p TipTrajectoryParams) TipTrajectoryDecision {
	d := TipTrajectoryDecision{Pick: median}
	if p.Window <= 0 || median <= 0 {
		return d
	}

	nowMs, prevSampleAgeMs := t.record(now, median, p)

	fit := t.fit.Load()
	if !fit.confident(nowMs, prevSampleAgeMs, p) {
		return d
	}

	heads := make([]int64, 0, len(tips))
	for _, in := range tips {
		if in.BlockNumber > 0 {
			heads = append(heads, in.BlockNumber)
		}
	}
	if len(heads) < tipMinGroupSize {
		return d
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i] > heads[j] })

	expected := fit.expectedAt(nowMs)
	tolerance := float64(p.ToleranceFloor)
	if widened := tipToleranceSigmas * fit.spread; widened > tolerance {
		tolerance = widened
	}
	d.Expected = int64(math.Round(expected))
	d.Tolerance = int64(math.Round(tolerance))
	d.VelocityPerSec = fit.vPerSec

	// Single-linkage clustering over the descending heads: a gap wider than one
	// cluster width starts a new group. Candidate groups are those with at
	// least tipMinGroupSize members; the winner is the one whose MAX (its best
	// claim about the head — using the min would penalise a wide group for its
	// slowest member) sits closest to where the trajectory says the head is.
	width := tipClusterWidth(fit.vPerSec)
	bestDistance := math.Inf(1)
	var bestMin int64
	for i := 0; i < len(heads); {
		j := i + 1
		for j < len(heads) && heads[j-1]-heads[j] <= width {
			j++
		}
		if j-i >= tipMinGroupSize {
			groupMax, groupMin := heads[i], heads[j-1]
			if distance := math.Abs(float64(groupMax) - expected); distance < bestDistance {
				bestDistance, bestMin = distance, groupMin
			}
		}
		i = j
	}

	if bestDistance > tolerance {
		// No candidate group at all (+Inf), or the elected one is nowhere near
		// the trajectory — nothing here is better evidence than the majority.
		d.Declined = bestMin > median
		return d
	}
	if bestMin <= median {
		// UPWARD-ONLY (see the section comment): the referee exists to let a
		// corroborated fresh minority outvote a stalled majority. Lowering or
		// holding the tip is the majority picker's and the regression guard's
		// job, and is the only direction from which a frozen tip is reachable.
		return d
	}

	// Serve the group's MINIMUM: every member of a winning group already has
	// that block, so the advertised tip stays servable by all of them — the
	// same invariant the majority order statistic provides.
	d.Pick = bestMin
	d.Overrode = true
	return d
}

// record appends a sample if at least tipSampleInterval has passed since the
// last one. It returns `now` on the tracker's monotonic timeline, and the age
// there of the previous freshest sample — the age BEFORE this evaluation's own
// sample, so a gap in the history cannot be papered over by the sample that
// notices it (a value past tipMaxSampleGap when there is no previous sample).
//
// The lock is taken only when a sample is actually due, so the per-request cost
// is two atomic loads.
func (t *TipTrajectory) record(now time.Time, head int64, p TipTrajectoryParams) (nowMs int64, prevAgeMs int64) {
	staleSentinel := tipMaxSampleGap.Milliseconds() + 1

	if base := t.base.Load(); base != nil {
		nowMs = int64(now.Sub(*base) / time.Millisecond)
		if last := t.lastSampleMs.Load(); nowMs-last < tipSampleInterval.Milliseconds() {
			return nowMs, nowMs - last
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	base := t.base.Load()
	if base == nil {
		at := now
		base = &at
		t.base.Store(base)
	}
	nowMs = int64(now.Sub(*base) / time.Millisecond)

	prevAgeMs = staleSentinel
	if t.count > 0 {
		last := t.lastSampleMs.Load()
		if nowMs-last < tipSampleInterval.Milliseconds() {
			return nowMs, nowMs - last
		}
		prevAgeMs = nowMs - last
	}

	if t.samples == nil {
		t.samples = make([]tipSample, tipBufferCapacity(p.Window))
	}
	t.samples[t.next] = tipSample{atMs: nowMs, head: head}
	t.next = (t.next + 1) % len(t.samples)
	if t.count < len(t.samples) {
		t.count++
	}
	t.lastSampleMs.Store(nowMs)
	t.refit()

	return nowMs, prevAgeMs
}

// refit recomputes the robust fit over the whole buffer. Called under t.mu, at
// most once per tipSampleInterval.
//
// Velocity is the MEDIAN OF HALF-WINDOW-LAGGED SLOPES: slope_i between sample i
// and sample i+n/2, median over the ~n/2 of them. Two properties matter here
// and full Theil-Sen buys neither: every slope spans about half the window, so
// per-sample noise is divided by a long baseline; and any single poisoned
// sample enters at most two of the ~90 slopes, so the median never moves.
// Theil-Sen's n²/2 pairs (≈16k at n=180) would cost a 128 KB sort per fit for
// the same answer. The intercept is the median residual (the standard robust
// intercept), and the spread is the median absolute residual around the
// resulting line — the window's own noise, which is what widens the tolerance
// for a chain that pauses and bursts.
func (t *TipTrajectory) refit() {
	n := t.count
	if n < tipMinSamples {
		t.fit.Store(nil)
		return
	}

	// Materialise the ring chronologically.
	ord := t.ordered[:0]
	if t.count < len(t.samples) {
		ord = append(ord, t.samples[:t.count]...)
	} else {
		ord = append(ord, t.samples[t.next:]...)
		ord = append(ord, t.samples[:t.next]...)
	}
	t.ordered = ord

	newest := ord[n-1]
	lag := n / 2
	slopes := t.slopes[:0]
	for i := 0; i+lag < n; i++ {
		dt := float64(ord[i+lag].atMs-ord[i].atMs) / 1000
		if dt <= 0 {
			continue
		}
		slopes = append(slopes, float64(ord[i+lag].head-ord[i].head)/dt)
	}
	t.slopes = slopes
	if len(slopes) == 0 {
		t.fit.Store(nil)
		return
	}
	vPerSec := medianInPlace(slopes)

	// Residuals in coordinates relative to the newest sample: seconds and
	// blocks, never epoch milliseconds or 8-digit block numbers, so float64
	// keeps full precision on chains of any height.
	resid := t.resid[:0]
	for _, s := range ord {
		ts := float64(s.atMs-newest.atMs) / 1000
		resid = append(resid, float64(s.head-newest.head)-vPerSec*ts)
	}
	t.resid = resid
	intercept := medianInPlace(resid)
	for i, r := range resid {
		resid[i] = math.Abs(r - intercept)
	}

	t.fit.Store(&tipFit{
		refMs:   newest.atMs,
		refHead: float64(newest.head) + intercept,
		vPerSec: vPerSec,
		spread:  medianInPlace(resid),
		spanMs:  newest.atMs - ord[0].atMs,
		count:   n,
	})
}

// tipBufferCapacity sizes the ring from the configured window: enough samples
// to span it with headroom for jitter, so a window an operator raises still
// becomes reachable instead of silently never satisfying the span gate. The
// hard cap means windows beyond ~85 minutes can never be spanned, and would
// stand the referee down permanently — see EvmServedTipConfig.TrajectoryWindow.
func tipBufferCapacity(window time.Duration) int {
	n := int(window/tipSampleInterval)*5/4 + 8
	if n < tipMinBufferCapacity {
		return tipMinBufferCapacity
	}
	if n > tipMaxBufferCapacity {
		return tipMaxBufferCapacity
	}
	return n
}

// tipClusterWidth is how far apart two heads may be and still be one view of
// the chain: tipClusterSeconds of chain progress, floored at
// tipMinClusterWidth.
func tipClusterWidth(vPerSec float64) int64 {
	w := vPerSec * tipClusterSeconds
	if !(w > tipMinClusterWidth) { // also catches NaN
		return tipMinClusterWidth
	}
	if w > float64(tipMaxClusterWidth) {
		return tipMaxClusterWidth
	}
	return int64(w)
}

// medianInPlace sorts xs and returns its median.
func medianInPlace(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	if n := len(xs); n%2 == 1 {
		return xs[n/2]
	}
	return (xs[len(xs)/2-1] + xs[len(xs)/2]) / 2
}
