package evm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The served-tip contract: PickServedTip returns the freshest block a strict
// MAJORITY of inputs have reached. These tests pin the order-statistic
// semantics and the two protections that motivated the design — one rogue
// far-future tip cannot move the pick, one stuck upstream cannot hold it back.

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

func TestPickServedTip_MajorityIndexAcrossN(t *testing.T) {
	// N=1: the only head.
	assert.Equal(t, int64(100), PickServedTip(tipsFromInts(100)).Tip)
	// N=2: the LOWER — never advertise a block only one upstream claims.
	assert.Equal(t, int64(100), PickServedTip(tipsFromInts(200, 100)).Tip)
	// N=3: 2nd highest (2 of 3 have it).
	assert.Equal(t, int64(101), PickServedTip(tipsFromInts(102, 101, 100)).Tip)
	// N=4: 3rd highest (3 of 4 have it).
	assert.Equal(t, int64(101), PickServedTip(tipsFromInts(103, 102, 101, 100)).Tip)
	// N=5: 3rd highest (3 of 5 have it).
	assert.Equal(t, int64(102), PickServedTip(tipsFromInts(104, 103, 102, 101, 100)).Tip)
	// N=7: 4th highest (4 of 7 have it).
	assert.Equal(t, int64(103), PickServedTip(tipsFromInts(106, 105, 104, 103, 102, 101, 100)).Tip)
}

func TestPickServedTip_GarbageTipCannotMoveThePick(t *testing.T) {
	// A rogue upstream reporting a fantasy-future block (wrong chain,
	// misconfigured endpoint) is just one voice — the majority ignores it.
	// This is the abstract/zora prod scenario that used to inflate lag gauges
	// and (pre-2026-06 fix) could poison the persistent counter.
	p := PickServedTip(tipsFromInts(999_999_999, 101, 100))
	assert.Equal(t, int64(101), p.Tip)
	assert.Equal(t, int64(101), p.Freshest,
		"the lag reference (corroborated freshest) must ignore the lone rogue too")

	// Even two agreeing rogues lose against a 5-upstream majority.
	assert.Equal(t, int64(102), PickServedTip(tipsFromInts(999_999_999, 999_999_999, 102, 101, 100)).Tip)

	// N=2 with one rogue: the SANE (lower) head wins — the old cluster
	// tie-break picked the garbage here.
	assert.Equal(t, int64(100), PickServedTip(tipsFromInts(999_999_999, 100)).Tip)
}

func TestPickServedTip_StuckUpstreamCannotHoldThePickBack(t *testing.T) {
	// One frozen/lagging upstream cannot pin the advertised tip while the
	// majority advances — the inverse of the garbage case.
	assert.Equal(t, int64(200), PickServedTip(tipsFromInts(5, 201, 200)).Tip)
	assert.Equal(t, int64(201), PickServedTip(tipsFromInts(5, 202, 201, 200, 201)).Tip)
}

func TestPickServedTip_AllAgreeingIsIdentity(t *testing.T) {
	p := PickServedTip(tipsFromInts(100, 100, 100))
	assert.Equal(t, int64(100), p.Tip)
	assert.Equal(t, int64(100), p.Freshest)
	assert.Equal(t, 3, p.Inputs)
}

func TestPickServedTip_ZeroAndEmptyInputs(t *testing.T) {
	// Zero/negative heads are "no data yet" and filtered.
	assert.Equal(t, int64(100), PickServedTip(tipsFromInts(0, 100, 0)).Tip)
	assert.Equal(t, 1, PickServedTip(tipsFromInts(0, 100, 0)).Inputs)
	assert.Equal(t, int64(0), PickServedTip(tipsFromInts(0, 0)).Tip)
	assert.Equal(t, int64(0), PickServedTip(nil).Tip)
}

func TestPickServedTip_TipNeverExceedsFreshestAndIsAlwaysAHead(t *testing.T) {
	// Structural properties consumers rely on: the tip is one of the live
	// heads (never an invented number) and never ahead of the freshest view.
	cases := [][]int64{
		{100}, {100, 200}, {1, 2, 3}, {7, 7, 9, 9},
		{5, 100, 101, 102, 999999},
	}
	for _, heads := range cases {
		p := PickServedTip(tipsFromInts(heads...))
		assert.LessOrEqual(t, p.Tip, p.Freshest, "heads=%v", heads)
		assert.Contains(t, heads, p.Tip, "tip must be a real observed head; heads=%v", heads)
	}
}

