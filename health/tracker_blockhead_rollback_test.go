package health

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

// These tests pin the rollback tolerance of the tracker's block-head state:
// a single bogus head sample (e.g. a provider briefly reporting another
// chain's height, or a corrupted response) must not pin the per-upstream
// latest/finalized values — nor the network-level head and every lag-based
// routing decision derived from it — until process restart. The semantics
// mirror the shared-state counter (data.CounterInt64SharedVariable): forward
// progress is always accepted, decreases within
// common.DefaultToleratedBlockHeadRollback are ignored as noise, larger
// decreases are accepted as corrections. The network head is never lowered by
// adopting a single sample; it is re-derived as the max over the per-upstream
// values, so genuinely-behind upstreams can never drag it down.

func newRollbackTestTracker(t *testing.T, projectID string) *Tracker {
	t.Helper()
	tracker := NewTracker(&log.Logger, projectID, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tracker.Bootstrap(ctx)
	return tracker
}

func networkLatest(t *Tracker, net string) int64 {
	return t.getMetadata(metadataKey{nil, net}).evmLatestBlockNumber.Load()
}

func upstreamLatest(t *Tracker, ups common.Upstream) int64 {
	return t.getMetadata(metadataKey{ups, ups.NetworkId()}).evmLatestBlockNumber.Load()
}

func networkFinalized(t *Tracker, net string) int64 {
	return t.getMetadata(metadataKey{nil, net}).evmFinalizedBlockNumber.Load()
}

func upstreamFinalized(t *Tracker, ups common.Upstream) int64 {
	return t.getMetadata(metadataKey{ups, ups.NetworkId()}).evmFinalizedBlockNumber.Load()
}

func blockHeadLag(t *Tracker, ups common.Upstream) int64 {
	return t.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll).BlockHeadLag.Load()
}

func finalizationLag(t *Tracker, ups common.Upstream) int64 {
	return t.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll).FinalizationLag.Load()
}

