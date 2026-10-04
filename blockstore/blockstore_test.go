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
	"github.com/stretchr/testify/assert"
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
	logCalls  int
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

// HeaderByNumber returns eth_getBlockByNumber(n, false): transactions as hashes.
func (c *fakeChain) HeaderByNumber(_ context.Context, n int64) (json.RawMessage, error) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headCalls++
	raw, err := c.blockLocked(n)
	if err != nil {
		return nil, err
	}
	var block map[string]json.RawMessage
	if json.Unmarshal(raw, &block) != nil {
		return raw, nil
	}
	var txs []map[string]interface{}
	if json.Unmarshal(block["transactions"], &txs) == nil && txs != nil {
		hashes := make([]string, 0, len(txs))
		for _, tx := range txs {
			hashes = append(hashes, tx["hash"].(string))
		}
		block["transactions"], _ = json.Marshal(hashes)
	}
	return json.Marshal(block)
}
func (c *fakeChain) LogsByBlockHash(_ context.Context, hash string) (json.RawMessage, error) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logCalls++
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
func (c *fakeChain) counts() (head, body, logs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headCalls, c.bodyCalls, c.logCalls
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
	mu       sync.Mutex
	payloads map[string]json.RawMessage
	puts     map[PayloadKind]int
	locks    map[string]bool
	lockErr  error
}

func newMapStore() *mapStore {
	return &mapStore{payloads: map[string]json.RawMessage{}, puts: map[PayloadKind]int{}, locks: map[string]bool{}}
}

func payloadKey(kind PayloadKind, hash string) string { return string(kind) + "/" + normHash(hash) }

func (s *mapStore) PutPayload(_ context.Context, _ Scope, kind PayloadKind, hash string, raw json.RawMessage, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts[kind]++
	s.payloads[payloadKey(kind, hash)] = append(json.RawMessage(nil), raw...)
	return nil
}

func (s *mapStore) GetPayload(_ context.Context, _ Scope, kind PayloadKind, hash string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.payloads[payloadKey(kind, hash)]
	if !ok {
		return nil, ErrNotFound
	}
	return append(json.RawMessage(nil), raw...), nil
}

func (s *mapStore) putCount(kind PayloadKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[kind]
}

func (s *mapStore) set(kind PayloadKind, hash string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloads[payloadKey(kind, hash)] = raw
}

func (s *mapStore) drop(kind PayloadKind, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.payloads, payloadKey(kind, hash))
}

// lockingStore adds the cross-replica fill lock the Redis store provides.
type lockingStore struct{ *mapStore }

func (s *lockingStore) TryLock(_ context.Context, _ Scope, key string, _ time.Duration) (func(context.Context), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockErr != nil {
		return nil, false, s.lockErr
	}
	if s.locks[key] {
		return nil, false, nil
	}
	s.locks[key] = true
	return func(context.Context) {
		s.mu.Lock()
		delete(s.locks, key)
		s.mu.Unlock()
	}, true, nil
}

func (s *lockingStore) Locked(_ context.Context, _ Scope, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.locks[key], nil
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

func testOpts() Options {
	return Options{Scope: Scope{Namespace: "t", ProjectId: "p", NetworkId: "evm:1"}, Depth: 8, MaxPerTick: 8,
		MaxBytes: 1 << 20, MaxBlockSize: 1 << 16, PollInterval: time.Second, FetchTimeout: time.Second,
		MaxStaleness: 2 * time.Second, MaxLogsRange: 8, RecordTTL: time.Hour}
}

func ctxb() context.Context { return context.Background() }

// drain returns the events currently queued on a subscription and whether it is still open.
func drain(sub *Subscription) ([]Event, bool) {
	var out []Event
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return out, false
			}
			out = append(out, ev)
		default:
			return out, true
		}
	}
}

// --- background cost ---------------------------------------------------------

// With no clients, the background refresh fetches exactly one header per new
// block (plus one per walked-back height on a reorg) and never a block body or logs.
func TestCache_BackgroundFetchesOnlyHeaders(t *testing.T) {
	ch := newFakeChain(20)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	require.True(t, c.Fresh())
	require.Equal(t, int64(20), c.Head())
	head, body, logs := ch.counts()
	require.Equal(t, int(testOpts().Depth), head, "cold start fetches one header per window height")

	const n = 25
	for i := 0; i < n; i++ {
		ch.mine(1)
		c.Tick(ctxb())
		c.Tick(ctxb()) // an unchanged tip costs nothing
	}
	h2, b2, l2 := ch.counts()
	require.Equal(t, head+n, h2, "exactly one header per new block")
	require.Equal(t, body, b2)
	require.Equal(t, logs, l2)
	require.Zero(t, b2, "the background never fetches a full block")
	require.Zero(t, l2, "the background never fetches logs")

	// A same-height reorg of the tip is found when the next block does not
	// link: one new header plus one walked-back header.
	ch.reorg(45, "b")
	ch.mine(1)
	c.Tick(ctxb())
	h3, b3, l3 := ch.counts()
	require.Equal(t, h2+2, h3)
	require.Zero(t, b3+l3)
	require.Equal(t, hashOf(45, "b"), c.CanonicalHash(45))
	require.Equal(t, hashOf(46, "a"), c.CanonicalHash(46))
}

