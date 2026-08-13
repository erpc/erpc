package evm

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// ballot is what the network hands the referee: the pick's own descending,
// zero-filtered slice (ServedTipPick.Sorted). Tests go through PickServedTip so
// they exercise the same single sort the request path performs.
func ballot(blocks ...int64) []ServedTipInput {
	return PickServedTip(tipsFromInts(blocks...)).Sorted
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

// ─── Long-term trajectory referee ────────────────────────────────────────────
//
// These pin the tracker's own mechanics: the fit it derives from a head track,
// the confidence gate that keeps it inert, and the group election. The
// end-to-end behaviour (which block a network actually serves) is pinned in
// package erpc's networks_served_tip_test.go.

// trajectoryParams is the referee's config as a network with default settings
// supplies it: a 10-minute window and the 1024-block regression tolerance.
func trajectoryParams() TipTrajectoryParams {
	return TipTrajectoryParams{Window: 10 * time.Minute, ToleranceFloor: 1024}
}

// warmTrajectory feeds `samples` observations one tipSampleInterval apart,
// advancing the head by `perSample` blocks each time, and returns the clock and
// head it stopped at. Every sample goes through Observe, i.e. the same entry
// point the request path uses.
func warmTrajectory(tr *TipTrajectory, start time.Time, head int64, perSample int64, samples int) (time.Time, int64) {
	return warmTrajectoryWith(tr, start, head, perSample, samples, trajectoryParams())
}

func warmTrajectoryWith(tr *TipTrajectory, start time.Time, head int64, perSample int64, samples int, p TipTrajectoryParams) (time.Time, int64) {
	now := start
	for i := 0; i < samples; i++ {
		tr.Observe(now, ballot(head, head), head, p)
		now = now.Add(tipSampleInterval)
		head += perSample
	}
	return now, head
}

func TestTipTrajectory_FitRecoversVelocityAndExpectedHead(t *testing.T) {
	var tr TipTrajectory
	// 20 blocks/s (100 blocks per 5s sample), 130 samples ≈ 10m50s of history.
	now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

	fit := tr.fit.Load()
	require.NotNil(t, fit)
	assert.InDelta(t, 20.0, fit.vPerSec, 0.001, "robust velocity must recover the 20 blocks/s track")
	assert.InDelta(t, 0.0, fit.spread, 0.001, "a perfectly linear track has no residual spread")

	// The head the fit expects one sample-interval on is the next sample's.
	assert.InDelta(t, float64(head), fit.expectedAt(int64(now.Sub(*tr.base.Load())/time.Millisecond)), 1.0)
}

func TestTipTrajectory_PoisonedSampleDoesNotMoveTheFit(t *testing.T) {
	var tr TipTrajectory
	now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 60)
	clean := tr.fit.Load()
	require.NotNil(t, clean)

	// One wrong-chain / garbage observation lands in the middle of the track.
	tr.Observe(now, ballot(head, head), 999_999_999, trajectoryParams())
	now, _ = warmTrajectory(&tr, now.Add(tipSampleInterval), head+100, 100, 70)

	poisoned := tr.fit.Load()
	require.NotNil(t, poisoned)
	assert.InDelta(t, clean.vPerSec, poisoned.vPerSec, 0.001,
		"a single poisoned sample must not move the median-of-lagged-slopes velocity")
	assert.Less(t, poisoned.spread, float64(1024),
		"nor may it inflate the residual spread past the tolerance floor")
}

