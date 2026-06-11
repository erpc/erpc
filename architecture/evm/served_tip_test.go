package evm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// These tests are intentionally written BEFORE ComputeServedTipCandidate exists.
// They define the contract for the cluster-based served-tip picker:
//
//   1. Returns the MIN block number of the DOMINANT cluster of upstreams whose
//      tips are within ClusterDelta of each other.
//   2. Dominant cluster = max by (size desc, then min desc). Larger clusters
//      win; ties broken in the chain-forward direction.
//   3. A velocity gate drops single-upstream fantasy-future tips BEFORE
//      clustering, so a buggy upstream reporting tip+1000 can't pull the
//      cluster forward.
//   4. The picker does NOT enforce monotonicity itself — that's done by the
//      caller via the shared-state TryUpdate. The picker is a pure function.
//   5. Works at any upstream count (1, 2, 3, 5, 7, ...). No K/quorum knob.

// ----- helpers ----------------------------------------------------------------

func tipsFromInts(blocks ...int64) []ServedTipInput {
	out := make([]ServedTipInput, len(blocks))
	for i, b := range blocks {
		out[i] = ServedTipInput{UpstreamID: "u" + itoa(i), BlockNumber: b}
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+(i%10))) + s
		i /= 10
	}
	return s
}

// defaultCfg disables velocity gating (BlockTimeSeconds=0) so tests can focus
// on cluster semantics. Velocity-gate tests opt in explicitly.
func defaultCfg(clusterDelta int64) ServedTipConfig {
	return ServedTipConfig{ClusterDelta: clusterDelta}
}

// ----- variable upstream counts: healthy steady state -------------------------

func TestComputeServedTipCandidate_OneUpstream(t *testing.T) {
	res := ComputeServedTipCandidate(tipsFromInts(100), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(100), res.Candidate)
	assert.Equal(t, int64(100), res.MaxObserved)
	assert.Equal(t, 1, res.ClusterCount)
	assert.Equal(t, 1, res.DominantSize)
	assert.Equal(t, 0, res.OutliersCount)
}

func TestComputeServedTipCandidate_TwoUpstreams_SameTip(t *testing.T) {
	res := ComputeServedTipCandidate(tipsFromInts(100, 100), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(100), res.Candidate)
	assert.Equal(t, 1, res.ClusterCount)
	assert.Equal(t, 2, res.DominantSize)
}

func TestComputeServedTipCandidate_TwoUpstreams_OneBlockJitter(t *testing.T) {
	// Normal vendor-to-vendor jitter; both in one cluster, MIN is conservative
	// (servable by both).
	res := ComputeServedTipCandidate(tipsFromInts(100, 99), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(99), res.Candidate)
	assert.Equal(t, 1, res.ClusterCount)
	assert.Equal(t, 2, res.DominantSize)
}

func TestComputeServedTipCandidate_ManyUpstreams_AllAgree(t *testing.T) {
	res := ComputeServedTipCandidate(tipsFromInts(101, 100, 100, 100, 99), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(99), res.Candidate)
	assert.Equal(t, 1, res.ClusterCount)
	assert.Equal(t, 5, res.DominantSize)
}

// ----- lagger scenarios: the matrix from the design discussion ---------------

func TestComputeServedTipCandidate_ThreeUpstreams_OneLagger_ClusterMovesForward(t *testing.T) {
	// User's case: 1 of 3 falling behind. Cluster {100, 100} dominates; the
	// 95 outlier does NOT drag down.
	res := ComputeServedTipCandidate(tipsFromInts(100, 100, 95), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(100), res.Candidate, "lagging upstream must not drag the dominant cluster down")
	assert.Equal(t, 2, res.ClusterCount)
	assert.Equal(t, 2, res.DominantSize)
	assert.Equal(t, 1, res.OutliersCount)
}

