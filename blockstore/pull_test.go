package blockstore

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// pullOpts is the default (pull) configuration: following only with subscribers.
func pullOpts() Options {
	o := testOpts()
	o.AlwaysFollow = false
	return o
}

// presenceFleetStore adds the fleet presence mark and canonical index.
type presenceFleetStore struct {
	*fakeFleetStore
	pmu      sync.Mutex
	presence time.Time
	canon    map[int64]canonEntry
}

type canonEntry struct {
	hash string
	at   time.Time
}

func newPresenceFleetStore() *presenceFleetStore {
	s := &presenceFleetStore{fakeFleetStore: newFakeFleetStore(), canon: map[int64]canonEntry{}}
	s.leader = true
	return s
}

func (s *presenceFleetStore) MarkPresence(_ context.Context, _ Scope, ttl time.Duration) error {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.presence = time.Now().Add(ttl)
	return nil
}

func (s *presenceFleetStore) HasPresence(context.Context, Scope) (bool, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return time.Now().Before(s.presence), nil
}

func (s *presenceFleetStore) expirePresence() {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.presence = time.Time{}
}

func (s *presenceFleetStore) PutCanonical(_ context.Context, _ Scope, n int64, hash string, at time.Time, _ time.Duration) error {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.canon[n] = canonEntry{hash: hash, at: at}
	return nil
}

func (s *presenceFleetStore) GetCanonical(_ context.Context, _ Scope, n int64) (string, time.Time, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	e, ok := s.canon[n]
	if !ok {
		return "", time.Time{}, ErrNotFound
	}
	return e.hash, e.at, nil
}

func newPullCache(ch *fakeChain, store Store) *Cache {
	o := pullOpts()
	o.Latest = ch.head
	return New(o, store, ch, ch.head, nil)
}

func fullBlock(t *testing.T, ch *fakeChain, n int64) json.RawMessage {
	t.Helper()
	ch.mu.Lock()
	defer ch.mu.Unlock()
	raw, err := ch.blockLocked(n)
	require.NoError(t, err)
	return raw
}

func headerOf(t *testing.T, ch *fakeChain, n int64) json.RawMessage {
	t.Helper()
	raw, err := (&BlockRecord{Block: fullBlock(t, ch, n)}).BlockJSON(false)
	require.NoError(t, err)
	return raw
}

func rangeLogs(t *testing.T, ch *fakeChain, from, to int64) json.RawMessage {
	t.Helper()
	ch.mu.Lock()
	defer ch.mu.Unlock()
	var out []map[string]interface{}
	for n := from; n <= to; n++ {
		out = append(out, ch.logsLocked(n, ch.blocks[n])...)
	}
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	return raw
}

func fetchTotal(kind PayloadKind, reason string) float64 {
	return testutil.ToFloat64(telemetry.MetricBlockStoreFetchTotal.WithLabelValues("p", "evm:1", string(kind), reason))
}

// No subscribers and no client traffic: the store makes no upstream call at
// all, however many blocks are produced.
func TestPull_IdleChainMakesNoUpstreamCalls(t *testing.T) {
	for _, fleet := range []bool{false, true} {
		ch := newFakeChain(20)
		var store Store = newMapStore()
		if fleet {
			store = newPresenceFleetStore()
		}
		c := newPullCache(ch, store)
		for i := 0; i < 25; i++ {
			ch.mine(1)
			c.Tick(ctxb())
		}
		head, body, logs := ch.counts()
		require.Zero(t, head+body+logs, "fleet=%v: no header, body or logs fetch without subscribers or clients", fleet)
		require.False(t, c.Fresh())
	}
}

