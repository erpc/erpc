package evm

import (
	"math"
	"sort"
	"time"
)

// ServedTipInput is a single observation: an upstream's last-known tip block.
// Callers are responsible for excluding syncing or cordoned upstreams BEFORE
// passing observations here; the picker treats every input as a candidate.
type ServedTipInput struct {
	// UpstreamID is preserved only for telemetry attribution
	// (e.g., per-upstream velocity-drop metrics). It is not used in the
	// clustering math.
	UpstreamID string

	// BlockNumber is the upstream's reported tip. Zero or negative values are
	// treated as "no data yet" and filtered before clustering.
	BlockNumber int64
}

// ServedTipConfig controls the cluster picker's discrimination behavior.
// Zero-value fields use sensible defaults (see field docs).
type ServedTipConfig struct {
	// ClusterDelta is the maximum gap between adjacent sorted candidates that
	// still groups them into one cluster. A larger delta makes the algorithm
	// MORE permissive (laggers stay inside the dominant cluster); a smaller
	// delta is more aggressive about splitting them out.
	//
	// Default when 0: clamp(ceil(2.0 / BlockTimeSeconds), 2, 10). The 2s
	// numerator targets ~2 seconds of vendor propagation slack; clamping to
	// [2, 10] keeps the value sane across chains with very slow or very fast
	// block times. When BlockTimeSeconds is also 0, defaults to 2.
	ClusterDelta int64

	// BlockTimeSeconds is the chain's nominal block time. Used to (a) auto-
	// derive ClusterDelta when ClusterDelta is 0, and (b) compute the
	// velocity-gate expected-max bound.
	//
	// When 0, velocity gating is disabled and ClusterDelta falls back to 2.
	BlockTimeSeconds float64

	// VelocitySlack scales the expected-blocks-since-lastServed bound.
	// Larger values are more lenient. Default 2.0 when 0.
	VelocitySlack float64

	// VelocityBufferBlocks is an additive buffer on top of the velocity gate
	// to absorb burst catch-up. Default 5 when 0.
	VelocityBufferBlocks int64

	// StaleReanchorAfter disarms the velocity gate for a pick whose anchor is
	// older than this duration: an anchor that has not advanced for many
	// multiples of the block time can no longer predict a sane expected max
	// (and if the block-time estimate is too high, the gate window expands
	// SLOWER than the chain and would exclude fresh tips forever). Such picks
	// run ungated — clustering still arbitrates outliers — and are flagged
	// via ServedTipResult.StaleReanchor.
	//
	// Default when 0: clamp(60 × BlockTimeSeconds, 30s, 10m).
	StaleReanchorAfter time.Duration
}

// ServedTipResult is the picker's output. The caller is responsible for
// applying any monotonic clamp (e.g., via a shared-state TryUpdate) on top
// of Candidate before serving the value to clients.
type ServedTipResult struct {
	// Candidate is the proposed served tip: the MIN block number of the
	// dominant cluster, or 0 if no valid candidates exist.
	Candidate int64

	// MaxObserved is the highest block number across all inputs with
	// BlockNumber > 0, computed BEFORE the velocity gate (so a velocity-rejected
	// far-future tip still counts here). Useful for the lag-vs-max telemetry
	// gauge.
	MaxObserved int64

	// ClusterCount is how many distinct clusters the inputs formed.
	ClusterCount int

	// DominantSize is the size of the dominant (winning) cluster.
	DominantSize int

	// OutliersCount is the number of inputs OUTSIDE the dominant cluster.
	// Equal to (total valid inputs - DominantSize).
	OutliersCount int

	// VelocityDropped lists the UpstreamIDs whose tips were rejected by the
	// velocity gate (claimed too-far-future given lastServedBlock and the
	// elapsed time). Always empty when the gate failed open — see
	// VelocityFailOpen.
	VelocityDropped []string

	// VelocityFailOpen reports that the velocity gate would have rejected
	// EVERY input, so the pick re-ran ungated. A bound that disagrees with all
	// live tips at once means the ANCHOR is wrong, not the world — e.g. a
	// stale persisted counter inherited at boot, or a stall the window never
	// caught up from. Without fail-open such a pick returns Candidate 0
	// ("no new information") forever, and a strict-monotonic served-tip
	// counter can never advance again. Outlier protection still applies: the
	// ungated re-run goes through the same clustering, so a lone garbage tip
	// still loses to the dominant cluster.
	VelocityFailOpen bool

	// StaleReanchor reports that the velocity gate was disarmed BEFORE
	// filtering because the anchor is older than StaleReanchorAfter (no
	// advance for many multiples of the block time). Unlike VelocityFailOpen
	// (which reacts after the gate misfired), this prevents a too-slow gate
	// window from excluding fresh tips at all. Clustering still applies.
	StaleReanchor bool

	// MaxEligible is the highest block number among inputs that SURVIVED the
	// velocity gate — the freshest sane tip. Equal to MaxObserved when the
	// velocity gate is inactive (no prior anchor or unknown block time) or
	// when it failed open. Prefer this over MaxObserved for the deliberate-lag
	// gauge: a garbage far-future tip (wrong-chain / misconfigured upstream)
	// is velocity-dropped and so cannot inflate the lag here.
	MaxEligible int64

	// Outliers lists the UpstreamIDs that survived the velocity gate but landed
	// OUTSIDE the dominant agreeing cluster (len == OutliersCount).
	Outliers []string
}