func TestCache_TipAdvanceExtendsVerifiedWindowIncrementally(t *testing.T) {
	ch := newFakeChain(10)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	require.True(t, c.Fresh())

	ch.mine(1)
	before, _, _ := ch.counts()
	c.Tick(ctxb())
	after, _, _ := ch.counts()
	require.Equal(t, before+1, after, "a linked one-block advance fetches only the new tip header")
	require.Equal(t, int64(11), c.Head())
	_, ok := c.LogsRange(ctxb(), 4, 11, nil)
	require.True(t, ok, "extended window stays complete across the full depth")

	ch.mine(3)
	before = after
	c.Tick(ctxb())
	after, _, _ = ch.counts()
	require.Equal(t, before+3, after, "a linked multi-block advance fetches only new headers")
	require.Equal(t, int64(14), c.Head())

	// A reorg below the new tip breaks the parent link and walks back only
	// the changed heights.
	sub := c.Subscribe(8)
	ch.reorg(13, "b")
	ch.mine(1)
	before = after
	c.Tick(ctxb())
	after, _, _ = ch.counts()
	require.Equal(t, before+3, after, "new tip plus two walked-back headers")
	evs, open := drain(sub)
	require.True(t, open)
	require.Len(t, evs, 1)
	require.Equal(t, hashOf(14, "a"), evs[0].Removed[0].Hash)
	require.Equal(t, hashOf(13, "a"), evs[0].Removed[1].Hash)
	require.Len(t, evs[0].Added, 3)
	_, ok = c.BlockByHash(ctxb(), hashOf(13, "b"), false)
	require.True(t, ok)
	_, ok = c.BlockByHash(ctxb(), hashOf(13, "a"), false)
	require.False(t, ok)
}

// --- on-demand payloads ------------------------------------------------------

// full=false is rendered from the verified header with no upstream call.
func TestCache_HashOnlyBlockServedFromHeader(t *testing.T) {
	ch := newFakeChain(10)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	_, body0, logs0 := ch.counts()
	for n := int64(3); n <= 10; n++ {
		raw, ok := c.BlockByNumber(ctxb(), n, false)
		require.True(t, ok)
		require.Contains(t, string(raw), hashOf(n, "a"))
		require.Contains(t, string(raw), txHashOf(n, "a"))
		require.NotContains(t, string(raw), `"from"`)
		raw, ok = c.BlockByHash(ctxb(), hashOf(n, "a"), false)
		require.True(t, ok)
		require.Contains(t, string(raw), hashOf(n, "a"))
	}
	_, body, logs := ch.counts()
	require.Equal(t, body0, body)
	require.Equal(t, logs0, logs)
	_, ok := c.BlockByNumber(ctxb(), 2, false)
	require.False(t, ok, "heights below the window are misses")
	_, ok = c.BlockByNumber(ctxb(), 11, false)
	require.False(t, ok, "heights above the window are misses")
}

// A full block for a window height is fetched once, then served from the
// local cache, to concurrent callers, and to another replica through the
// shared store.
func TestCache_BlockMissFetchesOnce(t *testing.T) {
	ch := newFakeChain(10)
	store := newMapStore()
	a := New(testOpts(), store, ch, ch.head, nil)
	a.Tick(ctxb())
	_, body0, _ := ch.counts()

	raw, ok := a.BlockByNumber(ctxb(), 9, true)
	require.True(t, ok)
	require.Contains(t, string(raw), `"from"`)
	_, body, _ := ch.counts()
	require.Equal(t, body0+1, body, "a miss fetches the full block once")
	require.Equal(t, 1, store.putCount(PayloadBlock))
	for i := 0; i < 3; i++ {
		_, ok = a.BlockByNumber(ctxb(), 9, true)
		require.True(t, ok)
		_, ok = a.BlockByHash(ctxb(), hashOf(9, "a"), true)
		require.True(t, ok)
	}
	_, body2, _ := ch.counts()
	require.Equal(t, body, body2, "later reads are local hits")

	// Concurrent misses for one height coalesce into one fetch.
	ch.mu.Lock()
	ch.delay = 20 * time.Millisecond
	ch.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok := a.BlockByNumber(ctxb(), 8, true)
			assert.True(t, ok)
		}()
	}
	wg.Wait()
	_, body3, _ := ch.counts()
	require.Equal(t, body2+1, body3, "concurrent misses share one fetch")

	// Another replica on the same store reuses the payload.
	b := New(testOpts(), store, ch, ch.head, nil)
	b.Tick(ctxb())
	_, ok = b.BlockByNumber(ctxb(), 9, true)
	require.True(t, ok)
	_, ok = b.BlockByNumber(ctxb(), 8, true)
	require.True(t, ok)
	_, body4, _ := ch.counts()
	require.Equal(t, body3, body4, "a second replica reads the shared payload")
	require.Equal(t, int64(2), a.Stats.Hydrated.Load())
	require.Zero(t, b.Stats.Hydrated.Load())

	// A corrupt shared payload is rejected and refetched.
	store.set(PayloadBlock, hashOf(7, "a"), json.RawMessage(`{"number":"0x7"}`))
	_, ok = b.BlockByNumber(ctxb(), 7, true)
	require.True(t, ok)
	_, body5, _ := ch.counts()
	require.Equal(t, body4+1, body5)
	require.Positive(t, b.Stats.Rejected.Load())
}

