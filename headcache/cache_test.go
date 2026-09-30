package headcache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/telemetry"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

var emitter = "0x5fbdb2315678afecb367f032d93f642f64180aa3"
var topicA = "0x1111111111111111111111111111111111111111111111111111111111111111"
var topicB = "0x2222222222222222222222222222222222222222222222222222222222222222"

type fakeChain struct {
	mu        sync.Mutex
	blocks    map[int64]string
	tip       int64
	dropLogs  map[string]bool
	failBlock map[int64]bool
	nullAt    map[int64]bool
	oversized map[int64]bool
	headCalls int
	bodyCalls int
	delay     time.Duration
	mixedCase bool
}

func newFakeChain(tip int64) *fakeChain {
	c := &fakeChain{blocks: map[int64]string{}, tip: tip, dropLogs: map[string]bool{}, failBlock: map[int64]bool{}, nullAt: map[int64]bool{}, oversized: map[int64]bool{}}
	for n := int64(0); n <= tip; n++ {
		c.blocks[n] = "a"
	}
	return c
}

func hashOf(n int64, fork string) string {
	return ethcommon.BytesToHash([]byte(fmt.Sprintf("block-%d-%s", n, fork))).Hex()
}
func txHashOf(n int64, fork string) string {
	return ethcommon.BytesToHash([]byte(fmt.Sprintf("tx-%d-%s", n, fork))).Hex()
}

func (c *fakeChain) blockLocked(n int64) (json.RawMessage, error) {
	if c.failBlock[n] {
		return nil, fmt.Errorf("upstream failure")
	}
	fork, ok := c.blocks[n]
	if !ok || n > c.tip || c.nullAt[n] {
		return json.RawMessage("null"), nil
	}
	logs := c.logsLocked(n, fork)
	var bloom types.Bloom
	for _, l := range logs {
		bloom.Add(ethcommon.HexToAddress(l["address"].(string)).Bytes())
		for _, topic := range l["topics"].([]string) {
			bloom.Add(ethcommon.HexToHash(topic).Bytes())
		}
	}
	parent := "0x" + fmt.Sprintf("%064x", 0)
	if n > 0 {
		parent = hashOf(n-1, c.blocks[n-1])
	}
	block := map[string]interface{}{
		"number": fmt.Sprintf("0x%x", n), "hash": hashOf(n, fork), "parentHash": parent,
		"logsBloom": "0x" + ethcommon.Bytes2Hex(bloom.Bytes()), "timestamp": fmt.Sprintf("0x%x", 1000+n),
		"transactions": []map[string]interface{}{{"hash": txHashOf(n, fork), "from": emitter}},
	}
	if c.oversized[n] {
		block["padding"] = strings.Repeat("x", 2048)
	}
	return json.Marshal(block)
}

func (c *fakeChain) logsLocked(n int64, fork string) []map[string]interface{} {
	topic := topicA
	if n%2 == 1 {
		topic = topicB
	}
	return []map[string]interface{}{{
		"address": emitter, "topics": []string{topic}, "data": "0x", "blockNumber": fmt.Sprintf("0x%x", n),
		"blockHash": hashOf(n, fork), "transactionHash": txHashOf(n, fork),
		"transactionIndex": "0x0", "logIndex": "0x0", "removed": false,
	}}
}

func (c *fakeChain) BlockByNumber(_ context.Context, n int64) (json.RawMessage, error) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodyCalls++
	raw, err := c.blockLocked(n)
	if err == nil && c.mixedCase {
		var block map[string]interface{}
		_ = json.Unmarshal(raw, &block)
		block["hash"] = strings.ToUpper(block["hash"].(string))
		block["parentHash"] = strings.ToUpper(block["parentHash"].(string))
		raw, _ = json.Marshal(block)
	}
	return raw, err
}
func (c *fakeChain) HeaderByNumber(_ context.Context, n int64) (json.RawMessage, error) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headCalls++
	return c.blockLocked(n)
}
func (c *fakeChain) LogsByBlockHash(_ context.Context, hash string) (json.RawMessage, error) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	for n, fork := range c.blocks {
		if normHash(hashOf(n, fork)) == normHash(hash) {
			if c.dropLogs[normHash(hash)] {
				return json.RawMessage("[]"), nil
			}
			return json.Marshal(c.logsLocked(n, fork))
		}
	}
	return json.RawMessage("[]"), nil
}
func (c *fakeChain) head(context.Context) int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.tip }
func (c *fakeChain) mine(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i < n; i++ {
		c.tip++
		c.blocks[c.tip] = "a"
	}
}
func (c *fakeChain) reorg(from int64, fork string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n := from; n <= c.tip; n++ {
		c.blocks[n] = fork
	}
}

