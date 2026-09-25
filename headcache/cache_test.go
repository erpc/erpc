package headcache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

// fakeChain is a deterministic scripted chain. Blocks are identified by
// (number, fork) so a reorg swaps the fork of a height range.
type fakeChain struct {
	mu          sync.Mutex
	blocks      map[int64]string // number -> fork label
	tip         int64
	calls       atomic.Int64
	headerCalls atomic.Int64
	dropLogs    map[string]bool // hash -> return [] (incomplete) logs
	failBlock   map[int64]bool
	nullAt      map[int64]bool // simulate a lagging upstream (null block)
}

var emitter = "0x5fbdb2315678afecb367f032d93f642f64180aa3"
var topicA = "0x1111111111111111111111111111111111111111111111111111111111111111"
var topicB = "0x2222222222222222222222222222222222222222222222222222222222222222"

func newFakeChain(tip int64) *fakeChain {
	c := &fakeChain{blocks: map[int64]string{}, tip: tip, dropLogs: map[string]bool{}, nullAt: map[int64]bool{}, failBlock: map[int64]bool{}}
	for i := int64(0); i <= tip; i++ {
		c.blocks[i] = "a"
	}
	return c
}

func hashOf(n int64, fork string) string {
	return common.BytesToHash([]byte(fmt.Sprintf("blk-%d-%s", n, fork))).Hex()
}

func txHashOf(n int64, fork string) string {
	return common.BytesToHash([]byte(fmt.Sprintf("tx-%d-%s", n, fork))).Hex()
}

func (c *fakeChain) hashAt(n int64) string { return hashOf(n, c.blocks[n]) }

func (c *fakeChain) logsFor(n int64, fork string) []map[string]interface{} {
	topic := topicA
	if n%2 == 1 {
		topic = topicB
	}
	return []map[string]interface{}{{
		"address": emitter, "topics": []string{topic}, "data": "0x",
		"blockNumber": fmt.Sprintf("0x%x", n), "blockHash": hashOf(n, fork),
		"transactionHash": txHashOf(n, fork), "transactionIndex": "0x0", "logIndex": "0x0", "removed": false,
	}}
}

func (c *fakeChain) BlockByNumber(_ context.Context, n int64) (json.RawMessage, error) {
	c.calls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failBlock[n] {
		return nil, fmt.Errorf("upstream failure")
	}
	fork, ok := c.blocks[n]
	if !ok || n > c.tip || c.nullAt[n] {
		return json.RawMessage("null"), nil
	}
	var bloom types.Bloom
	for _, l := range c.logsFor(n, fork) {
		bloom.Add(common.HexToAddress(l["address"].(string)).Bytes())
		for _, t := range l["topics"].([]string) {
			bloom.Add(common.HexToHash(t).Bytes())
		}
	}
	parent := "0x0000000000000000000000000000000000000000000000000000000000000000"
	if n > 0 {
		parent = c.hashAt(n - 1)
	}
	b := map[string]interface{}{
		"number": fmt.Sprintf("0x%x", n), "hash": hashOf(n, fork), "parentHash": parent,
		"logsBloom": "0x" + common.Bytes2Hex(bloom.Bytes()), "timestamp": fmt.Sprintf("0x%x", 1000+n),
		"transactions": []map[string]interface{}{{"hash": txHashOf(n, fork), "from": emitter}},
	}
	return json.Marshal(b)
}

func (c *fakeChain) HeaderByNumber(ctx context.Context, n int64) (json.RawMessage, error) {
	c.headerCalls.Add(1)
	return c.BlockByNumber(ctx, n)
}

func (c *fakeChain) LogsByBlockHash(_ context.Context, hash string) (json.RawMessage, error) {
	c.calls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropLogs[normHash(hash)] {
		return json.RawMessage("[]"), nil
	}
	for n, f := range c.blocks {
		if normHash(hashOf(n, f)) == normHash(hash) {
			return json.Marshal(c.logsFor(n, f))
		}
	}
	return json.RawMessage("[]"), nil
}

func (c *fakeChain) head(context.Context) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tip
}

func (c *fakeChain) mine(k int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i < k; i++ {
		c.tip++
		c.blocks[c.tip] = "a"
	}
}

// reorg replaces heights [from..tip] with fork label f.
func (c *fakeChain) reorg(from int64, f string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n := from; n <= c.tip; n++ {
		c.blocks[n] = f
	}
}