// Two replicas missing the same block at once fetch it once via the fill lock.
func TestCache_ReplicasCoalesceMissesThroughFillLock(t *testing.T) {
	ch := newFakeChain(10)
	ch.delay = 30 * time.Millisecond
	store := &lockingStore{newMapStore()}
	opts := testOpts()
	opts.PeerWait = 2 * time.Second
	a := New(opts, store, ch, ch.head, nil)
	b := New(opts, store, ch, ch.head, nil)
	a.Tick(ctxb())
	b.Tick(ctxb())
	_, body0, logs0 := ch.counts()
	var wg sync.WaitGroup
	for _, c := range []*Cache{a, b, a, b} {
		wg.Add(2)
		go func(c *Cache) {
			defer wg.Done()
			_, ok := c.BlockByNumber(ctxb(), 10, true)
			assert.True(t, ok)
		}(c)
		go func(c *Cache) {
			defer wg.Done()
			_, ok := c.LogsByHash(ctxb(), hashOf(10, "a"), nil)
			assert.True(t, ok)
		}(c)
	}
	wg.Wait()
	_, body, logs := ch.counts()
	require.Equal(t, body0+1, body, "one body fetch across replicas")
	require.Equal(t, logs0+1, logs, "one logs fetch across replicas")

	// A lock error falls back to a local fetch rather than failing.
	store.mu.Lock()
	store.lockErr = fmt.Errorf("redis down")
	store.mu.Unlock()
	_, ok := a.BlockByNumber(ctxb(), 9, true)
	require.True(t, ok)
}

// eth_getLogs over window heights fetches each block's logs once by hash,
// then serves any filter from the cache.
func TestCache_LogsRangeFetchesOncePerBlockHash(t *testing.T) {
	ch := newFakeChain(10)
	store := newMapStore()
	c := New(testOpts(), store, ch, ch.head, nil)
	c.Tick(ctxb())
	_, _, logs0 := ch.counts()
	all, ok := c.LogsRange(ctxb(), 7, 10, nil)
	require.True(t, ok)
	require.Len(t, all, 4)
	_, _, logs := ch.counts()
	require.Equal(t, logs0+4, logs, "one logs fetch per block hash")
	even, err := ParseLogFilter(map[string]interface{}{"topics": []interface{}{topicA}})
	require.NoError(t, err)
	got, ok := c.LogsRange(ctxb(), 7, 10, even)
	require.True(t, ok)
	require.Len(t, got, 2)
	got, ok = c.LogsRange(ctxb(), 8, 9, nil)
	require.True(t, ok)
	require.Len(t, got, 2)
	got, ok = c.LogsByHash(ctxb(), hashOf(10, "a"), even)
	require.True(t, ok)
	require.Len(t, got, 1)
	_, _, logs2 := ch.counts()
	require.Equal(t, logs, logs2, "cached logs answer every filter")
	_, body, _ := ch.counts()
	require.Zero(t, body, "logs never need a block body")

	// Overlapping range: only the new heights are fetched.
	_, ok = c.LogsRange(ctxb(), 5, 8, nil)
	require.True(t, ok)
	_, _, logs3 := ch.counts()
	require.Equal(t, logs2+2, logs3)

	_, ok = c.LogsRange(ctxb(), 0, 3, nil)
	require.False(t, ok, "a range below the window is a miss")
	_, ok = c.LogsRange(ctxb(), 9, 11, nil)
	require.False(t, ok, "partial ranges are not hits")
	_, ok = c.LogsRange(ctxb(), 0, int64(^uint64(0)>>1), nil)
	require.False(t, ok)
}

func TestCache_IncompleteLogsAreNeverServed(t *testing.T) {
	ch := newFakeChain(5)
	ch.dropLogs[normHash(hashOf(5, "a"))] = true
	store := newMapStore()
	c := New(testOpts(), store, ch, ch.head, nil)
	c.Tick(ctxb())
	_, ok := c.LogsRange(ctxb(), 4, 5, nil)
	require.False(t, ok)
	_, ok = c.LogsByHash(ctxb(), hashOf(5, "a"), nil)
	require.False(t, ok)
	require.Positive(t, c.Stats.Rejected.Load())
	require.Zero(t, store.putCount(PayloadLogs)-1, "only the valid height's logs are stored")
}

func TestCache_OversizedBlockIsAMiss(t *testing.T) {
	ch := newFakeChain(5)
	ch.oversized[4] = true
	o := testOpts()
	o.MaxBlockSize = 1500
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	require.True(t, c.Fresh())
	_, ok := c.BlockByNumber(ctxb(), 4, true)
	require.False(t, ok, "a body over maxBlockBytes is never served")
	_, ok = c.BlockByNumber(ctxb(), 5, true)
	require.True(t, ok)
	_, ok = c.BlockByNumber(ctxb(), 4, false)
	require.True(t, ok, "its header is still served")
}

func TestCache_PayloadMemoryIsBounded(t *testing.T) {
	ch := newFakeChain(10)
	block, _ := ch.BlockByNumber(ctxb(), 10)
	o := testOpts()
	o.MaxBytes = int64(len(block))*2 + 10
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	for n := int64(5); n <= 10; n++ {
		_, ok := c.BlockByNumber(ctxb(), n, true)
		require.True(t, ok)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	require.LessOrEqual(t, c.payloadBytes, o.MaxBytes)
	require.Len(t, c.bodies, 2)
	require.NotNil(t, c.bodies[normHash(hashOf(10, "a"))], "the lowest heights are evicted first")
}

func TestCache_MixedCaseBodyMatchesVerifiedHeader(t *testing.T) {
	ch := newFakeChain(5)
	ch.mixedCase = true
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	require.True(t, c.Fresh())
	_, ok := c.BlockByNumber(ctxb(), 5, true)
	require.True(t, ok)
}

func TestCache_BlockWithoutTransactionsArrayNeverServes(t *testing.T) {
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
			leader.Tick(ctxb())
			_, ok := leader.BlockByNumber(ctxb(), 5, true)
			require.Equal(t, tc.serve, ok)
			if tc.serve {
				_, ok = leader.LogsRange(ctxb(), 5, 5, nil)
				require.True(t, ok)
				return
			}
			require.Positive(t, leader.Stats.Rejected.Load())
			require.Nil(t, store.snap, "a writer must not publish the window")

			// A follower must also reject such a header written by an older replica.
			raw, err := chain.HeaderByNumber(ctxb(), 5)
			require.NoError(t, err)
			hash := normHash(hashOf(5, "a"))
			store.set(PayloadHeader, hash, raw)
			store.snap = &Snapshot{Head: 5, Hashes: []string{hash}, At: time.Now()}
			follower := New(testOpts(), store, chain, chain.head, nil)
			follower.Tick(ctxb())
			_, ok = follower.BlockByNumber(ctxb(), 5, false)
			require.False(t, ok)
		})
	}
}

