package health

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// The network head is corroborated: the second-highest reporter once two
// upstreams have reported, the only head while one has. A lone far-ahead report
// therefore never becomes the reference the honest upstreams are measured
// against.

func networkLatestGauge(t *Tracker, ups common.Upstream) float64 {
	return promUtil.ToFloat64(t.getLatestBlockGauge(t.projectId, "*", ups.NetworkLabel(), "*"))
}

func upstreamLatestGauge(t *Tracker, ups common.Upstream) float64 {
	return promUtil.ToFloat64(t.getLatestBlockGauge(t.projectId, ups.VendorName(), ups.NetworkLabel(), ups.Id()))
}

func networkLatestTimestamp(t *Tracker, net string) int64 {
	return t.getMetadata(metadataKey{nil, net}).evmLatestBlockTimestamp.Load()
}

type blockingIDUpstream struct {
	common.Upstream
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func (u *blockingIDUpstream) Id() string {
	if u.armed.CompareAndSwap(true, false) {
		close(u.reached)
		<-u.release
	}
	return u.Upstream.Id()
}

func TestTrackerCorroboratedHeadConcurrentUpdatesDoNotRegress(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-corr-concurrent")
	a := common.NewFakeUpstream("a")
	b := common.NewFakeUpstream("b")
	c := &blockingIDUpstream{
		Upstream: common.NewFakeUpstream("c"),
		reached:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	d := common.NewFakeUpstream("d")
	net := a.NetworkId()
	upstreams := []common.Upstream{a, b, c, d}

	ntwMeta := tracker.getMetadata(metadataKey{nil, net})
	ntwMeta.reporters = upstreams
	for i, head := range []int64{99, 100, 99, 99} {
		tracker.getMetadata(metadataKey{upstreams[i], net}).evmLatestBlockNumber.Store(head)
	}
	ntwMeta.evmLatestBlockNumber.Store(99)

	c.armed.Store(true)
	firstDone := make(chan struct{})
	go func() {
		tracker.SetLatestBlockNumber(a, 101, 0)
		close(firstDone)
	}()
	<-c.reached

	secondDone := make(chan struct{})
	go func() {
		tracker.SetLatestBlockNumber(b, 102, 0)
		close(secondDone)
	}()

	close(c.release)
	<-firstDone
	<-secondDone
	assert.Equal(t, int64(101), networkLatest(tracker, net))
}

func TestTrackerCorroboratedHeadIncludesPollersWithoutRequestMetrics(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-corr-reporters")
	a := common.NewFakeUpstream("a")
	b := common.NewFakeUpstream("b")
	outlier := common.NewFakeUpstream("outlier")
	net := a.NetworkId()

	tracker.getUpsMetrics(upstreamKey{ups: outlier, method: "eth_call", finality: common.DataFinalityStateAll})
	tracker.SetLatestBlockNumber(a, 100, 0)
	tracker.SetLatestBlockNumber(b, 101, 0)
	tracker.SetLatestBlockNumber(outlier, 1_000_000, 0)

	assert.Equal(t, int64(101), networkLatest(tracker, net))
}

func TestTrackerCorroboratedHeadCarriesItsReportersTimestamp(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-corr-block-time")
	a := common.NewFakeUpstream("a")
	b := common.NewFakeUpstream("b")
	net := a.NetworkId()

	tracker.SetLatestBlockNumber(a, 100, 1_800_000_000)
	tracker.SetLatestBlockNumber(b, 101, 1_800_000_001)
	tracker.SetLatestBlockNumber(a, 102, 1_800_000_002)
	tracker.SetLatestBlockNumber(b, 103, 1_800_000_003)
	tracker.SetLatestBlockNumber(a, 104, 1_800_000_004)

	assert.Equal(t, int64(103), networkLatest(tracker, net))
	assert.Equal(t, int64(1_800_000_003), networkLatestTimestamp(tracker, net))
	assert.Equal(t, time.Second, tracker.GetNetworkBlockTime(net))
}

func TestTrackerCorroboratedLatestHead(t *testing.T) {
	t.Run("TwoUpstreams_LoneHighReportDoesNotSetHead", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-two")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		net := a.NetworkId()

		tracker.SetLatestBlockNumber(a, 100, 0)
		tracker.SetLatestBlockNumber(b, 1_000_000, 0)

		assert.Equal(t, int64(100), networkLatest(tracker, net))
		assert.Equal(t, int64(0), blockHeadLag(tracker, a))
		assert.Equal(t, int64(0), blockHeadLag(tracker, b))
		assert.Equal(t, float64(100), networkLatestGauge(tracker, a))
		assert.Equal(t, float64(1_000_000), upstreamLatestGauge(tracker, b),
			"the raw per-upstream gauge still exposes the outlier")
	})

	t.Run("ThreeUpstreams_LoneHighOutlierIgnored", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-three")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		c := common.NewFakeUpstream("c")
		net := a.NetworkId()

		tracker.SetLatestBlockNumber(a, 100, 0)
		tracker.SetLatestBlockNumber(b, 101, 0)
		tracker.SetLatestBlockNumber(c, 1_000_000, 0)

		assert.Equal(t, int64(101), networkLatest(tracker, net))
		assert.Equal(t, int64(1), blockHeadLag(tracker, a))
		assert.Equal(t, int64(0), blockHeadLag(tracker, b))
		assert.Equal(t, int64(0), blockHeadLag(tracker, c))
	})

	t.Run("ThreeUpstreams_StaleReporterCannotHoldHeadBack", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-stale")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		c := common.NewFakeUpstream("c")
		net := a.NetworkId()

		tracker.SetLatestBlockNumber(a, 1000, 0)
		tracker.SetLatestBlockNumber(b, 999, 0)
		tracker.SetLatestBlockNumber(c, 100, 0)

		assert.Equal(t, int64(999), networkLatest(tracker, net))
		assert.Equal(t, int64(0), blockHeadLag(tracker, a), "ahead of the corroborated head is not lag")
		assert.Equal(t, int64(0), blockHeadLag(tracker, b))
		assert.Equal(t, int64(899), blockHeadLag(tracker, c))
	})

	t.Run("PartialStartup_OutlierReportsFirst", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-startup")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		c := common.NewFakeUpstream("c")
		net := a.NetworkId()

		tracker.SetLatestBlockNumber(c, 1_000_000, 0)
		assert.Equal(t, int64(1_000_000), networkLatest(tracker, net), "sole reporter is the head")

		tracker.SetLatestBlockNumber(a, 100, 0)
		assert.Equal(t, int64(100), networkLatest(tracker, net), "second reporter corroborates a lower head")
		assert.Equal(t, int64(0), blockHeadLag(tracker, a))
		assert.Equal(t, int64(0), blockHeadLag(tracker, c))

		tracker.SetLatestBlockNumber(b, 101, 0)
		assert.Equal(t, int64(101), networkLatest(tracker, net))
		assert.Equal(t, int64(1), blockHeadLag(tracker, a))
		assert.Equal(t, int64(0), blockHeadLag(tracker, b))
		assert.Equal(t, int64(0), blockHeadLag(tracker, c))
	})

	t.Run("OutlierCorrectionRederivesHead", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-correct")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		c := common.NewFakeUpstream("c")
		net := a.NetworkId()

		tracker.SetLatestBlockNumber(a, 100, 0)
		tracker.SetLatestBlockNumber(b, 101, 0)
		tracker.SetLatestBlockNumber(c, 1_000_000, 0)
		tracker.SetLatestBlockNumber(c, 102, 0)

		assert.Equal(t, int64(102), upstreamLatest(tracker, c))
		assert.Equal(t, int64(101), networkLatest(tracker, net))
		assert.Equal(t, int64(1), blockHeadLag(tracker, a))
		assert.Equal(t, int64(0), blockHeadLag(tracker, b))
		assert.Equal(t, int64(0), blockHeadLag(tracker, c))
	})

	t.Run("TimestampFollowsCorroboratedHead", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-timestamp")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		net := a.NetworkId()
		const honestTs = int64(1_800_000_000)
		const outlierTs = honestTs - 181*24*3600

		tracker.SetLatestBlockNumber(a, 100, honestTs)
		tracker.SetLatestBlockNumber(b, 1_000_000, outlierTs)
		assert.Equal(t, honestTs, networkLatestTimestamp(tracker, net),
			"the outlier's timestamp never becomes the network timestamp")

		tracker.SetLatestBlockNumber(a, 101, honestTs+1)
		assert.Equal(t, int64(101), networkLatest(tracker, net))
		assert.Equal(t, honestTs+1, networkLatestTimestamp(tracker, net),
			"the timestamp advances with the sample that became the head")
	})

	t.Run("FleetWithOneWrongChainUpstreamKeepsEveryoneEligible", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-fleet")
		net := ""
		honest := make([]common.Upstream, 0, 8)
		for _, id := range []string{"h1", "h2", "h3", "h4", "h5", "h6", "h7", "h8"} {
			ups := common.NewFakeUpstream(id)
			net = ups.NetworkId()
			honest = append(honest, ups)
			tracker.SetLatestBlockNumber(ups, 32_610_710, 0)
		}
		rogue := common.NewFakeUpstream("rogue")
		tracker.SetLatestBlockNumber(rogue, 62_381_379, 0)

		assert.Equal(t, int64(32_610_710), networkLatest(tracker, net))
		for _, ups := range honest {
			assert.Equal(t, int64(0), blockHeadLag(tracker, ups), ups.Id())
		}
		assert.Equal(t, int64(0), blockHeadLag(tracker, rogue))

		for _, ups := range honest {
			tracker.SetLatestBlockNumber(ups, 32_610_720, 0)
		}
		assert.Equal(t, int64(32_610_720), networkLatest(tracker, net))
	})
}

