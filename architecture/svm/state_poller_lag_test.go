package svm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// The tracker records slot lag when an upstream's slot is observed, and the
// counter's OnValue fires only when the slot advances. A node stuck at one
// slot must still have its lag recorded on each poll, against the network
// slot of that moment.
func TestSvmStatePoller_Poll_RecordsLagOfStuckSlot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tracker := health.NewTracker(&log.Logger, "test", time.Minute)
	cfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "1MB"},
		},
	}
	cfg.SetDefaults("test")
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, cfg)
	require.NoError(t, err)

	stuck := newScriptedUpstream()
	scriptAllFour(stuck) // processed slot 1000, finalized 990, forever
	p := NewSvmStatePoller("test", ctx, &log.Logger, stuck, tracker, ssr)

	peerA, peerB := upstreamAt("peer-a", 0), upstreamAt("peer-b", 0)
	slotLag := func() int64 {
		return tracker.GetUpstreamMethodMetrics(stuck, "*", common.DataFinalityStateAll).BlockHeadLag.Load()
	}
	finalizationLag := func() int64 {
		return tracker.GetUpstreamMethodMetrics(stuck, "*", common.DataFinalityStateAll).FinalizationLag.Load()
	}

	tracker.SetLatestBlockNumber(peerA, 1000, 0)
	tracker.SetLatestBlockNumber(peerB, 1000, 0)
	tracker.SetFinalizedBlockNumber(peerA, 990)
	tracker.SetFinalizedBlockNumber(peerB, 990)
	require.NoError(t, p.Poll(ctx))
	require.Zero(t, slotLag())
	require.Zero(t, finalizationLag())

	tracker.SetLatestBlockNumber(peerA, 1100, 0)
	tracker.SetLatestBlockNumber(peerB, 1100, 0)
	tracker.SetFinalizedBlockNumber(peerA, 1050)
	tracker.SetFinalizedBlockNumber(peerB, 1050)
	require.NoError(t, p.Poll(ctx))

	require.EqualValues(t, 100, slotLag(), "a poll that repeats the stuck slot must record its lag")
	require.EqualValues(t, 60, finalizationLag(), "a poll that repeats the stuck finalized slot must record its lag")
}