// ComputeServedTipCandidate clusters the inputs and returns the MIN block
// number of the dominant cluster — the most conservative tip that the
// dominant agreement cluster of upstreams can all serve.
//
// Algorithm (pure function; the caller wires the monotonic clamp on top):
//
//  1. Drop inputs with BlockNumber <= 0.
//  2. Velocity-gate: if lastServedBlock > 0, elapsedSinceLast > 0 (an anchor
//     with no known age cannot bound anything) and cfg.BlockTimeSeconds > 0,
//     drop inputs whose tip exceeds the expected max derived from
//     (lastServedBlock + elapsedBlocks × VelocitySlack + VelocityBufferBlocks).
//     An anchor older than StaleReanchorAfter disarms the gate for the pick
//     (ServedTipResult.StaleReanchor); if filtering would drop EVERY input
//     the gate FAILS OPEN (ServedTipResult.VelocityFailOpen). Either way the
//     pick proceeds ungated — clustering below still arbitrates outliers.
//  3. Sort surviving inputs ascending by block number.
//  4. Cluster greedily: adjacent values whose gap exceeds ClusterDelta split.
//  5. Pick the dominant cluster: max by (size desc, then min desc) — size
//     wins; ties broken in the chain-forward direction.
//  6. Return MIN of the dominant cluster as Candidate.
//
// The picker does NOT enforce monotonic forward progress — that is the
// caller's responsibility (typically via a CounterInt64SharedVariable
// TryUpdate at the Network level).
func ComputeServedTipCandidate(
	tips []ServedTipInput,
	lastServedBlock int64,
	elapsedSinceLast time.Duration,
	cfg ServedTipConfig,
) ServedTipResult {
	res := ServedTipResult{}

	// Resolve defaults
	clusterDelta := cfg.ClusterDelta
	if clusterDelta == 0 {
		clusterDelta = resolveAutoClusterDelta(cfg.BlockTimeSeconds)
	}
	velocitySlack := cfg.VelocitySlack
	if velocitySlack == 0 {
		velocitySlack = 2.0
	}
	velocityBuffer := cfg.VelocityBufferBlocks
	if velocityBuffer == 0 {
		velocityBuffer = 5
	}

	// 1. Filter zeros, compute MaxObserved across pre-velocity candidates.
	valid := make([]ServedTipInput, 0, len(tips))
	for _, t := range tips {
		if t.BlockNumber > 0 {
			valid = append(valid, t)
			if t.BlockNumber > res.MaxObserved {
				res.MaxObserved = t.BlockNumber
			}
		}
	}
	if len(valid) == 0 {
		return res
	}

	// 2. Velocity gate: only active when a prior anchor exists, the chain's
	//    block time is known, AND the anchor has a usable age
	//    (elapsedSinceLast > 0). Zero elapsed means the caller has never
	//    observed an advance in-process (e.g. a counter inherited from the
	//    persistent store at boot, or a lazily-materialized partition): the
	//    bound would collapse to lastServed+buffer — maximally strict exactly
	//    when the anchor is stalest — so leave all candidates in and let
	//    clustering decide.
	if lastServedBlock > 0 && elapsedSinceLast > 0 && cfg.BlockTimeSeconds > 0 {
		staleAfter := cfg.StaleReanchorAfter
		if staleAfter == 0 {
			staleAfter = resolveAutoStaleReanchorAfter(cfg.BlockTimeSeconds)
		}
		if elapsedSinceLast > staleAfter {
			// Anchor too old to predict an expected max (and with an
			// over-estimated block time the gate window expands SLOWER than
			// the chain, so it would exclude fresh tips forever). Run ungated;
			// clustering still arbitrates outliers.
			res.StaleReanchor = true
		} else {
			elapsedBlocks := elapsedSinceLast.Seconds() / cfg.BlockTimeSeconds
			expectedAdvance := int64(math.Ceil(elapsedBlocks * velocitySlack))
			expectedMax := lastServedBlock + expectedAdvance + velocityBuffer

			filtered := make([]ServedTipInput, 0, len(valid))
			for _, t := range valid {
				if t.BlockNumber > expectedMax {
					res.VelocityDropped = append(res.VelocityDropped, t.UpstreamID)
					continue
				}
				filtered = append(filtered, t)
			}
			if len(filtered) == 0 {
				// Fail open: a bound that rejects every live tip means the
				// anchor (lastServedBlock/elapsed) is wrong, not the world.
				// Dropping all inputs would yield Candidate 0 — "no new
				// information" — so the caller's monotonic counter would never
				// advance and every later pick would wedge the same way.
				// Re-run ungated and let clustering arbitrate outliers instead.
				res.VelocityFailOpen = true
				res.VelocityDropped = nil
			} else {
				valid = filtered
			}
		}
	}

	// 3. Sort ascending.
	sort.Slice(valid, func(i, j int) bool {
		return valid[i].BlockNumber < valid[j].BlockNumber
	})
	// Freshest velocity-surviving tip — the sane "highest available" reference
	// for the deliberate-lag gauge (excludes garbage tips the velocity gate
	// dropped above).
	res.MaxEligible = valid[len(valid)-1].BlockNumber

	// 4. Greedy clustering: split when the gap exceeds clusterDelta.
	clusters := [][]ServedTipInput{{valid[0]}}
	for i := 1; i < len(valid); i++ {
		last := clusters[len(clusters)-1]
		gap := valid[i].BlockNumber - last[len(last)-1].BlockNumber
		if gap > clusterDelta {
			clusters = append(clusters, []ServedTipInput{valid[i]})
		} else {
			clusters[len(clusters)-1] = append(last, valid[i])
		}
	}

	// 5. Dominant cluster: max by (size desc, then min desc).
	//    Since clusters are sorted ascending in input order AND each cluster's
	//    members are themselves sorted, cluster[i][0] is the cluster's MIN.
	dominantIdx := 0
	for i := 1; i < len(clusters); i++ {
		ci, cd := clusters[i], clusters[dominantIdx]
		if len(ci) > len(cd) {
			dominantIdx = i
		} else if len(ci) == len(cd) && ci[0].BlockNumber > cd[0].BlockNumber {
			dominantIdx = i
		}
	}

	dominant := clusters[dominantIdx]
	res.Candidate = dominant[0].BlockNumber
	res.ClusterCount = len(clusters)
	res.DominantSize = len(dominant)
	res.OutliersCount = len(valid) - len(dominant)
	// Attribute each non-dominant cluster member as an outlier for per-upstream
	// exclusion telemetry (these survived the velocity gate but disagreed with
	// the dominant cluster).
	for i, c := range clusters {
		if i == dominantIdx {
			continue
		}
		for _, t := range c {
			res.Outliers = append(res.Outliers, t.UpstreamID)
		}
	}
	return res
}

