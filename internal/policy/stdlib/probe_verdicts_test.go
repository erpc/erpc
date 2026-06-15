package stdlib_test

// Probe verdict matrix: exclude-family steps judge probe eligibility for
// every upstream they WOULD drop — including upstreams already removed by
// earlier steps — and an excluded upstream is shadow-probed iff at least
// one probe-eligible step excludes it and no probe-blocking step does.
// These tests pin the matrix semantics, the per-step defaults and
// overrides, order-insensitivity, and the untracked-step fallback.

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// mkVerdictEngine builds a frozen-tick engine over `eval` with two plain
// upstreams and one tagged `tier:static`. Returns engine + ups + tracker.
func mkVerdictEngine(t *testing.T, eval string) (*policy.Engine, []common.Upstream, *health.Tracker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "p1", time.Minute)
	engine := policy.NewEngine(ctx, &logger, "p1", tracker, stdlib.Install, nil)

	ups := mkUpsWithTags([]struct {
		id     string
		vendor string
		tags   []string
	}{
		{id: "plain1", vendor: "v1", tags: nil},
		{id: "plain2", vendor: "v2", tags: nil},
		{id: "tagged", vendor: "v3", tags: []string{"tier:static"}},
	})

	cfg := &common.SelectionPolicyConfig{
		EvalInterval: 0,
		EvalTimeout:  common.Duration(100 * time.Millisecond),
		EvalFunc:     eval,
	}
	require.NoError(t, cfg.SetDefaults())
	require.NoError(t, engine.RegisterNetwork("evm:1", "", func() []common.Upstream { return ups }, cfg))
	return engine, ups, tracker, cancel
}

// failHard drives an upstream's error rate above 0.7 with >10 samples.
func failHard(tracker *health.Tracker, u common.Upstream) {
	for i := 0; i < 30; i++ {
		tracker.RecordUpstreamRequest(u, "*", common.DataFinalityStateUnknown)
		tracker.RecordUpstreamFailure(u, "*", common.DataFinalityStateUnknown, errFailSynth)
	}
}

var errFailSynth = errSynth{}

type errSynth struct{}

func (errSynth) Error() string { return "synth" }

// probeSet ticks and returns the prober-visible excluded set (GetExcluded)
// and the full decision excluded set with verdicts.
func probeSet(engine *policy.Engine) (probeable map[string]bool, excludedAll map[string]bool) {
	policy.TickForTest(engine, "evm:1", "*")
	probeable = map[string]bool{}
	for _, u := range engine.GetExcluded("evm:1", "*", "*") {
		probeable[u.Id()] = true
	}
	excludedAll = map[string]bool{}
	_, exIDs := policy.LatestDecisionOutputForTest(engine, "evm:1", "*")
	for _, id := range exIDs {
		excludedAll[id] = true
	}
	return probeable, excludedAll
}

// ─── per-step defaults ───────────────────────────────────────────────────

// excludeTag default: static exclusion is probe-blocking.
func TestProbeVerdicts_ExcludeTagDefaultNoProbe(t *testing.T) {
	engine, _, _, cancel := mkVerdictEngine(t, `(upstreams) => upstreams.excludeTag('tier:static')`)
	defer cancel()
	defer engine.Stop()

	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["tagged"], "tag-excluded upstream is in the decision excluded set")
	require.False(t, probeable["tagged"], "tag-excluded upstream must NOT be a probe candidate")
}

// excludeTag override: { probe: true } opts back into probing.
func TestProbeVerdicts_ExcludeTagProbeOverride(t *testing.T) {
	engine, _, _, cancel := mkVerdictEngine(t, `(upstreams) => upstreams.excludeTag('tier:static', { probe: true })`)
	defer cancel()
	defer engine.Stop()

	probeable, _ := probeSet(engine)
	require.True(t, probeable["tagged"], "explicit probe:true must re-enable probing for tag exclusions")
}