func TestTipTrajectory_ConfidenceGate(t *testing.T) {
	params := trajectoryParams()

	t.Run("ColdBufferIsInert", func(t *testing.T) {
		var tr TipTrajectory
		now := time.Now()
		for i := 0; i < tipMinSamples-1; i++ {
			d := tr.Observe(now, ballot(1_000_000+int64(i)*100, 999_000), 999_000, params)
			require.False(t, d.Overrode, "sample %d: a cold tracker must never override", i)
			now = now.Add(tipSampleInterval)
		}
		assert.Nil(t, tr.fit.Load(), "no fit below tipMinSamples")
	})

	t.Run("ShortWindowIsInert", func(t *testing.T) {
		var tr TipTrajectory
		// 60 samples = 5 minutes of span: enough points, not enough window.
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 60)
		fit := tr.fit.Load()
		require.NotNil(t, fit)
		require.Less(t, fit.spanMs, params.Window.Milliseconds())

		// A stalled majority + a fresh pair: the shape that WOULD be overridden.
		d := tr.Observe(now, ballot(head, head, head-20_000, head-20_000, head-20_000), head-20_000, params)
		assert.False(t, d.Overrode, "span below the configured window must keep the referee inert")
	})

	t.Run("StaleTrackIsInert", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)
		require.True(t, tr.fit.Load().spanMs >= params.Window.Milliseconds())

		// Nothing evaluated for longer than the maximum gap: the fit no longer
		// describes the present, whatever it says.
		now = now.Add(tipMaxSampleGap + time.Second)
		d := tr.Observe(now, ballot(head+1200, head+1200, head, head, head), head, params)
		assert.False(t, d.Overrode, "a gap past tipMaxSampleGap must stand the referee down")
	})

	t.Run("HaltedChainIsInert", func(t *testing.T) {
		var tr TipTrajectory
		// A completely flat track: velocity 0, so there is no trajectory to be
		// off. The majority pick is already the right answer for a halted chain.
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 0, 130)
		require.NotNil(t, tr.fit.Load())
		d := tr.Observe(now, ballot(head+5_000, head+5_000, head, head, head), head, params)
		assert.False(t, d.Overrode, "v <= 0 must stand the referee down")
	})

	t.Run("DisabledRecordsNothing", func(t *testing.T) {
		var tr TipTrajectory
		off := TipTrajectoryParams{Window: 0, ToleranceFloor: 1024}
		now := time.Now()
		for i := 0; i < 200; i++ {
			tr.Observe(now, ballot(1_000_000+int64(i)*100, 1_000_000+int64(i)*100), 1_000_000+int64(i)*100, off)
			now = now.Add(tipSampleInterval)
		}
		assert.Zero(t, tr.SampleCount(), "trajectoryWindow: 0 must record nothing at all")
		assert.Nil(t, tr.fit.Load())
	})
}