// ─── Scenarios the retired cluster+gate+counter pipeline existed for ─────────
// Each case below is a REAL-WORLD situation the old machinery handled with a
// dedicated mechanism (greedy clustering, ClusterDelta, the velocity gate,
// fail-open, MaxEligible, the persistent monotonic counter). The majority pick
// must keep handling every one of them — these tests are the proof, mapped
// one-to-one from the old test matrix and the 2026-06 incident history.

func TestPickServedTip_Scenario_VendorPropagationJitter(t *testing.T) {
	// Old: ClusterDelta grouped heads within 1-2 blocks so vendor propagation
	// jitter never split agreement. New: the majority head IS inside the
	// jitter band — no grouping parameter needed.
	assert.Equal(t, int64(100), PickServedTip(tipsFromInts(101, 100, 100)).Tip)
	assert.Equal(t, int64(101), PickServedTip(tipsFromInts(101, 101, 100)).Tip,
		"two fresh vs one 1-block lagger: tip moves forward")
}

func TestPickServedTip_Scenario_SingleLaggerDoesNotHoldBack(t *testing.T) {
	// Old: the dominant (fresh) cluster outvoted a stuck/lagging upstream.
	assert.Equal(t, int64(101), PickServedTip(tipsFromInts(101, 101, 50)).Tip)
}

func TestPickServedTip_Scenario_SingleLeaderDoesNotDefineTip(t *testing.T) {
	// Old: a lone most-ahead node (flashblocks-style) formed a 1-node cluster
	// and lost to the agreeing pair. Advertising the loner's head is exactly
	// the "block not found" churn that motivated served-tip in PR #900.
	assert.Equal(t, int64(100), PickServedTip(tipsFromInts(120, 100, 99)).Tip,
		"the loner's head must never be advertised")
}

func TestPickServedTip_Scenario_MajorityLaggersWin(t *testing.T) {
	// Old: 4 laggers outvoted 3 leaders by cluster size — the network truth
	// is what most upstreams can serve. New: same outcome with a fresher
	// representative (the BEST lagger instead of the worst).
	assert.Equal(t, int64(50), PickServedTip(tipsFromInts(103, 102, 101, 50, 49, 48, 47)).Tip)
}

func TestPickServedTip_Scenario_BurstCatchupServedImmediately(t *testing.T) {
	// Old: the velocity gate carried slack+buffer tuned to ALLOW legitimate
	// burst catch-up (L2 sequencer batches, a halted chain resuming), plus
	// fail-open for when that tuning was wrong — mis-tuning is what froze
	// prod. New: stateless, so a jump is served the moment a majority
	// reports it; there is no window to outrun and nothing to mis-arm.
	assert.Equal(t, int64(100), PickServedTip(tipsFromInts(100, 100, 99)).Tip)
	assert.Equal(t, int64(1100), PickServedTip(tipsFromInts(1100, 1100, 1099)).Tip)
}

func TestPickServedTip_Scenario_GarbageCannotInflateLagReference(t *testing.T) {
	// Old: MaxEligible (the velocity-gated max) kept a rogue far-future tip
	// out of the lag gauge — without that, dashboards read "1.8 days behind"
	// on healthy chains (the abstract/zora incident). New: Freshest is the
	// 2nd-highest head, so a single rogue cannot touch the gauge either.
	p := PickServedTip(tipsFromInts(999_999_999, 102, 101, 100))
	assert.Equal(t, int64(101), p.Tip)
	assert.Equal(t, int64(102), p.Freshest, "lag reference ignores the lone rogue")
	assert.Equal(t, int64(1), p.Freshest-p.Tip, "deliberate lag stays in single digits")
}

func TestPickServedTip_Scenario_HaltedChainHoldsHonestly(t *testing.T) {
	// Old: a halted chain froze the counter (and post-incident, tripped the
	// stuck watchdog). New: picks over frozen heads keep returning the same
	// honest consensus value — no invented progress; the advance-age
	// watchdog still fires at the Network layer.
	frozen := tipsFromInts(500, 500, 499)
	assert.Equal(t, int64(500), PickServedTip(frozen).Tip)
	assert.Equal(t, PickServedTip(frozen), PickServedTip(frozen), "pure function: same inputs, same pick")
}