func TestComputeServedTipCandidate_ThreeUpstreams_OneAhead_TwoLagging(t *testing.T) {
	// User's "1 leader, all rest lagging" — trust the lagging cluster.
	// Candidate is the laggers' MIN; the caller's monotonic clamp decides
	// whether to actually serve it.
	res := ComputeServedTipCandidate(tipsFromInts(100, 50, 49), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(49), res.Candidate, "trust the lagging cluster; do not believe the lone leader")
	assert.Equal(t, 2, res.ClusterCount)
	assert.Equal(t, 2, res.DominantSize)
	assert.Equal(t, 1, res.OutliersCount)
}

func TestComputeServedTipCandidate_FiveUpstreams_ThreeClose_TwoWayBehind(t *testing.T) {
	// User's "three close + one or two super lagging" case.
	res := ComputeServedTipCandidate(tipsFromInts(100, 99, 98, 50, 49), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(98), res.Candidate, "must return MIN of the close (dominant) cluster")
	assert.Equal(t, 2, res.ClusterCount)
	assert.Equal(t, 3, res.DominantSize)
}

func TestComputeServedTipCandidate_SevenUpstreams_FiveLeaders_TwoStragglers(t *testing.T) {
	res := ComputeServedTipCandidate(tipsFromInts(101, 101, 100, 100, 100, 95, 50), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(100), res.Candidate)
	assert.Equal(t, 3, res.ClusterCount)
	assert.Equal(t, 5, res.DominantSize)
}

func TestComputeServedTipCandidate_SevenUpstreams_FourLaggers_ThreeLeaders(t *testing.T) {
	// Larger lagging cluster wins on size; monotonic clamp upstream will
	// likely hold the previously-served tip instead.
	res := ComputeServedTipCandidate(tipsFromInts(101, 100, 100, 95, 94, 94, 93), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(93), res.Candidate, "larger lagging cluster wins; caller's monotonic clamp will hold")
	assert.Equal(t, 2, res.ClusterCount)
	assert.Equal(t, 4, res.DominantSize)
}

// ----- tiebreak: equal-size clusters -----------------------------------------

func TestComputeServedTipCandidate_TwoUpstreams_Split_TiebreakHigherMin(t *testing.T) {
	// 1 ahead, 1 way behind: both are size-1 clusters. Tiebreak by higher
	// MIN (chain-forward direction). Monotonic clamp upstream will refuse if
	// this would create an unjustified forward jump.
	res := ComputeServedTipCandidate(tipsFromInts(100, 50), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(100), res.Candidate)
	assert.Equal(t, 2, res.ClusterCount)
	assert.Equal(t, 1, res.DominantSize)
}

func TestComputeServedTipCandidate_EqualSizeClusters_TiebreakHigherMin(t *testing.T) {
	// 2v2 split: both clusters size 2. Higher MIN cluster wins.
	res := ComputeServedTipCandidate(tipsFromInts(100, 99, 50, 49), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(99), res.Candidate)
	assert.Equal(t, 2, res.ClusterCount)
	assert.Equal(t, 2, res.DominantSize)
}

// ----- edge cases ------------------------------------------------------------

func TestComputeServedTipCandidate_NoInputs(t *testing.T) {
	res := ComputeServedTipCandidate(nil, 0, 0, defaultCfg(2))
	assert.Equal(t, int64(0), res.Candidate)
	assert.Equal(t, 0, res.ClusterCount)
	assert.Equal(t, 0, res.DominantSize)
}

func TestComputeServedTipCandidate_AllZeroTips(t *testing.T) {
	// Cold-start upstreams that haven't polled yet should be filtered out.
	res := ComputeServedTipCandidate(tipsFromInts(0, 0, 0), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(0), res.Candidate, "zero tips are not valid candidates")
}

func TestComputeServedTipCandidate_MixedZeroAndReal(t *testing.T) {
	res := ComputeServedTipCandidate(tipsFromInts(0, 100, 100, 99, 0), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(99), res.Candidate, "zeros excluded; cluster over real tips only")
	assert.Equal(t, 3, res.DominantSize)
}

// ----- input ordering and duplication ----------------------------------------

func TestComputeServedTipCandidate_UnsortedInputs(t *testing.T) {
	// Same data as the 3-close + 2-behind case, but unsorted.
	res := ComputeServedTipCandidate(tipsFromInts(50, 100, 49, 99, 98), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(98), res.Candidate)
	assert.Equal(t, 3, res.DominantSize)
}