func TestTipTrajectory_GroupElection(t *testing.T) {
	params := trajectoryParams()

	t.Run("StalledMajorityLosesToCorroboratedFreshPair", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		// The stall: three heads freeze, two keep the track. The tracker keeps
		// sampling the (frozen) majority median throughout — its evidence is
		// never its own output.
		frozen := head
		for i := 0; i < 24; i++ {
			now = now.Add(tipSampleInterval)
			head += 100
			tr.Observe(now, ballot(head, head, frozen, frozen, frozen), frozen, params)
		}

		d := tr.Observe(now.Add(time.Second), ballot(head, head, frozen, frozen, frozen), frozen, params)
		assert.True(t, d.Overrode, "the on-trajectory pair must outvote the stalled majority")
		assert.Equal(t, head, d.Pick, "the pick is the fresh group's MINIMUM — both members have it")
		assert.False(t, d.Declined, "an override is not a near miss")
	})

	t.Run("SingletonIsNeverCorroboration", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		frozen := head
		for i := 0; i < 24; i++ {
			now = now.Add(tipSampleInterval)
			head += 100
			tr.Observe(now, ballot(head, frozen, frozen, frozen, frozen), frozen, params)
		}

		d := tr.Observe(now.Add(time.Second), ballot(head, frozen, frozen, frozen, frozen), frozen, params)
		assert.False(t, d.Overrode,
			"one upstream matching the trajectory perfectly is still one witness")
		assert.Equal(t, frozen, d.Pick)
	})

	t.Run("NearMissIsDeclinedNotOverridden", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		// Everything is off the trajectory: the freshest pair is the closest
		// group to where the head should be, but still further than the
		// tolerance. The referee elects it and then refuses it — a near miss,
		// which is the one non-override outcome worth counting.
		d := tr.Observe(now, ballot(head+3_000, head+3_000, head-20_000, head-20_000, head-20_000), head-20_000, params)
		assert.False(t, d.Overrode)
		assert.True(t, d.Declined, "a refused raise is the fallback outcome")
		assert.Equal(t, head-20_000, d.Pick)
	})

	t.Run("RunawayPairIsOutsideTolerance", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		// Two upstreams jump far past where the chain can possibly be.
		d := tr.Observe(now, ballot(head+500_000, head+500_000, head, head, head), head, params)
		assert.False(t, d.Overrode, "a group that far off the trajectory must not win")
		assert.Equal(t, head, d.Pick)
	})

	t.Run("UpwardOnly", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		// Three upstreams run ahead, two sit exactly on the trajectory: the
		// on-trajectory group wins the election, but electing it would LOWER
		// the majority pick, so the referee stands down instead.
		d := tr.Observe(now, ballot(head+50_000, head+50_000, head+50_000, head, head), head+50_000, params)
		assert.False(t, d.Overrode, "the referee may never lower or hold the pick")
		assert.Equal(t, head+50_000, d.Pick)
	})

	t.Run("PersistentSplitOnAHealthyFleetIsDeclinedNotOverridden", func(t *testing.T) {
		// The production shape this gate exists for. A ~1.6 blocks/s chain with
		// four upstreams, every one of them advancing, split wider than one
		// cluster width: the top pair is the only electable group (the other two
		// are singletons, which tipMinGroupSize rejects), so it wins the election
		// unopposed and sits well inside the 1024-block tolerance.
		//
		// Nothing has stalled — the majority tracks the chain — so the referee
		// must decline. Before the majority-stall gate it let the pair earn a 30s
		// dwell and then raised the tip by the width of the split, indefinitely.
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 30_000_000, 8, 130)

		for i := 0; i < 240; i++ { // 20 minutes — far past tipDwellDuration
			now = now.Add(tipSampleInterval)
			head += 8
			pick := PickServedTip(tipsFromInts(head, head, head-24, head-64))
			d := tr.Observe(now, pick.Sorted, pick.Tip, params)
			require.False(t, d.Overrode,
				"sample %d: a fleet whose majority is on the trajectory must never be overridden (pick %d, majority %d, expected %d)",
				i, d.Pick, pick.Tip, d.Expected)
			require.Equal(t, pick.Tip, d.Pick, "sample %d: the majority pick must survive intact", i)
		}
	})

	t.Run("SlowChainStallIsStillCorrected", func(t *testing.T) {
		// The same slow chain, but the majority genuinely stops. The gate is
		// chain-relative, so a stall crosses it after tipMajorityStallSeconds
		// however few blocks that is — 48 here, against the 2400 a 20 blocks/s
		// chain would produce in the same time.
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 30_000_000, 8, 130)

		frozen := head
		var d TipTrajectoryDecision
		for i := 0; i < 24; i++ { // 2 minutes of a frozen majority
			now = now.Add(tipSampleInterval)
			head += 8
			d = tr.Observe(now, ballot(head, head, frozen, frozen, frozen), frozen, params)
		}
		assert.True(t, d.Overrode, "a genuine stall must still be corrected on a slow chain")
		assert.Equal(t, head, d.Pick, "the pick is the fresh group's minimum")
	})

	t.Run("HealthyFleetIsOneGroupAndNeverIntervenes", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		// Normal propagation skew, well inside one cluster width.
		pick := PickServedTip(tipsFromInts(head, head-1, head-3, head-7, head-11))
		d := tr.Observe(now, pick.Sorted, pick.Tip, params)
		assert.False(t, d.Overrode)
		assert.False(t, d.Declined, "a fleet in one cluster is not even a near miss")
		assert.Equal(t, pick.Tip, d.Pick)
	})
}

func TestTipTrajectory_BurstyChainWidensItsOwnTolerance(t *testing.T) {
	var steady, bursty TipTrajectory

	_, _ = warmTrajectory(&steady, time.Now(), 1_000_000, 100, 130)

	// Same average velocity, delivered in bursts: 9 samples of nothing, then a
	// 1000-block batch (the L2-sequencer shape).
	now, head := time.Now(), int64(1_000_000)
	for i := 0; i < 130; i++ {
		if i%10 == 9 {
			head += 1000
		}
		bursty.Observe(now, ballot(head, head), head, trajectoryParams())
		now = now.Add(tipSampleInterval)
	}

	steadyFit, burstyFit := steady.fit.Load(), bursty.fit.Load()
	require.NotNil(t, steadyFit)
	require.NotNil(t, burstyFit)
	assert.InDelta(t, steadyFit.vPerSec, burstyFit.vPerSec, 2.0,
		"both tracks average the same velocity")
	assert.Greater(t, burstyFit.spread*tipToleranceSigmas, float64(1024),
		"a chain that pauses and bursts must widen its own tolerance past the floor")
	assert.Less(t, steadyFit.spread*tipToleranceSigmas, float64(1024),
		"a metronomic chain keeps the configured floor")
}