// --- reorgs ------------------------------------------------------------------

// Walk-back replaces orphaned hashes; bodies and logs cached for them are
// never served again.
func TestCache_ReorgDropsOrphanedPayloads(t *testing.T) {
	ch := newFakeChain(10)
	store := newMapStore()
	c := New(testOpts(), store, ch, ch.head, nil)
	c.Tick(ctxb())
	orphan := hashOf(9, "a")
	_, ok := c.BlockByNumber(ctxb(), 9, true)
	require.True(t, ok)
	_, ok = c.LogsRange(ctxb(), 9, 10, nil)
	require.True(t, ok)

	ch.reorg(9, "b")
	ch.mine(1)
	c.Tick(ctxb())
	require.Equal(t, hashOf(9, "b"), c.CanonicalHash(9))
	_, ok = c.BlockByHash(ctxb(), orphan, true)
	require.False(t, ok)
	_, ok = c.BlockByHash(ctxb(), orphan, false)
	require.False(t, ok)
	_, ok = c.LogsByHash(ctxb(), orphan, nil)
	require.False(t, ok)
	raw, ok := c.BlockByNumber(ctxb(), 9, true)
	require.True(t, ok)
	require.Contains(t, string(raw), hashOf(9, "b"))
	require.NotContains(t, string(raw), orphan)
	logs, ok := c.LogsRange(ctxb(), 8, 11, nil)
	require.True(t, ok)
	require.NotContains(t, strings.Join(rawStrings(logs), ""), orphan)
	require.Contains(t, strings.Join(rawStrings(logs), ""), hashOf(9, "b"))
}

func rawStrings(in []json.RawMessage) []string {
	out := make([]string, len(in))
	for i, r := range in {
		out[i] = string(r)
	}
	return out
}

// A body fetched for a height whose window hash is stale (same-height reorg
// not yet linked by a new block) is rejected, and the next tick re-verifies the tip.
func TestCache_StaleTipBodyTriggersReverification(t *testing.T) {
	ch := newFakeChain(10)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	ch.reorg(10, "b")
	_, ok := c.BlockByNumber(ctxb(), 10, true)
	require.False(t, ok, "a body that disagrees with the window is not served")
	require.True(t, c.suspect.Load())
	sub := c.Subscribe(4)
	c.Tick(ctxb())
	require.Equal(t, hashOf(10, "b"), c.CanonicalHash(10))
	evs, open := drain(sub)
	require.True(t, open)
	require.Len(t, evs, 1)
	require.Equal(t, hashOf(10, "a"), evs[0].Removed[0].Hash)
	require.Equal(t, hashOf(10, "b"), evs[0].Added[0].Hash)
	_, ok = c.BlockByNumber(ctxb(), 10, true)
	require.True(t, ok)
}

func TestCache_ReorgEvents(t *testing.T) {
	ch := newFakeChain(10)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	sub := c.Subscribe(8)
	ch.reorg(10, "b")
	ch.mine(1)
	c.Tick(ctxb())
	ev := <-sub.C
	require.Equal(t, hashOf(10, "a"), ev.Removed[0].Hash)
	require.Equal(t, hashOf(10, "b"), ev.Added[0].Hash)
	require.Equal(t, hashOf(11, "a"), ev.Added[1].Hash)

	ch.reorg(7, "c")
	ch.mine(1)
	c.Tick(ctxb())
	ev = <-sub.C
	require.Len(t, ev.Removed, 5)
	require.Equal(t, int64(11), ev.Removed[0].Number)
	require.Equal(t, int64(7), ev.Removed[4].Number)
	require.Len(t, ev.Added, 6)
	require.Equal(t, int64(7), ev.Added[0].Number)
	require.Equal(t, int64(12), ev.Added[5].Number)
	_, ok := c.BlockByHash(ctxb(), hashOf(10, "b"), false)
	require.False(t, ok)
}

func TestCache_DeepReorgOutsideWindowClosesSubscribers(t *testing.T) {
	ch := newFakeChain(10)
	o := testOpts()
	o.Depth = 4
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	sub := c.Subscribe(8)
	ch.reorg(1, "fork")
	ch.mine(1)
	c.Tick(ctxb())
	_, open := drain(sub)
	require.False(t, open, "cannot emit a complete reorg outside retained window")
	require.Equal(t, int64(11), c.Head())
	require.Equal(t, hashOf(8, "fork"), c.CanonicalHash(8))
}

