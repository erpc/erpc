package health

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func snapshot(version int64, cordons common.ProjectCordons) *common.CordonSnapshot {
	return &common.CordonSnapshot{Version: version, Cordons: cordons}
}

func TestOperatorCordons_LookupPrecedence(t *testing.T) {
	tracker := NewTracker(&log.Logger, "op-project", 2*time.Second)
	ups := common.NewFakeUpstream("op-upstream")
	resolve := func(string) common.Upstream { return ups }

	require.False(t, tracker.IsCordoned(ups, "eth_call"))

	require.True(t, tracker.SetOperatorCordons(snapshot(1, common.ProjectCordons{
		"op-upstream": {"eth_getLogs": {Reason: "slow logs", CordonedAtMs: 1}},
	}), resolve))
	assert.True(t, tracker.IsCordoned(ups, "eth_getLogs"))
	assert.False(t, tracker.IsCordoned(ups, "eth_call"), "method cordon stays on its method")
	assert.False(t, tracker.IsCordoned(ups, "*"))

	require.True(t, tracker.SetOperatorCordons(snapshot(2, common.ProjectCordons{
		"op-upstream": {"eth_getLogs": {Reason: "slow logs", CordonedAtMs: 1}, "*": {Reason: "incident", CordonedAtMs: 2}},
	}), resolve))
	reason, ok := tracker.CordonedReason(ups, "eth_getLogs")
	require.True(t, ok)
	assert.Equal(t, "incident", reason, "wildcard shadows the method scope")
	assert.True(t, tracker.IsCordoned(ups, "eth_call"))

	// Automatic cordon on the same cell: operator reason wins; lifting the
	// automatic one leaves the operator cordon in place and vice versa.
	tracker.Cordon(ups, "*", "consensus sit-out")
	reason, _ = tracker.CordonedReason(ups, "*")
	assert.Equal(t, "incident", reason)
	tracker.Uncordon(ups, "*", "penalty over")
	assert.True(t, tracker.IsCordoned(ups, "*"))

	tracker.Cordon(ups, "*", "consensus sit-out")
	require.True(t, tracker.SetOperatorCordons(snapshot(3, common.ProjectCordons{}), resolve))
	reason, ok = tracker.CordonedReason(ups, "*")
	require.True(t, ok)
	assert.Equal(t, "consensus sit-out", reason, "operator layer cleared, detector cordon remains")
	tracker.Uncordon(ups, "*", "penalty over")
	assert.False(t, tracker.IsCordoned(ups, "*"))
}

func TestOperatorCordons_VersionOrdering(t *testing.T) {
	tracker := NewTracker(&log.Logger, "ver-project", 2*time.Second)
	ups := common.NewFakeUpstream("ver-upstream")
	resolve := func(string) common.Upstream { return ups }
	cordoned := common.ProjectCordons{"ver-upstream": {"*": {Reason: "incident", CordonedAtMs: 1}}}

	require.True(t, tracker.SetOperatorCordons(snapshot(5, cordoned), resolve))
	require.False(t, tracker.SetOperatorCordons(snapshot(5, common.ProjectCordons{}), resolve), "same version is a no-op")
	require.False(t, tracker.SetOperatorCordons(snapshot(4, common.ProjectCordons{}), resolve), "older version is ignored")
	assert.True(t, tracker.IsCordoned(ups, "*"))
	require.True(t, tracker.SetOperatorCordons(snapshot(0, common.ProjectCordons{}), resolve), "version 0 is a reset")
	assert.False(t, tracker.IsCordoned(ups, "*"))
	require.False(t, tracker.SetOperatorCordons(nil, resolve))
}

func TestOperatorCordons_MetricsFollowSnapshotEdges(t *testing.T) {
	tracker := NewTracker(&log.Logger, "metric-project", 2*time.Second)
	ups := common.NewFakeUpstream("metric-upstream")
	resolve := func(string) common.Upstream { return ups }
	cordonCounter := telemetry.MetricUpstreamCordonEventTotal.WithLabelValues("metric-project", ups.NetworkId(), ups.Id(), "cordon")
	uncordonCounter := telemetry.MetricUpstreamCordonEventTotal.WithLabelValues("metric-project", ups.NetworkId(), ups.Id(), "uncordon")
	beforeCordon := promUtil.ToFloat64(cordonCounter)
	beforeUncordon := promUtil.ToFloat64(uncordonCounter)

	startMs := time.Now().Add(-30 * time.Second).UnixMilli()
	require.True(t, tracker.SetOperatorCordons(snapshot(1, common.ProjectCordons{
		"metric-upstream": {"*": {Reason: "incident", CordonedAtMs: startMs}},
	}), resolve))
	assert.Equal(t, beforeCordon+1, promUtil.ToFloat64(cordonCounter))
	assert.Equal(t, float64(1), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident")))

	// Reason edit: old series drops to 0, new series is 1, no new event.
	require.True(t, tracker.SetOperatorCordons(snapshot(2, common.ProjectCordons{
		"metric-upstream": {"*": {Reason: "incident (extended)", CordonedAtMs: startMs}},
	}), resolve))
	assert.Equal(t, beforeCordon+1, promUtil.ToFloat64(cordonCounter))
	assert.Equal(t, float64(0), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident")))
	assert.Equal(t, float64(1), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident (extended)")))

	histBefore := promUtil.CollectAndCount(telemetry.MetricUpstreamCordonDurationSeconds)
	require.True(t, tracker.SetOperatorCordons(snapshot(3, common.ProjectCordons{}), resolve))
	assert.Equal(t, beforeUncordon+1, promUtil.ToFloat64(uncordonCounter))
	assert.Equal(t, float64(0), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident (extended)")))
	assert.GreaterOrEqual(t, promUtil.CollectAndCount(telemetry.MetricUpstreamCordonDurationSeconds), histBefore,
		"duration observed from the persisted start on the last edge")
}

func TestOperatorCordons_GaugeForLateRegisteredUpstream(t *testing.T) {
	tracker := NewTracker(&log.Logger, "late-project", 2*time.Second)
	ups := common.NewFakeUpstream("late-upstream")

	// Snapshot arrives before the upstream exists on this replica.
	require.True(t, tracker.SetOperatorCordons(snapshot(1, common.ProjectCordons{
		"late-upstream": {"*": {Reason: "incident", CordonedAtMs: 1}},
	}), func(string) common.Upstream { return nil }))
	assert.True(t, tracker.IsCordoned(ups, "*"), "cordon applies by id before registration")
	assert.Equal(t, float64(0), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident")))

	tracker.RefreshOperatorCordonGauges(ups)
	assert.Equal(t, float64(1), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident")))
}