// A client's full block is adopted (header and body): later reads of that
// block, full or hash-only, by number or hash, are served with no upstream call.
func TestPull_ClientBlockAdoptedAndServed(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	adoptedBefore := testutil.ToFloat64(telemetry.MetricBlockStoreAdoptTotal.WithLabelValues("p", "evm:1", "block"))
	c.AdoptBlock(ctxb(), fullBlock(t, ch, 20), true, true, false)

	full, ok := c.BlockByNumber(ctxb(), 20, true)
	require.True(t, ok)
	require.JSONEq(t, string(fullBlock(t, ch, 20)), string(full))
	lite, ok := c.BlockByHash(ctxb(), hashOf(20, "a"), false)
	require.True(t, ok)
	require.JSONEq(t, string(headerOf(t, ch, 20)), string(lite))
	head, body, logs := ch.counts()
	require.Zero(t, head+body+logs)
	require.Equal(t, adoptedBefore+1, testutil.ToFloat64(telemetry.MetricBlockStoreAdoptTotal.WithLabelValues("p", "evm:1", "block")))
	require.Equal(t, hashOf(20, "a"), c.CanonicalHash(20))

	// Not-yet-observed heights are misses (normal path).
	_, ok = c.BlockByNumber(ctxb(), 19, false)
	require.False(t, ok)
	// A by-hash result of an unheld block is not canonical evidence.
	c.AdoptBlock(ctxb(), fullBlock(t, ch, 18), true, false, false)
	_, ok = c.BlockByNumber(ctxb(), 18, true)
	require.False(t, ok)

	// After MaxStaleness without a fresh linked observation it stops serving.
	c.nowFn = func() time.Time { return time.Now().Add(3 * time.Second) }
	_, ok = c.BlockByNumber(ctxb(), 20, false)
	require.False(t, ok, "an adopted header is served only while recently confirmed")
	head, _, _ = ch.counts()
	require.Zero(t, head, "no speculative relinking")
}

// Another replica serves the adopted block from the shared store, with no
// upstream call of its own.
func TestPull_AdoptedBlockSharedAcrossReplicas(t *testing.T) {
	ch := newFakeChain(20)
	store := newPresenceFleetStore()
	a := newPullCache(ch, store)
	b := newPullCache(ch, store)
	a.AdoptBlock(ctxb(), fullBlock(t, ch, 20), true, true, false)
	full, ok := b.BlockByNumber(ctxb(), 20, true)
	require.True(t, ok)
	require.JSONEq(t, string(fullBlock(t, ch, 20)), string(full))
	_, ok = b.BlockByHash(ctxb(), hashOf(20, "a"), false)
	require.True(t, ok)
	head, body, logs := ch.counts()
	require.Zero(t, head+body+logs)
}

// An unfiltered client getLogs over an explicit range is adopted per block
// (validated against the adopted header); filtered reads are then local.
// Filtered or tag-ranged results are never stored as complete.
func TestPull_UnfilteredLogsAdoptedFilteredServedLocally(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	for n := int64(17); n <= 20; n++ {
		c.AdoptBlock(ctxb(), headerOf(t, ch, n), false, true, false)
	}
	// A filtered result is not complete: nothing is stored.
	c.ObserveLogs(ctxb(), rangeLogs(t, ch, 17, 18), 17, 18, false, false)
	_, ok := c.LogsRangeCached(ctxb(), 17, 18, nil)
	require.False(t, ok)

	c.ObserveLogs(ctxb(), rangeLogs(t, ch, 17, 20), 17, 20, true, false)
	f := &LogFilter{Topics: [][]string{{normHash(topicA)}}}
	got, ok := c.LogsRange(ctxb(), 17, 20, f)
	require.True(t, ok)
	require.Len(t, got, 2, "even heights carry topicA")
	got, ok = c.LogsByHash(ctxb(), hashOf(19, "a"), nil)
	require.True(t, ok)
	require.Len(t, got, 1)
	head, body, logs := ch.counts()
	require.Zero(t, head+body+logs)
}

// Adopting unfiltered logs for heights whose headers are unknown fetches
// nothing: those heights are skipped, and adopted once their header is held.
func TestPull_LogsAdoptionNeverFetchesHeaders(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	c.ObserveLogs(ctxb(), rangeLogs(t, ch, 17, 20), 17, 20, true, false)
	c.AdoptLogs(ctxb(), mustSplit(t, rangeLogs(t, ch, 17, 20), 17, 20))
	head, body, logs := ch.counts()
	require.Zero(t, head+body+logs, "no header, body or logs fetch from adoption")
	_, ok := c.LogsRangeCached(ctxb(), 17, 20, nil)
	require.False(t, ok, "unknown headers: nothing adopted")

	for n := int64(17); n <= 20; n++ {
		c.AdoptBlock(ctxb(), headerOf(t, ch, n), false, true, false)
	}
	c.AdoptLogs(ctxb(), mustSplit(t, rangeLogs(t, ch, 17, 20), 17, 20))
	got, ok := c.LogsRangeCached(ctxb(), 17, 20, &LogFilter{Topics: [][]string{{normHash(topicA)}}})
	require.True(t, ok, "held headers: complete lists adopted and served filtered")
	require.Len(t, got, 2)
	head, body, logs = ch.counts()
	require.Zero(t, head+body+logs)
}