func TestTipTrajectory_SampleThrottleAndBufferBound(t *testing.T) {
	var tr TipTrajectory
	params := trajectoryParams()
	now := time.Now()

	// A hot request path: 500 evaluations inside one sample interval.
	for i := 0; i < 500; i++ {
		tr.Observe(now.Add(time.Duration(i)*time.Millisecond), ballot(1_000_000, 1_000_000), 1_000_000, params)
	}
	assert.Equal(t, 1, tr.SampleCount(), "the throttle admits one sample per interval, whatever the QPS")

	// And the ring never grows past the capacity derived from the window.
	_, _ = warmTrajectory(&tr, now.Add(tipSampleInterval), 1_000_100, 100, 4_000)
	assert.Equal(t, tipBufferCapacity(params.Window), tr.SampleCount())
	assert.LessOrEqual(t, tr.fit.Load().spanMs, int64(tipBufferCapacity(params.Window))*tipSampleInterval.Milliseconds())
}

// The request path calls Observe from every serving goroutine at once, so the
// throttle's double-checked lock, the refit and the lock-free fit read all have
// to hold up under -race.
func TestTipTrajectory_ConcurrentObserve(t *testing.T) {
	var tr TipTrajectory
	params := trajectoryParams()
	start := time.Now()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				head := int64(1_000_000 + i*100)
				tr.Observe(start.Add(time.Duration(i)*tipSampleInterval), ballot(head, head-1), head, params)
			}
		}()
	}
	wg.Wait()

	assert.Positive(t, tr.SampleCount())
	assert.LessOrEqual(t, tr.SampleCount(), tipBufferCapacity(params.Window),
		"the ring must stay bounded however many goroutines record into it")
}

// ─── Corroboration must be EARNED (dwell + velocity agreement) ───────────────
//
// Matching the trajectory in one instant is not evidence. These pin the two
// exploits of the instant test, both confirmed by simulation against the first
// version of the referee.

// in / ballotOf build a ballot with EXPLICIT upstream ids, so a test can change
// a group's MEMBERSHIP without changing any head value.
func in(id string, blk int64) ServedTipInput { return ServedTipInput{UpstreamID: id, BlockNumber: blk} }
func ballotOf(ins ...ServedTipInput) []ServedTipInput {
	return PickServedTip(ins).Sorted
}

// A GENUINE chain halt, with no traffic gap at all: the process evaluates every
// 5s, the honest majority is frozen at the last real head, and two upstreams sit
// at a FIXED higher block (a fork, a wrong-chain pair, a shared counter frozen
// mid-flight). Because the fitted trajectory keeps extrapolating, the expected
// head SWEEPS upward through every fixed offset — so the static pair is "on
// trajectory" for as long as the sweep takes to cross it. Measured against the
// instant test, the +2000 pair won 74 of 240 halt evaluations, i.e. minutes of a
// halted chain served from a fork.
func TestTipTrajectory_HaltSweepCannotElectAStaticGroup(t *testing.T) {
	for _, ahead := range []int64{2_000, 6_000, 20_000} {
		var tr TipTrajectory
		p := trajectoryParams()
		now, frozen := warmTrajectory(&tr, time.Now(), 74_500_000, 100, 140)
		require.NotNil(t, tr.fit.Load(), "precondition: the referee is warm and confident")

		rogue := frozen + ahead
		overrides, contended := 0, 0
		for k := 0; k < 240; k++ { // 20 minutes of halt, sampled every 5s
			d := tr.Observe(now, ballotOf(
				in("r1", rogue), in("r2", rogue),
				in("h1", frozen), in("h2", frozen), in("h3", frozen),
			), frozen, p)
			if d.Overrode {
				overrides++
			}
			if d.Declined {
				contended++
			}
			now = now.Add(tipSampleInterval)
		}
		assert.Zero(t, overrides,
			"a static group cannot have produced the chain progress the fit describes, "+
				"however close the sweeping expected head passes to it (offset +%d)", ahead)
		t.Logf("offset +%-6d : 0 overrides, %d evaluations where the sweep made the static pair the ELECTED group",
			ahead, contended)
	}
}