func TestTrackerLatestBlockRollbackTolerance(t *testing.T) {
	tolerance := int64(common.DefaultToleratedBlockHeadRollback)

	t.Run("ForwardProgressAccepted", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-forward")
		ups := common.NewFakeUpstream("a")

		tracker.SetLatestBlockNumber(ups, 1000, 0)
		tracker.SetLatestBlockNumber(ups, 2000, 0)

		assert.Equal(t, int64(2000), upstreamLatest(tracker, ups))
		assert.Equal(t, int64(2000), networkLatest(tracker, ups.NetworkId()))
		assert.Equal(t, int64(0), blockHeadLag(tracker, ups))
	})

	t.Run("EqualValueIsNoop", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-equal")
		ups := common.NewFakeUpstream("a")

		tracker.SetLatestBlockNumber(ups, 1000, 0)
		tracker.SetLatestBlockNumber(ups, 1000, 0)

		assert.Equal(t, int64(1000), upstreamLatest(tracker, ups))
		assert.Equal(t, int64(1000), networkLatest(tracker, ups.NetworkId()))
	})

	t.Run("SmallRollbackIgnored", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-small")
		ups := common.NewFakeUpstream("a")

		tracker.SetLatestBlockNumber(ups, 10_000, 0)
		// Gap of exactly the tolerance is still "small" (mirrors the counter:
		// only gaps STRICTLY greater than the tolerance are corrections).
		tracker.SetLatestBlockNumber(ups, 10_000-tolerance, 0)

		assert.Equal(t, int64(10_000), upstreamLatest(tracker, ups))
		assert.Equal(t, int64(10_000), networkLatest(tracker, ups.NetworkId()))
		assert.Equal(t, int64(0), blockHeadLag(tracker, ups))
	})

	t.Run("RollbackJustBeyondToleranceApplied", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-boundary")
		ups := common.NewFakeUpstream("a")

		tracker.SetLatestBlockNumber(ups, 10_000, 0)
		tracker.SetLatestBlockNumber(ups, 10_000-tolerance-1, 0)

		assert.Equal(t, 10_000-tolerance-1, upstreamLatest(tracker, ups))
		// Sole upstream: the re-derived network head follows its correction.
		assert.Equal(t, 10_000-tolerance-1, networkLatest(tracker, ups.NetworkId()))
		assert.Equal(t, int64(0), blockHeadLag(tracker, ups))
	})

	// The poisoned-head scenario this change exists for: one upstream briefly
	// reports a bogus far-ahead head. Before the fix, the bogus sample pinned
	// both its own stored value and the network head forever, which made every
	// OTHER (healthy) upstream appear to lag by tens of millions of blocks —
	// selection policies using blockNumberLagAbove then excluded all of them
	// until the process restarted.
	t.Run("BogusHeadCorrectedAndNetworkRederived", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-rederive")
		upsA := common.NewFakeUpstream("a")
		upsB := common.NewFakeUpstream("b")
		upsC := common.NewFakeUpstream("c")
		net := upsA.NetworkId()

		tracker.SetLatestBlockNumber(upsB, 32_000_000, 0)
		tracker.SetLatestBlockNumber(upsC, 32_000_050, 0)
		// upsA delivers a bogus sample far ahead of the real chain.
		tracker.SetLatestBlockNumber(upsA, 100_000_000, 0)

		assert.Equal(t, int64(100_000_000), networkLatest(tracker, net))
		assert.Equal(t, int64(100_000_000-32_000_000), blockHeadLag(tracker, upsB),
			"healthy upstreams appear to lag the bogus head")

		// upsA's next real observation corrects it.
		tracker.SetLatestBlockNumber(upsA, 32_000_100, 0)

		assert.Equal(t, int64(32_000_100), upstreamLatest(tracker, upsA))
		assert.Equal(t, int64(32_000_100), networkLatest(tracker, net),
			"network head re-derived as max over per-upstream values")
		assert.Equal(t, int64(0), blockHeadLag(tracker, upsA))
		assert.Equal(t, int64(100), blockHeadLag(tracker, upsB),
			"healthy upstreams' lag recomputed against the corrected head")
		assert.Equal(t, int64(50), blockHeadLag(tracker, upsC))

		// Gauges follow the corrected values.
		assert.Equal(t, float64(32_000_100),
			promUtil.ToFloat64(tracker.getLatestBlockGauge(tracker.projectId, "*", upsA.NetworkLabel(), "*")))
		assert.Equal(t, float64(32_000_100),
			promUtil.ToFloat64(tracker.getLatestBlockGauge(tracker.projectId, upsA.VendorName(), upsA.NetworkLabel(), upsA.Id())))
		assert.Equal(t, float64(100),
			promUtil.ToFloat64(tracker.getHeadLagGauge(tracker.projectId, upsB.VendorName(), upsB.NetworkLabel(), upsB.Id())))
	})

	t.Run("LaggingUpstreamCannotDragNetworkHead", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-nodrag")
		upsA := common.NewFakeUpstream("a")
		upsB := common.NewFakeUpstream("b")
		net := upsA.NetworkId()

		tracker.SetLatestBlockNumber(upsA, 1_000_000, 0)

		// A far-behind upstream reporting forward progress of its own never
		// lowers the network head.
		tracker.SetLatestBlockNumber(upsB, 100, 0)
		tracker.SetLatestBlockNumber(upsB, 200, 0)
		assert.Equal(t, int64(1_000_000), networkLatest(tracker, net))
		assert.Equal(t, int64(1_000_000-200), blockHeadLag(tracker, upsB))

		// Even when upsB goes bogus-high and then corrects itself, the head is
		// re-derived from the remaining values (upsA's), NOT lowered to upsB's
		// corrected sample.
		tracker.SetLatestBlockNumber(upsB, 90_000_000, 0)
		assert.Equal(t, int64(90_000_000), networkLatest(tracker, net))
		tracker.SetLatestBlockNumber(upsB, 300, 0)
		assert.Equal(t, int64(300), upstreamLatest(tracker, upsB))
		assert.Equal(t, int64(1_000_000), networkLatest(tracker, net),
			"head re-derived to the healthy upstream's value, not the corrector's")
		assert.Equal(t, int64(0), blockHeadLag(tracker, upsA))
		assert.Equal(t, int64(1_000_000-300), blockHeadLag(tracker, upsB))
	})

	t.Run("LeaderReassertsAfterMistakenRollback", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-reassert")
		ups := common.NewFakeUpstream("a")

		tracker.SetLatestBlockNumber(ups, 5_000_000, 0)
		tracker.SetLatestBlockNumber(ups, 1_000, 0) // deep rollback accepted
		assert.Equal(t, int64(1_000), networkLatest(tracker, ups.NetworkId()))

		// If the rollback was wrong, the very next genuine observation
		// restores the head — the state is never pinned in either direction.
		tracker.SetLatestBlockNumber(ups, 5_000_001, 0)
		assert.Equal(t, int64(5_000_001), upstreamLatest(tracker, ups))
		assert.Equal(t, int64(5_000_001), networkLatest(tracker, ups.NetworkId()))
		assert.Equal(t, int64(0), blockHeadLag(tracker, ups))
	})
}

