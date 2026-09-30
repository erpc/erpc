package health

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

// These tests pin how the time between an upstream's head observations is
// charged. Within the observation window an upstream is measured against the
// network head as it stood when it was last observed; see
// alignedBlocksBehind. The poller-driven scenarios live in
// architecture/evm/evm_state_poller_head_lag_test.go.

func TestTrackerQuietUpstreamIsMeasuredAgainstCurrentHeadAfterObservationWindow(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-obs-window")
	a, b, quiet := common.NewFakeUpstream("a"), common.NewFakeUpstream("b"), common.NewFakeUpstream("quiet")
	const refresh = 50 * time.Millisecond
	tracker.SetHeadRefreshInterval(quiet, refresh)

	tracker.SetLatestBlockNumber(a, 100, 0)
	tracker.SetLatestBlockNumber(b, 100, 0)
	tracker.SetLatestBlockNumber(quiet, 100, 0)
	for n := int64(101); n <= 130; n++ {
		tracker.SetLatestBlockNumber(a, n, 0)
		tracker.SetLatestBlockNumber(b, n, 0)
	}
	assert.Equal(t, int64(0), blockHeadLag(tracker, quiet),
		"within the window, blocks produced since the last observation are not lag")

	// No observation for longer than the window: its polls are failing. Every
	// block since the last observation counts from the next head change on.
	time.Sleep(headObservationWindow*refresh + refresh)
	tracker.SetLatestBlockNumber(a, 131, 0)
	tracker.SetLatestBlockNumber(b, 131, 0)
	assert.Equal(t, int64(31), blockHeadLag(tracker, quiet))
}

func TestTrackerAlignedLagDropsWhenNetworkHeadIsReDerivedLower(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-obs-rederive")
	a, b, polled := common.NewFakeUpstream("a"), common.NewFakeUpstream("b"), common.NewFakeUpstream("polled")
	tracker.SetHeadRefreshInterval(polled, 30*time.Second)

	tracker.SetLatestBlockNumber(a, 1_000_000, 0)
	tracker.SetLatestBlockNumber(b, 1_000_000, 0)
	tracker.SetLatestBlockNumber(polled, 1_000_000, 0)

	// Both served upstreams briefly answer for another chain; the bogus head
	// is corroborated, and the next poll measures the polled upstream against it.
	tracker.SetLatestBlockNumber(a, 5_000_000, 0)
	tracker.SetLatestBlockNumber(b, 5_000_000, 0)
	tracker.SetLatestBlockNumber(polled, 1_000_001, 0)
	assert.Equal(t, int64(3_999_999), blockHeadLag(tracker, polled))

	// Their next real reports correct the network head downward. The polled
	// upstream's lag follows at once instead of waiting for its next poll.
	tracker.SetLatestBlockNumber(a, 1_000_010, 0)
	tracker.SetLatestBlockNumber(b, 1_000_010, 0)
	assert.Equal(t, int64(9), blockHeadLag(tracker, polled))
}

func TestTrackerFinalizedHeadAgeBetweenPollsIsNotFinalizationLag(t *testing.T) {
	tracker := newRollbackTestTracker(t, "test-obs-finalized")
	a, b, polled := common.NewFakeUpstream("a"), common.NewFakeUpstream("b"), common.NewFakeUpstream("polled")
	tracker.SetHeadRefreshInterval(polled, 30*time.Second)

	fin := int64(1_000)
	for step := range 61 {
		fin++
		tracker.SetFinalizedBlockNumber(a, fin)
		tracker.SetFinalizedBlockNumber(b, fin)
		if step%30 == 0 {
			tracker.SetFinalizedBlockNumber(polled, fin)
		}
		assert.Equal(t, int64(0), finalizationLag(tracker, polled), "step %d", step)
	}

	// A poll that finds it behind is lag.
	for range 30 {
		fin++
		tracker.SetFinalizedBlockNumber(a, fin)
		tracker.SetFinalizedBlockNumber(b, fin)
	}
	tracker.SetFinalizedBlockNumber(polled, fin-20)
	assert.Equal(t, int64(20), finalizationLag(tracker, polled))
}
