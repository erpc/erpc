package blockstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// HistoricalStore stores independently addressable finalized blocks, complete
// block-hash logs, and the finalized height-to-hash index.
type HistoricalStore interface {
	GetFinalizedHash(context.Context, Scope, int64) (string, error)
	PutFinalizedHash(context.Context, Scope, int64, string, time.Duration) error
	GetHistoricalBlock(context.Context, Scope, string) (json.RawMessage, error)
	PutHistoricalBlock(context.Context, Scope, string, json.RawMessage, time.Duration) error
	GetHistoricalLogs(context.Context, Scope, string) (json.RawMessage, json.RawMessage, error)
	PutHistoricalLogs(context.Context, Scope, string, json.RawMessage, json.RawMessage, time.Duration) error
}

type HistoricalOptions struct {
	Scope        Scope
	TTL          time.Duration
	MaxBlockSize int64
	MaxLogsRange int64
}

type historicalKind uint8

const (
	historicalBlock historicalKind = iota
	historicalLogs
)

type historicalFill struct {
	done chan struct{}
	err  error
}

// Historical serves finalized data outside the moving live block window.
type Historical struct {
	scope           Scope
	store           HistoricalStore
	fetcher         Fetcher
	finalizedHeight func(context.Context) int64
	maxBlockSize    int64
	ttl             time.Duration
	maxLogsRange    int64

	mu       sync.Mutex
	inflight map[historicalFillKey]*historicalFill
}

type historicalFillKey struct {
	height int64
	kind   historicalKind
}

// NewHistorical constructs a history reader. TTL defaults to one hour.
func NewHistorical(opt HistoricalOptions, store HistoricalStore, fetcher Fetcher, finalized func(context.Context) int64) *Historical {
	if opt.TTL <= 0 {
		opt.TTL = time.Hour
	}
	if opt.MaxLogsRange < 1 {
		opt.MaxLogsRange = 1
	}
	opt.Scope.Namespace += ":historical"
	return &Historical{scope: opt.Scope, store: store, fetcher: fetcher, finalizedHeight: finalized,
		maxBlockSize: opt.MaxBlockSize, ttl: opt.TTL, maxLogsRange: opt.MaxLogsRange,
		inflight: make(map[historicalFillKey]*historicalFill)}
}

func (h *Historical) finalHeight(ctx context.Context) (int64, bool) {
	if h == nil || h.store == nil || h.finalizedHeight == nil {
		return 0, false
	}
	n := h.finalizedHeight(ctx)
	return n, n > 0
}

// ReadBlockByNumber returns a finalized full block. Logs are intentionally not
// fetched as part of this independent cache entry.
func (h *Historical) ReadBlockByNumber(ctx context.Context, n int64) (*BlockRecord, bool) {
	if n < 0 {
		return nil, false
	}
	finalized, ok := h.finalHeight(ctx)
	if !ok || n > finalized {
		return nil, false
	}
	hash, err := h.store.GetFinalizedHash(ctx, h.scope, n)
	if err != nil || hash == "" {
		return nil, false
	}
	block, err := h.store.GetHistoricalBlock(ctx, h.scope, hash)
	if err != nil || len(block) == 0 {
		return nil, false
	}
	b, got, err := parseBlockHeader(block)
	if err != nil || got != n || normHash(b.Hash) != normHash(hash) {
		return nil, false
	}
	if _, full, err := txHashesOf(b); b.Transactions == nil || err != nil || !full {
		return nil, false
	}
	if h.maxBlockSize > 0 && int64(len(block)) > h.maxBlockSize {
		return nil, false
	}
	return &BlockRecord{Number: n, Hash: normHash(b.Hash), ParentHash: normHash(b.ParentHash), Block: append(json.RawMessage(nil), block...)}, true
}

// ReadBlockByHash returns a finalized full block only when its height index
// resolves to the requested hash.
func (h *Historical) ReadBlockByHash(ctx context.Context, hash string) (*BlockRecord, bool) {
	if hash == "" {
		return nil, false
	}
	finalized, ok := h.finalHeight(ctx)
	if !ok {
		return nil, false
	}
	block, err := h.store.GetHistoricalBlock(ctx, h.scope, hash)
	if err != nil || len(block) == 0 {
		return nil, false
	}
	b, n, err := parseBlockHeader(block)
	if err != nil || n > finalized || normHash(b.Hash) != normHash(hash) {
		return nil, false
	}
	indexed, err := h.store.GetFinalizedHash(ctx, h.scope, n)
	if err != nil || normHash(indexed) != normHash(hash) {
		return nil, false
	}
	if _, full, err := txHashesOf(b); b.Transactions == nil || err != nil || !full {
		return nil, false
	}
	if h.maxBlockSize > 0 && int64(len(block)) > h.maxBlockSize {
		return nil, false
	}
	return &BlockRecord{Number: n, Hash: normHash(b.Hash), ParentHash: normHash(b.ParentHash), Block: append(json.RawMessage(nil), block...)}, true
}

