package blockstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	lfAddrA  = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	lfAddrB  = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	lfTopicX = "0x1111111111111111111111111111111111111111111111111111111111111111"
	lfTopicY = "0x2222222222222222222222222222222222222222222222222222222222222222"
	lfTopicZ = "0x3333333333333333333333333333333333333333333333333333333333333333"
)

func lfHash(n int64) string { return fmt.Sprintf("0x%064x", 0xb10c0000+n) }

func lfLog(n, idx int64, addr string, topics ...string) map[string]interface{} {
	return map[string]interface{}{
		"address": addr, "topics": topics, "data": "0x",
		"blockNumber": fmt.Sprintf("0x%x", n), "blockHash": lfHash(n),
		"transactionHash": fmt.Sprintf("0x%064x", n*100+idx), "transactionIndex": "0x0",
		"logIndex": fmt.Sprintf("0x%x", idx), "removed": false,
	}
}

func lfRaw(t *testing.T, logs ...map[string]interface{}) json.RawMessage {
	t.Helper()
	if logs == nil {
		logs = []map[string]interface{}{}
	}
	b, err := json.Marshal(logs)
	require.NoError(t, err)
	return b
}

// lfChain: 100 has two logs (A:[X,Y], B:[Y]); 101 is empty; 102 has A:[Z] and
// A:[X]; 103 has B:[X,Z]. Logs are listed out of order to exercise sorting.
func lfChainLogs() []map[string]interface{} {
	return []map[string]interface{}{
		lfLog(102, 1, lfAddrA, lfTopicX),
		lfLog(100, 0, lfAddrA, lfTopicX, lfTopicY),
		lfLog(100, 1, lfAddrB, lfTopicY),
		lfLog(102, 0, lfAddrA, lfTopicZ),
		lfLog(103, 0, lfAddrB, lfTopicX, lfTopicZ),
	}
}

type lfStore struct {
	mu   sync.Mutex
	m    map[int64]*BlockLogs
	ttls map[int64]time.Duration
	puts atomic.Int64
	fail bool
}

func newLfStore() *lfStore {
	return &lfStore{m: map[int64]*BlockLogs{}, ttls: map[int64]time.Duration{}}
}

func (s *lfStore) GetBlockLogs(_ context.Context, _ Scope, h int64) (*BlockLogs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, ErrStoreUnavailable
	}
	e := s.m[h]
	if e == nil {
		return nil, ErrNotFound
	}
	return e, nil
}

func (s *lfStore) PutBlockLogs(_ context.Context, _ Scope, e *BlockLogs, ttl time.Duration) error {
	s.puts.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[e.Number] = e
	s.ttls[e.Number] = ttl
	return nil
}

func (s *lfStore) has(h int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[h] != nil
}

type lfFetcher struct {
	calls atomic.Int64
	gate  chan struct{}
	raw   func(from, to int64) (json.RawMessage, error)
}

func (f *lfFetcher) fetch(_ context.Context, from, to int64) (json.RawMessage, error) {
	f.calls.Add(1)
	if f.gate != nil {
		<-f.gate
	}
	return f.raw(from, to)
}

func chainFetcher(t *testing.T, logs []map[string]interface{}) *lfFetcher {
	return &lfFetcher{raw: func(from, to int64) (json.RawMessage, error) {
		var out []map[string]interface{}
		for _, l := range logs {
			var n int64
			_, _ = fmt.Sscanf(l["blockNumber"].(string), "0x%x", &n)
			if n >= from && n <= to {
				out = append(out, l)
			}
		}
		return lfRaw(t, out...), nil
	}}
}

func newTestFiller(store LogsFillStore, f *lfFetcher, latest, finalized int64) *LogsFiller {
	return NewLogsFiller(LogsFillOptions{
		Scope: Scope{Namespace: "t", ProjectId: "p", NetworkId: "evm:1"}, MaxRange: 10,
		FinalizedTTL: time.Hour, UnfinalizedTTL: func() time.Duration { return 3 * time.Second },
		EmptyTipGuard: 2, FetchTimeout: 5 * time.Second,
	}, store, f.fetch,
		func(context.Context) int64 { return latest },
		func(context.Context) int64 { return finalized })
}