func TestComputeServedTipCandidate_DuplicateTips(t *testing.T) {
	res := ComputeServedTipCandidate(tipsFromInts(100, 100, 100, 100), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(100), res.Candidate)
	assert.Equal(t, 1, res.ClusterCount)
	assert.Equal(t, 4, res.DominantSize)
}

// ----- velocity gate: fantasy-future rejection -------------------------------

func TestComputeServedTipCandidate_VelocityGate_RejectsFantasyFutureTip(t *testing.T) {
	// Last served = 100, elapsed = 2s, blockTime = 2s.
	// Expected max = 100 + (2/2) × 2.0_slack + 5_buffer = 107.
	// Upstream at 9999 is fantasy-future → dropped. Upstreams at 101, 101, 100
	// dominate. Returned = MIN of dominant = 100.
	cfg := ServedTipConfig{
		ClusterDelta:         2,
		BlockTimeSeconds:     2.0,
		VelocitySlack:        2.0,
		VelocityBufferBlocks: 5,
	}
	res := ComputeServedTipCandidate(
		tipsFromInts(9999, 101, 101, 100),
		100,           // lastServedBlock
		2*time.Second, // elapsedSinceLast
		cfg,
	)
	assert.Equal(t, int64(100), res.Candidate, "velocity gate must drop the 9999 fantasy tip")
	assert.Len(t, res.VelocityDropped, 1, "exactly one upstream dropped by velocity gate")
}

func TestComputeServedTipCandidate_VelocityGate_AllowsLegitimateBurstCatchup(t *testing.T) {
	// Last served = 100, elapsed = 10s, blockTime = 2s.
	// Expected max = 100 + (10/2) × 2.0 + 5 = 115.
	// All three at 110-112 are legitimate burst catch-up — none dropped.
	cfg := ServedTipConfig{
		ClusterDelta:         2,
		BlockTimeSeconds:     2.0,
		VelocitySlack:        2.0,
		VelocityBufferBlocks: 5,
	}
	res := ComputeServedTipCandidate(
		tipsFromInts(112, 111, 110),
		100,
		10*time.Second,
		cfg,
	)
	assert.Equal(t, int64(110), res.Candidate)
	assert.Empty(t, res.VelocityDropped)
}

func TestComputeServedTipCandidate_MaxEligible_ExcludesVelocityDroppedGarbage(t *testing.T) {
	// A wrong-chain / misbehaving upstream reports a tip ~10k blocks ahead. It is
	// velocity-dropped, so MaxEligible (the deliberate-lag reference) reflects the
	// sane fleet — NOT the garbage. This is what keeps the served_tip_lag gauge from
	// exploding to hundreds of thousands of blocks when one endpoint goes rogue.
	cfg := ServedTipConfig{
		ClusterDelta:         2,
		BlockTimeSeconds:     2.0,
		VelocitySlack:        2.0,
		VelocityBufferBlocks: 5,
	}
	res := ComputeServedTipCandidate(
		tipsFromInts(9999, 101, 101, 100),
		100,
		2*time.Second,
		cfg,
	)
	assert.Equal(t, int64(9999), res.MaxObserved, "raw max still records the garbage tip")
	assert.Equal(t, int64(101), res.MaxEligible, "eligible max excludes the velocity-dropped garbage")
	assert.Len(t, res.VelocityDropped, 1)
	// Deliberate lag = MaxEligible - Candidate = 101 - 100 = 1 block (not 9899).
	assert.Equal(t, int64(1), res.MaxEligible-res.Candidate)
}