type mapStore struct {
	mu      sync.Mutex
	records map[string]*BlockRecord
	puts    int
}

type fakeFleetStore struct {
	*mapStore
	muFleet  sync.Mutex
	leader   bool
	snap     *Snapshot
	acquires int
	releases int
}

func newFakeFleetStore() *fakeFleetStore { return &fakeFleetStore{mapStore: newMapStore()} }
func (s *fakeFleetStore) Acquire(_ context.Context, _ Scope, _ time.Duration) (Lease, error) {
	s.muFleet.Lock()
	defer s.muFleet.Unlock()
	s.acquires++
	if !s.leader {
		return nil, nil
	}
	s.leader = false
	return &fakeFleetLease{store: s}, nil
}
func (s *fakeFleetStore) ReadSnapshot(_ context.Context, _ Scope) (*Snapshot, error) {
	s.muFleet.Lock()
	defer s.muFleet.Unlock()
	if s.snap == nil {
		return nil, ErrNotFound
	}
	out := *s.snap
	out.Hashes = append([]string(nil), s.snap.Hashes...)
	return &out, nil
}

type fakeFleetLease struct {
	store    *fakeFleetStore
	released bool
}

func (l *fakeFleetLease) Renew(context.Context, time.Duration) (bool, error) { return !l.released, nil }
func (l *fakeFleetLease) Publish(_ context.Context, snap *Snapshot, _ time.Duration) (bool, error) {
	if l.released {
		return false, nil
	}
	l.store.muFleet.Lock()
	defer l.store.muFleet.Unlock()
	out := *snap
	out.Hashes = append([]string(nil), snap.Hashes...)
	l.store.snap = &out
	return true, nil
}
func (l *fakeFleetLease) Release(context.Context) error {
	if !l.released {
		l.released = true
		l.store.muFleet.Lock()
		l.store.releases++
		l.store.leader = true
		l.store.muFleet.Unlock()
	}
	return nil
}

func newMapStore() *mapStore { return &mapStore{records: map[string]*BlockRecord{}} }
func (s *mapStore) PutBlock(_ context.Context, _ Scope, r *BlockRecord, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	b, _ := json.Marshal(r)
	copy := &BlockRecord{}
	_ = json.Unmarshal(b, copy)
	s.records[normHash(r.Hash)] = copy
	return nil
}
func (s *mapStore) GetBlock(_ context.Context, _ Scope, hash string) (*BlockRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[normHash(hash)]
	if r == nil {
		return nil, ErrNotFound
	}
	b, _ := json.Marshal(r)
	copy := &BlockRecord{}
	_ = json.Unmarshal(b, copy)
	return copy, nil
}

func testOpts() Options {
	return Options{Scope: Scope{Namespace: "t", ProjectId: "p", NetworkId: "evm:1"}, Depth: 8,
		MaxBytes: 1 << 20, MaxBlockSize: 1 << 16, PollInterval: time.Second, FetchTimeout: time.Second,
		MaxStaleness: 2 * time.Second, MaxLogsRange: 8, RecordTTL: time.Hour}
}

func TestCache_VerifiedWindowHitMissAndTipFastPath(t *testing.T) {
	ch, store := newFakeChain(10), newMapStore()
	c := New(testOpts(), store, ch, ch.head, nil)
	c.Tick(context.Background())
	require.Equal(t, int64(10), c.Head())
	before := ch.headCalls
	c.Tick(context.Background())
	require.Equal(t, before+1, ch.headCalls, "unchanged tip needs one header read")
	_, ok := c.BlockByNumber(10, true)
	require.True(t, ok)
	_, ok = c.BlockByNumber(999, true)
	require.False(t, ok)
	_, ok = c.LogsRange(7, 10, nil)
	require.True(t, ok)
	_, ok = c.LogsRange(9, 11, nil)
	require.False(t, ok, "partial ranges are not hits")
}