func mustFilter(t *testing.T, js string) *LogFilter {
	t.Helper()
	var obj map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(js), &obj))
	f, err := ParseLogFilter(obj)
	require.NoError(t, err)
	return f
}

// logIDs renders "block:logIndex" for each log, preserving order.
func logIDs(t *testing.T, logs []json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0, len(logs))
	for _, raw := range logs {
		var l rawLog
		require.NoError(t, json.Unmarshal(raw, &l))
		n, _ := parseHexInt(l.BlockNumber)
		i, _ := parseHexInt(l.LogIndex)
		out = append(out, fmt.Sprintf("%d:%d", n, i))
	}
	return out
}

func TestLogsFill_FilterCorrectness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to int64
		filter   string
		want     []string
	}{
		{"no filter spans empty block", 100, 103, `{}`, []string{"100:0", "100:1", "102:0", "102:1", "103:0"}},
		{"single address", 100, 103, fmt.Sprintf(`{"address":%q}`, lfAddrA), []string{"100:0", "102:0", "102:1"}},
		{"address array", 100, 103, fmt.Sprintf(`{"address":[%q,%q]}`, lfAddrA, lfAddrB), []string{"100:0", "100:1", "102:0", "102:1", "103:0"}},
		{"address upper case", 100, 103, fmt.Sprintf(`{"address":%q}`, "0x"+strings.ToUpper(lfAddrB[2:])), []string{"100:1", "103:0"}},
		{"topic0", 100, 103, fmt.Sprintf(`{"topics":[%q]}`, lfTopicX), []string{"100:0", "102:1", "103:0"}},
		{"topic0 OR list", 100, 103, fmt.Sprintf(`{"topics":[[%q,%q]]}`, lfTopicY, lfTopicZ), []string{"100:1", "102:0"}},
		{"nil wildcard then topic1", 100, 103, fmt.Sprintf(`{"topics":[null,%q]}`, lfTopicY), []string{"100:0"}},
		{"positional mismatch", 100, 103, fmt.Sprintf(`{"topics":[%q,%q]}`, lfTopicY, lfTopicX), []string{}},
		{"trailing wildcard requires position", 100, 103, fmt.Sprintf(`{"topics":[%q,null]}`, lfTopicX), []string{"100:0", "103:0"}},
		{"address and topic", 100, 103, fmt.Sprintf(`{"address":%q,"topics":[%q]}`, lfAddrB, lfTopicX), []string{"103:0"}},
		{"only empty block", 101, 101, `{}`, []string{}},
		{"no match", 100, 103, fmt.Sprintf(`{"address":%q,"topics":[%q]}`, lfAddrB, lfTopicZ), []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFiller(newLfStore(), chainFetcher(t, lfChainLogs()), 200, 150)
			res := f.Serve(t.Context(), tc.from, tc.to, 0, mustFilter(t, tc.filter))
			require.True(t, res.OK, res.Reason)
			require.Equal(t, LogsFillFill, res.Outcome)
			require.NotNil(t, res.Logs, "result must be [] not null")
			require.Equal(t, tc.want, logIDs(t, res.Logs))
		})
	}
}

func TestLogsFill_SecondFilterServedFromStore(t *testing.T) {
	store := newLfStore()
	fetch := chainFetcher(t, lfChainLogs())
	f := newTestFiller(store, fetch, 200, 150)

	first := f.Serve(t.Context(), 100, 103, 0, mustFilter(t, fmt.Sprintf(`{"address":%q}`, lfAddrA)))
	require.True(t, first.OK)
	require.Equal(t, LogsFillFill, first.Outcome)
	require.EqualValues(t, 1, fetch.calls.Load())
	for h := int64(100); h <= 103; h++ {
		require.True(t, store.has(h), "height %d stored (including empty 101)", h)
	}
	empty, _ := store.GetBlockLogs(t.Context(), Scope{}, 101)
	require.JSONEq(t, `[]`, string(empty.Logs))
	require.Empty(t, empty.Hash)
	require.Equal(t, time.Hour, store.ttls[100], "finalized height uses finalized TTL")

	second := f.Serve(t.Context(), 100, 103, 0, mustFilter(t, fmt.Sprintf(`{"topics":[%q]}`, lfTopicX)))
	require.True(t, second.OK)
	require.Equal(t, LogsFillHit, second.Outcome)
	require.Equal(t, []string{"100:0", "102:1", "103:0"}, logIDs(t, second.Logs))
	sub := f.Serve(t.Context(), 101, 102, 0, mustFilter(t, `{}`))
	require.Equal(t, LogsFillHit, sub.Outcome, "a sub-range of a filled range is a hit")
	require.EqualValues(t, 1, fetch.calls.Load(), "different filters must not reach upstream")
}