// MajorityServedTip is the CANDIDATE REPLACEMENT for the cluster+gate+counter
// pipeline, currently computed only as a shadow for prod evaluation: the
// highest block number that a strict MAJORITY of the inputs have already
// reached (the floor(N/2)-th highest head, 0-indexed descending).
//
// One order statistic gives every protection the pipeline above engineers
// separately, with zero state and zero configuration:
//   - a single garbage far-future tip cannot move it (needs a majority);
//   - a single stuck/lagging upstream cannot hold it back;
//   - per-upstream heads are already monotonic (poller counters), and an
//     order statistic over monotonic inputs only regresses when the eligible
//     SET changes — bounded by the live head spread;
//   - nothing is persisted, so no boot inheritance, no anchor, no wedge.
//
// Examples (heads descending): N=1 → that head; N=2 → the lower (conservative:
// never advertise a block only one upstream claims); N=3 → 2nd; N=5 → 3rd.
func MajorityServedTip(tips []ServedTipInput) int64 {
	heads := make([]int64, 0, len(tips))
	for _, t := range tips {
		if t.BlockNumber > 0 {
			heads = append(heads, t.BlockNumber)
		}
	}
	if len(heads) == 0 {
		return 0
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i] > heads[j] })
	return heads[len(heads)/2]
}

