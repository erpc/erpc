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
	index     map[string]string
	blockPuts int
}

func newHistoricalTestStore() *historicalTestStore {
	return &historicalTestStore{blocks: make(map[string]json.RawMessage), index: make(map[string]string)}
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
	if cur := s.index[historicalHeightKey(scope, n)]; cur != "" {
		if normHash(cur) != normHash(hash) {
			return ErrIndexConflict
		}
		return nil
	}
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
func newHistoricalForTest(store HistoricalStore, finalized *int64) *Historical {
	return NewHistorical(HistoricalOptions{Scope: Scope{Namespace: "test", ProjectId: "p", NetworkId: "evm:1"}, MaxBlockSize: 1 << 16},
		store, func(context.Context) int64 { return *finalized })
}

func chainBlock(t *testing.T, chain *fakeChain, n int64) json.RawMessage {
	t.Helper()
	raw, err := chain.BlockByNumber(context.Background(), n)
	require.NoError(t, err)
	return raw
}

// Historical has no fetcher: everything it serves was adopted from a block
// response a client already received, so it can never cause an upstream call.
func TestHistorical_AdoptsServedBlocksWithoutFetching(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(6)
	store := newHistoricalTestStore()
	finalized := int64(4)
	h := newHistoricalForTest(store, &finalized)
	full := chainBlock(t, chain, 2)
	before, _, _ := chain.counts()

	require.NoError(t, h.Adopt(ctx, full, true))
	rec, hit := h.ReadBlockByNumber(ctx, 2)
	require.True(t, hit)
	require.JSONEq(t, string(full), string(rec.Block))
	_, hit = h.ReadBlockByHash(ctx, hashOf(2, "a"))
	require.True(t, hit)

	// Above the finalized height nothing is stored.
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 5), true))
	_, hit = h.ReadBlockByNumber(ctx, 5)
	require.False(t, hit)
	require.Equal(t, 1, store.blockPuts)

	// Re-adopting a stored block writes nothing again.
	require.NoError(t, h.Adopt(ctx, full, true))
	require.Equal(t, 1, store.blockPuts)

	// A hash-only response indexes the height but has no body to serve; a
	// later full by-hash response for the indexed hash completes it.
	header := historicalHashOnlyHeader(t, chainBlock(t, chain, 3))
	require.NoError(t, h.Adopt(ctx, header, true))
	_, hit = h.ReadBlockByNumber(ctx, 3)
	require.False(t, hit, "a header is not a full block")
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 3), false))
	_, hit = h.ReadBlockByNumber(ctx, 3)
	require.True(t, hit)

	// A by-hash response alone never establishes the canonical hash.
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), false))
	_, hit = h.ReadBlockByNumber(ctx, 1)
	require.False(t, hit)
	_, hit = h.ReadBlockByHash(ctx, hashOf(1, "a"))
	require.False(t, hit)

	head, body, logs := chain.counts()
	require.Equal(t, before, head)
	require.Equal(t, 5, head+body+logs, "only the test's own five chainBlock reads")
}

func TestHistorical_UnknownFinalityStoresNothing(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(3)
	store := newHistoricalTestStore()
	finalized := int64(0)
	h := newHistoricalForTest(store, &finalized)
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	require.Zero(t, store.blockPuts)
	require.Empty(t, store.index)
}

// Conflicting by-number observations of a finalized height fail closed: the
// index is dropped (a miss) rather than resolved with an upstream recheck.
func TestHistorical_ConflictingObservationFailsClosed(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(3)
	store := newHistoricalTestStore()
	finalized := int64(3)
	h := newHistoricalForTest(store, &finalized)
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	_, hit := h.ReadBlockByNumber(ctx, 1)
	require.True(t, hit)
	chain.reorg(1, "b")
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	_, hit = h.ReadBlockByNumber(ctx, 1)
	require.False(t, hit)
	_, hit = h.ReadBlockByHash(ctx, hashOf(1, "a"))
	require.False(t, hit)
	// The next observation indexes the height again.
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	rec, hit := h.ReadBlockByNumber(ctx, 1)
	require.True(t, hit)
	require.Equal(t, hashOf(1, "b"), rec.Hash)
}

