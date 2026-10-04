package blockstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type historicalTestStore struct {
	mu        sync.Mutex
	blocks    map[string]json.RawMessage
	logs      map[string]historicalLogsPayload
	index     map[string]string
	blockPuts int
	logsPuts  int
}

type historicalLogsPayload struct {
	header json.RawMessage
	logs   json.RawMessage
}

func newHistoricalTestStore() *historicalTestStore {
	return &historicalTestStore{blocks: make(map[string]json.RawMessage), logs: make(map[string]historicalLogsPayload), index: make(map[string]string)}
}

func historicalScopeKey(scope Scope, suffix string) string { return scope.Namespace + "/" + suffix }
func historicalHashKey(scope Scope, hash string) string {
	return historicalScopeKey(scope, normHash(hash))
}
func historicalHeightKey(scope Scope, n int64) string {
	return historicalScopeKey(scope, fmt.Sprintf("height/%d", n))
}

func (s *historicalTestStore) GetFinalizedHash(_ context.Context, scope Scope, n int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := s.index[historicalHeightKey(scope, n)]
	if hash == "" {
		return "", ErrNotFound
	}
	return hash, nil
}
func (s *historicalTestStore) PutFinalizedHash(_ context.Context, scope Scope, n int64, hash string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index[historicalHeightKey(scope, n)] = hash
	return nil
}
func (s *historicalTestStore) DeleteFinalizedHash(_ context.Context, scope Scope, n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.index, historicalHeightKey(scope, n))
	return nil
}
func (s *historicalTestStore) GetHistoricalBlock(_ context.Context, scope Scope, hash string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	block := s.blocks[historicalHashKey(scope, hash)]
	if block == nil {
		return nil, ErrNotFound
	}
	return append(json.RawMessage(nil), block...), nil
}
func (s *historicalTestStore) PutHistoricalBlock(_ context.Context, scope Scope, hash string, block json.RawMessage, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks[historicalHashKey(scope, hash)] = append(json.RawMessage(nil), block...)
	s.blockPuts++
	return nil
}
func (s *historicalTestStore) GetHistoricalLogs(_ context.Context, scope Scope, hash string) (json.RawMessage, json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, ok := s.logs[historicalHashKey(scope, hash)]
	if !ok {
		return nil, nil, ErrNotFound
	}
	return append(json.RawMessage(nil), payload.header...), append(json.RawMessage(nil), payload.logs...), nil
}
func (s *historicalTestStore) PutHistoricalLogs(_ context.Context, scope Scope, hash string, header, logs json.RawMessage, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs[historicalHashKey(scope, hash)] = historicalLogsPayload{header: append(json.RawMessage(nil), header...), logs: append(json.RawMessage(nil), logs...)}
	s.logsPuts++
	return nil
}

func newHistoricalForTest(t *testing.T, chain *fakeChain, store *historicalTestStore, finalized int64, maxRange int64) *Historical {
	t.Helper()
	return NewHistorical(HistoricalOptions{Scope: Scope{Namespace: "test", ProjectId: "p", NetworkId: "evm:1"}, MaxBlockSize: 1 << 16, MaxLogsRange: maxRange}, store, chain, func(context.Context) int64 { return finalized })
}

func TestHistorical_LiveCanonicalHashInvalidatesStaleIndex(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(4)
	store := newHistoricalTestStore()
	h := newHistoricalForTest(t, chain, store, 4, 4)
	block, err := chain.BlockByNumber(ctx, 1)
	require.NoError(t, err)
	header, err := chain.HeaderByNumber(ctx, 1)
	require.NoError(t, err)
	parsed, _, err := parseBlockHeader(block)
	require.NoError(t, err)
	logs, err := chain.LogsByBlockHash(ctx, parsed.Hash)
	require.NoError(t, err)
	require.NoError(t, store.PutHistoricalBlock(ctx, h.scope, parsed.Hash, block, time.Hour))
	require.NoError(t, store.PutHistoricalLogs(ctx, h.scope, parsed.Hash, header, logs, time.Hour))
	require.NoError(t, store.PutFinalizedHash(ctx, h.scope, 1, parsed.Hash, time.Hour))
	require.NotEqual(t, normHash(parsed.Hash), "0xreorg")
	h.liveHash = func(n int64) string {
		if n == 1 {
			return "0xreorg"
		}
		return ""
	}
	_, hit := h.ReadBlockByNumber(ctx, 1)
	require.False(t, hit, "historical block must not survive a conflicting live canonical hash")
	_, err = store.GetFinalizedHash(ctx, h.scope, 1)
	require.ErrorIs(t, err, ErrNotFound, "stale height index must be removed")
	require.NoError(t, store.PutFinalizedHash(ctx, h.scope, 1, parsed.Hash, time.Hour))
	_, hit = h.ReadLogsRange(ctx, 1, 1)
	require.False(t, hit, "historical logs must not survive a conflicting live canonical hash")
	_, err = store.GetFinalizedHash(ctx, h.scope, 1)
	require.ErrorIs(t, err, ErrNotFound, "logs read must also remove the stale height index")
}