func TestTrackerFinalizedBlockRollbackTolerance(t *testing.T) {
	tolerance := int64(common.DefaultToleratedBlockHeadRollback)

	t.Run("SmallRollbackIgnored", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-fin-rollback-small")
		ups := common.NewFakeUpstream("a")

		tracker.SetFinalizedBlockNumber(ups, 10_000)
		tracker.SetFinalizedBlockNumber(ups, 10_000-tolerance)

		assert.Equal(t, int64(10_000), upstreamFinalized(tracker, ups))
		assert.Equal(t, int64(10_000), networkFinalized(tracker, ups.NetworkId()))
	})

	t.Run("BogusFinalizedCorrectedAndNetworkRederived", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-fin-rollback-rederive")
		upsA := common.NewFakeUpstream("a")
		upsB := common.NewFakeUpstream("b")
		net := upsA.NetworkId()

		tracker.SetFinalizedBlockNumber(upsB, 31_000_000)
		tracker.SetFinalizedBlockNumber(upsA, 99_000_000) // bogus

		assert.Equal(t, int64(99_000_000), networkFinalized(tracker, net))
		assert.Equal(t, int64(99_000_000-31_000_000), finalizationLag(tracker, upsB))

		tracker.SetFinalizedBlockNumber(upsA, 31_000_010)

		assert.Equal(t, int64(31_000_010), upstreamFinalized(tracker, upsA))
		assert.Equal(t, int64(31_000_010), networkFinalized(tracker, net))
		assert.Equal(t, int64(0), finalizationLag(tracker, upsA))
		assert.Equal(t, int64(10), finalizationLag(tracker, upsB))
	})
}

// After a head rollback the block-time EMA must keep sampling: its previous
// anchor may sit at the (bogus) old head, and without re-anchoring every
// subsequent block would look out-of-order until the chain re-passed it —
// freezing dynamic block time estimation indefinitely.
func TestTrackerBlockTimeReanchorsAfterHeadRollback(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-rollback-blocktime")
	ups := common.NewFakeUpstream("a")
	net := ups.NetworkId()
	base := int64(1_700_000_000)

	// Establish a ~1s/block EMA (4 observations → 3 samples).
	tracker.SetLatestBlockNumber(ups, 100, base)
	tracker.SetLatestBlockNumber(ups, 101, base+1)
	tracker.SetLatestBlockNumber(ups, 102, base+2)
	tracker.SetLatestBlockNumber(ups, 103, base+3)
	assert.InDelta(t, float64(time.Second), float64(tracker.GetNetworkBlockTime(net)), float64(50*time.Millisecond))

	// Bogus far-ahead sample moves the EMA anchor to the bogus height.
	tracker.SetLatestBlockNumber(ups, 90_000_000, base+4)
	// Correction rolls the head back (no EMA sample of its own).
	tracker.SetLatestBlockNumber(ups, 104, base+5)
	assert.Equal(t, int64(104), networkLatest(tracker, net))

	// Sampling resumes: the first forward observation re-anchors, the next one
	// produces a sample again (3s/block here), and the EMA starts moving.
	tracker.SetLatestBlockNumber(ups, 105, base+8)
	tracker.SetLatestBlockNumber(ups, 106, base+11)

	bt := tracker.GetNetworkBlockTime(net)
	assert.Greater(t, int64(bt), int64(1050*time.Millisecond),
		"EMA must move toward the new 3s/block cadence instead of staying frozen")
	assert.Less(t, int64(bt), int64(2*time.Second))
}