// ReadLogsRange returns every block's complete logs, or a miss with no partial
// result. Range bounds are inclusive.
func (h *Historical) ReadLogsRange(ctx context.Context, from, to int64) ([]*BlockRecord, bool) {
	if from < 0 || to < from || to-from >= h.maxLogsRange {
		return nil, false
	}
	finalized, ok := h.finalHeight(ctx)
	if !ok || to > finalized {
		return nil, false
	}
	out := make([]*BlockRecord, 0, to-from+1)
	for n := from; n <= to; n++ {
		rec, hit := h.readLogsByNumber(ctx, n)
		if !hit || (len(out) > 0 && rec.ParentHash != out[len(out)-1].Hash) {
			return nil, false
		}
		out = append(out, rec)
	}
	return out, true
}

// ReadLogsByHash returns complete logs only when their hash is finalized at
// the header's height.
func (h *Historical) ReadLogsByHash(ctx context.Context, hash string) (*BlockRecord, bool) {
	if hash == "" {
		return nil, false
	}
	finalized, ok := h.finalHeight(ctx)
	if !ok {
		return nil, false
	}
	header, logs, err := h.store.GetHistoricalLogs(ctx, h.scope, hash)
	if err != nil {
		return nil, false
	}
	rec, hit := h.validateLogs(header, logs, normHash(hash), finalized)
	if !hit {
		return nil, false
	}
	indexed, err := h.store.GetFinalizedHash(ctx, h.scope, rec.Number)
	if err != nil || normHash(indexed) != normHash(hash) {
		return nil, false
	}
	return rec, true
}

func (h *Historical) readLogsByNumber(ctx context.Context, n int64) (*BlockRecord, bool) {
	hash, err := h.store.GetFinalizedHash(ctx, h.scope, n)
	if err != nil || hash == "" {
		return nil, false
	}
	header, logs, err := h.store.GetHistoricalLogs(ctx, h.scope, hash)
	if err != nil {
		return nil, false
	}
	rec, ok := h.validateLogs(header, logs, normHash(hash), n)
	if !ok || rec.Number != n {
		return nil, false
	}
	return rec, true
}

func (h *Historical) validateLogs(header, logs json.RawMessage, hash string, finalized int64) (*BlockRecord, bool) {
	b, n, err := parseBlockHeader(header)
	if err != nil || n < 0 || n > finalized || normHash(b.Hash) != hash {
		return nil, false
	}
	txs, _, err := txHashesOf(b)
	if err != nil || validateCompleteLogs(b, n, txs, logs) != nil {
		return nil, false
	}
	if h.maxBlockSize > 0 && int64(len(header)+len(logs)) > h.maxBlockSize {
		return nil, false
	}
	return &BlockRecord{Number: n, Hash: normHash(b.Hash), ParentHash: normHash(b.ParentHash),
		Block: append(json.RawMessage(nil), header...), Logs: append(json.RawMessage(nil), logs...)}, true
}

// WarmBlock fetches and publishes one full block, then publishes its finalized
// height index. It does not fetch logs.
func (h *Historical) WarmBlock(ctx context.Context, n int64) error {
	return h.coalesce(ctx, historicalFillKey{height: n, kind: historicalBlock}, func() error {
		return h.warmBlock(ctx, n, nil)
	})
}

// WarmBlockFromResult warms a full block already returned by the normal RPC
// path, avoiding a duplicate body fetch. It is still checked against finality
// and a freshly fetched canonical header.
func (h *Historical) WarmBlockFromResult(ctx context.Context, n int64, block json.RawMessage) error {
	return h.coalesce(ctx, historicalFillKey{height: n, kind: historicalBlock}, func() error {
		return h.warmBlock(ctx, n, block)
	})
}