func testOpts(holder string) Options {
	return Options{
		Scope:  Scope{Namespace: "t", ProjectId: "p", NetworkId: "evm:1"},
		Holder: holder, Depth: 16, MaxBytes: 1 << 20, MaxBlockSize: 1 << 16, MaxPerTick: 16,
		Concurrency: 4, PollInterval: 50 * time.Millisecond, FetchTimeout: time.Second,
		MaxStaleness: time.Second, LeaseTTL: 2 * time.Second, MaxLogsRange: 16,
	}
}

func TestCache_HydrateServeAndReorg(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(10)
	c := New(testOpts("a"), NewMemoryStore(), ch, ch.head, nil)
	c.Tick(ctx)
	require.Equal(t, int64(10), c.Head())

	sub := c.Subscribe(16)
	defer sub.Close()

	// Full and tx-hash-only renderings from one record.
	full, ok := c.BlockByNumber(10, true)
	require.True(t, ok)
	require.Contains(t, string(full), `"from"`)
	lite, ok := c.BlockByNumber(10, false)
	require.True(t, ok)
	require.Contains(t, string(lite), txHashOf(10, "a"))
	require.NotContains(t, string(lite), `"from"`)
	_, ok = c.BlockByHash(hashOf(9, "a"), true)
	require.True(t, ok)

	// Cross-filter reuse without upstream calls.
	before := ch.calls.Load()
	fA, _ := ParseLogFilter(map[string]interface{}{"topics": []interface{}{topicA}})
	logs, ok := c.LogsRange(1, 10, fA)
	require.True(t, ok)
	require.Len(t, logs, 5)
	fAny, _ := ParseLogFilter(map[string]interface{}{"address": emitter})
	logs, ok = c.LogsRange(1, 10, fAny)
	require.True(t, ok)
	require.Len(t, logs, 10)
	fNone, _ := ParseLogFilter(map[string]interface{}{"address": "0x0000000000000000000000000000000000000001"})
	logs, ok = c.LogsRange(1, 10, fNone)
	require.True(t, ok)
	require.Len(t, logs, 0)
	require.Equal(t, before, ch.calls.Load())

	// Range extending past the window is a miss, never partial.
	_, ok = c.LogsRange(9, 11, fAny)
	require.False(t, ok)

	// Same-height reorg of the tip.
	ch.reorg(10, "b")
	c.Tick(ctx)
	require.Equal(t, int64(10), c.Head())
	_, ok = c.BlockByHash(hashOf(10, "a"), true)
	require.False(t, ok, "orphan must not be served")
	b, ok := c.BlockByNumber(10, false)
	require.True(t, ok)
	require.Contains(t, string(b), hashOf(10, "b"))
	ev := <-sub.C
	require.Len(t, ev.Removed, 1)
	require.Equal(t, normHash(hashOf(10, "a")), ev.Removed[0].Hash)
	require.Len(t, ev.Added, 1)
	removed, _ := ev.Removed[0].FilterLogs(nil, true)
	require.Contains(t, string(removed[0]), `"removed":true`)

	// Multi-block reorg plus advance.
	ch.reorg(7, "c")
	ch.mine(2)
	c.Tick(ctx)
	c.Tick(ctx)
	require.Equal(t, int64(12), c.Head())
	for n := int64(7); n <= 12; n++ {
		b, ok := c.BlockByNumber(n, false)
		require.True(t, ok)
		require.Contains(t, string(b), ch.hashAt(n))
	}
	_, ok = c.LogsByHash(hashOf(8, "a"), nil)
	require.False(t, ok)
}

func TestCache_IncompleteLogsNeverCached(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(5)
	ch.dropLogs[normHash(hashOf(5, "a"))] = true
	c := New(testOpts("a"), NewMemoryStore(), ch, ch.head, nil)
	c.Tick(ctx)
	require.Equal(t, int64(4), c.Head(), "block with bloom/log mismatch must not be published")
	_, ok := c.LogsRange(5, 5, nil)
	require.False(t, ok)
	require.Greater(t, c.Stats.Rejected.Load(), int64(0))
}

func TestCache_StalenessDisablesServing(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(5)
	o := testOpts("a")
	c := New(o, NewMemoryStore(), ch, ch.head, nil)
	c.Tick(ctx)
	require.True(t, c.Fresh())
	// Upstream failure prevents re-verification => no refresh.
	ch.failBlock[5] = true
	now := time.Now()
	c.nowFn = func() time.Time { return now.Add(2 * o.MaxStaleness) }
	c.Tick(ctx)
	require.False(t, c.Fresh())
	_, ok := c.BlockByNumber(5, true)
	require.False(t, ok)
}

