package headcache

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/util"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func init() { util.ConfigureTestLogger() }

func TestCache_RedisEmptyRestartRecoversCompleteWindow(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := NewRedisStore(client, RedisStoreOptions{})
	chain := newFakeChain(8)
	leader := New(testOpts("leader"), store, chain, chain.head, nil)
	follower := New(testOpts("follower"), store, chain, chain.head, nil)
	leader.Tick(ctx)
	leader.releaseLease()
	leader.Tick(ctx) // epoch 2, so an empty restart will regress the epoch
	follower.Tick(ctx)
	require.Equal(t, uint64(2), follower.snap.Epoch)
	sub := follower.Subscribe(4)

	// Total Redis data loss, including its epoch counter. A restart's fresh
	// snapshot cannot replace a still-trusted view merely because it is newer.
	server.FlushAll()
	leader.Tick(ctx)
	require.Equal(t, uint64(1), leader.snap.Epoch)
	follower.Tick(ctx)
	require.Equal(t, uint64(2), follower.snap.Epoch)
	require.False(t, sub.closed.Load())
	for _, hash := range leader.snap.Hashes {
		_, err := store.GetBlock(ctx, leader.opt.Scope, hash)
		require.NoError(t, err, "retained records must be restored, not just new blocks")
	}

	// Expire only the follower's old local trust. An authoritative complete
	// snapshot then restores service, closing streams whose continuity is lost.
	follower.mu.Lock()
	follower.freshAt = time.Now().Add(-2 * follower.opt.MaxStaleness)
	follower.mu.Unlock()
	follower.Tick(ctx)
	require.Equal(t, uint64(1), follower.snap.Epoch)
	require.True(t, sub.closed.Load())
	for n := int64(0); n <= 8; n++ {
		_, ok := follower.BlockByNumber(n, true)
		require.True(t, ok, "recovered height %d", n)
	}
}

type deadlineFetcher struct {
	*fakeChain
	block bool
}

func (f *deadlineFetcher) BlockByNumber(ctx context.Context, number int64) (json.RawMessage, error) {
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.fakeChain.BlockByNumber(ctx, number)
}

func TestCache_WholeTickDeadlinePreventsLatePublish(t *testing.T) {
	chain := newFakeChain(4)
	fetcher := &deadlineFetcher{fakeChain: chain, block: true}
	opts := testOpts("leader")
	opts.LeaseTTL = 100 * time.Millisecond
	opts.FetchTimeout = time.Second
	cache := New(opts, NewMemoryStore(), fetcher, chain.head, nil)
	started := time.Now()
	cache.Tick(context.Background())
	require.Less(t, time.Since(started), 500*time.Millisecond)
	require.Zero(t, cache.Stats.Published.Load())
	require.False(t, cache.Fresh())
	fetcher.block = false
	cache.Tick(context.Background())
	require.Equal(t, int64(4), cache.Head(), "next tick renews and can make progress")
}

func TestCache_FollowerFutureSkewDoesNotExtendBudget(t *testing.T) {
	chain := newFakeChain(3)
	store := NewMemoryStore()
	leader := New(testOpts("leader"), store, chain, chain.head, nil)
	follower := New(testOpts("follower"), store, chain, chain.head, nil)
	leader.Tick(context.Background())
	now := time.Now()
	snapshot, err := store.LoadSnapshot(context.Background(), leader.opt.Scope)
	require.NoError(t, err)
	snapshot.Seq++
	snapshot.At = now.Add(4 * time.Second)
	require.NoError(t, store.PublishSnapshot(context.Background(), leader.lease, snapshot))
	follower.nowFn = func() time.Time { return now }
	follower.Tick(context.Background())
	require.True(t, follower.Fresh())
	follower.nowFn = func() time.Time { return now.Add(2 * follower.opt.MaxStaleness) }
	require.False(t, follower.Fresh())
}

func TestCache_WindowJumpClosesSubscription(t *testing.T) {
	chain := newFakeChain(3)
	cache := New(testOpts("leader"), NewMemoryStore(), chain, chain.head, nil)
	cache.Tick(context.Background())
	sub := cache.Subscribe(4)
	chain.mine(50)
	cache.Tick(context.Background())
	require.True(t, sub.closed.Load(), "coalesced snapshots cannot silently skip heads")
}

// The store borrows a connector-owned client: Stop releases the lease but must
// never close the client, and a nil client (connector reconnecting) fails
// closed as ErrStoreUnavailable instead of panicking.
func TestCache_StopNeverClosesBorrowedClient(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	var current redis.UniversalClient = client
	store := NewRedisStoreFunc(func() redis.UniversalClient { return current }, RedisStoreOptions{})
	chain := newFakeChain(4)
	cache := New(testOpts("leader"), store, chain, chain.head, nil)
	cache.Start(context.Background())
	require.Eventually(t, func() bool { return cache.Head() > 0 }, 5*time.Second, 10*time.Millisecond)
	cache.Stop()
	require.NoError(t, client.Ping(context.Background()).Err(), "Stop must not close the borrowed client")
	lease, err := store.AcquireLease(context.Background(), testOpts("leader").Scope, "other", time.Second)
	require.NoError(t, err, "Stop released the lease")
	require.NotNil(t, lease)

	current = nil
	_, err = store.LoadSnapshot(context.Background(), testOpts("leader").Scope)
	require.ErrorIs(t, err, ErrStoreUnavailable)
}