func TestCache_TickMetricsTrackFreshnessAndErrorOutcome(t *testing.T) {
	project, network := "headcache-metrics-test", "evm:headcache-metrics-test"
	opts := testOpts()
	opts.Scope.ProjectId, opts.Scope.NetworkId = project, network
	chain := newFakeChain(5)
	liveTip := int64(5)
	cache := New(opts, newMapStore(), chain, func(context.Context) int64 { return liveTip }, nil)

	labels := []string{project, network}
	fresh := telemetry.MetricHeadCacheFresh.WithLabelValues(labels...)
	refreshFresh := telemetry.MetricHeadCacheRefreshTotal.WithLabelValues(project, network, "fresh")
	refreshStale := telemetry.MetricHeadCacheRefreshTotal.WithLabelValues(project, network, "stale")
	refreshError := telemetry.MetricHeadCacheRefreshTotal.WithLabelValues(project, network, "error")
	initialFresh, initialStale, initialError := testutil.ToFloat64(refreshFresh), testutil.ToFloat64(refreshStale), testutil.ToFloat64(refreshError)

	cache.Tick(context.Background())
	require.True(t, cache.Fresh())
	require.Equal(t, float64(1), testutil.ToFloat64(fresh))
	require.Equal(t, initialFresh+1, testutil.ToFloat64(refreshFresh))
	require.Equal(t, initialStale, testutil.ToFloat64(refreshStale))
	require.Equal(t, initialError, testutil.ToFloat64(refreshError))

	liveTip = -1
	cache.nowFn = func() time.Time { return time.Now().Add(10 * opts.MaxStaleness) }
	cache.Tick(context.Background())
	require.False(t, cache.Fresh())
	require.Equal(t, float64(0), testutil.ToFloat64(fresh))
	require.Equal(t, initialError+1, testutil.ToFloat64(refreshError))
	require.Equal(t, initialFresh+1, testutil.ToFloat64(refreshFresh))
	require.Equal(t, initialStale, testutil.ToFloat64(refreshStale))

	cache.Stop()
	require.Equal(t, float64(0), testutil.ToFloat64(fresh))
}

func TestCache_SharedPayloadReuseAndCorruptionFallback(t *testing.T) {
	store, chain := newMapStore(), newFakeChain(5)
	a := New(testOpts(), store, chain, chain.head, nil)
	a.Tick(context.Background())
	require.Positive(t, a.Stats.Hydrated.Load())

	b := New(testOpts(), store, chain, chain.head, nil)
	b.Tick(context.Background())
	require.Zero(t, b.Stats.Hydrated.Load(), "another replica reuses hash-addressed payloads")

	bad, _ := store.GetBlock(context.Background(), testOpts().Scope, hashOf(5, "a"))
	bad.ParentHash = hashOf(4, "wrong")
	_ = store.PutBlock(context.Background(), testOpts().Scope, bad, time.Hour)
	c := New(testOpts(), store, chain, chain.head, nil)
	c.Tick(context.Background())
	require.Positive(t, c.Stats.Hydrated.Load(), "corrupt payload must fall back to upstream; body calls=%d", chain.bodyCalls)
	block, ok := c.BlockByNumber(5, true)
	require.True(t, ok)
	require.Contains(t, string(block), hashOf(5, "a"))
}

func TestCache_FleetLeaderFollowerAndFailover(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	leader := New(testOpts(), store, chain, chain.head, nil)
	leader.Tick(context.Background())
	require.True(t, leader.Fresh())
	require.NotNil(t, store.snap)
	require.Positive(t, chain.headCalls)
	_, err := store.GetBlock(context.Background(), testOpts().Scope, hashOf(5, "a"))
	require.NoError(t, err)

	followerChain := newFakeChain(5)
	follower := New(testOpts(), store, followerChain, followerChain.head, nil)
	follower.Tick(context.Background())
	require.True(t, follower.Fresh())
	require.Equal(t, int64(5), follower.Head())
	require.Zero(t, followerChain.headCalls, "follower does not query upstream headers")
	require.Zero(t, followerChain.bodyCalls, "follower does not fetch upstream payloads")

	sub := leader.Subscribe(2)
	leader.Stop()
	require.Equal(t, 1, store.releases)
	follower.Tick(context.Background())
	require.True(t, follower.Fresh(), "the follower can acquire and refresh after leader release")
	require.Positive(t, followerChain.headCalls)
	_, open := <-sub.C
	require.False(t, open)
}