func mustSplit(t *testing.T, raw json.RawMessage, from, to int64) []*BlockLogs {
	t.Helper()
	entries, removed, err := SplitRangeLogs(raw, from, to)
	require.NoError(t, err)
	require.False(t, removed)
	return entries
}

// A fresh observation of a different hash at a held height is a reorg: the
// stale entry, its payloads and its descendants are dropped, and lower
// entries are not served until relinked. Replayed (cached) evidence never
// replaces a held hash.
func TestPull_AdoptedReorgDropsStaleEntries(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	for n := int64(18); n <= 20; n++ {
		c.AdoptBlock(ctxb(), fullBlock(t, ch, n), true, true, false)
	}
	_, ok := c.BlockByNumber(ctxb(), 19, true)
	require.True(t, ok)

	ch.reorg(19, "b")
	// A cached (weak) replay of the new branch never displaces held data.
	c.AdoptBlock(ctxb(), headerOf(t, ch, 19), false, true, true)
	require.Equal(t, hashOf(19, "a"), c.CanonicalHash(19))

	c.AdoptBlock(ctxb(), headerOf(t, ch, 19), false, true, false)
	require.Equal(t, hashOf(19, "b"), c.CanonicalHash(19))
	_, ok = c.BlockByHash(ctxb(), hashOf(19, "a"), true)
	require.False(t, ok, "orphan is gone")
	_, ok = c.BlockByHash(ctxb(), hashOf(20, "a"), false)
	require.False(t, ok, "descendant of the orphan is gone")
	_, ok = c.BlockByNumber(ctxb(), 18, false)
	require.True(t, ok, "18 is the new block's parent: still linked and fresh")
	full, ok := c.BlockByNumber(ctxb(), 19, true)
	require.True(t, ok, "the new hash's body is fetched once on demand")
	require.Contains(t, string(full), hashOf(19, "b"))
	require.Equal(t, int64(1), c.Stats.Reorgs.Load())

	// A log carrying a different hash at a held height is reorg evidence too.
	c.ObserveLogs(ctxb(), json.RawMessage(`[{"address":"`+emitter+`","topics":["`+topicA+`"],"data":"0x","blockNumber":"0x12","blockHash":"`+hashOf(18, "z")+`","transactionHash":"`+txHashOf(18, "z")+`","transactionIndex":"0x0","logIndex":"0x0","removed":false}]`), -1, -1, false, false)
	require.Empty(t, c.CanonicalHash(18))
	require.Empty(t, c.CanonicalHash(19))
}

// Client full blocks at gapped heights are adopted and served from the store
// with no "link" header fetches: a stale unfinalized height is a miss (the
// client's own request takes the normal path), never a fetch.
func TestPull_GappedBlocksServedWithoutLinkFetches(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	heights := []int64{13, 16, 20}
	for _, n := range heights {
		c.AdoptBlock(ctxb(), fullBlock(t, ch, n), true, true, false)
	}
	for _, n := range heights {
		full, ok := c.BlockByNumber(ctxb(), n, true)
		require.True(t, ok, "height %d", n)
		require.JSONEq(t, string(fullBlock(t, ch, n)), string(full))
	}
	base := time.Now()
	c.nowFn = func() time.Time { return base.Add(3 * time.Second) }
	c.AdoptBlock(ctxb(), headerOf(t, ch, 20), false, true, false)
	_, ok := c.BlockByNumber(ctxb(), 16, true)
	require.False(t, ok, "stale and unlinked: a miss, not a link fetch")
	head, body, logs := ch.counts()
	require.Zero(t, head+body+logs)
}