func TestCache_TipRegressionStopsServingAboveVerifiedTip(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	ch := newFakeChain(10)
	c := New(testOpts(), store, ch, ch.head, nil)
	c.Tick(ctxb())
	require.Equal(t, int64(10), c.Head())
	sub := c.Subscribe(8)
	follower := New(testOpts(), store, ch, ch.head, nil)
	follower.Tick(ctxb())
	followerSub := follower.Subscribe(8)

	ch.mu.Lock()
	ch.tip = 8
	ch.mu.Unlock()
	c.Tick(ctxb())
	follower.Tick(ctxb())
	require.True(t, c.Fresh(), "the prefix up to the matching tip stays verified")
	require.Equal(t, int64(8), c.Head())
	_, ok := c.BlockByNumber(ctxb(), 9, false)
	require.False(t, ok, "blocks above the live tip are not served")
	_, ok = c.BlockByHash(ctxb(), hashOf(10, "a"), false)
	require.False(t, ok)
	_, ok = c.LogsRange(ctxb(), 7, 9, nil)
	require.False(t, ok)
	_, ok = c.LogsRange(ctxb(), 3, 8, nil)
	require.True(t, ok)
	require.Equal(t, int64(8), store.snap.Head, "followers receive the trimmed snapshot")
	require.Equal(t, int64(8), follower.Head())
	for _, stream := range []*Subscription{sub, followerSub} {
		_, open := drain(stream)
		require.False(t, open, "a lagging observation must not fabricate removed logs")
	}

	ch.mu.Lock()
	ch.tip = 10
	ch.mu.Unlock()
	c.Tick(ctxb())
	follower.Tick(ctxb())
	require.Equal(t, int64(10), c.Head())
	require.Equal(t, int64(10), follower.Head())
}

func TestCache_GapAfterHeadJumpClosesSubscribers(t *testing.T) {
	ch := newFakeChain(1)
	o := testOpts()
	o.Depth = 4
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	sub := c.Subscribe(4)
	ch.mine(6)
	c.Tick(ctxb())
	_, open := drain(sub)
	require.False(t, open)
	require.Equal(t, int64(7), c.Head())
}

func TestCache_StaleViewClosesSubscribers(t *testing.T) {
	ch := newFakeChain(5)
	tip := int64(5)
	c := New(testOpts(), newMapStore(), ch, func(context.Context) int64 { return tip }, nil)
	c.Tick(ctxb())
	sub := c.Subscribe(2)
	tip = -1
	c.nowFn = func() time.Time { return time.Now().Add(10 * time.Second) }
	c.Tick(ctxb())
	require.False(t, c.Fresh())
	_, open := drain(sub)
	require.False(t, open)
}

// --- cold start --------------------------------------------------------------

// maxPerTick bounds header backfill; the newest suffix is served while older
// heights fill over later ticks, and subscribers stay open.
func TestCache_ColdStartBackfillsHeadersWithinMaxPerTick(t *testing.T) {
	ch := newFakeChain(10)
	o := testOpts()
	o.MaxPerTick = 2
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	require.True(t, c.Fresh())
	head, _, _ := ch.counts()
	require.Equal(t, 3, head, "the tip plus maxPerTick older headers")
	require.True(t, c.snap.Incomplete)
	_, ok := c.LogsRange(ctxb(), 8, 10, nil)
	require.True(t, ok)
	_, ok = c.BlockByNumber(ctxb(), 5, false)
	require.False(t, ok, "heights not yet backfilled are misses")
	sub := c.Subscribe(8)
	c.Tick(ctxb())
	ch.mine(1)
	c.Tick(ctxb())
	evs, open := drain(sub)
	require.True(t, open, "backfill and new heads keep a subscription open")
	require.Len(t, evs, 1)
	require.Equal(t, int64(11), evs[0].Added[0].Number)
	c.Tick(ctxb())
	require.False(t, c.snap.Incomplete)
	require.Len(t, c.snap.Hashes, int(o.Depth))
	_, body, logs := ch.counts()
	require.Zero(t, body)
	require.Equal(t, 3, logs, "only the explicit LogsRange(8,10) fetched logs")
}

// --- fleet -------------------------------------------------------------------

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
	headerBg := telemetry.MetricBlockStoreFetchTotal.WithLabelValues(project, network, "header", FetchReasonBackground)
	blockMiss := telemetry.MetricBlockStoreFetchTotal.WithLabelValues(project, network, "block", FetchReasonMiss)
	logsMiss := telemetry.MetricBlockStoreFetchTotal.WithLabelValues(project, network, "logs", FetchReasonMiss)
	initialFresh, initialStale, initialError := testutil.ToFloat64(refreshFresh), testutil.ToFloat64(refreshStale), testutil.ToFloat64(refreshError)
	initialHeaders, initialBlocks, initialLogs := testutil.ToFloat64(headerBg), testutil.ToFloat64(blockMiss), testutil.ToFloat64(logsMiss)

	cache.Tick(ctxb())
	require.True(t, cache.Fresh())
	require.Equal(t, float64(1), testutil.ToFloat64(fresh))
	require.Equal(t, initialFresh+1, testutil.ToFloat64(refreshFresh))
	require.Equal(t, initialStale, testutil.ToFloat64(refreshStale))
	require.Equal(t, initialError, testutil.ToFloat64(refreshError))
	require.Equal(t, initialHeaders+6, testutil.ToFloat64(headerBg), "heights 0..5")

	_, ok := cache.BlockByNumber(ctxb(), 5, true)
	require.True(t, ok)
	_, ok = cache.LogsRange(ctxb(), 4, 5, nil)
	require.True(t, ok)
	require.Equal(t, initialBlocks+1, testutil.ToFloat64(blockMiss))
	require.Equal(t, initialLogs+2, testutil.ToFloat64(logsMiss))

	liveTip = -1
	cache.nowFn = func() time.Time { return time.Now().Add(10 * opts.MaxStaleness) }
	cache.Tick(ctxb())
	require.False(t, cache.Fresh())
	require.Equal(t, float64(0), testutil.ToFloat64(fresh))
	require.Equal(t, initialError+1, testutil.ToFloat64(refreshError))
	require.Equal(t, initialFresh+1, testutil.ToFloat64(refreshFresh))
	require.Equal(t, initialStale, testutil.ToFloat64(refreshStale))

	cache.Stop()
	require.Equal(t, float64(0), testutil.ToFloat64(fresh))
}

