package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// healthyIngress wraps fakeIngress with a controllable HealthReporter
// implementation.
type healthyIngress struct {
	fakeIngress
	healthy bool
}

func (i *healthyIngress) Healthy() bool { return i.healthy }

func TestIndexer_IngressHealth(t *testing.T) {
	idx := newIndexer(t)
	nw := newFakeNetwork("evm:324", 0)
	idx.RegisterNetwork(nw)

	t.Run("unknown network", func(t *testing.T) {
		live, total := idx.IngressHealth("evm:999")
		assert.Equal(t, 0, live)
		assert.Equal(t, 0, total)
	})

	t.Run("no ingresses yet", func(t *testing.T) {
		live, total := idx.IngressHealth("evm:324")
		assert.Equal(t, 0, live)
		assert.Equal(t, 0, total)
	})

	up := &healthyIngress{fakeIngress: fakeIngress{name: "ws:up"}, healthy: true}
	down := &healthyIngress{fakeIngress: fakeIngress{name: "ws:down"}, healthy: false}
	// An ingress that doesn't implement HealthReporter counts as live —
	// the indexer can't assess transports it doesn't understand.
	opaque := &fakeIngress{name: "kafka:topic"}

	require.NoError(t, idx.AddIngress(context.Background(), "evm:324", up))
	require.NoError(t, idx.AddIngress(context.Background(), "evm:324", down))
	require.NoError(t, idx.AddIngress(context.Background(), "evm:324", opaque))

	t.Run("mixed health", func(t *testing.T) {
		live, total := idx.IngressHealth("evm:324")
		assert.Equal(t, 2, live, "healthy reporter + opaque ingress")
		assert.Equal(t, 3, total)
	})

	t.Run("all reporters down", func(t *testing.T) {
		up.healthy = false
		live, total := idx.IngressHealth("evm:324")
		assert.Equal(t, 1, live, "only the opaque ingress remains assumed-live")
		assert.Equal(t, 3, total)
	})
}

func TestIndexer_LastHead(t *testing.T) {
	now := time.Date(2026, 6, 12, 9, 30, 0, 0, time.UTC)
	logger := zerolog.New(zerolog.NewTestWriter(t))
	idx := New(&logger, Options{Now: func() time.Time { return now }})
	nw := newFakeNetwork("evm:324", 0)
	idx.RegisterNetwork(nw)

	_, _, ok := idx.LastHead("evm:324")
	assert.False(t, ok, "no head delivered yet")

	_, _, ok = idx.LastHead("evm:999")
	assert.False(t, ok, "unknown network")

	idx.Ingest(StreamEvent{
		Kind:      KindNewHead,
		NetworkId: "evm:324",
		SourceId:  "ws:up",
		Block:     BlockRef{Number: 42, Hash: "0xaa", ParentHash: "0x99"},
	})

	block, at, ok := idx.LastHead("evm:324")
	require.True(t, ok)
	assert.Equal(t, int64(42), block.Number)
	assert.True(t, now.Equal(at), "expected %s got %s", now, at)

	// A duplicate head must not move the liveness timestamp (it was
	// deduped, not delivered) — but a NEW head must.
	now = now.Add(10 * time.Second)
	idx.Ingest(StreamEvent{
		Kind:      KindNewHead,
		NetworkId: "evm:324",
		SourceId:  "ws:up",
		Block:     BlockRef{Number: 42, Hash: "0xaa", ParentHash: "0x99"},
	})
	_, at, _ = idx.LastHead("evm:324")
	assert.True(t, now.Add(-10*time.Second).Equal(at), "deduped head must not refresh liveness")

	idx.Ingest(StreamEvent{
		Kind:      KindNewHead,
		NetworkId: "evm:324",
		SourceId:  "ws:up",
		Block:     BlockRef{Number: 43, Hash: "0xbb", ParentHash: "0xaa"},
	})
	block, at, _ = idx.LastHead("evm:324")
	assert.Equal(t, int64(43), block.Number)
	assert.True(t, now.Equal(at), "expected %s got %s", now, at)
}