// ClampServedValue bounds the shared monotonic counter's value by live pick
// reality before it is served to clients, returning the value to serve and
// how far the counter sat AHEAD of the freshest live eligible tip (0 when
// not ahead).
//
// Floor: a counter below the live candidate means the store regressed or
// returned garbage — the freshly computed candidate is strictly better
// information. Ceiling: a counter further ahead of maxEligible than
// CounterAheadServeMargin is poisoned (rogue store write / garbage pick);
// advertising it guarantees "block not found" on every interpolated request,
// and the monotonic clamp would preserve the poison for up to the rollback
// tolerance — so clients receive maxEligible instead. The STORED counter is
// deliberately left alone; only the served value is clamped, so normal
// cross-pod monotonicity is unaffected outside the anomaly.
func ClampServedValue(counterValue, candidate, maxEligible int64, blockTimeSec float64) (served int64, counterAhead int64) {
	served = counterValue
	if candidate > served {
		served = candidate
	}
	if maxEligible > 0 {
		if ahead := served - maxEligible; ahead > 0 {
			counterAhead = ahead
			if margin := CounterAheadServeMargin(blockTimeSec); margin > 0 && ahead > margin {
				served = maxEligible
			}
		}
	}
	return served, counterAhead
}

// CounterAheadServeMargin returns how many blocks the shared served-tip
// counter may sit ahead of the freshest live eligible tip before the
// serve-time ceiling clamps what clients receive: ~10 seconds of chain
// progress, clamped to [8, 1024]. Routine cross-pod poller skew (another pod
// saw a block first) stays well inside the margin; a poisoned counter (rogue
// store write, garbage pick) lands far outside it. Returns 0 when the block
// time is unknown — without it skew cannot be judged, so the ceiling is
// disabled rather than risking routine clamping.
func CounterAheadServeMargin(blockTimeSec float64) int64 {
	if blockTimeSec <= 0 {
		return 0
	}
	m := int64(math.Ceil(10.0 / blockTimeSec))
	if m < 8 {
		return 8
	}
	if m > 1024 {
		return 1024
	}
	return m
}

// resolveAutoStaleReanchorAfter derives the stale-anchor cutoff from chain
// block time: 60 block intervals, clamped to [30s, 10m]. A healthy network
// advances its served tip every block or two; an anchor that has not moved
// for 60 intervals is either a halted chain (running ungated is then a no-op:
// every tip agrees with the anchor and nothing moves) or a wedged/rogue
// anchor (running ungated re-anchors to live reality on the next pick).
func resolveAutoStaleReanchorAfter(blockTimeSec float64) time.Duration {
	d := time.Duration(60 * blockTimeSec * float64(time.Second))
	if d < 30*time.Second {
		return 30 * time.Second
	}
	if d > 10*time.Minute {
		return 10 * time.Minute
	}
	return d
}

// resolveAutoClusterDelta derives ClusterDelta from chain block time:
//
//	delta = clamp(ceil(2.0 / blockTimeSec), 2, 10)
//
// The 2.0s numerator targets vendor-to-vendor propagation slack typical on
// EVM networks. Clamping keeps the result sane on slow chains (Mainnet 12s
// → floor 2) and fast chains (sub-100ms hypothetical → ceiling 10).
//
// When blockTimeSec is 0 (not yet observed), returns the floor of 2.
func resolveAutoClusterDelta(blockTimeSec float64) int64 {
	if blockTimeSec <= 0 {
		return 2
	}
	d := int64(math.Ceil(2.0 / blockTimeSec))
	if d < 2 {
		return 2
	}
	if d > 10 {
		return 10
	}
	return d
}