func TestCache_FleetLeaderDoesNotRewriteUnchangedPayloadsEveryTick(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	c := New(testOpts(), store, chain, chain.head, nil)
	c.Tick(context.Background())
	store.mu.Lock()
	initialWrites := store.puts
	store.mu.Unlock()
	require.Equal(t, len(c.snap.Hashes), initialWrites, "takeover writes each payload once")
	c.Tick(context.Background())
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, initialWrites, store.puts, "unchanged payloads are not rewritten on each tick")
}

func TestCache_FleetTakeoverContinuesIncompleteColdFill(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	opts := testOpts()
	opts.Depth, opts.MaxLogsRange, opts.MaxPerTick = 6, 6, 2
	leader := New(opts, store, chain, chain.head, nil)
	leader.Tick(context.Background())
	require.True(t, store.snap.Incomplete)
	require.Len(t, store.snap.Hashes, 2)

	follower := New(opts, store, chain, chain.head, nil)
	follower.Tick(context.Background())
	before := chain.bodyCalls
	leader.Stop()
	follower.Tick(context.Background())
	require.True(t, follower.Fresh())
	require.True(t, store.snap.Incomplete)
	require.Len(t, follower.snap.Hashes, 4, "promoted follower hydrates the next older chunk")
	require.Equal(t, before+2, chain.bodyCalls)
}

func TestCache_FleetFollowerFailsClosedForStaleOrMissingPayload(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			store := newFakeFleetStore()
			store.leader = true
			leader := New(testOpts(), store, newFakeChain(5), func(context.Context) int64 { return 5 }, nil)
			leader.Tick(context.Background())
			leader.Stop()
			store.muFleet.Lock()
			store.leader = false
			store.muFleet.Unlock()
			if missing {
				store.mu.Lock()
				delete(store.records, normHash(hashOf(5, "a")))
				store.mu.Unlock()
			} else {
				store.muFleet.Lock()
				store.snap.At = time.Now().Add(-time.Hour)
				store.muFleet.Unlock()
			}
			follower := New(testOpts(), store, newFakeChain(5), func(context.Context) int64 { return 5 }, nil)
			sub := follower.Subscribe(1)
			follower.Tick(context.Background())
			require.False(t, follower.Fresh())
			require.Equal(t, int64(-1), follower.Head())
			_, open := <-sub.C
			require.False(t, open)
		})
	}
}

func TestCache_FleetFollowerDeliversShallowReorg(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	leader := New(testOpts(), store, chain, chain.head, nil)
	leader.Tick(context.Background())
	follower := New(testOpts(), store, newFakeChain(5), nil, nil)
	follower.Tick(context.Background())
	sub := follower.Subscribe(2)

	chain.reorg(5, "fork")
	leader.Tick(context.Background())
	follower.Tick(context.Background())
	ev := <-sub.C
	require.Len(t, ev.Removed, 1)
	require.Equal(t, hashOf(5, "a"), ev.Removed[0].Hash)
	require.Len(t, ev.Added, 1)
	require.Equal(t, hashOf(5, "fork"), ev.Added[0].Hash)
	leader.Stop()
}

func TestCache_FleetFollowerAcceptsUppercasePayloadHeaderHashes(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	leader := New(testOpts(), store, newFakeChain(5), func(context.Context) int64 { return 5 }, nil)
	leader.Tick(context.Background())

	rec, err := store.GetBlock(context.Background(), testOpts().Scope, hashOf(5, "a"))
	require.NoError(t, err)
	var block map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Block, &block))
	block["hash"] = strings.ToUpper(block["hash"].(string))
	block["parentHash"] = strings.ToUpper(block["parentHash"].(string))
	rec.Block, err = json.Marshal(block)
	require.NoError(t, err)
	require.NoError(t, store.PutBlock(context.Background(), testOpts().Scope, rec, time.Hour))

	follower := New(testOpts(), store, newFakeChain(5), nil, nil)
	follower.Tick(context.Background())
	require.True(t, follower.Fresh())
	require.Equal(t, int64(5), follower.Head())
	leader.Stop()
}

