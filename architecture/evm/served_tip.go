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
	// elapsed time).
	VelocityDropped []string
}

// ComputeServedTipCandidate clusters the inputs and returns the MIN block
// number of the dominant cluster — the most conservative tip that the
// dominant agreement cluster of upstreams can all serve.
//
// Algorithm (pure function; the caller wires the monotonic clamp on top):
//
//  1. Drop inputs with BlockNumber <= 0.
//  2. Velocity-gate: if lastServedBlock > 0 and cfg.BlockTimeSeconds > 0,
//     drop inputs whose tip exceeds the expected max derived from
//     (lastServedBlock + elapsedBlocks × VelocitySlack + VelocityBufferBlocks).
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

	// 2. Velocity gate: only active when BOTH a prior anchor exists AND the
	//    chain's block time is known. Without either, we can't predict an
	//    expected max — leave all candidates in and let clustering do the work.
	if lastServedBlock > 0 && cfg.BlockTimeSeconds > 0 {
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
		valid = filtered
		if len(valid) == 0 {
			return res
		}
	}

	// 3. Sort ascending.
	sort.Slice(valid, func(i, j int) bool {
		return valid[i].BlockNumber < valid[j].BlockNumber
	})

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
	return res
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
