package evm

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
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

	// Sorted is the valid inputs, DESCENDING by block number — the order
	// statistic's own working slice, exposed so the trajectory referee can
	// cluster the very same ballot without sorting it a second time. Nil when
	// there are no valid inputs; never mutate it.
	Sorted []ServedTipInput
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
	// One sorted slice serves the whole evaluation: the order statistic here
	// and the referee's clustering downstream (ServedTipPick.Sorted). The
	// UpstreamIDs travel with it because group IDENTITY — not just the head
	// values — is what the referee's dwell test is keyed on.
	sorted := make([]ServedTipInput, 0, len(tips))
	for _, t := range tips {
		if t.BlockNumber > 0 {
			sorted = append(sorted, t)
		}
	}
	if len(sorted) == 0 {
		return ServedTipPick{}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].BlockNumber > sorted[j].BlockNumber })
	freshest := sorted[0].BlockNumber
	if len(sorted) > 1 {
		// Corroborated freshest: a single rogue far-future tip must not be
		// able to inflate the lag reference (see ServedTipPick.Freshest).
		freshest = sorted[1].BlockNumber
	}
	return ServedTipPick{
		Tip:      sorted[len(sorted)/2].BlockNumber,
		Freshest: freshest,
		Inputs:   len(sorted),
		Sorted:   sorted,
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
//   - CORROBORATION IS EARNED OVER TIME, NOT IN AN INSTANT. Matching the
//     trajectory in one sample is not evidence: during a chain halt the expected
//     head SWEEPS upward through every fixed offset, so any static fork/pair sits
//     "on trajectory" for as long as the sweep takes to cross it, and a routine
//     15-second divergence puts a healthy pair "ahead" for one evaluation. Both
//     were confirmed exploits of the instant test. A group must therefore hold
//     its place — elected AND within tolerance — for a continuous tipDwellDuration
//     before it may win, and must have ADVANCED its own head over that stretch at
//     a fraction of the fitted velocity (tipDwellMinVelocityFraction). A frozen
//     group can never satisfy the second condition, whatever the first says.
//   - FAIL OPEN ON ANY UNCERTAINTY, and no shared state: everything below is
//     per-process, in-memory, rebuilt from live observations after a restart.
//     Per-pod independence is the point — a shared head counter is how the
//     2026-08 celo trigger propagated fleet-wide in the first place.

const (
	// tipSampleInterval throttles recording to one sample per evaluation-with-
	// this-much-elapsed. Fast enough that the default 10-minute window holds
	// 120 samples, slow enough that the O(n log n) refit runs at most 0.2 Hz
	// per network+axis while reads stay O(1). It lives in common because the
	// config validator derives the allowed trajectoryWindow range from it.
	tipSampleInterval = common.ServedTipTrajectorySampleInterval

	// tipMinSamples is the smallest buffer the fit is computed over: below it
	// a handful of points could define a "trajectory" on noise alone.
	tipMinSamples = 30

	// tipMaxSampleGap is how large a hole the window may contain — both at its
	// newest end (the age of the freshest sample as of the START of an
	// evaluation) and ANYWHERE INSIDE it (tipFit.maxGapMs). A gap larger than
	// this is where a halt or a burst hides, and a fit whose window straddles
	// one is extrapolating across the very thing it cannot see: the residuals
	// of the samples before the hole then pull the robust intercept up by a
	// whole gap of chain progress, which is exactly how a rogue pair sitting at
	// "where the head would be if the chain had not stopped" wins an election.
	// Checking only the newest sample's age stood the referee down for a single
	// evaluation — the next one, a millisecond later, extrapolated across the
	// hole regardless. The gap must therefore leave the WINDOW before the fit
	// counts as describing the present.
	tipMaxSampleGap = time.Minute

	// tipMinGroupSize is the corroboration rule: one witness is not evidence,
	// however perfectly it matches the trajectory. A group must contain at
	// least two upstreams to be electable.
	//
	// It stays 2 for a fleet of ANY size, deliberately: the whole point of the
	// referee is that two upstreams which genuinely hold the chain's head must
	// be able to outvote a larger stalled group, and scaling the requirement
	// with the fleet would hand the stalled majority a veto in exactly the
	// topology the referee exists for. What makes two trustworthy is not the
	// count but the dwell and velocity conditions below: two witnesses that
	// have tracked the network's own trajectory for half a minute, advancing
	// their own heads while doing so.
	tipMinGroupSize = 2

	// tipDwellDuration is how long the elected group must hold its place —
	// continuously within tolerance of the expected head — before it may
	// outvote the majority. Six samples at the 5s throttle: long enough that a
	// routine divergence (a 15s poller hiccup on half the fleet, the confirmed
	// false-override shape) resolves before it can win, short enough that a
	// genuine majority stall is corrected inside one block-explorer refresh.
	tipDwellDuration = 30 * time.Second

	// tipMajorityStallSeconds is how far behind the trajectory the MAJORITY pick
	// must have fallen — expressed as chain progress in SECONDS, converted to
	// blocks through the fitted velocity — before the referee may outvote it.
	//
	// It exists because nothing else in the decision path tests the referee's
	// own premise. The tolerance gate asks whether the ELECTED GROUP is on the
	// trajectory; the upward-only rule asks whether that group is above the
	// majority. Neither asks the question the override is named for: has the
	// majority actually stalled? Without this gate the answer is assumed, and
	// the assumption is wrong on any healthy fleet with a persistent upper
	// group — see the note on TipTrajectoryDecision.Declined.
	//
	// The gate is a duration, not a block count, because block counts are not
	// comparable across chains: ToleranceFloor's 1024-block default is ~3 hours
	// of a 12s chain but only ~10 minutes of a 1.7 blocks/s one, so as an
	// absolute floor it binds on slow chains and never binds on fast ones.
	// Seconds of missing chain progress mean the same thing everywhere.
	//
	// 30s matches tipDwellDuration: the elected group must hold its place for
	// that long anyway, so requiring the majority to be at least that far
	// behind adds no latency to correcting a genuine stall — a frozen majority
	// crosses this gate and earns the dwell over the same interval.
	tipMajorityStallSeconds = 30.0

	// tipDwellMinVelocityFraction is the share of the fitted velocity the
	// elected group must have delivered FROM ITS OWN HEAD over the dwell. It is
	// a lower bound only — closeness to expected already bounds the group from
	// above — and its job is to disqualify anything that is not moving: a fork,
	// a wrong-chain pair, a frozen shared counter parked at a fixed offset. A
	// static group's advance is 0 and the threshold is strictly positive
	// whenever the referee is confident at all (confident() requires v > 0 and
	// the dwell is ≥30s), so a static group can never be elected — not merely
	// unlikely to be. Half the fitted velocity leaves room for a group that is
	// catching up in bursts.
	tipDwellMinVelocityFraction = 0.5

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
	// the configured window (tipBufferCapacity). The cap is shared with the
	// config validator, which rejects a window this ring could never span.
	tipMinBufferCapacity = 64
	tipMaxBufferCapacity = common.ServedTipTrajectoryMaxSamples
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
	// refused it, because the group was not close enough to where the head
	// should be, because it had not held its place long enough, or because the
	// majority it would have outvoted had not actually stalled.
	//
	// A fleet in one cluster elects that cluster and sets neither flag. A fleet
	// permanently split around its own median sets Declined, NOT Overrode.
	//
	// It was once claimed here that such a fleet could not even elect its upper
	// group, reasoning that the fit is made from the median so no
	// permanently-offset group can sit nearer to it. That is wrong. Once the
	// split exceeds one cluster width the lower upstreams are singletons, which
	// tipMinGroupSize rejects, so the upper group is often the ONLY candidate
	// and wins by default however far from the median it sits — it is elected
	// against the tolerance, never against the median's distance. What keeps
	// election from becoming an override on a healthy fleet is the
	// majority-stall gate (tipMajorityStallSeconds), not the geometry of the
	// fit. Observed on a four-upstream fleet at a persistent ladder, which
	// before that gate took a ~24-block override every ~28 minutes.
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
	refMs    int64   // timestamp of the newest sample in the fit
	refHead  float64 // FITTED head at refMs (not the raw sample: one poisoned sample must not define the anchor)
	vPerSec  float64 // robust velocity, blocks/second
	spread   float64 // median absolute residual around the fit, blocks
	spanMs   int64   // newest − oldest sample timestamp
	maxGapMs int64   // largest interval BETWEEN two consecutive samples in the window
	count    int
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
	if prevSampleAgeMs > tipMaxSampleGap.Milliseconds() || f.maxGapMs > tipMaxSampleGap.Milliseconds() {
		// Both ends of the same rule: the track must be unbroken up to now AND
		// unbroken across the whole window it was fitted over (see
		// tipMaxSampleGap). A hole inside the window stands the referee down
		// until the hole has scrolled out of the ring.
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

	// window is the TipTrajectoryParams.Window the ring was sized for. The
	// params are re-read on every evaluation (a config reload takes effect
	// without rebuilding the tracker), but the ring is sized once — so a raised
	// window would otherwise demand a span the existing ring can never hold, and
	// the referee would stand down forever while looking configured. A change
	// therefore rebuilds the ring and clears the fit and the dwell: the tracker
	// re-warms from live observations, which is the same inert-then-confident
	// path every process takes after a deploy.
	window time.Duration

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

	// dwell is the current corroboration stretch: which group has been the
	// elected one, since when, and where its head was then. nil = no stretch in
	// progress. Immutable once stored, so a dwell that is simply CONTINUING
	// costs one atomic load per evaluation and no write at all.
	dwell atomic.Pointer[tipDwell]
}

// tipDwell is one continuous stretch during which the same group of upstreams
// has been elected by the trajectory and has stayed within tolerance of the
// expected head. Its fields are never mutated after the Store; a change of
// group, a miss, or a loss of confidence replaces or clears the whole struct.
type tipDwell struct {
	// group is the order-independent identity of the group's MEMBERS
	// (groupIdentity), not of their head values: a stretch belongs to a set of
	// upstreams, and any change to that set starts a new one.
	group uint64

	// startMs is when the stretch began, and startHead the group's max head at
	// that instant — the two ends of the velocity-agreement measurement.
	startMs   int64
	startHead int64
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
// `sorted` must be ServedTipPick.Sorted for the LIVE head observations of this
// same evaluation (the caller drops availability-bound-capped upstreams first —
// a serving range is not a head), i.e. already filtered and DESCENDING, and
// `median` the plain majority pick over them. Both the sample and the candidate
// groups come from those heads, so the referee's own output never feeds back
// into its evidence.
func (t *TipTrajectory) Observe(now time.Time, sorted []ServedTipInput, median int64, p TipTrajectoryParams) TipTrajectoryDecision {
	d := TipTrajectoryDecision{Pick: median}
	if p.Window <= 0 || median <= 0 {
		return d
	}
	if len(sorted) < tipMinGroupSize {
		// Nothing here can ever produce a decision — no group of two can form —
		// so nothing is recorded either. A single-upstream network (the majority
		// of chains in a large deployment) therefore allocates no ring, runs no
		// fit, and costs two comparisons per evaluation.
		return d
	}

	nowMs, prevSampleAgeMs := t.record(now, median, p)

	fit := t.fit.Load()
	if !fit.confident(nowMs, prevSampleAgeMs, p) {
		// No confident fit means no elected group, so any stretch in progress is
		// over: a group must re-earn its dwell once the referee can see again.
		t.breakDwell()
		return d
	}

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
	var bestMin, bestMax int64
	var bestGroup uint64
	for i := 0; i < len(sorted); {
		j := i + 1
		for j < len(sorted) && sorted[j-1].BlockNumber-sorted[j].BlockNumber <= width {
			j++
		}
		if j-i >= tipMinGroupSize {
			groupMax, groupMin := sorted[i].BlockNumber, sorted[j-1].BlockNumber
			if distance := math.Abs(float64(groupMax) - expected); distance < bestDistance {
				bestDistance, bestMin, bestMax = distance, groupMin, groupMax
				bestGroup = groupIdentity(sorted[i:j])
			}
		}
		i = j
	}

	if bestDistance > tolerance {
		// No candidate group at all (+Inf), or the elected one is nowhere near
		// the trajectory — nothing here is better evidence than the majority,
		// and whatever stretch was in progress just missed.
		t.breakDwell()
		d.Declined = bestMin > median
		return d
	}

	// The elected group keeps (or starts) its dwell whether or not it is above
	// the majority pick: in steady state the whole fleet is that group, and it
	// arrives at a stall with its corroboration already earned.
	corroborated := t.corroborate(nowMs, bestGroup, bestMax, fit.vPerSec)

	if bestMin <= median {
		// UPWARD-ONLY (see the section comment): the referee exists to let a
		// corroborated fresh minority outvote a stalled majority. Lowering or
		// holding the tip is the majority picker's and the regression guard's
		// job, and is the only direction from which a frozen tip is reachable.
		return d
	}
	if expected-float64(median) <= fit.vPerSec*tipMajorityStallSeconds {
		// THE MAJORITY HAS NOT STALLED, so there is nothing here to correct.
		// Everything above establishes that some group is on the trajectory and
		// sits above the majority; none of it establishes that the majority is
		// OFF the trajectory, which is the only thing that justifies trading the
		// order statistic's servability guarantee for a fresher tip.
		//
		// That gap was load-bearing. Once a fleet splits wider than one cluster
		// width, the upper group is frequently the ONLY electable group — the
		// lower upstreams are singletons, and tipMinGroupSize rejects them — so
		// the tolerance gate is the sole remaining check, and at ToleranceFloor's
		// 1024-block default it admits essentially any split a real fleet
		// produces. Election then implies override, and the referee fires on
		// ordinary poller skew.
		//
		// Production, on a ~1.66 blocks/s chain with four upstreams: majority
		// pick 30,699,698, elected group 30,699,722, expected 30,699,716,
		// tolerance 1024. The majority was 18 blocks — eleven seconds — off the
		// trajectory, and the referee raised the tip by 24 blocks every ~28
		// minutes. Nothing had stalled.
		//
		// Declined, not silent: an override was genuinely on the table, and the
		// near miss belongs in the same counter as the other refusals.
		d.Declined = true
		return d
	}
	if !corroborated {
		// An override was on the table and the group has not held its place long
		// enough (or has not moved its own head) to earn it — the same outcome,
		// and the same counter, as a group that sits too far from the
		// trajectory.
		d.Declined = true
		return d
	}

	// Serve the group's MINIMUM: every member of a winning group already has
	// that block, so the advertised tip stays servable by all of them — the
	// same invariant the majority order statistic provides.
	d.Pick = bestMin
	d.Overrode = true
	return d
}

// corroborate reports whether `group` has EARNED the right to outvote the
// majority, and keeps the dwell state that answers it.
//
// Two conditions, both measured over one continuous stretch of being the
// elected group:
//
//   - it has been that group for at least tipDwellDuration. A chain halt sweeps
//     the expected head upward through every fixed offset, so an instant match
//     says only "the sweep is passing over this group right now";
//   - its own head has advanced over the stretch by at least
//     tipDwellMinVelocityFraction of what the fitted velocity says the chain
//     produced. A fork, a wrong-chain pair, or a frozen counter parked at a
//     plausible offset advances by nothing and can never pass.
//
// Concurrency: several serving goroutines may race here; the only outcomes are
// "the stretch restarts" and "the stretch continues", and a restart is the
// conservative direction (it can delay an override, never cause one).
func (t *TipTrajectory) corroborate(nowMs int64, group uint64, groupMax int64, vPerSec float64) bool {
	d := t.dwell.Load()
	if d == nil || d.group != group || nowMs < d.startMs {
		t.dwell.Store(&tipDwell{group: group, startMs: nowMs, startHead: groupMax})
		return false
	}
	seconds := float64(nowMs-d.startMs) / 1000
	if seconds < tipDwellDuration.Seconds() {
		return false
	}
	return float64(groupMax-d.startHead) >= tipDwellMinVelocityFraction*vPerSec*seconds
}

// breakDwell ends any stretch in progress. Load-first, so the common case (no
// stretch, or a fleet that has been in one cluster for hours) writes nothing.
func (t *TipTrajectory) breakDwell() {
	if t.dwell.Load() != nil {
		t.dwell.Store(nil)
	}
}

// tipGroupIDScratch is how many member ids groupIdentity can sort without
// touching the heap. Real fleets are single digits; anything larger falls back
// to an allocation rather than to a wrong answer.
const tipGroupIDScratch = 32

// groupIdentity fingerprints a group by its MEMBERS: the ids are sorted, then
// hashed as one separated stream. Sorting is what makes the identity a property
// of the SET while keeping the hash's avalanche intact.
//
// The obvious cheap alternative — a commutative fold, e.g. summing per-id
// hashes — is structurally broken here, not merely weaker. FNV-1a's last step
// is (h ^ c) * prime, so for two ids sharing a prefix the DIFFERENCE between
// their hashes barely depends on that prefix; a fleet named with a shared
// vendor prefix and a numeric suffix therefore collides on suffix swaps:
// {prism-celo-1, nirvana-celo-2} and {prism-celo-2, nirvana-celo-1} folded to
// the same number. A review found four such colliding pairs inside one
// realistic 12-node fleet, and a collision here is not cosmetic — it lets a
// DIFFERENT group inherit the dwell an earlier one earned, which is exactly the
// corroboration the dwell exists to withhold.
//
// The caller's slice is in head order, so the sort works on a copy: a stack
// array for any realistic fleet, and the request path stays lock-free (a shared
// scratch buffer would have to be taken under the tracker's mutex, serialising
// every evaluation to save an allocation that does not happen).
func groupIdentity(group []ServedTipInput) uint64 {
	const (
		fnvOffset64 = uint64(14695981039346656037)
		fnvPrime64  = uint64(1099511628211)
	)
	var stack [tipGroupIDScratch]string
	var ids []string
	if len(group) <= len(stack) {
		ids = stack[:0]
	} else {
		ids = make([]string, 0, len(group))
	}
	for _, in := range group {
		ids = append(ids, in.UpstreamID)
	}
	// Insertion sort, not sort.Strings: the slices are tiny, and the interface
	// conversion sort.Strings performs would force the scratch array onto the
	// heap on every evaluation.
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}

	h := fnvOffset64
	for _, id := range ids {
		for i := 0; i < len(id); i++ {
			h ^= uint64(id[i])
			h *= fnvPrime64
		}
		// A separator, so {"ab","c"} and {"a","bc"} are different sets.
		h ^= 0x1f
		h *= fnvPrime64
	}
	return h
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

	if t.samples == nil || t.window != p.Window {
		// First use, or the operator changed trajectoryWindow under a live
		// process (see TipTrajectory.window): size the ring for the window in
		// force and start the track again, since neither the old samples' span
		// nor a fit over them describes the new configuration.
		t.samples = make([]tipSample, tipBufferCapacity(p.Window))
		t.window = p.Window
		t.next, t.count = 0, 0
		t.fit.Store(nil)
		t.dwell.Store(nil)
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
	var maxGapMs int64
	for i, s := range ord {
		if i > 0 {
			if gap := s.atMs - ord[i-1].atMs; gap > maxGapMs {
				maxGapMs = gap
			}
		}
		ts := float64(s.atMs-newest.atMs) / 1000
		resid = append(resid, float64(s.head-newest.head)-vPerSec*ts)
	}
	t.resid = resid
	intercept := medianInPlace(resid)
	for i, r := range resid {
		resid[i] = math.Abs(r - intercept)
	}

	t.fit.Store(&tipFit{
		refMs:    newest.atMs,
		refHead:  float64(newest.head) + intercept,
		vPerSec:  vPerSec,
		spread:   medianInPlace(resid),
		spanMs:   newest.atMs - ord[0].atMs,
		maxGapMs: maxGapMs,
		count:    n,
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