// A header observed (strong evidence) at or below the finalized height cannot
// reorg: it is served without fresh confirmation or linkage.
func TestPull_FinalizedHeightsServedWithoutLinkage(t *testing.T) {
	ch := newFakeChain(20)
	o := pullOpts()
	o.Latest = ch.head
	finalized := int64(16)
	o.Finalized = func(context.Context) int64 { return finalized }
	c := New(o, newMapStore(), ch, ch.head, nil)
	c.AdoptBlock(ctxb(), fullBlock(t, ch, 14), true, true, false)
	c.AdoptBlock(ctxb(), fullBlock(t, ch, 18), true, true, false)
	// A cache replay at a finalized height is weak: never final.
	c.AdoptBlock(ctxb(), fullBlock(t, ch, 15), true, true, true)
	c.nowFn = func() time.Time { return time.Now().Add(time.Hour) }
	_, ok := c.BlockByNumber(ctxb(), 14, true)
	require.True(t, ok, "finalized and observed: served without relinking")
	_, ok = c.BlockByNumber(ctxb(), 18, false)
	require.False(t, ok, "unfinalized and stale: a miss")
	_, ok = c.BlockByNumber(ctxb(), 15, false)
	require.False(t, ok, "weak evidence is never final")
	head, body, logs := ch.counts()
	require.Zero(t, head+body+logs)
}

// Header following runs only while a subscriber exists somewhere in the
// fleet: it starts (one header per new block) when one appears on any
// replica and stops when the shared presence mark expires.
func TestPull_FollowingOnlyWithSubscribers(t *testing.T) {
	ch := newFakeChain(20)
	store := newPresenceFleetStore()
	leader := newPullCache(ch, store)
	other := newPullCache(ch, store)
	leader.Tick(ctxb())
	other.Tick(ctxb())
	head, _, _ := ch.counts()
	require.Zero(t, head)

	// A subscriber on the other replica (not the leader) starts following.
	sub := other.Subscribe(16)
	other.Tick(ctxb())
	leader.Tick(ctxb())
	require.True(t, leader.Fresh())
	cold, _, _ := ch.counts()
	require.Equal(t, int(pullOpts().Depth), cold)
	before := fetchTotal(PayloadHeader, FetchReasonSubscription)
	for i := 0; i < 3; i++ {
		ch.mine(1)
		other.Tick(ctxb())
		leader.Tick(ctxb())
		other.Tick(ctxb())
	}
	head, _, _ = ch.counts()
	require.Equal(t, cold+3, head, "one header per new block")
	require.Equal(t, before+3, fetchTotal(PayloadHeader, FetchReasonSubscription))
	require.Equal(t, int64(23), other.Head())
	evs, open := drain(sub)
	require.True(t, open)
	require.NotEmpty(t, evs)
	require.Equal(t, int64(23), evs[len(evs)-1].Added[len(evs[len(evs)-1].Added)-1].Number)

	// The last subscriber leaves; once the presence mark expires following stops.
	sub.Close()
	store.expirePresence()
	other.Tick(ctxb())
	leader.Tick(ctxb())
	head, _, _ = ch.counts()
	for i := 0; i < 5; i++ {
		ch.mine(1)
		other.Tick(ctxb())
		leader.Tick(ctxb())
	}
	after, _, _ := ch.counts()
	require.Equal(t, head, after, "no header fetches without subscribers")
}

// Following that resumes after a pause extends headers clients already
// fetched instead of refetching them.
func TestPull_FollowingResumesFromAdoptedHeaders(t *testing.T) {
	ch := newFakeChain(20)
	c := newPullCache(ch, newMapStore())
	for n := int64(13); n <= 20; n++ {
		c.AdoptBlock(ctxb(), headerOf(t, ch, n), false, true, false)
	}
	sub := c.Subscribe(4)
	defer sub.Close()
	c.Tick(ctxb())
	require.True(t, c.Fresh())
	head, _, _ := ch.counts()
	require.Zero(t, head, "the adopted chain seeds the followed window")
	ch.mine(1)
	c.Tick(ctxb())
	head, _, _ = ch.counts()
	require.Equal(t, 1, head)
}