func TestLogsFill_Skips(t *testing.T) {
	for _, tc := range []struct {
		name       string
		from, to   int64
		maxRange   int64
		latest     int64
		wantReason string
	}{
		{"above head", 195, 201, 0, 200, "above_head"},
		{"head unknown", 100, 101, 0, 0, "head_unknown"},
		{"range too large", 100, 110, 0, 200, "range_too_large"},
		{"network max range caps", 100, 104, 4, 200, "range_too_large"},
		{"inverted", 101, 100, 0, 200, "invalid_range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetch := chainFetcher(t, lfChainLogs())
			f := newTestFiller(newLfStore(), fetch, tc.latest, 0)
			res := f.Serve(t.Context(), tc.from, tc.to, tc.maxRange, nil)
			require.False(t, res.OK)
			require.Equal(t, LogsFillSkipped, res.Outcome)
			require.Equal(t, tc.wantReason, res.Reason)
			require.Zero(t, fetch.calls.Load())
		})
	}
}

func TestLogsFill_FallbackAndNoCache(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        func(t *testing.T) (json.RawMessage, error)
		wantReason string
	}{
		{"upstream error", func(*testing.T) (json.RawMessage, error) { return nil, errors.New("boom") }, "fetch_error"},
		{"null result", func(*testing.T) (json.RawMessage, error) { return json.RawMessage(`null`), nil }, "parse_error"},
		{"not an array", func(*testing.T) (json.RawMessage, error) { return json.RawMessage(`{"x":1}`), nil }, "parse_error"},
		{"log outside range", func(t *testing.T) (json.RawMessage, error) { return lfRaw(t, lfLog(150, 0, lfAddrA)), nil }, "parse_error"},
		{"two hashes at one height", func(t *testing.T) (json.RawMessage, error) {
			l := lfLog(100, 1, lfAddrA)
			l["blockHash"] = lfHash(999)
			return lfRaw(t, lfLog(100, 0, lfAddrA), l), nil
		}, "parse_error"},
		{"duplicate logIndex", func(t *testing.T) (json.RawMessage, error) {
			return lfRaw(t, lfLog(100, 0, lfAddrA), lfLog(100, 0, lfAddrB)), nil
		}, "parse_error"},
		{"removed log", func(t *testing.T) (json.RawMessage, error) {
			l := lfLog(101, 0, lfAddrA)
			l["removed"] = true
			return lfRaw(t, lfLog(100, 0, lfAddrA), l), nil
		}, "removed_logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newLfStore()
			fetch := &lfFetcher{raw: func(int64, int64) (json.RawMessage, error) { return tc.raw(t) }}
			f := newTestFiller(store, fetch, 200, 150)
			res := f.Serve(t.Context(), 100, 102, 0, nil)
			require.False(t, res.OK)
			require.Equal(t, LogsFillFallback, res.Outcome)
			require.Equal(t, tc.wantReason, res.Reason)
			require.Zero(t, store.puts.Load(), "nothing may be stored")
		})
	}
}