func TestCache_FleetLeaderFollowerAndFailover(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	leader := New(testOpts(), store, chain, chain.head, nil)
	leader.Tick(ctxb())
	require.True(t, leader.Fresh())
	require.NotNil(t, store.snap)
	head, body, logs := chain.counts()
	require.Positive(t, head)
	require.Zero(t, body+logs, "the leader publishes headers only")
	_, err := store.GetPayload(ctxb(), testOpts().Scope, PayloadHeader, hashOf(5, "a"))
	require.NoError(t, err)

	followerChain := newFakeChain(5)
	follower := New(testOpts(), store, followerChain, followerChain.head, nil)
	follower.Tick(ctxb())
	require.True(t, follower.Fresh())
	require.Equal(t, int64(5), follower.Head())
	_, ok := follower.BlockByNumber(ctxb(), 5, false)
	require.True(t, ok)
	fh, fb, fl := followerChain.counts()
	require.Zero(t, fh+fb+fl, "follower makes no upstream calls for headers or hash-only blocks")

	sub := leader.Subscribe(2)
	leader.Stop()
	require.Equal(t, 1, store.releases)
	followerChain.mine(1)
	follower.Tick(ctxb())
	require.True(t, follower.Fresh(), "the follower can acquire and refresh after leader release")
	fh, _, _ = followerChain.counts()
	require.Equal(t, 1, fh, "a promoted follower extends the published window by one header")
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
	leader.Tick(ctxb())
	require.True(t, leader.Fresh())
	require.NotNil(t, leader.lease)

	broken.Store(true)
	for i := 1; i < maxLeaderFailures; i++ {
		leader.Tick(ctxb())
		require.NotNil(t, leader.lease, "a transient failure (%d) keeps the lease", i)
		require.Zero(t, store.releases)
	}
	leader.Tick(ctxb())
	require.Nil(t, leader.lease, "the sick leader releases after %d consecutive failures", maxLeaderFailures)
	require.Equal(t, 1, store.releases)

	followerChain := newFakeChain(6)
	follower := New(testOpts(), store, followerChain, followerChain.head, nil)
	follower.Tick(ctxb())
	require.NotNil(t, follower.lease, "a healthy replica acquires the released lease")
	require.True(t, follower.Fresh())
	require.Equal(t, int64(6), follower.Head())
}

// A leader whose new tip header keeps failing publishes nothing; it must
// step down so a healthy replica takes over.
func TestCache_FleetLeaderStepsDownWhenTipHeaderFails(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	leader := New(testOpts(), store, chain, chain.head, nil)
	leader.Tick(ctxb())
	require.True(t, leader.Fresh())
	chain.mine(1)
	chain.mu.Lock()
	chain.failBlock[6] = true
	chain.mu.Unlock()
	for i := 1; i < maxLeaderFailures; i++ {
		leader.Tick(ctxb())
		require.NotNil(t, leader.lease, "tick %d keeps the lease", i)
	}
	leader.Tick(ctxb())
	require.Nil(t, leader.lease)
	require.Equal(t, 1, store.releases)
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
	c.Tick(ctxb())
	for round := 0; round < 3; round++ {
		broken.Store(true)
		for i := 1; i < maxLeaderFailures; i++ {
			c.Tick(ctxb())
		}
		broken.Store(false)
		c.Tick(ctxb())
		require.NotNil(t, c.lease, "interleaved successes reset the count (round %d)", round)
	}
	require.Zero(t, store.releases)
}

func TestCache_FleetLeaderDoesNotRewriteUnchangedHeadersEveryTick(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	c := New(testOpts(), store, chain, chain.head, nil)
	c.Tick(ctxb())
	initialWrites := store.putCount(PayloadHeader)
	require.Equal(t, len(c.snap.Hashes), initialWrites, "takeover writes each header once")
	c.Tick(ctxb())
	require.Equal(t, initialWrites, store.putCount(PayloadHeader), "unchanged headers are not rewritten on each tick")
	chain.mine(1)
	c.Tick(ctxb())
	require.Equal(t, initialWrites+1, store.putCount(PayloadHeader), "a new block writes one header")
}

func TestCache_FleetTakeoverContinuesIncompleteColdFill(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	opts := testOpts()
	opts.Depth, opts.MaxLogsRange, opts.MaxPerTick = 6, 6, 2
	leader := New(opts, store, chain, chain.head, nil)
	leader.Tick(ctxb())
	require.True(t, store.snap.Incomplete)
	require.Len(t, store.snap.Hashes, 3)

	follower := New(opts, store, chain, chain.head, nil)
	follower.Tick(ctxb())
	before, _, _ := chain.counts()
	leader.Stop()
	follower.Tick(ctxb())
	require.True(t, follower.Fresh())
	require.True(t, store.snap.Incomplete)
	require.Len(t, follower.snap.Hashes, 5, "promoted follower backfills the next older chunk")
	after, _, _ := chain.counts()
	require.Equal(t, before+2, after)
}