func historicalHashOnlyHeader(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	var txs []map[string]json.RawMessage
	if json.Unmarshal(fields["transactions"], &txs) != nil {
		return raw // already hash-only
	}
	hashes := make([]string, 0, len(txs))
	for _, tx := range txs {
		var hash string
		require.NoError(t, json.Unmarshal(tx["hash"], &hash))
		hashes = append(hashes, hash)
	}
	fields["transactions"], _ = json.Marshal(hashes)
	out, err := json.Marshal(fields)
	require.NoError(t, err)
	return out
}

func TestHistorical_ReadsRequireFinalityIndexAndValidatedIndependentPayloads(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(4)
	store := newHistoricalTestStore()
	h := newHistoricalForTest(t, chain, store, 4, 4)
	unknown := newHistoricalForTest(t, chain, store, 0, 4)

	block, err := chain.BlockByNumber(ctx, 1)
	require.NoError(t, err)
	parsed, _, err := parseBlockHeader(block)
	require.NoError(t, err)
	require.NoError(t, store.PutHistoricalBlock(ctx, h.scope, parsed.Hash, block, time.Hour))
	_, hit := h.ReadBlockByHash(ctx, parsed.Hash)
	require.False(t, hit, "payload alone must not establish canonical finality")
	_, hit = unknown.ReadBlockByNumber(ctx, 1)
	require.False(t, hit, "unknown finality is not proof of genesis or any height")

	logsOnlyHeader, err := chain.HeaderByNumber(ctx, 1)
	require.NoError(t, err)
	logsOnlyHeader = historicalHashOnlyHeader(t, logsOnlyHeader)
	logs, err := chain.LogsByBlockHash(ctx, parsed.Hash)
	require.NoError(t, err)
	require.NoError(t, store.PutHistoricalLogs(ctx, h.scope, parsed.Hash, logsOnlyHeader, logs, time.Hour))
	require.NoError(t, store.PutFinalizedHash(ctx, h.scope, 1, parsed.Hash, time.Hour))
	store.mu.Lock()
	delete(store.blocks, historicalHashKey(h.scope, parsed.Hash))
	store.mu.Unlock()

	blockRec, hit := h.ReadBlockByNumber(ctx, 1)
	require.False(t, hit, "logs/index do not substitute for missing full block payload")
	logsRec, hit := h.ReadLogsByHash(ctx, parsed.Hash)
	require.True(t, hit)
	require.JSONEq(t, string(logsOnlyHeader), string(logsRec.Block), "logs records retain the header for metadata")
	require.JSONEq(t, string(logs), string(logsRec.Logs))
	require.Len(t, mustFilterLogs(t, logsRec), 1)

	require.NoError(t, store.PutHistoricalBlock(ctx, h.scope, parsed.Hash, block, time.Hour))
	blockRec, hit = h.ReadBlockByNumber(ctx, 1)
	require.True(t, hit)
	require.JSONEq(t, string(block), string(blockRec.Block))
	require.Empty(t, blockRec.Logs, "block reads do not require or return logs")
	var withoutTransactions map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(block, &withoutTransactions))
	delete(withoutTransactions, "transactions")
	missingTransactions, err := json.Marshal(withoutTransactions)
	require.NoError(t, err)
	require.NoError(t, store.PutHistoricalBlock(ctx, h.scope, parsed.Hash, missingTransactions, time.Hour))
	_, hit = h.ReadBlockByNumber(ctx, 1)
	require.False(t, hit, "full block payload requires an explicit transactions array")
	require.NoError(t, store.PutHistoricalBlock(ctx, h.scope, parsed.Hash, block, time.Hour))
	require.NoError(t, store.PutFinalizedHash(ctx, h.scope, 2, parsed.Hash, time.Hour))
	_, hit = h.ReadBlockByNumber(ctx, 2)
	require.False(t, hit, "index/payload height mismatch must miss")

	store.mu.Lock()
	store.logs[historicalHashKey(h.scope, parsed.Hash)] = historicalLogsPayload{header: logsOnlyHeader, logs: json.RawMessage(`[]`)}
	store.mu.Unlock()
	_, hit = h.ReadLogsByHash(ctx, parsed.Hash)
	require.False(t, hit, "empty logs cannot stand in for incomplete data when bloom commits to logs")

	var emptyHeader map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(logsOnlyHeader, &emptyHeader))
	zeroBloom, err := json.Marshal("0x" + strings.Repeat("0", 512))
	require.NoError(t, err)
	emptyHeader["logsBloom"] = zeroBloom
	emptyHeaderRaw, err := json.Marshal(emptyHeader)
	require.NoError(t, err)
	require.NoError(t, store.PutHistoricalLogs(ctx, h.scope, parsed.Hash, emptyHeaderRaw, json.RawMessage(`[]`), time.Hour))
	emptyRec, hit := h.ReadLogsByHash(ctx, parsed.Hash)
	require.True(t, hit, "complete empty logs are cacheable")
	require.Empty(t, mustFilterLogs(t, emptyRec))
}