func TestComputeServedTipCandidate_Outliers_ListNonDominantMembers(t *testing.T) {
	// Two upstreams agree at 100/101 (dominant); one sits far ahead at 200 in its
	// own cluster → recorded as an outlier (it survived the velocity gate but
	// disagreed). With no velocity gate, MaxEligible == MaxObserved.
	res := ComputeServedTipCandidate(tipsFromInts(100, 101, 200), 0, 0, defaultCfg(2))
	assert.Equal(t, int64(100), res.Candidate)
	assert.Equal(t, 1, res.OutliersCount)
	assert.Equal(t, []string{"u2"}, res.Outliers, "the 200 tip (u2) is the outlier")
	assert.Equal(t, int64(200), res.MaxEligible)
	assert.Equal(t, int64(200), res.MaxObserved)
}

func TestComputeServedTipCandidate_VelocityGate_DisabledWithoutBlockTime(t *testing.T) {
	// With BlockTimeSeconds=0, the velocity gate is disabled (we can't
	// compute expected max). Cluster picker runs unmodified.
	res := ComputeServedTipCandidate(
		tipsFromInts(9999, 101, 101, 100),
		100,
		1*time.Hour,
		defaultCfg(2), // BlockTimeSeconds=0 disables gate
	)
	// 9999 alone, [100,101,101] is the dominant cluster of size 3.
	assert.Equal(t, int64(100), res.Candidate)
	assert.Empty(t, res.VelocityDropped, "velocity gate disabled without blockTime")
}

func TestComputeServedTipCandidate_VelocityGate_NoLastServed_DisablesGate(t *testing.T) {
	// Cold start (lastServedBlock=0). Velocity gate is disabled because we
	// have no anchor to compare against.
	cfg := ServedTipConfig{
		ClusterDelta:         2,
		BlockTimeSeconds:     2.0,
		VelocitySlack:        2.0,
		VelocityBufferBlocks: 5,
	}
	res := ComputeServedTipCandidate(
		tipsFromInts(9999, 101, 101, 100),
		0, // cold start
		0,
		cfg,
	)
	// Without an anchor, we can't reject 9999. It becomes its own cluster.
	// Cluster {100,101,101} dominates by size → 100.
	assert.Equal(t, int64(100), res.Candidate)
	assert.Empty(t, res.VelocityDropped)
}

func TestComputeServedTipCandidate_VelocityGate_FailsOpenWhenAllDropped(t *testing.T) {
	// The wedge scenario: a freshly-booted process inherits a stale persisted
	// counter (lastServed=100) with no in-process advance yet (elapsed=0), so
	// the bound collapses to 100+5=105 while every live tip is far past it.
	// Pre-fail-open this dropped ALL inputs → Candidate 0 → the monotonic
	// counter never advanced again (and elapsed stayed 0) → wedged forever.
	cfg := ServedTipConfig{
		ClusterDelta:         2,
		BlockTimeSeconds:     2.0,
		VelocitySlack:        2.0,
		VelocityBufferBlocks: 5,
	}
	res := ComputeServedTipCandidate(
		tipsFromInts(5000, 5001, 5001),
		100, // stale inherited anchor
		0,   // no in-process advance observed yet
		cfg,
	)
	assert.True(t, res.VelocityFailOpen, "gate must fail open when it would drop every input")
	assert.Empty(t, res.VelocityDropped, "nothing was actually dropped on fail-open")
	assert.Equal(t, int64(5000), res.Candidate, "ungated re-run picks the real cluster min")
	assert.Equal(t, int64(5001), res.MaxEligible, "eligible max is the unfiltered max on fail-open")
}

func TestComputeServedTipCandidate_VelocityGate_FailOpenStillClustersOutGarbage(t *testing.T) {
	// Fail-open must not hand the pick to a garbage tip: the ungated re-run
	// goes through the same clustering, so the agreeing majority still wins
	// and the far-future tip is attributed as an outlier.
	cfg := ServedTipConfig{
		ClusterDelta:         2,
		BlockTimeSeconds:     2.0,
		VelocitySlack:        2.0,
		VelocityBufferBlocks: 5,
	}
	res := ComputeServedTipCandidate(
		tipsFromInts(5000, 5001, 999999),
		100,
		2*time.Second,
		cfg,
	)
	assert.True(t, res.VelocityFailOpen)
	assert.Equal(t, int64(5000), res.Candidate, "dominant cluster wins despite fail-open")
	assert.Equal(t, []string{"u2"}, res.Outliers, "garbage tip is an outlier, not the pick")
}