func TestLogsFill_UnfinalizedTTLAndEmptyTipGuard(t *testing.T) {
	// latest=200, finalized=190, guard=2: empty heights 199 and 200 are too
	// close to the head to trust; 198 is stored. Heights with logs are always
	// stored (they carry a block hash), with the unfinalized TTL above 190.
	logs := []map[string]interface{}{lfLog(196, 0, lfAddrA), lfLog(200, 0, lfAddrA)}
	store := newLfStore()
	f := newTestFiller(store, chainFetcher(t, logs), 200, 190)
	res := f.Serve(t.Context(), 189, 200, 12, nil)
	require.Equal(t, "range_too_large", res.Reason)

	res = f.Serve(t.Context(), 191, 200, 0, nil)
	require.True(t, res.OK)
	require.Equal(t, []string{"196:0", "200:0"}, logIDs(t, res.Logs))
	for h := int64(191); h <= 198; h++ {
		require.True(t, store.has(h), "height %d", h)
		require.Equal(t, 3*time.Second, store.ttls[h])
	}
	require.False(t, store.has(199), "empty height within guard of head must not be stored")
	require.True(t, store.has(200), "non-empty tip height is stored")

	finalizedStore := newLfStore()
	f = newTestFiller(finalizedStore, chainFetcher(t, nil), 200, 200)
	res = f.Serve(t.Context(), 195, 200, 0, nil)
	require.True(t, res.OK)
	require.True(t, finalizedStore.has(200), "finalized empty heights are always stored")
	require.Equal(t, time.Hour, finalizedStore.ttls[200])
}

func TestLogsFill_ReorgOverwritesHeight(t *testing.T) {
	store := newLfStore()
	stale := &BlockLogs{Number: 100, Hash: lfHash(1), Logs: lfRaw(t, lfLog(100, 0, lfAddrB))}
	require.NoError(t, store.PutBlockLogs(t.Context(), Scope{}, stale, time.Second))
	f := newTestFiller(store, chainFetcher(t, lfChainLogs()), 200, 150)
	res := f.Serve(t.Context(), 100, 101, 0, nil)
	require.Equal(t, LogsFillFill, res.Outcome, "a partially covered range refills")
	got, _ := store.GetBlockLogs(t.Context(), Scope{}, 100)
	require.Equal(t, normHash(lfHash(100)), got.Hash, "the refilled hash replaces the stale entry")
}

// A fully covered range whose unfinalized heights came from different fills may
// straddle a reorg (height 100 from the old fork, 101 from the new). It must be
// refilled with one fresh call instead of being served as one coherent answer.
func TestLogsFill_UnfinalizedHitRequiresOneFill(t *testing.T) {
	store := newLfStore()
	oldFork := &BlockLogs{Number: 100, Hash: lfHash(1), Logs: lfRaw(t, lfLog(100, 0, lfAddrB)), Fill: "old"}
	newFork := &BlockLogs{Number: 101, Hash: lfHash(101), Logs: lfRaw(t), Fill: "new"}
	require.NoError(t, store.PutBlockLogs(t.Context(), Scope{}, oldFork, time.Minute))
	require.NoError(t, store.PutBlockLogs(t.Context(), Scope{}, newFork, time.Minute))
	fetch := chainFetcher(t, lfChainLogs())
	f := newTestFiller(store, fetch, 200, 50) // 100..101 unfinalized
	res := f.Serve(t.Context(), 100, 101, 0, nil)
	require.Equal(t, LogsFillFill, res.Outcome, "mixed-fill unfinalized entries are not served as a hit")
	require.EqualValues(t, 1, fetch.calls.Load())
	got, _ := store.GetBlockLogs(t.Context(), Scope{}, 100)
	require.Equal(t, normHash(lfHash(100)), got.Hash, "the refill replaces the stale-fork entry")

	// Now both heights share one fill: a hit, no upstream call.
	res = f.Serve(t.Context(), 100, 101, 0, nil)
	require.Equal(t, LogsFillHit, res.Outcome)
	require.EqualValues(t, 1, fetch.calls.Load())

	// Pre-upgrade entries (no fill id) are a miss while unfinalized, a hit once finalized.
	legacy := newLfStore()
	for _, n := range []int64{100, 101} {
		require.NoError(t, legacy.PutBlockLogs(t.Context(), Scope{}, &BlockLogs{Number: n, Logs: lfRaw(t)}, time.Minute))
	}
	fetch2 := chainFetcher(t, lfChainLogs())
	require.Equal(t, LogsFillFill, newTestFiller(legacy, fetch2, 200, 50).Serve(t.Context(), 100, 101, 0, nil).Outcome)
	legacyFinal := newLfStore()
	for _, n := range []int64{100, 101} {
		require.NoError(t, legacyFinal.PutBlockLogs(t.Context(), Scope{}, &BlockLogs{Number: n, Logs: lfRaw(t)}, time.Minute))
	}
	fetch3 := chainFetcher(t, lfChainLogs())
	require.Equal(t, LogsFillHit, newTestFiller(legacyFinal, fetch3, 200, 150).Serve(t.Context(), 100, 101, 0, nil).Outcome,
		"finalized heights cannot reorg, so mixed or legacy entries are served")
	require.Zero(t, fetch3.calls.Load())
}

