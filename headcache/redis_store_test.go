package headcache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type testEnv struct {
	mr     *miniredis.Miniredis // nil when running against real Redis
	client redis.UniversalClient
}

// newEnv uses HEADCACHE_TEST_REDIS_ADDR (real Redis) when set, else miniredis.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	if addr := os.Getenv("HEADCACHE_TEST_REDIS_ADDR"); addr != "" {
		c := redis.NewClient(&redis.Options{Addr: addr})
		if err := c.Ping(context.Background()).Err(); err != nil {
			t.Fatalf("real redis ping: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return &testEnv{client: c}
	}
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return &testEnv{mr: mr, client: c}
}

// expire advances TTLs: fast-forward in miniredis, real sleep otherwise.
func (e *testEnv) expire(d time.Duration) {
	if e.mr != nil {
		e.mr.FastForward(d)
		return
	}
	time.Sleep(d)
}

func uniquePrefix(t *testing.T) string {
	return "test:" + t.Name() + ":" + time.Now().Format("150405.000000000")
}

var sc = Scope{ProjectId: "p1", NetworkId: "evm:1"}

func snap(l *Lease, seq uint64, head int64, hashes ...string) *Snapshot {
	return &Snapshot{Epoch: l.Epoch, Seq: seq, Head: head, Hashes: hashes, At: time.Now()}
}

func TestRedisStore_TwoOwnersSingleLease(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	p := uniquePrefix(t)
	a := NewRedisStore(env.client, RedisStoreOptions{Prefix: p})
	b := NewRedisStore(env.client, RedisStoreOptions{Prefix: p})

	la, err := a.AcquireLease(ctx, sc, "A", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AcquireLease(ctx, sc, "B", 2*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("want ErrLeaseHeld, got %v", err)
	}
	if _, err := a.RenewLease(ctx, la, 2*time.Second); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := a.ReleaseLease(ctx, la); err != nil {
		t.Fatal(err)
	}
	lb, err := b.AcquireLease(ctx, sc, "B", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if lb.Epoch <= la.Epoch {
		t.Fatalf("epoch must increase: %d <= %d", lb.Epoch, la.Epoch)
	}
}

func TestRedisStore_ExpiryStaleWriterRejected(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})

	la, err := s.AcquireLease(ctx, sc, "A", 1*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishSnapshot(ctx, la, snap(la, 1, 10, "0xa")); err != nil {
		t.Fatal(err)
	}
	env.expire(1500 * time.Millisecond)

	// After expiry, with no successor, stale holder still cannot publish or renew.
	// (Bypass local ExpiresAt check to exercise server-side fencing.)
	stale := *la
	stale.ExpiresAt = time.Time{}
	if err := s.PublishSnapshot(ctx, &stale, snap(la, 2, 11, "0xb")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("want ErrLeaseLost, got %v", err)
	}
	if _, err := s.RenewLease(ctx, la, time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renew want ErrLeaseLost, got %v", err)
	}

	lb, err := s.AcquireLease(ctx, sc, "B", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishSnapshot(ctx, lb, snap(lb, 1, 12, "0xc")); err != nil {
		t.Fatal(err)
	}
	// Stale A with same holder name reusing old epoch is rejected.
	if err := s.PublishSnapshot(ctx, &stale, snap(la, 99, 50, "0xevil")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("want ErrLeaseLost, got %v", err)
	}
	// A cannot release B's lease.
	_ = s.ReleaseLease(ctx, la)
	if _, err := s.RenewLease(ctx, lb, 5*time.Second); err != nil {
		t.Fatalf("B lease must survive stale release: %v", err)
	}
	got, err := s.LoadSnapshot(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Epoch != lb.Epoch || got.Head != 12 || got.HashAt(12) != "0xc" {
		t.Fatalf("unexpected snapshot %+v", got)
	}
	// Same holder name re-acquiring gets a fresh epoch; old token remains dead.
	_ = s.ReleaseLease(ctx, lb)
	la2, err := s.AcquireLease(ctx, sc, "A", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishSnapshot(ctx, &stale, snap(la, 100, 60, "0xevil")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old epoch of same holder must be fenced, got %v", err)
	}
	if err := s.PublishSnapshot(ctx, la2, snap(la2, 1, 13, "0xd")); err != nil {
		t.Fatal(err)
	}
}

func TestRedisStore_LocalExpiryRejectsPublish(t *testing.T) {
	env := newEnv(t)
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})
	l, err := s.AcquireLease(context.Background(), sc, "A", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	l.ExpiresAt = time.Now().Add(-time.Millisecond)
	if err := s.PublishSnapshot(context.Background(), l, snap(l, 1, 1, "0x1")); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("got %v", err)
	}
}

func TestRedisStore_SequenceCAS(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})
	l, _ := s.AcquireLease(ctx, sc, "A", time.Minute)
	if err := s.PublishSnapshot(ctx, l, snap(l, 5, 10, "0xa")); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{5, 4} {
		if err := s.PublishSnapshot(ctx, l, snap(l, seq, 9, "0xold")); !errors.Is(err, ErrStaleSequence) {
			t.Fatalf("seq %d: got %v", seq, err)
		}
	}
	if err := s.PublishSnapshot(ctx, l, snap(l, 7, 11)); err == nil {
		t.Fatal("empty snapshot must be rejected")
	}
	bad := snap(l, 6, 11, "0xb")
	bad.Epoch = l.Epoch + 1
	if err := s.PublishSnapshot(ctx, l, bad); err == nil {
		t.Fatal("epoch mismatch must be rejected")
	}
	got, _ := s.LoadSnapshot(ctx, sc)
	if got.Seq != 5 || got.HashAt(10) != "0xa" {
		t.Fatalf("snapshot must be unchanged: %+v", got)
	}
}

