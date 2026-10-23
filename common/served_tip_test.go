package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The served-tip contract: PickServedTip returns the freshest block at least
// quorum inputs have reached. These tests pin the order-statistic semantics
// for the default quorum (2) and "majority" with the pair rule off
// (pairLead 0), the two protections that motivated the design — one rogue
// far-future tip cannot move the pick, one stuck upstream cannot hold it
// back — and the pair rule itself.

func TestPickServedTip_MajorityIndexAcrossN(t *testing.T) {
	// N=1: the only head.
	assert.Equal(t, int64(100), PickServedTip([]int64{100}, ServedTipQuorumMajority, 0).Tip)
	// N=2 with the pair rule off: the LOWER — never advertise a block only
	// one upstream claims.
	assert.Equal(t, int64(100), PickServedTip([]int64{200, 100}, ServedTipQuorumMajority, 0).Tip)
	// N=3: 2nd highest (2 of 3 have it).
	assert.Equal(t, int64(101), PickServedTip([]int64{102, 101, 100}, ServedTipQuorumMajority, 0).Tip)
	// N=4: 3rd highest (3 of 4 have it).
	assert.Equal(t, int64(101), PickServedTip([]int64{103, 102, 101, 100}, ServedTipQuorumMajority, 0).Tip)
	// N=5: 3rd highest (3 of 5 have it).
	assert.Equal(t, int64(102), PickServedTip([]int64{104, 103, 102, 101, 100}, ServedTipQuorumMajority, 0).Tip)
	// N=7: 4th highest (4 of 7 have it).
	assert.Equal(t, int64(103), PickServedTip([]int64{106, 105, 104, 103, 102, 101, 100}, ServedTipQuorumMajority, 0).Tip)
}

func TestPickServedTip_DefaultQuorumIsSecondHighest(t *testing.T) {
	var q ServedTipQuorum // zero value = DefaultServedTipQuorum
	assert.Equal(t, int64(100), PickServedTip([]int64{100}, q, 0).Tip)
	assert.Equal(t, int64(100), PickServedTip([]int64{200, 100}, q, 0).Tip)
	assert.Equal(t, int64(102), PickServedTip([]int64{103, 102, 101, 100, 99}, q, 0).Tip,
		"N=5: 2nd highest, where majority would serve the 3rd")
	assert.Equal(t, int64(101), PickServedTip([]int64{999_999_999, 101, 100}, q, 0).Tip,
		"one rogue still cannot move the pick")
	assert.Equal(t, int64(100), PickServedTip([]int64{103, 102, 101, 100}, ServedTipQuorum(4), 0).Tip)
	assert.Equal(t, int64(101), PickServedTip([]int64{101, 100}, ServedTipQuorum(1), 0).Tip)
}

// erpc#1253: two voters cannot corroborate anything, so with exactly two heads
// within pairLead of each other the higher one is served — a node a few blocks
// behind must not pin every reader of "latest" at its height until it is
// benched. A wider gap is another chain or a garbage head: the lower still wins.
func TestPickServedTip_PairRule(t *testing.T) {
	const lead = int64(1024)
	for _, q := range []ServedTipQuorum{0, ServedTipQuorumMajority, 2, 3} {
		p := PickServedTip([]int64{100, 103}, q, lead)
		assert.Equal(t, int64(103), p.Tip, "q=%d: the higher of a plausible pair is served", q)
		assert.Equal(t, int64(103), p.Freshest, "q=%d: the served head is the freshest view, so the lag gauge reads 0", q)
		assert.Equal(t, int64(103), p.Max, "q=%d", q)
		assert.Equal(t, 2, p.Inputs, "q=%d", q)
	}
	assert.Equal(t, int64(1100), PickServedTip([]int64{76, 1100}, 0, lead).Tip, "a gap of exactly pairLead is plausible")
	assert.Equal(t, int64(75), PickServedTip([]int64{1100, 75}, 0, lead).Tip, "one block past pairLead is not a lead: the lower wins")
	assert.Equal(t, int64(100), PickServedTip([]int64{999_999_999, 100}, 0, lead).Tip, "N=2 with one rogue: the sane head still wins")
	assert.Equal(t, int64(100), PickServedTip([]int64{100, 103}, 0, 0).Tip, "pairLead 0 turns the rule off")
	assert.Equal(t, int64(100), PickServedTip([]int64{100, 100}, 0, lead).Tip, "an agreeing pair is unchanged")

	// The rule is a pair rule: N=1 and N>=3 are the plain order statistic.
	assert.Equal(t, int64(100), PickServedTip([]int64{100}, 0, lead).Tip)
	assert.Equal(t, int64(101), PickServedTip([]int64{103, 101, 100}, 0, lead).Tip, "N=3: 2nd highest, the lone leader is not served")
	assert.Equal(t, int64(100), PickServedTip([]int64{0, 103, 100, 0}, 0, lead).Freshest-3, "zero heads do not count toward the pair")
	assert.Equal(t, int64(103), PickServedTip([]int64{0, 103, 100, 0}, 0, lead).Tip)

	// quorum 1 already serves the higher head; the rule changes nothing.
	assert.Equal(t, int64(103), PickServedTip([]int64{100, 103}, ServedTipQuorum(1), lead).Tip)
}