func TestLogsFill_NegativeLogIndexRejected(t *testing.T) {
	bad := lfLog(100, 0, lfAddrA)
	bad["logIndex"] = "-0x1"
	_, _, err := SplitRangeLogs(lfRaw(t, bad), 100, 100)
	require.Error(t, err)
}

func TestLogsFill_SingleflightCoalesces(t *testing.T) {
	fetch := chainFetcher(t, lfChainLogs())
	fetch.gate = make(chan struct{})
	f := newTestFiller(newLfStore(), fetch, 200, 150)
	filters := []string{`{}`, fmt.Sprintf(`{"address":%q}`, lfAddrA), fmt.Sprintf(`{"topics":[%q]}`, lfTopicX)}
	const n = 24
	var wg sync.WaitGroup
	results := make([]LogsFillResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = f.Serve(context.Background(), 100, 103, 0, mustFilter(t, filters[i%len(filters)]))
		}(i)
	}
	require.Eventually(t, func() bool { return fetch.calls.Load() == 1 }, time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond) // let the remaining callers join the flight
	close(fetch.gate)
	wg.Wait()
	require.EqualValues(t, 1, fetch.calls.Load())
	for i, r := range results {
		require.True(t, r.OK, "request %d", i)
	}
	require.Len(t, results[1].Logs, 3)
}

func TestLogsFill_StoreErrorIsMiss(t *testing.T) {
	store := newLfStore()
	store.fail = true
	fetch := chainFetcher(t, lfChainLogs())
	f := newTestFiller(store, fetch, 200, 150)
	res := f.Serve(t.Context(), 100, 101, 0, nil)
	require.True(t, res.OK)
	require.Equal(t, LogsFillFill, res.Outcome)
	require.EqualValues(t, 1, fetch.calls.Load())
}

// gaugeStore records the peak number of in-flight store calls.
type gaugeStore struct {
	*lfStore
	cur, peak atomic.Int64
}

func (g *gaugeStore) track() func() {
	n := g.cur.Add(1)
	for p := g.peak.Load(); n > p && !g.peak.CompareAndSwap(p, n); p = g.peak.Load() {
	}
	time.Sleep(2 * time.Millisecond)
	return func() { g.cur.Add(-1) }
}

func (g *gaugeStore) GetBlockLogs(ctx context.Context, sc Scope, h int64) (*BlockLogs, error) {
	defer g.track()()
	return g.lfStore.GetBlockLogs(ctx, sc, h)
}

func (g *gaugeStore) PutBlockLogs(ctx context.Context, sc Scope, e *BlockLogs, ttl time.Duration) error {
	defer g.track()()
	return g.lfStore.PutBlockLogs(ctx, sc, e, ttl)
}

func TestLogsFill_StoreFanoutBoundedByConcurrency(t *testing.T) {
	store := &gaugeStore{lfStore: newLfStore()}
	fetch := chainFetcher(t, lfChainLogs())
	f := NewLogsFiller(LogsFillOptions{
		Scope: Scope{Namespace: "t"}, MaxRange: 1000, FinalizedTTL: time.Hour,
		EmptyTipGuard: 1, Concurrency: 3,
	}, store, fetch.fetch,
		func(context.Context) int64 { return 2000 },
		func(context.Context) int64 { return 1500 })
	res := f.Serve(t.Context(), 100, 299, 0, nil) // miss: 200 GETs, then 200 PUTs
	require.True(t, res.OK)
	require.EqualValues(t, 200, store.puts.Load())
	res = f.Serve(t.Context(), 100, 299, 0, nil) // hit: 200 GETs
	require.True(t, res.OK)
	require.Equal(t, LogsFillHit, res.Outcome)
	require.LessOrEqual(t, store.peak.Load(), int64(3), "per-height store calls exceed Concurrency")
	require.Greater(t, store.peak.Load(), int64(1), "store calls still run in parallel")
}