func TestCache_SharedLeaderFollowerAndFencing(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(8)
	store := NewMemoryStore()
	a := New(testOpts("a"), store, ch, ch.head, nil)
	b := New(testOpts("b"), store, ch, ch.head, nil)
	a.Tick(ctx)
	b.Tick(ctx)
	require.Equal(t, int64(8), a.Head())
	require.Equal(t, int64(8), b.Head())
	require.Zero(t, b.Stats.Hydrated.Load(), "follower must not hydrate while leader is active")
	require.Positive(t, a.Stats.Hydrated.Load())

	// Follower serves from shared records.
	_, ok := b.LogsRange(1, 8, nil)
	require.True(t, ok)

	// Leader stalls; lease expires; b takes over with a higher epoch.
	store.ExpireLease(a.opt.Scope)
	ch.mine(1)
	b.Tick(ctx)
	require.Equal(t, int64(9), b.Head())
	require.Equal(t, int64(1), b.Stats.LeaderEpochs.Load())

	// Stale leader a cannot publish: its renew fails and it becomes follower.
	stale := *a.lease
	err := store.PublishSnapshot(ctx, &stale, &Snapshot{Epoch: stale.Epoch, Seq: 99, Head: 9, Hashes: []string{"0x1"}, At: time.Now()})
	require.ErrorIs(t, err, ErrLeaseLost)
	a.Tick(ctx)
	require.Nil(t, a.lease)
	require.Equal(t, int64(9), a.Head())
}

func TestCache_StoreLossFailsSafe(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(5)
	store := NewMemoryStore()
	a := New(testOpts("a"), store, ch, ch.head, nil)
	b := New(testOpts("b"), store, ch, ch.head, nil)
	a.Tick(ctx)
	b.Tick(ctx)
	require.True(t, b.Fresh())
	store.SetUnavailable(true)
	now := time.Now()
	later := func() time.Time { return now.Add(3 * time.Second) }
	a.nowFn, b.nowFn = later, later
	a.Tick(ctx)
	b.Tick(ctx)
	require.False(t, a.Fresh())
	require.False(t, b.Fresh())
}

func TestCache_SlowSubscriberDropped(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(3)
	c := New(testOpts("a"), NewMemoryStore(), ch, ch.head, nil)
	c.Tick(ctx)
	sub := c.Subscribe(1)
	ch.mine(1)
	c.Tick(ctx)
	ch.mine(1)
	c.Tick(ctx)
	require.Equal(t, 0, c.SubscriberCount())
	<-sub.C
	_, open := <-sub.C
	require.False(t, open)
}

// Regression: with a fixed head, the running loop must not spin on its own
// snapshot notifications. Fetches and publications stay bounded by the poll
// interval, and re-verification uses compact headers, not full blocks.
func TestCache_StartLoopNoSelfSpin(t *testing.T) {
	ch := newFakeChain(10)
	store := NewMemoryStore()
	o := testOpts("a")
	o.PollInterval = 50 * time.Millisecond
	o.MaxStaleness = 3 * time.Second
	leader := New(o, store, ch, ch.head, nil)
	follower := New(testOpts("b"), store, ch, ch.head, nil)
	leader.Start(context.Background())
	defer leader.Stop()
	require.Eventually(t, func() bool { return leader.Head() == 10 }, 2*time.Second, 10*time.Millisecond)
	follower.Start(context.Background())
	defer follower.Stop()
	require.Eventually(t, func() bool { return follower.Head() == 10 }, 2*time.Second, 10*time.Millisecond)

	calls0, hdr0, pub0 := ch.calls.Load(), ch.headerCalls.Load(), leader.Stats.Published.Load()
	time.Sleep(500 * time.Millisecond) // ~10 poll intervals
	fullCalls := (ch.calls.Load() - calls0) - (ch.headerCalls.Load() - hdr0)
	require.Zero(t, fullCalls, "no full block/log fetches when head is unchanged")
	require.LessOrEqual(t, ch.headerCalls.Load()-hdr0, int64(14), "header checks bounded by poll interval")
	require.LessOrEqual(t, leader.Stats.Published.Load()-pub0, int64(2), "no republish storm (heartbeat only)")
	require.Zero(t, follower.Stats.Hydrated.Load())
	require.True(t, follower.Fresh())
}

// H1: a null tip from a lagging upstream is a failed verification, not a reorg.
func TestCache_NullTipIsNotReorg(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(10)
	c := New(testOpts("a"), NewMemoryStore(), ch, ch.head, nil)
	c.Tick(ctx)
	sub := c.Subscribe(8)
	ch.mu.Lock()
	ch.nullAt[10] = true
	ch.mu.Unlock()
	c.Tick(ctx)
	require.Equal(t, int64(10), c.Head())
	require.Zero(t, c.Stats.Reorgs.Load())
	select {
	case ev := <-sub.C:
		t.Fatalf("unexpected event %+v", ev)
	default:
	}
}