func TestCache_FleetFollowerClampsSmallFutureSnapshotSkew(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	leader := New(testOpts(), store, newFakeChain(5), func(context.Context) int64 { return 5 }, nil)
	leader.Tick(context.Background())
	now := time.Now()
	store.muFleet.Lock()
	store.snap.At = now.Add(2 * time.Second)
	store.muFleet.Unlock()

	follower := New(testOpts(), store, newFakeChain(5), nil, nil)
	follower.nowFn = func() time.Time { return now }
	follower.Tick(context.Background())
	require.True(t, follower.Fresh())
	follower.nowFn = func() time.Time { return now.Add(testOpts().MaxStaleness + time.Second) }
	require.False(t, follower.Fresh(), "future skew must not extend local staleness")
	leader.Stop()
}

func TestCache_SameHeightAndDeepReorgEvents(t *testing.T) {
	ch := newFakeChain(10)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	sub := c.Subscribe(8)
	ch.reorg(10, "b")
	c.Tick(context.Background())
	ev := <-sub.C
	require.Equal(t, hashOf(10, "a"), ev.Removed[0].Hash)
	require.Equal(t, hashOf(10, "b"), ev.Added[0].Hash)

	ch.reorg(7, "c")
	c.Tick(context.Background())
	ev = <-sub.C
	require.Len(t, ev.Removed, 4)
	require.Equal(t, int64(10), ev.Removed[0].Number)
	require.Equal(t, int64(7), ev.Removed[3].Number)
	require.Len(t, ev.Added, 4)
	require.Equal(t, int64(7), ev.Added[0].Number)
	require.Equal(t, int64(10), ev.Added[3].Number)
	_, ok := c.BlockByHash(hashOf(10, "a"), true)
	require.False(t, ok)
}

func TestCache_ProgressiveColdStartDoesNotPublishPartialView(t *testing.T) {
	ch := newFakeChain(5)
	ch.delay = 15 * time.Millisecond
	o := testOpts()
	o.Depth, o.MaxLogsRange, o.MaxPerTick, o.Concurrency = 6, 6, 2, 2
	c := New(o, newMapStore(), ch, ch.head, nil)
	for i := 0; i < 2; i++ {
		c.Tick(context.Background())
		_, ok := c.LogsRange(0, 1, nil)
		require.False(t, ok, "ranges crossing the unfilled prefix are misses")
	}
	c.Tick(context.Background())
	require.True(t, c.Fresh(), "bounded work converges over successive ticks")
	_, ok := c.LogsRange(0, 5, nil)
	require.True(t, ok)
}

func TestCache_OversizedBlockLeavesContiguousSuffix(t *testing.T) {
	ch := newFakeChain(5)
	ch.oversized[2] = true
	normal, _ := ch.BlockByNumber(context.Background(), 5)
	logs, _ := ch.LogsByBlockHash(context.Background(), hashOf(5, "a"))
	rec, err := buildRecord(normal, logs, 0)
	require.NoError(t, err)
	o := testOpts()
	o.Depth, o.MaxPerTick, o.MaxBlockSize, o.MaxLogsRange = 6, 2, rec.Size()+64, 6
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	c.Tick(context.Background())
	require.True(t, c.Fresh())
	require.Equal(t, int64(5), c.Head())
	_, ok := c.LogsRange(3, 5, nil)
	require.True(t, ok, "the complete suffix above the oversized block is usable")
	_, ok = c.LogsRange(0, 5, nil)
	require.False(t, ok, "ranges crossing the oversized block are misses")
	_, ok = c.BlockByNumber(2, true)
	require.False(t, ok)
	bodyCalls := ch.bodyCalls
	c.Tick(context.Background())
	require.Equal(t, bodyCalls, ch.bodyCalls, "known oversized block does not stall or retry the suffix")
}