func TestCache_FleetFollowerFailsClosedForStaleOrMissingHeader(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			store := newFakeFleetStore()
			store.leader = true
			leader := New(testOpts(), store, newFakeChain(5), func(context.Context) int64 { return 5 }, nil)
			leader.Tick(ctxb())
			leader.Stop()
			store.muFleet.Lock()
			store.leader = false
			store.muFleet.Unlock()
			if missing {
				store.drop(PayloadHeader, hashOf(5, "a"))
			} else {
				store.muFleet.Lock()
				store.snap.At = time.Now().Add(-time.Hour)
				store.muFleet.Unlock()
			}
			follower := New(testOpts(), store, newFakeChain(5), func(context.Context) int64 { return 5 }, nil)
			sub := follower.Subscribe(1)
			follower.Tick(ctxb())
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
	leader.Tick(ctxb())
	follower := New(testOpts(), store, newFakeChain(5), nil, nil)
	follower.Tick(ctxb())
	sub := follower.Subscribe(2)

	chain.reorg(5, "fork")
	chain.mine(1)
	leader.Tick(ctxb())
	follower.Tick(ctxb())
	ev := <-sub.C
	require.Len(t, ev.Removed, 1)
	require.Equal(t, hashOf(5, "a"), ev.Removed[0].Hash)
	require.Len(t, ev.Added, 2)
	require.Equal(t, hashOf(5, "fork"), ev.Added[0].Hash)
	leader.Stop()
}

func TestCache_FleetFollowerAcceptsUppercaseSnapshotAndHeaderHashes(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	chain := newFakeChain(5)
	leader := New(testOpts(), store, chain, func(context.Context) int64 { return 5 }, nil)
	leader.Tick(ctxb())

	raw, err := store.GetPayload(ctxb(), testOpts().Scope, PayloadHeader, hashOf(5, "a"))
	require.NoError(t, err)
	var block map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &block))
	block["hash"] = strings.ToUpper(block["hash"].(string))
	block["parentHash"] = strings.ToUpper(block["parentHash"].(string))
	raw, err = json.Marshal(block)
	require.NoError(t, err)
	store.set(PayloadHeader, hashOf(5, "a"), raw)

	store.muFleet.Lock()
	for i, hash := range store.snap.Hashes {
		store.snap.Hashes[i] = strings.ToUpper(hash)
	}
	store.muFleet.Unlock()
	follower := New(testOpts(), store, chain, nil, nil)
	follower.Tick(ctxb())
	require.True(t, follower.Fresh())
	require.Equal(t, int64(5), follower.Head())
	_, ok := follower.BlockByNumber(ctxb(), 5, true)
	require.True(t, ok)
	_, ok = follower.LogsRange(ctxb(), 4, 5, nil)
	require.True(t, ok)
	leader.Stop()
}

func TestCache_FleetFollowerClampsSmallFutureSnapshotSkew(t *testing.T) {
	store := newFakeFleetStore()
	store.leader = true
	leader := New(testOpts(), store, newFakeChain(5), func(context.Context) int64 { return 5 }, nil)
	leader.Tick(ctxb())
	now := time.Now()
	store.muFleet.Lock()
	store.snap.At = now.Add(2 * time.Second)
	store.muFleet.Unlock()

	follower := New(testOpts(), store, newFakeChain(5), nil, nil)
	follower.nowFn = func() time.Time { return now }
	follower.Tick(ctxb())
	require.True(t, follower.Fresh())
	follower.nowFn = func() time.Time { return now.Add(testOpts().MaxStaleness + time.Second) }
	require.False(t, follower.Fresh(), "future skew must not extend local staleness")
	leader.Stop()
}

type payloadErrStore struct {
	*fakeFleetStore
	mu  sync.Mutex
	err error
}

func (s *payloadErrStore) setErr(err error) { s.mu.Lock(); s.err = err; s.mu.Unlock() }
func (s *payloadErrStore) GetPayload(ctx context.Context, scope Scope, kind PayloadKind, hash string) (json.RawMessage, error) {
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.fakeFleetStore.GetPayload(ctx, scope, kind, hash)
}

type fleetReadErrStore struct {
	*fakeFleetStore
	acquireErr bool
	readErr    bool
}

func (s *fleetReadErrStore) Acquire(ctx context.Context, scope Scope, ttl time.Duration) (Lease, error) {
	if s.acquireErr {
		return nil, fmt.Errorf("transient acquire failure")
	}
	return s.fakeFleetStore.Acquire(ctx, scope, ttl)
}

func (s *fleetReadErrStore) ReadSnapshot(ctx context.Context, scope Scope) (*Snapshot, error) {
	if s.readErr {
		return nil, fmt.Errorf("transient snapshot failure")
	}
	return s.fakeFleetStore.ReadSnapshot(ctx, scope)
}

func TestCache_FleetErrorsRetainFreshFollowerView(t *testing.T) {
	for _, path := range []string{"acquire", "snapshot"} {
		t.Run(path, func(t *testing.T) {
			store := &fleetReadErrStore{fakeFleetStore: newFakeFleetStore()}
			store.leader = true
			chain := newFakeChain(5)
			leader := New(testOpts(), store, chain, chain.head, nil)
			leader.Tick(t.Context())
			defer leader.Stop()
			follower := New(testOpts(), store, chain, nil, nil)
			follower.Tick(t.Context())
			require.EqualValues(t, 5, follower.Head())
			originalFreshAt := follower.freshAt
			sub := follower.Subscribe(4)
			store.acquireErr = path == "acquire"
			store.readErr = path == "snapshot"
			follower.Tick(t.Context())
			require.True(t, follower.Fresh())
			require.Equal(t, originalFreshAt, follower.freshAt, "errors must not extend freshness")
			require.EqualValues(t, 5, follower.Head())
			require.Equal(t, 1, follower.SubscriberCount(), "subscriber remains open")
			now := originalFreshAt.Add(testOpts().MaxStaleness + time.Second)
			follower.nowFn = func() time.Time { return now }
			follower.Tick(t.Context())
			require.False(t, follower.Fresh())
			require.Equal(t, 0, follower.SubscriberCount())
			_, open := <-sub.C
			require.False(t, open)
		})
	}
}