// stalledFleet drives a fleet in which `fresh` upstreams sit exactly on the
// fitted trajectory and the rest are frozen `behind` blocks below it, evaluating
// every tipSampleInterval and reporting the first elapsed time at which the
// referee overrode (or -1). The elapsed times these tests assert on are ABSOLUTE
// literals, deliberately: a dwell pinned in terms of tipDwellDuration is pinned
// by nothing, since zeroing the constant would move the expectation with it.
func stalledFleet(tr *TipTrajectory, now time.Time, head int64, behind int64, run time.Duration, fresh []string, frozenIDs []string) (firstOverride time.Duration, overrides int, at map[time.Duration]bool) {
	p := trajectoryParams()
	start := now
	frozenHead := head - behind
	firstOverride, at = -1, map[time.Duration]bool{}
	for elapsed := time.Duration(0); elapsed <= run; elapsed += tipSampleInterval {
		ins := make([]ServedTipInput, 0, len(fresh)+len(frozenIDs))
		for _, id := range fresh {
			ins = append(ins, in(id, head+int64(elapsed.Seconds())*20))
		}
		for _, id := range frozenIDs {
			ins = append(ins, in(id, frozenHead))
		}
		pick := PickServedTip(ins)
		d := tr.Observe(start.Add(elapsed), pick.Sorted, pick.Tip, p)
		at[elapsed] = d.Overrode
		if d.Overrode {
			overrides++
			if firstOverride < 0 {
				firstOverride = elapsed
			}
		}
	}
	return firstOverride, overrides, at
}

func TestTipTrajectory_DwellMustBeEarned(t *testing.T) {
	// A pair that is on the trajectory, corroborating each other, advancing at
	// the chain's own velocity, and separated from the stalled majority from the
	// VERY FIRST evaluation — i.e. nothing but elapsed time stands between it and
	// an override. It must still wait.
	t.Run("AnOnTrajectoryGroupStillWaits", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		_, _, at := stalledFleet(&tr, now, head, 3_000, 45*time.Second,
			[]string{"u1", "u2"}, []string{"u3", "u4", "u5"})

		for _, elapsed := range []time.Duration{0, 5 * time.Second, 15 * time.Second, 25 * time.Second} {
			assert.False(t, at[elapsed],
				"at %v the group has corroborated nothing yet — it has merely been right for %v", elapsed, elapsed)
		}
		assert.True(t, at[45*time.Second],
			"but by 45s of holding that place it has earned the override the stall needs")
	})

	// The confirmed false-override shape, with the geometry taken out of the
	// argument: the two clusters are further apart than one cluster width from
	// the first sample, so nothing but the dwell prevents an override here.
	t.Run("TransientDivergenceNeverWins", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		_, overrides, _ := stalledFleet(&tr, now, head, 1_000, 15*time.Second,
			[]string{"u1", "u2"}, []string{"u3", "u4"})
		assert.Zero(t, overrides,
			"15 seconds of a two-of-four split must never hand the tip to half the fleet")

		// And the fleet re-merging leaves nothing behind.
		merged := head + 400
		pick := PickServedTip([]ServedTipInput{in("u1", merged), in("u2", merged), in("u3", merged), in("u4", merged)})
		d := tr.Observe(now.Add(20*time.Second), pick.Sorted, pick.Tip, trajectoryParams())
		assert.False(t, d.Overrode)
	})

	// A group is a SET of upstreams, not a pair of head values: replacing a
	// member restarts the corroboration, because the new set has proven nothing.
	// This must reach the dwell's RESET branch (an existing stretch, different
	// members), not its first-evaluation nil branch.
	t.Run("MembershipChangeRestartsTheDwell", func(t *testing.T) {
		var tr TipTrajectory
		p := trajectoryParams()
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		frozen := head - 3_000
		stall := func(elapsed time.Duration, freshA, freshB string) TipTrajectoryDecision {
			pick := PickServedTip([]ServedTipInput{
				in(freshA, head+int64(elapsed.Seconds())*20), in(freshB, head+int64(elapsed.Seconds())*20),
				in("u3", frozen), in("u4", frozen), in("u5", frozen),
			})
			return tr.Observe(now.Add(elapsed), pick.Sorted, pick.Tip, p)
		}
		var elapsed time.Duration
		for ; elapsed <= 60*time.Second; elapsed += tipSampleInterval {
			stall(elapsed, "u1", "u2")
		}
		require.True(t, stall(elapsed, "u1", "u2").Overrode, "precondition: {u1,u2} has earned its override")

		earned := tr.dwell.Load()
		require.NotNil(t, earned, "precondition: a stretch is in progress, so the swap hits the RESET branch")

		elapsed += tipSampleInterval
		d := stall(elapsed, "u1", "u6")
		assert.False(t, d.Overrode, "a different set of upstreams starts a new dwell")
		assert.True(t, d.Declined, "and the refusal is the fallback outcome, not silence")

		restarted := tr.dwell.Load()
		require.NotNil(t, restarted)
		assert.NotEqual(t, earned.group, restarted.group, "the stretch belongs to the new member set")
		assert.Greater(t, restarted.startMs, earned.startMs, "and it starts now, not when the old one did")
	})
}

