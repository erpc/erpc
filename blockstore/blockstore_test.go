package blockstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	// failBody fails only the full-block fetch (hydration); headers still succeed.
	failBody  map[int64]bool
	nullAt    map[int64]bool
	oversized map[int64]bool
	headCalls int
	bodyCalls int
	delay     time.Duration
	mixedCase bool
	// logless blocks have no logs and the given raw "transactions" value; "" omits the field.
	logless map[int64]string
}

func newFakeChain(tip int64) *fakeChain {
	c := &fakeChain{blocks: map[int64]string{}, tip: tip, dropLogs: map[string]bool{}, failBlock: map[int64]bool{}, failBody: map[int64]bool{}, nullAt: map[int64]bool{}, oversized: map[int64]bool{}, logless: map[int64]string{}}
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
	if txs, ok := c.logless[n]; ok {
		delete(block, "transactions")
		if txs != "" {
			block["transactions"] = json.RawMessage(txs)
		}
	}
	return json.Marshal(block)
}

func (c *fakeChain) logsLocked(n int64, fork string) []map[string]interface{} {
	if _, ok := c.logless[n]; ok {
		return []map[string]interface{}{}
	}
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
	if c.failBody[n] {
		return nil, fmt.Errorf("upstream body failure")
	}
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

type failingFleetLease struct {
	Lease
	failRenewAt int
	renewals    int
	failPublish bool
}

func (l *failingFleetLease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	l.renewals++
	if l.renewals == l.failRenewAt {
		return false, fmt.Errorf("transient renew failure")
	}
	return l.Lease.Renew(ctx, ttl)
}

func (l *failingFleetLease) Publish(ctx context.Context, snap *Snapshot, ttl time.Duration) (bool, error) {
	if l.failPublish {
		return false, fmt.Errorf("transient publish failure")
	}
	return l.Lease.Publish(ctx, snap, ttl)
}

func TestCache_FleetLeaseErrorsReleaseLock(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failRenewAt int
		failPublish bool
	}{
		{"renew", 1, false},
		{"renew before publish", 2, false},
		{"publish", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeFleetStore()
			store.leader = true
			chain := newFakeChain(5)
			leader := New(testOpts(), store, chain, chain.head, nil)
			leader.Tick(t.Context())
			defer leader.Stop()
			require.True(t, leader.Fresh())
			leader.lease = &failingFleetLease{Lease: leader.lease, failRenewAt: tc.failRenewAt, failPublish: tc.failPublish}
			leader.Tick(t.Context())
			require.Nil(t, leader.lease)
			require.Equal(t, 1, store.releases, "release the Redis lease on EVAL failure")
			other := New(testOpts(), store, chain, chain.head, nil)
			other.Tick(t.Context())
			defer other.Stop()
			require.NotNil(t, other.lease, "another replica can acquire on its next tick")
		})
	}
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
	project, network := "blockstore-metrics-test", "evm:blockstore-metrics-test"
	opts := testOpts()
	opts.Scope.ProjectId, opts.Scope.NetworkId = project, network
	chain := newFakeChain(5)
	liveTip := int64(5)
	cache := New(opts, newMapStore(), chain, func(context.Context) int64 { return liveTip }, nil)

	labels := []string{project, network}
	fresh := telemetry.MetricBlockStoreFresh.WithLabelValues(labels...)
	refreshFresh := telemetry.MetricBlockStoreRefreshTotal.WithLabelValues(project, network, "fresh")
	refreshStale := telemetry.MetricBlockStoreRefreshTotal.WithLabelValues(project, network, "stale")
	refreshError := telemetry.MetricBlockStoreRefreshTotal.WithLabelValues(project, network, "error")
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