func TestCache_FleetFollowerHeaderReadErrorOnAdvance(t *testing.T) {
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
			leader.Tick(ctxb())
			defer leader.Stop()
			follower := New(testOpts(), store, chain, nil, nil)
			follower.Tick(ctxb())
			require.Equal(t, int64(5), follower.Head())
			sub := follower.Subscribe(4)

			if tc.reorg {
				chain.reorg(5, "fork")
			}
			chain.mine(1)
			leader.Tick(ctxb())
			store.setErr(tc.err)
			follower.Tick(ctxb())

			if !tc.retain {
				require.False(t, follower.Fresh())
				_, open := <-sub.C
				require.False(t, open)
				return
			}
			require.Equal(t, int64(5), follower.Head(), "previous validated view keeps serving")
			_, open := drain(sub)
			require.True(t, open)
			now := time.Now()
			follower.nowFn = func() time.Time { return now.Add(testOpts().MaxStaleness + time.Second) }
			require.False(t, follower.Fresh(), "the failed attempt must not extend freshness")
			follower.nowFn = time.Now

			store.setErr(nil)
			follower.Tick(ctxb())
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
	leader.Tick(ctxb())
	leader.Stop()
	store.muFleet.Lock()
	store.leader = false
	store.snap.At = time.Now()
	at := store.snap.At
	store.muFleet.Unlock()

	follower := New(testOpts(), store, chain, nil, nil)
	for _, d := range []time.Duration{0, time.Second, 1500 * time.Millisecond} {
		follower.nowFn = func() time.Time { return at.Add(d) }
		follower.Tick(ctxb())
		require.True(t, follower.Fresh())
	}
	max := testOpts().MaxStaleness
	follower.nowFn = func() time.Time { return at.Add(max) }
	require.True(t, follower.Fresh(), "fresh through leader timestamp + MaxStaleness")
	follower.nowFn = func() time.Time { return at.Add(max + time.Nanosecond) }
	require.False(t, follower.Fresh(), "re-reading an unchanged snapshot must not extend freshness")
}

// --- subscriptions -----------------------------------------------------------

// newHeads subscribers never cause a logs fetch; logs subscribers cause one
// logs fetch per new block (shared by all of them); after the last logs
// subscriber leaves, nothing is fetched.
func TestCache_LogsFetchedOnlyForLogsSubscribers(t *testing.T) {
	ch := newFakeChain(10)
	c := New(testOpts(), newMapStore(), ch, ch.head, nil)
	c.Tick(ctxb())
	heads := c.Subscribe(16)
	for i := 0; i < 3; i++ {
		ch.mine(1)
		c.Tick(ctxb())
	}
	evs, open := drain(heads)
	require.True(t, open)
	require.Len(t, evs, 3)
	for _, ev := range evs {
		require.Nil(t, ev.Added[0].Logs, "newHeads events carry headers only")
	}
	_, body, logs := ch.counts()
	require.Zero(t, body+logs, "newHeads subscribers fetch nothing")
	require.Zero(t, c.LogsSubscriberCount())

	l1, l2 := c.SubscribeLogs(16), c.SubscribeLogs(16)
	require.Equal(t, 2, c.LogsSubscriberCount())
	for i := 0; i < 4; i++ {
		ch.mine(1)
		c.Tick(ctxb())
		for _, s := range []*Subscription{l1, l2} {
			ev := <-s.C
			full, ok := c.EventLogs(ctxb(), ev)
			require.True(t, ok)
			require.NotNil(t, full.Added[0].Logs)
		}
	}
	_, _, logs = ch.counts()
	require.Equal(t, 4, logs, "one logs fetch per new block, shared by all logs subscribers")
	_, _ = drain(heads)

	// A removed block's logs come from the cache, with no fetch.
	ch.reorg(17, "b")
	ch.mine(1)
	c.Tick(ctxb())
	ev := <-l1.C
	full, ok := c.EventLogs(ctxb(), ev)
	require.True(t, ok)
	require.Equal(t, hashOf(17, "a"), full.Removed[0].Hash)
	require.NotNil(t, full.Removed[0].Logs, "removed logs come from the cached entry")
	_, _, logs = ch.counts()
	require.Equal(t, 6, logs, "only the two added blocks are fetched")
	<-l2.C

	l1.Close()
	l2.Close()
	require.Zero(t, c.LogsSubscriberCount())
	for i := 0; i < 3; i++ {
		ch.mine(1)
		c.Tick(ctxb())
	}
	_, _, after := ch.counts()
	require.Equal(t, logs, after, "logs fetching stops after the last logs subscriber leaves")
	heads.Close()
	require.Zero(t, c.SubscriberCount())
}

func TestLogFilterGethSemantics(t *testing.T) {
	filter, err := ParseLogFilter(map[string]interface{}{"topics": []interface{}{topicA, nil}})
	require.NoError(t, err)
	require.True(t, filter.match(&rawLog{Address: emitter, Topics: []string{topicA, topicB}}))
	require.False(t, filter.match(&rawLog{Address: emitter, Topics: []string{topicB, topicA}}))
	_, err = ParseLogFilter(map[string]interface{}{"address": "not-an-address"})
	require.Error(t, err)
}
