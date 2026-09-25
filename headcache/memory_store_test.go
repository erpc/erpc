package headcache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A fast chain with a long RecordTTL must not accumulate records beyond the
// canonical window in the in-process store.
func TestMemoryStore_RetentionBoundedByWindowNotTTL(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(10)
	store := NewMemoryStore()
	o := testOpts("a")
	o.RecordTTL = 24 * time.Hour
	c := New(o, store, ch, ch.head, nil)
	c.Tick(ctx)
	for i := 0; i < 50; i++ {
		ch.mine(3)
		c.Tick(ctx)
	}
	require.Equal(t, int64(160), c.Head())
	n := store.blockCount(o.Scope)
	require.LessOrEqual(t, int64(n), o.Depth, "store holds only the committed window")
	require.Equal(t, int(o.Depth), n)
	// Everything the snapshot references is still readable.
	snap, err := store.LoadSnapshot(ctx, o.Scope)
	require.NoError(t, err)
	for _, h := range snap.Hashes {
		_, err := store.GetBlock(ctx, o.Scope, h)
		require.NoError(t, err)
	}
}

// Orphans from a reorg are pruned on the next accepted publish, while records
// already delivered to subscribers remain usable (they are owned pointers).
func TestMemoryStore_ReorgPrunesOrphansDeliveredEventsSafe(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(10)
	store := NewMemoryStore()
	c := New(testOpts("a"), store, ch, ch.head, nil)
	c.Tick(ctx)
	sub := c.Subscribe(16)
	defer sub.Close()
	orphan := normHash(hashOf(10, "a"))
	_, err := store.GetBlock(ctx, c.opt.Scope, orphan)
	require.NoError(t, err)

	ch.reorg(10, "b")
	c.Tick(ctx)
	ev := <-sub.C
	require.Len(t, ev.Removed, 1)
	require.Equal(t, orphan, ev.Removed[0].Hash)
	_, err = store.GetBlock(ctx, c.opt.Scope, orphan)
	require.ErrorIs(t, err, ErrNotFound, "orphan pruned from store")
	logs, err := ev.Removed[0].FilterLogs(nil, true)
	require.NoError(t, err)
	require.NotEmpty(t, logs, "removed event still renders after prune")
}

// A fenced (rejected) publish must never prune anything.
func TestMemoryStore_RejectedPublishDoesNotPrune(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(8)
	store := NewMemoryStore()
	a := New(testOpts("a"), store, ch, ch.head, nil)
	a.Tick(ctx)
	before := store.blockCount(a.opt.Scope)
	require.Positive(t, before)

	stale := *a.lease
	store.ExpireLease(a.opt.Scope)
	b := New(testOpts("b"), store, ch, ch.head, nil)
	b.Tick(ctx)
	err := store.PublishSnapshot(ctx, &stale, &Snapshot{Epoch: stale.Epoch, Seq: 99, Head: 1, Hashes: []string{"0x1"}, At: time.Now()})
	require.ErrorIs(t, err, ErrLeaseLost)
	// Invalid snapshot under a live lease: also rejected, no prune.
	err = store.PublishSnapshot(ctx, b.lease, &Snapshot{Epoch: b.lease.Epoch, Seq: 99, Head: 5, Hashes: nil, At: time.Now()})
	require.Error(t, err)
	require.Equal(t, before, store.blockCount(a.opt.Scope))
}

// Pruning one scope never touches another scope's records, including scopes
// whose key is a string prefix of it.
func TestMemoryStore_PruneIsScopeLocal(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	chA, chB := newFakeChain(10), newFakeChain(10)
	oa, ob := testOpts("a"), testOpts("b")
	oa.Scope.NetworkId, ob.Scope.NetworkId = "evm:1", "evm:10"
	a := New(oa, store, chA, chA.head, nil)
	b := New(ob, store, chB, chB.head, nil)
	a.Tick(ctx)
	b.Tick(ctx)
	nb := store.blockCount(ob.Scope)
	for i := 0; i < 20; i++ {
		chA.mine(5)
		a.Tick(ctx)
	}
	require.Equal(t, nb, store.blockCount(ob.Scope))
	snap, err := store.LoadSnapshot(ctx, ob.Scope)
	require.NoError(t, err)
	for _, h := range snap.Hashes {
		_, err := store.GetBlock(ctx, ob.Scope, h)
		require.NoError(t, err)
	}
}

// A follower that loaded an older snapshot while the leader publishes a newer
// one treats pruned heights as misses and never serves partial data.
func TestMemoryStore_FollowerRaceMissingRecordIsMiss(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(10)
	store := NewMemoryStore()
	a := New(testOpts("a"), store, ch, ch.head, nil)
	b := New(testOpts("b"), store, ch, ch.head, nil)
	a.Tick(ctx)
	old, err := store.LoadSnapshot(ctx, a.opt.Scope)
	require.NoError(t, err)
	ch.mine(20)
	a.Tick(ctx)
	a.Tick(ctx)
	missing := 0
	for _, h := range old.Hashes {
		if _, err := store.GetBlock(ctx, a.opt.Scope, h); err != nil {
			missing++
		}
	}
	require.Positive(t, missing, "old window was pruned")
	b.Tick(ctx)
	require.Equal(t, a.Head(), b.Head())
	_, ok := b.BlockByNumber(old.Base(), false)
	require.False(t, ok, "pruned height outside the new window is a miss")
}
