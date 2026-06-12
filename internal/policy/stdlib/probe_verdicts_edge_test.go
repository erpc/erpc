package stdlib_test

// Adversarial edges for the probe verdict matrix, mapped to the review
// assumptions: (1) reason of exclusion must drive probing; (2) the
// exclusion site decides; (3) a vendor excluded for BAD PERF (not tag)
// must still be probed; (4) stacked reasons must sequence correctly as
// they heal; (5) the rich default policy must keep sane probe behavior.

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/stretchr/testify/require"
)

// (3) Aram's literal case: a vendor whose ONLY exclusion is bad perf
// (latency), with a tag step present in the chain that does NOT match it
// — it must be probed.
func TestProbeVerdictsEdge_PerfExcludedVendorIsProbed(t *testing.T) {
	eval := `(upstreams) => upstreams
		.excludeTag('tier:other')
		.excludeIf(all(samplesAbove(10), latencyAbove(1000)))`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	for i := 0; i < 30; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[2], "*", 3*time.Second, true, "none", common.DataFinalityStateUnknown, "n/a")
	}
	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["tagged"], "slow vendor excluded for perf")
	require.True(t, probeable["tagged"], "perf-excluded vendor MUST be probed — that's its only way back")
}

// (4) Stacked reasons sequence correctly: lagging (probe:false) AND
// erroring (probe-eligible) → blocked while lag persists (probing can't
// fix lag; poller refreshes it). When lag heals, the error exclusion
// remains and probing must switch ON so the upstream can recover.
func TestProbeVerdictsEdge_StackedReasonsSequenceAcrossTicks(t *testing.T) {
	eval := `(upstreams) => upstreams
		.excludeIf(blockNumberLagAbove(16), { probe: false })
		.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	failHard(tracker, ups[0])
	setLag(tracker, ups[0], 50)

	probeable, _ := probeSet(engine)
	require.False(t, probeable["plain1"],
		"tick 1: lag (blocking) + errors (eligible) -> no probe; probing cannot fix lag")

	setLag(tracker, ups[0], 0) // lag heals via poller; errors persist
	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["plain1"], "still excluded by error gate")
	require.True(t, probeable["plain1"],
		"tick 2: only the traffic-starved reason remains -> probing must switch ON")
}

// (2) Two tag steps, conflicting flags, upstream matches both: blocking
// wins (matrix rule: any blocking verdict suppresses).
func TestProbeVerdictsEdge_ConflictingTagStepsBlockingWins(t *testing.T) {
	eval := `(upstreams) => upstreams
		.excludeTag('tier:static')
		.excludeTag('v3tag', { probe: true })`
	engine, ups, _, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	if f, ok := ups[2].(*fakeUpstream); ok {
		f.tags = []string{"tier:static", "v3tag"}
	}
	probeable, _ := probeSet(engine)
	require.False(t, probeable["tagged"],
		"a blocking verdict from one tag step suppresses probing even when another tag step opts in")
}

// Guard semantics: an erroring upstream below the samplesAbove(10) gate is
// NOT excluded at all — no verdict, no probe candidacy, still serving.
func TestProbeVerdictsEdge_BelowSampleGateNotExcluded(t *testing.T) {
	eval := `(upstreams) => upstreams.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	for i := 0; i < 5; i++ { // only 5 samples — below the gate
		tracker.RecordUpstreamRequest(ups[0], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(ups[0], "*", common.DataFinalityStateUnknown, errFailSynth)
	}
	probeable, excludedAll := probeSet(engine)
	require.False(t, excludedAll["plain1"], "below the sample gate the upstream keeps serving")
	require.False(t, probeable["plain1"])
}

// (5) The rich DEFAULT policy end-to-end: cordoned upstream not probed,
// health-excluded upstream probed, healthy upstream serving. Pins that
// the default chain's removeCordoned/excludeIf verdicts behave under the
// production policy source, not just minimal evals.
func TestProbeVerdictsEdge_DefaultPolicyMixedPool(t *testing.T) {
	engine, ups, tracker, cancel := mkVerdictEngine(t, common.DefaultSelectionPolicySource)
	defer cancel()
	defer engine.Stop()

	tracker.Cordon(ups[1], "*", "ops cordon") // plain2 cordoned
	failHard(tracker, ups[0])                 // plain1 erroring
	// give the healthy one samples so deviation predicates have peers
	for i := 0; i < 30; i++ {
		tracker.RecordUpstreamRequest(ups[2], "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamDuration(ups[2], "*", 20*time.Millisecond, true, "none", common.DataFinalityStateUnknown, "n/a")
	}

	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["plain1"] && excludedAll["plain2"], "both degraded upstreams excluded")
	require.True(t, probeable["plain1"], "default policy: health-excluded upstream is probed")
	require.False(t, probeable["plain2"], "default policy: cordoned upstream is NOT probed")
	require.False(t, probeable["tagged"], "healthy upstream serves; not a probe candidate")
}

// Per-method eval scope: verdicts are computed per slot; a wildcard-tag
// exclusion must block probing in a narrow method slot too.
func TestProbeVerdictsEdge_PerMethodScope(t *testing.T) {
	eval := `(upstreams) => upstreams.excludeTag('tier:static')`
	ctxEngine, _, _, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer ctxEngine.Stop()

	ctxEngine.GetOrdered("evm:1", "eth_call", "*") // materialize narrow slot
	policy.TickForTest(ctxEngine, "evm:1", "eth_call")
	probeable := map[string]bool{}
	for _, u := range ctxEngine.GetExcluded("evm:1", "eth_call", "*") {
		probeable[u.Id()] = true
	}
	require.False(t, probeable["tagged"], "tag verdict blocks probing in per-method slots too")
}