// A group's identity must survive being renamed by the fleet's own naming
// convention. The commutative fold this replaced collided on suffix swaps —
// nineteen colliding pairs inside one realistic twelve-node fleet — and a
// collision lets a DIFFERENT set of upstreams inherit a dwell it never earned.
func TestTipTrajectory_GroupIdentityDistinguishesSuffixSwaps(t *testing.T) {
	a := []ServedTipInput{in("prism-celo-1", 10), in("nirvana-celo-2", 10)}
	b := []ServedTipInput{in("prism-celo-2", 10), in("nirvana-celo-1", 10)}

	assert.NotEqual(t, groupIdentity(a), groupIdentity(b),
		"two different pairs of the same fleet must not share an identity")
	assert.Equal(t, groupIdentity(a), groupIdentity([]ServedTipInput{a[1], a[0]}),
		"but member ORDER must not change it — the identity is a property of the set")

	// The consequence that matters: the earned dwell is not inherited.
	var tr TipTrajectory
	p := trajectoryParams()
	now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)
	frozen := head - 3_000
	stall := func(elapsed time.Duration, freshA, freshB string) TipTrajectoryDecision {
		pick := PickServedTip([]ServedTipInput{
			in(freshA, head+int64(elapsed.Seconds())*20), in(freshB, head+int64(elapsed.Seconds())*20),
			in("quicknode-celo-1", frozen), in("quicknode-celo-2", frozen), in("quicknode-celo-3", frozen),
		})
		return tr.Observe(now.Add(elapsed), pick.Sorted, pick.Tip, p)
	}
	var elapsed time.Duration
	for ; elapsed <= 60*time.Second; elapsed += tipSampleInterval {
		stall(elapsed, "prism-celo-1", "nirvana-celo-2")
	}
	require.True(t, stall(elapsed, "prism-celo-1", "nirvana-celo-2").Overrode,
		"precondition: {prism-celo-1, nirvana-celo-2} earned its override")

	elapsed += tipSampleInterval
	d := stall(elapsed, "prism-celo-2", "nirvana-celo-1")
	assert.False(t, d.Overrode,
		"the suffix-swapped pair is a different set and must earn its own dwell")
}

// The gap rule is about the WINDOW, not about the newest sample: standing the
// referee down for the single evaluation that notices a hole let the next
// evaluation — a millisecond later — extrapolate straight across it.
func TestTipTrajectory_GapMustLeaveTheWindow(t *testing.T) {
	p := trajectoryParams()

	t.Run("TheEvaluationAfterTheGapIsInertToo", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 74_500_000, 100, 140)

		// A 10-minute traffic hole: the chain kept going, the process saw none
		// of it. Then the stall shape — 2 fresh, 3 frozen far behind.
		gap := 10 * time.Minute
		now = now.Add(gap)
		head += int64(gap.Seconds()) * 20
		frozen := head - 30_000
		tips := ballotOf(
			in("a", head), in("b", head),
			in("c", frozen), in("d", frozen), in("e", frozen),
		)

		d1 := tr.Observe(now, tips, frozen, p)
		d2 := tr.Observe(now.Add(time.Millisecond), tips, frozen, p)
		assert.False(t, d1.Overrode, "the evaluation that notices the gap is inert")
		assert.False(t, d2.Overrode,
			"and so is the next one: the fit still spans the hole it cannot see across")
	})

	t.Run("HaltInsideTheGapCannotLiftExpected", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 74_500_000, 100, 140)

		// 10 minutes of neither traffic nor chain progress. The robust intercept
		// would otherwise lift the fit's anchor by a whole gap of chain, putting
		// "expected" thousands of blocks above the true halted head — exactly
		// where a rogue pair would be waiting.
		now = now.Add(10 * time.Minute)
		_ = tr.Observe(now, ballotOf(in("h1", head), in("h2", head), in("h3", head)), head, p)

		fit := tr.fit.Load()
		require.NotNil(t, fit)
		nowMs := int64(now.Sub(*tr.base.Load()) / time.Millisecond)
		inflation := int64(fit.expectedAt(nowMs)) - head
		require.Positive(t, inflation, "precondition: the raw fit does extrapolate across the hole")

		d := tr.Observe(now.Add(time.Millisecond), ballotOf(
			in("r1", head+inflation), in("r2", head+inflation),
			in("h1", head), in("h2", head), in("h3", head),
		), head, p)
		assert.False(t, d.Overrode,
			"a pair sitting exactly where the gap-inflated fit expects the head must not win")
		assert.Zero(t, d.Expected, "the fit is refused outright, so nothing is even compared to it")
	})
}