func TestCache_ConstrainedMemoryDoesNotRefillEvictedRecords(t *testing.T) {
	ch := newFakeChain(5)
	block, _ := ch.BlockByNumber(context.Background(), 5)
	logs, _ := ch.LogsByBlockHash(context.Background(), hashOf(5, "a"))
	rec, err := buildRecord(block, logs, 0)
	require.NoError(t, err)
	o := testOpts()
	o.MaxBytes = rec.Size() * 2
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	require.True(t, c.Fresh())
	require.Len(t, c.snap.Hashes, 2, "published view retains a complete suffix within budget")
	require.Empty(t, c.pending, "pending does not retain published or evicted payloads")
	bodyCalls := ch.bodyCalls
	c.Tick(context.Background())
	require.Equal(t, bodyCalls, ch.bodyCalls, "same-tip verification must not rehydrate capacity evictions")
	_, ok := c.LogsRange(4, 5, nil)
	require.True(t, ok)
	_, ok = c.LogsRange(3, 5, nil)
	require.False(t, ok)
}

func TestCache_MaxBytesBelowRecordSizeDoesNotPublishEmptyView(t *testing.T) {
	ch := newFakeChain(0)
	block, _ := ch.BlockByNumber(context.Background(), 0)
	logs, _ := ch.LogsByBlockHash(context.Background(), hashOf(0, "a"))
	rec, err := buildRecord(block, logs, 0)
	require.NoError(t, err)
	maxBytes := int64(len(rec.Block) + len(rec.Logs))
	require.Less(t, maxBytes, rec.Size())
	o := testOpts()
	o.Depth, o.MaxBytes = 1, maxBytes
	c := New(o, newMapStore(), ch, ch.head, nil)
	sub := c.Subscribe(1)
	c.Tick(context.Background())
	require.False(t, c.Fresh())
	require.Equal(t, int64(-1), c.Head())
	require.Nil(t, c.snap, "an empty retained range is not a published view")
	require.Equal(t, 0, c.SubscriberCount())
	_, open := <-sub.C
	require.False(t, open, "subscribers close when the complete range cannot fit")
}

func TestCache_MixedCaseHydratedHeaderMatchesVerifiedTip(t *testing.T) {
	ch := newFakeChain(5)
	ch.mixedCase = true
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	require.True(t, c.Fresh())
	require.Equal(t, int64(5), c.Head())
}

func TestCache_IncompleteLogsNeverInstall(t *testing.T) {
	ch := newFakeChain(5)
	ch.dropLogs[normHash(hashOf(5, "a"))] = true
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	require.False(t, c.Fresh())
	require.Equal(t, int64(-1), c.Head())
	require.Positive(t, c.Stats.Rejected.Load())
}

func TestCache_DeepReorgOutsideWindowClosesSubscribers(t *testing.T) {
	ch := newFakeChain(10)
	o := testOpts()
	o.Depth = 4
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	sub := c.Subscribe(8)
	ch.reorg(1, "fork")
	c.Tick(context.Background())
	_, open := <-sub.C
	require.False(t, open, "cannot emit a complete reorg outside retained window")
	require.Equal(t, int64(10), c.Head())
}

func TestCache_StaleViewClosesSubscribers(t *testing.T) {
	ch := newFakeChain(5)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	sub := c.Subscribe(2)
	ch.mu.Lock()
	ch.nullAt[5] = true
	ch.mu.Unlock()
	c.nowFn = func() time.Time { return time.Now().Add(10 * time.Second) }
	c.Tick(context.Background())
	require.False(t, c.Fresh())
	_, open := <-sub.C
	require.False(t, open)
}

func TestCache_GapAfterHeadJumpClosesSubscribers(t *testing.T) {
	ch := newFakeChain(1)
	o := testOpts()
	o.Depth = 4
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	sub := c.Subscribe(4)
	ch.mine(6)
	c.Tick(context.Background())
	_, open := <-sub.C
	require.False(t, open)
	require.Equal(t, int64(7), c.Head())
}

func TestCache_RejectsHugeExplicitLogRange(t *testing.T) {
	c := New(testOpts(), newMapStore(), nil, nil, nil)
	_, ok := c.LogsRange(0, int64(^uint64(0)>>1), nil)
	require.False(t, ok)
}

func TestLogFilterGethSemantics(t *testing.T) {
	filter, err := ParseLogFilter(map[string]interface{}{"topics": []interface{}{topicA, nil}})
	require.NoError(t, err)
	require.True(t, filter.match(&rawLog{Address: emitter, Topics: []string{topicA, topicB}}))
	require.False(t, filter.match(&rawLog{Address: emitter, Topics: []string{topicB, topicA}}))
	_, err = ParseLogFilter(map[string]interface{}{"address": "not-an-address"})
	require.Error(t, err)
}