// A large head rollback is not just something to chart. Until it was recorded as
// a misbehaviour, RecordBlockHeadLargeRollback only set a gauge, so an upstream
// could roll its head back every few seconds and stay fully weighted -- its head
// kept voting in the served-tip ballot and it kept being picked to answer
// "latest" from whichever node pool it landed on.
//
// MisbehaviorsTotal is the signal the rest of eRPC already acts on: it is
// carried by routing.scoreMultipliers (`misbehaviors`) and visible to
// selectionPolicy evalFunc. Feeding rollbacks into it means one rare deep reorg
// barely moves the score while a flapping endpoint accumulates until the
// operator's own policy deprioritises or drops it -- the threshold stays in
// configuration rather than being compiled in here.
//
// Production shape, goldsky ECS edge 2026-08-18: one Dwellir endpoint on
// darwinia logged 610 large latest-head rollbacks in three hours (~3,341-block
// sawtooth every ~18s, seen identically from four deployments) while every other
// upstream in the fleet that hour recorded exactly one.
func TestTrackerBlockHeadLargeRollbackIsAMisbehavior(t *testing.T) {
	misbehaviors := func(tr *Tracker, ups common.Upstream, finality common.DataFinalityState) int64 {
		return tr.GetUpstreamMethodMetrics(ups, "*", finality).MisbehaviorsTotal.Load()
	}

	t.Run("a latest-head rollback counts against realtime", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-misbehavior-latest")
		ups := common.NewFakeUpstream("a")

		assert.Zero(t, misbehaviors(tracker, ups, common.DataFinalityStateRealtime))

		tracker.RecordBlockHeadLargeRollback(ups, "latest", 12_760_481, 12_757_135)

		assert.Equal(t, int64(1), misbehaviors(tracker, ups, common.DataFinalityStateRealtime),
			"a large head rollback must reach the signal routing and selectionPolicy already read")
	})

	t.Run("repeated rollbacks accumulate", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-misbehavior-repeat")
		ups := common.NewFakeUpstream("a")

		// A flapping endpoint separates itself from a one-off reorg by volume,
		// which is exactly what a rolling counter is for.
		for i := 0; i < 25; i++ {
			tracker.RecordBlockHeadLargeRollback(ups, "latest", 12_760_481, 12_757_135)
		}

		assert.Equal(t, int64(25), misbehaviors(tracker, ups, common.DataFinalityStateRealtime))
	})

	t.Run("the two head axes are stratified", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-rollback-misbehavior-axes")
		tracker.EnableFinalityTracking() // per-finality buckets only exist in 4-key mode
		ups := common.NewFakeUpstream("a")

		// prism-style upstreams correct their finalized boundary routinely while
		// their latest head is perfectly sound, so the two axes must land in their
		// own buckets and a policy can weigh them differently.
		tracker.RecordBlockHeadLargeRollback(ups, "finalized", 12_760_481, 12_757_135)
		for i := 0; i < 3; i++ {
			tracker.RecordBlockHeadLargeRollback(ups, "latest", 12_760_481, 12_757_135)
		}

		assert.Equal(t, int64(1), misbehaviors(tracker, ups, common.DataFinalityStateFinalized),
			"the finalized axis carries only its own rollback")
		assert.Equal(t, int64(3), misbehaviors(tracker, ups, common.DataFinalityStateRealtime),
			"the latest axis carries only its own rollbacks")
		assert.Equal(t, int64(4), misbehaviors(tracker, ups, common.DataFinalityStateAll),
			"the all-finalities rollup carries both")
	})
}