// H2: a reorg deeper than one tick's budget is never published partially;
// orphans stop being served immediately and the walk resumes next tick.
func TestCache_DeepReorgBudgetNeverPublishesOrphans(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(20)
	o := testOpts("a")
	o.MaxPerTick = 4
	c := New(o, NewMemoryStore(), ch, ch.head, nil)
	for i := 0; i < 8 && c.Head() != 20; i++ {
		c.Tick(ctx)
	}
	require.Equal(t, int64(20), c.Head())
	pub := c.Stats.Published.Load()
	ch.reorg(12, "b")
	c.Tick(ctx)
	require.Equal(t, pub, c.Stats.Published.Load(), "no publish before ancestor is confirmed")
	require.False(t, c.Fresh(), "held window with known orphans must stop serving")
	_, ok := c.BlockByNumber(15, false)
	require.False(t, ok)
	for i := 0; i < 10 && c.Head() != 20; i++ {
		c.Tick(ctx)
	}
	require.Equal(t, int64(20), c.Head())
	for n := int64(5); n <= 20; n++ {
		if b, ok := c.BlockByNumber(n, false); ok {
			require.Contains(t, string(b), ch.hashAt(n))
		}
	}
}

// H3/H4: geth topic-length semantics; malformed filters are rejected (miss).
func TestLogFilter_GethSemantics(t *testing.T) {
	ch := newFakeChain(3)
	c := New(testOpts("a"), NewMemoryStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	cases := []struct {
		name  string
		obj   map[string]interface{}
		err   bool
		count int
	}{
		{"extra null position never matches", map[string]interface{}{"topics": []interface{}{topicA, nil}}, false, 0},
		{"single position", map[string]interface{}{"topics": []interface{}{topicA}}, false, 2},
		{"null wildcard", map[string]interface{}{"topics": []interface{}{nil}}, false, 4},
		{"or list", map[string]interface{}{"topics": []interface{}{[]interface{}{topicA, topicB}}}, false, 4},
		{"empty or list is wildcard", map[string]interface{}{"topics": []interface{}{[]interface{}{}}}, false, 4},
		{"bad address", map[string]interface{}{"address": "not-an-address"}, true, 0},
		{"short topic", map[string]interface{}{"topics": []interface{}{"0x11"}}, true, 0},
		{"too many topics", map[string]interface{}{"topics": []interface{}{nil, nil, nil, nil, nil}}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseLogFilter(tc.obj)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			logs, ok := c.LogsRange(0, 3, f)
			require.True(t, ok)
			require.Len(t, logs, tc.count)
		})
	}
}

// S1: a follower's freshness is anchored to the writer's verification time.
func TestCache_FollowerFreshnessAnchoredToSnapshot(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(5)
	store := NewMemoryStore()
	a := New(testOpts("a"), store, ch, ch.head, nil)
	b := New(testOpts("b"), store, ch, ch.head, nil)
	a.Tick(ctx)
	now := time.Now()
	b.nowFn = func() time.Time { return now.Add(900 * time.Millisecond) }
	b.Tick(ctx)
	require.True(t, b.Fresh())
	b.nowFn = func() time.Time { return now.Add(1500 * time.Millisecond) }
	require.False(t, b.Fresh(), "must not serve past maxStaleness from the leader's verification")
}

// S3: a follower missing a record for an event closes subscribers instead
// of delivering a gapped stream.
func TestCache_GapClosesSubscribers(t *testing.T) {
	ctx := context.Background()
	ch := newFakeChain(5)
	store := NewMemoryStore()
	a := New(testOpts("a"), store, ch, ch.head, nil)
	b := New(testOpts("b"), store, ch, ch.head, nil)
	a.Tick(ctx)
	b.Tick(ctx)
	sub := b.Subscribe(8)
	ch.mine(2)
	a.Tick(ctx)
	store.mu.Lock()
	delete(store.blocks, a.opt.Scope.Key()+"/"+normHash(hashOf(6, "a")))
	store.mu.Unlock()
	b.Tick(ctx)
	_, open := <-sub.C
	require.False(t, open)
	_, ok := b.BlockByNumber(6, false)
	require.False(t, ok)
	_, ok = b.BlockByNumber(7, false)
	require.True(t, ok)
}