// excludeIf default: conditional exclusion is probe-eligible.
func TestProbeVerdicts_ExcludeIfDefaultProbes(t *testing.T) {
	eval := `(upstreams) => upstreams.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	failHard(tracker, ups[0]) // plain1 collapses
	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["plain1"], "erroring upstream is excluded")
	require.True(t, probeable["plain1"], "health-excluded upstream must be probed (its recovery needs fresh samples)")
}

// excludeIf override: { probe: false } for traffic-independent predicates
// (the structural-lag case — lag is refreshed by the state poller).
func TestProbeVerdicts_ExcludeIfProbeFalseOverride(t *testing.T) {
	eval := `(upstreams) => upstreams.excludeIf(blockNumberLagAbove(16), { probe: false })`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	setLag(tracker, ups[0], 50)
	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["plain1"], "lagging upstream is excluded")
	require.False(t, probeable["plain1"], "probe:false override must suppress probing for structural exclusions")
}

// excludeIf legacy second arg (reason override string) keeps working and
// keeps the probe-eligible default.
func TestProbeVerdicts_ExcludeIfLegacyReasonString(t *testing.T) {
	eval := `(upstreams) => upstreams.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)), 'custom_reason')`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	failHard(tracker, ups[0])
	probeable, _ := probeSet(engine)
	require.True(t, probeable["plain1"], "legacy string second arg keeps default probe-eligible behavior")
}

// removeCordoned default: cordons reverse via timers/admin, not traffic.
func TestProbeVerdicts_RemoveCordonedDefaultNoProbe(t *testing.T) {
	engine, ups, tracker, cancel := mkVerdictEngine(t, `(upstreams) => upstreams.removeCordoned()`)
	defer cancel()
	defer engine.Stop()

	tracker.Cordon(ups[0], "*", "penalty box")
	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["plain1"], "cordoned upstream is excluded")
	require.False(t, probeable["plain1"], "cordoned upstream must NOT be probed by default")
}

func TestProbeVerdicts_RemoveCordonedProbeOverride(t *testing.T) {
	engine, ups, tracker, cancel := mkVerdictEngine(t, `(upstreams) => upstreams.removeCordoned({ probe: true })`)
	defer cancel()
	defer engine.Stop()

	tracker.Cordon(ups[0], "*", "penalty box")
	probeable, _ := probeSet(engine)
	require.True(t, probeable["plain1"], "probe:true override re-enables probing for cordoned upstreams")
}

// ─── the verdict matrix: multi-reason + order-insensitivity ─────────────

// THE ordering trap: an upstream that is BOTH erroring and tag-excluded.
// The health step runs FIRST and drops it — but the later excludeTag step
// still judges it droppable and records a blocking verdict, so it is NOT
// probed. "Which step dropped it" must not decide billing.
func TestProbeVerdicts_MultiReason_HealthFirstTagSecond_NoProbe(t *testing.T) {
	eval := `(upstreams) => upstreams
		.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
		.excludeTag('tier:static')`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	failHard(tracker, ups[2]) // "tagged" is ALSO erroring
	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["tagged"], "doubly-doomed upstream is excluded")
	require.False(t, probeable["tagged"],
		"erroring+tagged upstream must NOT be probed: the static tag still blocks regardless of step order")
}

// Same scenario, opposite order — identical outcome.
func TestProbeVerdicts_MultiReason_TagFirstHealthSecond_NoProbe(t *testing.T) {
	eval := `(upstreams) => upstreams
		.excludeTag('tier:static')
		.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	failHard(tracker, ups[2])
	probeable, _ := probeSet(engine)
	require.False(t, probeable["tagged"], "order-insensitive: tag verdict blocks probing either way")
}

