package headcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func redisBlockKeys(t *testing.T, env *testEnv, s *RedisStore, scope Scope) int {
	t.Helper()
	keys, err := env.client.Keys(context.Background(), s.tag(scope)+":blk:*").Result()
	require.NoError(t, err)
	return len(keys)
}

// Regression for unbounded retention: a fast chain with a long RecordTTL
// stays within Depth + MaxPerTick block keys (was ~400 for depth 16).
func TestRedisStore_RetentionBoundedOnFastChain(t *testing.T) {
	env := newEnv(t)
	st := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t), SnapshotTTL: 10 * time.Second})
	ch := newFakeChain(20)
	o := testOpts("a")
	o.RecordTTL = time.Hour
	c := New(o, st, ch, ch.head, nil)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		ch.mine(8)
		c.Tick(ctx)
	}
	require.Equal(t, int64(420), c.Head())
	n := redisBlockKeys(t, env, st, o.Scope)
	require.LessOrEqual(t, int64(n), o.Depth+int64(o.MaxPerTick), "block keys bounded by window")
	snap, err := st.LoadSnapshot(ctx, o.Scope)
	require.NoError(t, err)
	for _, h := range snap.Hashes {
		_, err := st.GetBlock(ctx, o.Scope, h)
		require.NoError(t, err, "committed snapshot references survive")
	}
}

func putRec(t *testing.T, s *RedisStore, scope Scope, hash string) {
	t.Helper()
	require.NoError(t, s.PutBlock(context.Background(), scope, &BlockRecord{Hash: hash}, time.Hour))
}

func blockExists(s *RedisStore, scope Scope, hash string) bool {
	_, err := s.GetBlock(context.Background(), scope, hash)
	return err == nil
}

// Only hashes absent from the new snapshot are deleted, including across an
// A-B-A reorg where A is re-put before the publish that references it again.
func TestRedisStore_PruneKeepsNewSnapshotRefsABA(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})
	l, err := s.AcquireLease(ctx, sc, "A", 5*time.Second)
	require.NoError(t, err)
	for _, h := range []string{"0x1", "0x2a", "0x2b"} {
		putRec(t, s, sc, h)
	}
	require.NoError(t, s.PublishSnapshot(ctx, l, snap(l, 1, 2, "0x1", "0x2a")))
	require.NoError(t, s.PublishSnapshot(ctx, l, snap(l, 2, 2, "0x1", "0x2b")))
	require.False(t, blockExists(s, sc, "0x2a"), "orphan pruned")
	require.True(t, blockExists(s, sc, "0x1"))
	require.True(t, blockExists(s, sc, "0x2b"))
	putRec(t, s, sc, "0x2a") // put-before-publish on A-B-A
	require.NoError(t, s.PublishSnapshot(ctx, l, snap(l, 3, 2, "0x1", "0x2a")))
	require.True(t, blockExists(s, sc, "0x2a"))
	require.True(t, blockExists(s, sc, "0x1"))
	require.False(t, blockExists(s, sc, "0x2b"))
}

// Rejected publishes (stale epoch, stale seq) must delete nothing.
func TestRedisStore_RejectedPublishDoesNotPrune(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})
	la, err := s.AcquireLease(ctx, sc, "A", time.Second)
	require.NoError(t, err)
	putRec(t, s, sc, "0x1")
	putRec(t, s, sc, "0x2")
	require.NoError(t, s.PublishSnapshot(ctx, la, snap(la, 1, 2, "0x1", "0x2")))

	// Stale seq under the live lease.
	err = s.PublishSnapshot(ctx, la, snap(la, 1, 3, "0x3"))
	require.ErrorIs(t, err, ErrInvalidSnapshot)
	require.True(t, blockExists(s, sc, "0x1"))
	require.True(t, blockExists(s, sc, "0x2"))

	// Stale epoch after takeover.
	env.expire(1500 * time.Millisecond)
	putRec(t, s, sc, "0x1")
	putRec(t, s, sc, "0x2")
	lb, err := s.AcquireLease(ctx, sc, "B", 5*time.Second)
	require.NoError(t, err)
	_ = lb
	stale := *la
	stale.ExpiresAt = time.Time{}
	err = s.PublishSnapshot(ctx, &stale, snap(la, 9, 3, "0x3"))
	require.True(t, errors.Is(err, ErrLeaseLost), "got %v", err)
	require.True(t, blockExists(s, sc, "0x1"))
	require.True(t, blockExists(s, sc, "0x2"))
}

// Pruning one scope never touches another scope's records.
func TestRedisStore_PruneIsScopeLocal(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})
	other := Scope{Namespace: "ns1", ProjectId: "p1", NetworkId: "evm:10"}
	putRec(t, s, other, "0x1")
	putRec(t, s, other, "0x2")
	lo, err := s.AcquireLease(ctx, other, "A", 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, s.PublishSnapshot(ctx, lo, snap(lo, 1, 2, "0x1", "0x2")))

	l, err := s.AcquireLease(ctx, sc, "A", 5*time.Second)
	require.NoError(t, err)
	putRec(t, s, sc, "0x1")
	putRec(t, s, sc, "0x2")
	require.NoError(t, s.PublishSnapshot(ctx, l, snap(l, 1, 2, "0x1", "0x2")))
	putRec(t, s, sc, "0x9")
	require.NoError(t, s.PublishSnapshot(ctx, l, snap(l, 2, 9, "0x9")))
	require.False(t, blockExists(s, sc, "0x1"))
	require.True(t, blockExists(s, other, "0x1"))
	require.True(t, blockExists(s, other, "0x2"))
}

// Unchanged snapshot (heartbeat republish) prunes nothing.
func TestRedisStore_HeartbeatPublishIsNoOpPrune(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})
	l, err := s.AcquireLease(ctx, sc, "A", 5*time.Second)
	require.NoError(t, err)
	putRec(t, s, sc, "0x1")
	require.NoError(t, s.PublishSnapshot(ctx, l, snap(l, 1, 1, "0x1")))
	require.Empty(t, s.pruneCandidates(ctx, sc, snap(l, 2, 1, "0x1")))
	require.NoError(t, s.PublishSnapshot(ctx, l, snap(l, 2, 1, "0x1")))
	require.True(t, blockExists(s, sc, "0x1"))
}

// Candidate count is capped per publish.
func TestRedisStore_PruneCandidatesCapped(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})
	l, err := s.AcquireLease(ctx, sc, "A", 5*time.Second)
	require.NoError(t, err)
	var hs []string
	for i := 0; i < maxPrunePerPublish+10; i++ {
		hs = append(hs, "0x"+string(rune('a'+i%26))+time.Duration(i).String())
	}
	require.NoError(t, s.PublishSnapshot(ctx, l, snap(l, 1, int64(len(hs)), hs...)))
	require.Len(t, s.pruneCandidates(ctx, sc, snap(l, 2, 1, "0xnew")), maxPrunePerPublish)
}