func TestCache_FleetLeaderStepsDownAfterRepeatedRefreshFailures(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	var broken atomic.Bool
	head := func(ctx context.Context) int64 {
		if broken.Load() {
			return -1 // upstream path down while the shared store stays healthy
		}
		return chain.head(ctx)
	}
	leader := New(testOpts(), store, chain, head, nil)
	leader.Tick(context.Background())
	require.True(t, leader.Fresh())
	require.NotNil(t, leader.lease)

	broken.Store(true)
	for i := 1; i < maxLeaderFailures; i++ {
		leader.Tick(context.Background())
		require.NotNil(t, leader.lease, "a transient failure (%d) keeps the lease", i)
		require.Zero(t, store.releases)
	}
	leader.Tick(context.Background())
	require.Nil(t, leader.lease, "the sick leader releases after %d consecutive failures", maxLeaderFailures)
	require.Equal(t, 1, store.releases)

	// A healthy replica can now take over and publish.
	followerChain := newFakeChain(6)
	follower := New(testOpts(), store, followerChain, followerChain.head, nil)
	follower.Tick(context.Background())
	require.NotNil(t, follower.lease, "a healthy replica acquires the released lease")
	require.True(t, follower.Fresh())
	require.Equal(t, int64(6), follower.Head())
}

// A leader whose tip block never hydrates returns no error from refresh but
// publishes nothing; it must still step down so a healthy replica takes over.
func TestCache_FleetLeaderStepsDownWhenTipNeverHydrates(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	leader := New(testOpts(), store, chain, chain.head, nil)
	leader.Tick(context.Background())
	require.True(t, leader.Fresh())

	// New tip whose body fetch keeps failing; headers still work and Redis is healthy.
	chain.mine(1)
	chain.mu.Lock()
	chain.failBody[6] = true
	chain.mu.Unlock()
	for i := 1; i < maxLeaderFailures; i++ {
		leader.Tick(context.Background())
		require.NotNil(t, leader.lease, "tick %d keeps the lease", i)
	}
	leader.Tick(context.Background())
	require.Nil(t, leader.lease, "a leader that cannot hydrate its tip steps down")
	require.Equal(t, 1, store.releases)

	healthy := newFakeChain(6)
	follower := New(testOpts(), store, healthy, healthy.head, nil)
	follower.Tick(context.Background())
	require.NotNil(t, follower.lease)
	require.True(t, follower.Fresh())
	require.Equal(t, int64(6), follower.Head())
}

// While the tip keeps failing, non-tip heights still hydrate into pending
// (nothing is installed yet). Records for heights that left the moving window
// must be dropped instead of accumulating.
func TestCache_PendingPrunedWhileTipFails(t *testing.T) {
	opts := testOpts()
	opts.Depth = 4
	opts.MaxPerTick = 4
	chain := newFakeChain(4)
	chain.failBody[4] = true // the tip fails from the very first (cold) tick
	c := New(opts, nil, chain, chain.head, nil)
	for i := 0; i < 20; i++ {
		c.Tick(context.Background())
		require.False(t, c.Fresh(), "nothing is publishable while the tip fails")
		require.LessOrEqual(t, len(c.pending), int(opts.Depth), "pending must stay within the window (tick %d)", i)
		// Only the newest block is ever unavailable, so every older height
		// hydrates and then falls out of the window as the chain advances.
		delete(chain.failBody, chain.tip)
		chain.mine(1)
		chain.mu.Lock()
		chain.failBody[chain.tip] = true
		chain.mu.Unlock()
	}
}

