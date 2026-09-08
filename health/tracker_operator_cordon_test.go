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

func TestOperatorCordon_LookupPrecedence(t *testing.T) {
	tracker := NewTracker(&log.Logger, "op-project", 2*time.Second)
	ups := common.NewFakeUpstream("op-upstream")

	require.False(t, tracker.IsCordoned(ups, "eth_call"))

	tracker.SetOperatorCordon(ups, "incident", 1)
	assert.True(t, tracker.IsCordoned(ups, "eth_call"), "operator cordon covers every method")
	assert.True(t, tracker.IsCordoned(ups, "*"))
	reason, ok := tracker.CordonedReason(ups, "eth_getLogs")
	require.True(t, ok)
	assert.Equal(t, "incident", reason)

	// Automatic cordon on the same upstream: operator reason wins; lifting
	// the automatic one leaves the operator cordon and vice versa.
	tracker.Cordon(ups, "*", "consensus sit-out")
	reason, _ = tracker.CordonedReason(ups, "*")
	assert.Equal(t, "incident", reason)
	tracker.Uncordon(ups, "*", "penalty over")
	assert.True(t, tracker.IsCordoned(ups, "*"), "detector cannot lift the operator cordon")

	tracker.Cordon(ups, "*", "consensus sit-out")
	tracker.SetOperatorCordon(ups, "", 0)
	reason, ok = tracker.CordonedReason(ups, "*")
	require.True(t, ok)
	assert.Equal(t, "consensus sit-out", reason, "operator cordon lifted, detector cordon remains")
	tracker.Uncordon(ups, "*", "penalty over")
	assert.False(t, tracker.IsCordoned(ups, "*"))

	// Method-scoped cordons stay on their method.
	tracker.Cordon(ups, "eth_getLogs", "slow logs")
	assert.True(t, tracker.IsCordoned(ups, "eth_getLogs"))
	assert.False(t, tracker.IsCordoned(ups, "eth_call"))
}

func TestOperatorCordon_MetricsFollowEdges(t *testing.T) {
	tracker := NewTracker(&log.Logger, "metric-project", 2*time.Second)
	ups := common.NewFakeUpstream("metric-upstream")
	cordonCounter := telemetry.MetricUpstreamCordonEventTotal.WithLabelValues("metric-project", ups.NetworkId(), ups.Id(), "cordon")
	uncordonCounter := telemetry.MetricUpstreamCordonEventTotal.WithLabelValues("metric-project", ups.NetworkId(), ups.Id(), "uncordon")
	beforeCordon := promUtil.ToFloat64(cordonCounter)
	beforeUncordon := promUtil.ToFloat64(uncordonCounter)

	startMs := time.Now().Add(-30 * time.Second).UnixMilli()
	tracker.SetOperatorCordon(ups, "incident", startMs)
	assert.Equal(t, beforeCordon+1, promUtil.ToFloat64(cordonCounter))
	assert.Equal(t, float64(1), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident")))

	// Same value again (idempotent re-cordon): no new event.
	tracker.SetOperatorCordon(ups, "incident", startMs)
	assert.Equal(t, beforeCordon+1, promUtil.ToFloat64(cordonCounter))

	// Reason edit: old series drops to 0, new series is 1, no new event.
	tracker.SetOperatorCordon(ups, "incident (extended)", startMs)
	assert.Equal(t, beforeCordon+1, promUtil.ToFloat64(cordonCounter))
	assert.Equal(t, float64(0), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident")))
	assert.Equal(t, float64(1), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident (extended)")))

	histBefore := promUtil.CollectAndCount(telemetry.MetricUpstreamCordonDurationSeconds)
	tracker.SetOperatorCordon(ups, "", 0)
	assert.Equal(t, beforeUncordon+1, promUtil.ToFloat64(uncordonCounter))
	assert.Equal(t, float64(0), promUtil.ToFloat64(tracker.getCordonedGauge(ups, "*", "incident (extended)")))
	assert.GreaterOrEqual(t, promUtil.CollectAndCount(telemetry.MetricUpstreamCordonDurationSeconds), histBefore)

	// Lifting when nothing is held is a no-op.
	tracker.SetOperatorCordon(ups, "", 0)
	assert.Equal(t, beforeUncordon+1, promUtil.ToFloat64(uncordonCounter))
}