func TestPickServedTip_GarbageTipCannotMoveThePick(t *testing.T) {
	// A rogue upstream reporting a fantasy-future block (wrong chain,
	// misconfigured endpoint) is just one voice — the majority ignores it.
	// This is the wrong-chain endpoint shape that used to inflate lag gauges
	// and, under the earlier persistent-counter design, could poison the counter.
	p := PickServedTip([]int64{999_999_999, 101, 100}, ServedTipQuorumMajority, 0)
	assert.Equal(t, int64(101), p.Tip)
	assert.Equal(t, int64(101), p.Freshest,
		"the lag reference (corroborated freshest) must ignore the lone rogue too")

	// Even two agreeing rogues lose against a 5-upstream majority.
	assert.Equal(t, int64(102), PickServedTip([]int64{999_999_999, 999_999_999, 102, 101, 100}, ServedTipQuorumMajority, 0).Tip)

	// N=2 with one rogue: the SANE (lower) head wins — the old cluster
	// tie-break picked the garbage here.
	assert.Equal(t, int64(100), PickServedTip([]int64{999_999_999, 100}, ServedTipQuorumMajority, 0).Tip)
}

func TestPickServedTip_StuckUpstreamCannotHoldThePickBack(t *testing.T) {
	// One frozen/lagging upstream cannot pin the advertised tip while the
	// majority advances — the inverse of the garbage case.
	assert.Equal(t, int64(200), PickServedTip([]int64{5, 201, 200}, ServedTipQuorumMajority, 0).Tip)
	assert.Equal(t, int64(201), PickServedTip([]int64{5, 202, 201, 200, 201}, ServedTipQuorumMajority, 0).Tip)
}

func TestPickServedTip_AllAgreeingIsIdentity(t *testing.T) {
	p := PickServedTip([]int64{100, 100, 100}, ServedTipQuorumMajority, 0)
	assert.Equal(t, int64(100), p.Tip)
	assert.Equal(t, int64(100), p.Freshest)
	assert.Equal(t, 3, p.Inputs)
}

func TestPickServedTip_ZeroAndEmptyInputs(t *testing.T) {
	// Zero/negative heads are "no data yet" and filtered.
	assert.Equal(t, int64(100), PickServedTip([]int64{0, 100, 0}, ServedTipQuorumMajority, 0).Tip)
	assert.Equal(t, 1, PickServedTip([]int64{0, 100, 0}, ServedTipQuorumMajority, 0).Inputs)
	assert.Equal(t, int64(0), PickServedTip([]int64{0, 0}, ServedTipQuorumMajority, 0).Tip)
	assert.Equal(t, int64(0), PickServedTip(nil, ServedTipQuorumMajority, 0).Tip)
}

