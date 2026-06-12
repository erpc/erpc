package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubNetworkHandle struct{ id string }

func (h stubNetworkHandle) Id() string                       { return h.id }
func (h stubNetworkHandle) FinalityDepth() int64             { return 0 }
func (h stubNetworkHandle) SuggestLatestBlock(string, int64) {}

// stubIngress implements indexer.EventIngress plus indexer.HealthReporter
// with a flippable health flag.
type stubIngress struct {
	name    string
	healthy bool
}

func (i *stubIngress) Name() string { return i.name }
func (i *stubIngress) Start(context.Context, indexer.NetworkHandle, indexer.Sink) error {
	return nil
}
func (i *stubIngress) EnsureFilter(context.Context, string, string, []interface{}) error { return nil }
func (i *stubIngress) RemoveFilter(context.Context, string, string) error                { return nil }
func (i *stubIngress) Stop(context.Context) error                                        { return nil }
func (i *stubIngress) Healthy() bool                                                     { return i.healthy }

func newTestSubscriptionManager(t *testing.T) (*SubscriptionManager, *indexer.Indexer) {
	t.Helper()
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	idx := indexer.New(&logger, indexer.Options{})
	return NewSubscriptionManager(&logger, idx), idx
}

// TestWaitForLiveHeadSource pins the incident-driven contract: a pod with
// zero live head sources must refuse newHeads subscriptions (retryable
// error) instead of handing out a subscription ID that never delivers —
// the silent failure mode that hid the 2026-06-12 zkSync outage for hours.
func TestWaitForLiveHeadSource(t *testing.T) {
	origWait := liveHeadSourceWaitMax
	liveHeadSourceWaitMax = 300 * time.Millisecond
	t.Cleanup(func() { liveHeadSourceWaitMax = origWait })

	const networkID = "evm:324"

	t.Run("refuses when no ingress is live", func(t *testing.T) {
		sm, idx := newTestSubscriptionManager(t)
		idx.RegisterNetwork(stubNetworkHandle{id: networkID})
		require.NoError(t, idx.AddIngress(context.Background(), networkID, &stubIngress{name: "ws:a", healthy: false}))

		err := sm.waitForLiveHeadSource(context.Background(), networkID)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeNoLiveSubscriptionSource),
			"expected ErrNoLiveSubscriptionSource, got: %v", err)
	})

	t.Run("passes immediately when an ingress is live", func(t *testing.T) {
		sm, idx := newTestSubscriptionManager(t)
		idx.RegisterNetwork(stubNetworkHandle{id: networkID})
		require.NoError(t, idx.AddIngress(context.Background(), networkID, &stubIngress{name: "ws:a", healthy: true}))

		start := time.Now()
		require.NoError(t, sm.waitForLiveHeadSource(context.Background(), networkID))
		assert.Less(t, time.Since(start), liveHeadSourceWaitMax/2,
			"a live source must not incur the bootstrap grace wait")
	})

	t.Run("passes when an ingress becomes live during the grace wait", func(t *testing.T) {
		sm, idx := newTestSubscriptionManager(t)
		idx.RegisterNetwork(stubNetworkHandle{id: networkID})
		ing := &stubIngress{name: "ws:a", healthy: false}
		require.NoError(t, idx.AddIngress(context.Background(), networkID, ing))

		go func() {
			time.Sleep(120 * time.Millisecond)
			ing.healthy = true
		}()
		require.NoError(t, sm.waitForLiveHeadSource(context.Background(), networkID))
	})

	t.Run("honours caller context cancellation", func(t *testing.T) {
		sm, idx := newTestSubscriptionManager(t)
		idx.RegisterNetwork(stubNetworkHandle{id: networkID})
		require.NoError(t, idx.AddIngress(context.Background(), networkID, &stubIngress{name: "ws:a", healthy: false}))

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := sm.waitForLiveHeadSource(ctx, networkID)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeNoLiveSubscriptionSource))
	})
}

func TestSubscriptionHealth(t *testing.T) {
	const networkID = "evm:324"
	sm, idx := newTestSubscriptionManager(t)

	assert.Nil(t, sm.SubscriptionHealth(networkID), "nil before the network is bootstrapped")

	idx.RegisterNetwork(stubNetworkHandle{id: networkID})
	require.NoError(t, idx.AddIngress(context.Background(), networkID, &stubIngress{name: "ws:a", healthy: true}))
	require.NoError(t, idx.AddIngress(context.Background(), networkID, &stubIngress{name: "ws:b", healthy: false}))
	sm.networks.Store(networkID, struct{}{})

	h := sm.SubscriptionHealth(networkID)
	require.NotNil(t, h)
	assert.Equal(t, 1, h.LiveIngresses)
	assert.Equal(t, 2, h.TotalIngresses)
	assert.Empty(t, h.LastHeadAt, "no head delivered yet")

	idx.Ingest(indexer.StreamEvent{
		Kind:      indexer.KindNewHead,
		NetworkId: networkID,
		SourceId:  "ws:a",
		Block:     indexer.BlockRef{Number: 99, Hash: "0xaa", ParentHash: "0x98"},
	})

	h = sm.SubscriptionHealth(networkID)
	require.NotNil(t, h)
	assert.Equal(t, int64(99), h.LastHeadNumber)
	assert.NotEmpty(t, h.LastHeadAt)
}