func (h *Historical) warmBlock(ctx context.Context, n int64, provided json.RawMessage) error {
	finalized, ok := h.finalHeight(ctx)
	if !ok || n < 0 || n > finalized || h.fetcher == nil {
		return nil
	}
	// Already indexed and valid: skip upstream work.
	if _, hit := h.ReadBlockByNumber(ctx, n); hit {
		return nil
	}
	block := provided
	if len(block) == 0 {
		var err error
		block, err = h.fetcher.BlockByNumber(ctx, n)
		if err != nil {
			return fmt.Errorf("fetch historical block %d: %w", n, err)
		}
	}
	b, got, err := parseBlockHeader(block)
	if err != nil {
		return fmt.Errorf("invalid historical header or block %d: %w", n, err)
	}
	if got != n {
		return fmt.Errorf("invalid historical header or block %d: returned number %d", n, got)
	}
	if _, full, err := txHashesOf(b); b.Transactions == nil || err != nil || !full {
		return fmt.Errorf("historical block %d does not contain full transactions", n)
	}
	if h.maxBlockSize > 0 && int64(len(block)) > h.maxBlockSize {
		return errRecordTooLarge
	}
	if err := h.recheckCanonical(ctx, n, b.Hash); err != nil {
		return err
	}
	if err := h.store.PutHistoricalBlock(ctx, h.scope, b.Hash, append(json.RawMessage(nil), block...), h.ttl); err != nil {
		return fmt.Errorf("store historical block %d: %w", n, err)
	}
	if err := h.store.PutFinalizedHash(ctx, h.scope, n, normHash(b.Hash), h.ttl); err != nil {
		return fmt.Errorf("store historical block index %d: %w", n, err)
	}
	return nil
}

// WarmLogs fetches only the header and its unfiltered blockHash logs.
func (h *Historical) WarmLogs(ctx context.Context, n int64) error {
	return h.coalesce(ctx, historicalFillKey{height: n, kind: historicalLogs}, func() error {
		return h.warmLogs(ctx, n)
	})
}

func (h *Historical) warmLogs(ctx context.Context, n int64) error {
	finalized, ok := h.finalHeight(ctx)
	if !ok || n < 0 || n > finalized || h.fetcher == nil {
		return nil
	}
	// Already indexed and valid: skip upstream work.
	if _, hit := h.readLogsByNumber(ctx, n); hit {
		return nil
	}
	header, err := h.fetcher.HeaderByNumber(ctx, n)
	if err != nil {
		return fmt.Errorf("fetch historical header %d: %w", n, err)
	}
	b, got, err := parseBlockHeader(header)
	if err != nil {
		return fmt.Errorf("invalid historical header or block %d: %w", n, err)
	}
	if got != n {
		return fmt.Errorf("invalid historical header or block %d: returned number %d", n, got)
	}
	logs, err := h.fetcher.LogsByBlockHash(ctx, normHash(b.Hash))
	if err != nil {
		return fmt.Errorf("fetch historical logs %d: %w", n, err)
	}
	if _, ok := h.validateLogs(header, logs, normHash(b.Hash), finalized); !ok {
		return fmt.Errorf("invalid or incomplete historical logs at %d", n)
	}
	if err := h.recheckCanonical(ctx, n, b.Hash); err != nil {
		return err
	}
	if err := h.store.PutHistoricalLogs(ctx, h.scope, b.Hash, append(json.RawMessage(nil), header...), append(json.RawMessage(nil), logs...), h.ttl); err != nil {
		return fmt.Errorf("store historical logs %d: %w", n, err)
	}
	if err := h.store.PutFinalizedHash(ctx, h.scope, n, normHash(b.Hash), h.ttl); err != nil {
		return fmt.Errorf("store historical logs index %d: %w", n, err)
	}
	return nil
}

func (h *Historical) recheckCanonical(ctx context.Context, n int64, hash string) error {
	finalized, ok := h.finalHeight(ctx)
	if !ok || n > finalized {
		return fmt.Errorf("historical block %d is not positively finalized", n)
	}
	header, err := h.fetcher.HeaderByNumber(ctx, n)
	if err != nil {
		return fmt.Errorf("recheck historical header %d: %w", n, err)
	}
	b, got, err := parseBlockHeader(header)
	if err != nil {
		return fmt.Errorf("recheck historical header %d: %w", n, err)
	}
	if got != n || normHash(b.Hash) != normHash(hash) {
		return fmt.Errorf("historical block %d is no longer canonical", n)
	}
	return nil
}

func (h *Historical) coalesce(ctx context.Context, key historicalFillKey, fillFn func() error) (result error) {
	if h == nil || h.store == nil || h.finalizedHeight == nil || h.fetcher == nil {
		return errors.New("historical warming unavailable")
	}
	h.mu.Lock()
	if existing := h.inflight[key]; existing != nil {
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-existing.done:
			return existing.err
		}
	}
	fill := &historicalFill{done: make(chan struct{})}
	h.inflight[key] = fill
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		fill.err = result
		delete(h.inflight, key)
		close(fill.done)
		h.mu.Unlock()
	}()
	return fillFn()
}