func TestCache_FleetLeaderFailureCountResetsOnSuccess(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	var broken atomic.Bool
	head := func(ctx context.Context) int64 {
		if broken.Load() {
			return -1
		}
		return chain.head(ctx)
	}
	c := New(testOpts(), store, chain, head, nil)
	c.Tick(context.Background())
	for round := 0; round < 3; round++ {
		broken.Store(true)
		for i := 1; i < maxLeaderFailures; i++ {
			c.Tick(context.Background())
		}
		broken.Store(false)
		c.Tick(context.Background())
		require.NotNil(t, c.lease, "interleaved successes reset the count (round %d)", round)
	}
	require.Zero(t, store.releases)
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

func TestCache_FleetFollowerAcceptsUppercaseSnapshotAndPayloadHashes(t *testing.T) {
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

	store.muFleet.Lock()
	for i, hash := range store.snap.Hashes {
		store.snap.Hashes[i] = strings.ToUpper(hash)
	}
	store.muFleet.Unlock()
	follower := New(testOpts(), store, newFakeChain(5), nil, nil)
	follower.Tick(context.Background())
	require.True(t, follower.Fresh())
	require.Equal(t, int64(5), follower.Head())
	_, ok := follower.BlockByNumber(5, true)
	require.True(t, ok)
	_, ok = follower.LogsRange(4, 5, nil)
	require.True(t, ok)
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

func TestCache_BlockWithoutTransactionsArrayNeverServes(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		txs   string
		serve bool
	}{
		"omitted":     {txs: "", serve: false},
		"null":        {txs: "null", serve: false},
		"empty block": {txs: "[]", serve: true},
	} {
		t.Run(name, func(t *testing.T) {
			chain := newFakeChain(5)
			chain.logless[5] = tc.txs
			store := newFakeFleetStore()
			store.leader = true
			leader := New(testOpts(), store, chain, chain.head, nil)
			leader.Tick(ctx)
			_, ok := leader.BlockByNumber(5, true)
			require.Equal(t, tc.serve, ok)
			if tc.serve {
				return
			}
			require.Positive(t, leader.Stats.Rejected.Load())
			require.Nil(t, store.snap, "a writer must not publish the block")

			// A follower must also reject such a payload written by an older replica.
			raw, err := chain.BlockByNumber(ctx, 5)
			require.NoError(t, err)
			hash := normHash(hashOf(5, "a"))
			require.NoError(t, store.PutBlock(ctx, testOpts().Scope, &BlockRecord{Number: 5, Hash: hash, ParentHash: normHash(hashOf(4, "a")), Block: raw, Logs: json.RawMessage("[]")}, time.Hour))
			store.snap = &Snapshot{Head: 5, Hashes: []string{hash}, At: time.Now()}
			follower := New(testOpts(), store, chain, chain.head, nil)
			follower.Tick(ctx)
			_, ok = follower.BlockByNumber(5, true)
			require.False(t, ok)
		})
	}
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

func TestCache_TipAdvanceExtendsVerifiedWindowIncrementally(t *testing.T) {
	ch := newFakeChain(10)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(context.Background())
	require.True(t, c.Fresh())

	ch.mine(1)
	ch.mu.Lock()
	before := ch.headCalls
	ch.mu.Unlock()
	c.Tick(context.Background())
	ch.mu.Lock()
	require.Equal(t, before+1, ch.headCalls, "a linked one-block advance fetches only the new tip header")
	ch.mu.Unlock()
	require.Equal(t, int64(11), c.Head())
	_, ok := c.LogsRange(4, 11, nil)
	require.True(t, ok, "extended window stays complete across the full depth")

	ch.mine(3)
	ch.mu.Lock()
	before = ch.headCalls
	ch.mu.Unlock()
	c.Tick(context.Background())
	ch.mu.Lock()
	require.Equal(t, before+3, ch.headCalls, "a linked multi-block advance fetches only new headers")
	ch.mu.Unlock()
	require.Equal(t, int64(14), c.Head())

	// A reorg below the new tip breaks the parent link and falls back to a full refetch.
	sub := c.Subscribe(8)
	ch.reorg(13, "b")
	ch.mine(1)
	ch.mu.Lock()
	before = ch.headCalls
	ch.mu.Unlock()
	c.Tick(context.Background())
	ch.mu.Lock()
	require.Greater(t, ch.headCalls-before, 2, "unlinked advance refetches the window")
	ch.mu.Unlock()
	ev := <-sub.C
	require.Equal(t, hashOf(14, "a"), ev.Removed[0].Hash)
	_, ok = c.BlockByHash(hashOf(13, "b"), true)
	require.True(t, ok)
	_, ok = c.BlockByHash(hashOf(13, "a"), true)
	require.False(t, ok)
}

func TestCache_TipRegressionStopsServingAboveVerifiedTip(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	ch := newFakeChain(10)
	c := New(testOpts(), store, ch, ch.head, nil)
	c.Tick(context.Background())
	require.Equal(t, int64(10), c.Head())
	sub := c.Subscribe(8)
	follower := New(testOpts(), store, ch, ch.head, nil)
	follower.Tick(context.Background())
	followerSub := follower.Subscribe(8)

	ch.mu.Lock()
	ch.tip = 8
	ch.mu.Unlock()
	c.Tick(context.Background())
	follower.Tick(context.Background())
	require.True(t, c.Fresh(), "the prefix up to the matching tip stays verified")
	require.Equal(t, int64(8), c.Head())
	_, ok := c.BlockByNumber(9, true)
	require.False(t, ok, "blocks above the live tip are not served")
	_, ok = c.BlockByHash(hashOf(10, "a"), true)
	require.False(t, ok)
	_, ok = c.LogsRange(7, 9, nil)
	require.False(t, ok)
	_, ok = c.LogsRange(3, 8, nil)
	require.True(t, ok)
	require.Equal(t, int64(8), store.snap.Head, "followers receive the trimmed snapshot")
	require.Equal(t, int64(8), follower.Head())
	for _, stream := range []*Subscription{sub, followerSub} {
		_, open := <-stream.C
		require.False(t, open, "a lagging observation must not fabricate removed logs")
	}

	ch.mu.Lock()
	ch.tip = 10
	body := ch.bodyCalls
	ch.mu.Unlock()
	c.Tick(context.Background())
	follower.Tick(context.Background())
	require.Equal(t, int64(10), c.Head())
	require.Equal(t, int64(10), follower.Head())
	ch.mu.Lock()
	require.Equal(t, body, ch.bodyCalls, "recovered blocks reload from the shared store")
	ch.mu.Unlock()
	_, ok = c.BlockByNumber(10, true)
	require.True(t, ok)
}

// Partial views (cold fill, permanent holes, byte trims) must keep
// subscribers open exactly when the published events still extend what they
// were sent, and close them when a changed height cannot be emitted.
func TestCache_PartialViewSubscriberContinuity(t *testing.T) {
	recordSize := func(ch *fakeChain, n int64) int64 {
		block, _ := ch.BlockByNumber(context.Background(), n)
		logs, _ := ch.LogsByBlockHash(context.Background(), hashOf(n, "a"))
		rec, err := buildRecord(block, logs, 0)
		require.NoError(t, err)
		return rec.Size()
	}
	cases := []struct {
		name   string
		tip    int64
		opts   func(*fakeChain, *Options)
		warm   int
		change func(*fakeChain)
		open   bool
		added  []int64
	}{
		{
			name: "cold fill tip advance", tip: 10, warm: 1,
			opts:   func(_ *fakeChain, o *Options) { o.Depth, o.MaxLogsRange, o.MaxPerTick = 8, 8, 2 },
			change: func(ch *fakeChain) { ch.mine(1) },
			open:   true, added: []int64{11},
		},
		{
			name: "cold fill same-tip backfill", tip: 10, warm: 1,
			opts:   func(_ *fakeChain, o *Options) { o.Depth, o.MaxLogsRange, o.MaxPerTick = 8, 8, 2 },
			change: func(*fakeChain) {},
			open:   true,
		},
		{
			name: "new head above oversized hole", tip: 5, warm: 1,
			opts: func(ch *fakeChain, o *Options) {
				ch.oversized[2] = true
				o.Depth, o.MaxLogsRange, o.MaxPerTick, o.MaxBlockSize = 6, 6, 6, 1500
			},
			change: func(ch *fakeChain) { ch.mine(1) },
			open:   true, added: []int64{6},
		},
		{
			name: "reorg below published cold-fill suffix", tip: 10, warm: 4,
			opts: func(_ *fakeChain, o *Options) { o.Depth, o.MaxLogsRange, o.MaxPerTick = 8, 8, 2 },
			change: func(ch *fakeChain) {
				ch.reorg(8, "x")
				ch.mine(1)
			},
		},
		{
			name: "byte trim hides lowest reorged height", tip: 10, warm: 1,
			opts: func(ch *fakeChain, o *Options) {
				o.Depth, o.MaxLogsRange = 4, 4
				o.MaxBytes = 4*recordSize(ch, 10) + 100
			},
			change: func(ch *fakeChain) {
				ch.oversized[10] = true
				ch.reorg(8, "x")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newFakeChain(tc.tip)
			o := testOpts()
			tc.opts(ch, &o)
			c := New(o, newMapStore(), ch, ch.head, nil)
			for i := 0; i < tc.warm; i++ {
				c.Tick(context.Background())
			}
			require.True(t, c.Fresh())
			sub := c.Subscribe(8)
			tc.change(ch)
			c.Tick(context.Background())
			var added []int64
			open := true
			for open {
				select {
				case ev, ok := <-sub.C:
					open = ok
					for _, r := range ev.Added {
						added = append(added, r.Number)
					}
				default:
					require.Equal(t, tc.open, open)
					require.Equal(t, tc.added, added)
					return
				}
			}
			require.False(t, tc.open, "subscriber closed")
		})
	}
}

type payloadErrStore struct {
	*fakeFleetStore
	mu  sync.Mutex
	err error
}

func (s *payloadErrStore) setErr(err error) { s.mu.Lock(); s.err = err; s.mu.Unlock() }
func (s *payloadErrStore) GetBlock(ctx context.Context, scope Scope, hash string) (*BlockRecord, error) {
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.fakeFleetStore.GetBlock(ctx, scope, hash)
}

func TestCache_FleetFollowerPayloadReadErrorOnAdvance(t *testing.T) {
	unavailable := fmt.Errorf("redis timeout: %w", ErrStoreUnavailable)
	for _, tc := range []struct {
		name   string
		err    error
		reorg  bool
		retain bool
	}{
		{"unavailable store on same-branch advance keeps view", unavailable, false, true},
		{"unavailable store on conflicting snapshot fails closed", unavailable, true, false},
		{"unclassified read error fails closed", fmt.Errorf("boom"), false, false},
		{"not found fails closed", ErrNotFound, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &payloadErrStore{fakeFleetStore: newFakeFleetStore()}
			store.leader = true
			chain := newFakeChain(5)
			leader := New(testOpts(), store, chain, chain.head, nil)
			leader.Tick(context.Background())
			defer leader.Stop()
			follower := New(testOpts(), store, chain, nil, nil)
			follower.Tick(context.Background())
			require.Equal(t, int64(5), follower.Head())
			sub := follower.Subscribe(4)

			if tc.reorg {
				chain.reorg(5, "fork")
			} else {
				chain.mine(1)
			}
			leader.Tick(context.Background())
			store.setErr(tc.err)
			follower.Tick(context.Background())

			if !tc.retain {
				require.False(t, follower.Fresh())
				_, open := <-sub.C
				require.False(t, open)
				return
			}
			require.Equal(t, int64(5), follower.Head(), "previous validated view keeps serving")
			select {
			case _, open := <-sub.C:
				require.True(t, open, "subscription stays open")
				t.Fatal("no event expected for an unadvanced view")
			default:
			}
			now := time.Now()
			follower.nowFn = func() time.Time { return now.Add(testOpts().MaxStaleness + time.Second) }
			require.False(t, follower.Fresh(), "the failed attempt must not extend freshness")
			follower.nowFn = time.Now

			store.setErr(nil)
			follower.Tick(context.Background())
			require.Equal(t, int64(6), follower.Head())
			ev := <-sub.C
			require.Len(t, ev.Added, 1)
			require.Equal(t, hashOf(6, "a"), ev.Added[0].Hash)
		})
	}
}

func TestCache_FleetFollowerUnchangedSnapshotExpiresAtLeaderTimestamp(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	leader := New(testOpts(), store, chain, chain.head, nil)
	leader.Tick(context.Background())
	leader.Stop()
	store.muFleet.Lock()
	store.leader = false
	store.snap.At = time.Now()
	at := store.snap.At
	store.muFleet.Unlock()

	follower := New(testOpts(), store, chain, nil, nil)
	for _, d := range []time.Duration{0, time.Second, 1500 * time.Millisecond} {
		follower.nowFn = func() time.Time { return at.Add(d) }
		follower.Tick(context.Background())
		require.True(t, follower.Fresh())
	}
	max := testOpts().MaxStaleness
	follower.nowFn = func() time.Time { return at.Add(max) }
	require.True(t, follower.Fresh(), "fresh through leader timestamp + MaxStaleness")
	follower.nowFn = func() time.Time { return at.Add(max + time.Nanosecond) }
	require.False(t, follower.Fresh(), "re-reading an unchanged snapshot must not extend freshness")
}