func TestMemoryLogsFillStore_TTLAndEviction(t *testing.T) {
	s := NewMemoryLogsFillStore(400)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	sc := Scope{Namespace: "n", ProjectId: "p", NetworkId: "evm:1"}
	entry := func(h int64) *BlockLogs {
		return &BlockLogs{Number: h, Hash: lfHash(h), Logs: json.RawMessage(`[]`)}
	}
	for h := int64(1); h <= 3; h++ {
		require.NoError(t, s.PutBlockLogs(t.Context(), sc, entry(h), time.Minute))
	}
	_, err := s.GetBlockLogs(t.Context(), sc, 1) // touch 1 so 2 becomes LRU
	require.NoError(t, err)
	for h := int64(4); h <= 6; h++ {
		require.NoError(t, s.PutBlockLogs(t.Context(), sc, entry(h), time.Minute))
	}
	require.LessOrEqual(t, s.bytes, int64(400))
	_, err = s.GetBlockLogs(t.Context(), sc, 2)
	require.ErrorIs(t, err, ErrNotFound, "LRU entry evicted")
	_, err = s.GetBlockLogs(t.Context(), sc, 6)
	require.NoError(t, err)

	now = now.Add(2 * time.Minute)
	_, err = s.GetBlockLogs(t.Context(), sc, 6)
	require.ErrorIs(t, err, ErrNotFound, "expired")

	other := Scope{Namespace: "n", ProjectId: "p", NetworkId: "evm:2"}
	require.NoError(t, s.PutBlockLogs(t.Context(), sc, entry(7), time.Minute))
	_, err = s.GetBlockLogs(t.Context(), other, 7)
	require.ErrorIs(t, err, ErrNotFound, "scopes are isolated")
}

// lfLockStore is an lfStore shared by several LogsFillers (one per simulated
// replica) that also implements LogsFillLocker like the Redis store does.
type lfLockStore struct {
	*lfStore
	lmu       sync.Mutex
	locks     map[string]string
	nextToken int
	tryCalls  atomic.Int64
	lockErr   error
	heldErr   error
}

func newLfLockStore() *lfLockStore {
	return &lfLockStore{lfStore: newLfStore(), locks: map[string]string{}}
}

func lfLockKey(from, to int64) string { return fmt.Sprintf("%d-%d", from, to) }

func (s *lfLockStore) TryLockFill(_ context.Context, _ Scope, from, to int64, _ time.Duration) (func(context.Context), bool, error) {
	s.tryCalls.Add(1)
	s.lmu.Lock()
	defer s.lmu.Unlock()
	if s.lockErr != nil {
		return nil, false, s.lockErr
	}
	key := lfLockKey(from, to)
	if _, held := s.locks[key]; held {
		return nil, false, nil
	}
	s.nextToken++
	token := fmt.Sprint(s.nextToken)
	s.locks[key] = token
	return func(context.Context) {
		s.lmu.Lock()
		defer s.lmu.Unlock()
		if s.locks[key] == token {
			delete(s.locks, key)
		}
	}, true, nil
}

func (s *lfLockStore) FillLocked(_ context.Context, _ Scope, from, to int64) (bool, error) {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	if s.heldErr != nil {
		return false, s.heldErr
	}
	_, held := s.locks[lfLockKey(from, to)]
	return held, nil
}