func mustFilterLogs(t *testing.T, rec *BlockRecord) []json.RawMessage {
	t.Helper()
	logs, err := rec.FilterLogs(nil, false)
	require.NoError(t, err)
	return logs
}

func TestHistorical_ReadLogsRangeIsBoundedAndAllOrMiss(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(5)
	store := newHistoricalTestStore()
	h := newHistoricalForTest(t, chain, store, 5, 3)
	for n := int64(1); n <= 3; n++ {
		header, err := chain.HeaderByNumber(ctx, n)
		require.NoError(t, err)
		b, _, err := parseBlockHeader(header)
		require.NoError(t, err)
		logs, err := chain.LogsByBlockHash(ctx, b.Hash)
		require.NoError(t, err)
		require.NoError(t, store.PutHistoricalLogs(ctx, h.scope, b.Hash, header, logs, time.Hour))
		require.NoError(t, store.PutFinalizedHash(ctx, h.scope, n, b.Hash, time.Hour))
	}
	got, hit := h.ReadLogsRange(ctx, 1, 3)
	require.True(t, hit)
	require.Len(t, got, 3)
	got, hit = h.ReadLogsRange(ctx, 1, 4)
	require.False(t, hit, "range larger than maxLogsRange misses")
	require.Nil(t, got)

	store.mu.Lock()
	key := historicalHashKey(h.scope, hashOf(2, "a"))
	payload := store.logs[key]
	var headerFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload.header, &headerFields))
	brokenParent, err := json.Marshal(hashOf(0, "unrelated"))
	require.NoError(t, err)
	headerFields["parentHash"] = brokenParent
	payload.header, err = json.Marshal(headerFields)
	require.NoError(t, err)
	store.logs[key] = payload
	store.mu.Unlock()
	got, hit = h.ReadLogsRange(ctx, 1, 3)
	require.False(t, hit, "a contiguous height index with mismatched parent links is not a canonical range")
	require.Nil(t, got)

	delete(store.index, historicalHeightKey(h.scope, 2))
	got, hit = h.ReadLogsRange(ctx, 1, 3)
	require.False(t, hit, "index gap misses the whole range")
	require.Nil(t, got, "partial logs must not escape")
}

// caseSensitiveHistoricalStore keys payloads by the exact string it is given,
// like the Redis store (which sha256-hashes the key), so any read/write
// normalization mismatch surfaces as a miss.
type caseSensitiveHistoricalStore struct{ *historicalTestStore }