func TestPickServedTip_TipNeverExceedsFreshestAndIsAlwaysAHead(t *testing.T) {
	// Structural properties consumers rely on: the tip is one of the live
	// heads (never an invented number) and never ahead of the freshest view.
	cases := [][]int64{
		{100}, {100, 200}, {1, 2, 3}, {7, 7, 9, 9},
		{5, 100, 101, 102, 999999},
	}
	for _, heads := range cases {
		p := PickServedTip(heads, ServedTipQuorumMajority, 0)
		assert.LessOrEqual(t, p.Tip, p.Freshest, "heads=%v", heads)
		assert.Contains(t, heads, p.Tip, "tip must be a real observed head; heads=%v", heads)
	}
}
func TestPickServedTip_Scenario_VendorPropagationJitter(t *testing.T) {
	// The quorum head sits inside the vendor propagation jitter band, so no
	// grouping parameter is needed.
	assert.Equal(t, int64(100), PickServedTip([]int64{101, 100, 100}, ServedTipQuorumMajority, 0).Tip)
	assert.Equal(t, int64(101), PickServedTip([]int64{101, 101, 100}, ServedTipQuorumMajority, 0).Tip,
		"two fresh vs one 1-block lagger: tip moves forward")
}

func TestPickServedTip_Scenario_SingleLaggerDoesNotHoldBack(t *testing.T) {
	// Old: the dominant (fresh) cluster outvoted a stuck/lagging upstream.
	assert.Equal(t, int64(101), PickServedTip([]int64{101, 101, 50}, ServedTipQuorumMajority, 0).Tip)
}

func TestPickServedTip_Scenario_SingleLeaderDoesNotDefineTip(t *testing.T) {
	// Old: a lone most-ahead node (flashblocks-style) formed a 1-node cluster
	// and lost to the agreeing pair. Advertising the loner's head is exactly
	// the "block not found" churn that motivated served-tip in PR #900.
	assert.Equal(t, int64(100), PickServedTip([]int64{120, 100, 99}, ServedTipQuorumMajority, 0).Tip,
		"the loner's head must never be advertised")
}

func TestPickServedTip_Scenario_MajorityLaggersWin(t *testing.T) {
	// Old: 4 laggers outvoted 3 leaders by cluster size — the network truth
	// is what most upstreams can serve. New: same outcome with a fresher
	// representative (the BEST lagger instead of the worst).
	assert.Equal(t, int64(50), PickServedTip([]int64{103, 102, 101, 50, 49, 48, 47}, ServedTipQuorumMajority, 0).Tip)
}

func TestPickServedTip_Scenario_BurstCatchupServedImmediately(t *testing.T) {
	// Old: the velocity gate carried slack+buffer tuned to ALLOW legitimate
	// burst catch-up (L2 sequencer batches, a halted chain resuming), plus
	// fail-open for when that tuning was wrong — mis-tuning froze the
	// tip. New: stateless, so a jump is served the moment a majority
	// reports it; there is no window to outrun and nothing to mis-arm.
	assert.Equal(t, int64(100), PickServedTip([]int64{100, 100, 99}, ServedTipQuorumMajority, 0).Tip)
	assert.Equal(t, int64(1100), PickServedTip([]int64{1100, 1100, 1099}, ServedTipQuorumMajority, 0).Tip)
}

func TestPickServedTip_Scenario_GarbageCannotInflateLagReference(t *testing.T) {
	// Old: MaxEligible (the velocity-gated max) kept a rogue far-future tip
	// out of the lag gauge — without that, dashboards read days behind
	// on healthy chains (a wrong-chain endpoint). New: Freshest is the
	// 2nd-highest head, so a single rogue cannot touch the gauge either.
	p := PickServedTip([]int64{999_999_999, 102, 101, 100}, ServedTipQuorumMajority, 0)
	assert.Equal(t, int64(101), p.Tip)
	assert.Equal(t, int64(102), p.Freshest, "lag reference ignores the lone rogue")
	assert.Equal(t, int64(1), p.Freshest-p.Tip, "deliberate lag stays in single digits")
}

func TestPickServedTip_Scenario_HaltedChainHoldsHonestly(t *testing.T) {
	// Old: a halted chain froze the counter (and, later, tripped the
	// stuck watchdog). New: picks over frozen heads keep returning the same
	// honest consensus value — no invented progress; the advance-age
	// watchdog still fires at the Network layer.
	frozen := []int64{500, 500, 499}
	assert.Equal(t, int64(500), PickServedTip(frozen, ServedTipQuorumMajority, 0).Tip)
	assert.Equal(t, PickServedTip(frozen, ServedTipQuorumMajority, 0), PickServedTip(frozen, ServedTipQuorumMajority, 0), "pure function: same inputs, same pick")
}