// The dual: a PLAIN upstream that errors is probed regardless of where the
// (non-matching) tag step sits in the chain.
func TestProbeVerdicts_HealthOnlyExclusionProbesRegardlessOfTagStep(t *testing.T) {
	eval := `(upstreams) => upstreams
		.excludeTag('tier:static')
		.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	failHard(tracker, ups[0]) // plain1: errors only, no tag
	probeable, _ := probeSet(engine)
	require.True(t, probeable["plain1"], "health-only exclusion stays probe-eligible")
	require.False(t, probeable["tagged"], "tag-only exclusion stays blocked")
}

// ─── untracked exclusions: conservative fallback ─────────────────────────

// An upstream dropped by a raw JS filter (no tracked step) has no verdicts
// and defaults to probing — preserving prior behavior.
func TestProbeVerdicts_UntrackedExclusionDefaultsToProbe(t *testing.T) {
	eval := `(upstreams) => upstreams.filter(u => u.id !== 'plain1')`
	engine, _, _, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["plain1"], "raw-filtered upstream is excluded")
	require.True(t, probeable["plain1"], "untracked exclusion must keep prior probe-everything behavior")
}

// But the matrix still covers untracked drops: if a TRACKED blocking step
// would also drop the raw-filtered upstream, the blocking verdict wins.
func TestProbeVerdicts_UntrackedDropStillBlockedByTagVerdict(t *testing.T) {
	eval := `(upstreams) => upstreams
		.filter(u => u.id !== 'tagged')
		.excludeTag('tier:static')`
	engine, _, _, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["tagged"], "tagged upstream is excluded (by the raw filter)")
	require.False(t, probeable["tagged"],
		"the absent-upstream sweep lets the tag step block probing even though the raw filter dropped it first")
}

// ─── interactions ────────────────────────────────────────────────────────

// Admitted upstreams are never probe candidates, whatever the verdicts.
func TestProbeVerdicts_AdmittedUpstreamsNeverProbed(t *testing.T) {
	engine, _, _, cancel := mkVerdictEngine(t, `(upstreams) => upstreams`)
	defer cancel()
	defer engine.Stop()

	probeable, excludedAll := probeSet(engine)
	require.Empty(t, probeable, "nothing excluded -> nothing probed")
	require.Empty(t, excludedAll)
}

// whenEmpty resurrection: if the fallback restores everyone, nothing is
// excluded and nothing is probed.
func TestProbeVerdicts_WhenEmptyResurrectionClearsProbing(t *testing.T) {
	eval := `(upstreams) => upstreams
		.excludeTag('tier:static')
		.excludeIf(all(samplesAbove(10), errorRateAbove(0.7)))
		.whenEmpty(() => upstreams)`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	failHard(tracker, ups[0])
	failHard(tracker, ups[1]) // both plain upstreams collapse -> pool empties -> whenEmpty restores all
	probeable, excludedAll := probeSet(engine)
	require.Empty(t, excludedAll, "whenEmpty restored the full set; nothing is excluded")
	require.Empty(t, probeable)
}

// A judge that throws during the absent-upstream sweep must not sink the
// eval or accidentally block probing. (A predicate throwing on IN-chain
// upstreams sinks the eval — pre-existing excludeIf semantics, unchanged.)
func TestProbeVerdicts_ThrowingJudgeIsSafe(t *testing.T) {
	eval := `(upstreams) => upstreams
		.filter(u => u.id !== 'plain1')
		.excludeIf((u) => { if (u.id === 'plain1') throw new Error('boom'); return false; })`
	engine, _, _, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	probeable, excludedAll := probeSet(engine)
	require.True(t, excludedAll["plain1"], "raw-filtered upstream still excluded")
	require.True(t, probeable["plain1"], "throwing judge contributes no verdict; conservative default probes")
}

// Verdicts reset across ticks: a recovered upstream's stale verdicts must
// not leak into the next tick's decision.
func TestProbeVerdicts_ResetAcrossTicks(t *testing.T) {
	eval := `(upstreams) => upstreams.excludeIf(blockNumberLagAbove(16), { probe: false })`
	engine, ups, tracker, cancel := mkVerdictEngine(t, eval)
	defer cancel()
	defer engine.Stop()

	setLag(tracker, ups[0], 50)
	probeable, _ := probeSet(engine)
	require.False(t, probeable["plain1"])

	setLag(tracker, ups[0], 0) // recovers
	probeable, excludedAll := probeSet(engine)
	require.False(t, excludedAll["plain1"], "recovered upstream re-admitted")
	require.False(t, probeable["plain1"], "no stale probe candidacy")
}