func (s caseSensitiveHistoricalStore) GetHistoricalBlock(_ context.Context, scope Scope, hash string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.blocks[historicalScopeKey(scope, hash)]; b != nil {
		return append(json.RawMessage(nil), b...), nil
	}
	return nil, ErrNotFound
}
func (s caseSensitiveHistoricalStore) PutHistoricalBlock(_ context.Context, scope Scope, hash string, block json.RawMessage, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks[historicalScopeKey(scope, hash)] = append(json.RawMessage(nil), block...)
	s.blockPuts++
	return nil
}
func (s caseSensitiveHistoricalStore) GetHistoricalLogs(_ context.Context, scope Scope, hash string) (json.RawMessage, json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.logs[historicalScopeKey(scope, hash)]
	if !ok {
		return nil, nil, ErrNotFound
	}
	return append(json.RawMessage(nil), p.header...), append(json.RawMessage(nil), p.logs...), nil
}
func (s caseSensitiveHistoricalStore) PutHistoricalLogs(_ context.Context, scope Scope, hash string, header, logs json.RawMessage, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs[historicalScopeKey(scope, hash)] = historicalLogsPayload{header: append(json.RawMessage(nil), header...), logs: append(json.RawMessage(nil), logs...)}
	s.logsPuts++
	return nil
}

func TestHistorical_HashKeysAreCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(3)
	chain.mixedCase = true // upstream returns upper-case block hashes
	store := caseSensitiveHistoricalStore{newHistoricalTestStore()}
	h := NewHistorical(HistoricalOptions{Scope: Scope{Namespace: "test", ProjectId: "p", NetworkId: "evm:1"}, MaxBlockSize: 1 << 16, MaxLogsRange: 3}, store, chain, func(context.Context) int64 { return 3 })

	require.NoError(t, h.WarmBlock(ctx, 1))
	require.NoError(t, h.WarmLogs(ctx, 1))
	lower := hashOf(1, "a")
	upper := strings.ToUpper(lower)
	mixed := "0x" + strings.ToUpper(lower[2:4]) + lower[4:]

	_, hit := h.ReadBlockByNumber(ctx, 1)
	require.True(t, hit, "by-number read finds a payload stored under an upper-case upstream hash")
	for _, q := range []string{lower, upper, mixed} {
		_, hit = h.ReadBlockByHash(ctx, q)
		require.True(t, hit, "block by hash %q", q)
		_, hit = h.ReadLogsByHash(ctx, q)
		require.True(t, hit, "logs by hash %q", q)
	}
	_, hit = h.ReadLogsRange(ctx, 1, 1)
	require.True(t, hit, "range read follows the normalized index to the payload")
}

func TestHistorical_WarmPathsAreIndependentAndRecheckCanonicality(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(3)
	store := newHistoricalTestStore()
	h := newHistoricalForTest(t, chain, store, 3, 3)
	require.NoError(t, h.WarmBlock(ctx, 1))
	require.Equal(t, 1, store.blockPuts)
	require.Zero(t, store.logsPuts, "warming a block must not fetch or store logs")

	chain.reorg(1, "new")
	// A newly fetched header and logs for the current canonical fork are valid.
	require.NoError(t, h.WarmLogs(ctx, 1))
	require.Equal(t, 1, store.logsPuts)
	require.Equal(t, 1, store.blockPuts, "warming logs must not fetch or store a full block")
	_, hit := h.ReadLogsByHash(ctx, hashOf(1, "new"))
	require.True(t, hit)
	_, hit = h.ReadLogsByHash(ctx, hashOf(1, "a"))
	require.False(t, hit, "old hash is no longer the finalized height index")

	chain.reorg(1, "later")
	require.Error(t, h.WarmBlockFromResult(ctx, 1, json.RawMessage(`{"number":"0x1","hash":"bad"}`)))
}

type reorgAfterBodyFetcher struct {
	*fakeChain
	number int64
}

func (f *reorgAfterBodyFetcher) BlockByNumber(ctx context.Context, n int64) (json.RawMessage, error) {
	block, err := f.fakeChain.BlockByNumber(ctx, n)
	if err == nil && n == f.number {
		f.fakeChain.reorg(n, "replacement")
	}
	return block, err
}

