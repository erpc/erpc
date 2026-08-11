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
	now := start
	for i := 0; i < samples; i++ {
		tr.Observe(now, tipsFromInts(head), head, trajectoryParams())
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
	tr.Observe(now, tipsFromInts(head), 999_999_999, trajectoryParams())
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
			d := tr.Observe(now, tipsFromInts(1_000_000+int64(i)*100, 999_000), 999_000, params)
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
		d := tr.Observe(now, tipsFromInts(head, head, head-20_000, head-20_000, head-20_000), head-20_000, params)
		assert.False(t, d.Overrode, "span below the configured window must keep the referee inert")
	})

	t.Run("StaleTrackIsInert", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)
		require.True(t, tr.fit.Load().spanMs >= params.Window.Milliseconds())

		// Nothing evaluated for longer than the maximum gap: the fit no longer
		// describes the present, whatever it says.
		now = now.Add(tipMaxSampleGap + time.Second)
		d := tr.Observe(now, tipsFromInts(head+1200, head+1200, head, head, head), head, params)
		assert.False(t, d.Overrode, "a gap past tipMaxSampleGap must stand the referee down")
	})

	t.Run("HaltedChainIsInert", func(t *testing.T) {
		var tr TipTrajectory
		// A completely flat track: velocity 0, so there is no trajectory to be
		// off. The majority pick is already the right answer for a halted chain.
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 0, 130)
		require.NotNil(t, tr.fit.Load())
		d := tr.Observe(now, tipsFromInts(head+5_000, head+5_000, head, head, head), head, params)
		assert.False(t, d.Overrode, "v <= 0 must stand the referee down")
	})

	t.Run("DisabledRecordsNothing", func(t *testing.T) {
		var tr TipTrajectory
		off := TipTrajectoryParams{Window: 0, ToleranceFloor: 1024}
		now := time.Now()
		for i := 0; i < 200; i++ {
			tr.Observe(now, tipsFromInts(1_000_000+int64(i)*100), 1_000_000+int64(i)*100, off)
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
			tr.Observe(now, tipsFromInts(head, head, frozen, frozen, frozen), frozen, params)
		}

		d := tr.Observe(now.Add(time.Second), tipsFromInts(head, head, frozen, frozen, frozen), frozen, params)
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
			tr.Observe(now, tipsFromInts(head, frozen, frozen, frozen, frozen), frozen, params)
		}

		d := tr.Observe(now.Add(time.Second), tipsFromInts(head, frozen, frozen, frozen, frozen), frozen, params)
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
		d := tr.Observe(now, tipsFromInts(head+3_000, head+3_000, head-20_000, head-20_000, head-20_000), head-20_000, params)
		assert.False(t, d.Overrode)
		assert.True(t, d.Declined, "a refused raise is the fallback outcome")
		assert.Equal(t, head-20_000, d.Pick)
	})

	t.Run("RunawayPairIsOutsideTolerance", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		// Two upstreams jump far past where the chain can possibly be.
		d := tr.Observe(now, tipsFromInts(head+500_000, head+500_000, head, head, head), head, params)
		assert.False(t, d.Overrode, "a group that far off the trajectory must not win")
		assert.Equal(t, head, d.Pick)
	})

	t.Run("UpwardOnly", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		// Three upstreams run ahead, two sit exactly on the trajectory: the
		// on-trajectory group wins the election, but electing it would LOWER
		// the majority pick, so the referee stands down instead.
		d := tr.Observe(now, tipsFromInts(head+50_000, head+50_000, head+50_000, head, head), head+50_000, params)
		assert.False(t, d.Overrode, "the referee may never lower or hold the pick")
		assert.Equal(t, head+50_000, d.Pick)
	})

	t.Run("HealthyFleetIsOneGroupAndNeverIntervenes", func(t *testing.T) {
		var tr TipTrajectory
		now, head := warmTrajectory(&tr, time.Now(), 1_000_000, 100, 130)

		// Normal propagation skew, well inside one cluster width.
		heads := tipsFromInts(head, head-1, head-3, head-7, head-11)
		median := PickServedTip(heads).Tip
		d := tr.Observe(now, heads, median, params)
		assert.False(t, d.Overrode)
		assert.False(t, d.Declined, "a fleet in one cluster is not even a near miss")
		assert.Equal(t, median, d.Pick)
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
		bursty.Observe(now, tipsFromInts(head), head, trajectoryParams())
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
		tr.Observe(now.Add(time.Duration(i)*time.Millisecond), tipsFromInts(1_000_000), 1_000_000, params)
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
				tr.Observe(start.Add(time.Duration(i)*tipSampleInterval), tipsFromInts(head, head-1), head, params)
			}
		}()
	}
	wg.Wait()

	assert.Positive(t, tr.SampleCount())
	assert.LessOrEqual(t, tr.SampleCount(), tipBufferCapacity(params.Window),
		"the ring must stay bounded however many goroutines record into it")
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
