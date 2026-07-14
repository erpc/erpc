package health

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// These tests pin the block-head / finalization LAG reference used by the
// selection-policy predicates (blockNumberLagAbove / blockSecondsLagAbove). The
// reference is the "corroborated freshest" head — the SECOND-highest positive
// head across the network's upstreams — NOT the raw network maximum. The raw
// maximum let a single most-ahead upstream (a cross-wired / poisoned node
// briefly reporting another chain's much-higher height) inflate every honest
// upstream's lag past the exclusion threshold, so the whole healthy majority got
// evicted and routing collapsed onto the poisoned node. See
// Tracker.networkLagReference and evm.ServedTipPick.Freshest.

// evictionThreshold is a representative blockNumberLagAbove(N) bound; the tests
// assert honest upstreams stay well under it and genuine laggards stay over it.
const evictionThreshold = int64(16)

func newLagTracker(t *testing.T, projectID string) *Tracker {
	return newRollbackTestTracker(t, projectID)
}

// TestLagReference_LoneFutureOutlierDoesNotEvictMajority reproduces the incident:
// seven honest sepolia-height upstreams plus one poisoned upstream reporting a
// far-higher (mainnet) height. The honest majority must NOT be marked as
// lagging.
func TestLagReference_LoneFutureOutlierDoesNotEvictMajority(t *testing.T) {
	tracker := newLagTracker(t, "test-lagref-incident")

	const sepolia = int64(44_122_190)
	const mainnetPoison = int64(48_590_164) // ~4.47M blocks ahead — another chain

	honest := make([]common.Upstream, 7)
	for i := range honest {
		honest[i] = common.NewFakeUpstream(string(rune('a' + i)))
		tracker.SetLatestBlockNumber(honest[i], sepolia, 0)
	}
	// The poison arrives LAST and advances the raw network max, forcing a global
	// lag recompute across every upstream.
	poison := common.NewFakeUpstream("poison")
	tracker.SetLatestBlockNumber(poison, mainnetPoison, 0)

	// Raw per-upstream max is still observable (unchanged behavior).
	assert.Equal(t, mainnetPoison, networkLatest(tracker, poison.NetworkId()),
		"raw network max still reflects the most-ahead upstream")

	// The corroborated reference is the honest majority height, so every honest
	// upstream reads ~zero lag and stays eligible.
	for _, u := range honest {
		lag := blockHeadLag(tracker, u)
		assert.Equal(t, int64(0), lag, "honest upstream %s must not be marked as lagging", u.Id())
		assert.Less(t, lag, evictionThreshold)
	}
	// The lone outlier is not itself excluded (its negative lag is clamped to 0)
	// — harmless; the point is it can no longer evict the majority.
	assert.Equal(t, int64(0), blockHeadLag(tracker, poison))
}

// TestLagReference_GenuineLaggardStillExcluded proves the fix does not
// under-exclude: an upstream genuinely behind the majority still shows a large
// lag and stays excludable.
func TestLagReference_GenuineLaggardStillExcluded(t *testing.T) {
	tracker := newLagTracker(t, "test-lagref-laggard")

	a := common.NewFakeUpstream("a")
	b := common.NewFakeUpstream("b")
	c := common.NewFakeUpstream("c")
	stale := common.NewFakeUpstream("stale")

	tracker.SetLatestBlockNumber(a, 1000, 0)
	tracker.SetLatestBlockNumber(b, 1000, 0)
	tracker.SetLatestBlockNumber(c, 1000, 0)
	tracker.SetLatestBlockNumber(stale, 200, 0)

	assert.Equal(t, int64(0), blockHeadLag(tracker, a))
	assert.Equal(t, int64(0), blockHeadLag(tracker, b))
	assert.Equal(t, int64(800), blockHeadLag(tracker, stale))
	assert.Greater(t, blockHeadLag(tracker, stale), evictionThreshold)
}

// TestLagReference_OutlierAndLaggardTogether is the decisive case: a poison
// outlier AND a genuine laggard in the same network. The outlier must not evict
// the healthy majority, and the laggard must still be caught.
func TestLagReference_OutlierAndLaggardTogether(t *testing.T) {
	tracker := newLagTracker(t, "test-lagref-both")

	a := common.NewFakeUpstream("a")
	b := common.NewFakeUpstream("b")
	c := common.NewFakeUpstream("c")
	stale := common.NewFakeUpstream("stale")
	poison := common.NewFakeUpstream("poison")

	tracker.SetLatestBlockNumber(a, 1000, 0)
	tracker.SetLatestBlockNumber(b, 1000, 0)
	tracker.SetLatestBlockNumber(c, 1000, 0)
	tracker.SetLatestBlockNumber(stale, 200, 0)
	tracker.SetLatestBlockNumber(poison, 999_999, 0) // last → global recompute

	// Healthy majority: not lagging.
	assert.Equal(t, int64(0), blockHeadLag(tracker, a))
	assert.Equal(t, int64(0), blockHeadLag(tracker, b))
	assert.Equal(t, int64(0), blockHeadLag(tracker, c))
	// Genuine laggard: still excludable (reference is the majority 1000).
	assert.Equal(t, int64(800), blockHeadLag(tracker, stale))
	assert.Greater(t, blockHeadLag(tracker, stale), evictionThreshold)
	// Outlier: clamped to 0, cannot evict others.
	assert.Equal(t, int64(0), blockHeadLag(tracker, poison))
}