// hold takes the range lock as an external peer would; the returned func
// releases it.
func (s *lfLockStore) hold(t *testing.T, from, to int64) func() {
	t.Helper()
	release, ok, err := s.TryLockFill(t.Context(), Scope{}, from, to, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	return func() { release(context.Background()) }
}

func newPeerFiller(store LogsFillStore, f *lfFetcher, latest, finalized int64, peerWait time.Duration) *LogsFiller {
	lf := newTestFiller(store, f, latest, finalized)
	lf.opt.PeerWait = peerWait
	return lf
}

func TestLogsFill_MixedPeerEntriesAreRefilled(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(fmt.Sprintf("waiting=%t", waiting), func(t *testing.T) {
			store := newLfLockStore()
			fetch := chainFetcher(t, lfChainLogs())
			f := newPeerFiller(store, fetch, 200, 50, 300*time.Millisecond)
			// The first lookup misses; the peer then supplies a complete but
			// incoherent range before our post-lock lookup.
			store.m[100] = &BlockLogs{Number: 100, Fill: "one", Logs: lfRaw(t, lfLog(100, 0, lfAddrA, lfTopicX))}
			store.m[101] = &BlockLogs{Number: 101, Fill: "two", Logs: lfRaw(t)}
			if waiting {
				defer store.hold(t, 100, 101)()
			}
			res := f.Serve(t.Context(), 100, 101, 0, nil)
			require.True(t, res.OK)
			require.Equal(t, LogsFillFill, res.Outcome)
			require.EqualValues(t, 1, fetch.calls.Load(), "mixed fills must be fetched as one range")
		})
	}
}

func TestLogsFill_PeerReplicasShareOneFetch(t *testing.T) {
	store := newLfLockStore()
	fetch := chainFetcher(t, lfChainLogs())
	fetch.gate = make(chan struct{})
	replicas := []*LogsFiller{
		newPeerFiller(store, fetch, 200, 150, 5*time.Second),
		newPeerFiller(store, fetch, 200, 150, 5*time.Second),
	}
	filters := []string{`{}`, fmt.Sprintf(`{"address":%q}`, lfAddrA), fmt.Sprintf(`{"topics":[%q]}`, lfTopicX)}
	want := [][]string{
		{"100:0", "100:1", "102:0", "102:1", "103:0"},
		{"100:0", "102:0", "102:1"},
		{"100:0", "102:1", "103:0"},
	}
	const n = 24
	var wg sync.WaitGroup
	results := make([]LogsFillResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = replicas[i%2].Serve(context.Background(), 100, 103, 0, mustFilter(t, filters[i%len(filters)]))
		}(i)
	}
	require.Eventually(t, func() bool { return fetch.calls.Load() == 1 }, time.Second, time.Millisecond)
	time.Sleep(150 * time.Millisecond) // the other replica is now waiting on the lock
	close(fetch.gate)
	wg.Wait()

	require.EqualValues(t, 1, fetch.calls.Load(), "one upstream fetch across both replicas")
	var fills, peerHits int
	for i, r := range results {
		require.True(t, r.OK, "request %d: %s", i, r.Reason)
		require.Equal(t, want[i%len(filters)], logIDs(t, r.Logs), "request %d", i)
		switch {
		case r.Outcome == LogsFillFill:
			fills++
		case r.Outcome == LogsFillHit && r.Reason == LogsFillReasonPeerFill:
			peerHits++
		}
	}
	require.Equal(t, n/2, fills, "the lock holder's replica serves its callers from its own fill")
	require.Equal(t, n/2, peerHits, "the waiting replica serves from the peer's stored entries")
	ok, err := store.FillLocked(t.Context(), Scope{}, 100, 103)
	require.NoError(t, err)
	require.False(t, ok, "the lock is released after the fill")
}

func TestLogsFill_PeerNeverCompletesFallsBackAfterPeerWait(t *testing.T) {
	store := newLfLockStore()
	defer store.hold(t, 100, 103)()
	fetch := chainFetcher(t, lfChainLogs())
	const peerWait = 300 * time.Millisecond
	f := newPeerFiller(store, fetch, 200, 150, peerWait)

	start := time.Now()
	res := f.Serve(t.Context(), 100, 103, 0, nil)
	elapsed := time.Since(start)

	require.True(t, res.OK)
	require.Equal(t, LogsFillFill, res.Outcome)
	require.Equal(t, LogsFillReasonPeerTimeout, res.Reason)
	require.EqualValues(t, 1, fetch.calls.Load(), "the waiter fetches itself")
	require.GreaterOrEqual(t, elapsed, peerWait)
	require.Less(t, elapsed, peerWait+250*time.Millisecond, "latency is bounded by peerWait")
}