func TestRedisStore_BlocksAndIsolation(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	p := uniquePrefix(t)
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: p})
	other := NewRedisStore(env.client, RedisStoreOptions{Prefix: p + ":trust2"})
	rec := &BlockRecord{Number: 1, Hash: "0xh", ParentHash: "0xp", Block: []byte(`{"a":1}`), Logs: []byte(`[]`)}

	if err := s.PutBlock(ctx, sc, rec, 0); err == nil {
		t.Fatal("unbounded ttl must be rejected")
	}
	if err := s.PutBlock(ctx, sc, rec, time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBlock(ctx, sc, "0xh")
	if err != nil || got.ParentHash != "0xp" || string(got.Block) != `{"a":1}` {
		t.Fatalf("got %+v %v", got, err)
	}
	for _, c := range []struct {
		st *RedisStore
		sc Scope
	}{
		{s, Scope{ProjectId: "p2", NetworkId: "evm:1"}},
		{s, Scope{ProjectId: "p1", NetworkId: "evm:2"}},
		{other, sc},
	} {
		if _, err := c.st.GetBlock(ctx, c.sc, "0xh"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("isolation leak %+v: %v", c.sc, err)
		}
		if _, err := c.st.LoadSnapshot(ctx, c.sc); !errors.Is(err, ErrNotFound) {
			t.Fatalf("snapshot leak: %v", err)
		}
	}
	// Lease in one scope does not block another.
	if _, err := s.AcquireLease(ctx, sc, "A", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := other.AcquireLease(ctx, sc, "B", time.Minute); err != nil {
		t.Fatalf("other trust set must be independent: %v", err)
	}
	if _, err := s.AcquireLease(ctx, Scope{"p1", "evm:2"}, "B", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBlock(ctx, Scope{}, "0xh"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("empty scope must be invalid, got %v", err)
	}
	// Cleanup: block TTL bounds storage.
	env.expire(1500 * time.Millisecond)
	if _, err := s.GetBlock(ctx, sc, "0xh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("block must expire: %v", err)
	}
}

func TestRedisStore_SnapshotTTLBoundsStaleness(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t), SnapshotTTL: time.Second})
	l, _ := s.AcquireLease(ctx, sc, "A", time.Minute)
	if err := s.PublishSnapshot(ctx, l, snap(l, 1, 1, "0x1")); err != nil {
		t.Fatal(err)
	}
	env.expire(1500 * time.Millisecond)
	if _, err := s.LoadSnapshot(ctx, sc); !errors.Is(err, ErrNotFound) {
		t.Fatalf("snapshot must expire, got %v", err)
	}
}

func TestRedisStore_Watch(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})
	ch, stop, err := s.WatchSnapshots(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := s.AcquireLease(ctx, sc, "A", time.Minute)
	if err := s.PublishSnapshot(ctx, l, snap(l, 1, 1, "0x1")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no watch signal")
	}
	stop()
	select {
	case _, ok := <-ch:
		for ok {
			_, ok = <-ch
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after stop")
	}
}

func TestRedisStore_UnavailableAndCancel(t *testing.T) {
	env := newEnv(t)
	s := NewRedisStore(env.client, RedisStoreOptions{Prefix: uniquePrefix(t)})

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.AcquireLease(cctx, sc, "A", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	defer dead.Close()
	d := NewRedisStore(dead, RedisStoreOptions{})
	ctx := context.Background()
	if _, err := d.AcquireLease(ctx, sc, "A", time.Second); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := d.LoadSnapshot(ctx, sc); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("load: %v", err)
	}
	if err := d.PublishSnapshot(ctx, &Lease{Scope: sc, Holder: "A", Epoch: 1}, &Snapshot{Epoch: 1, Seq: 1, Head: 1, Hashes: []string{"0x1"}}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("publish: %v", err)
	}
	if _, _, err := d.WatchSnapshots(ctx, sc); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("watch: %v", err)
	}
	if env.mr != nil {
		// Live backend going down mid-flight.
		env.mr.Close()
		if _, err := s.GetBlock(ctx, sc, "0x"); !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("closed backend: %v", err)
		}
	}
}

func TestRedisStore_ClusterHashTag(t *testing.T) {
	s := NewRedisStore(nil, RedisStoreOptions{Prefix: "x"})
	keys := []string{s.leaseKey(sc), s.epochKey(sc), s.snapKey(sc), s.snapMetaKey(sc), s.blockKey(sc, "0x1")}
	slot := func(k string) string {
		i := len("{")
		j := 0
		for j = i; k[j] != '}'; j++ {
		}
		return k[i:j]
	}
	for _, k := range keys {
		if slot(k) != "x|p1/evm:1" {
			t.Fatalf("key %s lacks shared hash tag", k)
		}
	}
}