func TestHistorical_WarmRejectsSameHeightCanonicalChange(t *testing.T) {
	chain := newFakeChain(3)
	fetcher := &reorgAfterBodyFetcher{fakeChain: chain, number: 1}
	store := newHistoricalTestStore()
	h := NewHistorical(HistoricalOptions{Scope: Scope{Namespace: "test", ProjectId: "p", NetworkId: "evm:1"}, MaxBlockSize: 1 << 16}, store, fetcher, func(context.Context) int64 { return 3 })
	require.Error(t, h.WarmBlock(context.Background(), 1))
	require.Zero(t, store.blockPuts)
	require.Empty(t, store.index)
}

func TestHistorical_WarmBlockFromResultAvoidsBodyFetchAndUnknownFinalityDoesNotFill(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(3)
	store := newHistoricalTestStore()
	finalized := int64(0)
	h := NewHistorical(HistoricalOptions{Scope: Scope{Namespace: "test", ProjectId: "p", NetworkId: "evm:1"}, MaxBlockSize: 1 << 16}, store, chain, func(context.Context) int64 { return finalized })
	require.NoError(t, h.WarmBlock(ctx, 1))
	require.Zero(t, store.blockPuts)
	block, err := chain.BlockByNumber(ctx, 1)
	require.NoError(t, err)
	finalized = 3
	require.NoError(t, h.WarmBlockFromResult(ctx, 1, block))
	require.Equal(t, 1, store.blockPuts)
	chain.mu.Lock()
	calls := chain.bodyCalls
	chain.mu.Unlock()
	require.Equal(t, 1, calls, "only the explicit result fetch called BlockByNumber")
}

func TestHistorical_WarmsCoalesceByHeightAndPayloadKind(t *testing.T) {
	chain := newFakeChain(3)
	chain.delay = 15 * time.Millisecond
	store := newHistoricalTestStore()
	h := newHistoricalForTest(t, chain, store, 3, 3)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); errs <- h.WarmBlock(context.Background(), 2) }()
		go func() { defer wg.Done(); errs <- h.WarmLogs(context.Background(), 2) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, store.blockPuts)
	require.Equal(t, 1, store.logsPuts)
	chain.mu.Lock()
	bodyCalls, headerCalls := chain.bodyCalls, chain.headCalls
	chain.mu.Unlock()
	require.Equal(t, 1, bodyCalls, "block warmers coalesce per height")
	require.Equal(t, 3, headerCalls, "logs warmers fetch initial and rechecked headers once")
}

func TestHistorical_RewarmOfStoredEntriesSkipsUpstream(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(5)
	store := newHistoricalTestStore()
	h := newHistoricalForTest(t, chain, store, 5, 5)
	for n := int64(1); n <= 3; n++ {
		require.NoError(t, h.WarmLogs(ctx, n))
		require.NoError(t, h.WarmBlock(ctx, n))
	}
	chain.mu.Lock()
	body, head := chain.bodyCalls, chain.headCalls
	chain.mu.Unlock()
	for n := int64(1); n <= 3; n++ {
		require.NoError(t, h.WarmLogs(ctx, n))
		require.NoError(t, h.WarmBlock(ctx, n))
	}
	chain.mu.Lock()
	require.Equal(t, body, chain.bodyCalls, "stored blocks are not refetched")
	require.Equal(t, head, chain.headCalls, "stored logs are not refetched")
	chain.mu.Unlock()
	require.Equal(t, 3, store.logsPuts)
	require.Equal(t, 3, store.blockPuts)

	// A corrupt stored entry is not a hit, so warming repairs it.
	store.mu.Lock()
	key := historicalHashKey(h.scope, hashOf(2, "a"))
	store.logs[key] = historicalLogsPayload{header: store.logs[key].header, logs: json.RawMessage(`[]`)}
	store.mu.Unlock()
	require.NoError(t, h.WarmLogs(ctx, 2))
	require.Equal(t, 4, store.logsPuts)
	_, hit := h.ReadLogsRange(ctx, 1, 3)
	require.True(t, hit)
}