func TestHistorical_LiveCanonicalHashInvalidatesStaleIndex(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(4)
	store := newHistoricalTestStore()
	finalized := int64(4)
	h := newHistoricalForTest(store, &finalized)
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	h.liveHash = func(n int64) string {
		if n == 1 {
			return "0xreorg"
		}
		return ""
	}
	_, hit := h.ReadBlockByNumber(ctx, 1)
	require.False(t, hit, "historical block must not survive a conflicting live canonical hash")
	_, err := store.GetFinalizedHash(ctx, h.scope, 1)
	require.ErrorIs(t, err, ErrNotFound, "stale height index must be removed")
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	_, err = store.GetFinalizedHash(ctx, h.scope, 1)
	require.ErrorIs(t, err, ErrNotFound, "a block the live window disagrees with is not indexed")
}

func historicalHashOnlyHeader(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	out, err := (&BlockRecord{Block: raw}).BlockJSON(false)
	require.NoError(t, err)
	return out
}

func TestHistorical_ReadsValidatePayloads(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(4)
	store := newHistoricalTestStore()
	finalized := int64(4)
	h := newHistoricalForTest(store, &finalized)
	block := chainBlock(t, chain, 1)
	parsed, _, err := parseBlockHeader(block)
	require.NoError(t, err)
	require.NoError(t, store.PutHistoricalBlock(ctx, h.scope, parsed.Hash, block, time.Hour))
	_, hit := h.ReadBlockByHash(ctx, parsed.Hash)
	require.False(t, hit, "payload alone must not establish canonical finality")

	require.NoError(t, store.PutFinalizedHash(ctx, h.scope, 1, parsed.Hash, time.Hour))
	_, hit = h.ReadBlockByNumber(ctx, 1)
	require.True(t, hit)
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
	finalized = 0
	_, hit = h.ReadBlockByNumber(ctx, 1)
	require.False(t, hit, "unknown finality is not proof of any height")
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

func TestHistorical_HashKeysAreCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(3)
	chain.mixedCase = true // upstream returns upper-case block hashes
	store := caseSensitiveHistoricalStore{newHistoricalTestStore()}
	finalized := int64(3)
	h := newHistoricalForTest(store, &finalized)
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	lower := hashOf(1, "a")
	upper := strings.ToUpper(lower)
	mixed := "0x" + strings.ToUpper(lower[2:4]) + lower[4:]
	_, hit := h.ReadBlockByNumber(ctx, 1)
	require.True(t, hit, "by-number read finds a payload stored under an upper-case upstream hash")
	for _, q := range []string{lower, upper, mixed} {
		_, hit = h.ReadBlockByHash(ctx, q)
		require.True(t, hit, "block by hash %q", q)
	}
}

// errIndexStore fails index reads with a transport error.
type errIndexStore struct{ *historicalTestStore }

func (s errIndexStore) GetFinalizedHash(context.Context, Scope, int64) (string, error) {
	return "", fmt.Errorf("dial tcp: connection refused")
}

// An unreadable index is not "nothing indexed": Adopt stores neither the body
// nor the index, since a conflicting hash might already be indexed.
func TestHistorical_AdoptFailsClosedOnIndexReadError(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(4)
	store := errIndexStore{newHistoricalTestStore()}
	finalized := int64(4)
	h := newHistoricalForTest(store, &finalized)
	require.Error(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	require.Empty(t, store.index, "no index written when the index read failed")
	require.Zero(t, store.blockPuts, "no body written when the index read failed")
}

// staleReadStore always reads "not indexed", modelling two adopts that both
// read before either writes.
type staleReadStore struct{ *historicalTestStore }

func (s staleReadStore) GetFinalizedHash(context.Context, Scope, int64) (string, error) {
	return "", ErrNotFound
}

// Concurrent conflicting by-number adopts of one finalized height cannot
// overwrite each other's index: the loser sees ErrIndexConflict and the
// height fails closed (unindexed) instead of naming the last writer.
func TestHistorical_ConcurrentConflictingAdoptsFailClosed(t *testing.T) {
	ctx := context.Background()
	chain := newFakeChain(4)
	base := newHistoricalTestStore()
	finalized := int64(4)
	h := newHistoricalForTest(staleReadStore{base}, &finalized)
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	chain.blocks[1] = "b"
	require.NoError(t, h.Adopt(ctx, chainBlock(t, chain, 1), true))
	_, err := base.GetFinalizedHash(ctx, h.scope, 1)
	require.ErrorIs(t, err, ErrNotFound, "conflicting adopts leave the height unindexed")
}