func TestLogsFill_PeerLeavesTipUnstoredWaiterDoesNotWaitOut(t *testing.T) {
	// latest=200, guard=2: the peer's fill of 195..200 deliberately leaves
	// the empty heights 199 and 200 unstored.
	store := newLfLockStore()
	logs := []map[string]interface{}{lfLog(196, 0, lfAddrA)}
	peerFetch := chainFetcher(t, logs)
	peerFetch.gate = make(chan struct{})
	const peerWait = 5 * time.Second
	peer := newPeerFiller(store, peerFetch, 200, 190, peerWait)
	waiterFetch := chainFetcher(t, logs)
	waiter := newPeerFiller(store, waiterFetch, 200, 190, peerWait)

	peerDone := make(chan LogsFillResult, 1)
	go func() { peerDone <- peer.Serve(context.Background(), 195, 200, 0, nil) }()
	require.Eventually(t, func() bool { return peerFetch.calls.Load() == 1 }, time.Second, time.Millisecond)

	start := time.Now()
	waiterDone := make(chan LogsFillResult, 1)
	go func() { waiterDone <- waiter.Serve(context.Background(), 195, 200, 0, nil) }()
	time.Sleep(100 * time.Millisecond)
	close(peerFetch.gate)

	require.True(t, (<-peerDone).OK)
	res := <-waiterDone
	elapsed := time.Since(start)
	require.False(t, store.has(200), "the peer left the empty tip unstored")
	require.True(t, res.OK)
	require.Equal(t, LogsFillFill, res.Outcome)
	require.Equal(t, LogsFillReasonPeerIncomplete, res.Reason)
	require.Equal(t, []string{"196:0"}, logIDs(t, res.Logs))
	require.EqualValues(t, 1, waiterFetch.calls.Load(), "missing heights are fetched locally")
	require.Less(t, elapsed, time.Second, "the waiter stops when the peer releases, not at peerWait")
}

func TestLogsFill_LockErrorsFailOpen(t *testing.T) {
	t.Run("acquire error", func(t *testing.T) {
		store := newLfLockStore()
		store.lockErr = errors.New("redis down")
		fetch := chainFetcher(t, lfChainLogs())
		f := newPeerFiller(store, fetch, 200, 150, time.Second)
		res := f.Serve(t.Context(), 100, 103, 0, nil)
		require.True(t, res.OK)
		require.Equal(t, LogsFillFill, res.Outcome)
		require.Equal(t, LogsFillReasonLockError, res.Reason)
		require.EqualValues(t, 1, fetch.calls.Load())
	})
	t.Run("poll error while waiting", func(t *testing.T) {
		store := newLfLockStore()
		defer store.hold(t, 100, 103)()
		store.heldErr = errors.New("redis down")
		fetch := chainFetcher(t, lfChainLogs())
		f := newPeerFiller(store, fetch, 200, 150, 5*time.Second)
		start := time.Now()
		res := f.Serve(t.Context(), 100, 103, 0, nil)
		require.True(t, res.OK)
		require.Equal(t, LogsFillReasonLockError, res.Reason)
		require.EqualValues(t, 1, fetch.calls.Load())
		require.Less(t, time.Since(start), time.Second, "a poll error does not wait out peerWait")
	})
}

func TestLogsFill_PeerWaitZeroDisablesLocking(t *testing.T) {
	store := newLfLockStore()
	fetch := chainFetcher(t, lfChainLogs())
	f := newPeerFiller(store, fetch, 200, 150, 0)
	res := f.Serve(t.Context(), 100, 103, 0, nil)
	require.True(t, res.OK)
	require.Equal(t, LogsFillFill, res.Outcome)
	require.Empty(t, res.Reason)
	require.Zero(t, store.tryCalls.Load(), "no lock with peerWait=0")
}