func TestComputeServedTipCandidate_VelocityGate_PartialDropDoesNotFailOpen(t *testing.T) {
	// As long as at least one input survives the gate, behavior is unchanged:
	// the too-far-future tip is dropped and attributed, no fail-open.
	cfg := ServedTipConfig{
		ClusterDelta:         2,
		BlockTimeSeconds:     2.0,
		VelocitySlack:        2.0,
		VelocityBufferBlocks: 5,
	}
	res := ComputeServedTipCandidate(
		tipsFromInts(105, 106, 9999),
		100,
		2*time.Second, // expectedMax = 100 + ceil(1×2.0) + 5 = 107
		cfg,
	)
	assert.False(t, res.VelocityFailOpen)
	assert.Equal(t, []string{"u2"}, res.VelocityDropped)
	assert.Equal(t, int64(105), res.Candidate)
	assert.Equal(t, int64(106), res.MaxEligible)
}

// ----- cluster delta auto-derivation ----------------------------------------

func TestComputeServedTipCandidate_ClusterDelta_AutoDerivedFromBlockTime(t *testing.T) {
	// When ClusterDelta is 0 (unset), derive from BlockTimeSeconds:
	//   delta = clamp(ceil(2.0 / blockTimeSec), 2, 10)
	//
	// Verify on Arbitrum-like 0.25s blocks: delta = clamp(ceil(8), 2, 10) = 8.
	// With delta=8, a 7-block lag stays inside the cluster.
	cfg := ServedTipConfig{
		ClusterDelta:     0,
		BlockTimeSeconds: 0.25,
	}
	res := ComputeServedTipCandidate(tipsFromInts(100, 100, 93), 0, 0, cfg)
	assert.Equal(t, int64(93), res.Candidate,
		"on Arbitrum-like chains, 7-block lag is within normal jitter")
	assert.Equal(t, 1, res.ClusterCount)
	assert.Equal(t, 3, res.DominantSize)
}

func TestComputeServedTipCandidate_ClusterDelta_AutoMin2(t *testing.T) {
	// On slow chains (Mainnet 12s), ceil(2/12) = 1, but we clamp floor to 2.
	cfg := ServedTipConfig{
		ClusterDelta:     0,
		BlockTimeSeconds: 12.0,
	}
	res := ComputeServedTipCandidate(tipsFromInts(100, 99, 95), 0, 0, cfg)
	// delta should be 2. So 99-95=4 splits.
	assert.Equal(t, int64(99), res.Candidate)
	assert.Equal(t, 2, res.ClusterCount)
}

func TestComputeServedTipCandidate_ClusterDelta_AutoMax10(t *testing.T) {
	// On hypothetical chains with sub-100ms blocks, ceil(2 / 0.05) = 40, but
	// we clamp ceiling to 10. So delta = 10.
	cfg := ServedTipConfig{
		ClusterDelta:     0,
		BlockTimeSeconds: 0.05,
	}
	res := ComputeServedTipCandidate(tipsFromInts(100, 90, 89), 0, 0, cfg)
	// delta=10. Gap 100-90 = 10 (within delta if inclusive), 90-89=1.
	// All in one cluster, MIN = 89.
	assert.Equal(t, int64(89), res.Candidate)
	assert.Equal(t, 1, res.ClusterCount)
}

func TestComputeServedTipCandidate_ClusterDelta_ExplicitOverride(t *testing.T) {
	// Explicit ClusterDelta wins over auto-derivation.
	cfg := ServedTipConfig{
		ClusterDelta:     5, // explicit
		BlockTimeSeconds: 0.25,
	}
	// With delta=5, gap 100-93=7 splits; gap 100-95=5 doesn't (inclusive).
	res := ComputeServedTipCandidate(tipsFromInts(100, 100, 93), 0, 0, cfg)
	assert.Equal(t, int64(100), res.Candidate,
		"explicit delta=5 overrides; 7-block gap excludes the lagger")
	assert.Equal(t, 2, res.ClusterCount)
}
