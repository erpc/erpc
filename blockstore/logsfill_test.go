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