// TestLagReference_SingleUpstreamNeverLags: with one upstream there is no
// reference to compare against — it can never be marked as lagging.
func TestLagReference_SingleUpstreamNeverLags(t *testing.T) {
	tracker := newLagTracker(t, "test-lagref-single")
	a := common.NewFakeUpstream("a")
	tracker.SetLatestBlockNumber(a, 1000, 0)
	assert.Equal(t, int64(0), blockHeadLag(tracker, a))
}

// TestLagReference_TwoUpstreamsFallBackToMax documents the sub-quorum behavior:
// with fewer than minLagReferenceUpstreams upstreams there is no second opinion
// to appeal to, so the raw maximum is kept. A genuine laggard is still
// detected; a lone outlier is NOT yet protected against — that protection
// needs a real quorum (>= 3 upstreams), exercised by the other tests here.
func TestLagReference_TwoUpstreamsFallBackToMax(t *testing.T) {
	t.Run("genuineLaggardDetected", func(t *testing.T) {
		tracker := newLagTracker(t, "test-lagref-two-laggard")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		tracker.SetLatestBlockNumber(a, 1000, 0)
		tracker.SetLatestBlockNumber(b, 990, 0)
		assert.Equal(t, int64(0), blockHeadLag(tracker, a))
		assert.Equal(t, int64(10), blockHeadLag(tracker, b), "with 2 upstreams the max is still the reference")
	})
	t.Run("outlierNotYetProtectedBelowQuorum", func(t *testing.T) {
		tracker := newLagTracker(t, "test-lagref-two-poison")
		a := common.NewFakeUpstream("a")
		poison := common.NewFakeUpstream("poison")
		tracker.SetLatestBlockNumber(a, 1000, 0)
		tracker.SetLatestBlockNumber(poison, 999_999, 0)
		assert.Equal(t, int64(998_999), blockHeadLag(tracker, a),
			"below quorum the outlier still defines the reference (known limitation)")
	})
}

// TestLagReference_FinalizationLagCorroborated mirrors the protection on the
// finalized axis (blockSecondsLagAbove reads FinalizationLag).
func TestLagReference_FinalizationLagCorroborated(t *testing.T) {
	tracker := newLagTracker(t, "test-lagref-finalized")

	honest := make([]common.Upstream, 3)
	for i := range honest {
		honest[i] = common.NewFakeUpstream(string(rune('a' + i)))
		tracker.SetFinalizedBlockNumber(honest[i], 5000)
	}
	poison := common.NewFakeUpstream("poison")
	tracker.SetFinalizedBlockNumber(poison, 9_000_000)

	for _, u := range honest {
		assert.Equal(t, int64(0), finalizationLag(tracker, u), "honest upstream %s finalization lag must stay 0", u.Id())
	}
	assert.Equal(t, int64(0), finalizationLag(tracker, poison))

	// A genuinely behind upstream is still caught on the finalized axis.
	behind := common.NewFakeUpstream("behind")
	tracker.SetFinalizedBlockNumber(behind, 4000)
	assert.Equal(t, int64(1000), finalizationLag(tracker, behind))
}

// TestNetworkLagReference_OrderStatistic pins the pure order-statistic of the
// reference across fan-outs and ties — the second-highest positive head at or
// above the corroboration quorum (N >= 3), the supplied max below it, and the
// supplied max when there are no positive heads.
func TestNetworkLagReference_OrderStatistic(t *testing.T) {
	getLatest := func(m *NetworkMetadata) int64 { return m.evmLatestBlockNumber.Load() }

	cases := []struct {
		name   string
		heads  []int64
		expect int64
	}{
		{"single", []int64{100}, 100},
		{"two_below_quorum_uses_max", []int64{200, 100}, 200},
		{"three_small_spread_uses_second", []int64{102, 101, 100}, 101},
		{"lone_rogue_ignored", []int64{999_999_999, 101, 100}, 101},
		{"eight_incident_shape", []int64{48_590_164, 44_122_190, 44_122_190, 44_122_190, 44_122_190, 44_122_190, 44_122_190, 44_122_190}, 44_122_190},
		{"all_equal", []int64{100, 100, 100}, 100},
		// Two agreeing rogues corroborate each other — inherent to height-only
		// corroboration; the second-highest is still one of the rogues.
		{"two_agreeing_rogues_corroborate", []int64{999_999_999, 999_999_998, 300, 200, 100}, 999_999_998},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := newLagTracker(t, "test-orderstat-"+tc.name)
			net := ""
			var rawMax int64
			for i, h := range tc.heads {
				u := common.NewFakeUpstream("u" + string(rune('0'+i)))
				net = u.NetworkId()
				tracker.SetLatestBlockNumber(u, h, 0)
				if h > rawMax {
					rawMax = h
				}
			}
			got := tracker.networkLagReference(net, nil, getLatest, rawMax)
			assert.Equal(t, tc.expect, got)
		})
	}

	t.Run("no_positive_heads_falls_back_to_max", func(t *testing.T) {
		tracker := newLagTracker(t, "test-orderstat-empty")
		// Nothing recorded for this network → fall back to the supplied max.
		assert.Equal(t, int64(12345), tracker.networkLagReference("evm:123", nil, getLatest, 12345))
	})
}