// The params are re-read on every evaluation so a config reload takes effect
// without rebuilding the tracker — but the ring was sized once, at first use. An
// operator RAISING trajectoryWindow therefore asked for a span the existing ring
// could never hold, and the referee stood down forever while looking configured.
// A window change now rebuilds the ring and re-warms.
func TestTipTrajectory_WindowChangeRebuildsTheRing(t *testing.T) {
	var tr TipTrajectory
	now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)
	require.NotNil(t, tr.fit.Load(), "precondition: warm and confident at the original window")
	require.Greater(t, tr.SampleCount(), tipMinSamples)

	// The same tracker, now asked for a window its ring cannot span.
	raised := TipTrajectoryParams{Window: 30 * time.Minute, ToleranceFloor: 1024}
	require.Greater(t, tipBufferCapacity(raised.Window), tipBufferCapacity(10*time.Minute),
		"precondition: the raised window needs a bigger ring")

	tr.Observe(now, ballot(head, head), head, raised)
	assert.Equal(t, 1, tr.SampleCount(), "the track restarts on the new window")
	assert.Nil(t, tr.fit.Load(), "and no fit from the old one survives it")
	assert.Nil(t, tr.dwell.Load(), "nor any corroboration earned under it")

	// Re-warming at the new window works — i.e. the referee comes back rather
	// than standing down forever.
	now, _ = warmTrajectoryWith(&tr, now.Add(tipSampleInterval), head+100, 100, 420, raised)
	fit := tr.fit.Load()
	require.NotNil(t, fit)
	assert.GreaterOrEqual(t, fit.spanMs, raised.Window.Milliseconds(),
		"the rebuilt ring can span the window the operator asked for")
	_ = now
}

// A ballot that cannot form a group can never produce a decision, so it must not
// pay for one: no ring, no fit, no periodic O(n log n) refit. Most chains in a
// large deployment have exactly one upstream.
func TestTipTrajectory_BallotBelowGroupSizeRecordsNothing(t *testing.T) {
	var tr TipTrajectory
	p := trajectoryParams()
	now := time.Now()
	for i := 0; i < 200; i++ {
		head := 1_000_000 + int64(i)*100
		tr.Observe(now, ballot(head), head, p)
		now = now.Add(tipSampleInterval)
	}
	assert.Zero(t, tr.SampleCount(), "a single-upstream network must not materialize a head track")
	assert.Nil(t, tr.fit.Load())
}

func TestTipClusterWidth(t *testing.T) {
	// Sub-second-block chains and slow chains alike keep the floor; a fast
	// chain gets 10 seconds of its own progress.
	assert.Equal(t, int64(tipMinClusterWidth), tipClusterWidth(0.083), "12s blocks → floor")
	assert.Equal(t, int64(tipMinClusterWidth), tipClusterWidth(1), "1s blocks → floor")
	assert.Equal(t, int64(40), tipClusterWidth(4), "250ms blocks → 10s of progress")
	assert.Equal(t, int64(tipMinClusterWidth), tipClusterWidth(math.NaN()), "NaN must fall to the floor")
	assert.Equal(t, tipMaxClusterWidth, tipClusterWidth(math.MaxFloat64), "no int64 overflow")
}