func TestTrackerCorroboratedFinalizedHead(t *testing.T) {
	t.Run("ThreeUpstreams_LoneHighOutlierIgnored", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-fin")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		c := common.NewFakeUpstream("c")
		net := a.NetworkId()

		tracker.SetFinalizedBlockNumber(a, 31_000_000)
		tracker.SetFinalizedBlockNumber(b, 31_000_010)
		tracker.SetFinalizedBlockNumber(c, 99_000_000)

		assert.Equal(t, int64(31_000_010), networkFinalized(tracker, net))
		assert.Equal(t, int64(10), finalizationLag(tracker, a))
		assert.Equal(t, int64(0), finalizationLag(tracker, b))
		assert.Equal(t, int64(0), finalizationLag(tracker, c))
	})

	t.Run("TwoUpstreams_LoneHighReportDoesNotSetHead", func(t *testing.T) {
		tracker := newRollbackTestTracker(t, "test-corr-fin-two")
		a := common.NewFakeUpstream("a")
		b := common.NewFakeUpstream("b")
		net := a.NetworkId()

		tracker.SetFinalizedBlockNumber(a, 100)
		tracker.SetFinalizedBlockNumber(b, 1_000_000)

		assert.Equal(t, int64(100), networkFinalized(tracker, net))
		assert.Equal(t, int64(0), finalizationLag(tracker, a))
		assert.Equal(t, int64(0), finalizationLag(tracker, b))
	})
}
